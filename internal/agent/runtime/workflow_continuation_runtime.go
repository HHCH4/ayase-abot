package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
)

const defaultWorkflowContinuationMessage = "继续处理上次被中断的工作流"

// ContinueInterruptedWorkflow creates a new queued Invocation from an
// Invocation that Runtime conservatively failed during restart recovery. The
// old Invocation is never reopened and no tool/model call is made before the
// new child is durably committed.
func (c *Coordinator) ContinueInterruptedWorkflow(ctx context.Context, sourceInvocationID string, request WorkflowContinuationRequest) (Invocation, error) {
	if c == nil || c.repo == nil {
		return Invocation{}, ErrWorkflowContinuationUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.ensureStarted(); err != nil {
		return Invocation{}, err
	}
	normalizedRequest, err := normalizeWorkflowContinuationRequest(sourceInvocationID, request)
	if err != nil {
		return Invocation{}, err
	}
	planRepo, planOK := c.repo.(TaskPlanRepository)
	commitRepo, commitOK := c.repo.(WorkflowContinuationCommitRepository)
	if !planOK || !commitOK {
		return Invocation{}, ErrWorkflowContinuationUnavailable
	}

	// The in-process admission lock complements the repository's transaction;
	// SQLite still rechecks active conversations under its transaction for
	// multi-process callers.
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()

	source, err := c.repo.GetInvocation(ctx, strings.TrimSpace(sourceInvocationID))
	if err != nil {
		return Invocation{}, err
	}
	if source.Status != InvocationFailed || !strings.Contains(strings.ToLower(source.Error), "runtime_interrupted") {
		return Invocation{}, fmt.Errorf("%w: source invocation 必须是 Runtime 中断失败", ErrWorkflowContinuationSource)
	}
	if source.LeaseOwner != "" && (source.LeaseExpiresAt == nil || source.LeaseExpiresAt.After(time.Now().UTC())) {
		return Invocation{}, fmt.Errorf("%w: source invocation 仍有活动租约", ErrWorkflowContinuationSource)
	}
	if !c.hasWorkflowRecoveryProof(ctx, source.ID) {
		return Invocation{}, fmt.Errorf("%w: 缺少 runtime_recovery 事件证明", ErrWorkflowContinuationSource)
	}
	plan, err := planRepo.GetTaskPlan(ctx, source.ID)
	if err != nil {
		return Invocation{}, err
	}
	now := time.Now().UTC()
	childID := workflowContinuationID(source.ID, normalizedRequest.ExpectedPlanRevision, normalizedRequest.Decision, normalizedRequest.StepID, normalizedRequest.IdempotencyKey)
	childPlan, err := workflowContinuationPlan(plan, childID, normalizedRequest, now)
	if err != nil {
		return Invocation{}, err
	}
	message := normalizedRequest.Message
	if message == "" {
		message = defaultWorkflowContinuationMessage
	}
	child := Invocation{
		ID: childID, UserID: source.UserID, IdempotencyKey: "workflow-continuation:" + childID,
		BotID: source.BotID, ConversationID: source.ConversationID, WorkspaceID: source.WorkspaceID,
		TargetPath: source.TargetPath, SessionID: source.SessionID, ParentInvocationID: source.ID,
		ContinuationDecision: normalizedRequest.Decision, ContinuationSourcePlanRevision: plan.Revision,
		ProviderID: source.ProviderID, ModelID: source.ModelID, ConfigSnapshot: source.ConfigSnapshot,
		Message: message, Status: InvocationQueued, CreatedAt: now, UpdatedAt: now,
	}
	sourceEvent := AgentEvent{
		ID: workflowContinuationEventID(child.ID, "requested"), InvocationID: source.ID,
		Type: EventWorkflowContinuationRequested, Timestamp: now,
		Data: map[string]any{
			"child_invocation_id": child.ID, "decision": string(normalizedRequest.Decision),
			"step_id": normalizedRequest.StepID, "source_plan_revision": plan.Revision,
			"evidence": append([]EvidenceRef(nil), normalizedRequest.Evidence...),
		},
	}
	childEvent := AgentEvent{
		ID: workflowContinuationEventID(child.ID, "queued"), InvocationID: child.ID,
		Type: EventInvocationQueued, Timestamp: now,
		Data: map[string]any{
			"conversation_id": child.ConversationID, "parent_invocation_id": source.ID,
			"continuation_decision": string(normalizedRequest.Decision), "step_id": normalizedRequest.StepID,
		},
	}
	child, _, events, err := commitRepo.CommitWorkflowContinuation(ctx, WorkflowContinuationCommit{
		SourceInvocationID: source.ID, SourcePlanRevision: plan.Revision, Decision: normalizedRequest.Decision,
		StepID: normalizedRequest.StepID, IdempotencyKey: normalizedRequest.IdempotencyKey,
		Child: child, ChildPlan: childPlan, SourceEvent: sourceEvent, ChildEvents: []AgentEvent{childEvent},
	})
	if err != nil {
		return Invocation{}, err
	}
	for _, event := range events {
		c.publishStoredEvent(event)
	}
	if child.Status == InvocationQueued {
		c.launch(child.ID, agent.ChatRequest{
			UserID: child.UserID, IdempotencyKey: child.IdempotencyKey, BotID: child.BotID,
			ConversationID: child.ConversationID, SessionID: child.SessionID, ProviderID: child.ProviderID,
			ModelID: child.ModelID, Message: child.Message, WorkspaceID: optionalString(child.WorkspaceID),
			TargetPath: child.TargetPath, ConfigSnapshot: child.ConfigSnapshot, Stream: true,
		})
	}
	return child, nil
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func (c *Coordinator) hasWorkflowRecoveryProof(ctx context.Context, invocationID string) bool {
	events, err := c.listAllInvocationEvents(ctx, invocationID, maxRuntimeEventReplay)
	if err != nil {
		return false
	}
	return HasWorkflowRecoveryProof(events)
}
