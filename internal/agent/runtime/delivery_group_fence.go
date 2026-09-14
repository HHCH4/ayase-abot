package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Fence revisions are intentionally bounded so a malformed remote value
// cannot overflow a source allocator or create a value that loses precision
// when a non-Go client decodes the signed JSON.
const (
	MaxRuntimeDeliveryFenceIDLength       = 220
	MaxRuntimeDeliveryFenceRevision int64 = 1 << 53
)

var (
	ErrInvalidRuntimeDeliveryFence     = errors.New("Runtime delivery fence 无效")
	ErrRuntimeDeliveryFenceStale       = errors.New("Runtime delivery fence 已过期")
	ErrRuntimeDeliveryFenceConflict    = errors.New("Runtime delivery fence 冲突")
	ErrRuntimeDeliveryFenceRequired    = errors.New("Runtime delivery fence 必须提供")
	ErrRuntimeDeliveryFenceUnavailable = errors.New("Runtime delivery fence 未配置")
)

// RuntimeDeliveryGroupFenceID identifies one monotonic fence scope.  A scope
// is intentionally narrower than a source Runtime: independent destination
// routes cannot invalidate one another, while every group for the same
// Invocation and route shares one epoch.
func RuntimeDeliveryGroupFenceID(source, destination, invocationID string) string {
	material := strings.Join([]string{strings.TrimSpace(source), strings.TrimSpace(destination), strings.TrimSpace(invocationID)}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-delivery-fence-" + hex.EncodeToString(sum[:16])
}

// RuntimeDeliveryGroupFence is the source-issued metadata-only monotonic
// token.  GroupID is the only mutable-looking binding: it records which
// group owns the currently issued revision and prevents two groups from
// claiming the same revision.
type RuntimeDeliveryGroupFence struct {
	FenceID      string    `json:"fence_id"`
	Source       string    `json:"source"`
	Destination  string    `json:"destination"`
	InvocationID string    `json:"invocation_id"`
	GroupID      string    `json:"group_id"`
	Revision     int64     `json:"revision"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (f RuntimeDeliveryGroupFence) normalize(now time.Time) (RuntimeDeliveryGroupFence, error) {
	f.FenceID = strings.TrimSpace(f.FenceID)
	f.Source = strings.TrimSpace(f.Source)
	f.Destination = strings.TrimSpace(f.Destination)
	f.InvocationID = strings.TrimSpace(f.InvocationID)
	f.GroupID = strings.TrimSpace(f.GroupID)
	if f.Source == "" || f.Destination == "" || f.InvocationID == "" || f.GroupID == "" {
		return RuntimeDeliveryGroupFence{}, fmt.Errorf("%w: fence metadata 不完整", ErrInvalidRuntimeDeliveryFence)
	}
	if len(f.Source) > MaxRuntimeDeliveryAttemptSourceLength || len(f.Destination) > MaxRuntimeDeliveryAttemptDestinationLength || len(f.InvocationID) > MaxRuntimeDeliveryGroupInvocationLength || len(f.GroupID) > MaxRuntimeDeliveryGroupIDLength || len(f.FenceID) > MaxRuntimeDeliveryFenceIDLength {
		return RuntimeDeliveryGroupFence{}, fmt.Errorf("%w: fence metadata 超出长度限制", ErrInvalidRuntimeDeliveryFence)
	}
	if f.FenceID == "" {
		f.FenceID = RuntimeDeliveryGroupFenceID(f.Source, f.Destination, f.InvocationID)
	}
	if f.FenceID != RuntimeDeliveryGroupFenceID(f.Source, f.Destination, f.InvocationID) {
		return RuntimeDeliveryGroupFence{}, fmt.Errorf("%w: fence_id 不是稳定派生值", ErrInvalidRuntimeDeliveryFence)
	}
	if f.Revision < 1 || f.Revision > MaxRuntimeDeliveryFenceRevision {
		return RuntimeDeliveryGroupFence{}, fmt.Errorf("%w: revision 超出范围", ErrInvalidRuntimeDeliveryFence)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = now
	} else {
		f.CreatedAt = f.CreatedAt.UTC()
	}
	if f.UpdatedAt.IsZero() {
		f.UpdatedAt = f.CreatedAt
	} else {
		f.UpdatedAt = f.UpdatedAt.UTC()
	}
	return f, nil
}

func (f RuntimeDeliveryGroupFence) Normalize(now time.Time) (RuntimeDeliveryGroupFence, error) {
	return f.normalize(now)
}

func NewRuntimeDeliveryGroupFence(source, destination, invocationID, groupID string, revision int64, now time.Time) (RuntimeDeliveryGroupFence, error) {
	item := RuntimeDeliveryGroupFence{Source: source, Destination: destination, InvocationID: invocationID, GroupID: groupID, Revision: revision}
	return item.normalize(now)
}

func (f RuntimeDeliveryGroupFence) MatchesIdentity(other RuntimeDeliveryGroupFence) bool {
	left, leftErr := f.normalize(time.Time{})
	right, rightErr := other.normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return left.FenceID == right.FenceID && left.Source == right.Source && left.Destination == right.Destination && left.InvocationID == right.InvocationID && left.GroupID == right.GroupID && left.Revision == right.Revision
}

// validateRuntimeDeliveryGroupFenceFields is shared by source group and
// signed envelope normalization. Empty fields mean the legacy, unfenced
// protocol. Once enabled, both fields are required and the scope ID is
// derived from the envelope's routing metadata.
func validateRuntimeDeliveryGroupFenceFields(source, destination, invocationID, fenceID string, revision int64) error {
	fenceID = strings.TrimSpace(fenceID)
	if fenceID == "" && revision == 0 {
		return nil
	}
	if fenceID == "" || revision < 1 || revision > MaxRuntimeDeliveryFenceRevision {
		return fmt.Errorf("%w: fence_id/revision 必须成对且有效", ErrInvalidRuntimeDeliveryFence)
	}
	if len(fenceID) > MaxRuntimeDeliveryFenceIDLength {
		return fmt.Errorf("%w: fence_id 超出长度限制", ErrInvalidRuntimeDeliveryFence)
	}
	if fenceID != RuntimeDeliveryGroupFenceID(source, destination, invocationID) {
		return fmt.Errorf("%w: fence_id 不是稳定派生值", ErrInvalidRuntimeDeliveryFence)
	}
	return nil
}

func cloneRuntimeDeliveryGroupFence(item RuntimeDeliveryGroupFence) RuntimeDeliveryGroupFence {
	return item
}

// BindRuntimeDeliveryGroupFence returns a copy of group with a validated
// immutable fence identity. The group ID itself is unchanged, so callers can
// safely bind a fence after the local group row already exists.
func BindRuntimeDeliveryGroupFence(group RuntimeDeliveryGroup, fence RuntimeDeliveryGroupFence, now time.Time) (RuntimeDeliveryGroup, error) {
	normalizedGroup, err := group.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroup{}, err
	}
	normalizedFence, err := fence.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroup{}, err
	}
	if normalizedFence.Source != normalizedGroup.Source || normalizedFence.Destination != normalizedGroup.Destination || normalizedFence.InvocationID != normalizedGroup.InvocationID || normalizedFence.GroupID != normalizedGroup.ID {
		return RuntimeDeliveryGroup{}, fmt.Errorf("%w: fence 与 group 身份不一致", ErrRuntimeDeliveryFenceConflict)
	}
	normalizedGroup.FenceID = normalizedFence.FenceID
	normalizedGroup.FenceRevision = normalizedFence.Revision
	return normalizedGroup.Normalize(now)
}

// ValidateFence validates only the optional fence fields on a group. It is
// useful when a coordinator reloads a group before building a signed envelope
// and should not mutate any timestamps or lease metadata.
func (g RuntimeDeliveryGroup) ValidateFence() error {
	return validateRuntimeDeliveryGroupFenceFields(g.Source, g.Destination, g.InvocationID, g.FenceID, g.FenceRevision)
}
