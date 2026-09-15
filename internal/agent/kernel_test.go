package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"iter"
	"strings"
	"sync"
	"testing"

	"Abot/internal/conversation"
	"Abot/internal/provider"
	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

func TestKernelRunPersistsSessionModelState(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {
			ID: "demo", Name: "演示供应商", BaseURL: "http://127.0.0.1:9999/v1",
			Protocol: provider.ProtocolOpenAICompatible,
			Models:   []provider.Model{{ID: "demo-model", DisplayName: "演示模型", Enabled: true}},
		},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: adapter,
	})
	if err != nil {
		t.Fatalf("创建供应商 Registry 失败: %v", err)
	}
	sessions := session.InMemoryService()
	var resolvedWorkspaces []string
	kernel, err := NewKernel(Config{
		AppName:        "kernel-test",
		SessionService: sessions,
		Providers:      registry,
		Instruction:    "只回复测试文本。",
		WorkspaceTools: func(_ context.Context, workspaceID string) ([]tool.Tool, error) {
			resolvedWorkspaces = append(resolvedWorkspaces, workspaceID)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("创建 Kernel 失败: %v", err)
	}

	workspaceID := "workspace-a"
	first := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", SessionID: "session-1", ProviderID: "demo", ModelID: "demo-model", WorkspaceID: &workspaceID, Message: "第一条消息",
	}))
	if first.err != nil {
		t.Fatalf("第一次运行失败: %v", first.err)
	}
	if first.text != "测试回复" {
		t.Fatalf("第一次回复文本 = %q，期望测试回复", first.text)
	}

	stored, err := sessions.Get(context.Background(), &session.GetRequest{AppName: "kernel-test", UserID: "user-1", SessionID: "session-1"})
	if err != nil {
		t.Fatalf("读取持久化会话失败: %v", err)
	}
	if got, _ := stored.Session.State().Get(StateCurrentProvider); got != "demo" {
		t.Fatalf("会话供应商 State = %#v，期望 demo", got)
	}
	if got, _ := stored.Session.State().Get(StateCurrentModel); got != "demo-model" {
		t.Fatalf("会话模型 State = %#v，期望 demo-model", got)
	}
	wantPersonaID := effectivePersonaID(RuntimeOptions{Instruction: "只回复测试文本。"})
	if got, _ := stored.Session.State().Get(StatePersonaID); got != wantPersonaID {
		t.Fatalf("会话人格 State = %#v，期望 %q", got, wantPersonaID)
	}
	if got, _ := stored.Session.State().Get(StateCurrentWorkspace); got != "workspace-a" {
		t.Fatalf("会话工作区 State = %#v，期望 workspace-a", got)
	}
	if stored.Session.Events().Len() == 0 {
		t.Fatal("会话没有持久化事件")
	}

	// 第二轮不显式传模型，验证会话 State 会覆盖全局解析入口。
	second := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", SessionID: "session-1", Message: "第二条消息",
	}))
	if second.err != nil || second.text != "测试回复" {
		t.Fatalf("第二次运行结果不正确: %#v", second)
	}
	if adapter.builds != 1 {
		t.Fatalf("同一模型重复运行 Build 次数 = %d，期望 1", adapter.builds)
	}
	if len(resolvedWorkspaces) != 2 || resolvedWorkspaces[0] != "workspace-a" || resolvedWorkspaces[1] != "workspace-a" {
		t.Fatalf("工作区工具解析记录不正确: %#v", resolvedWorkspaces)
	}
	emptyWorkspace := ""
	third := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", SessionID: "session-1", WorkspaceID: &emptyWorkspace, Message: "取消工作区",
	}))
	if third.err != nil {
		t.Fatalf("取消工作区后运行失败: %v", third.err)
	}
	stored, err = sessions.Get(context.Background(), &session.GetRequest{AppName: "kernel-test", UserID: "user-1", SessionID: "session-1"})
	if err != nil {
		t.Fatalf("读取取消后的会话失败: %v", err)
	}
	if got, _ := stored.Session.State().Get(StateCurrentWorkspace); got != "" {
		t.Fatalf("取消后的工作区 State = %#v，期望空字符串", got)
	}
	if len(resolvedWorkspaces) != 2 {
		t.Fatalf("未选择工作区时不应装配工作区工具: %#v", resolvedWorkspaces)
	}
	kernel.locksMu.Lock()
	lockCount := len(kernel.sessionLocks)
	kernel.locksMu.Unlock()
	if lockCount != 0 {
		t.Fatalf("会话请求完成后 keyed mutex 应回收，剩余=%d", lockCount)
	}
}

func TestKernelBindsExplicitPersonaIDAcrossConfigChanges(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {
			ID: "demo", Name: "演示供应商", BaseURL: "http://127.0.0.1:9999/v1",
			Protocol: provider.ProtocolOpenAICompatible,
			Models:   []provider.Model{{ID: "demo-model", DisplayName: "演示模型", Enabled: true}},
		},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatalf("创建 persona Registry 失败: %v", err)
	}
	sessions := session.InMemoryService()
	personaID := "persona-alpha"
	kernel, err := NewKernel(Config{
		AppName: "persona-binding-kernel", SessionService: sessions, Providers: registry,
		RuntimeConfigResolver: func(context.Context, string, string, string) (RuntimeOptions, error) {
			return RuntimeOptions{AIEnabled: true, ProviderID: "demo", ModelID: "demo-model", PersonaID: personaID, Instruction: "当前人格 " + personaID}, nil
		},
	})
	if err != nil {
		t.Fatalf("创建 persona Kernel 失败: %v", err)
	}
	for index, message := range []string{"第一轮", "第二轮"} {
		result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "persona-user", SessionID: "persona-session", Message: message}))
		if result.err != nil {
			t.Fatalf("persona 第 %d 轮运行失败: %v", index+1, result.err)
		}
		stored, getErr := sessions.Get(context.Background(), &session.GetRequest{AppName: "persona-binding-kernel", UserID: "persona-user", SessionID: "persona-session"})
		if getErr != nil {
			t.Fatalf("读取 persona 第 %d 轮会话失败: %v", index+1, getErr)
		}
		if got, _ := stored.Session.State().Get(StatePersonaID); got != personaID {
			t.Fatalf("persona 第 %d 轮 State=%#v，期望 %q", index+1, got, personaID)
		}
		if index == 0 {
			personaID = "persona-beta"
		}
	}
}

