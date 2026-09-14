package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// WorkflowContinuationDecision is an explicit user decision for a workflow
// step that was blocked after Runtime lost the worker lease. Runtime never
// infers either decision from a timeout or from model prose.
type WorkflowContinuationDecision string

const (
	WorkflowContinuationRetryStep    WorkflowContinuationDecision = "retry_step"
	WorkflowContinuationCompleteStep WorkflowContinuationDecision = "complete_step"
)

var (
	ErrInvalidWorkflowContinuation     = errors.New("工作流继续请求无效")
	ErrWorkflowContinuationUnavailable = errors.New("当前 Runtime 未启用工作流继续")
	ErrWorkflowContinuationSource      = errors.New("工作流继续源任务不可继续")
	ErrWorkflowContinuationConflict    = errors.New("工作流继续请求冲突")
)

const (
	MaxWorkflowContinuationMessageBytes  = 16 * 1024
	MaxWorkflowContinuationEvidence      = 16
	MaxWorkflowContinuationEvidenceBytes = 4096
	MaxWorkflowContinuationKeyBytes      = 200
)

// WorkflowContinuationRequest is the metadata-only user decision. Source
// Invocation ID is supplied by the route and is therefore not part of the
// request body. StepID is mandatory even though a recovery plan normally has
// one blocked step; making it explicit prevents a stale UI from targeting a
// different step after the plan changes.
type WorkflowContinuationRequest struct {
	ExpectedPlanRevision int64                        `json:"expected_plan_revision"`
	Decision             WorkflowContinuationDecision `json:"decision"`
	StepID               string                       `json:"step_id"`
	Message              string                       `json:"message,omitempty"`
	IdempotencyKey       string                       `json:"idempotency_key,omitempty"`
	Evidence             []EvidenceRef                `json:"evidence,omitempty"`
}

// WorkflowContinuationCommit is the repository transaction payload. Source
// and child events are kept separate because each invocation has an
// independent sequence and event-outbox cursor.
type WorkflowContinuationCommit struct {
	SourceInvocationID string
	SourcePlanRevision int64
	Decision           WorkflowContinuationDecision
	StepID             string
	IdempotencyKey     string
	Child              Invocation
	ChildPlan          TaskPlan
	SourceEvent        AgentEvent
	ChildEvents        []AgentEvent
}

func validWorkflowContinuationDecision(decision WorkflowContinuationDecision) bool {
	return decision == WorkflowContinuationRetryStep || decision == WorkflowContinuationCompleteStep
}

func normalizeWorkflowContinuationEvidence(evidence []EvidenceRef) ([]EvidenceRef, error) {
	if len(evidence) > MaxWorkflowContinuationEvidence {
		return nil, fmt.Errorf("%w: evidence 不能超过 %d 项", ErrInvalidWorkflowContinuation, MaxWorkflowContinuationEvidence)
	}
	result := make([]EvidenceRef, len(evidence))
	for index, item := range evidence {
		item.Kind = sanitizeRuntimeString(strings.TrimSpace(item.Kind))
		item.Ref = sanitizeRuntimeString(strings.TrimSpace(item.Ref))
		item.Summary = sanitizeRuntimeString(strings.TrimSpace(item.Summary))
		if item.Kind == "" || item.Ref == "" {
			return nil, fmt.Errorf("%w: evidence[%d] 必须包含 kind 和 ref", ErrInvalidWorkflowContinuation, index)
		}
		if len(item.Kind) > MaxWorkflowContinuationEvidenceBytes || len(item.Ref) > MaxWorkflowContinuationEvidenceBytes || len(item.Summary) > MaxWorkflowContinuationEvidenceBytes {
			return nil, fmt.Errorf("%w: evidence[%d] 字段过长", ErrInvalidWorkflowContinuation, index)
		}
		result[index] = item
	}
	return result, nil
}

