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
	"strings"
	"time"
)

// RuntimeConfigDirectoryRebindApplyStatusVersion is kept separate from the
// apply receipt version: a status proof is a read-only answer and may evolve
// without changing the projection acceptance contract.
const RuntimeConfigDirectoryRebindApplyStatusVersion = 1

const (
	RuntimeConfigDirectoryRebindApplyStatusAbsent       RuntimeConfigDirectoryRebindApplyStatusPhase = "absent"
	RuntimeConfigDirectoryRebindApplyStatusAccepted     RuntimeConfigDirectoryRebindApplyStatusPhase = "accepted"
	MaxRuntimeConfigDirectoryRebindApplyStatusBodyBytes                                              = 16 << 10
)

var (
	ErrInvalidRuntimeConfigDirectoryRebindApplyStatus     = errors.New("Runtime 配置目录 rebind apply status 无效")
	ErrRuntimeConfigDirectoryRebindApplyStatusAuth        = errors.New("Runtime 配置目录 rebind apply status 认证失败")
	ErrRuntimeConfigDirectoryRebindApplyStatusUnavailable = errors.New("Runtime 配置目录 rebind apply status 查询未配置")
)

type RuntimeConfigDirectoryRebindApplyStatusPhase string

// RuntimeConfigDirectoryRebindApplyStatusProof is a signed, metadata-only
// destination answer. It deliberately omits the checkpoint projection.
type RuntimeConfigDirectoryRebindApplyStatusProof struct {
	Version              int                                          `json:"version"`
	Source               string                                       `json:"source"`
	Destination          string                                       `json:"destination"`
	PlanID               string                                       `json:"plan_id"`
	ApplyID              string                                       `json:"apply_id"`
	ConfirmationID       string                                       `json:"confirmation_id"`
	ConfirmationDigest   string                                       `json:"confirmation_digest"`
	InvocationID         string                                       `json:"invocation_id"`
	BoundaryStatus       InvocationStatus                             `json:"boundary_status"`
	ConfigSnapshotDigest string                                       `json:"config_snapshot_digest"`
	SnapshotRevision     int64                                        `json:"snapshot_revision"`
	EventSequence        int64                                        `json:"event_sequence"`
	SnapshotDigest       string                                       `json:"snapshot_digest"`
	Phase                RuntimeConfigDirectoryRebindApplyStatusPhase `json:"phase"`
	Found                bool                                         `json:"found"`
	UpdatedAt            time.Time                                    `json:"updated_at,omitempty"`
	IssuedAt             time.Time                                    `json:"issued_at"`
	Signature            string                                       `json:"signature,omitempty"`
}

