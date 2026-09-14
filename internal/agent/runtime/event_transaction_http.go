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

func (r *RuntimeEventDeliveryReceiver) PrepareHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhasePrepared)
	})
}

func (r *RuntimeEventDeliveryReceiver) CommitHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhaseCommitted)
	})
}

// AbortHandler exposes the authenticated compensation endpoint for a
// prepared event transaction.
func (r *RuntimeEventDeliveryReceiver) AbortHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeTransactionHTTP(writer, request, RuntimeEventDeliveryTransactionPhaseAborted)
	})
}

// ServeTransactionHTTP implements authenticated prepare/commit endpoints for
// an event receiver. The phase is bound both to the route selected by the
// caller and to an explicit header, so a proxy cannot silently turn prepare
// into commit.
func (r *RuntimeEventDeliveryReceiver) ServeTransactionHTTP(writer http.ResponseWriter, request *http.Request, phase string) {
	if r == nil {
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeEventDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeEventDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeEventDelivery))
		return
	}
	if phase != RuntimeEventDeliveryTransactionPhasePrepared && phase != RuntimeEventDeliveryTransactionPhaseCommitted && phase != RuntimeEventDeliveryTransactionPhaseAborted {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: transaction phase 无效", ErrInvalidRuntimeEventDelivery))
		return
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Event-Transaction-Phase")); header != phase {
		writeRuntimeEventDeliveryError(writer, http.StatusBadRequest, fmt.Errorf("%w: transaction phase header 与 route 不一致", ErrInvalidRuntimeEventDelivery))
		return
	}
	txnRepo, ok := r.Inbox.(RuntimeEventDeliveryTransactionRepository)
	if !ok {
		writeRuntimeEventDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver 未启用 transaction repository", ErrInvalidRuntimeEventDelivery))
		return
	}
	envelope, err := readRuntimeEventDeliveryEnvelope(request, r.MaxBodyBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrRuntimeEventDeliveryAuth) {
			status = http.StatusUnauthorized
		}
		writeRuntimeEventDeliveryError(writer, status, err)
		return
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		writeRuntimeEventDeliveryError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeEventDeliveryAuth))
		return
	}
	if err := VerifyRuntimeEventDeliveryEnvelope(envelope, r.SharedSecret); err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	now := timeNowRuntimeEventDeliveryReceiver(r)
	if err := validateRuntimeEventDeliveryTimestamp(RuntimeEventDeliveryReplayTimestamp(envelope), now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	var duplicate bool
	switch phase {
	case RuntimeEventDeliveryTransactionPhasePrepared:
		duplicate, err = txnRepo.PrepareRuntimeEventDelivery(request.Context(), envelope)
	case RuntimeEventDeliveryTransactionPhaseCommitted:
		duplicate, err = txnRepo.CommitRuntimeEventDelivery(request.Context(), envelope)
	case RuntimeEventDeliveryTransactionPhaseAborted:
		aborter, abortOK := r.Inbox.(RuntimeEventDeliveryAbortableTransactionRepository)
		if !abortOK {
			writeRuntimeEventDeliveryError(writer, http.StatusNotImplemented, fmt.Errorf("%w: receiver 未启用 transaction abort repository", ErrInvalidRuntimeEventDelivery))
			return
		}
		duplicate, err = aborter.AbortRuntimeEventDelivery(request.Context(), envelope)
	}
	if err != nil {
		status := http.StatusConflict
		switch {
		case errors.Is(err, ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrInvalidRuntimeEventDelivery):
			status = http.StatusBadRequest
		case errors.Is(err, ErrRuntimeEventDeliveryAuth):
			status = http.StatusUnauthorized
		}
		writeRuntimeEventDeliveryError(writer, status, err)
		return
	}
	receipt := RuntimeEventDeliveryReceipt{
		Version: envelope.Version, DeliveryID: envelope.DeliveryID, EventID: envelope.EventID,
		InvocationID: envelope.InvocationID, Sequence: envelope.Sequence, Type: envelope.Type,
		EventDigest: envelope.EventDigest, Phase: phase, Duplicate: duplicate,
	}
	writeRuntimeEventDeliveryJSON(writer, http.StatusOK, receipt)
}

