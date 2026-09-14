package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// ToolError is the stable, model-facing error taxonomy for the execution
// wrapper. The original error is kept out of the model projection unless the
// caller explicitly includes it in Details.
type ToolError struct {
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Category   string         `json:"category"`
	Retryable  bool           `json:"retryable"`
	UserAction string         `json:"user_action,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	Cause      error          `json:"-"`
}

func (e *ToolError) Error() string {
	if e == nil {
		return "工具执行失败"
	}
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("工具执行失败: %s", e.Code)
	}
	return e.Message
}

func (e *ToolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

const (
	ToolErrorValidation     = "validation"
	ToolErrorAuthorization  = "authorization"
	ToolErrorApproval       = "approval"
	ToolErrorNotFound       = "not_found"
	ToolErrorConflict       = "conflict"
	ToolErrorStale          = "stale"
	ToolErrorTimeout        = "timeout"
	ToolErrorCancelled      = "cancelled"
	ToolErrorTransport      = "transport"
	ToolErrorDependency     = "dependency"
	ToolErrorRateLimit      = "rate_limit"
	ToolErrorResultTooLarge = "result_too_large"
	ToolErrorInternal       = "internal"
	ToolErrorUnknown        = "unknown"
)

// ToolExecutionPolicy is evaluated again at the execution boundary. The
// descriptor selection callback is intentionally not reused here because the
// normalized arguments and current runtime state may have changed.
type ToolExecutionPolicy func(context.Context, ToolDescriptor, map[string]any) (ToolPolicy, string)

type ToolExecutionRecord struct {
	ToolID          string
	Version         string
	ModelName       string
	CallID          string
	StartedAt       time.Time
	FinishedAt      time.Time
	Duration        time.Duration
	Outcome         string
	ErrorCode       string
	ResultBytes     int
	ResultTruncated bool
}

type ToolExecutionObserver func(context.Context, ToolExecutionRecord)

type ToolExecutorOptions struct {
	Policy   ToolExecutionPolicy
	Observer ToolExecutionObserver
}

// ToolExecutor applies the descriptor limits that can be enforced without
// knowing the concrete tool implementation. A timeout uses a derived context
// and is therefore cooperative: an implementation that ignores cancellation
// remains responsible for its own process-group termination.
type ToolExecutor struct {
	policy   ToolExecutionPolicy
	observer ToolExecutionObserver
}

func NewToolExecutor(options ToolExecutorOptions) *ToolExecutor {
	return &ToolExecutor{policy: options.Policy, observer: options.Observer}
}

// SetObserver installs the metadata-only audit sink before a run starts. The
// observer never receives tool arguments or result bodies.
func (e *ToolExecutor) SetObserver(observer ToolExecutionObserver) {
	if e == nil {
		return
	}
	e.observer = observer
}

type toolRun func(adkagent.Context, any) (map[string]any, error)

func (e *ToolExecutor) execute(ctx adkagent.Context, descriptor ToolDescriptor, run toolRun, args any) (map[string]any, error) {
	started := time.Now().UTC()
	record := ToolExecutionRecord{ToolID: descriptor.ID, Version: descriptor.Version, ModelName: descriptor.ModelName, StartedAt: started}
	if ctx != nil {
		record.CallID = strings.TrimSpace(ctx.FunctionCallID())
	}
	finish := func(result map[string]any, err error) (map[string]any, error) {
		record.FinishedAt = time.Now().UTC()
		record.Duration = record.FinishedAt.Sub(record.StartedAt)
		if err != nil {
			record.Outcome = "failed"
			var toolErr *ToolError
			if errors.As(err, &toolErr) && toolErr != nil {
				record.ErrorCode = toolErr.Code
			}
		} else {
			record.Outcome = "completed"
		}
		if encoded, marshalErr := json.Marshal(result); marshalErr == nil {
			record.ResultBytes = len(encoded)
		}
		if e != nil && e.observer != nil {
			e.observer(ctx, record)
		}
		return result, err
	}
	if ctx == nil {
		return finish(nil, toolExecutionError("tool_context_missing", ToolErrorInternal, false, "工具执行上下文缺失", nil))
	}
	if descriptor.Availability.Status != "" && descriptor.Availability.Status != ToolStatusReady && descriptor.Availability.Status != ToolStatusDegraded {
		return finish(nil, toolExecutionError("tool_unavailable", ToolErrorDependency, false, "工具当前不可用", nil))
	}
	encodedArgs, marshalErr := json.Marshal(args)
	if marshalErr != nil {
		return finish(nil, toolExecutionError("invalid_arguments", ToolErrorValidation, false, "工具参数不是合法 JSON", marshalErr))
	}
	if descriptor.Limits.MaxInputBytes > 0 && len(encodedArgs) > descriptor.Limits.MaxInputBytes {
		return finish(nil, toolExecutionError("input_too_large", ToolErrorValidation, false, "工具参数超过输入大小限制", nil))
	}
	arguments, ok := args.(map[string]any)
	if !ok {
		arguments = map[string]any{}
	}
	if err := validateToolArguments(descriptor.InputSchema, arguments, ok); err != nil {
		return finish(nil, toolExecutionError("invalid_arguments", ToolErrorValidation, false, err.Error(), nil))
	}
	if e != nil && e.policy != nil {
		decision, reason := e.policy(ctx, descriptor, arguments)
		switch decision {
		case ToolPolicyDeny:
			return finish(nil, toolExecutionError("tool_denied", ToolErrorAuthorization, false, firstToolMessage(reason, "当前策略拒绝此工具调用"), nil))
		case ToolPolicyAsk:
			payload := map[string]any{"tool_id": descriptor.ID, "version": descriptor.Version, "schema_digest": descriptor.SchemaDigest}
			if err := ctx.RequestConfirmation(firstToolMessage(reason, "该工具调用需要审批"), payload); err != nil {
				return finish(nil, toolExecutionError("approval_request_failed", ToolErrorApproval, false, "请求工具审批失败", err))
			}
			if actions := ctx.Actions(); actions != nil {
				actions.SkipSummarization = true
			}
			return finish(nil, toolExecutionError("approval_required", ToolErrorApproval, false, "等待用户批准工具调用", nil))
		case ToolPolicyAllow, "":
		default:
			return finish(nil, toolExecutionError("invalid_policy_decision", ToolErrorAuthorization, false, "工具策略返回了未知决策", nil))
		}
	}
	if descriptor.Limits.TimeoutSeconds > 0 {
		deadline := time.Duration(descriptor.Limits.TimeoutSeconds) * time.Second
		if existing, ok := ctx.Deadline(); !ok || time.Until(existing) > deadline {
			derived, cancel := context.WithTimeout(ctx, deadline)
			defer cancel()
			ctx = &toolExecutionContext{Context: ctx, deadline: derived}
		}
	}
	result, err := run(ctx, args)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return finish(nil, toolExecutionError("tool_timeout", ToolErrorTimeout, true, "工具执行超时", err))
		}
		if errors.Is(err, context.Canceled) {
			return finish(nil, toolExecutionError("tool_cancelled", ToolErrorCancelled, false, "工具执行已取消", err))
		}
		return finish(nil, toolExecutionError("tool_failed", ToolErrorUnknown, false, "工具执行失败", err))
	}
	if redacted, ok := redactToolValue(result).(map[string]any); ok {
		result = redacted
	}
	if descriptor.Limits.MaxInlineResultBytes > 0 {
		if encoded, marshalErr := json.Marshal(result); marshalErr == nil && len(encoded) > descriptor.Limits.MaxInlineResultBytes {
			sum := sha256.Sum256(encoded)
			projected := map[string]any{
				"truncated":         true,
				"full_result_ref":   "inline-result:" + hex.EncodeToString(sum[:])[:24],
				"returned_bytes":    0,
				"total_bytes":       len(encoded),
				"result_digest":     hex.EncodeToString(sum[:]),
				"projection_reason": "max_inline_result_bytes",
			}
			record.ResultTruncated = true
			return finish(projected, nil)
		}
	}
	return finish(result, nil)
}

// validateToolArguments intentionally implements the bounded JSON-Schema
// subset needed by host tools (object/required/properties/additionalProperties
// and primitive types). Complex schemas are still validated by the concrete
// tool; this guard prevents missing or unexpected top-level arguments from
// silently crossing the Runtime boundary.
func validateToolArguments(schema json.RawMessage, arguments map[string]any, objectInput bool) error {
	if len(schema) == 0 || string(schema) == "null" {
		return nil
	}
	var definition struct {
		Type                 string                     `json:"type"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &definition); err != nil {
		return errors.New("工具参数 Schema 无效")
	}
	if definition.Type != "" && definition.Type != "object" {
		return nil
	}
	if !objectInput && (definition.Type == "object" || len(definition.Required) > 0 || len(definition.Properties) > 0) {
		return errors.New("工具参数必须是对象")
	}
	for _, key := range definition.Required {
		if _, exists := arguments[key]; !exists {
			return fmt.Errorf("工具参数缺少必填字段 %q", key)
		}
	}
	if definition.AdditionalProperties != nil && !*definition.AdditionalProperties {
		for key := range arguments {
			if _, exists := definition.Properties[key]; !exists {
				return fmt.Errorf("工具参数包含未知字段 %q", key)
			}
		}
	}
	for key, raw := range definition.Properties {
		value, exists := arguments[key]
		if !exists || value == nil {
			continue
		}
		var property struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &property) != nil || property.Type == "" {
			continue
		}
		if !toolValueMatchesType(value, property.Type) {
			return fmt.Errorf("工具参数字段 %q 类型不匹配", key)
		}
	}
	return nil
}

