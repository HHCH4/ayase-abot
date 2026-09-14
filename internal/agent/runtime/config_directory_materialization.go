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

// RuntimeConfigDirectoryMaterializer is an explicit destination capability.
// The receiver passes only a record that it has already authenticated and
// accepted into the destination catalog. Implementations must apply the
// record with their own revision/body CAS and preserve defaults/bindings.
type RuntimeConfigDirectoryMaterializer interface {
	MaterializeRuntimeConfigDirectory(context.Context, RuntimeConfigDirectoryRecord) (duplicate bool, err error)
}

// RuntimeConfigDirectoryMaterializerFunc adapts a callback for small
// embedders and tests while preserving the explicit capability boundary.
type RuntimeConfigDirectoryMaterializerFunc func(context.Context, RuntimeConfigDirectoryRecord) (bool, error)

func (f RuntimeConfigDirectoryMaterializerFunc) MaterializeRuntimeConfigDirectory(ctx context.Context, record RuntimeConfigDirectoryRecord) (bool, error) {
	return f(ctx, record)
}

var (
	ErrInvalidRuntimeConfigDirectoryMaterialization     = errors.New("Runtime 配置目录 materialization 无效")
	ErrRuntimeConfigDirectoryMaterializationUnavailable = errors.New("Runtime 配置目录 materialization 未启用")
)

// RuntimeConfigDirectoryMaterializationReceipt proves that the destination
// applied (or already had) the exact accepted catalog record. It is
// metadata-only and never returns the profile/persona body.
type RuntimeConfigDirectoryMaterializationReceipt struct {
	Version     int                        `json:"version"`
	Source      string                     `json:"source"`
	Destination string                     `json:"destination"`
	DeliveryID  string                     `json:"delivery_id"`
	Kind        RuntimeConfigDirectoryKind `json:"kind"`
	EntryID     string                     `json:"entry_id"`
	Revision    int                        `json:"revision"`
	BodyDigest  string                     `json:"body_digest"`
	Duplicate   bool                       `json:"duplicate"`
	AppliedAt   time.Time                  `json:"applied_at"`
	Signature   string                     `json:"signature,omitempty"`
}

func (r RuntimeConfigDirectoryMaterializationReceipt) normalize() (RuntimeConfigDirectoryMaterializationReceipt, error) {
	r.Source = strings.TrimSpace(r.Source)
	r.Destination = strings.TrimSpace(r.Destination)
	r.DeliveryID = strings.TrimSpace(r.DeliveryID)
	r.Kind = RuntimeConfigDirectoryKind(strings.TrimSpace(strings.ToLower(string(r.Kind))))
	r.EntryID = strings.TrimSpace(r.EntryID)
	r.BodyDigest = strings.TrimSpace(strings.ToLower(r.BodyDigest))
	r.Signature = strings.TrimSpace(strings.ToLower(r.Signature))
	if r.Version != RuntimeConfigDirectoryEnvelopeVersion || r.Source == "" || r.Destination == "" || r.DeliveryID == "" || !r.Kind.valid() || r.EntryID == "" || r.Revision <= 0 || !runtimeConfigDirectoryDigestValid(r.BodyDigest) || r.AppliedAt.IsZero() {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: receipt metadata 不完整", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	if len(r.Source) > MaxRuntimeConfigDirectorySourceLength || len(r.Destination) > MaxRuntimeConfigDirectoryTargetLength || len(r.DeliveryID) > MaxRuntimeConfigDirectoryDeliveryID || len(r.EntryID) > MaxRuntimeConfigDirectoryIDLength {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: receipt metadata 超出长度限制", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	if err := validateRuntimeConfigDirectoryID(r.EntryID); err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: %v", ErrInvalidRuntimeConfigDirectoryMaterialization, err)
	}
	if r.Signature != "" && !isRuntimeEventDeliveryDigest(r.Signature) {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: receipt signature 无效", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	r.AppliedAt = r.AppliedAt.UTC()
	return r, nil
}

func (r RuntimeConfigDirectoryMaterializationReceipt) Normalize() (RuntimeConfigDirectoryMaterializationReceipt, error) {
	return r.normalize()
}

func NewRuntimeConfigDirectoryMaterializationReceipt(record RuntimeConfigDirectoryRecord, duplicate bool, appliedAt time.Time) (RuntimeConfigDirectoryMaterializationReceipt, error) {
	envelope, entry, err := NormalizeRuntimeConfigDirectoryPair(record.Envelope, record.Entry)
	if err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	if appliedAt.IsZero() {
		appliedAt = time.Now().UTC()
	} else {
		appliedAt = appliedAt.UTC()
	}
	return (RuntimeConfigDirectoryMaterializationReceipt{
		Version: envelope.Version, Source: envelope.Source, Destination: envelope.Destination,
		DeliveryID: envelope.DeliveryID, Kind: entry.Kind, EntryID: entry.ID, Revision: entry.Revision,
		BodyDigest: envelope.BodyDigest, Duplicate: duplicate, AppliedAt: appliedAt,
	}).normalize()
}

func (r RuntimeConfigDirectoryMaterializationReceipt) canonicalUnsigned() ([]byte, error) {
	normalized, err := r.normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeConfigDirectoryMaterializationReceipt(receipt RuntimeConfigDirectoryMaterializationReceipt, secret []byte) (RuntimeConfigDirectoryMaterializationReceipt, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	canonical, err := receipt.canonicalUnsigned()
	if err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	mac := hmacSHA256(secret, canonical)
	receipt.Signature = mac
	return receipt.normalize()
}

func VerifyRuntimeConfigDirectoryMaterializationReceipt(receipt RuntimeConfigDirectoryMaterializationReceipt, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := receipt.normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeConfigDirectoryMaterializationUnavailable)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	if !verifyHMACSHA256(secret, canonical, normalized.Signature) {
		return fmt.Errorf("%w: receipt HMAC 不匹配", ErrRuntimeConfigDirectoryMaterializationUnavailable)
	}
	return nil
}

func (r RuntimeConfigDirectoryMaterializationReceipt) ValidateAgainst(record RuntimeConfigDirectoryRecord) error {
	envelope, entry, err := NormalizeRuntimeConfigDirectoryPair(record.Envelope, record.Entry)
	if err != nil {
		return err
	}
	receipt, err := r.normalize()
	if err != nil {
		return err
	}
	if receipt.Version != envelope.Version || receipt.Source != envelope.Source || receipt.Destination != envelope.Destination || receipt.DeliveryID != envelope.DeliveryID || receipt.Kind != entry.Kind || receipt.EntryID != entry.ID || receipt.Revision != entry.Revision || receipt.BodyDigest != envelope.BodyDigest {
		return fmt.Errorf("%w: receipt metadata 不一致", ErrRuntimeConfigDirectoryMaterializationUnavailable)
	}
	return nil
}

func (r *RuntimeConfigDirectoryReceiver) SetMaterializer(materializer RuntimeConfigDirectoryMaterializer) error {
	if r == nil {
		return fmt.Errorf("%w: receiver 为空", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	r.Materializer = materializer
	return nil
}

func (r *RuntimeConfigDirectoryReceiver) MaterializeHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeMaterializeHTTP(writer, request)
	})
}

