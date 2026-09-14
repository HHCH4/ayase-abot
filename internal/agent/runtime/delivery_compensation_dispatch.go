package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const runtimeDeliveryCompensationWorkerLease = 30 * time.Second

// enqueueRuntimeDeliveryCompensation records the only durable source-side
// intent needed to release a destination transaction.  The source family
// outbox remains the owner of the payload; this row intentionally stores no
// event body, checkpoint projection, config snapshot or rejection reason.
func (c *Coordinator) enqueueRuntimeDeliveryCompensation(ctx context.Context, kind RuntimeDeliveryKind, source, destination, deliveryID, invocationID, outboxID, groupID string, sourceAttempt int, now time.Time) error {
	if c == nil || c.repo == nil {
		return ErrRuntimeDeliveryCompensationUnavailable
	}
	repo, ok := c.repo.(RuntimeDeliveryCompensationRepository)
	if !ok {
		return ErrRuntimeDeliveryCompensationUnavailable
	}
	item, err := NewRuntimeDeliveryCompensation(kind, source, destination, deliveryID, invocationID, outboxID, groupID, sourceAttempt, now)
	if err != nil {
		return err
	}
	_, err = repo.EnqueueRuntimeDeliveryCompensation(context.WithoutCancel(runtimeDeliveryCompensationContext(ctx)), item)
	return err
}

func runtimeDeliveryCompensationContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func compensationTransportTransactionsEnabled(transport any, kind RuntimeDeliveryKind) bool {
	switch kind {
	case RuntimeDeliveryKindEvent:
		if marker, ok := transport.(RuntimeEventDeliveryTransactionalOptIn); ok && !marker.RuntimeEventDeliveryTransactionsEnabled() {
			return false
		}
		_, transactional := transport.(RuntimeEventDeliveryTransactionalTransport)
		_, abortable := transport.(RuntimeEventDeliveryAbortableTransport)
		return transactional && abortable
	case RuntimeDeliveryKindCheckpoint:
		if marker, ok := transport.(RuntimeCheckpointDeliveryTransactionalOptIn); ok && !marker.RuntimeCheckpointDeliveryTransactionsEnabled() {
			return false
		}
		_, transactional := transport.(RuntimeCheckpointDeliveryTransactionalTransport)
		_, abortable := transport.(RuntimeCheckpointDeliveryAbortableTransport)
		return transactional && abortable
	case RuntimeDeliveryKindConfig:
		if marker, ok := transport.(RuntimeConfigDeliveryTransactionalOptIn); ok && !marker.RuntimeConfigDeliveryTransactionsEnabled() {
			return false
		}
		_, transactional := transport.(RuntimeConfigDeliveryTransactionalTransport)
		_, abortable := transport.(RuntimeConfigDeliveryAbortableTransport)
		return transactional && abortable
	case RuntimeDeliveryKindRejection:
		if marker, ok := transport.(RuntimeApprovalRejectionDeliveryTransactionalOptIn); ok && !marker.RuntimeApprovalRejectionDeliveryTransactionsEnabled() {
			return false
		}
		_, transactional := transport.(RuntimeApprovalRejectionDeliveryTransactionalTransport)
		_, abortable := transport.(RuntimeApprovalRejectionDeliveryAbortableTransport)
		return transactional && abortable
	default:
		return false
	}
}

func (c *Coordinator) runtimeDeliveryCompensationConfigured() bool {
	if c == nil || c.repo == nil {
		return false
	}
	if _, ok := c.repo.(RuntimeDeliveryCompensationRepository); !ok {
		return false
	}
	c.mu.Lock()
	eventTransport := c.eventDeliveryTransport
	checkpointTransport := c.checkpointDeliveryTransport
	checkpointSource, checkpointDestination := c.checkpointDeliverySource, c.checkpointDeliveryDestination
	configTransport := c.configDeliveryTransport
	configSource, configDestination := c.configDeliverySource, c.configDeliveryDestination
	rejectionTransport := c.rejectionDeliveryTransport
	rejectionSource, rejectionDestination := c.rejectionDeliverySource, c.rejectionDeliveryDestination
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false
	}
	if compensationTransportTransactionsEnabled(eventTransport, RuntimeDeliveryKindEvent) {
		return true
	}
	if checkpointTransport != nil && checkpointSource != "" && checkpointDestination != "" && compensationTransportTransactionsEnabled(checkpointTransport, RuntimeDeliveryKindCheckpoint) {
		return true
	}
	if configTransport != nil && configSource != "" && configDestination != "" && compensationTransportTransactionsEnabled(configTransport, RuntimeDeliveryKindConfig) {
		return true
	}
	if rejectionTransport != nil && rejectionSource != "" && rejectionDestination != "" && compensationTransportTransactionsEnabled(rejectionTransport, RuntimeDeliveryKindRejection) {
		return true
	}
	return false
}

