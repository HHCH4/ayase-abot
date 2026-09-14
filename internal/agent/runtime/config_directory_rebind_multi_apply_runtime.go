package runtime

import (
	"context"
	"strings"
	"time"
)

// ConfirmRuntimeConfigDirectoryRebindMulti records one explicit confirmation
// for the complete multi-target route set. It does not contact destinations.
func (c *Coordinator) ConfirmRuntimeConfigDirectoryRebindMulti(ctx context.Context, planID, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindMultiConfirmation, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindMultiConfirmationRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	planRepo, ok := c.repo.(RuntimeConfigDirectoryRebindPlanRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := planRepo.GetRuntimeConfigDirectoryRebindPlan(ctx, strings.TrimSpace(planID))
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiConfirmation{}, err
	}
	return repo.CreateRuntimeConfigDirectoryRebindMultiConfirmation(ctx, plan, idempotencyKey, now)
}

// ApplyRuntimeConfigDirectoryRebindMulti creates one source-local parent and
// all signed child queue rows in one repository transaction/lock. The parent
// is returned as metadata only; child projections remain private to the
// existing apply queue and are dispatched independently by route.
func (c *Coordinator) ApplyRuntimeConfigDirectoryRebindMulti(ctx context.Context, planID, confirmationID, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindMultiApply, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	planRepo, ok := c.repo.(RuntimeConfigDirectoryRebindPlanRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	confirmationRepo, ok := c.repo.(RuntimeConfigDirectoryRebindMultiConfirmationRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	commitRepo, ok := c.repo.(RuntimeConfigDirectoryRebindMultiApplyCommitRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	snapshotRepo, ok := c.repo.(RuntimeSnapshotRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	sourceRepo, ok := c.repo.(RuntimeCheckpointDeliverySourceRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	plan, err := planRepo.GetRuntimeConfigDirectoryRebindPlan(ctx, strings.TrimSpace(planID))
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	plan, err = plan.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	if plan.Status != RuntimeConfigDirectoryRebindValidated || len(plan.Destinations) < 2 {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiApplyConflict
	}
	confirmationID = strings.TrimSpace(confirmationID)
	confirmation, err := confirmationRepo.GetRuntimeConfigDirectoryRebindMultiConfirmation(ctx, confirmationID)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	if confirmation.PlanID != plan.ID || confirmation.InvocationID != plan.InvocationID || confirmation.Source != plan.Source || confirmation.ConfigSnapshotDigest != plan.ConfigSnapshotDigest || confirmation.CapabilitiesDigest != plan.CapabilitiesDigest || !equalRuntimeConfigDirectoryStrings(confirmation.Destinations, plan.Destinations) {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	confirmationForBuild := confirmation
	if confirmation.Status == RuntimeConfigDirectoryRebindMultiConfirmationConsumed {
		confirmationForBuild.Status = RuntimeConfigDirectoryRebindMultiConfirmationConfirmed
		confirmationForBuild.ConsumedAt = nil
	}
	if confirmationForBuild.Status != RuntimeConfigDirectoryRebindMultiConfirmationConfirmed {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiConfirmationConflict
	}
	invocation, err := c.repo.GetInvocation(ctx, plan.InvocationID)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	if invocation.Status != plan.BoundaryStatus || strings.TrimSpace(invocation.LeaseOwner) != "" || (invocation.LeaseExpiresAt != nil && invocation.LeaseExpiresAt.After(now)) {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindNotEligible
	}
	if strings.TrimSpace(invocation.ConfigSnapshotDigest) != plan.ConfigSnapshotDigest {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindSnapshot
	}
	snapshot, err := snapshotRepo.GetRuntimeSnapshot(ctx, plan.InvocationID)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	if snapshot.InvocationID != plan.InvocationID {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindMultiApplyConflict
	}
	eventSequence, err := runtimeCheckpointLastEventSequence(ctx, sourceRepo, plan.InvocationID)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrInvalidRuntimeConfigDirectoryRebindMultiApply
	}
	projection, err := NewRuntimeCheckpointProjection(snapshot, eventSequence)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	snapshotDigest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	parentID := RuntimeConfigDirectoryRebindMultiApplyID(plan.ID, confirmation.ID, snapshot.Revision, eventSequence, snapshotDigest, idempotencyKey, plan.Destinations)
	parent, children, err := NewRuntimeConfigDirectoryRebindMultiApply(plan, confirmationForBuild, parentID, snapshot, eventSequence, idempotencyKey, now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	parent, err = commitRepo.CommitRuntimeConfigDirectoryRebindMultiApply(ctx, confirmation.ID, parent, children, now)
	if err != nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, err
	}
	// Dispatch is deliberately bounded. A caller can observe queued metadata
	// even if no route transport is configured; the durable children remain the
	// source of truth for a later scheduler.
	c.dispatchDueRuntimeConfigDirectoryRebindAppliesBounded(context.WithoutCancel(ctx), now, len(children))
	if latest, getErr := c.GetRuntimeConfigDirectoryRebindMultiApply(context.WithoutCancel(ctx), parent.ID); getErr == nil {
		return latest, nil
	}
	return parent, nil
}

func (c *Coordinator) GetRuntimeConfigDirectoryRebindMultiApply(ctx context.Context, id string) (RuntimeConfigDirectoryRebindMultiApply, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindMultiApplyRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.GetRuntimeConfigDirectoryRebindMultiApply(ctx, id)
}

func (c *Coordinator) ListRuntimeConfigDirectoryRebindMultiApplies(ctx context.Context, planID, invocationID string, status RuntimeConfigDirectoryRebindMultiApplyStatus, limit int) ([]RuntimeConfigDirectoryRebindMultiApply, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindMultiApplyRepository)
	if !ok {
		return nil, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.ListRuntimeConfigDirectoryRebindMultiApplies(ctx, planID, invocationID, status, limit)
}

func (c *Coordinator) ReconcileRuntimeConfigDirectoryRebindMultiApply(ctx context.Context, id string, now time.Time) (RuntimeConfigDirectoryRebindMultiApply, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindMultiApplyRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindMultiApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.ReconcileRuntimeConfigDirectoryRebindMultiApply(ctx, id, now)
}

func (c *Coordinator) reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(ctx context.Context, parentID string, now time.Time) {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" || c == nil || c.repo == nil {
		return
	}
	if repo, ok := c.repo.(RuntimeConfigDirectoryRebindMultiApplyRepository); ok {
		_, _ = repo.ReconcileRuntimeConfigDirectoryRebindMultiApply(ctx, parentID, now)
	}
}