func (p RuntimeConfigDirectoryRebindApplyStatusProof) normalize() (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
	p.Source = strings.TrimSpace(p.Source)
	p.Destination = strings.TrimSpace(p.Destination)
	p.PlanID = strings.TrimSpace(p.PlanID)
	p.ApplyID = strings.TrimSpace(p.ApplyID)
	p.ConfirmationID = strings.TrimSpace(p.ConfirmationID)
	p.ConfirmationDigest = strings.TrimSpace(strings.ToLower(p.ConfirmationDigest))
	p.InvocationID = strings.TrimSpace(p.InvocationID)
	p.BoundaryStatus = InvocationStatus(strings.TrimSpace(strings.ToLower(string(p.BoundaryStatus))))
	p.ConfigSnapshotDigest = strings.TrimSpace(strings.ToLower(p.ConfigSnapshotDigest))
	p.SnapshotDigest = strings.TrimSpace(strings.ToLower(p.SnapshotDigest))
	p.Phase = RuntimeConfigDirectoryRebindApplyStatusPhase(strings.TrimSpace(strings.ToLower(string(p.Phase))))
	p.Signature = strings.TrimSpace(strings.ToLower(p.Signature))
	if p.Version != RuntimeConfigDirectoryRebindApplyStatusVersion || p.Source == "" || p.Destination == "" || p.PlanID == "" || p.ApplyID == "" || p.ConfirmationID == "" || p.ConfirmationDigest == "" || p.InvocationID == "" || p.SnapshotRevision <= 0 || p.EventSequence < 0 || p.ConfigSnapshotDigest == "" || p.SnapshotDigest == "" || p.IssuedAt.IsZero() {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: metadata 不完整", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	if p.BoundaryStatus != InvocationWaitingTool && p.BoundaryStatus != InvocationWaitingApproval && p.BoundaryStatus != InvocationWaitingUser {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: boundary 不受支持", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	if !runtimeConfigSnapshotDigestValid(p.ConfigSnapshotDigest) || !isRuntimeEventDeliveryDigest(p.SnapshotDigest) || !isRuntimeEventDeliveryDigest(p.ConfirmationDigest) {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: digest 无效", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	if len(p.Source) > MaxRuntimeConfigDirectorySourceLength || len(p.Destination) > MaxRuntimeConfigDirectoryTargetLength || len(p.PlanID) > MaxRuntimeConfigDirectoryRebindID || len(p.ApplyID) > MaxRuntimeConfigDirectoryRebindApplyIDLength || len(p.ConfirmationID) > MaxRuntimeConfigDirectoryRebindConfirmationIDLength || len(p.InvocationID) > MaxRuntimeConfigDirectoryRebindInvocationID {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	switch p.Phase {
	case RuntimeConfigDirectoryRebindApplyStatusAbsent:
		if p.Found {
			return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: absent proof 不能 found", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
		}
	case RuntimeConfigDirectoryRebindApplyStatusAccepted:
		if !p.Found {
			return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: accepted proof 必须 found", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
		}
	default:
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: phase %q 不受支持", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus, p.Phase)
	}
	if !p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.UpdatedAt.UTC()
	}
	p.IssuedAt = p.IssuedAt.UTC()
	if p.Signature != "" && !isRuntimeEventDeliveryDigest(p.Signature) {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	return p, nil
}

func NormalizeRuntimeConfigDirectoryRebindApplyStatusProof(proof RuntimeConfigDirectoryRebindApplyStatusProof) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
	return proof.normalize()
}

func NewRuntimeConfigDirectoryRebindApplyStatusProof(envelope RuntimeConfigDirectoryRebindApplyEnvelope, phase RuntimeConfigDirectoryRebindApplyStatusPhase, found bool, updatedAt, issuedAt time.Time) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
	envelope, err := envelope.normalize()
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	if phase == RuntimeConfigDirectoryRebindApplyStatusAbsent {
		found = false
	} else if phase == RuntimeConfigDirectoryRebindApplyStatusAccepted {
		found = true
	} else {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: phase 不受支持", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	if issuedAt.IsZero() {
		issuedAt = time.Now().UTC()
	}
	return (RuntimeConfigDirectoryRebindApplyStatusProof{
		Version: RuntimeConfigDirectoryRebindApplyStatusVersion, Source: envelope.Source, Destination: envelope.Destination,
		PlanID: envelope.PlanID, ApplyID: envelope.ApplyID, ConfirmationID: envelope.ConfirmationID, ConfirmationDigest: envelope.ConfirmationDigest,
		InvocationID: envelope.InvocationID, BoundaryStatus: envelope.BoundaryStatus, ConfigSnapshotDigest: envelope.ConfigSnapshotDigest,
		SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence, SnapshotDigest: envelope.SnapshotDigest,
		Phase: phase, Found: found, UpdatedAt: updatedAt, IssuedAt: issuedAt,
	}).normalize()
}

func (p RuntimeConfigDirectoryRebindApplyStatusProof) canonicalUnsigned() ([]byte, error) {
	normalized, err := p.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeConfigDirectoryRebindApplyStatusProof(proof RuntimeConfigDirectoryRebindApplyStatusProof, secret []byte) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	canonical, err := proof.canonicalUnsigned()
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	proof.Signature = hex.EncodeToString(mac.Sum(nil))
	return proof.normalize()
}

func VerifyRuntimeConfigDirectoryRebindApplyStatusProof(proof RuntimeConfigDirectoryRebindApplyStatusProof, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := proof.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeConfigDirectoryRebindApplyStatusAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeConfigDirectoryRebindApplyStatusAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeConfigDirectoryRebindApplyStatusAuth)
	}
	return nil
}