func TestKernelValidationAndSessionID(t *testing.T) {
	if NewSessionID() == "" || NewSessionID() == NewSessionID() {
		t.Fatal("NewSessionID 应返回非空且通常唯一的值")
	}
	if _, err := NewKernel(Config{}); err == nil {
		t.Fatal("缺少 SessionService/Registry 时应拒绝创建 Kernel")
	}
}

func TestKernelUsesRuntimeConfigResolver(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "演示供应商", BaseURL: "http://127.0.0.1:9999/v1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "demo-model", DisplayName: "演示模型", Enabled: true}}},
	}}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &kernelTestAdapter{},
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	var gotBotID, gotUserID, gotConversationID string
	kernel, err := NewKernel(Config{
		AppName: "runtime-config-test", SessionService: session.InMemoryService(), Providers: registry,
		RuntimeConfigResolver: func(_ context.Context, botID, userID, conversationID string) (RuntimeOptions, error) {
			gotBotID, gotUserID, gotConversationID = botID, userID, conversationID
			return RuntimeOptions{AIEnabled: false}, nil
		},
	})
	if err != nil {
		t.Fatalf("创建 Kernel 失败: %v", err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", BotID: "bot-1", ConversationID: "conversation-1", Message: "测试运行时配置",
	}))
	if result.err == nil || !strings.Contains(result.err.Error(), "停用 AI") {
		t.Fatalf("AI 关闭配置没有在运行时生效: %v", result.err)
	}
	if gotBotID != "bot-1" || gotUserID != "user-1" || gotConversationID != "conversation-1" {
		t.Fatalf("运行时配置解析参数不正确: %q %q %q", gotBotID, gotUserID, gotConversationID)
	}
}

func TestKernelUsesConversationWorkspaceAndRejectsArchivedConversation(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "演示供应商", BaseURL: "http://127.0.0.1:9999/v1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "demo-model", DisplayName: "演示模型", Enabled: true}}},
	}}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &kernelTestAdapter{},
	})
	if err != nil {
		t.Fatalf("创建供应商 Registry 失败: %v", err)
	}
	sessions := session.InMemoryService()
	conversationService, err := conversation.NewService(newKernelConversationRepository(), sessions, "kernel-test")
	if err != nil {
		t.Fatalf("创建对话服务失败: %v", err)
	}
	item, err := conversationService.Create(context.Background(), conversation.CreateRequest{UserID: "user-1", WorkspaceID: "workspace-a", Title: "项目对话"})
	if err != nil {
		t.Fatalf("创建工作区对话失败: %v", err)
	}
	var resolved []string
	kernel, err := NewKernel(Config{
		AppName: "kernel-test", SessionService: sessions, Providers: registry, Conversations: conversationService,
		WorkspaceToolsForConversation: func(_ context.Context, workspaceID, conversationID string) ([]tool.Tool, error) {
			resolved = append(resolved, workspaceID+":"+conversationID)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("创建 Kernel 失败: %v", err)
	}
	otherWorkspace := "workspace-b"
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user-1", ConversationID: item.ID, ProviderID: "demo", ModelID: "demo-model", WorkspaceID: &otherWorkspace, Message: "读取项目",
	}))
	if result.err != nil {
		t.Fatalf("使用工作区对话运行失败: %v", result.err)
	}
	if len(resolved) != 1 || resolved[0] != "workspace-a:"+item.ID {
		t.Fatalf("Agent 没有使用对话绑定的工作区: %#v", resolved)
	}
	stored, err := sessions.Get(context.Background(), &session.GetRequest{AppName: "kernel-test", UserID: "user-1", SessionID: item.ID})
	if err != nil {
		t.Fatalf("读取工作区对话 Session 失败: %v", err)
	}
	if _, stateErr := stored.Session.State().Get(StateCurrentWorkspace); stateErr == nil {
		t.Fatal("正式工作区对话不应把 workspace_id 写入 current_workspace State")
	}
	if _, err := conversationService.Archive(context.Background(), item.UserID, item.ID); err != nil {
		t.Fatalf("归档工作区对话失败: %v", err)
	}
	archived := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: item.UserID, ConversationID: item.ID, Message: "继续操作"}))
	if !errors.Is(archived.err, conversation.ErrArchived) {
		t.Fatalf("归档对话运行错误 = %v，期望 ErrArchived", archived.err)
	}
}

func TestContentFromChatRequestBuildsTextAndAttachments(t *testing.T) {
	data := []byte{1, 2, 3}
	content, err := contentFromChatRequest(ChatRequest{
		Message: "请分析附件",
		Attachments: []Attachment{{Name: "截图.png", MIMEType: "image/png", Data: data}, {
			Name: "说明.pdf", MIMEType: "application/pdf", Data: []byte{4, 5},
		}},
	})
	if err != nil {
		t.Fatalf("构造带附件消息失败: %v", err)
	}
	if len(content.Parts) != 3 || content.Parts[0].Text != "请分析附件" {
		t.Fatalf("消息分片不正确: %#v", content.Parts)
	}
	if got := content.Parts[1].InlineData; got == nil || got.MIMEType != "image/png" || got.DisplayName != "截图.png" || len(got.Data) != 3 {
		t.Fatalf("图片附件分片不正确: %#v", got)
	}
	if got := content.Parts[2].InlineData; got == nil || got.MIMEType != "application/pdf" || got.DisplayName != "说明.pdf" {
		t.Fatalf("文件附件分片不正确: %#v", got)
	}
	data[0] = 99
	if content.Parts[1].InlineData.Data[0] == 99 {
		t.Fatal("消息分片不应复用调用方的附件缓冲区")
	}
}