func normalizeWorkflowContinuationRequest(sourceInvocationID string, request WorkflowContinuationRequest) (WorkflowContinuationRequest, error) {
	sourceInvocationID = strings.TrimSpace(sourceInvocationID)
	if sourceInvocationID == "" {
		return WorkflowContinuationRequest{}, fmt.Errorf("%w: source invocation_id 不能为空", ErrInvalidWorkflowContinuation)
	}
	request.StepID = strings.TrimSpace(request.StepID)
	request.Decision = WorkflowContinuationDecision(strings.ToLower(strings.TrimSpace(string(request.Decision))))
	request.Message = sanitizeRuntimeString(strings.TrimSpace(request.Message))
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.ExpectedPlanRevision <= 0 {
		return WorkflowContinuationRequest{}, fmt.Errorf("%w: expected_plan_revision 必须为正数", ErrInvalidWorkflowContinuation)
	}
	if !validWorkflowContinuationDecision(request.Decision) {
		return WorkflowContinuationRequest{}, fmt.Errorf("%w: decision %q 不受支持", ErrInvalidWorkflowContinuation, request.Decision)
	}
	if request.StepID == "" {
		return WorkflowContinuationRequest{}, fmt.Errorf("%w: step_id 不能为空", ErrInvalidWorkflowContinuation)
	}
	if len(request.Message) > MaxWorkflowContinuationMessageBytes {
		return WorkflowContinuationRequest{}, fmt.Errorf("%w: message 过长", ErrInvalidWorkflowContinuation)
	}
	if len(request.IdempotencyKey) > MaxWorkflowContinuationKeyBytes {
		return WorkflowContinuationRequest{}, fmt.Errorf("%w: idempotency_key 过长", ErrInvalidWorkflowContinuation)
	}
	if request.Decision == WorkflowContinuationRetryStep && len(request.Evidence) > 0 {
		return WorkflowContinuationRequest{}, fmt.Errorf("%w: retry_step 不能携带 evidence", ErrInvalidWorkflowContinuation)
	}
	if request.Decision == WorkflowContinuationCompleteStep && len(request.Evidence) == 0 {
		return WorkflowContinuationRequest{}, fmt.Errorf("%w: complete_step 必须携带 evidence", ErrInvalidWorkflowContinuation)
	}
	evidence, err := normalizeWorkflowContinuationEvidence(request.Evidence)
	if err != nil {
		return WorkflowContinuationRequest{}, err
	}
	request.Evidence = evidence
	return request, nil
}

func workflowContinuationID(sourceInvocationID string, expectedRevision int64, decision WorkflowContinuationDecision, stepID, idempotencyKey string) string {
	value := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s", strings.TrimSpace(sourceInvocationID), expectedRevision, decision, strings.TrimSpace(stepID), strings.TrimSpace(idempotencyKey))
	digest := sha256.Sum256([]byte(value))
	return "invocation-continuation-" + hex.EncodeToString(digest[:])[:32]
}

func workflowContinuationEventID(childID, suffix string) string {
	childID = strings.TrimSpace(childID)
	suffix = strings.TrimSpace(suffix)
	return "workflow-continuation-" + childID + "-" + suffix
}

