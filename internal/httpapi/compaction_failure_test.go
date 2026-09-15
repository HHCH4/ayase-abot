package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"google.golang.org/adk/v2/session"
)

func TestStreamRuntimeChatIncludesCompactionFailure(t *testing.T) {
	ctx := context.Background()
	repo := agentruntime.NewMemoryRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{
		ID: "inv-compaction-stream", UserID: "user", ConversationID: "conversation", SessionID: "conversation",
		Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, event := range []agentruntime.AgentEvent{
		{ID: "compaction-failure-stream", InvocationID: invocation.ID, Type: agentruntime.EventContextCompactionFailed, Timestamp: now, Data: map[string]any{
			"code": "context_compaction_failed", "message": "上下文压缩失败，已保留已有会话事件", "retryable": true,
		}},
		{ID: "assistant-stream", InvocationID: invocation.ID, Type: agentruntime.EventAssistantMessage, Timestamp: now.Add(time.Millisecond), Data: map[string]any{"text": "已保留回复"}},
		{ID: "completed-stream", InvocationID: invocation.ID, Type: agentruntime.EventInvocationCompleted, Timestamp: now.Add(2 * time.Millisecond)},
	} {
		if _, err := repo.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := provider.NewRegistry(ctx, httpEmptyProviderRepository{}, map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "http-compaction-stream", SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/chat", nil)
	response := httptest.NewRecorder()
	server.streamRuntimeChat(response, request, invocation)

	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("legacy chat SSE status = %d, body=%s", response.Code, body)
	}
	if !strings.Contains(body, "event: runtime") || !strings.Contains(body, "context.compaction_failed") {
		t.Fatalf("legacy chat SSE 未转发压缩失败 runtime event: %s", body)
	}
	if !strings.Contains(body, "已保留回复") || !strings.Contains(body, "event: done") {
		t.Fatalf("压缩失败后仍应保留最终回复: %s", body)
	}
}

func TestRuntimeChatKeepsModalFallbackOutOfFinalAnswer(t *testing.T) {
	ctx := context.Background()
	repo := agentruntime.NewMemoryRepository()
	now := time.Now().UTC()
	invocation := agentruntime.Invocation{
		ID: "inv-modal-fallback-projection", UserID: "user", ConversationID: "conversation", SessionID: "conversation",
		Status: agentruntime.InvocationCompleted, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	for _, event := range []agentruntime.AgentEvent{
		{ID: "fallback-output", InvocationID: invocation.ID, Type: agentruntime.EventAssistantMessage, Timestamp: now, Data: map[string]any{
			"text": "图片转述：一张测试截图", "scope": "modal_fallback", "source": "modal_fallback",
		}},
		{ID: "primary-output", InvocationID: invocation.ID, Type: agentruntime.EventAssistantMessage, Timestamp: now.Add(time.Millisecond), Data: map[string]any{"text": "主模型回答"}},
		{ID: "modal-fallback-completed", InvocationID: invocation.ID, Type: agentruntime.EventInvocationCompleted, Timestamp: now.Add(2 * time.Millisecond)},
	} {
		if _, err := repo.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := provider.NewRegistry(ctx, httpEmptyProviderRepository{}, map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "http-modal-fallback", SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)

	streamRequest := httptest.NewRequest(http.MethodGet, "/api/v1/chat", nil)
	streamResponse := httptest.NewRecorder()
	server.streamRuntimeChat(streamResponse, streamRequest, invocation)
	streamBody := streamResponse.Body.String()
	if streamResponse.Code != http.StatusOK || !strings.Contains(streamBody, "图片转述：一张测试截图") || !strings.Contains(streamBody, `"text":"主模型回答"`) {
		t.Fatalf("流式兼容投影未同时展示 fallback 与主模型最终答案: %d %s", streamResponse.Code, streamBody)
	}
	doneIndex := strings.Index(streamBody, "event: done")
	if doneIndex < 0 {
		t.Fatal("流式兼容投影缺少 done 事件")
	}
	if doneBody := streamBody[doneIndex:]; strings.Contains(doneBody, "图片转述：一张测试截图") || !strings.Contains(doneBody, "主模型回答") {
		t.Fatal("fallback 中间输出不应写入 done 的最终 text")
	}

	waitRequest := httptest.NewRequest(http.MethodGet, "/api/v1/chat", nil)
	waitResponse := httptest.NewRecorder()
	server.waitRuntimeChat(waitResponse, waitRequest, invocation)
	waitBody := waitResponse.Body.String()
	if waitResponse.Code != http.StatusOK || !strings.Contains(waitBody, `"text":"主模型回答"`) || strings.Contains(waitBody, "图片转述：一张测试截图") {
		t.Fatalf("非流式兼容投影错误: %d %s", waitResponse.Code, waitBody)
	}
}
