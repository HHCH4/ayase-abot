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

func (r *RuntimeConfigDeliveryReceiver) StatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeStatusHTTP(writer, request)
	})
}

func readRuntimeConfigDeliveryStatusEnvelope(request *http.Request, configuredMax int64) (RuntimeConfigDeliveryEnvelope, error) {
	if request == nil || request.Body == nil {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeConfigDelivery)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDeliveryEnvelopeBytes {
		maxBody = MaxRuntimeConfigDeliveryEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: status 请求 body 超过上限或读取失败", ErrInvalidRuntimeConfigDelivery)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope RuntimeConfigDeliveryEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: status JSON 无效", ErrInvalidRuntimeConfigDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: status 包含多个 JSON 文档", ErrInvalidRuntimeConfigDelivery)
	}
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeConfigDeliveryEnvelope{}, err
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Config-Delivery-Version")); header != "" && header != fmt.Sprint(normalized.Version) {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: header version 与 body 不一致", ErrInvalidRuntimeConfigDelivery)
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Config-Delivery-Signature"))); header != "" && header != normalized.Signature {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: header signature 与 body 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != normalized.DeliveryID {
		return RuntimeConfigDeliveryEnvelope{}, fmt.Errorf("%w: idempotency key 与 delivery_id 不一致", ErrInvalidRuntimeConfigDelivery)
	}
	return normalized, nil
}

func (r *RuntimeConfigDeliveryReceiver) ServeStatusHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeConfigDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeConfigDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeConfigDelivery))
		return
	}
	envelope, err := readRuntimeConfigDeliveryStatusEnvelope(request, r.MaxBodyBytes)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrRuntimeConfigDeliveryAuth) {
			code = http.StatusUnauthorized
		}
		writeRuntimeConfigDeliveryError(writer, code, err)
		return
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		writeRuntimeConfigDeliveryError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDeliveryAuth))
		return
	}
	if err := VerifyRuntimeConfigDeliveryEnvelope(envelope, r.SharedSecret); err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeConfigDeliveryTimestamp(envelope.IssuedAt, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusUnauthorized, err)
		return
	}
	status, err := r.resolveConfigDeliveryStatus(request.Context(), envelope)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrConflict):
			code = http.StatusConflict
		case errors.Is(err, ErrRuntimeDeliveryStatusUnavailable):
			code = http.StatusNotImplemented
		}
		writeRuntimeConfigDeliveryError(writer, code, err)
		return
	}
	signed, err := SignRuntimeDeliveryStatus(status, r.SharedSecret)
	if err != nil {
		writeRuntimeConfigDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	writeRuntimeConfigDeliveryJSON(writer, http.StatusOK, signed)
}

func runtimeConfigTransactionMatchesIdentity(transaction RuntimeConfigDeliveryTransaction, envelope RuntimeConfigDeliveryEnvelope) bool {
	return transaction.Version == envelope.Version && transaction.DeliveryID == envelope.DeliveryID && transaction.Source == envelope.Source && transaction.Destination == envelope.Destination && transaction.InvocationID == envelope.InvocationID && transaction.ExpectedDigest == envelope.ExpectedDigest && transaction.SnapshotDigest == envelope.SnapshotDigest && transaction.SnapshotVersion == envelope.SnapshotVersion && transaction.IdempotencyKey == envelope.IdempotencyKey
}

func (r *RuntimeConfigDeliveryReceiver) resolveConfigDeliveryStatus(ctx context.Context, envelope RuntimeConfigDeliveryEnvelope) (RuntimeDeliveryStatus, error) {
	txnReader, hasTxnReader := r.Inbox.(interface {
		GetRuntimeConfigDeliveryTransaction(context.Context, string) (RuntimeConfigDeliveryTransaction, error)
	})
	if hasTxnReader {
		transaction, err := txnReader.GetRuntimeConfigDeliveryTransaction(ctx, envelope.DeliveryID)
		if err == nil {
			if !runtimeConfigTransactionMatchesIdentity(transaction, envelope) {
				return RuntimeDeliveryStatus{}, ErrConflict
			}
			switch transaction.Status {
			case RuntimeConfigDeliveryTransactionCommitted:
				return NewRuntimeDeliveryStatus(RuntimeDeliveryKindConfig, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusCommitted, true, transaction.UpdatedAt)
			case RuntimeConfigDeliveryTransactionPrepared:
				return NewRuntimeDeliveryStatus(RuntimeDeliveryKindConfig, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusPrepared, true, transaction.UpdatedAt)
			case RuntimeConfigDeliveryTransactionAborted:
			}
		} else if !errors.Is(err, ErrNotFound) {
			return RuntimeDeliveryStatus{}, err
		}
	}
	record, err := r.Inbox.GetRuntimeConfigDelivery(ctx, envelope.InvocationID)
	if err == nil {
		if !record.MatchesIdentity(envelope) {
			return RuntimeDeliveryStatus{}, ErrConflict
		}
		return NewRuntimeDeliveryStatus(RuntimeDeliveryKindConfig, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAccepted, true, record.ReceivedAt)
	}
	if !errors.Is(err, ErrNotFound) {
		return RuntimeDeliveryStatus{}, err
	}
	return NewRuntimeDeliveryStatus(RuntimeDeliveryKindConfig, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAbsent, false, time.Time{})
}

func (t *RuntimeConfigDeliveryHTTPTransport) ReconcileConfigDelivery(ctx context.Context, envelope RuntimeConfigDeliveryEnvelope, projection RuntimeConfigDeliveryProjection) (RuntimeDeliveryStatus, error) {
	if t == nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeDeliveryStatus)
	}
	normalized, err := NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDeliveryAuth)
	}
	parsed, err := normalizeRuntimeConfigDeliveryProjection(projection)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	if parsed.Version != normalized.SnapshotVersion || agentRuntimeConfigSnapshotDigest(parsed) != normalized.SnapshotDigest {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: projection digest/version 不一致", ErrRuntimeConfigDeliveryAuth)
	}
	signedEnvelope, err := SignRuntimeConfigDeliveryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	body, err := json.Marshal(signedEnvelope)
	if err != nil || len(body) > MaxRuntimeConfigDeliveryEnvelopeBytes {
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
	request.Header.Set("X-Abot-Config-Delivery-Version", fmt.Sprint(signedEnvelope.Version))
	request.Header.Set("X-Abot-Config-Delivery-Signature", signedEnvelope.Signature)
	request.Header.Set("Idempotency-Key", signedEnvelope.DeliveryID)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("Runtime config delivery status 请求失败: %w", err)
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
		return RuntimeDeliveryStatus{}, fmt.Errorf("Runtime config delivery status 返回 HTTP %d: %s", response.StatusCode, message)
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
	if !status.Matches(RuntimeDeliveryKindConfig, t.Source, t.Destination, signedEnvelope.DeliveryID, signedEnvelope.InvocationID) {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: status metadata 不一致", ErrRuntimeDeliveryStatusAuth)
	}
	return status, nil
}

var _ RuntimeConfigDeliveryReconciler = (*RuntimeConfigDeliveryHTTPTransport)(nil)