func toolValueMatchesType(value any, expected string) bool {
	switch expected {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
			return true
		default:
			return false
		}
	case "integer":
		switch number := value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return number == float64(int64(number))
		case json.Number:
			_, err := number.Int64()
			return err == nil
		default:
			return false
		}
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

func toolExecutionError(code, category string, retryable bool, message string, cause error) *ToolError {
	return &ToolError{Code: code, Category: category, Retryable: retryable, Message: message, Cause: cause}
}

func firstToolMessage(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

// toolExecutionContext keeps the ADK context methods while replacing the
// embedded context.Context methods with the bounded derived context.
type toolExecutionContext struct {
	adkagent.Context
	deadline context.Context
}

func (c *toolExecutionContext) Deadline() (time.Time, bool) { return c.deadline.Deadline() }
func (c *toolExecutionContext) Done() <-chan struct{}       { return c.deadline.Done() }
func (c *toolExecutionContext) Err() error                  { return c.deadline.Err() }
func (c *toolExecutionContext) Value(key any) any           { return c.deadline.Value(key) }

func redactToolValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(item))
		for key, nested := range item {
			if isSensitiveToolKey(key) {
				result[key] = "[REDACTED]"
				continue
			}
			result[key] = redactToolValue(nested)
		}
		return result
	case []any:
		result := make([]any, len(item))
		for index, nested := range item {
			result[index] = redactToolValue(nested)
		}
		return result
	default:
		return value
	}
}

func isSensitiveToolKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
	for _, part := range []string{"password", "passwd", "secret", "token", "api_key", "apikey", "authorization", "cookie", "private_key"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

type executableTool struct {
	inner      adktool.Tool
	descriptor ToolDescriptor
	executor   *ToolExecutor
	permits    chan struct{}
	calls      atomic.Int64
}

func (t *executableTool) Name() string        { return t.inner.Name() }
func (t *executableTool) Description() string { return t.inner.Description() }
func (t *executableTool) IsLongRunning() bool { return t.inner.IsLongRunning() }

func (t *executableTool) Declaration() *genai.FunctionDeclaration {
	if declaration, ok := t.inner.(interface {
		Declaration() *genai.FunctionDeclaration
	}); ok {
		return declaration.Declaration()
	}
	return nil
}

func (t *executableTool) ProcessRequest(ctx adkagent.Context, request *adkmodel.LLMRequest) error {
	if processor, ok := t.inner.(interface {
		ProcessRequest(adkagent.Context, *adkmodel.LLMRequest) error
	}); ok {
		if err := processor.ProcessRequest(ctx, request); err != nil {
			return err
		}
		// Function tools register themselves in req.Tools. Replace that value
		// after delegation so the runtime wrapper remains the actual executor.
		if request != nil && request.Tools != nil {
			request.Tools[t.Name()] = t
		}
		return nil
	}
	return toolutils.PackTool(request, t)
}

func (t *executableTool) Run(ctx adkagent.Context, args any) (map[string]any, error) {
	if t == nil || t.inner == nil {
		return nil, toolExecutionError("tool_not_found", ToolErrorNotFound, false, "工具实现不存在", nil)
	}
	if ctx == nil {
		return nil, toolExecutionError("tool_context_missing", ToolErrorInternal, false, "工具执行上下文缺失", nil)
	}
	if t.descriptor.Limits.MaxCallsPerInvocation > 0 && int(t.calls.Add(1)) > t.descriptor.Limits.MaxCallsPerInvocation {
		return nil, toolExecutionError("tool_call_limit", ToolErrorAuthorization, false, "已达到该工具的 Invocation 调用上限", nil)
	}
	if t.permits != nil {
		select {
		case t.permits <- struct{}{}:
			defer func() { <-t.permits }()
		case <-ctx.Done():
			return nil, toolExecutionError("tool_cancelled", ToolErrorCancelled, false, "工具等待并发许可时已取消", ctx.Err())
		}
	}
	runnable, ok := t.inner.(interface {
		Run(adkagent.Context, any) (map[string]any, error)
	})
	if !ok {
		return nil, toolExecutionError("tool_not_runnable", ToolErrorDependency, false, "工具没有可执行接口", nil)
	}
	if t.executor == nil {
		return runnable.Run(ctx, args)
	}
	return t.executor.execute(ctx, t.descriptor, runnable.Run, args)
}

// WrapSelection returns per-invocation wrappers. The original ToolSet
// Snapshot remains descriptor-only, while every runnable selected for ADK is
// routed through the execution boundary.
func (r *ToolRegistry) WrapSelection(selection ToolSelection, executor *ToolExecutor, parallelLimits ...int) []adktool.Tool {
	if len(selection.Tools) == 0 {
		return nil
	}
	if executor == nil {
		executor = NewToolExecutor(ToolExecutorOptions{})
	}
	result := make([]adktool.Tool, 0, len(selection.Tools))
	parallelLimit := 0
	if len(parallelLimits) > 0 && parallelLimits[0] > 0 {
		parallelLimit = parallelLimits[0]
	}
	for index, implementation := range selection.Tools {
		if implementation == nil || index >= len(selection.Descriptors) {
			continue
		}
		descriptor := selection.Descriptors[index]
		var permits chan struct{}
		concurrency := descriptor.Limits.MaxConcurrency
		if parallelLimit > 0 && (concurrency <= 0 || parallelLimit < concurrency) {
			concurrency = parallelLimit
		}
		if concurrency > 0 {
			permits = make(chan struct{}, concurrency)
		}
		result = append(result, &executableTool{inner: implementation, descriptor: descriptor, executor: executor, permits: permits})
	}
	return result
}
