package runtime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RuntimeDeliveryGroupEnvelopeVersion is the version of the signed,
// metadata-only group settlement contract.  Group settlement never carries an
// event body, checkpoint projection, rejection reason or configuration data.
const RuntimeDeliveryGroupEnvelopeVersion = 1

const (
	MaxRuntimeDeliveryGroupEnvelopeBytes      = 64 << 10
	MaxRuntimeDeliveryGroupResponseBytes      = 16 << 10
	DefaultRuntimeDeliveryGroupMaxAge         = 10 * time.Minute
	DefaultRuntimeDeliveryGroupFutureSkew     = 30 * time.Second
	DefaultRuntimeDeliveryGroupTransactionTTL = 10 * time.Minute
)

var (
	ErrInvalidRuntimeDeliveryGroupTransaction     = errors.New("Runtime delivery group transaction 无效")
	ErrRuntimeDeliveryGroupTransactionAuth        = errors.New("Runtime delivery group transaction 认证失败")
	ErrRuntimeDeliveryGroupTransactionStale       = errors.New("Runtime delivery group transaction 已过期")
	ErrRuntimeDeliveryGroupTransactionUnavailable = errors.New("Runtime delivery group transaction 未配置")
)

// RuntimeDeliveryGroupMemberEnvelope is the immutable identity of one source
// cursor. It intentionally excludes member status and all delivery payloads.
type RuntimeDeliveryGroupMemberEnvelope struct {
	Kind       RuntimeDeliveryKind `json:"kind"`
	OutboxID   string              `json:"outbox_id"`
	DeliveryID string              `json:"delivery_id"`
}

func runtimeDeliveryGroupMemberEnvelopeIdentity(member RuntimeDeliveryGroupMemberEnvelope) string {
	return strings.Join([]string{string(member.Kind), strings.TrimSpace(member.OutboxID), strings.TrimSpace(member.DeliveryID)}, "\x00")
}

