package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (r *RuntimeCheckpointDeliveryReceiver) PrepareHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeCheckpointDeliveryTransactionPhasePrepared)
	})
}

func (r *RuntimeCheckpointDeliveryReceiver) CommitHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeCheckpointDeliveryTransactionPhaseCommitted)
	})
}

// AbortHandler exposes the authenticated compensation endpoint for a
// prepared checkpoint transaction.
func (r *RuntimeCheckpointDeliveryReceiver) AbortHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeCheckpointDeliveryTransactionPhaseAborted)
	})
}

// ServeTransactionHTTP implements the authenticated prepare/commit endpoints
// for a transactional receiver. It intentionally shares the signed envelope
// and projection wire shape with one-phase delivery while keeping the phase in
// the route and response, so a proxy cannot silently turn prepare into commit.
func (r *RuntimeCheckpointDeliveryReceiver) ServeTransactionHTTP(writer http.ResponseWriter, request *http.Request, phase string) {
	if r == nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeCheckpointDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	if phase != RuntimeCheckpointDeliveryTransactionPhasePrepared && phase != RuntimeCheckpointDeliveryTransactionPhaseCommitted && phase != RuntimeCheckpointDeliveryTransactionPhaseAborted {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: transaction phase 无效", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Checkpoint-Transaction-Phase")); header != phase {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: transaction phase header 与 route 不一致", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	txnRepo, ok := r.Inbox.(RuntimeCheckpointDeliveryTransactionRepository)
	if !ok {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver 未启用 transaction repository", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	payload, err := readRuntimeCheckpointDeliveryPayload(request, r.MaxBodyBytes)
	if err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	envelope, projection, err := NormalizeRuntimeCheckpointDeliveryPair(payload.Envelope, payload.Projection)
	if err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusBadRequest, err)
		return
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeCheckpointDeliveryAuth))
		return
	}
	if err := VerifyRuntimeCheckpointDeliveryEnvelope(envelope, r.SharedSecret); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	now := timeNowRuntimeCheckpointReceiver(r)
	if err := validateRuntimeCheckpointDeliveryTimestamp(envelope.Timestamp, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	var duplicate bool
	switch phase {
	case RuntimeCheckpointDeliveryTransactionPhasePrepared:
		duplicate, err = txnRepo.PrepareRuntimeCheckpointDelivery(request.Context(), envelope, projection)
	case RuntimeCheckpointDeliveryTransactionPhaseCommitted:
		duplicate, err = txnRepo.CommitRuntimeCheckpointDelivery(request.Context(), envelope, projection)
	case RuntimeCheckpointDeliveryTransactionPhaseAborted:
		aborter, abortOK := r.Inbox.(RuntimeCheckpointDeliveryAbortableTransactionRepository)
		if !abortOK {
			writeRuntimeCheckpointDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver 未启用 transaction abort repository", ErrInvalidRuntimeCheckpointDelivery))
			return
		}
		duplicate, err = aborter.AbortRuntimeCheckpointDelivery(request.Context(), envelope, projection)
	}
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		if errors.Is(err, ErrInvalidRuntimeCheckpointDelivery) || errors.Is(err, ErrRuntimeCheckpointDeliveryAuth) {
			status = http.StatusBadRequest
		}
		writeRuntimeCheckpointDeliveryError(writer, status, err)
		return
	}
	receipt := RuntimeCheckpointDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, InvocationID: envelope.InvocationID,
		SnapshotRevision: envelope.SnapshotRevision, EventSequence: envelope.EventSequence,
		SnapshotDigest: envelope.SnapshotDigest, Phase: phase, Duplicate: duplicate,
	}
	writeRuntimeCheckpointDeliveryJSON(writer, http.StatusOK, receipt)
}

func timeNowRuntimeCheckpointReceiver(receiver *RuntimeCheckpointDeliveryReceiver) time.Time {
	now := time.Now().UTC()
	if receiver != nil && receiver.Now != nil {
		now = receiver.Now().UTC()
	}
	return now
}