// runtimeDeliveryCompensationSourceState gates Abort until the source cursor
// has durably entered its terminal failed state.  An expired source lease is
// treated as failed only after the recorded attempt reaches the attempt that
// created this compensation row; this closes the crash window without
// allowing a live source worker to be compensated underneath.
func (c *Coordinator) runtimeDeliveryCompensationSourceState(ctx context.Context, item RuntimeDeliveryCompensation, now time.Time) (ready, terminal bool, message string) {
	if c == nil || c.repo == nil {
		return false, true, "Runtime repository unavailable"
	}
	ctx = runtimeDeliveryCompensationContext(ctx)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	status := ""
	attempt := 0
	var lease *time.Time
	verify := func(invocationID, outboxID, deliveryID string) (bool, string) {
		if strings.TrimSpace(invocationID) != item.InvocationID || strings.TrimSpace(outboxID) != item.OutboxID || strings.TrimSpace(deliveryID) != item.DeliveryID {
			return false, "source outbox identity does not match compensation"
		}
		return true, ""
	}
	switch item.Kind {
	case RuntimeDeliveryKindEvent:
		repo, ok := c.repo.(RuntimeEventOutboxRepository)
		if !ok {
			return false, true, "event outbox repository unavailable"
		}
		outbox, err := repo.GetRuntimeEventOutbox(ctx, item.OutboxID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return false, true, "source event outbox missing"
			}
			return false, false, fmt.Sprintf("read source event outbox: %v", err)
		}
		if ok, reason := verify(outbox.InvocationID, outbox.ID, outbox.ID); !ok {
			return false, true, reason
		}
		status, attempt, lease = string(outbox.Status), outbox.Attempt, outbox.LeaseExpiresAt
	case RuntimeDeliveryKindCheckpoint:
		repo, ok := c.repo.(RuntimeCheckpointDeliveryOutboxRepository)
		if !ok {
			return false, true, "checkpoint outbox repository unavailable"
		}
		outbox, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, item.OutboxID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return false, true, "source checkpoint outbox missing"
			}
			return false, false, fmt.Sprintf("read source checkpoint outbox: %v", err)
		}
		if ok, reason := verify(outbox.InvocationID, outbox.ID, outbox.DeliveryID); !ok {
			return false, true, reason
		}
		status, attempt, lease = string(outbox.Status), outbox.Attempt, outbox.LeaseExpiresAt
	case RuntimeDeliveryKindConfig:
		repo, ok := c.repo.(RuntimeConfigDeliveryOutboxRepository)
		if !ok {
			return false, true, "config outbox repository unavailable"
		}
		outbox, err := repo.GetRuntimeConfigDeliveryOutbox(ctx, item.OutboxID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return false, true, "source config outbox missing"
			}
			return false, false, fmt.Sprintf("read source config outbox: %v", err)
		}
		if ok, reason := verify(outbox.InvocationID, outbox.ID, outbox.DeliveryID); !ok {
			return false, true, reason
		}
		status, attempt, lease = string(outbox.Status), outbox.Attempt, outbox.LeaseExpiresAt
	case RuntimeDeliveryKindRejection:
		repo, ok := c.repo.(RuntimeApprovalRejectionDeliveryOutboxRepository)
		if !ok {
			return false, true, "approval rejection outbox repository unavailable"
		}
		outbox, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, item.OutboxID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return false, true, "source approval rejection outbox missing"
			}
			return false, false, fmt.Sprintf("read source approval rejection outbox: %v", err)
		}
		if ok, reason := verify(outbox.InvocationID, outbox.ID, outbox.DeliveryID); !ok {
			return false, true, reason
		}
		status, attempt, lease = string(outbox.Status), outbox.Attempt, outbox.LeaseExpiresAt
	default:
		return false, true, "unsupported compensation kind"
	}
	// A completed source cursor is terminal evidence that the destination may
	// already have committed, even if a corrupted/old row reports a smaller
	// attempt. Never keep a compensation item spinning on that contradiction.
	if status == "completed" {
		return false, true, "source outbox completed; destination may already be committed"
	}
	if attempt < item.SourceAttempt {
		return false, false, "source outbox attempt has not reached compensation attempt"
	}
	switch status {
	case "failed":
		return true, false, ""
	case "processing":
		if lease == nil || !lease.After(now) {
			return true, false, ""
		}
		return false, false, "source outbox lease is still live"
	case "queued":
		return false, false, "source outbox is still queued"
	default:
		return false, true, "source outbox status is invalid"
	}
}

