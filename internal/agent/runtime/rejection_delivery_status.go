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

func (r *RuntimeApprovalRejectionDeliveryReceiver) StatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeStatusHTTP(writer, request)
	})
}

func (r *RuntimeApprovalRejectionDeliveryReceiver) ServeStatusHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusInternalServerError, ErrInvalidRuntimeApprovalRejectionDelivery)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeApprovalRejectionDelivery))
		return
	}
	envelope, err := readRuntimeApprovalRejectionDeliveryEnvelope(request, r.MaxBodyBytes)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrRuntimeApprovalRejectionDeliveryAuth) {
			code = http.StatusUnauthorized
		}
		writeRuntimeApprovalRejectionDeliveryError(writer, code, err)
		return
	}
	if err := r.authenticate(envelope); err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, ErrInvalidRuntimeApprovalRejectionDelivery) {
			code = http.StatusBadRequest
		}
		writeRuntimeApprovalRejectionDeliveryError(writer, code, err)
		return
	}
	status, err := r.resolveApprovalRejectionDeliveryStatus(request.Context(), envelope)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrConflict):
			code = http.StatusConflict
		case errors.Is(err, ErrRuntimeDeliveryStatusUnavailable):
			code = http.StatusNotImplemented
		}
		writeRuntimeApprovalRejectionDeliveryError(writer, code, err)
		return
	}
	signed, err := SignRuntimeDeliveryStatus(status, r.SharedSecret)
	if err != nil {
		writeRuntimeApprovalRejectionDeliveryError(writer, http.StatusInternalServerError, err)
		return
	}
	writeRuntimeApprovalRejectionDeliveryJSON(writer, http.StatusOK, signed)
}

func runtimeApprovalRejectionTransactionMatchesIdentity(transaction RuntimeApprovalRejectionDeliveryTransaction, envelope RuntimeApprovalRejectionDeliveryEnvelope) bool {
	return transaction.Version == envelope.Version && transaction.DeliveryID == envelope.DeliveryID && transaction.ApprovalID == envelope.ApprovalID && transaction.InvocationID == envelope.InvocationID && transaction.ToolCallID == envelope.ToolCallID && transaction.OperationID == envelope.OperationID && transaction.Source == envelope.Source && transaction.Destination == envelope.Destination && transaction.Decision == envelope.Decision && transaction.ReasonDigest == envelope.ReasonDigest && transaction.DecisionAt.UTC().Equal(envelope.DecisionAt.UTC())
}

func (r *RuntimeApprovalRejectionDeliveryReceiver) resolveApprovalRejectionDeliveryStatus(ctx context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeDeliveryStatus, error) {
	txnReader, hasTxnReader := r.Inbox.(interface {
		GetRuntimeApprovalRejectionDeliveryTransaction(context.Context, string) (RuntimeApprovalRejectionDeliveryTransaction, error)
	})
	if hasTxnReader {
		transaction, err := txnReader.GetRuntimeApprovalRejectionDeliveryTransaction(ctx, envelope.DeliveryID)
		if err == nil {
			if !runtimeApprovalRejectionTransactionMatchesIdentity(transaction, envelope) {
				return RuntimeDeliveryStatus{}, ErrConflict
			}
			switch transaction.Status {
			case RuntimeApprovalRejectionDeliveryTransactionCommitted:
				return NewRuntimeDeliveryStatus(RuntimeDeliveryKindRejection, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusCommitted, true, transaction.UpdatedAt)
			case RuntimeApprovalRejectionDeliveryTransactionPrepared:
				return NewRuntimeDeliveryStatus(RuntimeDeliveryKindRejection, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusPrepared, true, transaction.UpdatedAt)
			case RuntimeApprovalRejectionDeliveryTransactionAborted:
			}
		} else if !errors.Is(err, ErrNotFound) {
			return RuntimeDeliveryStatus{}, err
		}
	}
	record, err := r.Inbox.GetRuntimeApprovalRejectionDelivery(ctx, envelope.DeliveryID)
	if err == nil {
		if !record.Matches(envelope) {
			return RuntimeDeliveryStatus{}, ErrConflict
		}
		return NewRuntimeDeliveryStatus(RuntimeDeliveryKindRejection, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAccepted, true, record.ReceivedAt)
	}
	if !errors.Is(err, ErrNotFound) {
		return RuntimeDeliveryStatus{}, err
	}
	return NewRuntimeDeliveryStatus(RuntimeDeliveryKindRejection, r.Source, r.Destination, envelope.DeliveryID, envelope.InvocationID, RuntimeDeliveryStatusAbsent, false, time.Time{})
}

func (t *RuntimeApprovalRejectionDeliveryHTTPTransport) ReconcileApprovalRejectionDelivery(ctx context.Context, envelope RuntimeApprovalRejectionDeliveryEnvelope) (RuntimeDeliveryStatus, error) {
	if t == nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeDeliveryStatus)
	}
	normalized, err := envelope.normalize()
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeApprovalRejectionDeliveryAuth)
	}
	signedEnvelope, err := SignRuntimeApprovalRejectionDeliveryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeDeliveryStatus{}, err
	}
	body, err := json.Marshal(signedEnvelope)
	if err != nil || len(body) > MaxRuntimeApprovalRejectionDeliveryEnvelopeBytes {
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
	request.Header.Set("X-Abot-Approval-Rejection-Delivery-Version", fmt.Sprint(signedEnvelope.Version))
	request.Header.Set("X-Abot-Approval-Rejection-Delivery-Signature", signedEnvelope.Signature)
	request.Header.Set("Idempotency-Key", signedEnvelope.DeliveryID)
	response, err := t.Client.Do(request)
	if err != nil {
		return RuntimeDeliveryStatus{}, fmt.Errorf("Runtime approval rejection delivery status 请求失败: %w", err)
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
		return RuntimeDeliveryStatus{}, fmt.Errorf("Runtime approval rejection delivery status 返回 HTTP %d: %s", response.StatusCode, message)
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
	if !status.Matches(RuntimeDeliveryKindRejection, t.Source, t.Destination, signedEnvelope.DeliveryID, signedEnvelope.InvocationID) {
		return RuntimeDeliveryStatus{}, fmt.Errorf("%w: status metadata 不一致", ErrRuntimeDeliveryStatusAuth)
	}
	return status, nil
}

var _ RuntimeApprovalRejectionDeliveryReconciler = (*RuntimeApprovalRejectionDeliveryHTTPTransport)(nil)
