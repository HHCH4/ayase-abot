package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TraceSpan is a metadata-only diagnostic span. It intentionally has no
// prompt, response, command text, file content, tool arguments or raw event
// payload. Product recovery remains based on the durable entities, not this
// derived view.
type TraceSpan struct {
	ID           string         `json:"id"`
	ParentID     string         `json:"parent_id,omitempty"`
	InvocationID string         `json:"invocation_id"`
	Kind         string         `json:"kind"`
	Name         string         `json:"name"`
	StartAt      time.Time      `json:"start_at"`
	EndAt        *time.Time     `json:"end_at,omitempty"`
	Status       string         `json:"status,omitempty"`
	Outcome      string         `json:"outcome,omitempty"`
	ErrorCode    string         `json:"error_code,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
}

// UsageValue distinguishes an observed zero from a value that the provider
// did not return. Value is omitted from JSON while Known is false.
type UsageValue struct {
	Value int  `json:"value,omitempty"`
	Known bool `json:"known"`
}

// UsageTotals is a bounded usage breakdown for one class of model call. The
// normal invocation fields remain on UsageSummary for wire compatibility;
// compaction calls use this nested shape so their cost cannot be mistaken for
// a user-facing response.
type UsageTotals struct {
	PromptTokens        UsageValue `json:"prompt_tokens"`
	CachedInputTokens   UsageValue `json:"cached_input_tokens"`
	ToolUsePromptTokens UsageValue `json:"tool_use_prompt_tokens"`
	OutputTokens        UsageValue `json:"output_tokens"`
	ReasoningTokens     UsageValue `json:"reasoning_tokens"`
	TotalTokens         UsageValue `json:"total_tokens"`
	ModelCalls          int        `json:"model_calls"`
	UnknownCalls        int        `json:"unknown_calls"`
	UnknownFields       []string   `json:"unknown_fields,omitempty"`
	Sources             []string   `json:"sources,omitempty"`
}

type UsageSummary struct {
	PromptTokens        UsageValue         `json:"prompt_tokens"`
	CachedInputTokens   UsageValue         `json:"cached_input_tokens"`
	ToolUsePromptTokens UsageValue         `json:"tool_use_prompt_tokens"`
	OutputTokens        UsageValue         `json:"output_tokens"`
	ReasoningTokens     UsageValue         `json:"reasoning_tokens"`
	TotalTokens         UsageValue         `json:"total_tokens"`
	ModelCalls          int                `json:"model_calls"`
	UnknownCalls        int                `json:"unknown_calls"`
	UnknownFields       []string           `json:"unknown_fields,omitempty"`
	Sources             []string           `json:"sources,omitempty"`
	Compaction          UsageTotals        `json:"compaction"`
	ContextCalibration  ContextCalibration `json:"context_calibration"`
}

// ContextCalibration is a derived, metadata-only summary of manifests that
// received provider prompt-token usage. It reports estimator bias and
// absolute error without changing the conservative estimator or pretending
// that a handful of samples is a tokenizer implementation.
type ContextCalibration struct {
	Samples             int   `json:"samples"`
	EstimatedTokens     int   `json:"estimated_tokens"`
	ActualTokens        int   `json:"actual_tokens"`
	ErrorTokens         int64 `json:"error_tokens"`
	AbsoluteErrorTokens int64 `json:"absolute_error_tokens"`
}

// TraceMetrics uses milliseconds for durations so clients can render a
// compact timeline without floating point rounding.
type TraceMetrics struct {
	QueueMS                     int64    `json:"queue_ms"`
	ActiveRuntimeMS             int64    `json:"active_runtime_ms"`
	ModelMS                     int64    `json:"model_ms"`
	ToolMS                      int64    `json:"tool_ms"`
	ApprovalWaitMS              int64    `json:"approval_wait_ms"`
	CommandQueueMS              int64    `json:"command_queue_ms"`
	CommandRunMS                int64    `json:"command_run_ms"`
	ContextBuildMS              int64    `json:"context_build_ms"`
	CompactionMS                int64    `json:"compaction_ms"`
	TotalWallMS                 int64    `json:"total_wall_ms"`
	ModelCalls                  int      `json:"model_calls"`
	ToolCalls                   int      `json:"tool_calls"`
	ApprovalRequests            int      `json:"approval_requests"`
	ApprovalApproved            int      `json:"approval_approved"`
	ApprovalRejected            int      `json:"approval_rejected"`
	ApprovalExpired             int      `json:"approval_expired"`
	VerificationRuns            int      `json:"verification_runs"`
	CommandChunks               int      `json:"command_chunks"`
	CommandOutputBytes          int64    `json:"command_output_bytes"`
	ContextEstimateSamples      int      `json:"context_estimate_samples"`
	ContextEstimateError        int64    `json:"context_estimate_error_tokens"`
	UnknownUsageCalls           int      `json:"unknown_usage_calls"`
	CompactionModelCalls        int      `json:"compaction_model_calls"`
	CompactionUnknownUsageCalls int      `json:"compaction_unknown_usage_calls"`
	ContextLimitRetries         int      `json:"context_limit_retries"`
	UnknownStateCount           int      `json:"unknown_state_count"`
	RuntimeNoticeCodes          []string `json:"runtime_notice_codes,omitempty"`
}

type InvocationTrace struct {
	InvocationID string           `json:"invocation_id"`
	Status       InvocationStatus `json:"status"`
	GeneratedAt  time.Time        `json:"generated_at"`
	Spans        []TraceSpan      `json:"spans"`
	Metrics      TraceMetrics     `json:"metrics"`
	Usage        UsageSummary     `json:"usage"`
}

// GetInvocationTrace builds a fresh diagnostic projection from durable facts.
// It is deliberately recomputable; trace loss can never change Runtime state.
func (c *Coordinator) GetInvocationTrace(ctx context.Context, invocationID string) (InvocationTrace, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return InvocationTrace{}, fmt.Errorf("invocation_id 不能为空")
	}
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return InvocationTrace{}, err
	}
	events, err := c.listAllInvocationEvents(ctx, invocationID, 10000)
	if err != nil {
		return InvocationTrace{}, err
	}
	var toolCalls []ToolCall
	if repo, ok := c.repo.(ToolCallRepository); ok {
		toolCalls, err = repo.ListToolCalls(ctx, invocationID)
		if err != nil {
			return InvocationTrace{}, err
		}
	}
	var approvals []Approval
	if approvals, err = c.repo.ListApprovals(ctx, invocationID, ""); err != nil {
		return InvocationTrace{}, err
	}
	var manifests []ContextManifest
	if repo, ok := c.repo.(ContextManifestRepository); ok {
		manifests, err = repo.ListContextManifests(ctx, invocationID)
		if err != nil {
			return InvocationTrace{}, err
		}
	}
	var verifications []VerificationRun
	if repo, ok := c.repo.(VerificationRepository); ok {
		verifications, err = repo.ListVerificationRuns(ctx, invocationID)
		if err != nil {
			return InvocationTrace{}, err
		}
	}
	trace := buildInvocationTrace(invocation, events, toolCalls, approvals, manifests, verifications, time.Now().UTC())
	return trace, nil
}

// GetInvocationUsage returns the same usage projection as Trace without
// exposing spans to clients that only need cost/usage counters.
func (c *Coordinator) GetInvocationUsage(ctx context.Context, invocationID string) (UsageSummary, error) {
	trace, err := c.GetInvocationTrace(ctx, invocationID)
	if err != nil {
		return UsageSummary{}, err
	}
	return trace.Usage, nil
}

func buildInvocationTrace(invocation Invocation, events []AgentEvent, toolCalls []ToolCall, approvals []Approval, manifests []ContextManifest, verifications []VerificationRun, now time.Time) InvocationTrace {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	root := TraceSpan{
		ID: "invocation:" + invocation.ID, InvocationID: invocation.ID, Kind: "invocation", Name: "agent.invocation",
		StartAt: invocation.CreatedAt, Status: string(invocation.Status), Outcome: traceOutcome(string(invocation.Status)),
		Attributes: map[string]any{"provider_id": invocation.ProviderID, "model_id": invocation.ModelID, "session_id": invocation.SessionID},
	}
	if root.StartAt.IsZero() {
		root.StartAt = now
	}
	if invocation.FinishedAt != nil {
		end := invocation.FinishedAt.UTC()
		root.EndAt = &end
	} else if invocation.Status.Terminal() {
		end := invocation.UpdatedAt
		if end.IsZero() {
			end = now
		}
		end = end.UTC()
		root.EndAt = &end
	}
	if invocation.Status == InvocationFailed {
		root.ErrorCode = "runtime_failed"
	}

	spans := make([]TraceSpan, 0, 1+len(toolCalls)+len(approvals)+len(manifests)+len(verifications))
	spans = append(spans, root)
	metrics := TraceMetrics{}
	usage := usageFromEvents(events)
	metrics.UnknownUsageCalls = usage.UnknownCalls
	metrics.CompactionModelCalls = usage.Compaction.ModelCalls
	metrics.CompactionUnknownUsageCalls = usage.Compaction.UnknownCalls
	if usage.UnknownCalls > 0 || usage.Compaction.UnknownCalls > 0 {
		metrics.UnknownStateCount++
	}

	// Model lifecycle events are accepted when an adapter emits them. Current
	// adapters may only emit ContextManifest, so manifests fill the same
	// correlation slot without inventing prompt/response data.
	modelSpans := make(map[string]int)
	for _, event := range events {
		switch event.Type {
		case EventModelStarted, EventModelCompleted, EventModelFailed:
			callID := eventString(event.Data, "model_call_id", "call_id")
			if callID == "" {
				callID = fmt.Sprintf("event-%d", event.Sequence)
			}
			spanIndex, exists := modelSpans[callID]
			if !exists {
				created := TraceSpan{ID: "model:" + callID, ParentID: root.ID, InvocationID: invocation.ID, Kind: "model", Name: "model.generate", StartAt: eventTime(event, root.StartAt), Attributes: map[string]any{"model_call_id": callID}}
				spans = append(spans, created)
				spanIndex = len(spans) - 1
				modelSpans[callID] = spanIndex
			}
			span := &spans[spanIndex]
			if event.Type != EventModelStarted {
				end := eventTime(event, span.StartAt)
				span.EndAt = &end
				span.Status = "failed"
				if event.Type == EventModelCompleted {
					span.Status, span.Outcome = "completed", "success"
				} else {
					span.Outcome, span.ErrorCode = "error", eventErrorCode(event)
				}
			}
		case EventModelRetrying:
			span := TraceSpan{ID: fmt.Sprintf("model-retry:%d", event.Sequence), ParentID: root.ID, InvocationID: invocation.ID, Kind: "model", Name: "model.retry", StartAt: eventTime(event, root.StartAt), Attributes: map[string]any{"attempt": eventInt(event.Data, "attempt")}}
			end := span.StartAt
			span.EndAt = &end
			span.Status, span.Outcome = "completed", "retry"
			spans = append(spans, span)
		}
	}
	for _, manifest := range manifests {
		if strings.TrimSpace(manifest.ModelCallID) != "" {
			if existingIndex, exists := modelSpans[manifest.ModelCallID]; exists {
				spans[existingIndex].Attributes["manifest_id"] = manifest.ID
				continue
			}
		}
		id := manifest.ID
		if id == "" {
			id = fmt.Sprintf("manifest-%d", len(spans))
		}
		attrs := map[string]any{"manifest_id": id, "estimated_input": manifest.EstimatedInput, "context_window": manifest.ContextWindow, "output_reserve": manifest.OutputReserve, "safety_reserve": manifest.SafetyReserve}
		if manifest.ActualInput != nil {
			attrs["actual_input"] = *manifest.ActualInput
			metrics.ContextEstimateSamples++
			errorTokens := int64(*manifest.ActualInput - manifest.EstimatedInput)
			metrics.ContextEstimateError += errorTokens
			usage.ContextCalibration.Samples++
			usage.ContextCalibration.EstimatedTokens += manifest.EstimatedInput
			usage.ContextCalibration.ActualTokens += *manifest.ActualInput
			usage.ContextCalibration.ErrorTokens += errorTokens
			if errorTokens < 0 {
				usage.ContextCalibration.AbsoluteErrorTokens -= errorTokens
			} else {
				usage.ContextCalibration.AbsoluteErrorTokens += errorTokens
			}
		}
		if manifest.ModelCallID != "" {
			attrs["model_call_id"] = manifest.ModelCallID
		}
		for _, warning := range manifest.Warnings {
			if warning == "" {
				continue
			}
			attrs["warning"] = warning
			break
		}
		span := TraceSpan{ID: "context:" + id, ParentID: root.ID, InvocationID: invocation.ID, Kind: "context", Name: "context.build", StartAt: manifest.CreatedAt, Status: "completed", Outcome: "success", Attributes: attrs}
		if span.StartAt.IsZero() {
			span.StartAt = root.StartAt
		}
		end := span.StartAt
		span.EndAt = &end
		spans = append(spans, span)
		if manifest.ModelCallID != "" {
			// A model span with no explicit callback is still useful for counting
			// calls and linking usage to its context manifest.
			if _, exists := modelSpans[manifest.ModelCallID]; !exists {
				model := TraceSpan{ID: "model:" + manifest.ModelCallID, ParentID: root.ID, InvocationID: invocation.ID, Kind: "model", Name: "model.generate", StartAt: manifest.CreatedAt, Status: "unknown", Attributes: map[string]any{"model_call_id": manifest.ModelCallID, "manifest_id": manifest.ID, "duration_known": false}}
				spans = append(spans, model)
				modelSpans[manifest.ModelCallID] = len(spans) - 1
			}
		}
	}

	for _, call := range toolCalls {
		attrs := map[string]any{"tool_call_id": call.ID, "tool_name": call.ToolName, "original_call_id": call.OriginalCallID}
		if call.OperationID != "" {
			attrs["operation_id"] = call.OperationID
		}
		if call.ApprovalID != "" {
			attrs["approval_id"] = call.ApprovalID
		}
		span := TraceSpan{ID: "tool:" + call.ID, ParentID: root.ID, InvocationID: invocation.ID, Kind: "tool", Name: "tool.execute", StartAt: call.CreatedAt, Status: string(call.Status), Outcome: traceOutcome(string(call.Status)), Attributes: attrs}
		if span.StartAt.IsZero() {
			span.StartAt = root.StartAt
		}
		if call.FinishedAt != nil {
			end := call.FinishedAt.UTC()
			span.EndAt = &end
		} else if call.Status.Terminal() {
			end := call.UpdatedAt
			if end.IsZero() {
				end = span.StartAt
			}
			end = end.UTC()
			span.EndAt = &end
		}
		if call.Status == ToolCallFailed || call.Status == ToolCallDenied {
			span.ErrorCode = "tool_failed"
		}
		spans = append(spans, span)
	}
	if len(toolCalls) == 0 {
		spans = append(spans, toolSpansFromEvents(invocation.ID, root.ID, events)...)
	}

	for _, approval := range approvals {
		attrs := map[string]any{"approval_id": approval.ID, "tool_name": approval.ToolName, "status": string(approval.Status)}
		if approval.ToolCallID != "" {
			attrs["tool_call_id"] = approval.ToolCallID
		}
		if approval.OperationID != "" {
			attrs["operation_id"] = approval.OperationID
		}
		span := TraceSpan{ID: "approval:" + approval.ID, ParentID: root.ID, InvocationID: invocation.ID, Kind: "approval", Name: "approval.wait", StartAt: approval.CreatedAt, Status: string(approval.Status), Outcome: traceOutcome(string(approval.Status)), Attributes: attrs}
		if span.StartAt.IsZero() {
			span.StartAt = root.StartAt
		}
		if approval.ResolvedAt != nil {
			end := approval.ResolvedAt.UTC()
			span.EndAt = &end
		} else if approval.Status != ApprovalPending {
			end := approval.UpdatedAt
			if end.IsZero() {
				end = span.StartAt
			}
			end = end.UTC()
			span.EndAt = &end
		}
		spans = append(spans, span)
	}

	for _, verification := range verifications {
		attrs := map[string]any{"verification_id": verification.ID, "kind": verification.Kind, "status": string(verification.Status)}
		if verification.PlanStepID != "" {
			attrs["plan_step_id"] = verification.PlanStepID
		}
		if verification.ExitCode != nil {
			attrs["exit_code"] = *verification.ExitCode
		}
		if verification.OutputDigest != "" {
			attrs["output_digest"] = verification.OutputDigest
		}
		start := verification.CreatedAt
		if verification.StartedAt != nil {
			start = *verification.StartedAt
		}
		span := TraceSpan{ID: "verification:" + verification.ID, ParentID: root.ID, InvocationID: invocation.ID, Kind: "workflow", Name: "workflow.verify", StartAt: start, Status: string(verification.Status), Outcome: traceOutcome(string(verification.Status)), Attributes: attrs}
		if span.StartAt.IsZero() {
			span.StartAt = root.StartAt
		}
		if verification.FinishedAt != nil {
			end := verification.FinishedAt.UTC()
			span.EndAt = &end
		} else if verification.Status.Terminal() {
			end := verification.UpdatedAt
			if end.IsZero() {
				end = span.StartAt
			}
			end = end.UTC()
			span.EndAt = &end
		}
		spans = append(spans, span)
	}

	commandSpans, commandChunks, commandBytes := commandSpansFromEvents(invocation.ID, root.ID, events)
	spans = append(spans, commandSpans...)
	metrics.CommandChunks = commandChunks
	metrics.CommandOutputBytes = commandBytes
	for _, event := range events {
		if event.Type != EventContextCompacted && event.Type != EventContextCompactionFailed {
			continue
		}
		name, status, outcome, errorCode := "context.compact", "completed", "success", ""
		if event.Type == EventContextCompactionFailed {
			name, status, outcome, errorCode = "context.compact", "failed", "error", "context_compaction_failed"
		}
		span := TraceSpan{ID: fmt.Sprintf("compaction:%d", event.Sequence), ParentID: root.ID, InvocationID: invocation.ID, Kind: "context", Name: name, StartAt: eventTime(event, root.StartAt), Status: status, Outcome: outcome, ErrorCode: errorCode}
		if event.Data != nil {
			if consecutive := eventInt(event.Data, "consecutive_failures"); consecutive > 0 {
				span.Attributes = map[string]any{"consecutive_failures": consecutive}
			}
		}
		end := span.StartAt
		span.EndAt = &end
		spans = append(spans, span)
	}
	for _, event := range events {
		if event.Type != EventRuntimeNotice || event.Data == nil || strings.TrimSpace(eventString(event.Data, "code")) != "context_limit_exceeded" {
			continue
		}
		recovered, _ := event.Data["recovered"].(bool)
		status, outcome, errorCode := "completed", "success", ""
		if !recovered {
			status, outcome, errorCode = "failed", "error", "context_limit_exceeded"
		}
		attrs := map[string]any{"recovered": recovered}
		if attempt := eventInt(event.Data, "attempt"); attempt > 0 {
			attrs["attempt"] = attempt
		}
		if modelCallID := eventString(event.Data, "model_call_id"); modelCallID != "" {
			attrs["model_call_id"] = modelCallID
		}
		start := eventTime(event, root.StartAt)
		end := start
		spans = append(spans, TraceSpan{ID: fmt.Sprintf("context-recovery:%d", event.Sequence), ParentID: root.ID, InvocationID: invocation.ID, Kind: "context", Name: "context.rebuild", StartAt: start, EndAt: &end, Status: status, Outcome: outcome, ErrorCode: errorCode, Attributes: attrs})
		metrics.ContextLimitRetries++
	}
	for _, event := range events {
		if event.Data == nil {
			continue
		}
		if event.Type == EventContextCompactionFailed {
			metrics.RuntimeNoticeCodes = appendUniqueString(metrics.RuntimeNoticeCodes, "context_compaction_failed")
			if root.ErrorCode == "" {
				root.ErrorCode = "context_compaction_failed"
			}
			continue
		}
		if event.Type != EventRuntimeNotice {
			continue
		}
		if code := strings.TrimSpace(eventString(event.Data, "code")); code != "" {
			metrics.RuntimeNoticeCodes = appendUniqueString(metrics.RuntimeNoticeCodes, code)
			if code == "context_limit_exceeded" {
				if recovered, _ := event.Data["recovered"].(bool); !recovered && root.ErrorCode == "" {
					root.ErrorCode = code
				}
				continue
			}
			if code == "unknown_state" || code == "context_budget_exhausted" {
				metrics.UnknownStateCount++
			}
			if root.ErrorCode == "" && strings.Contains(code, "context") {
				root.ErrorCode = code
			}
		}
	}
	spans[0] = root

	metrics.ModelCalls = len(modelSpans)
	metrics.ToolCalls = 0
	for _, span := range spans {
		duration := spanDurationMS(span, now)
		switch span.Kind {
		case "model":
			metrics.ModelMS += duration
		case "tool":
			metrics.ToolMS += duration
		case "approval":
			metrics.ApprovalWaitMS += duration
		case "context":
			if span.Name == "context.build" {
				metrics.ContextBuildMS += duration
			} else {
				metrics.CompactionMS += duration
			}
		case "workflow":
			// Verification duration is represented in spans; no separate
			// aggregate is needed beyond the run count.
		}
	}
	metrics.ToolCalls = len(toolCalls)
	if metrics.ToolCalls == 0 {
		for _, span := range spans {
			if span.Kind == "tool" {
				metrics.ToolCalls++
			}
		}
	}
	metrics.ApprovalRequests = len(approvals)
	for _, approval := range approvals {
		switch approval.Status {
		case ApprovalApproved:
			metrics.ApprovalApproved++
		case ApprovalRejected:
			metrics.ApprovalRejected++
		case ApprovalExpired:
			metrics.ApprovalExpired++
		}
	}
	metrics.VerificationRuns = len(verifications)
	metrics.TotalWallMS = spanDurationMS(root, now)
	if invocation.StartedAt != nil {
		metrics.QueueMS = maxDurationMS(invocation.CreatedAt, *invocation.StartedAt)
	}
	if metrics.TotalWallMS > metrics.QueueMS {
		metrics.ActiveRuntimeMS = metrics.TotalWallMS - metrics.QueueMS - metrics.ApprovalWaitMS
		if metrics.ActiveRuntimeMS < 0 {
			metrics.ActiveRuntimeMS = 0
		}
	}
	// CommandRun timing remains a metadata-only trace metric; output chunks are
	// replayable through the separate bounded CommandOutput store without
	// copying their text into the trace.
	metrics.CommandRunMS = commandSpanDurationMS(spans, now)
	metrics.UnknownUsageCalls = usage.UnknownCalls
	metrics.CompactionModelCalls = usage.Compaction.ModelCalls
	metrics.CompactionUnknownUsageCalls = usage.Compaction.UnknownCalls
	sort.Strings(metrics.RuntimeNoticeCodes)
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].StartAt.Equal(spans[j].StartAt) {
			return spans[i].ID < spans[j].ID
		}
		return spans[i].StartAt.Before(spans[j].StartAt)
	})
	return InvocationTrace{InvocationID: invocation.ID, Status: invocation.Status, GeneratedAt: now, Spans: spans, Metrics: metrics, Usage: usage}
}

func traceOutcome(status string) string {
	switch status {
	case "completed", "approved", "passed":
		return "success"
	case "failed", "denied", "rejected", "expired", "blocked", "unknown":
		return "error"
	case "cancelled":
		return "cancelled"
	default:
		return "pending"
	}
}

func eventTime(event AgentEvent, fallback time.Time) time.Time {
	if event.Timestamp.IsZero() {
		return fallback
	}
	return event.Timestamp.UTC()
}

func eventString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func eventInt(data map[string]any, key string) int {
	if data == nil {
		return 0
	}
	switch value := data[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func eventErrorCode(event AgentEvent) string {
	if event.Data == nil {
		return "model_failed"
	}
	if code := eventString(event.Data, "error_code", "code"); code != "" {
		return code
	}
	return "model_failed"
}

func spanDurationMS(span TraceSpan, now time.Time) int64 {
	end := now
	if span.EndAt != nil {
		end = *span.EndAt
	}
	return maxDurationMS(span.StartAt, end)
}

func maxDurationMS(start, end time.Time) int64 {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func commandSpanDurationMS(spans []TraceSpan, now time.Time) int64 {
	var total int64
	for _, span := range spans {
		if span.Kind == "command" {
			total += spanDurationMS(span, now)
		}
	}
	return total
}

func appendUniqueString(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func toolSpansFromEvents(invocationID, parentID string, events []AgentEvent) []TraceSpan {
	type pending struct {
		span TraceSpan
	}
	items := make(map[string]*pending)
	for _, event := range events {
		if event.Type != EventToolRequested && event.Type != EventToolStarted && event.Type != EventToolCompleted && event.Type != EventToolFailed {
			continue
		}
		callID := eventString(event.Data, "call_id", "tool_call_id")
		if callID == "" {
			callID = fmt.Sprintf("event-%d", event.Sequence)
		}
		entry := items[callID]
		if entry == nil {
			entry = &pending{span: TraceSpan{ID: "tool:" + callID, ParentID: parentID, InvocationID: invocationID, Kind: "tool", Name: "tool.execute", StartAt: eventTime(event, time.Now().UTC()), Attributes: map[string]any{"tool_call_id": callID}}}
			items[callID] = entry
		}
		if name := eventString(event.Data, "name"); name != "" {
			entry.span.Attributes["tool_name"] = name
		}
		if event.Type == EventToolCompleted || event.Type == EventToolFailed {
			end := eventTime(event, entry.span.StartAt)
			entry.span.EndAt = &end
			if event.Type == EventToolCompleted {
				entry.span.Status, entry.span.Outcome = string(ToolCallCompleted), "success"
			} else {
				entry.span.Status, entry.span.Outcome, entry.span.ErrorCode = string(ToolCallFailed), "error", "tool_failed"
			}
		} else if event.Type == EventToolStarted {
			entry.span.Status = string(ToolCallRunning)
		}
	}
	result := make([]TraceSpan, 0, len(items))
	for _, item := range items {
		result = append(result, item.span)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].StartAt.Before(result[j].StartAt) })
	return result
}

func commandSpansFromEvents(invocationID, parentID string, events []AgentEvent) ([]TraceSpan, int, int64) {
	type aggregate struct {
		span  TraceSpan
		bytes int64
		count int
	}
	items := make(map[string]*aggregate)
	for _, event := range events {
		if event.Type != EventCommandOutput {
			continue
		}
		key := eventString(event.Data, "command_run_id", "run_id", "operation_id")
		if key == "" {
			key = fmt.Sprintf("event-%d", event.Sequence)
		}
		entry := items[key]
		if entry == nil {
			entry = &aggregate{span: TraceSpan{ID: "command:" + key, ParentID: parentID, InvocationID: invocationID, Kind: "command", Name: "command.run", StartAt: eventTime(event, time.Now().UTC()), Status: "running", Attributes: map[string]any{"command_run_id": key}}}
			items[key] = entry
		}
		at := eventTime(event, entry.span.StartAt)
		if at.Before(entry.span.StartAt) {
			entry.span.StartAt = at
		}
		if entry.span.EndAt == nil || at.After(*entry.span.EndAt) {
			end := at
			entry.span.EndAt = &end
		}
		entry.count++
		if stream := eventString(event.Data, "stream"); stream != "" {
			entry.span.Attributes["stream"] = stream
		}
		if data, ok := event.Data["data"].(string); ok {
			entry.bytes += int64(len(data))
		}
	}
	result := make([]TraceSpan, 0, len(items))
	var chunks int
	var bytes int64
	for _, item := range items {
		item.span.Status, item.span.Outcome = "completed", "observed"
		item.span.Attributes["chunks"] = item.count
		item.span.Attributes["output_bytes"] = item.bytes
		result = append(result, item.span)
		chunks += item.count
		bytes += item.bytes
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].StartAt.Before(result[j].StartAt) })
	return result, chunks, bytes
}

type usageAccumulator struct {
	sum     int
	known   int
	seen    int
	unknown bool
	invalid bool
}

func (a *usageAccumulator) add(value int) bool {
	if a == nil || value < 0 {
		return false
	}
	maxInt := int(^uint(0) >> 1)
	if a.sum > maxInt-value {
		return false
	}
	a.sum += value
	return true
}

type usageBreakdownAccumulator struct {
	fields map[string]*usageAccumulator
	result UsageTotals
}

func newUsageBreakdownAccumulator() *usageBreakdownAccumulator {
	return &usageBreakdownAccumulator{fields: map[string]*usageAccumulator{
		"prompt_tokens": {}, "cached_input_tokens": {}, "tool_use_prompt_tokens": {}, "output_tokens": {}, "reasoning_tokens": {}, "total_tokens": {},
	}}
}

func (a *usageBreakdownAccumulator) add(event AgentEvent) {
	if a == nil {
		return
	}
	a.result.ModelCalls++
	usage := normalizeUsageMap(eventUsagePayload(event.Data))
	if usage == nil {
		a.result.UnknownCalls++
		return
	}
	for field, accumulator := range a.fields {
		accumulator.seen++
		if value, ok := usageNumber(usage, usageAliases(field)...); ok {
			if accumulator.add(value) {
				accumulator.known++
			} else {
				accumulator.invalid = true
			}
		} else {
			accumulator.unknown = true
			a.result.UnknownFields = appendUniqueString(a.result.UnknownFields, field)
		}
	}
	if source := eventString(usage, "source", "usage_source"); source != "" {
		a.result.Sources = appendUniqueString(a.result.Sources, source)
	}
}

func (a *usageBreakdownAccumulator) finalize() UsageTotals {
	if a == nil {
		return UsageTotals{}
	}
	a.result.PromptTokens = usageValue(a.fields["prompt_tokens"])
	a.result.CachedInputTokens = usageValue(a.fields["cached_input_tokens"])
	a.result.ToolUsePromptTokens = usageValue(a.fields["tool_use_prompt_tokens"])
	a.result.OutputTokens = usageValue(a.fields["output_tokens"])
	a.result.ReasoningTokens = usageValue(a.fields["reasoning_tokens"])
	a.result.TotalTokens = usageValue(a.fields["total_tokens"])
	sort.Strings(a.result.UnknownFields)
	sort.Strings(a.result.Sources)
	return a.result
}

func usageEventScope(event AgentEvent) string {
	if event.Data == nil {
		return ""
	}
	for _, key := range []string{"scope", "usage_scope"} {
		if value, ok := event.Data[key].(string); ok && strings.EqualFold(strings.TrimSpace(value), "compaction") {
			return "compaction"
		}
	}
	return ""
}

func usageFromEvents(events []AgentEvent) UsageSummary {
	normal := newUsageBreakdownAccumulator()
	compaction := newUsageBreakdownAccumulator()
	for _, event := range events {
		if event.Type != EventUsageUpdated || event.Data == nil {
			continue
		}
		if usageEventScope(event) == "compaction" {
			compaction.add(event)
			continue
		}
		normal.add(event)
	}
	result := UsageSummary{}
	normalized := normal.finalize()
	result.PromptTokens = normalized.PromptTokens
	result.CachedInputTokens = normalized.CachedInputTokens
	result.ToolUsePromptTokens = normalized.ToolUsePromptTokens
	result.OutputTokens = normalized.OutputTokens
	result.ReasoningTokens = normalized.ReasoningTokens
	result.TotalTokens = normalized.TotalTokens
	result.ModelCalls = normalized.ModelCalls
	result.UnknownCalls = normalized.UnknownCalls
	result.UnknownFields = normalized.UnknownFields
	result.Sources = normalized.Sources
	result.Compaction = compaction.finalize()
	return result
}

func usageValue(value *usageAccumulator) UsageValue {
	if value == nil || value.seen == 0 || value.invalid || value.known != value.seen {
		return UsageValue{}
	}
	return UsageValue{Value: value.sum, Known: true}
}

func normalizeUsageMap(value any) map[string]any {
	return normalizeUsageMapDepth(value, 0)
}

func normalizeUsageMapDepth(value any, depth int) map[string]any {
	if value == nil {
		return nil
	}
	if depth > 3 {
		return nil
	}
	if result, ok := value.(map[string]any); ok {
		for _, key := range []string{"usage", "usageMetadata", "usage_metadata"} {
			if nested, exists := result[key]; exists {
				if nestedMap := normalizeUsageMapDepth(nested, depth+1); nestedMap != nil {
					return nestedMap
				}
			}
		}
		return result
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil
	}
	return normalizeUsageMapDepth(result, depth)
}

// eventUsagePayload accepts both the stable Runtime wrapper and the common
// ADK/provider spellings used by older adapters. It deliberately returns the
// payload untouched; normalizeUsageMap performs the bounded JSON-shaped
// conversion and nested envelope unwrapping below.
func eventUsagePayload(data map[string]any) any {
	if data == nil {
		return nil
	}
	for _, key := range []string{"usage", "usageMetadata", "usage_metadata"} {
		if value, ok := data[key]; ok {
			return value
		}
	}
	if raw, ok := data["raw"]; ok {
		if rawMap := normalizeUsageMap(raw); rawMap != nil {
			for _, key := range []string{"usageMetadata", "usage_metadata", "usage"} {
				if value, exists := rawMap[key]; exists {
					return value
				}
			}
		}
	}
	// A few legacy callers pass the usage object itself as Event.Data. Accept
	// that shape only when it contains a recognized counter; an event with no
	// usage payload must remain an unknown call rather than becoming a map of
	// unknown fields.
	for _, key := range []string{
		"promptTokenCount", "prompt_token_count", "promptTokens", "prompt_tokens", "inputTokenCount", "input_token_count", "inputTokens", "input_tokens",
		"cachedContentTokenCount", "cached_content_token_count", "cached_input_tokens", "cache_read_input_tokens", "cachedTokens",
		"toolUsePromptTokenCount", "tool_use_prompt_token_count", "tool_use_prompt_tokens", "toolUsePromptTokens",
		"candidatesTokenCount", "candidates_token_count", "output_tokens", "outputTokenCount", "outputTokens", "completion_tokens", "completionTokens",
		"thoughtsTokenCount", "thoughts_token_count", "reasoning_tokens", "reasoningTokenCount", "thought_tokens",
		"totalTokenCount", "total_token_count", "total_tokens", "totalTokens",
	} {
		if _, exists := data[key]; exists {
			return data
		}
	}
	return nil
}

func usageAliases(field string) []string {
	switch field {
	case "prompt_tokens":
		return []string{"promptTokenCount", "prompt_token_count", "promptTokens", "prompt_tokens", "inputTokenCount", "input_token_count", "inputTokens", "input_tokens"}
	case "cached_input_tokens":
		return []string{"cachedContentTokenCount", "cached_content_token_count", "cached_input_tokens", "cache_read_input_tokens", "cachedTokens", "cache_read_tokens", "prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens"}
	case "tool_use_prompt_tokens":
		return []string{"toolUsePromptTokenCount", "tool_use_prompt_token_count", "tool_use_prompt_tokens", "toolUsePromptTokens", "tool_prompt_tokens", "toolTokens"}
	case "output_tokens":
		return []string{"candidatesTokenCount", "candidates_token_count", "output_tokens", "outputTokenCount", "outputTokens", "completion_tokens", "completionTokens"}
	case "reasoning_tokens":
		return []string{"thoughtsTokenCount", "thoughts_token_count", "reasoning_tokens", "reasoningTokenCount", "thought_tokens", "completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens"}
	case "total_tokens":
		return []string{"totalTokenCount", "total_token_count", "total_tokens", "totalTokens"}
	default:
		return nil
	}
}

func usageNumber(fields map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		value, ok := usageLookup(fields, key)
		if !ok {
			continue
		}
		if number, ok := usageInteger(value); ok {
			return number, true
		}
	}
	return 0, false
}

func usageLookup(fields map[string]any, path string) (any, bool) {
	if fields == nil || strings.TrimSpace(path) == "" {
		return nil, false
	}
	var current any = fields
	for _, part := range strings.Split(path, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false
		}
		mapValue, ok := current.(map[string]any)
		if !ok {
			mapValue = normalizeUsageMap(current)
			if mapValue == nil {
				return nil, false
			}
		}
		current, ok = mapValue[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func usageInteger(value any) (int, bool) {
	maxInt := uint64(^uint(0) >> 1)
	boundedInt64 := func(number int64) (int, bool) {
		if number < 0 || uint64(number) > maxInt {
			return 0, false
		}
		return int(number), true
	}
	boundedUint64 := func(number uint64) (int, bool) {
		if number > maxInt {
			return 0, false
		}
		return int(number), true
	}
	boundedFloat := func(number float64) (int, bool) {
		// Counts must be finite, integral and representable as an int. The
		// strict upper bound also avoids implementation-defined float→int
		// conversion at 2^63 on 64-bit systems.
		if number < 0 || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number >= float64(maxInt) {
			return 0, false
		}
		return int(number), true
	}
	switch number := value.(type) {
	case int:
		return boundedInt64(int64(number))
	case int8:
		return boundedInt64(int64(number))
	case int16:
		return boundedInt64(int64(number))
	case int32:
		return boundedInt64(int64(number))
	case int64:
		return boundedInt64(number)
	case uint:
		return boundedUint64(uint64(number))
	case uint8:
		return boundedUint64(uint64(number))
	case uint16:
		return boundedUint64(uint64(number))
	case uint32:
		return boundedUint64(uint64(number))
	case uint64:
		return boundedUint64(number)
	case float32:
		return boundedFloat(float64(number))
	case float64:
		return boundedFloat(number)
	case json.Number:
		parsed, err := strconv.ParseInt(strings.TrimSpace(number.String()), 10, 64)
		if err != nil {
			return 0, false
		}
		return boundedInt64(parsed)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(number), 10, 64)
		if err != nil {
			return 0, false
		}
		return boundedInt64(parsed)
	default:
		return 0, false
	}
}