func TestContentFromChatRequestResolvesBoundedArtifactRef(t *testing.T) {
	data := []byte("artifact-backed attachment")
	digest := sha256.Sum256(data)
	ref := AttachmentRef{ID: "artifact-1", Version: 2, Digest: "sha256:" + hex.EncodeToString(digest[:]), Kind: "input_attachment", MIMEType: "text/plain", Size: int64(len(data)), Name: "note.txt"}
	content, err := contentFromChatRequestWithResolver(context.Background(), ChatRequest{
		UserID: "user-1", Message: "请阅读引用文件", Attachments: []Attachment{{Ref: &ref}},
	}, func(_ context.Context, userID string, got AttachmentRef) (io.ReadCloser, error) {
		if userID != "user-1" || got.ID != ref.ID || got.Version != ref.Version {
			t.Fatalf("resolver 收到的 ref 不正确: user=%q ref=%#v", userID, got)
		}
		return io.NopCloser(strings.NewReader(string(data))), nil
	})
	if err != nil {
		t.Fatalf("解析 Artifact ref 失败: %v", err)
	}
	if len(content.Parts) != 2 || content.Parts[1].InlineData == nil || string(content.Parts[1].InlineData.Data) != string(data) {
		t.Fatalf("Artifact ref 未 materialize: %#v", content.Parts)
	}
	if content.Parts[1].InlineData.DisplayName != ref.Name || content.Parts[1].InlineData.MIMEType != ref.MIMEType {
		t.Fatalf("Artifact metadata 未投影: %#v", content.Parts[1].InlineData)
	}
}

func TestContentFromChatRequestRejectsArtifactRefMismatchAndUnboundedInput(t *testing.T) {
	data := []byte("different")
	digest := sha256.Sum256([]byte("expected"))
	_, err := contentFromChatRequestWithResolver(context.Background(), ChatRequest{
		UserID: "user-1", Attachments: []Attachment{{Ref: &AttachmentRef{ID: "artifact-1", Version: 1, Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: int64(len(data)), MIMEType: "text/plain"}}},
	}, func(context.Context, string, AttachmentRef) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(data))), nil
	})
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("digest 不一致应拒绝，err=%v", err)
	}
	_, err = contentFromChatRequestWithResolver(context.Background(), ChatRequest{
		UserID: "user-1", Attachments: []Attachment{{Ref: &AttachmentRef{ID: "artifact-large", Version: 1, Size: maxAttachmentMaterializeBytes + 1}}},
	}, func(context.Context, string, AttachmentRef) (io.ReadCloser, error) {
		t.Fatal("超限 ref 不应调用 resolver")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "上下文上限") {
		t.Fatalf("超限 ref 应拒绝，err=%v", err)
	}
}

func TestRetryLLMOnlyRetriesBeforeFirstResponse(t *testing.T) {
	model := &retryTestLLM{failures: 2}
	wrapped := &retryLLM{inner: model, retries: 2}
	var responses int
	var runErr error
	for response, err := range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil {
			runErr = err
			break
		}
		if response != nil {
			responses++
		}
	}
	if runErr != nil || responses != 1 || model.calls != 3 {
		t.Fatalf("前置失败重试结果不正确: err=%v responses=%d calls=%d", runErr, responses, model.calls)
	}

	model = &retryTestLLM{failures: 1, emitBeforeFailure: true}
	wrapped = &retryLLM{inner: model, retries: 2}
	runErr = nil
	responses = 0
	for response, err := range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
		if err != nil {
			runErr = err
			break
		}
		if response != nil {
			responses++
		}
	}
	if runErr == nil || responses != 1 || model.calls != 1 {
		t.Fatalf("已产生响应后不应重试: err=%v responses=%d calls=%d", runErr, responses, model.calls)
	}
}

func TestContextRecoveryLLMTrimsOnceAfterProviderLimit(t *testing.T) {
	model := &contextRecoveryTestLLM{}
	request := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("old history", genai.RoleUser)}}
	var trims int
	wrapped := &contextRecoveryLLM{
		inner: model,
		trim: func(_ adkagent.Context, got *adkmodel.LLMRequest) bool {
			trims++
			if len(got.Contents) == 0 {
				return false
			}
			got.Contents = got.Contents[1:]
			return true
		},
	}
	var responses int
	var runErr error
	for response, err := range wrapped.GenerateContent(context.Background(), request, false) {
		if err != nil {
			runErr = err
			break
		}
		if response != nil {
			responses++
		}
	}
	if runErr != nil || responses != 1 || model.calls != 2 || trims != 1 || len(request.Contents) != 0 {
		t.Fatalf("上下文超限恢复结果不正确: err=%v responses=%d calls=%d trims=%d contents=%d", runErr, responses, model.calls, trims, len(request.Contents))
	}

	// A second context-limit error must be returned without another trim.
	model = &contextRecoveryTestLLM{alwaysLimit: true}
	request = &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("history", genai.RoleUser)}}
	trims = 0
	wrapped = &contextRecoveryLLM{inner: model, trim: func(_ adkagent.Context, _ *adkmodel.LLMRequest) bool {
		trims++
		return true
	}}
	runErr = nil
	for _, err := range wrapped.GenerateContent(context.Background(), request, false) {
		if err != nil {
			runErr = err
			break
		}
	}
	if runErr == nil || model.calls != 2 || trims != 1 {
		t.Fatalf("第二次上下文超限不应继续重试: err=%v calls=%d trims=%d", runErr, model.calls, trims)
	}
}