func (r *RuntimeConfigDirectoryReceiver) ServeMaterializeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusInternalServerError, map[string]string{"error": ErrInvalidRuntimeConfigDirectoryMaterialization.Error()})
		return
	}
	if request == nil || request.Method != http.MethodPost {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	envelope, err := readRuntimeConfigDirectoryEnvelope(request, MaxRuntimeConfigDirectoryEnvelopeBytes)
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusBadRequest, map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	normalized, err := envelope.Normalize()
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusBadRequest, map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	if err := r.authenticateMaterializationEnvelope(request, normalized); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrInvalidRuntimeConfigDirectory) || errors.Is(err, ErrInvalidRuntimeConfigDirectoryMaterialization) {
			status = http.StatusBadRequest
		}
		writeRuntimeConfigDirectoryJSON(writer, status, map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	if r.Materializer == nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusNotImplemented, map[string]string{"error": ErrRuntimeConfigDirectoryMaterializationUnavailable.Error()})
		return
	}
	record, getErr := r.Repository.GetRuntimeConfigDirectory(request.Context(), normalized.Kind, normalized.EntryID)
	if errors.Is(getErr, ErrNotFound) {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusNotFound, map[string]string{"error": ErrNotFound.Error()})
		return
	}
	if getErr != nil {
		writeRuntimeConfigDirectoryJSON(writer, runtimeConfigDirectoryHTTPStatus(getErr), map[string]string{"error": SanitizeRuntimeString(getErr.Error())})
		return
	}
	if !runtimeConfigDirectoryRecordMatches(record, normalized, record.Entry) {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusConflict, map[string]string{"error": ErrRuntimeConfigDirectoryConflict.Error()})
		return
	}
	// Do not let an embedder mutate the handler's local map/pointer copy and
	// invalidate the receipt identity after the authenticated lookup.
	materializationRecord := cloneRuntimeConfigDirectoryRecord(record)
	duplicate, err := r.Materializer.MaterializeRuntimeConfigDirectory(request.Context(), materializationRecord)
	if err != nil {
		status := runtimeConfigDirectoryHTTPStatus(err)
		if errors.Is(err, ErrRuntimeConfigDirectoryMaterializationUnavailable) {
			status = http.StatusNotImplemented
		}
		writeRuntimeConfigDirectoryJSON(writer, status, map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	receipt, err := NewRuntimeConfigDirectoryMaterializationReceipt(record, duplicate, now)
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusInternalServerError, map[string]string{"error": ErrInvalidRuntimeConfigDirectoryMaterialization.Error()})
		return
	}
	signed, err := SignRuntimeConfigDirectoryMaterializationReceipt(receipt, r.SharedSecret)
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusInternalServerError, map[string]string{"error": ErrInvalidRuntimeConfigDirectoryMaterialization.Error()})
		return
	}
	writeRuntimeConfigDirectoryJSON(writer, http.StatusOK, signed)
}

