package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
)

// CommitWorkflowContinuation keeps the source audit, child Invocation, child
// TaskPlan and both event outbox cursors in one SQLite transaction. A repeated
// stable child ID is treated as an idempotent read only when every immutable
// identity field and plan shape matches.
func (r *runtimeRepository) CommitWorkflowContinuation(ctx context.Context, commit agentruntime.WorkflowContinuationCommit) (agentruntime.Invocation, agentruntime.TaskPlan, []agentruntime.AgentEvent, error) {
	if r == nil || r.db == nil {
		return agentruntime.Invocation{}, agentruntime.TaskPlan{}, nil, agentruntime.ErrWorkflowContinuationUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var savedInvocation agentruntime.Invocation
	var savedPlan agentruntime.TaskPlan
	var savedEvents []agentruntime.AgentEvent
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var sourceRow invocationRow
		if err := tx.Where("id = ?", strings.TrimSpace(commit.SourceInvocationID)).First(&sourceRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		var sourcePlanRow taskPlanRow
		if err := tx.Where("invocation_id = ?", sourceRow.ID).First(&sourcePlanRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		source := invocationFromRow(sourceRow)
		sourcePlan := taskPlanFromRow(sourcePlanRow)
		var recoveryRows []agentEventRow
		if err := tx.Where("invocation_id = ?", sourceRow.ID).Order("sequence ASC").Limit(100001).Find(&recoveryRows).Error; err != nil {
			return err
		}
		if len(recoveryRows) > 100000 {
			return agentruntime.ErrWorkflowContinuationSource
		}
		recoveryEvents := make([]agentruntime.AgentEvent, 0, len(recoveryRows))
		for _, row := range recoveryRows {
			recoveryEvents = append(recoveryEvents, eventFromRow(row))
		}
		if !agentruntime.HasWorkflowRecoveryProof(recoveryEvents) {
			return agentruntime.ErrWorkflowContinuationSource
		}
		if err := agentruntime.ValidateWorkflowContinuationCommit(commit, source, sourcePlan); err != nil {
			return err
		}

		childID := strings.TrimSpace(commit.Child.ID)
		var existingRow invocationRow
		findErr := tx.Where("id = ?", childID).First(&existingRow).Error
		if findErr == nil {
			existing := invocationFromRow(existingRow)
			if !sqliteWorkflowContinuationInvocationMatches(existing, commit.Child) {
				return agentruntime.ErrWorkflowContinuationConflict
			}
			var existingPlanRow taskPlanRow
			if planErr := tx.Where("invocation_id = ?", childID).First(&existingPlanRow).Error; planErr != nil {
				if errors.Is(planErr, gorm.ErrRecordNotFound) {
					return agentruntime.ErrWorkflowContinuationConflict
				}
				return planErr
			}
			existingPlan := taskPlanFromRow(existingPlanRow)
			if !agentruntime.PlansSemanticallyEqual(existingPlan, commit.ChildPlan) {
				return agentruntime.ErrWorkflowContinuationConflict
			}
			savedInvocation, savedPlan = existing, existingPlan
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var priorChildren []invocationRow
		if err := tx.Where("parent_invocation_id = ? AND id <> ?", source.ID, childID).Limit(1).Find(&priorChildren).Error; err != nil {
			return err
		}
		if len(priorChildren) > 0 {
			return agentruntime.ErrWorkflowContinuationConflict
		}

		var activeRows []invocationRow
		if err := tx.Where("user_id = ? AND conversation_id = ? AND status IN ?", source.UserID, source.ConversationID, []string{
			string(agentruntime.InvocationQueued), string(agentruntime.InvocationRunning), string(agentruntime.InvocationWaitingApproval), string(agentruntime.InvocationWaitingTool), string(agentruntime.InvocationWaitingUser), string(agentruntime.InvocationCancelling),
		}).Find(&activeRows).Error; err != nil {
			return err
		}
		for _, row := range activeRows {
			if row.ID != source.ID {
				return agentruntime.ErrConflict
			}
		}

		child := commit.Child
		childRow := invocationToRow(child)
		if err := tx.Create(childRow).Error; err != nil {
			return err
		}
		steps, err := json.Marshal(commit.ChildPlan.Steps)
		if err != nil {
			return err
		}
		planRow := &taskPlanRow{ID: commit.ChildPlan.ID, InvocationID: child.ID, Revision: commit.ChildPlan.Revision, Status: string(commit.ChildPlan.Status), CurrentStepID: commit.ChildPlan.CurrentStepID, Blocker: commit.ChildPlan.Blocker, StepsJSON: string(steps), CreatedAt: commit.ChildPlan.CreatedAt, UpdatedAt: commit.ChildPlan.UpdatedAt}
		if err := tx.Create(planRow).Error; err != nil {
			return err
		}

		events := make([]agentruntime.AgentEvent, 0, 2)
		storeEvent := func(event agentruntime.AgentEvent, invocationID string, sequence int64) error {
			event.InvocationID = invocationID
			event.ID = strings.TrimSpace(event.ID)
			event.Type = strings.TrimSpace(event.Type)
			if event.ID == "" || event.Type == "" {
				return agentruntime.ErrConflict
			}
			var existing agentEventRow
			if findErr := tx.Where("id = ?", event.ID).First(&existing).Error; findErr == nil {
				return agentruntime.ErrConflict
			} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
				return findErr
			}
			if event.Timestamp.IsZero() {
				event.Timestamp = time.Now().UTC()
			} else {
				event.Timestamp = event.Timestamp.UTC()
			}
			event.Sequence = sequence
			encoded, err := json.Marshal(event.Data)
			if err != nil {
				return err
			}
			if err := tx.Create(&agentEventRow{ID: event.ID, InvocationID: invocationID, Sequence: sequence, Type: event.Type, Timestamp: event.Timestamp, DataJSON: string(encoded)}).Error; err != nil {
				return err
			}
			if err := createRuntimeEventOutboxTx(tx, event); err != nil {
				return err
			}
			events = append(events, agentruntime.SanitizeAgentEvent(event))
			return nil
		}
		var last agentEventRow
		lastErr := tx.Where("invocation_id = ?", source.ID).Order("sequence DESC").First(&last).Error
		if lastErr != nil && !errors.Is(lastErr, gorm.ErrRecordNotFound) {
			return lastErr
		}
		if err := storeEvent(commit.SourceEvent, source.ID, last.Sequence+1); err != nil {
			return err
		}
		if len(commit.ChildEvents) != 1 {
			return agentruntime.ErrWorkflowContinuationConflict
		}
		if err := storeEvent(commit.ChildEvents[0], child.ID, 1); err != nil {
			return err
		}
		savedInvocation = invocationFromRow(*childRow)
		savedPlan = commit.ChildPlan
		savedEvents = events
		return nil
	})
	if err != nil {
		return agentruntime.Invocation{}, agentruntime.TaskPlan{}, nil, err
	}
	return savedInvocation, savedPlan, savedEvents, nil
}

func sqliteWorkflowContinuationInvocationMatches(existing, expected agentruntime.Invocation) bool {
	return existing.ID == expected.ID && existing.ParentInvocationID == expected.ParentInvocationID && existing.UserID == expected.UserID && existing.BotID == expected.BotID && existing.ConversationID == expected.ConversationID && existing.WorkspaceID == expected.WorkspaceID && existing.TargetPath == expected.TargetPath && existing.SessionID == expected.SessionID && existing.ProviderID == expected.ProviderID && existing.ModelID == expected.ModelID && existing.Message == expected.Message && existing.IdempotencyKey == expected.IdempotencyKey && existing.ContinuationDecision == expected.ContinuationDecision && existing.ContinuationSourcePlanRevision == expected.ContinuationSourcePlanRevision && existing.ConfigSnapshot == expected.ConfigSnapshot && len(existing.Attachments) == 0 && len(expected.Attachments) == 0
}
