package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RuntimeDeliveryStatusVersion is the stable, metadata-only status contract
// used by source workers to reconcile a destination after a crash.  The
// status response never contains an event body, checkpoint projection,
// configuration snapshot or rejection reason.
const RuntimeDeliveryStatusVersion = 1

const (
	RuntimeDeliveryStatusAbsent    RuntimeDeliveryStatusPhase = "absent"
	RuntimeDeliveryStatusPrepared  RuntimeDeliveryStatusPhase = "prepared"
	RuntimeDeliveryStatusAccepted  RuntimeDeliveryStatusPhase = "accepted"
	RuntimeDeliveryStatusCommitted RuntimeDeliveryStatusPhase = "committed"

	MaxRuntimeDeliveryStatusBodyBytes      = 16 << 10
	MaxRuntimeDeliveryStatusSourceLength   = MaxRuntimeEventDeliverySourceLength
	MaxRuntimeDeliveryStatusTargetLength   = MaxRuntimeEventDeliveryTargetLength
	MaxRuntimeDeliveryStatusDeliveryLength = MaxRuntimeDeliveryAttemptDeliveryIDLength
	MaxRuntimeDeliveryStatusInvocationLen  = MaxRuntimeDeliveryAttemptInvocationLength
)

var (
	ErrInvalidRuntimeDeliveryStatus = errors.New("Runtime delivery status 无效")
	ErrRuntimeDeliveryStatusAuth    = errors.New("Runtime delivery status 认证失败")
)

// RuntimeDeliveryStatusPhase describes the destination-side durable proof.
// Absent is an explicit, authenticated answer and is different from a status
// endpoint that is unavailable or failed authentication.
type RuntimeDeliveryStatusPhase string

// RuntimeDeliveryStatus is signed by the destination with the same route
// secret as the delivery envelope.  UpdatedAt is operational metadata only;
// no provider or user-authored content crosses this boundary.
type RuntimeDeliveryStatus struct {
	Version      int                        `json:"version"`
	Kind         RuntimeDeliveryKind        `json:"kind"`
	Source       string                     `json:"source"`
	Destination  string                     `json:"destination"`
	DeliveryID   string                     `json:"delivery_id"`
	InvocationID string                     `json:"invocation_id"`
	Phase        RuntimeDeliveryStatusPhase `json:"phase"`
	Found        bool                       `json:"found"`
	UpdatedAt    time.Time                  `json:"updated_at,omitempty"`
	Signature    string                     `json:"signature,omitempty"`
}

func (s RuntimeDeliveryStatus) normalize() (RuntimeDeliveryStatus, error) {
	s.Kind = RuntimeDeliveryKind(strings.TrimSpace(string(s.Kind)))
	s.Source = strings.TrimSpace(s.Source)
	s.Destination = strings.TrimSpace(s.Destination)
	s.DeliveryID = strings.TrimSpace(s.DeliveryID)
	s.InvocationID = strings.TrimSpace(s.InvocationID)
	s.Signature = strings.TrimSpace(strings.ToLower(s.Signature))
	if s.Version != RuntimeDeliveryStatusVersion {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: version %d 不受支持", ErrInvalidRuntimeDeliveryStatus, s.Version)
	}
	if !validRuntimeDeliveryKind(s.Kind) {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: kind %q 不受支持", ErrInvalidRuntimeDeliveryStatus, s.Kind)
	}
	if s.Source == "" || s.Destination == "" || s.DeliveryID == "" || s.InvocationID == "" {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: metadata 不完整", ErrInvalidRuntimeDeliveryStatus)
	}
	if len(s.Source) > MaxRuntimeDeliveryStatusSourceLength || len(s.Destination) > MaxRuntimeDeliveryStatusTargetLength || len(s.DeliveryID) > MaxRuntimeDeliveryStatusDeliveryLength || len(s.InvocationID) > MaxRuntimeDeliveryStatusInvocationLen {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeDeliveryStatus)
	}
	switch s.Phase {
	case RuntimeDeliveryStatusAbsent:
		if s.Found {
			return RuntimeDeliveryStatus{}, fmt.Errorf("%w: absent status 不能标记 found", ErrInvalidRuntimeDeliveryStatus)
		}
	case RuntimeDeliveryStatusPrepared, RuntimeDeliveryStatusAccepted, RuntimeDeliveryStatusCommitted:
		if !s.Found {
			return RuntimeDeliveryStatus{}, fmt.Errorf("%w: %s status 必须标记 found", ErrInvalidRuntimeDeliveryStatus, s.Phase)
		}
	default:
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: phase %q 不受支持", ErrInvalidRuntimeDeliveryStatus, s.Phase)
	}
	if s.Signature != "" && !isRuntimeEventDeliveryDigest(s.Signature) {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeDeliveryStatus)
	}
	if !s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.UpdatedAt.UTC()
	}
	return s, nil
}

func NormalizeRuntimeDeliveryStatus(status RuntimeDeliveryStatus) (RuntimeDeliveryStatus, error) {
	return status.normalize()
}

func (s RuntimeDeliveryStatus) Validate() error {
	_, err := s.normalize()
	return err
}

func NewRuntimeDeliveryStatus(kind RuntimeDeliveryKind, source, destination, deliveryID, invocationID string, phase RuntimeDeliveryStatusPhase, found bool, updatedAt time.Time) (RuntimeDeliveryStatus, error) {
	if phase == RuntimeDeliveryStatusAbsent {
		found = false
	} else {
		found = true
	}
	return (RuntimeDeliveryStatus{
		Version: RuntimeDeliveryStatusVersion, Kind: kind, Source: source, Destination: destination,
		DeliveryID: deliveryID, InvocationID: invocationID, Phase: phase, Found: found, UpdatedAt: updatedAt,
	}).normalize()
}

func (s RuntimeDeliveryStatus) canonicalUnsigned() ([]byte, error) {
	normalized, err := s.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeDeliveryStatus(status RuntimeDeliveryStatus, secret []byte) (RuntimeDeliveryStatus, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	canonical, err := status.canonicalUnsigned()
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	status.Signature = hex.EncodeToString(mac.Sum(nil))
	return status.normalize()
}

func VerifyRuntimeDeliveryStatus(status RuntimeDeliveryStatus, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := status.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeDeliveryStatusAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeDeliveryStatusAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeDeliveryStatusAuth)
	}
	return nil
}

func (s RuntimeDeliveryStatus) Matches(kind RuntimeDeliveryKind, source, destination, deliveryID, invocationID string) bool {
	normalized, err := s.normalize()
	if err != nil {
		return false
	}
	return normalized.Kind == kind && normalized.Source == strings.TrimSpace(source) && normalized.Destination == strings.TrimSpace(destination) && normalized.DeliveryID == strings.TrimSpace(deliveryID) && normalized.InvocationID == strings.TrimSpace(invocationID)
}

// RuntimeDeliveryStatusUnavailable is returned by a receiver whose backing
// inbox does not expose a durable read path.  It is intentionally distinct
// from an authenticated "absent" answer so a source never treats an
// unsupported endpoint as permission to release its outbox cursor.
var ErrRuntimeDeliveryStatusUnavailable = errors.New("Runtime delivery status 查询未配置")

func runtimeDeliveryStatusEndpoint(base string) (string, error) {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeDeliveryStatus)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/status"
	return u.String(), nil
}