func normalizeRuntimeDeliveryGroupMemberEnvelope(member RuntimeDeliveryGroupMemberEnvelope) (RuntimeDeliveryGroupMemberEnvelope, error) {
	member.Kind = RuntimeDeliveryKind(strings.TrimSpace(string(member.Kind)))
	member.OutboxID = strings.TrimSpace(member.OutboxID)
	member.DeliveryID = strings.TrimSpace(member.DeliveryID)
	if !validRuntimeDeliveryKind(member.Kind) || member.OutboxID == "" || member.DeliveryID == "" {
		return RuntimeDeliveryGroupMemberEnvelope{}, fmt.Errorf("%w: member metadata 不完整", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if len(member.OutboxID) > MaxRuntimeDeliveryGroupMemberIDLength || len(member.DeliveryID) > MaxRuntimeDeliveryGroupMemberIDLength {
		return RuntimeDeliveryGroupMemberEnvelope{}, fmt.Errorf("%w: member identity 超出长度限制", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	return member, nil
}

func runtimeDeliveryGroupMemberEnvelopes(members []RuntimeDeliveryGroupMemberEnvelope) ([]RuntimeDeliveryGroupMemberEnvelope, error) {
	if len(members) == 0 || len(members) > MaxRuntimeDeliveryGroupMemberCount {
		return nil, fmt.Errorf("%w: member 数量必须在 1-%d", ErrInvalidRuntimeDeliveryGroupTransaction, MaxRuntimeDeliveryGroupMemberCount)
	}
	normalized := make([]RuntimeDeliveryGroupMemberEnvelope, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		item, err := normalizeRuntimeDeliveryGroupMemberEnvelope(member)
		if err != nil {
			return nil, err
		}
		identity := runtimeDeliveryGroupMemberEnvelopeIdentity(item)
		if _, exists := seen[identity]; exists {
			return nil, fmt.Errorf("%w: member identity 重复", ErrInvalidRuntimeDeliveryGroupTransaction)
		}
		seen[identity] = struct{}{}
		normalized = append(normalized, item)
	}
	sortRuntimeDeliveryGroupMemberEnvelopes(normalized)
	return normalized, nil
}

func sortRuntimeDeliveryGroupMemberEnvelopes(members []RuntimeDeliveryGroupMemberEnvelope) {
	for i := 1; i < len(members); i++ {
		for j := i; j > 0 && runtimeDeliveryGroupMemberEnvelopeIdentity(members[j]) < runtimeDeliveryGroupMemberEnvelopeIdentity(members[j-1]); j-- {
			members[j], members[j-1] = members[j-1], members[j]
		}
	}
}

func runtimeDeliveryGroupMemberEnvelopesFromGroup(group RuntimeDeliveryGroup) []RuntimeDeliveryGroupMemberEnvelope {
	members := make([]RuntimeDeliveryGroupMemberEnvelope, 0, len(group.Members))
	for _, member := range group.Members {
		members = append(members, RuntimeDeliveryGroupMemberEnvelope{Kind: member.Kind, OutboxID: member.OutboxID, DeliveryID: member.DeliveryID})
	}
	return members
}

// RuntimeDeliveryGroupMembersDigest is stable across member ordering and
// excludes mutable status fields. It binds a remote settlement to the exact
// source cursors registered in the local group ledger.
func RuntimeDeliveryGroupMembersDigest(members []RuntimeDeliveryGroupMemberEnvelope) string {
	normalized, err := runtimeDeliveryGroupMemberEnvelopes(members)
	if err != nil {
		return ""
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// RuntimeDeliveryGroupEnvelope binds one remote settlement to a stable local
// group. IssuedAt is refreshed for every HTTP attempt; Timestamp is the
// immutable group creation time and does not control replay freshness.
type RuntimeDeliveryGroupEnvelope struct {
	Version       int                                  `json:"version"`
	Source        string                               `json:"source"`
	Destination   string                               `json:"destination"`
	GroupID       string                               `json:"group_id"`
	InvocationID  string                               `json:"invocation_id"`
	FenceID       string                               `json:"fence_id,omitempty"`
	FenceRevision int64                                `json:"fence_revision,omitempty"`
	Members       []RuntimeDeliveryGroupMemberEnvelope `json:"members"`
	MembersDigest string                               `json:"members_digest"`
	Timestamp     time.Time                            `json:"timestamp"`
	IssuedAt      time.Time                            `json:"issued_at"`
	Signature     string                               `json:"signature,omitempty"`
}

func (e RuntimeDeliveryGroupEnvelope) normalize() (RuntimeDeliveryGroupEnvelope, error) {
	e.Source = strings.TrimSpace(e.Source)
	e.Destination = strings.TrimSpace(e.Destination)
	e.GroupID = strings.TrimSpace(e.GroupID)
	e.InvocationID = strings.TrimSpace(e.InvocationID)
	e.FenceID = strings.TrimSpace(e.FenceID)
	e.MembersDigest = strings.TrimSpace(strings.ToLower(e.MembersDigest))
	e.Signature = strings.TrimSpace(strings.ToLower(e.Signature))
	if e.Version != RuntimeDeliveryGroupEnvelopeVersion {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: version %d 不受支持", ErrInvalidRuntimeDeliveryGroupTransaction, e.Version)
	}
	if e.Source == "" || e.Destination == "" || e.GroupID == "" || e.InvocationID == "" || e.Timestamp.IsZero() || e.IssuedAt.IsZero() {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: metadata 不完整", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if len(e.Source) > MaxRuntimeDeliveryAttemptSourceLength || len(e.Destination) > MaxRuntimeDeliveryAttemptDestinationLength || len(e.GroupID) > MaxRuntimeDeliveryGroupIDLength || len(e.InvocationID) > MaxRuntimeDeliveryGroupInvocationLength {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if err := validateRuntimeDeliveryGroupFenceFields(e.Source, e.Destination, e.InvocationID, e.FenceID, e.FenceRevision); err != nil {
		return RuntimeDeliveryGroupEnvelope{}, err
	}
	members, err := runtimeDeliveryGroupMemberEnvelopes(e.Members)
	if err != nil {
		return RuntimeDeliveryGroupEnvelope{}, err
	}
	e.Members = members
	digest := RuntimeDeliveryGroupMembersDigest(members)
	if digest == "" || e.MembersDigest != digest {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: members_digest 不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	if RuntimeDeliveryGroupID(e.Source, e.Destination, e.InvocationID, func() []RuntimeDeliveryGroupMember {
		converted := make([]RuntimeDeliveryGroupMember, 0, len(members))
		for _, member := range members {
			converted = append(converted, RuntimeDeliveryGroupMember{Kind: member.Kind, OutboxID: member.OutboxID, DeliveryID: member.DeliveryID})
		}
		return converted
	}()) != e.GroupID {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: group_id 不是稳定派生值", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	if !isRuntimeEventDeliveryDigest(e.MembersDigest) {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: members_digest 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	e.Timestamp = e.Timestamp.UTC()
	e.IssuedAt = e.IssuedAt.UTC()
	if e.Signature != "" && !isRuntimeEventDeliveryDigest(e.Signature) {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	return e, nil
}

func (e RuntimeDeliveryGroupEnvelope) Validate() error {
	_, err := e.normalize()
	return err
}

func NormalizeRuntimeDeliveryGroupEnvelope(e RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupEnvelope, error) {
	return e.normalize()
}

func (e RuntimeDeliveryGroupEnvelope) canonicalUnsigned() ([]byte, error) {
	normalized, err := e.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeDeliveryGroupEnvelope(e RuntimeDeliveryGroupEnvelope, secret []byte) (RuntimeDeliveryGroupEnvelope, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeDeliveryGroupEnvelope{}, err
	}
	canonical, err := e.canonicalUnsigned()
	if err != nil {
		return RuntimeDeliveryGroupEnvelope{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	e.Signature = hex.EncodeToString(mac.Sum(nil))
	return e.normalize()
}

func VerifyRuntimeDeliveryGroupEnvelope(e RuntimeDeliveryGroupEnvelope, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := e.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	return nil
}

type RuntimeDeliveryGroupTransactionStatus string

const (
	RuntimeDeliveryGroupTransactionPrepared  RuntimeDeliveryGroupTransactionStatus = "prepared"
	RuntimeDeliveryGroupTransactionCommitted RuntimeDeliveryGroupTransactionStatus = "committed"
	RuntimeDeliveryGroupTransactionAborted   RuntimeDeliveryGroupTransactionStatus = "aborted"
)

// RuntimeDeliveryGroupTransaction is a destination-side metadata ledger. It
// records group settlement intent only; family-specific inboxes remain the
// source of truth for event/checkpoint/config/rejection payloads.
type RuntimeDeliveryGroupTransaction struct {
	Version       int                                   `json:"version"`
	GroupID       string                                `json:"group_id"`
	Source        string                                `json:"source"`
	Destination   string                                `json:"destination"`
	InvocationID  string                                `json:"invocation_id"`
	FenceID       string                                `json:"fence_id,omitempty"`
	FenceRevision int64                                 `json:"fence_revision,omitempty"`
	Members       []RuntimeDeliveryGroupMemberEnvelope  `json:"members"`
	MembersDigest string                                `json:"members_digest"`
	Timestamp     time.Time                             `json:"timestamp"`
	IssuedAt      time.Time                             `json:"issued_at"`
	Signature     string                                `json:"-"`
	Status        RuntimeDeliveryGroupTransactionStatus `json:"status"`
	ExpiresAt     time.Time                             `json:"expires_at"`
	CreatedAt     time.Time                             `json:"created_at"`
	UpdatedAt     time.Time                             `json:"updated_at"`
}

func (t RuntimeDeliveryGroupTransaction) envelope() RuntimeDeliveryGroupEnvelope {
	return RuntimeDeliveryGroupEnvelope{Version: t.Version, Source: t.Source, Destination: t.Destination, GroupID: t.GroupID, InvocationID: t.InvocationID, FenceID: t.FenceID, FenceRevision: t.FenceRevision, Members: append([]RuntimeDeliveryGroupMemberEnvelope(nil), t.Members...), MembersDigest: t.MembersDigest, Timestamp: t.Timestamp, IssuedAt: t.IssuedAt, Signature: t.Signature}
}

func (t RuntimeDeliveryGroupTransaction) Envelope() RuntimeDeliveryGroupEnvelope { return t.envelope() }

func (t RuntimeDeliveryGroupTransaction) normalize(now time.Time) (RuntimeDeliveryGroupTransaction, error) {
	normalizedEnvelope, err := t.envelope().normalize()
	if err != nil {
		return RuntimeDeliveryGroupTransaction{}, err
	}
	if t.Status == "" {
		t.Status = RuntimeDeliveryGroupTransactionPrepared
	}
	switch t.Status {
	case RuntimeDeliveryGroupTransactionPrepared, RuntimeDeliveryGroupTransactionCommitted, RuntimeDeliveryGroupTransactionAborted:
	default:
		return RuntimeDeliveryGroupTransaction{}, fmt.Errorf("%w: status %q 不受支持", ErrInvalidRuntimeDeliveryGroupTransaction, t.Status)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = now.Add(DefaultRuntimeDeliveryGroupTransactionTTL)
	} else {
		t.ExpiresAt = t.ExpiresAt.UTC()
	}
	if t.Status == RuntimeDeliveryGroupTransactionPrepared && !t.ExpiresAt.After(now) {
		return RuntimeDeliveryGroupTransaction{}, ErrRuntimeDeliveryGroupTransactionStale
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	} else {
		t.CreatedAt = t.CreatedAt.UTC()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	} else {
		t.UpdatedAt = t.UpdatedAt.UTC()
	}
	t.Version, t.GroupID, t.Source, t.Destination, t.InvocationID = normalizedEnvelope.Version, normalizedEnvelope.GroupID, normalizedEnvelope.Source, normalizedEnvelope.Destination, normalizedEnvelope.InvocationID
	t.FenceID, t.FenceRevision = normalizedEnvelope.FenceID, normalizedEnvelope.FenceRevision
	t.Members = append([]RuntimeDeliveryGroupMemberEnvelope(nil), normalizedEnvelope.Members...)
	t.MembersDigest, t.Timestamp, t.IssuedAt, t.Signature = normalizedEnvelope.MembersDigest, normalizedEnvelope.Timestamp, normalizedEnvelope.IssuedAt, normalizedEnvelope.Signature
	return t, nil
}

func (t RuntimeDeliveryGroupTransaction) Normalize(now time.Time) (RuntimeDeliveryGroupTransaction, error) {
	return t.normalize(now)
}

func NewRuntimeDeliveryGroupTransaction(envelope RuntimeDeliveryGroupEnvelope, now time.Time) (RuntimeDeliveryGroupTransaction, error) {
	normalized, err := envelope.normalize()
	if err != nil {
		return RuntimeDeliveryGroupTransaction{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return RuntimeDeliveryGroupTransaction{Version: normalized.Version, GroupID: normalized.GroupID, Source: normalized.Source, Destination: normalized.Destination, InvocationID: normalized.InvocationID, FenceID: normalized.FenceID, FenceRevision: normalized.FenceRevision, Members: append([]RuntimeDeliveryGroupMemberEnvelope(nil), normalized.Members...), MembersDigest: normalized.MembersDigest, Timestamp: normalized.Timestamp, IssuedAt: normalized.IssuedAt, Signature: normalized.Signature, Status: RuntimeDeliveryGroupTransactionPrepared, ExpiresAt: now.Add(DefaultRuntimeDeliveryGroupTransactionTTL), CreatedAt: now, UpdatedAt: now}, nil
}

func (t RuntimeDeliveryGroupTransaction) Matches(envelope RuntimeDeliveryGroupEnvelope) bool {
	normalized, err := envelope.normalize()
	if err != nil {
		return false
	}
	return t.Version == normalized.Version && t.GroupID == normalized.GroupID && t.Source == normalized.Source && t.Destination == normalized.Destination && t.InvocationID == normalized.InvocationID && t.FenceID == normalized.FenceID && t.FenceRevision == normalized.FenceRevision && t.MembersDigest == normalized.MembersDigest && t.Timestamp.UTC().Equal(normalized.Timestamp.UTC()) && sameRuntimeDeliveryGroupMembers(t.Members, normalized.Members)
}

func sameRuntimeDeliveryGroupMembers(left, right []RuntimeDeliveryGroupMemberEnvelope) bool {
	left, leftErr := runtimeDeliveryGroupMemberEnvelopes(left)
	right, rightErr := runtimeDeliveryGroupMemberEnvelopes(right)
	if leftErr != nil || rightErr != nil || len(left) != len(right) {
		return false
	}
	for index := range left {
		if runtimeDeliveryGroupMemberEnvelopeIdentity(left[index]) != runtimeDeliveryGroupMemberEnvelopeIdentity(right[index]) {
			return false
		}
	}
	return true
}

type RuntimeDeliveryGroupTransactionReceipt struct {
	Version       int    `json:"version"`
	GroupID       string `json:"group_id"`
	InvocationID  string `json:"invocation_id"`
	FenceID       string `json:"fence_id,omitempty"`
	FenceRevision int64  `json:"fence_revision,omitempty"`
	MembersDigest string `json:"members_digest"`
	Phase         string `json:"phase,omitempty"`
	Duplicate     bool   `json:"duplicate"`
}

func (r RuntimeDeliveryGroupTransactionReceipt) ValidateAgainst(envelope RuntimeDeliveryGroupEnvelope) error {
	normalized, err := envelope.normalize()
	if err != nil {
		return err
	}
	if r.Version != normalized.Version || strings.TrimSpace(r.GroupID) != normalized.GroupID || strings.TrimSpace(r.InvocationID) != normalized.InvocationID || strings.TrimSpace(r.FenceID) != normalized.FenceID || r.FenceRevision != normalized.FenceRevision || strings.TrimSpace(strings.ToLower(r.MembersDigest)) != normalized.MembersDigest {
		return fmt.Errorf("%w: receipt metadata 不一致", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	return nil
}

func (r RuntimeDeliveryGroupTransactionReceipt) ValidateAgainstPhase(envelope RuntimeDeliveryGroupEnvelope, phase string) error {
	if err := r.ValidateAgainst(envelope); err != nil {
		return err
	}
	if strings.TrimSpace(r.Phase) != strings.TrimSpace(phase) {
		return fmt.Errorf("%w: receipt phase 不一致", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	return nil
}

// RuntimeDeliveryGroupTransactionRepository is the destination-side
// metadata ledger for group settlement. It does not itself publish any
// family-specific payload.
type RuntimeDeliveryGroupTransactionRepository interface {
	PrepareRuntimeDeliveryGroup(context.Context, RuntimeDeliveryGroupEnvelope) (duplicate bool, err error)
	CommitRuntimeDeliveryGroup(context.Context, RuntimeDeliveryGroupEnvelope) (duplicate bool, err error)
	AbortRuntimeDeliveryGroup(context.Context, RuntimeDeliveryGroupEnvelope) (duplicate bool, err error)
	GetRuntimeDeliveryGroupTransaction(context.Context, string) (RuntimeDeliveryGroupTransaction, error)
}

// RuntimeDeliveryGroupMemberPhaseResolver is optional. A receiver may require
// every family-specific transaction to be committed before recording the group
// commit; the resolver is kept separate so metadata-only receivers remain
// compatible and do not need to inspect private payload tables.
type RuntimeDeliveryGroupMemberPhaseResolver interface {
	RuntimeDeliveryGroupMemberPhase(context.Context, RuntimeDeliveryGroupMemberEnvelope) (RuntimeDeliveryGroupTransactionStatus, error)
}

type RuntimeDeliveryGroupTransactionalTransport interface {
	BuildRuntimeDeliveryGroupEnvelope(RuntimeDeliveryGroup, time.Time) (RuntimeDeliveryGroupEnvelope, error)
	PrepareRuntimeDeliveryGroup(context.Context, RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error)
	CommitRuntimeDeliveryGroup(context.Context, RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error)
	AbortRuntimeDeliveryGroup(context.Context, RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error)
}

// RuntimeDeliveryGroupReconciler is an optional read-only destination proof
// used to avoid repeating a terminal settlement after a response-loss crash.
type RuntimeDeliveryGroupReconciler interface {
	ReconcileRuntimeDeliveryGroup(context.Context, RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupRemoteStatus, error)
}

type RuntimeDeliveryGroupTransactionalOptIn interface {
	RuntimeDeliveryGroupTransactionsEnabled() bool
}

// RuntimeDeliveryGroupFenceRequirement is an explicit source-side opt-in. A
// transport that implements it asks Coordinator to issue/bind a monotonic
// fence before the first remote status/prepare call for a group.
type RuntimeDeliveryGroupFenceRequirement interface {
	RuntimeDeliveryGroupFenceRequired() bool
}

type RuntimeDeliveryGroupRemoteStatus struct {
	Version       int                                   `json:"version"`
	Source        string                                `json:"source"`
	Destination   string                                `json:"destination"`
	GroupID       string                                `json:"group_id"`
	InvocationID  string                                `json:"invocation_id"`
	FenceID       string                                `json:"fence_id,omitempty"`
	FenceRevision int64                                 `json:"fence_revision,omitempty"`
	MembersDigest string                                `json:"members_digest"`
	Phase         RuntimeDeliveryGroupTransactionStatus `json:"phase"`
	Found         bool                                  `json:"found"`
	UpdatedAt     time.Time                             `json:"updated_at,omitempty"`
	Signature     string                                `json:"signature,omitempty"`
}

func (s RuntimeDeliveryGroupRemoteStatus) normalize() (RuntimeDeliveryGroupRemoteStatus, error) {
	s.Source = strings.TrimSpace(s.Source)
	s.Destination = strings.TrimSpace(s.Destination)
	s.GroupID = strings.TrimSpace(s.GroupID)
	s.InvocationID = strings.TrimSpace(s.InvocationID)
	s.FenceID = strings.TrimSpace(s.FenceID)
	s.MembersDigest = strings.TrimSpace(strings.ToLower(s.MembersDigest))
	s.Signature = strings.TrimSpace(strings.ToLower(s.Signature))
	if s.Version != RuntimeDeliveryGroupEnvelopeVersion || s.Source == "" || s.Destination == "" || s.GroupID == "" || s.InvocationID == "" || !isRuntimeEventDeliveryDigest(s.MembersDigest) {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: status metadata 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if err := validateRuntimeDeliveryGroupFenceFields(s.Source, s.Destination, s.InvocationID, s.FenceID, s.FenceRevision); err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	switch s.Phase {
	case "":
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: status phase 不能为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	case RuntimeDeliveryGroupTransactionPrepared, RuntimeDeliveryGroupTransactionCommitted, RuntimeDeliveryGroupTransactionAborted:
		if !s.Found {
			return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: status phase 必须标记 found", ErrInvalidRuntimeDeliveryGroupTransaction)
		}
	case "absent":
		if s.Found {
			return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: absent status 不能标记 found", ErrInvalidRuntimeDeliveryGroupTransaction)
		}
	default:
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: status phase %q 不受支持", ErrInvalidRuntimeDeliveryGroupTransaction, s.Phase)
	}
	if !s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.UpdatedAt.UTC()
	}
	if s.Signature != "" && !isRuntimeEventDeliveryDigest(s.Signature) {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	return s, nil
}

func NewRuntimeDeliveryGroupRemoteStatus(envelope RuntimeDeliveryGroupEnvelope, phase RuntimeDeliveryGroupTransactionStatus, found bool, updatedAt time.Time) (RuntimeDeliveryGroupRemoteStatus, error) {
	normalized, err := envelope.normalize()
	if err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	if phase == "" {
		phase = "absent"
	}
	if phase == "absent" {
		found = false
	} else {
		found = true
	}
	return (RuntimeDeliveryGroupRemoteStatus{Version: RuntimeDeliveryGroupEnvelopeVersion, Source: normalized.Source, Destination: normalized.Destination, GroupID: normalized.GroupID, InvocationID: normalized.InvocationID, FenceID: normalized.FenceID, FenceRevision: normalized.FenceRevision, MembersDigest: normalized.MembersDigest, Phase: phase, Found: found, UpdatedAt: updatedAt}).normalize()
}

func SignRuntimeDeliveryGroupStatus(status RuntimeDeliveryGroupRemoteStatus, secret []byte) (RuntimeDeliveryGroupRemoteStatus, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	normalized, err := status.normalize()
	if err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	normalized.Signature = ""
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	status.Signature = hex.EncodeToString(mac.Sum(nil))
	return status.normalize()
}

func VerifyRuntimeDeliveryGroupStatus(status RuntimeDeliveryGroupRemoteStatus, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := status.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	signature := normalized.Signature
	normalized.Signature = ""
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	return nil
}

// ValidateAgainst binds a signed status proof to the exact source envelope
// that was queried. The transport performs this check as well, but keeping it
// on the value lets durable settlement workers defend against custom
// reconcilers that are not HTTP implementations.
func (s RuntimeDeliveryGroupRemoteStatus) ValidateAgainst(envelope RuntimeDeliveryGroupEnvelope) error {
	normalized, err := envelope.normalize()
	if err != nil {
		return err
	}
	status, err := s.normalize()
	if err != nil {
		return err
	}
	if status.Version != normalized.Version || status.Source != normalized.Source || status.Destination != normalized.Destination || status.GroupID != normalized.GroupID || status.InvocationID != normalized.InvocationID || status.FenceID != normalized.FenceID || status.FenceRevision != normalized.FenceRevision || status.MembersDigest != normalized.MembersDigest {
		return fmt.Errorf("%w: status metadata 不一致", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	return nil
}

func validateRuntimeDeliveryGroupTimestamp(timestamp, now time.Time, maxAge, futureSkew time.Duration) error {
	if maxAge <= 0 || futureSkew < 0 {
		return fmt.Errorf("%w: timestamp policy 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	timestamp = timestamp.UTC()
	if timestamp.After(now.Add(futureSkew)) {
		return fmt.Errorf("%w: timestamp 位于允许未来窗口之外", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	if now.Sub(timestamp) > maxAge {
		return ErrRuntimeDeliveryGroupTransactionStale
	}
	return nil
}

func runtimeDeliveryGroupTransactionEndpoint(base, phase string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	phase = strings.TrimSpace(phase)
	if phase == "" {
		phase = "status"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + phase
	return u.String(), nil
}

func NewRuntimeDeliveryGroupEnvelope(group RuntimeDeliveryGroup, now time.Time) (RuntimeDeliveryGroupEnvelope, error) {
	normalized, err := group.Normalize(now)
	if err != nil {
		return RuntimeDeliveryGroupEnvelope{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	members := runtimeDeliveryGroupMemberEnvelopesFromGroup(normalized)
	return (RuntimeDeliveryGroupEnvelope{Version: RuntimeDeliveryGroupEnvelopeVersion, Source: normalized.Source, Destination: normalized.Destination, GroupID: normalized.ID, InvocationID: normalized.InvocationID, FenceID: normalized.FenceID, FenceRevision: normalized.FenceRevision, Members: members, MembersDigest: RuntimeDeliveryGroupMembersDigest(members), Timestamp: normalized.CreatedAt.UTC(), IssuedAt: now}).normalize()
}

// RuntimeDeliveryGroupHTTPTransport is an opt-in remote group settlement
// adapter. It never sends family payloads; those remain on their own routes.
type RuntimeDeliveryGroupHTTPTransport struct {
	Endpoint      string
	Source        string
	Destination   string
	SharedSecret  []byte
	Client        *http.Client
	MaxBodyBytes  int64
	transactional bool
	requireFence  bool
}

func NewRuntimeDeliveryGroupHTTPTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeDeliveryGroupHTTPTransport, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &RuntimeDeliveryGroupHTTPTransport{Endpoint: endpoint, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Client: client, MaxBodyBytes: MaxRuntimeDeliveryGroupResponseBytes}, nil
}

func NewRuntimeDeliveryGroupHTTPTransactionalTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeDeliveryGroupHTTPTransport, error) {
	transport, err := NewRuntimeDeliveryGroupHTTPTransport(endpoint, source, destination, secret, client)
	if err != nil {
		return nil, err
	}
	transport.transactional = true
	return transport, nil
}

// SetRuntimeDeliveryGroupFenceRequired enables source-side automatic fence
// binding for this transport. It is deliberately a setter instead of a
// constructor argument to preserve the legacy constructor contract.
func (t *RuntimeDeliveryGroupHTTPTransport) SetRuntimeDeliveryGroupFenceRequired(required bool) {
	if t != nil {
		t.requireFence = required
	}
}

func (t *RuntimeDeliveryGroupHTTPTransport) RuntimeDeliveryGroupFenceRequired() bool {
	return t != nil && t.requireFence
}

func (t *RuntimeDeliveryGroupHTTPTransport) RuntimeDeliveryGroupTransactionsEnabled() bool {
	return t != nil && t.transactional
}

func (t *RuntimeDeliveryGroupHTTPTransport) BuildRuntimeDeliveryGroupEnvelope(group RuntimeDeliveryGroup, now time.Time) (RuntimeDeliveryGroupEnvelope, error) {
	if t == nil {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	envelope, err := NewRuntimeDeliveryGroupEnvelope(group, now)
	if err != nil {
		return RuntimeDeliveryGroupEnvelope{}, err
	}
	if envelope.Source != t.Source || envelope.Destination != t.Destination {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	return SignRuntimeDeliveryGroupEnvelope(envelope, t.SharedSecret)
}

func (t *RuntimeDeliveryGroupHTTPTransport) PrepareRuntimeDeliveryGroup(ctx context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	return t.do(ctx, "prepare", envelope)
}

func (t *RuntimeDeliveryGroupHTTPTransport) CommitRuntimeDeliveryGroup(ctx context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	return t.do(ctx, "commit", envelope)
}

func (t *RuntimeDeliveryGroupHTTPTransport) AbortRuntimeDeliveryGroup(ctx context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	return t.do(ctx, "abort", envelope)
}

func (t *RuntimeDeliveryGroupHTTPTransport) ReconcileRuntimeDeliveryGroup(ctx context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupRemoteStatus, error) {
	if t == nil {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	normalized, err := NormalizeRuntimeDeliveryGroupEnvelope(envelope)
	if err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	signed, err := SignRuntimeDeliveryGroupEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	body, err := json.Marshal(signed)
	if err != nil || int64(len(body)) > t.maxBodyBytes() {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: status request body 超过上限", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	endpoint, err := runtimeDeliveryGroupTransactionEndpoint(t.Endpoint, "status")
	if err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Delivery-Group-Version", fmt.Sprint(signed.Version))
	request.Header.Set("X-Abot-Delivery-Group-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.GroupID)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("Runtime delivery group status 请求失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, MaxRuntimeDeliveryGroupResponseBytes+1))
	if err != nil || len(responseBody) > MaxRuntimeDeliveryGroupResponseBytes {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: status response 超过上限或读取失败", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
			return RuntimeDeliveryGroupRemoteStatus{}, ErrRuntimeDeliveryGroupTransactionUnavailable
		}
		if response.StatusCode == http.StatusConflict {
			return RuntimeDeliveryGroupRemoteStatus{}, ErrConflict
		}
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("Runtime delivery group status 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var status RuntimeDeliveryGroupRemoteStatus
	if err := decoder.Decode(&status); err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: status JSON 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeDeliveryGroupRemoteStatus{}, fmt.Errorf("%w: status 包含多个 JSON 文档", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if err := VerifyRuntimeDeliveryGroupStatus(status, t.SharedSecret); err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	if err := status.ValidateAgainst(normalized); err != nil {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	return status, nil
}

func (t *RuntimeDeliveryGroupHTTPTransport) do(ctx context.Context, phase string, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupTransactionReceipt, error) {
	if t == nil {
		return RuntimeDeliveryGroupTransactionReceipt{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	normalized, err := NormalizeRuntimeDeliveryGroupEnvelope(envelope)
	if err != nil {
		return RuntimeDeliveryGroupTransactionReceipt{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeDeliveryGroupTransactionReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	signed, err := SignRuntimeDeliveryGroupEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeDeliveryGroupTransactionReceipt{}, err
	}
	body, err := json.Marshal(signed)
	if err != nil || int64(len(body)) > t.maxBodyBytes() {
		return RuntimeDeliveryGroupTransactionReceipt{}, fmt.Errorf("%w: request body 超过上限", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	endpoint, err := runtimeDeliveryGroupTransactionEndpoint(t.Endpoint, phase)
	if err != nil {
		return RuntimeDeliveryGroupTransactionReceipt{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeDeliveryGroupTransactionReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Delivery-Group-Version", fmt.Sprint(signed.Version))
	request.Header.Set("X-Abot-Delivery-Group-Signature", signed.Signature)
	request.Header.Set("X-Abot-Delivery-Group-Phase", phase)
	request.Header.Set("Idempotency-Key", signed.GroupID)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeDeliveryGroupTransactionReceipt{}, fmt.Errorf("Runtime delivery group 请求失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, MaxRuntimeDeliveryGroupResponseBytes+1))
	if err != nil || len(responseBody) > MaxRuntimeDeliveryGroupResponseBytes {
		return RuntimeDeliveryGroupTransactionReceipt{}, fmt.Errorf("%w: response 超过上限或读取失败", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
			return RuntimeDeliveryGroupTransactionReceipt{}, ErrRuntimeDeliveryGroupTransactionUnavailable
		}
		if response.StatusCode == http.StatusConflict {
			return RuntimeDeliveryGroupTransactionReceipt{}, ErrConflict
		}
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		return RuntimeDeliveryGroupTransactionReceipt{}, fmt.Errorf("Runtime delivery group 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeDeliveryGroupTransactionReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeDeliveryGroupTransactionReceipt{}, fmt.Errorf("%w: receipt JSON 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeDeliveryGroupTransactionReceipt{}, fmt.Errorf("%w: receipt 包含多个 JSON 文档", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if err := receipt.ValidateAgainstPhase(normalized, phase); err != nil {
		return RuntimeDeliveryGroupTransactionReceipt{}, err
	}
	return receipt, nil
}

func (t *RuntimeDeliveryGroupHTTPTransport) maxBodyBytes() int64 {
	if t.MaxBodyBytes <= 0 || t.MaxBodyBytes > MaxRuntimeDeliveryGroupEnvelopeBytes {
		return MaxRuntimeDeliveryGroupEnvelopeBytes
	}
	return t.MaxBodyBytes
}

var _ RuntimeDeliveryGroupTransactionalTransport = (*RuntimeDeliveryGroupHTTPTransport)(nil)
var _ RuntimeDeliveryGroupTransactionalOptIn = (*RuntimeDeliveryGroupHTTPTransport)(nil)
var _ RuntimeDeliveryGroupReconciler = (*RuntimeDeliveryGroupHTTPTransport)(nil)
var _ RuntimeDeliveryGroupFenceRequirement = (*RuntimeDeliveryGroupHTTPTransport)(nil)
