package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	MaxRuntimeDeliveryGroupBatchStatusCount = 32
	MaxRuntimeDeliveryGroupBatchStatusBytes = 256 << 10
)

// RuntimeDeliveryGroupStatusBatchRequest is a bounded collection of
// independently signed envelopes. It carries no delivery payloads.
type RuntimeDeliveryGroupStatusBatchRequest struct {
	Envelopes []RuntimeDeliveryGroupEnvelope `json:"envelopes"`
}

type RuntimeDeliveryGroupStatusBatchResponse struct {
	Statuses []RuntimeDeliveryGroupRemoteStatus `json:"statuses"`
}

func normalizeRuntimeDeliveryGroupStatusBatch(envelopes []RuntimeDeliveryGroupEnvelope) ([]RuntimeDeliveryGroupEnvelope, error) {
	if len(envelopes) == 0 || len(envelopes) > MaxRuntimeDeliveryGroupBatchStatusCount {
		return nil, fmt.Errorf("%w: batch 数量必须在 1-%d", ErrInvalidRuntimeDeliveryGroupTransaction, MaxRuntimeDeliveryGroupBatchStatusCount)
	}
	normalized := make([]RuntimeDeliveryGroupEnvelope, len(envelopes))
	seen := make(map[string]struct{}, len(envelopes))
	for index, envelope := range envelopes {
		item, err := NormalizeRuntimeDeliveryGroupEnvelope(envelope)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[item.GroupID]; exists {
			return nil, fmt.Errorf("%w: batch group_id 重复", ErrInvalidRuntimeDeliveryGroupTransaction)
		}
		seen[item.GroupID] = struct{}{}
		normalized[index] = item
	}
	return normalized, nil
}

