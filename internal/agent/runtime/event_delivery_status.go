package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// StatusHandler exposes an authenticated destination-side status query. The
// request reuses the signed event envelope, while the response is separately
// signed so an intermediary cannot forge an accepted/committed proof.
func (r *RuntimeEventDeliveryReceiver) StatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeStatusHTTP(writer, request)
	})
}

func (r *RuntimeEventDeliveryReceiver) ServeStatusHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeEventDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeEventDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeEventDelivery))
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
	if err := validateRuntimeEventDeliveryTimestamp(RuntimeEventDeliveryReplayTimestamp(envelope), timeNowRuntimeEventDeliveryReceiver(r), r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	status, err := r.resolveEventDeliveryStatus(request.Context(), envelope)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrConflict):
			code = http.StatusConflict
		case errors.Is(err, ErrRuntimeDeliveryStatusUnavailable):
			code = http.StatusNotImplemented
		}
		writeRuntimeEventDeliveryError(writer, code, err)
		return
	}
	signed, err := SignRuntimeDeliveryStatus(status, r.SharedSecret)
	if err != nil {
		writeRuntimeEventDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	writeRuntimeEventDeliveryJSON(writer, http.StatusOK, signed)
}

func (r *RuntimeEventDeliveryReceiver) resolveEventDeliveryStatus(ctx context.Context, envelope RuntimeEventDeliveryEnvelope) (RuntimeDeliveryStatus, error) {
	txnReader, hasTxnReader := r.Inbox.(interface {
		GetRuntimeEventDeliveryTransaction(context.Context, string) (RuntimeEventDeliveryTransaction, error)
	})
	if hasTxnReader {
		transaction, err := txnReader.GetRuntimeEventDeliveryTransaction(ctx, envelope.DeliveryID)
		if err == nil {
			if !transaction.Matches(envelope) {
				return RuntimeDeliveryStatus{}, ErrConflict
			}
			switch transaction.Status {
			case RuntimeEventDeliveryTransactionCommitted:
				return NewRuntimeDeliveryStatus(RuntimeDeliveryKindEvent, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusCommitted, true, transaction.UpdatedAt)
			case RuntimeEventDeliveryTransactionPrepared:
				return NewRuntimeDeliveryStatus(RuntimeDeliveryKindEvent, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusPrepared, true, transaction.UpdatedAt)
			case RuntimeEventDeliveryTransactionAborted:
				// An aborted transaction is not a destination proof. Continue to
				// the visible inbox in case a one-phase retry already published it.
			}
		} else if !errors.Is(err, ErrNotFound) {
			return RuntimeDeliveryStatus{}, err
		}
	}

	reader, ok := r.Inbox.(RuntimeEventDeliveryInboxReader)
	if !ok {
		return RuntimeDeliveryStatus{}, ErrRuntimeDeliveryStatusUnavailable
	}
	record, err := reader.GetRuntimeEventDelivery(ctx, envelope.EventID)
	if err == nil {
		if !record.Matches(envelope) {
			return RuntimeDeliveryStatus{}, ErrConflict
		}
		return NewRuntimeDeliveryStatus(RuntimeDeliveryKindEvent, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAccepted, true, record.ReceivedAt)
	}
	if !errors.Is(err, ErrNotFound) {
		return RuntimeDeliveryStatus{}, err
	}
	return NewRuntimeDeliveryStatus(RuntimeDeliveryKindEvent, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAbsent, false, time.Time{})
}

func (t *RuntimeEventDeliveryHTTPTransport) ReconcileEventDelivery(ctx context.Context, outbox RuntimeEventOutbox, event AgentEvent) (RuntimeDeliveryStatus, error) {
	if t == nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeDeliveryStatus)
	}
	issuedAt := time.Now().UTC()
	if t.Now != nil {
		issuedAt = t.Now().UTC()
	}
	envelope, err := t.BuildEnvelopeAt(outbox, event, issuedAt)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	signedEnvelope, err := SignRuntimeEventDeliveryEnvelope(envelope, t.SharedSecret)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	body, err := json.Marshal(signedEnvelope)
	if err != nil || len(body) > MaxRuntimeEventDeliveryEnvelopeBytes {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: status request 编码超过上限", ErrInvalidRuntimeDeliveryStatus)
	}
	endpoint, err := runtimeDeliveryStatusEndpoint(t.Endpoint)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Event-Delivery-Version", fmt.Sprint(signedEnvelope.Version))
	request.Header.Set("X-Abot-Event-Delivery-Signature", signedEnvelope.Signature)
	request.Header.Set("Idempotency-Key", signedEnvelope.EventID)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("Runtime event delivery status 请求失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, MaxRuntimeDeliveryStatusBodyBytes+1))
	if err != nil || len(responseBody) > MaxRuntimeDeliveryStatusBodyBytes {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: status response 超过上限或读取失败", ErrInvalidRuntimeDeliveryStatus)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
			return RuntimeDeliveryStatus{}, ErrRuntimeDeliveryStatusUnavailable
		}
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		if message == "" {
			message = "receiver returned no diagnostic"
		}
		return RuntimeDeliveryStatus{}, fmt.Errorf("Runtime event delivery status 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var status RuntimeDeliveryStatus
	if err := decoder.Decode(&status); err != nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: status JSON 无效", ErrInvalidRuntimeDeliveryStatus)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: status 包含多个 JSON 文档", ErrInvalidRuntimeDeliveryStatus)
	}
	if err := VerifyRuntimeDeliveryStatus(status, t.SharedSecret); err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	if !status.Matches(RuntimeDeliveryKindEvent, t.Source, t.Destination, signedEnvelope.DeliveryID, signedEnvelope.InvocationID) {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: status metadata 不一致 kind=%q source=%q destination=%q delivery_id=%q invocation_id=%q", ErrRuntimeDeliveryStatusAuth, status.Kind, status.Source, status.Destination, status.DeliveryID, status.InvocationID)
	}
	return status, nil
}

var _ RuntimeEventDeliveryReconciler = (*RuntimeEventDeliveryHTTPTransport)(nil)
