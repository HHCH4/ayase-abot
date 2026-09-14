package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type toolExecutorTestContext struct {
	adkagent.ContextMock
	context.Context
}

func (c *toolExecutorTestContext) Deadline() (time.Time, bool) { return c.Context.Deadline() }
func (c *toolExecutorTestContext) Done() <-chan struct{}       { return c.Context.Done() }
func (c *toolExecutorTestContext) Err() error                  { return c.Context.Err() }
func (c *toolExecutorTestContext) Value(key any) any           { return c.Context.Value(key) }

func TestToolExecutorRedactsAndProjectsLargeResults(t *testing.T) {
	ctx := &toolExecutorTestContext{Context: context.Background()}
	executor := NewToolExecutor(ToolExecutorOptions{})
	result, err := executor.execute(ctx, ToolDescriptor{
		ID: "builtin.test", ModelName: "test", Version: "1.0.0", Availability: ToolAvailability{Status: ToolStatusReady},
		Limits: ToolLimits{MaxInlineResultBytes: 32},
	}, func(adkagent.Context, any) (map[string]any, error) {
		return map[string]any{"token": "secret-value", "nested": map[string]any{"password": "pw"}, "value": "this result is intentionally long"}, nil
	}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if result["truncated"] != true || result["full_result_ref"] == "" {
		t.Fatalf("large result should be projected: %#v", result)
	}
	if result["result_digest"] == "" {
		t.Fatalf("projection should retain a digest: %#v", result)
	}
}

func TestToolExecutorRejectsInputAndPolicy(t *testing.T) {
	ctx := &toolExecutorTestContext{Context: context.Background()}
	run := func(adkagent.Context, any) (map[string]any, error) {
		t.Fatal("denied tool must not run")
		return nil, nil
	}
	executor := NewToolExecutor(ToolExecutorOptions{Policy: func(context.Context, ToolDescriptor, map[string]any) (ToolPolicy, string) {
		return ToolPolicyDeny, "policy denied"
	}})
	_, err := executor.execute(ctx, ToolDescriptor{ID: "builtin.test", ModelName: "test", Version: "1.0.0", Availability: ToolAvailability{Status: ToolStatusReady}, Limits: ToolLimits{MaxInputBytes: 4}}, run, map[string]any{"value": "too large"})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != "input_too_large" {
		t.Fatalf("input limit should fail before policy/run: %v", err)
	}
	_, err = executor.execute(ctx, ToolDescriptor{ID: "builtin.test", ModelName: "test", Version: "1.0.0", Availability: ToolAvailability{Status: ToolStatusReady}}, run, map[string]any{})
	if !errors.As(err, &toolErr) || toolErr.Code != "tool_denied" || toolErr.Category != ToolErrorAuthorization {
		t.Fatalf("policy denial should be stable ToolError: %v", err)
	}
}

func TestToolExecutorValidatesBoundedInputSchema(t *testing.T) {
	executor := NewToolExecutor(ToolExecutorOptions{})
	descriptor := ToolDescriptor{ID: "builtin.schema", ModelName: "schema", Version: "1.0.0", Availability: ToolAvailability{Status: ToolStatusReady}, InputSchema: json.RawMessage(`{"type":"object","required":["path"],"additionalProperties":false,"properties":{"path":{"type":"string"}}}`)}
	ctx := &toolExecutorTestContext{ContextMock: adkagent.ContextMock{}, Context: context.Background()}
	_, err := executor.execute(ctx, descriptor, func(_ adkagent.Context, _ any) (map[string]any, error) { return map[string]any{"ok": true}, nil }, map[string]any{"other": "x"})
	if err == nil || !strings.Contains(err.Error(), "必填") {
		t.Fatalf("缺少 required 字段应失败: %v", err)
	}
	_, err = executor.execute(ctx, descriptor, func(_ adkagent.Context, _ any) (map[string]any, error) { return map[string]any{"ok": true}, nil }, map[string]any{"path": "/tmp/a", "other": true})
	if err == nil || !strings.Contains(err.Error(), "未知字段") {
		t.Fatalf("additionalProperties=false 应拒绝未知字段: %v", err)
	}
	_, err = executor.execute(ctx, descriptor, func(_ adkagent.Context, _ any) (map[string]any, error) { return map[string]any{"ok": true}, nil }, map[string]any{"path": 1})
	if err == nil || !strings.Contains(err.Error(), "类型") {
		t.Fatalf("字段类型错误应失败: %v", err)
	}
}

type executableToolTestDouble struct{}

func (executableToolTestDouble) Name() string        { return "double" }
func (executableToolTestDouble) Description() string { return "test" }
func (executableToolTestDouble) IsLongRunning() bool { return false }
func (executableToolTestDouble) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "double"}
}
func (executableToolTestDouble) ProcessRequest(_ adkagent.Context, _ *adkmodel.LLMRequest) error {
	return nil
}
func (executableToolTestDouble) Run(_ adkagent.Context, _ any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func TestToolRegistryWrapSelectionDelegatesRunnableTool(t *testing.T) {
	registry := NewToolRegistry()
	descriptor, err := registry.RegisterRuntimeTool(executableToolTestDouble{}, ToolSourceBuiltin)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := registry.Select(ToolSelectionRequest{InvocationID: "inv-wrap", AllowedTools: []string{descriptor.ModelName}})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := registry.WrapSelection(selection, nil)
	if len(wrapped) != 1 {
		t.Fatalf("wrapped tools=%d", len(wrapped))
	}
	runnable, ok := wrapped[0].(interface {
		Run(adkagent.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("wrapped tool should expose runnable interface: %T", wrapped[0])
	}
	result, err := runnable.Run(&toolExecutorTestContext{Context: context.Background()}, map[string]any{})
	if err != nil || result["ok"] != true {
		t.Fatalf("wrapped run result=%#v err=%v", result, err)
	}
}

func TestToolRegistryWrapSelectionAppliesNegotiatedParallelLimit(t *testing.T) {
	registry := NewToolRegistry()
	descriptor, err := registry.RegisterRuntimeTool(executableToolTestDouble{}, ToolSourceBuiltin)
	if err != nil {
		t.Fatal(err)
	}
	descriptor.Limits.MaxConcurrency = 8
	// Re-registering a changed descriptor is intentionally a conflict. Build a
	// selection directly to exercise the wrapper's conservative clamp without
	// mutating the registry catalog.
	selection := ToolSelection{Tools: []adktool.Tool{executableToolTestDouble{}}, Descriptors: []ToolDescriptor{descriptor}}
	wrapped := registry.WrapSelection(selection, nil, 1)
	if len(wrapped) != 1 {
		t.Fatalf("wrapped tools=%d", len(wrapped))
	}
	wrappedTool, ok := wrapped[0].(*executableTool)
	if !ok || wrappedTool.permits == nil || cap(wrappedTool.permits) != 1 {
		t.Fatalf("协商并发上限未收紧: %#v", wrapped[0])
	}
}