func TestKernelContextRecoveryRebuildsOnceWithoutDuplicateOutput(t *testing.T) {
	model := &kernelContextRecoveryLLM{}
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {
			ID: "demo", Name: "demo", BaseURL: "http://127.0.0.1:9999/v1",
			Protocol: provider.ProtocolOpenAICompatible,
			Models:   []provider.Model{{ID: "model", DisplayName: "model", Enabled: true, ContextWindow: 4096, MaxOutputTokens: 128}},
		},
	}}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &kernelContextRecoveryAdapter{model: model},
	})
	if err != nil {
		t.Fatalf("创建 context recovery Registry 失败: %v", err)
	}
	var manifestMu sync.Mutex
	manifests := make([]ContextManifest, 0, 8)
	recoveryNotices := make([]ContextRecoveryNotice, 0, 2)
	kernel, err := NewKernel(Config{
		AppName: "context-recovery-kernel", SessionService: session.InMemoryService(), Providers: registry,
		Instruction: "只回复固定测试文本。",
		ContextManifestObserver: func(_ context.Context, manifest ContextManifest) {
			manifestMu.Lock()
			manifests = append(manifests, manifest)
			manifestMu.Unlock()
		},
		ContextRecoveryObserver: func(_ context.Context, _ string, notice ContextRecoveryNotice) {
			manifestMu.Lock()
			recoveryNotices = append(recoveryNotices, notice)
			manifestMu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("创建 context recovery Kernel 失败: %v", err)
	}

	// Populate the same ADK Session with old user/model turns so the recovery
	// callback has removable non-structural history to work with.
	for _, message := range []string{"历史消息一", "历史消息二"} {
		result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
			UserID: "user", SessionID: "context-recovery-session", ProviderID: "demo", ModelID: "model", Message: message,
		}))
		if result.err != nil || result.text != "正常回复" {
			t.Fatalf("准备历史会话失败: err=%v text=%q", result.err, result.text)
		}
	}

	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user", SessionID: "context-recovery-session", Message: "触发上下文恢复",
	}))
	if result.err != nil {
		t.Fatalf("Kernel context recovery 失败: %v", result.err)
	}
	if result.text != "恢复成功" {
		t.Fatalf("恢复后用户可见输出 = %q，期望仅一份恢复成功文本", result.text)
	}
	model.mu.Lock()
	calls := model.calls
	lengths := append([]int(nil), model.requestLengths...)
	model.mu.Unlock()
	if calls != 4 {
		t.Fatalf("两轮历史加一次失败/恢复应只有 4 次 provider call，实际=%d", calls)
	}
	if len(lengths) != 4 || lengths[3] >= lengths[2] {
		t.Fatalf("恢复请求应在同一 request 上移除旧历史: request contents=%v", lengths)
	}
	manifestMu.Lock()
	observed := append([]ContextManifest(nil), manifests...)
	manifestMu.Unlock()
	trimmed := false
	for _, manifest := range observed {
		for _, warning := range manifest.Warnings {
			if warning == "context_history_trimmed_once" {
				trimmed = true
				if len(manifest.Excluded) == 0 {
					t.Fatalf("受控重建 manifest 缺少 excluded 历史: %#v", manifest)
				}
			}
		}
	}
	if !trimmed {
		t.Fatalf("未观测到 context_history_trimmed_once，manifests=%#v", observed)
	}
	manifestMu.Lock()
	notices := append([]ContextRecoveryNotice(nil), recoveryNotices...)
	manifestMu.Unlock()
	if len(notices) != 1 || notices[0].Attempt != 1 || !notices[0].Recovered || notices[0].ModelCallID == "" {
		t.Fatalf("context-limit recovery notice 不正确: %#v", notices)
	}

	// Once the rebuilt request also hits the provider limit, the generic retry
	// layer must not send the unchanged request again. This exercises the
	// outer Kernel composition rather than the wrapper in isolation.
	model.mu.Lock()
	model.alwaysLimit = true
	model.mu.Unlock()
	failed := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{
		UserID: "user", SessionID: "context-recovery-session", Message: "再次触发上下文失败",
	}))
	if failed.err == nil || model.callCount() != 6 {
		t.Fatalf("第二次 context-limit 后应直接失败且不被通用 retry 重发: err=%v calls=%d", failed.err, model.callCount())
	}
	manifestMu.Lock()
	notices = append([]ContextRecoveryNotice(nil), recoveryNotices...)
	manifestMu.Unlock()
	if len(notices) != 3 || notices[1].Attempt != 1 || !notices[1].Recovered || notices[2].Attempt != 2 || notices[2].Recovered {
		t.Fatalf("context-limit 两次失败的 notice 不正确: %#v", notices)
	}
}

func TestRetryLLMDoesNotResendContextLimitError(t *testing.T) {
	model := &contextRecoveryTestLLM{alwaysLimit: true}
	wrapped := &retryLLM{inner: model, retries: 3}
	var runErr error
	for _, err := range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil {
			runErr = err
			break
		}
	}
	if runErr == nil || model.calls != 1 {
		t.Fatalf("context-limit error 不应被通用重试再次发送: err=%v calls=%d", runErr, model.calls)
	}
}

type contextRecoveryTestLLM struct {
	calls       int
	alwaysLimit bool
}

type kernelContextRecoveryAdapter struct {
	model *kernelContextRecoveryLLM
}

func (a *kernelContextRecoveryAdapter) BuildModel(context.Context, provider.Provider, string) (adkmodel.LLM, error) {
	return a.model, nil
}

func (a *kernelContextRecoveryAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "连接正常"}, nil
}

func (a *kernelContextRecoveryAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

type kernelContextRecoveryLLM struct {
	mu             sync.Mutex
	calls          int
	alwaysLimit    bool
	requestLengths []int
}

func (m *kernelContextRecoveryLLM) Name() string { return "kernel-context-recovery" }

func (m *kernelContextRecoveryLLM) GenerateContent(_ context.Context, request *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.calls++
	call := m.calls
	length := 0
	if request != nil {
		length = len(request.Contents)
	}
	m.requestLengths = append(m.requestLengths, length)
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		alwaysLimit := m.alwaysLimit
		m.mu.Unlock()
		if alwaysLimit || call == 3 {
			yield(nil, errors.New("maximum context length exceeded"))
			return
		}
		text := "正常回复"
		if call == 4 {
			text = "恢复成功"
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}, nil)
	}
}

func (m *kernelContextRecoveryLLM) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *contextRecoveryTestLLM) Name() string { return "context-recovery-test" }

func (m *contextRecoveryTestLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.calls++
		if m.alwaysLimit || m.calls == 1 {
			yield(nil, errors.New("maximum context length exceeded"))
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("recovered", genai.RoleModel)}, nil)
	}
}

type retryTestLLM struct {
	calls             int
	failures          int
	emitBeforeFailure bool
}

func (m *retryTestLLM) Name() string { return "retry-test" }