func readRuntimeDeliveryGroupStatusBatch(request *http.Request, configuredMax int64) ([]RuntimeDeliveryGroupEnvelope, error) {
	if request == nil || request.Body == nil {
		return nil, fmt.Errorf("%w: batch 请求 body 为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeDeliveryGroupBatchStatusBytes {
		maxBody = MaxRuntimeDeliveryGroupBatchStatusBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return nil, fmt.Errorf("%w: batch 请求 body 超过上限或读取失败", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var batch RuntimeDeliveryGroupStatusBatchRequest
	if err := decoder.Decode(&batch); err != nil {
		return nil, fmt.Errorf("%w: batch JSON 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%w: batch 包含多个 JSON 文档", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	return normalizeRuntimeDeliveryGroupStatusBatch(batch.Envelopes)
}

func (r *RuntimeDeliveryGroupReceiver) ServeBatchStatusHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeDeliveryGroupHTTPError(writer, http.StatusInternalServerError, ErrInvalidRuntimeDeliveryGroupTransaction)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeDeliveryGroupHTTPError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeDeliveryGroupTransaction))
		return
	}
	if strings.TrimSpace(request.Header.Get("X-Abot-Delivery-Group-Signature")) != "" || strings.TrimSpace(request.Header.Get("Idempotency-Key")) != "" {
		writeRuntimeDeliveryGroupHTTPError(writer, http.StatusBadRequest, fmt.Errorf("%w: batch 不支持单值 signature/idempotency header", ErrInvalidRuntimeDeliveryGroupTransaction))
		return
	}
	configuredMax := r.MaxBodyBytes
	if configuredMax == MaxRuntimeDeliveryGroupEnvelopeBytes {
		configuredMax = MaxRuntimeDeliveryGroupBatchStatusBytes
	}
	envelopes, err := readRuntimeDeliveryGroupStatusBatch(request, configuredMax)
	if err != nil {
		writeRuntimeDeliveryGroupHTTPError(writer, runtimeDeliveryGroupHTTPStatus(err), err)
		return
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Delivery-Group-Batch-Count")); header != "" && header != fmt.Sprint(len(envelopes)) {
		writeRuntimeDeliveryGroupHTTPError(writer, http.StatusBadRequest, fmt.Errorf("%w: batch count header 与 body 不一致", ErrInvalidRuntimeDeliveryGroupTransaction))
		return
	}
	for _, envelope := range envelopes {
		if err := r.authenticateEnvelope(request, envelope); err != nil {
			writeRuntimeDeliveryGroupHTTPError(writer, runtimeDeliveryGroupHTTPStatus(err), err)
			return
		}
	}
	statuses := make([]RuntimeDeliveryGroupRemoteStatus, len(envelopes))
	for index, envelope := range envelopes {
		status, statusErr := r.statusForEnvelope(request.Context(), envelope)
		if statusErr != nil {
			writeRuntimeDeliveryGroupHTTPError(writer, runtimeDeliveryGroupHTTPStatus(statusErr), statusErr)
			return
		}
		signed, signErr := SignRuntimeDeliveryGroupStatus(status, r.SharedSecret)
		if signErr != nil {
			writeRuntimeDeliveryGroupHTTPError(writer, http.StatusInternalServerError, signErr)
			return
		}
		statuses[index] = signed
	}
	writeRuntimeDeliveryGroupJSON(writer, http.StatusOK, RuntimeDeliveryGroupStatusBatchResponse{Statuses: statuses})
}

// RuntimeDeliveryGroupBatchReconciler is optional. A caller can retain the
// single-envelope reconciler when the remote endpoint predates /status/batch.
type RuntimeDeliveryGroupBatchReconciler interface {
	ReconcileRuntimeDeliveryGroups(context.Context, []RuntimeDeliveryGroupEnvelope) ([]RuntimeDeliveryGroupRemoteStatus, error)
}

func (t *RuntimeDeliveryGroupHTTPTransport) ReconcileRuntimeDeliveryGroups(ctx context.Context, envelopes []RuntimeDeliveryGroupEnvelope) ([]RuntimeDeliveryGroupRemoteStatus, error) {
	if t == nil {
		return nil, fmt.Errorf("%w: transport 为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	normalized, err := normalizeRuntimeDeliveryGroupStatusBatch(envelopes)
	if err != nil {
		return nil, err
	}
	signedEnvelopes := make([]RuntimeDeliveryGroupEnvelope, len(normalized))
	for index, envelope := range normalized {
		if envelope.Source != t.Source || envelope.Destination != t.Destination {
			return nil, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
		}
		signed, signErr := SignRuntimeDeliveryGroupEnvelope(envelope, t.SharedSecret)
		if signErr != nil {
			return nil, signErr
		}
		signedEnvelopes[index] = signed
	}
	body, err := json.Marshal(RuntimeDeliveryGroupStatusBatchRequest{Envelopes: signedEnvelopes})
	if err != nil || int64(len(body)) > t.maxBatchBodyBytes() {
		return nil, fmt.Errorf("%w: batch request body 超过上限", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	endpoint, err := runtimeDeliveryGroupTransactionEndpoint(t.Endpoint, "status/batch")
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Delivery-Group-Version", fmt.Sprint(RuntimeDeliveryGroupEnvelopeVersion))
	request.Header.Set("X-Abot-Delivery-Group-Batch-Count", fmt.Sprint(len(signedEnvelopes)))
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Runtime delivery group status batch 请求失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, MaxRuntimeDeliveryGroupBatchStatusBytes+1))
	if err != nil || len(responseBody) > MaxRuntimeDeliveryGroupBatchStatusBytes {
		return nil, fmt.Errorf("%w: batch response 超过上限或读取失败", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
			return nil, ErrRuntimeDeliveryGroupTransactionUnavailable
		}
		if response.StatusCode == http.StatusConflict {
			return nil, ErrConflict
		}
		message := strings.TrimSpace(SanitizeRuntimeString(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		return nil, fmt.Errorf("Runtime delivery group status batch 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var batch RuntimeDeliveryGroupStatusBatchResponse
	if err := decoder.Decode(&batch); err != nil {
		return nil, fmt.Errorf("%w: batch response JSON 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%w: batch response 包含多个 JSON 文档", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if len(batch.Statuses) != len(normalized) {
		return nil, fmt.Errorf("%w: batch response 数量不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	for index, status := range batch.Statuses {
		if err := VerifyRuntimeDeliveryGroupStatus(status, t.SharedSecret); err != nil {
			return nil, err
		}
		if err := status.ValidateAgainst(normalized[index]); err != nil {
			return nil, err
		}
	}
	return batch.Statuses, nil
}

func (t *RuntimeDeliveryGroupHTTPTransport) maxBatchBodyBytes() int64 {
	if t == nil || t.MaxBodyBytes <= 0 || t.MaxBodyBytes == MaxRuntimeDeliveryGroupEnvelopeBytes || t.MaxBodyBytes == MaxRuntimeDeliveryGroupResponseBytes || t.MaxBodyBytes > MaxRuntimeDeliveryGroupBatchStatusBytes {
		return MaxRuntimeDeliveryGroupBatchStatusBytes
	}
	return t.MaxBodyBytes
}

var _ RuntimeDeliveryGroupBatchReconciler = (*RuntimeDeliveryGroupHTTPTransport)(nil)