func readRuntimeCheckpointDeliveryPayload(request *http.Request, configuredMax int64) (RuntimeCheckpointDeliverySourceResponse, error) {
	if request == nil || request.Body == nil {
		return RuntimeCheckpointDeliverySourceResponse{}, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeCheckpointDeliveryResponseBytes {
		maxBody = MaxRuntimeCheckpointDeliveryResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil {
		return RuntimeCheckpointDeliverySourceResponse{}, fmt.Errorf("%w: 读取请求失败", ErrInvalidRuntimeCheckpointDelivery)
	}
	if int64(len(body)) > maxBody {
		return RuntimeCheckpointDeliverySourceResponse{}, fmt.Errorf("%w: 请求超过 %d 字节上限", ErrInvalidRuntimeCheckpointDelivery, maxBody)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload RuntimeCheckpointDeliverySourceResponse
	if err := decoder.Decode(&payload); err != nil {
		return RuntimeCheckpointDeliverySourceResponse{}, fmt.Errorf("%w: request JSON 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeCheckpointDeliverySourceResponse{}, fmt.Errorf("%w: request 包含多个 JSON 文档", ErrInvalidRuntimeCheckpointDelivery)
	}
	return payload, nil
}

func transactionalCheckpointEndpoint(base, phase string) (string, error) {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeCheckpointDelivery)
	}
	pathSegment := ""
	switch phase {
	case RuntimeCheckpointDeliveryTransactionPhasePrepared:
		pathSegment = "prepare"
	case RuntimeCheckpointDeliveryTransactionPhaseCommitted:
		pathSegment = "commit"
	case RuntimeCheckpointDeliveryTransactionPhaseAborted:
		pathSegment = "abort"
	default:
		return "", fmt.Errorf("%w: transaction phase 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + pathSegment
	return u.String(), nil
}

func (t *RuntimeCheckpointDeliveryHTTPTransport) Prepare(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	return t.deliverTransactionPhase(ctx, envelope, projection, RuntimeCheckpointDeliveryTransactionPhasePrepared)
}

func (t *RuntimeCheckpointDeliveryHTTPTransport) Commit(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	return t.deliverTransactionPhase(ctx, envelope, projection, RuntimeCheckpointDeliveryTransactionPhaseCommitted)
}

func (t *RuntimeCheckpointDeliveryHTTPTransport) Abort(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
	return t.deliverTransactionPhase(ctx, envelope, projection, RuntimeCheckpointDeliveryTransactionPhaseAborted)
}

func (t *RuntimeCheckpointDeliveryHTTPTransport) deliverTransactionPhase(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection, phase string) (RuntimeCheckpointDeliveryReceipt, error) {
	if t == nil {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeCheckpointDelivery)
	}
	normalized, err := NormalizeRuntimeCheckpointDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeCheckpointDeliveryAuth)
	}
	if err := projection.Validate(); err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	if projection.InvocationID != normalized.InvocationID || projection.SnapshotRevision != normalized.SnapshotRevision || projection.EventSequence != normalized.EventSequence {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: projection metadata 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil || digest != normalized.SnapshotDigest {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: projection digest 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	signed, err := SignRuntimeCheckpointDeliveryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	body, err := json.Marshal(RuntimeCheckpointDeliverySourceResponse{Envelope: signed, Projection: projection})
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: request 编码失败", ErrInvalidRuntimeCheckpointDelivery)
	}
	if len(body) > MaxRuntimeCheckpointDeliveryResponseBytes {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: request 超过字节上限", ErrInvalidRuntimeCheckpointDelivery)
	}
	endpoint, err := transactionalCheckpointEndpoint(t.Endpoint, phase)
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Checkpoint-Delivery-Version", fmt.Sprint(signed.Version))
	request.Header.Set("X-Abot-Checkpoint-Delivery-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	request.Header.Set("X-Abot-Checkpoint-Transaction-Phase", phase)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("Runtime checkpoint %s 请求失败: %w", phase, err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeCheckpointDeliveryResponseBytes {
		maxBody = MaxRuntimeCheckpointDeliveryResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: response 超过字节上限或读取失败", ErrInvalidRuntimeCheckpointDelivery)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("Runtime checkpoint %s 返回 HTTP %d: %s", phase, response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeCheckpointDeliveryReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: receipt JSON 无效", ErrInvalidRuntimeCheckpointDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeCheckpointDeliveryReceipt{}, fmt.Errorf("%w: receipt 包含多个 JSON 文档", ErrInvalidRuntimeCheckpointDelivery)
	}
	if err := receipt.ValidateAgainstPhase(signed, phase); err != nil {
		return RuntimeCheckpointDeliveryReceipt{}, err
	}
	return receipt, nil
}

var _ RuntimeCheckpointDeliveryTransactionalTransport = (*RuntimeCheckpointDeliveryHTTPTransport)(nil)
var _ RuntimeCheckpointDeliveryAbortableTransport = (*RuntimeCheckpointDeliveryHTTPTransport)(nil)
