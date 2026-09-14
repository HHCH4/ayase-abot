package workspace

import (
	"context"
	"strings"
	"sync/atomic"
)

// runtimeInvocationKey carries the stable Abot Invocation ID through ADK's
// tool context. It is deliberately private so a model cannot set it through a
// tool argument or session state.
type runtimeInvocationKey struct{}

type runtimeUserIDKey struct{}

type commandRunContextKey struct{}

type commandChunkSinkKey struct{}

// commandChunkState is shared by stdout/stderr readers so sequence numbers
// describe the order in which the recorder observed bytes across both
// streams. Each stream keeps its own byte offset in observedBuffer.
type commandChunkState struct {
	id       string
	sequence atomic.Uint64
}

// WithInvocationID binds a product-level Invocation ID to a command/tool run.
// The value is used only for audit and approval association; it is not an
// authorization token.
func WithInvocationID(ctx context.Context, invocationID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, runtimeInvocationKey{}, strings.TrimSpace(invocationID))
}

// InvocationIDFromContext returns the product Invocation ID attached by the
// Coordinator, if any.
func InvocationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(runtimeInvocationKey{}).(string)
	return strings.TrimSpace(value)
}

// WithUserID binds the authenticated product user to an ADK tool context.
// Unlike a tool argument this value is installed by Runtime before entering
// Kernel and is used only for ownership/audit projections such as Artifact
// refs.
func WithUserID(ctx context.Context, userID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, runtimeUserIDKey{}, strings.TrimSpace(userID))
}

// UserIDFromContext returns the authenticated user bound by Runtime.
func UserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(runtimeUserIDKey{}).(string)
	return strings.TrimSpace(value)
}

// WithCommandRunID binds output chunks to their durable CommandRun. The
// pointer state is intentionally private; callers can only supply an ID and
// cannot forge sequence or offset values.
func WithCommandRunID(ctx context.Context, commandRunID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	commandRunID = strings.TrimSpace(commandRunID)
	if commandRunID == "" {
		return ctx
	}
	return context.WithValue(ctx, commandRunContextKey{}, &commandChunkState{id: commandRunID})
}

// CommandRunIDFromContext returns the durable command attempt ID attached to
// a command context, if any.
func CommandRunIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	state, _ := ctx.Value(commandRunContextKey{}).(*commandChunkState)
	if state == nil {
		return ""
	}
	return strings.TrimSpace(state.id)
}

// WithCommandChunkSink attaches a bounded-persistence sink to a command
// context. The sink is invoked after the live observer and must not depend on
// browser state; callers that need asynchronous persistence should enqueue in
// the sink and return promptly.
func WithCommandChunkSink(ctx context.Context, sink func(CommandChunk)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, commandChunkSinkKey{}, sink)
}

func commandChunkSink(ctx context.Context) func(CommandChunk) {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(commandChunkSinkKey{}).(func(CommandChunk))
	return sink
}