func (c *Coordinator) abortRuntimeDeliveryCompensation(ctx context.Context, item RuntimeDeliveryCompensation, now time.Time) error {
	ctx = runtimeDeliveryCompensationContext(ctx)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var abortErr error
	switch item.Kind {
	case RuntimeDeliveryKindEvent:
		outboxRepo, ok := c.repo.(RuntimeEventOutboxRepository)
		if !ok {
			return ErrRuntimeDeliveryCompensationUnavailable
		}
		eventRepo, ok := c.repo.(AgentEventRepository)
		if !ok {
			return fmt.Errorf("%w: event repository unavailable", ErrConflict)
		}
		outbox, err := outboxRepo.GetRuntimeEventOutbox(ctx, item.OutboxID)
		if err != nil {
			return err
		}
		event, err := eventRepo.GetAgentEvent(ctx, outbox.EventID)
		if err != nil {
			return err
		}
		c.mu.Lock()
		transport := c.eventDeliveryTransport
		c.mu.Unlock()
		transactional, transactionalOK := transport.(RuntimeEventDeliveryTransactionalTransport)
		abortable, abortableOK := transport.(RuntimeEventDeliveryAbortableTransport)
		if !transactionalOK || !abortableOK || (func() bool {
			if marker, ok := transport.(RuntimeEventDeliveryTransactionalOptIn); ok {
				return !marker.RuntimeEventDeliveryTransactionsEnabled()
			}
			return false
		}()) {
			return fmt.Errorf("%w: event compensation transport unavailable", ErrRuntimeDeliveryCompensationUnavailable)
		}
		var envelope RuntimeEventDeliveryEnvelope
		if builder, ok := transport.(RuntimeEventDeliveryReconciliationEnvelopeBuilder); ok {
			envelope, err = builder.BuildRuntimeEventDeliveryReconciliationEnvelope(outbox, event, now)
		} else {
			envelope, err = transactional.BuildEnvelope(outbox, event)
		}
		if err != nil {
			return err
		}
		if envelope.Source != item.Source || envelope.Destination != item.Destination || envelope.DeliveryID != item.DeliveryID || envelope.InvocationID != item.InvocationID {
			return fmt.Errorf("%w: event envelope identity does not match compensation", ErrConflict)
		}
		abortErr = withRuntimeDeliveryCompensationTimeout(ctx, func(compensationCtx context.Context) error {
			receipt, err := abortable.Abort(compensationCtx, envelope)
			if err != nil {
				return err
			}
			return receipt.ValidateAgainstPhase(envelope, RuntimeEventDeliveryTransactionPhaseAborted)
		})
	case RuntimeDeliveryKindCheckpoint:
		repo, ok := c.repo.(RuntimeCheckpointDeliveryOutboxRepository)
		if !ok {
			return ErrRuntimeDeliveryCompensationUnavailable
		}
		outbox, err := repo.GetRuntimeCheckpointDeliveryOutbox(ctx, item.OutboxID)
		if err != nil {
			return err
		}
		if outbox.Source != item.Source || outbox.Destination != item.Destination || outbox.InvocationID != item.InvocationID || outbox.DeliveryID != item.DeliveryID {
			return fmt.Errorf("%w: checkpoint outbox identity does not match compensation", ErrConflict)
		}
		c.mu.Lock()
		transport := c.checkpointDeliveryTransport
		configuredSource, configuredDestination := c.checkpointDeliverySource, c.checkpointDeliveryDestination
		c.mu.Unlock()
		_, transactionalOK := transport.(RuntimeCheckpointDeliveryTransactionalTransport)
		abortable, abortableOK := transport.(RuntimeCheckpointDeliveryAbortableTransport)
		if configuredSource != item.Source || configuredDestination != item.Destination {
			return fmt.Errorf("%w: checkpoint compensation route does not match source outbox", ErrConflict)
		}
		if !transactionalOK || !abortableOK || !compensationTransportTransactionsEnabled(transport, RuntimeDeliveryKindCheckpoint) {
			return fmt.Errorf("%w: checkpoint compensation transport unavailable", ErrRuntimeDeliveryCompensationUnavailable)
		}
		envelope, err := outbox.Envelope(now)
		if err != nil {
			return err
		}
		abortErr = withRuntimeDeliveryCompensationTimeout(ctx, func(compensationCtx context.Context) error {
			receipt, err := abortable.Abort(compensationCtx, envelope, outbox.Projection)
			if err != nil {
				return err
			}
			return receipt.ValidateAgainstPhase(envelope, RuntimeCheckpointDeliveryTransactionPhaseAborted)
		})
	case RuntimeDeliveryKindConfig:
		repo, ok := c.repo.(RuntimeConfigDeliveryOutboxRepository)
		if !ok {
			return ErrRuntimeDeliveryCompensationUnavailable
		}
		outbox, err := repo.GetRuntimeConfigDeliveryOutbox(ctx, item.OutboxID)
		if err != nil {
			return err
		}
		if outbox.Source != item.Source || outbox.Destination != item.Destination || outbox.InvocationID != item.InvocationID || outbox.DeliveryID != item.DeliveryID {
			return fmt.Errorf("%w: config outbox identity does not match compensation", ErrConflict)
		}
		c.mu.Lock()
		transport := c.configDeliveryTransport
		configuredSource, configuredDestination := c.configDeliverySource, c.configDeliveryDestination
		c.mu.Unlock()
		_, transactionalOK := transport.(RuntimeConfigDeliveryTransactionalTransport)
		abortable, abortableOK := transport.(RuntimeConfigDeliveryAbortableTransport)
		if configuredSource != item.Source || configuredDestination != item.Destination {
			return fmt.Errorf("%w: config compensation route does not match source outbox", ErrConflict)
		}
		if !transactionalOK || !abortableOK || !compensationTransportTransactionsEnabled(transport, RuntimeDeliveryKindConfig) {
			return fmt.Errorf("%w: config compensation transport unavailable", ErrRuntimeDeliveryCompensationUnavailable)
		}
		envelope, err := outbox.Envelope(now)
		if err != nil {
			return err
		}
		abortErr = withRuntimeDeliveryCompensationTimeout(ctx, func(compensationCtx context.Context) error {
			receipt, err := abortable.Abort(compensationCtx, envelope, outbox.Projection)
			if err != nil {
				return err
			}
			return receipt.ValidateAgainstPhase(envelope, RuntimeConfigDeliveryTransactionPhaseAborted)
		})
	case RuntimeDeliveryKindRejection:
		repo, ok := c.repo.(RuntimeApprovalRejectionDeliveryOutboxRepository)
		if !ok {
			return ErrRuntimeDeliveryCompensationUnavailable
		}
		outbox, err := repo.GetRuntimeApprovalRejectionDeliveryOutbox(ctx, item.OutboxID)
		if err != nil {
			return err
		}
		if outbox.Source != item.Source || outbox.Destination != item.Destination || outbox.InvocationID != item.InvocationID || outbox.DeliveryID != item.DeliveryID {
			return fmt.Errorf("%w: approval rejection outbox identity does not match compensation", ErrConflict)
		}
		c.mu.Lock()
		transport := c.rejectionDeliveryTransport
		configuredSource, configuredDestination := c.rejectionDeliverySource, c.rejectionDeliveryDestination
		c.mu.Unlock()
		_, transactionalOK := transport.(RuntimeApprovalRejectionDeliveryTransactionalTransport)
		abortable, abortableOK := transport.(RuntimeApprovalRejectionDeliveryAbortableTransport)
		if configuredSource != item.Source || configuredDestination != item.Destination {
			return fmt.Errorf("%w: approval rejection compensation route does not match source outbox", ErrConflict)
		}
		if !transactionalOK || !abortableOK || !compensationTransportTransactionsEnabled(transport, RuntimeDeliveryKindRejection) {
			return fmt.Errorf("%w: approval rejection compensation transport unavailable", ErrRuntimeDeliveryCompensationUnavailable)
		}
		envelope, err := outbox.Envelope(now)
		if err != nil {
			return err
		}
		abortErr = withRuntimeDeliveryCompensationTimeout(ctx, func(compensationCtx context.Context) error {
			receipt, err := abortable.Abort(compensationCtx, envelope)
			if err != nil {
				return err
			}
			return receipt.ValidateAgainstPhase(envelope, RuntimeApprovalRejectionDeliveryTransactionPhaseAborted)
		})
	default:
		return fmt.Errorf("%w: unsupported compensation kind", ErrConflict)
	}
	return abortErr
}