func timeNowRuntimeEventDeliveryReceiver(receiver *RuntimeEventDeliveryReceiver) time.Time {
	now := time.Now().UTC()
	if receiver != nil && receiver.Now != nil {
		now = receiver.Now().UTC()
	}
	return now
}

func transactionalRuntimeEventDeliveryEndpoint(base, phase string) (string, error) {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeEventDelivery)
	}
	segment := ""
	switch phase {
	case RuntimeEventDeliveryTransactionPhasePrepared:
		segment = "prepare"
	case RuntimeEventDeliveryTransactionPhaseCommitted:
		segment = "commit"
	case RuntimeEventDeliveryTransactionPhaseAborted:
		segment = "abort"
	default:
		return "", fmt.Errorf("%w: transaction phase 无效", ErrInvalidRuntimeEventDelivery)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + segment
	return u.String(), nil
}

func (t *RuntimeEventDeliveryHTTPTransport) Prepare(ctx context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	return t.deliverTransactionPhase(ctx, envelope, RuntimeEventDeliveryTransactionPhasePrepared)
}

func (t *RuntimeEventDeliveryHTTPTransport) Commit(ctx context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	return t.deliverTransactionPhase(ctx, envelope, RuntimeEventDeliveryTransactionPhaseCommitted)
}

func (t *RuntimeEventDeliveryHTTPTransport) Abort(ctx context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeEventDeliveryReceipt, error) {
	return t.deliverTransactionPhase(ctx, envelope, RuntimeEventDeliveryTransactionPhaseAborted)
}

func (t *RuntimeEventDeliveryHTTPTransport) deliverTransactionPhase(ctx context.Context, envelope RuntimeEventDeliveryEnvelope, phase string) (RuntimeEventDeliveryReceipt, error) {
	if t == nil {
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeEventDelivery)
	}
	normalized, err := NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeEventDeliveryReceipt{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeEventDeliveryAuth)
	}
	signed, err := SignRuntimeEventDeliveryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeEventDeliveryReceipt{}, err
	}
	body, err := json.Marshal(signed)
	if err != nil {
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("%w: request 编码失败", ErrInvalidRuntimeEventDelivery)
	}
	if len(body) > MaxRuntimeEventDeliveryEnvelopeBytes {
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("%w: request 超过字节上限", ErrInvalidRuntimeEventDelivery)
	}
	endpoint, err := transactionalRuntimeEventDeliveryEndpoint(t.Endpoint, phase)
	if err != nil {
		return RuntimeEventDeliveryReceipt{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeEventDeliveryReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Event-Delivery-Version", fmt.Sprint(signed.Version))
	request.Header.Set("X-Abot-Event-Delivery-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.EventID)
	request.Header.Set("X-Abot-Event-Transaction-Phase", phase)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("Runtime event %s 请求失败: %w", phase, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, MaxRuntimeEventDeliveryEnvelopeBytes+1))
	if err != nil || len(responseBody) > MaxRuntimeEventDeliveryEnvelopeBytes {
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("%w: response 超过字节上限或读取失败", ErrInvalidRuntimeEventDelivery)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		if message == "" {
			message = "receiver returned no diagnostic"
		}
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("Runtime event %s 返回 HTTP %d: %s", phase, response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeEventDeliveryReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("%w: receipt JSON 无效", ErrInvalidRuntimeEventDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeEventDeliveryReceipt{}, fmt.Errorf("%w: receipt 包含多个 JSON 文档", ErrInvalidRuntimeEventDelivery)
	}
	if err := receipt.ValidateAgainstPhase(signed, phase); err != nil {
		return RuntimeEventDeliveryReceipt{}, err
	}
	return receipt, nil
}

var _ RuntimeEventDeliveryTransactionalTransport = (*RuntimeEventDeliveryHTTPTransport)(nil)
var _ RuntimeEventDeliveryAbortableTransport = (*RuntimeEventDeliveryHTTPTransport)(nil)
