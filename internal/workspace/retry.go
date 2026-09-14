package workspace

import (
	"context"
	"fmt"
	"strings"
)

// RetryableOperation reports whether a human retry is meaningful for this
// operation. Three cases qualify:
//
//   - unknown: the process control handle was lost (typically a service
//     restart) and the side effects cannot be proven either way;
//   - failed: the run reached a terminal failure such as a start failure or a
//     forced termination;
//   - completed with a non-zero exit code: the run finished, but its result is a
//     failure. This is the most common case a user wants to retry, and the
//     operation status alone ("completed") does not express it.
//
// A successful completion, a rejection, a cancellation, an expiry or a stale
// operation is a decision or an outcome the user already owns; retrying it would
// silently re-run something they stopped or already have.
func RetryableOperation(operation Operation) bool {
	switch operation.Status {
	case OperationUnknown, OperationFailed:
		return true
	case OperationCompleted:
		return operation.CommandOutcome == string(CommandOutcomeNonzero)
	default:
		return false
	}
}

// RetryOperation creates a brand-new operation carrying the same command as a
// failed or unknown source operation.
//
// The original attempt is never replayed: its side effects are unknown, so
// restarting it is a human decision rather than an automatic recovery. The new
// operation is created in the normal prepared state and still has to be
// approved, which is what makes "no automatic replay" true end to end.
//
// Retry is idempotent while a previous retry is still in flight: the pending
// retry is returned instead of stacking duplicates. Once that retry reaches a
// terminal state the user may retry again.
func (s *Service) RetryOperation(ctx context.Context, operationID string) (Operation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return Operation{}, fmt.Errorf("%w: 操作 ID 不能为空", ErrInvalidRequest)
	}
	source, err := s.repository.GetOperation(ctx, operationID)
	if err != nil {
		return Operation{}, err
	}
	if !RetryableOperation(source) {
		return Operation{}, fmt.Errorf("%w: 状态为 %s 的操作不需要重试", ErrOperationState, source.Status)
	}
	if source.Type != OperationCommand {
		return Operation{}, fmt.Errorf("%w: 当前只支持重试命令操作", ErrUnsupported)
	}
	if strings.TrimSpace(source.Command) == "" {
		return Operation{}, fmt.Errorf("%w: 源操作没有可重试的命令", ErrInvalidRequest)
	}
	existing, found, err := s.pendingRetry(ctx, source)
	if err != nil {
		return Operation{}, err
	}
	if found {
		return existing, nil
	}
	return s.CreateOperation(ctx, OperationRequest{
		WorkspaceID: source.WorkspaceID,
		UserID:      source.UserID,
		// The conversation is kept so an archived conversation still blocks the
		// retry. The Invocation and ToolCall are deliberately dropped: this run
		// is a user decision, not a Runtime resume, and must not feed a new
		// approval into a finished Invocation.
		ConversationID: source.ConversationID,
		Type:           source.Type,
		Path:           source.Path,
		Command:        source.Command,
		CWD:            source.CWD,
		Timeout:        source.Timeout,
		TTY:            cloneTTYSpec(source.TTY),
		RetryOf:        source.ID,
	})
}

// pendingRetry returns a retry of the source operation that has not reached a
// terminal state yet.
func (s *Service) pendingRetry(ctx context.Context, source Operation) (Operation, bool, error) {
	operations, err := s.repository.ListOperations(ctx, source.WorkspaceID)
	if err != nil {
		return Operation{}, false, err
	}
	var best Operation
	found := false
	for _, item := range operations {
		if item.RetryOf != source.ID || item.Status.Terminal() {
			continue
		}
		if !found || item.CreatedAt.After(best.CreatedAt) {
			best = item
			found = true
		}
	}
	return best, found, nil
}