func (m *retryTestLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.calls++
		if m.calls <= m.failures {
			if m.emitBeforeFailure {
				if !yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("partial", genai.RoleModel)}, nil) {
					return
				}
			}
			yield(nil, errors.New("temporary model failure"))
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
	}
}

type kernelTestRepository struct {
	providers map[string]provider.Provider
	defaults  provider.DefaultRef
}

type kernelConversationRepository struct {
	items map[string]conversation.Conversation
}

func newKernelConversationRepository() *kernelConversationRepository {
	return &kernelConversationRepository{items: map[string]conversation.Conversation{}}
}

func (r *kernelConversationRepository) List(_ context.Context, userID, workspaceID string, includeArchived bool) ([]conversation.Conversation, error) {
	items := make([]conversation.Conversation, 0)
	for _, item := range r.items {
		if item.UserID != userID || (workspaceID != "*" && item.WorkspaceID != workspaceID) || (!includeArchived && item.Status != conversation.StatusActive) {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *kernelConversationRepository) Get(_ context.Context, userID, id string) (conversation.Conversation, error) {
	item, ok := r.items[id]
	if !ok || item.UserID != userID {
		return conversation.Conversation{}, conversation.ErrNotFound
	}
	return item, nil
}

func (r *kernelConversationRepository) GetByID(_ context.Context, id string) (conversation.Conversation, error) {
	item, ok := r.items[id]
	if !ok {
		return conversation.Conversation{}, conversation.ErrNotFound
	}
	return item, nil
}

func (r *kernelConversationRepository) CountByWorkspace(_ context.Context, workspaceID string) (int, error) {
	count := 0
	for _, item := range r.items {
		if item.WorkspaceID == workspaceID {
			count++
		}
	}
	return count, nil
}

func (r *kernelConversationRepository) Save(_ context.Context, item conversation.Conversation) error {
	r.items[item.ID] = item
	return nil
}

func (r *kernelConversationRepository) Delete(_ context.Context, userID, id string) error {
	item, ok := r.items[id]
	if !ok || item.UserID != userID {
		return conversation.ErrNotFound
	}
	delete(r.items, id)
	return nil
}

func (r *kernelTestRepository) List(context.Context) ([]provider.Provider, error) {
	result := make([]provider.Provider, 0, len(r.providers))
	for _, item := range r.providers {
		result = append(result, item)
	}
	return result, nil
}

func (r *kernelTestRepository) Get(_ context.Context, id string) (provider.Provider, error) {
	item, ok := r.providers[id]
	if !ok {
		return provider.Provider{}, provider.ErrNotFound
	}
	return item, nil
}

func (r *kernelTestRepository) Save(_ context.Context, item provider.Provider) error {
	r.providers[item.ID] = item
	return nil
}

func (r *kernelTestRepository) Delete(_ context.Context, id string) error {
	if _, ok := r.providers[id]; !ok {
		return provider.ErrNotFound
	}
	delete(r.providers, id)
	return nil
}

func (r *kernelTestRepository) GetDefault(context.Context) (provider.DefaultRef, error) {
	return r.defaults, nil
}

func (r *kernelTestRepository) SetDefault(_ context.Context, ref provider.DefaultRef) error {
	r.defaults = ref
	return nil
}

type kernelTestAdapter struct {
	builds int
	model  *kernelTestLLM
}

func (a *kernelTestAdapter) BuildModel(_ context.Context, _ provider.Provider, modelID string) (adkmodel.LLM, error) {
	a.builds++
	a.model = &kernelTestLLM{name: modelID}
	return a.model, nil
}

func (a *kernelTestAdapter) TestConnection(context.Context, provider.Provider) (provider.ProbeResult, error) {
	return provider.ProbeResult{Message: "连接正常"}, nil
}

func (a *kernelTestAdapter) DiscoverModels(context.Context, provider.Provider) ([]provider.Model, error) {
	return nil, nil
}

type tokenCountKernelAdapter struct {
	kernelTestAdapter
	counted int
	tokens  int
}

func (a *tokenCountKernelAdapter) CountTokens(_ context.Context, _ provider.Provider, _ provider.Model, request *adkmodel.LLMRequest) (provider.TokenCount, error) {
	if request == nil || request.Config == nil {
		return provider.TokenCount{}, errors.New("token count 收到空请求")
	}
	a.counted++
	tokens := a.tokens
	if tokens == 0 {
		tokens = 37
	}
	return provider.TokenCount{Tokens: tokens, Name: provider.GeminiTokenCounterName, Version: provider.GeminiTokenCounterVersion, Source: "provider", Quality: "provider", Exact: true}, nil
}

type kernelTestLLM struct {
	name          string
	instruction   string
	requestConfig *genai.GenerateContentConfig
	request       *adkmodel.LLMRequest
	calls         int
}

func (m *kernelTestLLM) Name() string { return m.name }

func (m *kernelTestLLM) GenerateContent(_ context.Context, request *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.calls++
	if request != nil && request.Config != nil && request.Config.SystemInstruction != nil {
		m.instruction = TextFromContent(request.Config.SystemInstruction)
	}
	if request != nil {
		m.request = request
		m.requestConfig = request.Config
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		// 模拟一个已完成的上游模型响应，验证 Runner 的事件和 State 持久化。
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("测试回复", genai.RoleModel)}, nil)
	}
}

