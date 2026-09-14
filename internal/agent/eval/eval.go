// Package eval contains deterministic, model-free checks for Agent Runtime
// invariants. It consumes durable projections and never executes tools or
// interprets hidden model reasoning.
package eval

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	agentruntime "Abot/internal/agent/runtime"
)

type AssertionKind string

const (
	AssertionSequenceContiguous AssertionKind = "sequence_contiguous"
	AssertionTerminalStatus     AssertionKind = "terminal_status"
	AssertionEventCount         AssertionKind = "event_count"
	AssertionEventAbsent        AssertionKind = "event_absent"
	AssertionEventOrder         AssertionKind = "event_order"
	AssertionToolLifecycle      AssertionKind = "tool_lifecycle"
	AssertionApprovalSafety     AssertionKind = "approval_no_side_effect"
	AssertionSensitiveAbsent    AssertionKind = "sensitive_data_absent"
)

// EvalCase is intentionally small and versioned. Fixture isolation, provider
// selection and live-model runs can be layered around it without weakening
// these deterministic assertions.
type EvalCase struct {
	ID              string      `json:"id"`
	Version         string      `json:"version"`
	ExpectedOutcome string      `json:"expected_outcome,omitempty"`
	Assertions      []Assertion `json:"assertions,omitempty"`
}

var ErrInvalidCase = errors.New("EvalCase 无效")

// Validate keeps the persisted case definition bounded and unambiguous. A
// case is metadata, not a place to embed a prompt, file, or model response.
func (c EvalCase) Validate() error {
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("%w: id 和 version 不能为空", ErrInvalidCase)
	}
	if len(c.ID) > 160 || len(c.Version) > 80 || len(c.ExpectedOutcome) > 128 {
		return fmt.Errorf("%w: case 标识或 expected_outcome 过长", ErrInvalidCase)
	}
	if len(c.Assertions) > 256 {
		return fmt.Errorf("%w: assertions 数量过多", ErrInvalidCase)
	}
	seen := make(map[string]struct{}, len(c.Assertions))
	for _, assertion := range c.Assertions {
		id := strings.TrimSpace(assertion.ID)
		if id == "" {
			return fmt.Errorf("%w: assertion.id 不能为空", ErrInvalidCase)
		}
		if len(id) > 160 || len(assertion.EventType) > 128 || len(assertion.Before) > 128 || len(assertion.After) > 128 || len(assertion.Expected) > 1024 {
			return fmt.Errorf("%w: assertion %s 字段过长", ErrInvalidCase, id)
		}
		if assertion.ExpectedCount < 0 || assertion.ExpectedCount > 100000 {
			return fmt.Errorf("%w: assertion %s expected_count 超出范围", ErrInvalidCase, id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("%w: assertion.id 重复 %s", ErrInvalidCase, id)
		}
		seen[id] = struct{}{}
		switch assertion.Kind {
		case AssertionSequenceContiguous, AssertionTerminalStatus, AssertionEventCount, AssertionEventAbsent, AssertionEventOrder, AssertionToolLifecycle, AssertionApprovalSafety, AssertionSensitiveAbsent:
		default:
			return fmt.Errorf("%w: assertion %s 类型不支持", ErrInvalidCase, id)
		}
		switch assertion.Kind {
		case AssertionEventCount, AssertionEventAbsent:
			if strings.TrimSpace(assertion.EventType) == "" {
				return fmt.Errorf("%w: assertion %s 缺少 event_type", ErrInvalidCase, id)
			}
		case AssertionEventOrder:
			if strings.TrimSpace(assertion.Before) == "" || strings.TrimSpace(assertion.After) == "" || strings.TrimSpace(assertion.Before) == strings.TrimSpace(assertion.After) {
				return fmt.Errorf("%w: assertion %s before/after 无效", ErrInvalidCase, id)
			}
		}
	}
	return nil
}

