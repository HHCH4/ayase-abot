package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SetRuntimeConfigDirectoryRebindApplyTransport enables the explicit,
// receipt-backed single-target rebind apply worker. The route is part of the
// source cursor identity and changing it never mutates an existing apply.
func (c *Coordinator) SetRuntimeConfigDirectoryRebindApplyTransport(source, destination string, transport RuntimeConfigDirectoryRebindApplyTransport) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.rebindApplySource = strings.TrimSpace(source)
	c.rebindApplyDestination = strings.TrimSpace(destination)
	c.rebindApplyTransport = transport
	if c.rebindApplyTransports == nil {
		c.rebindApplyTransports = make(map[string]RuntimeConfigDirectoryRebindApplyTransport)
	}
	route := runtimeConfigDirectoryRebindRouteKey(c.rebindApplySource, c.rebindApplyDestination)
	if transport == nil {
		delete(c.rebindApplyTransports, route)
	} else if c.rebindApplySource != "" && c.rebindApplyDestination != "" {
		c.rebindApplyTransports[route] = transport
	}
	started := c.started && !c.closed
	c.mu.Unlock()
	if started && transport != nil && strings.TrimSpace(source) != "" && strings.TrimSpace(destination) != "" {
		c.startRuntimeDeliveryOutboxLoop()
	}
}

// SetRuntimeConfigDirectoryRebindApplyTransportForDestination adds one
// explicit child route for a multi-target apply.  A nil transport removes
// only that route; the legacy single-target route is unaffected.
func (c *Coordinator) SetRuntimeConfigDirectoryRebindApplyTransportForDestination(source, destination string, transport RuntimeConfigDirectoryRebindApplyTransport) {
	if c == nil {
		return
	}
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	c.mu.Lock()
	if c.rebindApplyTransports == nil {
		c.rebindApplyTransports = make(map[string]RuntimeConfigDirectoryRebindApplyTransport)
	}
	route := runtimeConfigDirectoryRebindRouteKey(source, destination)
	if transport == nil || source == "" || destination == "" {
		delete(c.rebindApplyTransports, route)
	} else {
		c.rebindApplyTransports[route] = transport
	}
	started := c.started && !c.closed
	c.mu.Unlock()
	if started && transport != nil && source != "" && destination != "" {
		c.startRuntimeDeliveryOutboxLoop()
	}
}

