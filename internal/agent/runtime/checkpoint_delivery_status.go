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

func (r *RuntimeCheckpointDeliveryReceiver) StatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeStatusHTTP(writer, request)
	})
}

func (r *RuntimeCheckpointDeliveryReceiver) ServeStatusHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeCheckpointDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeCheckpointDelivery))
		return
	}
	envelope, err := readRuntimeCheckpointDeliveryEnvelope(request, r.MaxBodyBytes)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrRuntimeCheckpointDeliveryAuth) {
			code = http.StatusUnauthorized
		}
		writeRuntimeCheckpointDeliveryError(writer, code, err)
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
	if err := validateRuntimeCheckpointDeliveryTimestamp(envelope.Timestamp, timeNowRuntimeCheckpointReceiver(r), r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	status, err := r.resolveCheckpointDeliveryStatus(request.Context(), envelope)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrConflict):
			code = http.StatusConflict
		case errors.Is(err, ErrRuntimeDeliveryStatusUnavailable):
			code = http.StatusNotImplemented
		}
		writeRuntimeCheckpointDeliveryError(writer, code, err)
		return
	}
	signed, err := SignRuntimeDeliveryStatus(status, r.SharedSecret)
	if err != nil {
		writeRuntimeCheckpointDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	writeRuntimeCheckpointDeliveryJSON(writer, http.StatusOK, signed)
}

func runtimeCheckpointTransactionMatchesIdentity(transaction RuntimeCheckpointDeliveryTransaction, envelope RuntimeCheckpointDeliveryEnvelope) bool {
	return transaction.Version == envelope.Version && transaction.DeliveryID == envelope.DeliveryID && transaction.Source == envelope.Source && transaction.Destination == envelope.Destination && transaction.InvocationID == envelope.InvocationID && transaction.SnapshotRevision == envelope.SnapshotRevision && transaction.EventSequence == envelope.EventSequence && transaction.SnapshotDigest == envelope.SnapshotDigest
}

func (r *RuntimeCheckpointDeliveryReceiver) resolveCheckpointDeliveryStatus(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope) (RuntimeDeliveryStatus, error) {
	txnReader, hasTxnReader := r.Inbox.(interface {
		GetRuntimeCheckpointDeliveryTransaction(context.Context, string) (RuntimeCheckpointDeliveryTransaction, error)
	})
	if hasTxnReader {
		transaction, err := txnReader.GetRuntimeCheckpointDeliveryTransaction(ctx, envelope.DeliveryID)
		if err == nil {
			if !runtimeCheckpointTransactionMatchesIdentity(transaction, envelope) {
				return RuntimeDeliveryStatus{}, ErrConflict
			}
			switch transaction.Status {
			case RuntimeCheckpointDeliveryTransactionCommitted:
				return NewRuntimeDeliveryStatus(RuntimeDeliveryKindCheckpoint, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusCommitted, true, transaction.UpdatedAt)
			case RuntimeCheckpointDeliveryTransactionPrepared:
				return NewRuntimeDeliveryStatus(RuntimeDeliveryKindCheckpoint, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusPrepared, true, transaction.UpdatedAt)
			case RuntimeCheckpointDeliveryTransactionAborted:
				// Continue to the visible inbox; an aborted prepare is not a proof.
			}
		} else if !errors.Is(err, ErrNotFound) {
			return RuntimeDeliveryStatus{}, err
		}
	}
	record, err := r.Inbox.GetRuntimeCheckpointDelivery(ctx, envelope.InvocationID)
	if err == nil {
		if !record.MatchesIdentity(envelope) {
			return RuntimeDeliveryStatus{}, ErrConflict
		}
		return NewRuntimeDeliveryStatus(RuntimeDeliveryKindCheckpoint, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAccepted, true, record.ReceivedAt)
	}
	if !errors.Is(err, ErrNotFound) {
		return RuntimeDeliveryStatus{}, err
	}
	return NewRuntimeDeliveryStatus(RuntimeDeliveryKindCheckpoint, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAbsent, false, time.Time{})
}

func (t *RuntimeCheckpointDeliveryHTTPTransport) ReconcileCheckpointDelivery(ctx context.Context, envelope RuntimeCheckpointDeliveryEnvelope, projection RuntimeCheckpointProjection) (RuntimeDeliveryStatus, error) {
	if t == nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeDeliveryStatus)
	}
	normalized, err := NormalizeRuntimeCheckpointDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeCheckpointDeliveryAuth)
	}
	if err := projection.Validate(); err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	if projection.InvocationID != normalized.InvocationID || projection.SnapshotRevision != normalized.SnapshotRevision || projection.EventSequence != normalized.EventSequence {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: projection metadata 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	digest, err := RuntimeCheckpointProjectionDigest(projection)
	if err != nil || digest != normalized.SnapshotDigest {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: projection digest 不一致", ErrRuntimeCheckpointDeliveryAuth)
	}
	signedEnvelope, err := SignRuntimeCheckpointDeliveryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	body, err := json.Marshal(signedEnvelope)
	if err != nil || len(body) > MaxRuntimeCheckpointDeliveryEnvelopeBytes {
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
	request.Header.Set("X-Abot-Checkpoint-Delivery-Version", fmt.Sprint(signedEnvelope.Version))
	request.Header.Set("X-Abot-Checkpoint-Delivery-Signature", signedEnvelope.Signature)
	request.Header.Set("Idempotency-Key", signedEnvelope.DeliveryID)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("Runtime checkpoint delivery status 请求失败: %w", err)
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
		return RuntimeDeliveryStatus{}, fmt.Errorf("Runtime checkpoint delivery status 返回 HTTP %d: %s", response.StatusCode, message)
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
	if !status.Matches(RuntimeDeliveryKindCheckpoint, t.Source, t.Destination, signedEnvelope.DeliveryID, signedEnvelope.InvocationID) {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: status metadata 不一致", ErrRuntimeDeliveryStatusAuth)
	}
	return status, nil
}

var _ RuntimeCheckpointDeliveryReconciler = (*RuntimeCheckpointDeliveryHTTPTransport)(nil)