func withRuntimeDeliveryCompensationTimeout(ctx context.Context, fn func(context.Context) error) error {
	compensationCtx, cancel := context.WithTimeout(context.WithoutCancel(runtimeDeliveryCompensationContext(ctx)), 10*time.Second)
	defer cancel()
	return fn(compensationCtx)
}

// runtimeDeliveryCompensationEnqueueFailure keeps a failed durable enqueue
// visible on the source cursor without allowing a storage error to leak
// secrets into the error summary. The source retry/fail transition remains
// bounded; a later operator can discover the missing compensation intent
// instead of silently assuming it was persisted.
func runtimeDeliveryCompensationEnqueueFailure(primary, enqueueErr error) error {
	if enqueueErr == nil {
		return primary
	}
	summary := SanitizeRuntimeString(strings.TrimSpace(enqueueErr.Error()))
	if summary == "" {
		return primary
	}
	if primary == nil {
		return fmt.Errorf("durable compensation enqueue failed: %s", summary)
	}
	return fmt.Errorf("%w; durable compensation enqueue failed: %s", primary, summary)
}

func runtimeDeliveryCompensationPermanentError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		return true
	}
	for _, target := range []error{
		ErrInvalidRuntimeEventDelivery, ErrRuntimeEventDeliveryAuth, ErrRuntimeEventDeliveryStale,
		ErrInvalidRuntimeCheckpointDelivery, ErrRuntimeCheckpointDeliveryAuth, ErrRuntimeCheckpointDeliveryStale,
		ErrInvalidRuntimeConfigDelivery, ErrRuntimeConfigDeliveryAuth, ErrRuntimeConfigDeliveryStale,
		ErrInvalidRuntimeApprovalRejectionDelivery, ErrRuntimeApprovalRejectionDeliveryAuth, ErrRuntimeApprovalRejectionDeliveryStale,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (c *Coordinator) dispatchDueRuntimeCompensationsBounded(ctx context.Context, now time.Time, maxClaims int) {
	if c == nil || c.repo == nil || maxClaims <= 0 {
		return
	}
	repo, ok := c.repo.(RuntimeDeliveryCompensationRepository)
	if !ok {
		return
	}
	c.mu.Lock()
	owner, closed := strings.TrimSpace(c.deliveryCompensationWorkerID), c.closed
	c.mu.Unlock()
	if owner == "" || closed {
		return
	}
	ctx = runtimeDeliveryCompensationContext(ctx)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for attempt := 0; attempt < maxClaims; attempt++ {
		item, claimed, err := repo.ClaimRuntimeDeliveryCompensation(ctx, owner, now, runtimeDeliveryCompensationWorkerLease)
		if err != nil || !claimed {
			return
		}
		ready, terminal, message := c.runtimeDeliveryCompensationSourceState(context.WithoutCancel(ctx), item, now)
		if terminal {
			_, _ = repo.FailRuntimeDeliveryCompensation(context.WithoutCancel(ctx), item.ID, owner, now, message)
			continue
		}
		if !ready {
			deferAt := now.Add(time.Second)
			if strings.Contains(message, "read source") {
				deferAt = now.Add(RuntimeDeliveryCompensationBackoff(item.Attempt))
			}
			_, _ = repo.DeferRuntimeDeliveryCompensation(context.WithoutCancel(ctx), item.ID, owner, now, deferAt, message)
			continue
		}
		if err := c.abortRuntimeDeliveryCompensation(context.WithoutCancel(ctx), item, now); err != nil {
			if runtimeDeliveryCompensationPermanentError(err) {
				_, _ = repo.FailRuntimeDeliveryCompensation(context.WithoutCancel(ctx), item.ID, owner, now, err.Error())
			} else {
				_, _ = repo.RetryRuntimeDeliveryCompensation(context.WithoutCancel(ctx), item.ID, owner, now, err.Error())
			}
			continue
		}
		_, _ = repo.CompleteRuntimeDeliveryCompensation(context.WithoutCancel(ctx), item.ID, owner, now)
	}
}
