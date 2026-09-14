package runtime

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestInvocationTraceDerivesDurationsAndKeepsUnknownUsage(t *testing.T) {
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-trace", UserID: "user", ConversationID: "conversation", SessionID: "conversation", ProviderID: "demo", ModelID: "model", Status: InvocationCompleted, CreatedAt: base, UpdatedAt: base.Add(3 * time.Second)}
	started := base.Add(100 * time.Millisecond)
	finished := base.Add(3 * time.Second)
	invocation.StartedAt, invocation.FinishedAt = &started, &finished
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	events := []AgentEvent{
		{ID: "event-start", InvocationID: invocation.ID, Type: EventInvocationStarted, Timestamp: started},
		{ID: "event-usage", InvocationID: invocation.ID, Type: EventUsageUpdated, Timestamp: base.Add(200 * time.Millisecond), Data: map[string]any{"usage": map[string]any{"promptTokenCount": 10, "cachedContentTokenCount": 2, "candidatesTokenCount": 5, "thoughtsTokenCount": 1, "totalTokenCount": 16}}},
		{ID: "event-usage-missing", InvocationID: invocation.ID, Type: EventUsageUpdated, Timestamp: base.Add(250 * time.Millisecond), Data: map[string]any{"usage": map[string]any{"promptTokenCount": 4, "totalTokenCount": 6}}},
		{ID: "event-command", InvocationID: invocation.ID, Type: EventCommandOutput, Timestamp: base.Add(300 * time.Millisecond), Data: map[string]any{"command_run_id": "run-1", "stream": "stdout", "data": "abc"}},
		{ID: "event-compact", InvocationID: invocation.ID, Type: EventContextCompacted, Timestamp: base.Add(400 * time.Millisecond)},
		{ID: "event-notice", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: base.Add(500 * time.Millisecond), Data: map[string]any{"code": "context_budget_exhausted", "prompt": "must not leak"}},
		{ID: "event-context-recovery", InvocationID: invocation.ID, Type: EventRuntimeNotice, Timestamp: base.Add(600 * time.Millisecond), Data: map[string]any{"code": "context_limit_exceeded", "attempt": 1, "recovered": true, "model_call_id": "model-call-2"}},
	}
	for _, event := range events {
		if _, err := repo.AppendEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	toolFinished := base.Add(700 * time.Millisecond)
	if err := repo.CreateToolCall(context.Background(), ToolCall{ID: "tool-1", InvocationID: invocation.ID, ToolName: "read_file", OriginalCallID: "call-1", Status: ToolCallCompleted, CreatedAt: base.Add(500 * time.Millisecond), UpdatedAt: toolFinished, FinishedAt: &toolFinished}); err != nil {
		t.Fatal(err)
	}
	approvalResolved := base.Add(1800 * time.Millisecond)
	if err := repo.CreateApproval(context.Background(), Approval{ID: "approval-1", InvocationID: invocation.ID, ToolCallID: "tool-1", ToolName: "write_file", Status: ApprovalApproved, CreatedAt: base.Add(800 * time.Millisecond), UpdatedAt: approvalResolved, ResolvedAt: &approvalResolved}); err != nil {
		t.Fatal(err)
	}
	actual := 110
	if err := repo.SaveContextManifest(context.Background(), ContextManifest{ID: "manifest-1", InvocationID: invocation.ID, ModelCallID: "model-call-1", Model: "model", EstimatedInput: 100, ActualInput: &actual, ContextWindow: 1000, CreatedAt: base.Add(200 * time.Millisecond), Digest: "digest"}); err != nil {
		t.Fatal(err)
	}
	verificationFinished := base.Add(2 * time.Second)
	if err := repo.CreateVerificationRun(context.Background(), VerificationRun{ID: "verify-1", InvocationID: invocation.ID, Kind: "test", Status: VerificationPassed, CreatedAt: base.Add(1900 * time.Millisecond), UpdatedAt: verificationFinished, FinishedAt: &verificationFinished, Revision: 1, OutputDigest: "sha256:abc"}); err != nil {
		t.Fatal(err)
	}

	trace := buildInvocationTrace(invocation, events, []ToolCall{{ID: "tool-1", InvocationID: invocation.ID, ToolName: "read_file", OriginalCallID: "call-1", Status: ToolCallCompleted, CreatedAt: base.Add(500 * time.Millisecond), UpdatedAt: toolFinished, FinishedAt: &toolFinished}}, []Approval{{ID: "approval-1", InvocationID: invocation.ID, ToolCallID: "tool-1", ToolName: "write_file", Status: ApprovalApproved, CreatedAt: base.Add(800 * time.Millisecond), UpdatedAt: approvalResolved, ResolvedAt: &approvalResolved}}, []ContextManifest{{ID: "manifest-1", InvocationID: invocation.ID, ModelCallID: "model-call-1", Model: "model", EstimatedInput: 100, ActualInput: &actual, ContextWindow: 1000, CreatedAt: base.Add(200 * time.Millisecond), Digest: "digest"}}, []VerificationRun{{ID: "verify-1", InvocationID: invocation.ID, Kind: "test", Status: VerificationPassed, CreatedAt: base.Add(1900 * time.Millisecond), UpdatedAt: verificationFinished, FinishedAt: &verificationFinished, Revision: 1, OutputDigest: "sha256:abc"}}, base.Add(3*time.Second))
	if trace.Status != InvocationCompleted || trace.Metrics.TotalWallMS != 3000 || trace.Metrics.QueueMS != 100 || trace.Metrics.ApprovalWaitMS != 1000 || trace.Metrics.ToolCalls != 1 || trace.Metrics.CommandChunks != 1 || trace.Metrics.CommandOutputBytes != 3 {
		t.Fatalf("Trace 指标不正确: %#v", trace.Metrics)
	}
	if trace.Usage.ModelCalls != 2 || !trace.Usage.PromptTokens.Known || trace.Usage.PromptTokens.Value != 14 || trace.Usage.OutputTokens.Known || trace.Usage.TotalTokens.Value != 22 || trace.Usage.UnknownCalls != 0 {
		t.Fatalf("缺失 usage 应保持 unknown: %#v", trace.Usage)
	}
	if !trace.Usage.TotalTokens.Known || trace.Usage.TotalTokens.Value != 22 {
		t.Fatalf("完整 usage 未正确聚合: %#v", trace.Usage)
	}
	if trace.Metrics.ContextEstimateSamples != 1 || trace.Metrics.ContextEstimateError != 10 {
		t.Fatalf("Context 估算误差不正确: %#v", trace.Metrics)
	}
	if trace.Metrics.ContextLimitRetries != 1 {
		t.Fatalf("context-limit recovery 指标不正确: %#v", trace.Metrics)
	}
	if trace.Usage.ContextCalibration.Samples != 1 || trace.Usage.ContextCalibration.EstimatedTokens != 100 || trace.Usage.ContextCalibration.ActualTokens != 110 || trace.Usage.ContextCalibration.ErrorTokens != 10 || trace.Usage.ContextCalibration.AbsoluteErrorTokens != 10 {
		t.Fatalf("Context calibration summary 不正确: %#v", trace.Usage.ContextCalibration)
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "must not leak") || strings.Contains(string(encoded), "read_file") == false {
		t.Fatalf("Trace 脱敏或工具元数据不正确: %s", encoded)
	}
}

func TestUsageNormalizationAcceptsProviderAliasesAndNestedEnvelopes(t *testing.T) {
	usage := usageFromEvents([]AgentEvent{
		{Type: EventUsageUpdated, Data: map[string]any{"usageMetadata": map[string]any{
			"input_tokens":                "17",
			"prompt_tokens_details":       map[string]any{"cached_tokens": json.Number("3")},
			"tool_use_prompt_token_count": uint32(5),
			"output_tokens":               float32(7),
			"output_tokens_details":       map[string]any{"reasoning_tokens": int16(2)},
			"total_tokens":                int64(31),
		}}},
	})
	if !usage.PromptTokens.Known || usage.PromptTokens.Value != 17 {
		t.Fatalf("OpenAI input_tokens 未归一化: %#v", usage.PromptTokens)
	}
	if !usage.CachedInputTokens.Known || usage.CachedInputTokens.Value != 3 {
		t.Fatalf("嵌套 cached_tokens 未归一化: %#v", usage.CachedInputTokens)
	}
	if !usage.ToolUsePromptTokens.Known || usage.ToolUsePromptTokens.Value != 5 {
		t.Fatalf("Gemini tool_use_prompt_tokens 未归一化: %#v", usage.ToolUsePromptTokens)
	}
	if !usage.OutputTokens.Known || usage.OutputTokens.Value != 7 || !usage.ReasoningTokens.Known || usage.ReasoningTokens.Value != 2 || !usage.TotalTokens.Known || usage.TotalTokens.Value != 31 {
		t.Fatalf("provider usage 字段未完整归一化: %#v", usage)
	}
}

func TestUsageNormalizationRejectsUnsafeNumbersAndOverflow(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{name: "negative", value: -1},
		{name: "fractional", value: 1.5},
		{name: "nan", value: math.NaN()},
		{name: "infinity", value: math.Inf(1)},
		{name: "decimal string", value: "1.5"},
		{name: "overflow string", value: "9223372036854775808"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := usageFromEvents([]AgentEvent{{Type: EventUsageUpdated, Data: map[string]any{"usage": map[string]any{"input_tokens": tc.value}}}})
			if usage.PromptTokens.Known {
				t.Fatalf("不安全 token 值不应被标记为 known: %#v", usage.PromptTokens)
			}
		})
	}
	maxInt := int(^uint(0) >> 1)
	if got, ok := usageInteger(maxInt); !ok || got != maxInt {
		t.Fatalf("最大可表示 int 应可接受: got=%d ok=%v", got, ok)
	}
	if got, ok := usageInteger(uint64(maxInt) + 1); ok || got != 0 {
		t.Fatalf("超过 int 上限应拒绝: got=%d ok=%v", got, ok)
	}
}