func (c *Coordinator) runtimeConfigDirectoryRebindApplyDeliveryConfig() (source, destination string, transport RuntimeConfigDirectoryRebindApplyTransport, enabled bool) {
	if c == nil {
		return "", "", nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	source = c.rebindApplySource
	destination = c.rebindApplyDestination
	transport = c.rebindApplyTransport
	enabled = transport != nil && source != "" && destination != ""
	return source, destination, transport, enabled
}

func (c *Coordinator) runtimeConfigDirectoryRebindApplyTransportFor(source, destination string) (RuntimeConfigDirectoryRebindApplyTransport, bool) {
	if c == nil {
		return nil, false
	}
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	c.mu.Lock()
	defer c.mu.Unlock()
	if transport, ok := c.rebindApplyTransports[runtimeConfigDirectoryRebindRouteKey(source, destination)]; ok && transport != nil {
		return transport, true
	}
	if c.rebindApplyTransport != nil && c.rebindApplySource == source && c.rebindApplyDestination == destination {
		return c.rebindApplyTransport, true
	}
	return nil, false
}

// ConfirmRuntimeConfigDirectoryRebind records a user confirmation. It only
// changes the source-local confirmation ledger; no invocation or destination
// state is changed at this point.
func (c *Coordinator) ConfirmRuntimeConfigDirectoryRebind(ctx context.Context, planID, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindConfirmation, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindConfirmationRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	planRepo, ok := c.repo.(RuntimeConfigDirectoryRebindPlanRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindConfirmation{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := planRepo.GetRuntimeConfigDirectoryRebindPlan(ctx, strings.TrimSpace(planID))
	if err != nil {
		return RuntimeConfigDirectoryRebindConfirmation{}, err
	}
	return repo.CreateRuntimeConfigDirectoryRebindConfirmation(ctx, plan, idempotencyKey, now)
}

// ApplyRuntimeConfigDirectoryRebind snapshots the current durable Runtime
// checkpoint, atomically consumes the confirmation and enqueues one source
// apply. If a transport is configured it performs one bounded synchronous
// dispatch so HTTP callers can observe an immediate receipt-backed result;
// the durable queue remains the source of truth after a timeout or crash.
func (c *Coordinator) ApplyRuntimeConfigDirectoryRebind(ctx context.Context, planID, confirmationID, idempotencyKey string, now time.Time) (RuntimeConfigDirectoryRebindApply, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	planRepo, ok := c.repo.(RuntimeConfigDirectoryRebindPlanRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindUnavailable
	}
	confirmationRepo, ok := c.repo.(RuntimeConfigDirectoryRebindConfirmationRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	commitRepo, ok := c.repo.(RuntimeConfigDirectoryRebindApplyCommitRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	snapshotRepo, ok := c.repo.(RuntimeSnapshotRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	sourceRepo, ok := c.repo.(RuntimeCheckpointDeliverySourceRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
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
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	plan, err = plan.Normalize(now)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	if plan.Status != RuntimeConfigDirectoryRebindValidated || len(plan.Destinations) != 1 || len(plan.Targets) != 1 {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	confirmation, err := confirmationRepo.GetRuntimeConfigDirectoryRebindConfirmation(ctx, strings.TrimSpace(confirmationID))
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	if confirmation.PlanID != plan.ID || confirmation.Destination != plan.Destinations[0] {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindConfirmationConflict
	}
	// A retried apply may observe the confirmation after the first successful
	// commit consumed it. Build the deterministic identity from an ephemeral
	// confirmed view; the composite repository below still requires the real
	// durable confirmation to be confirmed for a new apply and returns the
	// existing row for the same apply ID.
	confirmationForBuild := confirmation
	if confirmation.Status == RuntimeConfigDirectoryRebindConfirmationConsumed {
		confirmationForBuild.Status = RuntimeConfigDirectoryRebindConfirmationConfirmed
		confirmationForBuild.ConsumedAt = nil
	}
	invocation, err := c.repo.GetInvocation(ctx, plan.InvocationID)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	if invocation.Status != plan.BoundaryStatus || strings.TrimSpace(invocation.LeaseOwner) != "" || (invocation.LeaseExpiresAt != nil && invocation.LeaseExpiresAt.After(now)) {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindNotEligible
	}
	if strings.TrimSpace(invocation.ConfigSnapshotDigest) != plan.ConfigSnapshotDigest {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindSnapshot
	}
	snapshot, err := snapshotRepo.GetRuntimeSnapshot(ctx, plan.InvocationID)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	if snapshot.InvocationID != plan.InvocationID {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	eventSequence, err := runtimeCheckpointLastEventSequence(ctx, sourceRepo, plan.InvocationID)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	apply, err := NewRuntimeConfigDirectoryRebindApply(plan, confirmationForBuild, snapshot, eventSequence, idempotencyKey, now)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	apply, err = commitRepo.CommitRuntimeConfigDirectoryRebindApply(ctx, confirmation.ID, apply, now)
	if err != nil {
		return RuntimeConfigDirectoryRebindApply{}, err
	}
	if _, _, transport, enabled := c.runtimeConfigDirectoryRebindApplyDeliveryConfig(); enabled {
		dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		c.dispatchDueRuntimeConfigDirectoryRebindAppliesBounded(dispatchCtx, now, 1)
		cancel()
		if latest, getErr := c.GetRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), apply.ID); getErr == nil {
			return latest, nil
		}
		_ = transport
	}
	return apply, nil
}

func (c *Coordinator) GetRuntimeConfigDirectoryRebindApply(ctx context.Context, id string) (RuntimeConfigDirectoryRebindApply, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindApplyRepository)
	if !ok {
		return RuntimeConfigDirectoryRebindApply{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.GetRuntimeConfigDirectoryRebindApply(ctx, id)
}

func (c *Coordinator) ListRuntimeConfigDirectoryRebindApplies(ctx context.Context, planID, invocationID string, status RuntimeConfigDirectoryRebindApplyStatus, limit int) ([]RuntimeConfigDirectoryRebindApply, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindApplyRepository)
	if !ok {
		return nil, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.ListRuntimeConfigDirectoryRebindApplies(ctx, planID, invocationID, status, limit)
}

// runtimeConfigDirectoryRebindApplySourceReady re-reads all source facts
// before a send. A queue row that no longer describes the current waiting
// checkpoint is failed closed instead of sending stale recovery state.
func (c *Coordinator) runtimeConfigDirectoryRebindApplySourceReady(ctx context.Context, item RuntimeConfigDirectoryRebindApply, now time.Time) (RuntimeCheckpointProjection, error) {
	if c == nil || c.repo == nil {
		return RuntimeCheckpointProjection{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	invocation, err := c.repo.GetInvocation(ctx, item.InvocationID)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if invocation.Status != item.BoundaryStatus || strings.TrimSpace(invocation.LeaseOwner) != "" || (invocation.LeaseExpiresAt != nil && invocation.LeaseExpiresAt.After(now)) {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: source Invocation 已离开等待边界", ErrRuntimeConfigDirectoryRebindApplyStale)
	}
	if strings.TrimSpace(invocation.ConfigSnapshotDigest) != item.ConfigSnapshotDigest {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: source 配置快照已变化", ErrRuntimeConfigDirectoryRebindApplyStale)
	}
	sourceRepo, ok := c.repo.(RuntimeCheckpointDeliverySourceRepository)
	if !ok {
		return RuntimeCheckpointProjection{}, ErrRuntimeConfigDirectoryRebindApplyUnavailable
	}
	snapshot, err := sourceRepo.GetRuntimeSnapshot(ctx, item.InvocationID)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if snapshot.InvocationID != item.InvocationID || snapshot.Revision != item.SnapshotRevision {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: source snapshot revision 已变化", ErrRuntimeConfigDirectoryRebindApplyStale)
	}
	eventSequence, err := runtimeCheckpointLastEventSequence(ctx, sourceRepo, item.InvocationID)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if eventSequence != item.EventSequence {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: source event high-water 已变化", ErrRuntimeConfigDirectoryRebindApplyStale)
	}
	projection, err := NewRuntimeCheckpointProjection(snapshot, eventSequence)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil {
		return RuntimeCheckpointProjection{}, err
	}
	if digest != item.SnapshotDigest {
		return RuntimeCheckpointProjection{}, fmt.Errorf("%w: source checkpoint projection 已变化", ErrRuntimeConfigDirectoryRebindApplyStale)
	}
	return projection, nil
}

func (c *Coordinator) dispatchDueRuntimeConfigDirectoryRebindAppliesBounded(ctx context.Context, now time.Time, maxClaims int) {
	repo, ok := c.repo.(RuntimeConfigDirectoryRebindApplyRepository)
	if !ok {
		return
	}
	c.mu.Lock()
	owner := c.rebindApplyWorkerID
	closed := c.closed
	c.mu.Unlock()
	if owner == "" || closed || maxClaims <= 0 {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := repo.ClaimRuntimeConfigDirectoryRebindApply(ctx, owner, now, 30*time.Second)
		if err != nil || !claimed {
			return
		}
		transport, routeEnabled := c.runtimeConfigDirectoryRebindApplyTransportFor(item.Source, item.Destination)
		if !routeEnabled {
			if releaser, releaseOK := c.repo.(RuntimeConfigDirectoryRebindApplyReleaseRepository); releaseOK {
				_, _ = releaser.ReleaseRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now)
			} else {
				_, _ = repo.RetryRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, "rebind apply route 未配置")
			}
			c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
			continue
		}
		projection, sourceErr := c.runtimeConfigDirectoryRebindApplySourceReady(context.WithoutCancel(ctx), item, now)
		if sourceErr != nil {
			_, _ = repo.FailRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, sourceErr.Error())
			c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
			continue
		}
		envelope, envelopeErr := item.Envelope(now)
		if envelopeErr != nil {
			_, _ = repo.FailRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, envelopeErr.Error())
			c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
			continue
		}
		if reconciler, hasReconciler := transport.(RuntimeConfigDirectoryRebindApplyReconciler); hasReconciler {
			proof, statusErr := reconciler.ReconcileRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), envelope, projection)
			if statusErr != nil && !errors.Is(statusErr, ErrRuntimeConfigDirectoryRebindApplyStatusUnavailable) {
				if runtimeConfigDirectoryRebindApplyPermanentError(statusErr) {
					_, _ = repo.FailRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
				} else {
					_, _ = repo.RetryRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, statusErr.Error())
				}
				c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
				continue
			}
			if statusErr == nil {
				if validateErr := proof.ValidateAgainst(envelope); validateErr != nil {
					_, _ = repo.FailRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, validateErr.Error())
					c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
					continue
				}
				if proof.Phase == RuntimeConfigDirectoryRebindApplyStatusAccepted && proof.Found {
					if completed, completeErr := repo.CompleteRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now); completeErr != nil || !completed {
						reason := "rebind apply status proof 收口失败"
						if completeErr != nil {
							reason = completeErr.Error()
						}
						_, _ = repo.RetryRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, reason)
					}
					c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
					continue
				}
				if proof.Phase != RuntimeConfigDirectoryRebindApplyStatusAbsent || proof.Found {
					_, _ = repo.FailRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, "rebind apply status proof phase 无效")
					c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
					continue
				}
			}
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		receipt, deliveryErr := transport.Deliver(deliveryCtx, envelope, projection)
		cancel()
		if deliveryErr == nil {
			deliveryErr = receipt.ValidateAgainst(envelope)
		}
		if deliveryErr != nil {
			if runtimeConfigDirectoryRebindApplyPermanentError(deliveryErr) {
				_, _ = repo.FailRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, deliveryErr.Error())
			} else {
				_, _ = repo.RetryRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, deliveryErr.Error())
			}
			c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
			continue
		}
		if completed, completeErr := repo.CompleteRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now); completeErr != nil || !completed {
			if completeErr != nil {
				_, _ = repo.RetryRuntimeConfigDirectoryRebindApply(context.WithoutCancel(ctx), item.ID, owner, now, completeErr.Error())
			}
		}
		c.reconcileRuntimeConfigDirectoryRebindMultiApplyForChild(context.WithoutCancel(ctx), item.ParentID, now)
	}
}

func runtimeConfigDirectoryRebindApplyPermanentError(err error) bool {
	return errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyConflict) ||
		errors.Is(err, ErrInvalidRuntimeConfigDirectoryRebindApply) ||
		errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyAuth) ||
		errors.Is(err, ErrInvalidRuntimeConfigDirectoryRebindApplyStatus) ||
		errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyStatusAuth) ||
		errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyStale) ||
		errors.Is(err, ErrRuntimeConfigDirectoryRebindConfirmationConflict) ||
		errors.Is(err, ErrRuntimeConfigDirectoryRebindConfirmationExpired) ||
		errors.Is(err, ErrNotFound)
}