func workflowContinuationPlan(source TaskPlan, childInvocationID string, request WorkflowContinuationRequest, now time.Time) (TaskPlan, error) {
	if err := source.Validate(); err != nil {
		return TaskPlan{}, fmt.Errorf("%w: source plan: %v", ErrWorkflowContinuationSource, err)
	}
	if source.Revision != request.ExpectedPlanRevision {
		return TaskPlan{}, fmt.Errorf("%w: plan revision=%d，当前 revision=%d", ErrWorkflowContinuationConflict, request.ExpectedPlanRevision, source.Revision)
	}
	if source.Status != PlanBlocked || strings.TrimSpace(source.CurrentStepID) != "" {
		return TaskPlan{}, fmt.Errorf("%w: source plan 必须为 blocked 且没有 active step", ErrWorkflowContinuationSource)
	}
	steps := make([]PlanStep, len(source.Steps))
	copy(steps, source.Steps)
	found := false
	blockedCount := 0
	inProgressCount := 0
	for index := range steps {
		step := &steps[index]
		if step.Status == PlanStepBlocked {
			blockedCount++
		}
		if step.Status == PlanStepInProgress {
			inProgressCount++
		}
		if step.ID != request.StepID {
			continue
		}
		if step.Status != PlanStepBlocked || !strings.Contains(step.Blocker, "runtime_interrupted") || len(step.Evidence) != 0 {
			return TaskPlan{}, fmt.Errorf("%w: step %q 不是 Runtime 中断阻塞步骤", ErrWorkflowContinuationSource, request.StepID)
		}
		found = true
		if request.Decision == WorkflowContinuationRetryStep {
			step.Status = PlanStepPending
			step.Blocker = ""
		} else {
			for _, dependency := range step.DependsOn {
				for _, candidate := range source.Steps {
					if candidate.ID == dependency && candidate.Status != PlanStepCompleted && candidate.Status != PlanStepSkipped {
						return TaskPlan{}, fmt.Errorf("%w: step %q 的依赖 %q 尚未完成", ErrWorkflowContinuationSource, request.StepID, dependency)
					}
				}
			}
			step.Status = PlanStepCompleted
			step.Evidence = append([]EvidenceRef(nil), request.Evidence...)
			step.Blocker = ""
			step.LastFailureCode = ""
			step.LastFailure = ""
		}
		step.UpdatedAt = now.UTC()
	}
	if !found || blockedCount != 1 || inProgressCount != 0 {
		return TaskPlan{}, fmt.Errorf("%w: source plan 必须恰有一个 Runtime 中断阻塞步骤", ErrWorkflowContinuationSource)
	}
	childID := strings.TrimSpace(childInvocationID)
	child := TaskPlan{ID: "plan-" + childID, InvocationID: childID, Revision: 1, Steps: steps, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	child.Status = derivePlanStatus(child)
	child, err := child.Normalize(now)
	if err != nil {
		return TaskPlan{}, err
	}
	return child, nil
}

func continuationEvidenceFromPlan(plan TaskPlan, stepID string, decision WorkflowContinuationDecision) []EvidenceRef {
	if decision != WorkflowContinuationCompleteStep {
		return nil
	}
	for _, step := range plan.Steps {
		if step.ID == strings.TrimSpace(stepID) {
			return append([]EvidenceRef(nil), step.Evidence...)
		}
	}
	return nil
}

// HasWorkflowRecoveryProof is shared by the Coordinator and built-in
// repositories. A continuation is executable only when both the blocked-plan
// audit and the terminal recovery audit are present; a caller cannot forge a
// failed Invocation by setting its status/error alone.
func HasWorkflowRecoveryProof(events []AgentEvent) bool {
	var planRecovery, terminalRecovery bool
	for _, event := range events {
		if event.Data == nil {
			continue
		}
		reason, _ := event.Data["reason"].(string)
		if strings.TrimSpace(reason) != "runtime_recovery" {
			continue
		}
		switch event.Type {
		case EventPlanUpdated:
			planRecovery = true
		case EventInvocationFailed:
			terminalRecovery = true
		}
	}
	return planRecovery && terminalRecovery
}

// ValidateWorkflowContinuationCommit verifies the repository-facing payload
// without requiring a concrete storage implementation. SQLite and Memory use
// the same function so a direct repository caller cannot bypass the decision,
// evidence, stable-ID or plan-shape contract enforced by Coordinator.
func ValidateWorkflowContinuationCommit(commit WorkflowContinuationCommit, source Invocation, sourcePlan TaskPlan) error {
	sourceID := strings.TrimSpace(commit.SourceInvocationID)
	if sourceID == "" || source.ID != sourceID || source.Status != InvocationFailed || !strings.Contains(strings.ToLower(source.Error), "runtime_interrupted") {
		return ErrWorkflowContinuationSource
	}
	if strings.TrimSpace(source.UserID) == "" || strings.TrimSpace(source.ConversationID) == "" || strings.TrimSpace(source.SessionID) == "" {
		return ErrWorkflowContinuationSource
	}
	if !validWorkflowContinuationDecision(commit.Decision) || commit.SourcePlanRevision <= 0 || strings.TrimSpace(commit.StepID) == "" {
		return ErrInvalidWorkflowContinuation
	}
	if sourcePlan.Revision != commit.SourcePlanRevision {
		return ErrWorkflowContinuationConflict
	}
	childID := strings.TrimSpace(commit.Child.ID)
	if childID == "" || childID != workflowContinuationID(sourceID, commit.SourcePlanRevision, commit.Decision, commit.StepID, commit.IdempotencyKey) {
		return ErrWorkflowContinuationConflict
	}
	if source.LeaseOwner != "" && (source.LeaseExpiresAt == nil || source.LeaseExpiresAt.After(time.Now().UTC())) {
		return ErrWorkflowContinuationSource
	}
	if commit.Child.ParentInvocationID != sourceID || commit.Child.Status != InvocationQueued || commit.Child.ContinuationDecision != commit.Decision || commit.Child.ContinuationSourcePlanRevision != commit.SourcePlanRevision || commit.Child.Message == "" || len(commit.Child.Message) > MaxWorkflowContinuationMessageBytes || commit.Child.Message != sanitizeRuntimeString(strings.TrimSpace(commit.Child.Message)) || strings.TrimSpace(commit.Child.UserID) == "" || strings.TrimSpace(commit.Child.ConversationID) == "" || strings.TrimSpace(commit.Child.SessionID) == "" || commit.Child.UserID != source.UserID || commit.Child.BotID != source.BotID || commit.Child.ConversationID != source.ConversationID || commit.Child.WorkspaceID != source.WorkspaceID || commit.Child.TargetPath != source.TargetPath || commit.Child.SessionID != source.SessionID || commit.Child.ProviderID != source.ProviderID || commit.Child.ModelID != source.ModelID || commit.Child.ConfigSnapshot != source.ConfigSnapshot || commit.Child.IdempotencyKey != "workflow-continuation:"+childID || len(commit.Child.Attachments) != 0 {
		return ErrWorkflowContinuationConflict
	}
	if strings.TrimSpace(commit.ChildPlan.ID) != "plan-"+childID || strings.TrimSpace(commit.ChildPlan.InvocationID) != childID {
		return ErrWorkflowContinuationConflict
	}
	request := WorkflowContinuationRequest{ExpectedPlanRevision: commit.SourcePlanRevision, Decision: commit.Decision, StepID: commit.StepID, IdempotencyKey: commit.IdempotencyKey, Evidence: continuationEvidenceFromPlan(commit.ChildPlan, commit.StepID, commit.Decision)}
	if _, err := normalizeWorkflowContinuationRequest(sourceID, request); err != nil {
		return err
	}
	expected, err := workflowContinuationPlan(sourcePlan, childID, request, time.Now().UTC())
	if err != nil {
		return err
	}
	if !plansSemanticallyEqual(expected, commit.ChildPlan) {
		return ErrWorkflowContinuationConflict
	}
	if len(commit.ChildEvents) != 1 {
		return ErrWorkflowContinuationConflict
	}
	if strings.TrimSpace(commit.SourceEvent.InvocationID) != sourceID || strings.TrimSpace(commit.ChildEvents[0].InvocationID) != childID || strings.TrimSpace(commit.SourceEvent.ID) == "" || strings.TrimSpace(commit.ChildEvents[0].ID) == "" || commit.SourceEvent.Type != EventWorkflowContinuationRequested || commit.ChildEvents[0].Type != EventInvocationQueued {
		return ErrWorkflowContinuationConflict
	}
	if commit.SourceEvent.ID != workflowContinuationEventID(childID, "requested") || commit.ChildEvents[0].ID != workflowContinuationEventID(childID, "queued") {
		return ErrWorkflowContinuationConflict
	}
	if stringValue(commit.SourceEvent.Data, "child_invocation_id") != childID || stringValue(commit.SourceEvent.Data, "decision") != string(commit.Decision) || stringValue(commit.SourceEvent.Data, "step_id") != commit.StepID || int64Value(commit.SourceEvent.Data, "source_plan_revision") != commit.SourcePlanRevision {
		return ErrWorkflowContinuationConflict
	}
	if stringValue(commit.ChildEvents[0].Data, "conversation_id") != commit.Child.ConversationID || stringValue(commit.ChildEvents[0].Data, "parent_invocation_id") != sourceID || stringValue(commit.ChildEvents[0].Data, "continuation_decision") != string(commit.Decision) || stringValue(commit.ChildEvents[0].Data, "step_id") != commit.StepID {
		return ErrWorkflowContinuationConflict
	}
	return nil
}

func stringValue(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	value, _ := data[key].(string)
	return strings.TrimSpace(value)
}

func int64Value(data map[string]any, key string) int64 {
	if data == nil {
		return 0
	}
	switch value := data[key].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		if value != math.Trunc(value) {
			return 0
		}
		return int64(value)
	default:
		return 0
	}
}