func TestUsageNormalizationMarksAggregateOverflowUnknown(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	usage := usageFromEvents([]AgentEvent{
		{Type: EventUsageUpdated, Data: map[string]any{"usage": map[string]any{"input_tokens": maxInt}}},
		{Type: EventUsageUpdated, Data: map[string]any{"usage": map[string]any{"input_tokens": 1}}},
	})
	if usage.PromptTokens.Known {
		t.Fatalf("累计溢出不应返回伪造的 known 值: %#v", usage.PromptTokens)
	}
}

func TestUsageNormalizationKeepsMissingPayloadUnknownCall(t *testing.T) {
	usage := usageFromEvents([]AgentEvent{{Type: EventUsageUpdated, Data: map[string]any{"model_call_id": "call-without-usage"}}})
	if usage.ModelCalls != 1 || usage.UnknownCalls != 1 || len(usage.UnknownFields) != 0 {
		t.Fatalf("缺失 usage payload 应计为 unknown call: %#v", usage)
	}
}

func TestUsageSeparatesCompactionCallsFromUserModelCalls(t *testing.T) {
	usage := usageFromEvents([]AgentEvent{
		{Type: EventUsageUpdated, Data: map[string]any{"usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14}}},
		{Type: EventUsageUpdated, Data: map[string]any{"scope": "compaction", "usage": map[string]any{"promptTokenCount": 30, "candidatesTokenCount": 6, "totalTokenCount": 36}}},
		{Type: EventUsageUpdated, Data: map[string]any{"scope": "COMPACTION", "model_call_id": "compaction-without-usage"}},
	})
	if usage.ModelCalls != 1 || !usage.PromptTokens.Known || usage.PromptTokens.Value != 10 || !usage.TotalTokens.Known || usage.TotalTokens.Value != 14 {
		t.Fatalf("普通模型 usage 不应混入压缩调用: %#v", usage)
	}
	if usage.Compaction.ModelCalls != 2 || usage.Compaction.UnknownCalls != 1 {
		t.Fatalf("压缩调用次数或 unknown 统计不正确: %#v", usage.Compaction)
	}
	if !usage.Compaction.PromptTokens.Known || usage.Compaction.PromptTokens.Value != 30 || !usage.Compaction.OutputTokens.Known || usage.Compaction.OutputTokens.Value != 6 || !usage.Compaction.TotalTokens.Known || usage.Compaction.TotalTokens.Value != 36 {
		t.Fatalf("压缩 usage 未独立聚合: %#v", usage.Compaction)
	}

	encoded, err := json.Marshal(usage)
	if err != nil || !strings.Contains(string(encoded), `"compaction"`) {
		t.Fatalf("UsageSummary 应暴露独立 compaction breakdown: %s err=%v", encoded, err)
	}
}

func TestCoordinatorTraceAndUsageRejectMissingInvocation(t *testing.T) {
	coordinator := &Coordinator{repo: NewMemoryRepository()}
	if _, err := coordinator.GetInvocationTrace(context.Background(), "missing"); err == nil {
		t.Fatal("缺失 invocation 应拒绝读取 Trace")
	}
	if _, err := coordinator.GetInvocationUsage(context.Background(), "missing"); err == nil {
		t.Fatal("缺失 invocation 应拒绝读取 Usage")
	}
}
