package runtime

import (
	"context"
	"time"
)

const runtimeDeliveryCompensationTimeout = 10 * time.Second

// compensateRuntimeDeliveryTransaction makes one bounded best-effort abort
// attempt after the source outbox reaches its final delivery attempt. The
// source remains failed regardless of the remote response: a committed remote
// transaction is never rolled back, while an uncommitted prepare can still be
// released. The helper intentionally ignores the remote error so a cleanup
// outage cannot turn a deterministic source failure into an unbounded retry.
func compensateRuntimeDeliveryTransaction(ctx context.Context, terminalAttempt, prepared bool, abort func(context.Context) error) {
	if !terminalAttempt || !prepared || abort == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	compensationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeDeliveryCompensationTimeout)
	defer cancel()
	_ = abort(compensationCtx)
}