func (p RuntimeConfigDirectoryRebindApplyStatusProof) ValidateAgainst(envelope RuntimeConfigDirectoryRebindApplyEnvelope) error {
	envelope, err := envelope.normalize()
	if err != nil {
		return err
	}
	proof, err := p.normalize()
	if err != nil {
		return err
	}
	if proof.Source != envelope.Source || proof.Destination != envelope.Destination || proof.PlanID != envelope.PlanID || proof.ApplyID != envelope.ApplyID || proof.ConfirmationID != envelope.ConfirmationID || proof.ConfirmationDigest != envelope.ConfirmationDigest || proof.InvocationID != envelope.InvocationID || proof.BoundaryStatus != envelope.BoundaryStatus || proof.ConfigSnapshotDigest != envelope.ConfigSnapshotDigest || proof.SnapshotRevision != envelope.SnapshotRevision || proof.EventSequence != envelope.EventSequence || proof.SnapshotDigest != envelope.SnapshotDigest {
		return fmt.Errorf("%w: status proof identity 不一致", ErrRuntimeConfigDirectoryRebindApplyStatusAuth)
	}
	return nil
}

// RuntimeConfigDirectoryRebindApplyReconciler is an optional source
// transport capability. It may return an authenticated accepted proof without
// receiving the checkpoint projection.
type RuntimeConfigDirectoryRebindApplyReconciler interface {
	ReconcileRuntimeConfigDirectoryRebindApply(context.Context, RuntimeConfigDirectoryRebindApplyEnvelope, RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyStatusProof, error)
}

func (r *RuntimeConfigDirectoryRebindInboxRecord) matchesStatusEnvelope(envelope RuntimeConfigDirectoryRebindApplyEnvelope) bool {
	if r == nil {
		return false
	}
	return r.Version == envelope.Version && r.Source == envelope.Source && r.Destination == envelope.Destination && r.PlanID == envelope.PlanID && r.ApplyID == envelope.ApplyID && r.ConfirmationID == envelope.ConfirmationID && r.ConfirmationDigest == envelope.ConfirmationDigest && r.InvocationID == envelope.InvocationID && r.BoundaryStatus == envelope.BoundaryStatus && r.ConfigSnapshotDigest == envelope.ConfigSnapshotDigest && r.SnapshotRevision == envelope.SnapshotRevision && r.EventSequence == envelope.EventSequence && r.SnapshotDigest == envelope.SnapshotDigest
}

func (r *RuntimeConfigDirectoryRebindApplyReceiver) StatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { r.ServeStatusHTTP(writer, request) })
}