func (r *RuntimeConfigDirectoryReceiver) authenticateMaterializationEnvelope(request *http.Request, envelope RuntimeConfigDirectoryEnvelope) error {
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		return fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDirectoryMaterializationUnavailable)
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Config-Directory-Version")); header != "" && header != fmt.Sprintf("%d", envelope.Version) {
		return fmt.Errorf("%w: version header 与 body 不一致", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Config-Directory-Signature"))); header != "" && header != envelope.Signature {
		return fmt.Errorf("%w: signature header 与 body 不一致", ErrRuntimeConfigDirectoryMaterializationUnavailable)
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != envelope.DeliveryID {
		return fmt.Errorf("%w: idempotency key 与 delivery_id 不一致", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	if err := VerifyRuntimeConfigDirectoryEnvelope(envelope, r.SharedSecret); err != nil {
		return err
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	return validateRuntimeConfigDirectoryTimestamp(envelope.IssuedAt, now, r.MaxAge, r.MaxFutureSkew)
}

func (t *RuntimeConfigDirectoryHTTPTransport) MaterializeRuntimeConfigDirectory(ctx context.Context, record RuntimeConfigDirectoryRecord) (RuntimeConfigDirectoryMaterializationReceipt, error) {
	if t == nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, ErrRuntimeConfigDirectoryMaterializationUnavailable
	}
	envelope, entry, err := NormalizeRuntimeConfigDirectoryPair(record.Envelope, record.Entry)
	if err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	if envelope.Source != t.Source || envelope.Destination != t.Destination {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDirectoryMaterializationUnavailable)
	}
	_ = entry
	signed, err := SignRuntimeConfigDirectoryEnvelope(envelope, t.SharedSecret)
	if err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	body, err := json.Marshal(signed)
	if err != nil || len(body) > MaxRuntimeConfigDirectoryEnvelopeBytes {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: materialize request 编码超过上限", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	endpoint, err := runtimeConfigDirectorySubEndpoint(t.Endpoint, "materialize", ErrInvalidRuntimeConfigDirectoryMaterialization)
	if err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Config-Directory-Version", fmt.Sprintf("%d", signed.Version))
	request.Header.Set("X-Abot-Config-Directory-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: materialize 请求失败: %v", ErrRuntimeConfigDirectoryMaterializationUnavailable, err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDirectoryResponseBytes {
		maxBody = MaxRuntimeConfigDirectoryResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: materialize response 超过上限或读取失败", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented || response.StatusCode == http.StatusServiceUnavailable {
			return RuntimeConfigDirectoryMaterializationReceipt{}, ErrRuntimeConfigDirectoryMaterializationUnavailable
		}
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("materialize 返回 HTTP %d: %s", response.StatusCode, SanitizeRuntimeString(strings.TrimSpace(string(responseBody))))
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeConfigDirectoryMaterializationReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: materialization receipt JSON 无效", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDirectoryMaterializationReceipt{}, fmt.Errorf("%w: materialization receipt 包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	if err := VerifyRuntimeConfigDirectoryMaterializationReceipt(receipt, t.SharedSecret); err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	if err := receipt.ValidateAgainst(record); err != nil {
		return RuntimeConfigDirectoryMaterializationReceipt{}, err
	}
	return receipt, nil
}

func runtimeConfigDirectorySubEndpoint(base, suffix string, invalid error) (string, error) {
	endpoint, err := runtimeConfigDirectoryStatusEndpoint(base)
	if err != nil {
		return "", fmt.Errorf("%w: endpoint 无效", invalid)
	}
	endpoint = strings.TrimSuffix(endpoint, "/status") + "/" + strings.Trim(strings.TrimSpace(suffix), "/")
	return endpoint, nil
}

// Keep the function-local HMAC implementation in one place while retaining
// the existing envelope helpers' exact hexadecimal wire format.
func hmacSHA256(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyHMACSHA256(secret, payload []byte, provided string) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(strings.ToLower(provided)))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	return hmac.Equal(decoded, mac.Sum(nil))
}