type Assertion struct {
	ID            string        `json:"id"`
	Kind          AssertionKind `json:"kind"`
	EventType     string        `json:"event_type,omitempty"`
	Before        string        `json:"before,omitempty"`
	After         string        `json:"after,omitempty"`
	ExpectedCount int           `json:"expected_count,omitempty"`
	Expected      string        `json:"expected,omitempty"`
	Hard          bool          `json:"hard"`
}

type Input struct {
	Invocation agentruntime.Invocation
	Events     []agentruntime.AgentEvent
	ToolCalls  []agentruntime.ToolCall
	Approvals  []agentruntime.Approval
}

type AssertionResult struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Passed  bool   `json:"passed"`
	Hard    bool   `json:"hard"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type Result struct {
	CaseID      string            `json:"case_id"`
	CaseVersion string            `json:"case_version,omitempty"`
	Passed      bool              `json:"passed"`
	HardFailure bool              `json:"hard_failure"`
	Assertions  []AssertionResult `json:"assertions"`
}

// Evaluate applies the core safety/lifecycle assertions and any case-specific
// assertions. Results are stable for the same input; no wall clock or random
// data is consulted.
func Evaluate(def EvalCase, input Input) Result {
	result := Result{CaseID: strings.TrimSpace(def.ID), CaseVersion: strings.TrimSpace(def.Version), Passed: true, Assertions: make([]AssertionResult, 0, len(def.Assertions)+5)}
	add := func(assertion Assertion, passed bool, code, message string) {
		id := strings.TrimSpace(assertion.ID)
		if id == "" {
			id = string(assertion.Kind)
		}
		item := AssertionResult{ID: id, Kind: string(assertion.Kind), Passed: passed, Hard: assertion.Hard, Code: code, Message: message}
		result.Assertions = append(result.Assertions, item)
		if !passed {
			result.Passed = false
			if assertion.Hard {
				result.HardFailure = true
			}
		}
	}
	defaultHard := func(kind AssertionKind) bool {
		return kind == AssertionSequenceContiguous || kind == AssertionTerminalStatus || kind == AssertionToolLifecycle || kind == AssertionApprovalSafety || kind == AssertionSensitiveAbsent
	}

	sequence := Assertion{ID: "runtime.sequence", Kind: AssertionSequenceContiguous, Hard: defaultHard(AssertionSequenceContiguous)}
	if sequenceOK(input.Events) {
		add(sequence, true, "ok", "event sequence contiguous")
	} else {
		add(sequence, false, "event_sequence_gap", "event sequence is not contiguous from one")
	}
	terminal := Assertion{ID: "runtime.terminal", Kind: AssertionTerminalStatus, Expected: strings.TrimSpace(def.ExpectedOutcome), Hard: defaultHard(AssertionTerminalStatus)}
	if passed, code, message := terminalOK(input.Invocation, input.Events, terminal.Expected); passed {
		add(terminal, true, code, message)
	} else {
		add(terminal, false, code, message)
	}
	toolLifecycle := Assertion{ID: "runtime.tool_lifecycle", Kind: AssertionToolLifecycle, Hard: defaultHard(AssertionToolLifecycle)}
	if passed, code, message := toolLifecycleOK(input.Events); passed {
		add(toolLifecycle, true, code, message)
	} else {
		add(toolLifecycle, false, code, message)
	}
	approvalSafety := Assertion{ID: "runtime.approval_safety", Kind: AssertionApprovalSafety, Hard: defaultHard(AssertionApprovalSafety)}
	if passed, code, message := approvalSafetyOK(input.Events, input.ToolCalls, input.Approvals); passed {
		add(approvalSafety, true, code, message)
	} else {
		add(approvalSafety, false, code, message)
	}
	sensitive := Assertion{ID: "runtime.sensitive_data", Kind: AssertionSensitiveAbsent, Hard: defaultHard(AssertionSensitiveAbsent)}
	if passed, code, message := sensitiveDataOK(input.Events); passed {
		add(sensitive, true, code, message)
	} else {
		add(sensitive, false, code, message)
	}

	for _, assertion := range def.Assertions {
		if strings.TrimSpace(assertion.ID) == "" || assertion.Kind == "" {
			assertion.Hard = assertion.Hard || defaultHard(assertion.Kind)
		}
		passed, code, message := evaluateCustom(assertion, input)
		add(assertion, passed, code, message)
	}
	return result
}

func sequenceOK(events []agentruntime.AgentEvent) bool {
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			return false
		}
	}
	return true
}

func terminalOK(invocation agentruntime.Invocation, events []agentruntime.AgentEvent, expected string) (bool, string, string) {
	terminalCount := 0
	terminalStatus := ""
	for _, event := range events {
		if status := terminalEventStatus(event.Type); status != "" {
			terminalCount++
			terminalStatus = status
		}
	}
	if terminalCount != 1 {
		return false, "terminal_event_count", fmt.Sprintf("expected one terminal event, got %d", terminalCount)
	}
	if string(invocation.Status) != terminalStatus {
		return false, "terminal_status_mismatch", fmt.Sprintf("invocation status=%s event status=%s", invocation.Status, terminalStatus)
	}
	if expected != "" && !strings.EqualFold(expected, terminalStatus) {
		return false, "unexpected_terminal_status", fmt.Sprintf("expected %s, got %s", expected, terminalStatus)
	}
	return true, "ok", "terminal event matches invocation status"
}

func terminalEventStatus(eventType string) string {
	switch eventType {
	case agentruntime.EventInvocationCompleted:
		return string(agentruntime.InvocationCompleted)
	case agentruntime.EventInvocationFailed:
		return string(agentruntime.InvocationFailed)
	case agentruntime.EventInvocationCancelled:
		return string(agentruntime.InvocationCancelled)
	case agentruntime.EventInvocationExpired:
		return string(agentruntime.InvocationExpired)
	default:
		return ""
	}
}

func toolLifecycleOK(events []agentruntime.AgentEvent) (bool, string, string) {
	type lifecycle struct {
		requested, started, terminal int
	}
	items := map[string]*lifecycle{}
	for _, event := range events {
		if event.Data == nil {
			continue
		}
		callID := stringValue(event.Data, "call_id", "tool_call_id")
		if callID == "" {
			continue
		}
		item := items[callID]
		if item == nil {
			item = &lifecycle{}
			items[callID] = item
		}
		switch event.Type {
		case agentruntime.EventToolRequested:
			item.requested++
		case agentruntime.EventToolStarted:
			item.started++
		case agentruntime.EventToolCompleted, agentruntime.EventToolFailed:
			item.terminal++
		}
	}
	for callID, item := range items {
		if item.requested == 0 {
			continue
		}
		if item.requested > 1 || item.started > 1 || item.terminal > 1 {
			return false, "tool_duplicate_lifecycle", fmt.Sprintf("tool call %s has duplicate lifecycle events", callID)
		}
		if item.started == 0 || item.terminal == 0 {
			return false, "tool_lifecycle_incomplete", fmt.Sprintf("tool call %s has no start/terminal event", callID)
		}
	}
	return true, "ok", "tool lifecycle is complete"
}

func approvalSafetyOK(events []agentruntime.AgentEvent, toolCalls []agentruntime.ToolCall, approvals []agentruntime.Approval) (bool, string, string) {
	blocked := map[string]bool{}
	for _, approval := range approvals {
		if approval.Status == agentruntime.ApprovalRejected || approval.Status == agentruntime.ApprovalExpired || approval.Status == agentruntime.ApprovalCancelled {
			for _, key := range []string{approval.ToolCallID, approval.OriginalCallID, approval.ConfirmationCallID} {
				if strings.TrimSpace(key) != "" {
					blocked[key] = true
				}
			}
		}
	}
	for _, call := range toolCalls {
		if call.Status == agentruntime.ToolCallCompleted && (call.Status == agentruntime.ToolCallRejected || call.Status == agentruntime.ToolCallExpired || call.Status == agentruntime.ToolCallCancelled || call.Status == agentruntime.ToolCallDenied) {
			return false, "tool_status_invalid", fmt.Sprintf("tool call %s has contradictory terminal status", call.ID)
		}
		if call.Status == agentruntime.ToolCallCompleted && (blocked[call.ID] || blocked[call.OriginalCallID] || blocked[call.ConfirmationCallID]) {
			return false, "approval_side_effect", fmt.Sprintf("rejected/expired approval completed tool call %s", call.ID)
		}
	}
	for _, event := range events {
		if event.Type != agentruntime.EventToolCompleted || event.Data == nil {
			continue
		}
		callID := stringValue(event.Data, "call_id", "tool_call_id")
		if blocked[callID] {
			return false, "approval_side_effect", fmt.Sprintf("rejected/expired approval completed call %s", callID)
		}
	}
	return true, "ok", "rejected/expired approvals have no completed side effect"
}

func sensitiveDataOK(events []agentruntime.AgentEvent) (bool, string, string) {
	for _, event := range events {
		if key := sensitiveKey(event.Data, ""); key != "" {
			return false, "sensitive_event_field", fmt.Sprintf("event contains sensitive field %s", key)
		}
	}
	return true, "ok", "event data contains no sensitive field names"
}

func sensitiveKey(value any, path string) string {
	switch item := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(item))
		for key := range item {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if isSensitiveKey(key) {
				return joinPath(path, key)
			}
			if nested := sensitiveKey(item[key], joinPath(path, key)); nested != "" {
				return nested
			}
		}
	case []any:
		for index, child := range item {
			if nested := sensitiveKey(child, fmt.Sprintf("%s[%d]", path, index)); nested != "" {
				return nested
			}
		}
	}
	return ""
}

func isSensitiveKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"), ".", "_"))
	for _, marker := range []string{"password", "passwd", "secret", "api_key", "apikey", "authorization", "cookie", "private_key", "access_token", "refresh_token"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func stringValue(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func evaluateCustom(assertion Assertion, input Input) (bool, string, string) {
	switch assertion.Kind {
	case AssertionSequenceContiguous:
		if sequenceOK(input.Events) {
			return true, "ok", "event sequence contiguous"
		}
		return false, "event_sequence_gap", "event sequence is not contiguous from one"
	case AssertionTerminalStatus:
		return terminalOK(input.Invocation, input.Events, assertion.Expected)
	case AssertionEventCount:
		count := 0
		for _, event := range input.Events {
			if event.Type == assertion.EventType {
				count++
			}
		}
		if count == assertion.ExpectedCount {
			return true, "ok", fmt.Sprintf("event %s count=%d", assertion.EventType, count)
		}
		return false, "event_count_mismatch", fmt.Sprintf("event %s count=%d expected=%d", assertion.EventType, count, assertion.ExpectedCount)
	case AssertionEventAbsent:
		for _, event := range input.Events {
			if event.Type == assertion.EventType {
				return false, "event_present", fmt.Sprintf("event %s should be absent", assertion.EventType)
			}
		}
		return true, "ok", fmt.Sprintf("event %s is absent", assertion.EventType)
	case AssertionEventOrder:
		before, after := -1, -1
		for index, event := range input.Events {
			if event.Type == assertion.Before && before == -1 {
				before = index
			}
			if event.Type == assertion.After && after == -1 {
				after = index
			}
		}
		if before >= 0 && after > before {
			return true, "ok", "event order matches"
		}
		return false, "event_order_mismatch", fmt.Sprintf("expected %s before %s", assertion.Before, assertion.After)
	case AssertionToolLifecycle:
		return toolLifecycleOK(input.Events)
	case AssertionApprovalSafety:
		return approvalSafetyOK(input.Events, input.ToolCalls, input.Approvals)
	case AssertionSensitiveAbsent:
		return sensitiveDataOK(input.Events)
	default:
		return false, "unknown_assertion", fmt.Sprintf("unsupported assertion kind %q", assertion.Kind)
	}
}