func readRuntimeConfigDirectoryRebindApplyStatusEnvelope(request *http.Request, maxBody int64) (RuntimeConfigDirectoryRebindApplyEnvelope, error) {
	if request == nil || request.Body == nil {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: request body 为空", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDirectoryRebindApplyStatusBodyBytes {
		maxBody = MaxRuntimeConfigDirectoryRebindApplyStatusBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: request body 超过上限或读取失败", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope RuntimeConfigDirectoryRebindApplyEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: request JSON 无效", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: request 包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	normalized, err := envelope.normalize()
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, err
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Rebind-Apply-Version")); header != "" && header != fmt.Sprint(normalized.Version) {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: header version 与 body 不一致", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Rebind-Apply-Signature"))); header != "" && header != normalized.Signature {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: header signature 与 body 不一致", ErrRuntimeConfigDirectoryRebindApplyStatusAuth)
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != normalized.ApplyID {
		return RuntimeConfigDirectoryRebindApplyEnvelope{}, fmt.Errorf("%w: idempotency key 与 apply_id 不一致", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	return normalized, nil
}

func (r *RuntimeConfigDirectoryRebindApplyReceiver) resolveStatus(ctx context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
	record, err := r.Inbox.GetRuntimeConfigDirectoryRebind(ctx, envelope.ApplyID)
	if err == nil {
		if !record.matchesStatusEnvelope(envelope) {
			return RuntimeConfigDirectoryRebindApplyStatusProof{}, ErrConflict
		}
		return NewRuntimeConfigDirectoryRebindApplyStatusProof(envelope, RuntimeConfigDirectoryRebindApplyStatusAccepted, true, record.ReceivedAt, time.Now().UTC())
	}
	if !errors.Is(err, ErrNotFound) {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	return NewRuntimeConfigDirectoryRebindApplyStatusProof(envelope, RuntimeConfigDirectoryRebindApplyStatusAbsent, false, time.Time{}, time.Now().UTC())
}

func (r *RuntimeConfigDirectoryRebindApplyReceiver) ServeStatusHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusInternalServerError, ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
		return
	}
	if request == nil || request.Method != http.MethodPost {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus))
		return
	}
	envelope, err := readRuntimeConfigDirectoryRebindApplyStatusEnvelope(request, r.MaxBodyBytes)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrRuntimeConfigDirectoryRebindApplyStatusAuth) {
			code = http.StatusUnauthorized
		}
		writeRuntimeConfigDirectoryRebindApplyError(writer, code, err)
		return
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDirectoryRebindApplyStatusAuth))
		return
	}
	if err := VerifyRuntimeConfigDirectoryRebindApplyEnvelope(envelope, r.SharedSecret); err != nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeConfigDirectoryRebindApplyTimestamp(envelope.IssuedAt, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusUnauthorized, err)
		return
	}
	proof, err := r.resolveStatus(request.Context(), envelope)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrConflict) {
			code = http.StatusConflict
		}
		writeRuntimeConfigDirectoryRebindApplyError(writer, code, err)
		return
	}
	proof.IssuedAt = now
	proof, err = SignRuntimeConfigDirectoryRebindApplyStatusProof(proof, r.SharedSecret)
	if err != nil {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusInternalServerError, err)
		return
	}
	encoded, err := json.Marshal(proof)
	if err != nil || len(encoded) > MaxRuntimeConfigDirectoryRebindApplyStatusBodyBytes {
		writeRuntimeConfigDirectoryRebindApplyError(writer, http.StatusInternalServerError, fmt.Errorf("%w: status proof 超过响应上限", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus))
		return
	}
	writeRuntimeConfigDirectoryRebindApplyJSON(writer, http.StatusOK, proof)
}

func (t *RuntimeConfigDirectoryRebindApplyHTTPTransport) ReconcileRuntimeConfigDirectoryRebindApply(ctx context.Context, envelope RuntimeConfigDirectoryRebindApplyEnvelope, projection RuntimeCheckpointProjection) (RuntimeConfigDirectoryRebindApplyStatusProof, error) {
	if t == nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	normalized, projection, err := normalizeRuntimeConfigDirectoryRebindApplyPair(envelope, projection)
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDirectoryRebindApplyStatusAuth)
	}
	_ = projection
	signed, err := SignRuntimeConfigDirectoryRebindApplyEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	body, err := json.Marshal(signed)
	if err != nil || len(body) > MaxRuntimeConfigDirectoryRebindApplyStatusBodyBytes {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: status request 超过字节上限", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	endpoint, err := runtimeDeliveryStatusEndpoint(t.Endpoint)
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Rebind-Apply-Version", fmt.Sprint(signed.Version))
	request.Header.Set("X-Abot-Rebind-Apply-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.ApplyID)
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("Runtime rebind apply status 请求失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, MaxRuntimeConfigDirectoryRebindApplyStatusBodyBytes+1))
	if err != nil || len(responseBody) > MaxRuntimeConfigDirectoryRebindApplyStatusBodyBytes {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: status response 超过上限或读取失败", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
			return RuntimeConfigDirectoryRebindApplyStatusProof{}, ErrRuntimeConfigDirectoryRebindApplyStatusUnavailable
		}
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: Runtime rebind apply status 返回 HTTP %d: %s", ErrRuntimeConfigDirectoryRebindApplyStatusAuth, response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var proof RuntimeConfigDirectoryRebindApplyStatusProof
	if err := decoder.Decode(&proof); err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: status proof JSON 无效", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, fmt.Errorf("%w: status proof 包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectoryRebindApplyStatus)
	}
	if err := VerifyRuntimeConfigDirectoryRebindApplyStatusProof(proof, t.SharedSecret); err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	if err := proof.ValidateAgainst(signed); err != nil {
		return RuntimeConfigDirectoryRebindApplyStatusProof{}, err
	}
	return proof, nil
}

var _ RuntimeConfigDirectoryRebindApplyReconciler = (*RuntimeConfigDirectoryRebindApplyHTTPTransport)(nil)