func TestKernelUsesProviderTokenCountForContextManifest(t *testing.T) {
	profile := provider.DefaultCapabilities(provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini}, provider.Model{ID: "gemini-2.5-flash", ContextWindow: 4096})
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"gemini": {ID: "gemini", Name: "Gemini", Protocol: provider.ProtocolGemini, Models: []provider.Model{{ID: "gemini-2.5-flash", Enabled: true, ContextWindow: 4096, Capabilities: &profile}}},
	}}
	adapter := &tokenCountKernelAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolGemini: adapter})
	if err != nil {
		t.Fatal(err)
	}
	var manifest ContextManifest
	kernel, err := NewKernel(Config{AppName: "provider-count-kernel", SessionService: session.InMemoryService(), Providers: registry, ContextManifestObserver: func(_ context.Context, got ContextManifest) { manifest = got }})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", SessionID: "provider-count-session", ProviderID: "gemini", ModelID: "gemini-2.5-flash", Message: "统计上下文"}))
	if result.err != nil {
		t.Fatalf("使用 provider token count 运行失败: %v", result.err)
	}
	if adapter.counted != 1 {
		t.Fatalf("provider token count 调用次数=%d，期望 1", adapter.counted)
	}
	if manifest.EstimatedInput != 37 || manifest.EstimatorName != provider.GeminiTokenCounterName || manifest.EstimatorVersion != provider.GeminiTokenCounterVersion || manifest.EstimateQuality != "provider" || manifest.EstimateMultiplier != 1 {
		t.Fatalf("Context Manifest 未采用 provider token count: %#v", manifest)
	}
	if manifest.Digest == "" {
		t.Fatal("provider token count manifest 缺少 digest")
	}
}

func TestKernelProviderTokenCountParticipatesInHardBudget(t *testing.T) {
	profile := provider.DefaultCapabilities(provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini}, provider.Model{ID: "gemini-2.5-flash", ContextWindow: 128})
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"gemini": {ID: "gemini", Name: "Gemini", Protocol: provider.ProtocolGemini, Models: []provider.Model{{ID: "gemini-2.5-flash", Enabled: true, ContextWindow: 128, Capabilities: &profile}}},
	}}
	adapter := &tokenCountKernelAdapter{tokens: 256}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolGemini: adapter})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := NewKernel(Config{AppName: "provider-count-budget", SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", SessionID: "provider-count-budget-session", ProviderID: "gemini", ModelID: "gemini-2.5-flash", Message: "很短"}))
	if result.err == nil || !errors.Is(result.err, ErrContextBudgetExceeded) {
		t.Fatalf("provider exact count 超窗时应在 provider call 前失败: %v", result.err)
	}
	if adapter.counted != 1 {
		t.Fatalf("超窗请求 token count 次数=%d，期望 1", adapter.counted)
	}
}

func TestKernelSlidingWindowCompactionComposesWithTailRetention(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "演示供应商", BaseURL: "http://127.0.0.1:9999/v1", Protocol: provider.ProtocolOpenAICompatible,
			Models: []provider.Model{{ID: "demo-model", Enabled: true, ContextWindow: 4096}},
		},
	}}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: &kernelTestAdapter{},
	})
	if err != nil {
		t.Fatalf("创建供应商 Registry 失败: %v", err)
	}
	sessions := session.InMemoryService()
	kernel, err := NewKernel(Config{
		AppName: "sliding-compaction-test", SessionService: sessions, Providers: registry,
		EnableCompaction: true, CompactionRatio: 0.99, CompactionSafety: 1, CompactionRetention: 1,
		CompactionInterval: 2, CompactionOverlap: 1,
	})
	if err != nil {
		t.Fatalf("创建滑动窗口 Kernel 失败: %v", err)
	}
	for index, message := range []string{"第一轮", "第二轮"} {
		result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", SessionID: "sliding-session", ProviderID: "demo", ModelID: "demo-model", Message: message}))
		if result.err != nil || result.text != "测试回复" {
			t.Fatalf("第 %d 轮运行失败: err=%v text=%q", index+1, result.err, result.text)
		}
	}
	stored, err := sessions.Get(context.Background(), &session.GetRequest{AppName: "sliding-compaction-test", UserID: "user", SessionID: "sliding-session"})
	if err != nil {
		t.Fatalf("读取滑动窗口会话失败: %v", err)
	}
	var compactions []*session.Event
	for event := range stored.Session.Events().All() {
		if event != nil && event.Actions.Compaction != nil {
			compactions = append(compactions, event)
		}
	}
	if len(compactions) != 1 {
		t.Fatalf("两轮完成后滑动窗口摘要数=%d，期望 1", len(compactions))
	}
	compaction := compactions[0].Actions.Compaction
	if compaction == nil || compaction.CompactedContent == nil || TextFromContent(compaction.CompactedContent) == "" {
		t.Fatalf("滑动窗口摘要内容为空: %#v", compaction)
	}
	if compaction.StartTimestamp.IsZero() || compaction.EndTimestamp.IsZero() || !compaction.EndTimestamp.After(compaction.StartTimestamp) {
		t.Fatalf("滑动窗口覆盖范围无效: %#v", compaction)
	}
}

type kernelEventCollection struct {
	text   string
	events []*session.Event
	err    error
}

// collectKernelEvents 收集 Runner 输出，并只拼接模型事件中的文本。
func collectKernelEvents(sequence iter.Seq2[*session.Event, error]) kernelEventCollection {
	result := kernelEventCollection{}
	for event, err := range sequence {
		if err != nil {
			result.err = err
			continue
		}
		if event == nil {
			continue
		}
		result.events = append(result.events, event)
		if event.Author != "user" {
			result.text += TextFromContent(event.Content)
		}
	}
	return result
}

func TestEffectiveInstructionKeepsPersonaAndAppendsRuntimeContract(t *testing.T) {
	const persona = "你是一位项目助手。"
	got := effectiveInstruction(persona)
	if !strings.HasPrefix(got, persona) {
		t.Fatalf("运行时指令不应覆盖人格文本: %q", got)
	}
	for _, required := range []string{"Abot 任务执行契约", "等待用户批准", "验证", "不得声称已完成"} {
		if !strings.Contains(got, required) {
			t.Fatalf("运行时契约缺少 %q: %s", required, got)
		}
	}
	if empty := effectiveInstruction(" "); !strings.Contains(empty, "你是 Abot") {
		t.Fatalf("空人格应使用安全默认文本: %s", empty)
	}
	project := effectiveInstruction(persona, []ProjectInstruction{{
		Path: "src/AGENTS.md", ScopePath: "src", ContentDigest: "abc123", Content: "先运行目标测试。", Priority: 1,
	}})
	if !strings.Contains(project, "src/AGENTS.md") || !strings.Contains(project, "先运行目标测试") || !strings.Contains(project, "不能覆盖当前用户请求") {
		t.Fatalf("项目指令未以受限数据注入: %s", project)
	}
}

