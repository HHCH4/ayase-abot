package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func readRuntimeConfigDirectoryEnvelope(request *http.Request, maxBody int64) (RuntimeConfigDirectoryEnvelope, error) {
	if request == nil || request.Body == nil {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: request body 为空", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDirectoryEnvelopeBytes {
		maxBody = MaxRuntimeConfigDirectoryEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: request body 超过上限或读取失败", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope RuntimeConfigDirectoryEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: envelope JSON 无效", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: envelope 包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	return envelope, nil
}

func (r *RuntimeConfigDirectoryReceiver) StatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		r.ServeStatusHTTP(writer, request)
	})
}

// ServeStatusHTTP returns an authenticated proof for the exact delivery
// identity. It never returns the catalog body, even when the entry exists.
func (r *RuntimeConfigDirectoryReceiver) ServeStatusHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusInternalServerError, map[string]string{"error": ErrInvalidRuntimeConfigDirectoryStatus.Error()})
		return
	}
	if request.Method != http.MethodPost {
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
	if normalized.Source != r.Source || normalized.Destination != r.Destination {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusForbidden, map[string]string{"error": ErrRuntimeConfigDirectoryStatusAuth.Error()})
		return
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Config-Directory-Version")); header != "" && header != fmt.Sprintf("%d", normalized.Version) {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusBadRequest, map[string]string{"error": "version header 与 body 不一致"})
		return
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Config-Directory-Signature"))); header != "" && header != normalized.Signature {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusUnauthorized, map[string]string{"error": ErrRuntimeConfigDirectoryStatusAuth.Error()})
		return
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != normalized.DeliveryID {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusBadRequest, map[string]string{"error": "idempotency key 与 delivery_id 不一致"})
		return
	}
	if err := VerifyRuntimeConfigDirectoryEnvelope(normalized, r.SharedSecret); err != nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusUnauthorized, map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeConfigDirectoryTimestamp(normalized.IssuedAt, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeConfigDirectoryJSON(writer, runtimeConfigDirectoryHTTPStatus(err), map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	record, getErr := r.Repository.GetRuntimeConfigDirectory(request.Context(), normalized.Kind, normalized.EntryID)
	var status RuntimeConfigDirectoryStatus
	switch {
	case errors.Is(getErr, ErrNotFound):
		status, err = NewRuntimeConfigDirectoryStatus(normalized, RuntimeConfigDirectoryStatusAbsent, false, time.Time{})
	case getErr != nil:
		err = getErr
	case !runtimeConfigDirectoryRecordMatches(record, normalized, record.Entry):
		// Returning a generic conflict prevents an old source cursor from
		// mistaking a different revision/body for proof of this delivery.
		err = fmt.Errorf("%w: destination record identity 不一致", ErrRuntimeConfigDirectoryConflict)
	default:
		status, err = NewRuntimeConfigDirectoryStatus(normalized, RuntimeConfigDirectoryStatusAccepted, true, record.ReceivedAt)
	}
	if err != nil {
		code := runtimeConfigDirectoryHTTPStatus(err)
		if code == http.StatusBadRequest {
			code = http.StatusConflict
		}
		writeRuntimeConfigDirectoryJSON(writer, code, map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	signed, err := SignRuntimeConfigDirectoryStatus(status, r.SharedSecret)
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusInternalServerError, map[string]string{"error": ErrInvalidRuntimeConfigDirectoryStatus.Error()})
		return
	}
	writeRuntimeConfigDirectoryJSON(writer, http.StatusOK, signed)
}