func TestEffectiveInstructionIncludesVersionedTaskContractProjection(t *testing.T) {
	prompt := effectiveInstructionWithTaskContract("persona", nil, &TaskContractProjection{
		Version: 3, TaskType: "change", Goal: "修复并发问题", RequestedOutcome: "实现并通过回归测试",
		AcceptanceCriteria: []TaskCriterionProjection{{ID: "tests", Description: "目标测试通过"}},
		Constraints:        []string{"保持公开 API 兼容"}, NonGoals: []string{"不提交 git"}, MutationAllowed: true,
		ValidationRequired: true, ExternalActions: []string{"denied: git push"}, SourceMessageIDs: []string{"message-1"},
	})
	for _, want := range []string{"version: 3", "task type: change", "修复并发问题", "[tests] 目标测试通过", "保持公开 API 兼容", "不提交 git", "denied: git push", "message-1", "validation required: yes", "Abot 任务执行契约"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("任务契约投影缺少 %q: %s", want, prompt)
		}
	}
}

func TestKernelLoadsTaskContractProjectionBeforeModelCall(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	kernel, err := NewKernel(Config{
		AppName: "contract-kernel-test", SessionService: session.InMemoryService(), Providers: registry, Instruction: "persona",
		TaskContractResolver: func(context.Context, string) (TaskContractProjection, error) {
			calls++
			return TaskContractProjection{Version: 1, TaskType: "change", Goal: "完成契约测试", MutationAllowed: true, ValidationRequired: true, SourceMessageIDs: []string{"message-1"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", InvocationID: "inv-1", SessionID: "session-1", ProviderID: "demo", ModelID: "model", Message: "执行任务"}))
	if result.err != nil {
		t.Fatal(result.err)
	}
	if calls != 1 || adapter.model == nil || !strings.Contains(adapter.model.instruction, "完成契约测试") || !strings.Contains(adapter.model.instruction, "version: 1") {
		t.Fatalf("任务契约未在模型调用前注入: calls=%d instruction=%q", calls, adapter.model.instruction)
	}
}

func TestKernelLoadsRuntimeSnapshotBeforeModelCall(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	kernel, err := NewKernel(Config{
		AppName: "snapshot-kernel-test", SessionService: session.InMemoryService(), Providers: registry, Instruction: "persona",
		RuntimeSnapshotResolver: func(context.Context, string) (RuntimeSnapshotProjection, error) {
			calls++
			return RuntimeSnapshotProjection{
				InvocationID: "inv-snapshot", Revision: 4, ContractVersion: 2, PlanRevision: 3,
				Phase: "waiting_approval", ActivePlanStepID: "verify", ActivePlanStepTitle: "运行验证",
				WorkflowStatus: "waiting", WorkflowBranchID: "branch-a", WorkflowBoundaryID: "boundary-4", WorkflowBoundaryKind: "approval", WorkflowBoundaryStatus: "waiting", WorkflowBoundaryWaitIDs: []string{"approval-1"}, WorkflowActiveBoundaries: []string{"boundary-4"},
				PendingApprovals: []string{"approval-1"}, Blockers: []string{"tool_failed: 测试失败"},
				Budget: RuntimeBudgetProjection{ContextWindow: 16000, OutputReserve: 1000, SafetyReserve: 500, EstimatedInput: 1200, ToolCallsUsed: 2, ToolCallsLimit: 3},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", InvocationID: "inv-snapshot", SessionID: "session-snapshot", ProviderID: "demo", ModelID: "model", Message: "继续"}))
	if result.err != nil {
		t.Fatal(result.err)
	}
	if calls != 1 || adapter.model == nil {
		t.Fatalf("Runtime Snapshot resolver/model 调用次数不正确: calls=%d model=%#v", calls, adapter.model)
	}
	for _, want := range []string{"Durable Runtime Snapshot", "revision: 4", "phase: waiting_approval", "workflow status: waiting", "workflow branch: branch-a", "active workflow boundary: boundary-4 (approval, waiting)", "workflow boundary pending waits", "workflow active boundaries", "contract version: 2", "plan revision: 3", "active plan step: verify", "approval-1", "tool_failed: 测试失败", "context_window=16000"} {
		if !strings.Contains(adapter.model.instruction, want) {
			t.Fatalf("Runtime Snapshot 未在模型调用前注入 %q: %s", want, adapter.model.instruction)
		}
	}
}

func TestKernelLoadsWorkingSetMetadataBeforeModelCall(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true}}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := NewKernel(Config{
		AppName: "working-set-kernel-test", SessionService: session.InMemoryService(), Providers: registry, Instruction: "persona",
		WorkingSetResolver: func(context.Context, string) ([]WorkingSetItem, error) {
			return []WorkingSetItem{{ID: "slice-1", Kind: "file_slice", SourceRef: "file:main.go", Priority: WorkingSetPriorityP1, RelevanceScore: 1, TokenEstimate: 5, ContentDigest: "sha", Freshness: WorkingSetFresh, Summary: "入口函数"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", InvocationID: "inv-working-set", SessionID: "session-working-set", ProviderID: "demo", ModelID: "model", Message: "检查"}))
	if result.err != nil {
		t.Fatal(result.err)
	}
	if adapter.model == nil || !strings.Contains(adapter.model.instruction, "Abot Working Set") || !strings.Contains(adapter.model.instruction, "slice-1") || !strings.Contains(adapter.model.instruction, "file:main.go") {
		t.Fatalf("Working Set 未注入模型指令: %q", adapter.model.instruction)
	}
}

func TestKernelStopsBeforeProviderWhenContextBudgetIsExceeded(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true, ContextWindow: 64, MaxOutputTokens: 32}}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := NewKernel(Config{AppName: "budget-kernel-test", SessionService: session.InMemoryService(), Providers: registry, Instruction: strings.Repeat("非常长的固定规则 ", 100)})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", SessionID: "session-budget", ProviderID: "demo", ModelID: "model", Message: "继续"}))
	if !errors.Is(result.err, ErrContextBudgetExceeded) {
		t.Fatalf("超窗应在 provider call 前返回 ContextBudgetError，实际=%v", result.err)
	}
}

func TestKernelUsesUnknownContextFallbackBeforeProvider(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, BaseURL: "http://127.0.0.1:9999/v1", Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true, MaxOutputTokens: 64}}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	var manifest ContextManifest
	kernel, err := NewKernel(Config{
		AppName: "unknown-window-budget-kernel", SessionService: session.InMemoryService(), Providers: registry,
		Instruction:             strings.Repeat("固定规则 ", 3000),
		ContextManifestObserver: func(_ context.Context, got ContextManifest) { manifest = got },
	})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", SessionID: "unknown-window-budget-session", ProviderID: "demo", ModelID: "model", Message: "继续"}))
	if !errors.Is(result.err, ErrContextBudgetExceeded) {
		t.Fatalf("未知窗口使用保守兜底后应在 provider call 前失败: %v", result.err)
	}
	if adapter.model == nil || adapter.model.calls != 0 {
		var calls int
		if adapter.model != nil {
			calls = adapter.model.calls
		}
		t.Fatalf("未知窗口超窗不应调用 provider: model=%#v calls=%d", adapter.model, calls)
	}
	fallbackWarning := false
	for _, warning := range manifest.Warnings {
		fallbackWarning = fallbackWarning || warning == "context_window_fallback"
	}
	if manifest.ContextWindow != 8192 || !fallbackWarning {
		t.Fatalf("未知窗口应在 manifest 标记保守兜底: %#v", manifest)
	}
}

func TestKernelFreezesToolSetAndModelCapabilitySnapshots(t *testing.T) {
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true, ContextWindow: 8192}}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	echo, err := functiontool.New(functiontool.Config{Name: "echo", Description: "返回输入"}, func(_ adkagent.Context, args struct {
		Text string `json:"text"`
	}) (struct {
		Text string `json:"text"`
	}, error) {
		return args, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	toolRegistry := NewToolRegistry()
	var toolSnapshot ToolSetSnapshot
	var capabilitySnapshot provider.CapabilitySnapshot
	kernel, err := NewKernel(Config{
		AppName: "snapshot-tool-kernel", SessionService: session.InMemoryService(), Providers: registry,
		Tools: []tool.Tool{echo}, ToolRegistry: toolRegistry,
		ToolSetSnapshotObserver: func(_ context.Context, snapshot ToolSetSnapshot) error { toolSnapshot = snapshot; return nil },
		ModelCapabilityObserver: func(_ context.Context, snapshot provider.CapabilitySnapshot) error {
			capabilitySnapshot = snapshot
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", InvocationID: "inv-snapshots", SessionID: "session-snapshots", ProviderID: "demo", ModelID: "model", Message: "测试"}))
	if result.err != nil {
		t.Fatal(result.err)
	}
	if toolSnapshot.InvocationID != "inv-snapshots" || len(toolSnapshot.Tools) != 1 || toolSnapshot.Tools[0].ModelName != "echo" {
		t.Fatalf("ToolSet Snapshot=%#v", toolSnapshot)
	}
	if capabilitySnapshot.InvocationID != "inv-snapshots" || capabilitySnapshot.Result.ProfileSnapshotID == "" || !capabilitySnapshot.Result.Compatible {
		t.Fatalf("能力 Snapshot=%#v", capabilitySnapshot)
	}
}

func TestKernelAppliesNegotiatedRequestPlanToProviderConfig(t *testing.T) {
	profile := provider.DefaultCapabilities(provider.Provider{ID: "demo", Protocol: provider.ProtocolOpenAICompatible}, provider.Model{ID: "model", MaxOutputTokens: 128})
	profile.Streaming = provider.Support{State: provider.SupportUnsupported, Source: "user_override"}
	repo := &kernelTestRepository{providers: map[string]provider.Provider{
		"demo": {ID: "demo", Name: "demo", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", DisplayName: "model", Enabled: true, MaxOutputTokens: 128, Capabilities: &profile}}},
	}}
	adapter := &kernelTestAdapter{}
	registry, err := provider.NewRegistry(context.Background(), repo, map[provider.Protocol]provider.Adapter{provider.ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := NewKernel(Config{AppName: "request-plan-kernel", SessionService: session.InMemoryService(), Providers: registry, Instruction: "回复测试"})
	if err != nil {
		t.Fatal(err)
	}
	result := collectKernelEvents(kernel.Run(context.Background(), ChatRequest{UserID: "user", SessionID: "request-plan-session", ProviderID: "demo", ModelID: "model", Message: "测试", Stream: true}))
	if result.err != nil {
		t.Fatal(result.err)
	}
	if adapter.model == nil || adapter.model.requestConfig == nil {
		t.Fatal("provider 未收到 GenerateContentConfig")
	}
	if adapter.model.requestConfig.MaxOutputTokens != 128 {
		t.Fatalf("协商后的 max output 未应用: %d", adapter.model.requestConfig.MaxOutputTokens)
	}
	if adapter.model.requestConfig.ToolConfig == nil || adapter.model.requestConfig.ToolConfig.FunctionCallingConfig == nil || adapter.model.requestConfig.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeAuto {
		t.Fatalf("协商后的 tool choice 未应用: %#v", adapter.model.requestConfig.ToolConfig)
	}
}

func TestModelCallMetadataLLMAnnotatesResponses(t *testing.T) {
	request := &adkmodel.LLMRequest{}
	wrapped := &modelCallMetadataLLM{
		inner: &kernelTestLLM{name: "metadata-test"},
		idFor: func(got *adkmodel.LLMRequest) string {
			if got != request {
				t.Fatalf("模型调用 correlation 收到错误 request 指针")
			}
			return "invocation:model:1"
		},
	}
	var response *adkmodel.LLMResponse
	for item, err := range wrapped.GenerateContent(context.Background(), request, false) {
		if err != nil {
			t.Fatal(err)
		}
		response = item
	}
	if response == nil || response.CustomMetadata["abot_model_call_id"] != "invocation:model:1" {
		t.Fatalf("模型调用 correlation 未写入 metadata: %#v", response)
	}
}
