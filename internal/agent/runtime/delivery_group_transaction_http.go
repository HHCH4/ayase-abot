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

type RuntimeDeliveryGroupReceiver struct {
	Source                  string
	Destination             string
	SharedSecret            []byte
	Inbox                   RuntimeDeliveryGroupTransactionRepository
	MemberPhaseResolver     RuntimeDeliveryGroupMemberPhaseResolver
	RequireCommittedMembers bool
	// RequireFence turns on the opt-in monotonic fence contract. Legacy
	// receivers leave it false and continue accepting unfenced envelopes.
	RequireFence  bool
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	MaxBodyBytes  int64
	Now           func() time.Time
}

func NewRuntimeDeliveryGroupReceiver(source, destination string, secret []byte, inbox RuntimeDeliveryGroupTransactionRepository) (*RuntimeDeliveryGroupReceiver, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if inbox == nil {
		return nil, fmt.Errorf("%w: transaction repository 不能为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	return &RuntimeDeliveryGroupReceiver{
		Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Inbox: inbox,
		MaxAge: DefaultRuntimeDeliveryGroupMaxAge, MaxFutureSkew: DefaultRuntimeDeliveryGroupFutureSkew,
		MaxBodyBytes: MaxRuntimeDeliveryGroupEnvelopeBytes, Now: time.Now,
	}, nil
}

func (r *RuntimeDeliveryGroupReceiver) Handler() http.Handler { return r }

func (r *RuntimeDeliveryGroupReceiver) PrepareHandler() http.Handler {
	return r.transactionHandler("prepare")
}

func (r *RuntimeDeliveryGroupReceiver) CommitHandler() http.Handler {
	return r.transactionHandler("commit")
}

func (r *RuntimeDeliveryGroupReceiver) AbortHandler() http.Handler {
	return r.transactionHandler("abort")
}

func (r *RuntimeDeliveryGroupReceiver) StatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { r.ServeStatusHTTP(writer, request) })
}

// BatchStatusHandler exposes the bounded, read-only status reconciliation
// endpoint. Each envelope remains independently HMAC-signed; the batch is
// rejected as a whole if any member fails validation.
func (r *RuntimeDeliveryGroupReceiver) BatchStatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { r.ServeBatchStatusHTTP(writer, request) })
}

func (r *RuntimeDeliveryGroupReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request != nil && request.URL != nil {
		path := strings.TrimRight(request.URL.Path, "/")
		switch {
		case strings.HasSuffix(path, "/status/batch"):
			r.ServeBatchStatusHTTP(writer, request)
			return
		case strings.HasSuffix(path, "/status"):
			r.ServeStatusHTTP(writer, request)
			return
		case strings.HasSuffix(path, "/prepare"):
			r.transactionHandler("prepare").ServeHTTP(writer, request)
			return
		case strings.HasSuffix(path, "/commit"):
			r.transactionHandler("commit").ServeHTTP(writer, request)
			return
		case strings.HasSuffix(path, "/abort"):
			r.transactionHandler("abort").ServeHTTP(writer, request)
			return
		}
	}
	writeRuntimeDeliveryGroupHTTPError(writer, http.StatusNotFound, fmt.Errorf("%w: 未知 group transaction 路由", ErrInvalidRuntimeDeliveryGroupTransaction))
}

func readRuntimeDeliveryGroupEnvelope(request *http.Request, configuredMax int64) (RuntimeDeliveryGroupEnvelope, error) {
	if request == nil || request.Body == nil {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: 请求 body 为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	maxBody := configuredMax
	if maxBody <= 0 || maxBody > MaxRuntimeDeliveryGroupEnvelopeBytes {
		maxBody = MaxRuntimeDeliveryGroupEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: 请求 body 超过上限或读取失败", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope RuntimeDeliveryGroupEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeDeliveryGroupEnvelope{}, fmt.Errorf("%w: 请求包含多个 JSON 文档", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	return NormalizeRuntimeDeliveryGroupEnvelope(envelope)
}

func (r *RuntimeDeliveryGroupReceiver) authenticate(request *http.Request) (RuntimeDeliveryGroupEnvelope, error) {
	envelope, err := readRuntimeDeliveryGroupEnvelope(request, r.MaxBodyBytes)
	if err != nil {
		return RuntimeDeliveryGroupEnvelope{}, err
	}
	if err := r.authenticateEnvelope(request, envelope); err != nil {
		return RuntimeDeliveryGroupEnvelope{}, err
	}
	return envelope, nil
}

func (r *RuntimeDeliveryGroupReceiver) authenticateEnvelope(request *http.Request, envelope RuntimeDeliveryGroupEnvelope) error {
	if r == nil || request == nil {
		return fmt.Errorf("%w: receiver/request 为空", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		return fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	if err := VerifyRuntimeDeliveryGroupEnvelope(envelope, r.SharedSecret); err != nil {
		return err
	}
	if r.RequireFence && envelope.FenceRevision == 0 {
		return ErrRuntimeDeliveryFenceRequired
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeDeliveryGroupTimestamp(envelope.IssuedAt, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		return err
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Delivery-Group-Version")); header != "" && header != fmt.Sprint(envelope.Version) {
		return fmt.Errorf("%w: header version 与 body 不一致", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Delivery-Group-Signature"))); header != "" && header != envelope.Signature {
		return fmt.Errorf("%w: header signature 与 body 不一致", ErrRuntimeDeliveryGroupTransactionAuth)
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != envelope.GroupID {
		return fmt.Errorf("%w: idempotency key 与 group_id 不一致", ErrInvalidRuntimeDeliveryGroupTransaction)
	}
	return nil
}

func (r *RuntimeDeliveryGroupReceiver) transactionHandler(phase string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if r == nil {
			writeRuntimeDeliveryGroupHTTPError(writer, http.StatusInternalServerError, ErrInvalidRuntimeDeliveryGroupTransaction)
			return
		}
		if request.Method != http.MethodPost {
			writeRuntimeDeliveryGroupHTTPError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeDeliveryGroupTransaction))
			return
		}
		if header := strings.TrimSpace(request.Header.Get("X-Abot-Delivery-Group-Phase")); header != phase {
			writeRuntimeDeliveryGroupHTTPError(writer, http.StatusBadRequest, fmt.Errorf("%w: phase header 不一致", ErrInvalidRuntimeDeliveryGroupTransaction))
			return
		}
		envelope, err := r.authenticate(request)
		if err != nil {
			writeRuntimeDeliveryGroupHTTPError(writer, runtimeDeliveryGroupHTTPStatus(err), err)
			return
		}
		if phase == "commit" && r.RequireCommittedMembers {
			if r.MemberPhaseResolver == nil {
				writeRuntimeDeliveryGroupHTTPError(writer, http.StatusNotImplemented, fmt.Errorf("%w: 未配置 member phase resolver", ErrInvalidRuntimeDeliveryGroupTransaction))
				return
			}
			for _, member := range envelope.Members {
				memberPhase, resolveErr := r.MemberPhaseResolver.RuntimeDeliveryGroupMemberPhase(request.Context(), member)
				if resolveErr != nil {
					writeRuntimeDeliveryGroupHTTPError(writer, http.StatusConflict, fmt.Errorf("%w: member phase 不可证明", ErrRuntimeDeliveryGroupTransactionAuth))
					return
				}
				if memberPhase != RuntimeDeliveryGroupTransactionCommitted {
					writeRuntimeDeliveryGroupHTTPError(writer, http.StatusConflict, fmt.Errorf("%w: member 尚未 committed", ErrConflict))
					return
				}
			}
		}
		var duplicate bool
		switch phase {
		case "prepare":
			duplicate, err = r.Inbox.PrepareRuntimeDeliveryGroup(request.Context(), envelope)
		case "commit":
			duplicate, err = r.Inbox.CommitRuntimeDeliveryGroup(request.Context(), envelope)
		case "abort":
			duplicate, err = r.Inbox.AbortRuntimeDeliveryGroup(request.Context(), envelope)
		default:
			err = fmt.Errorf("%w: phase %q 不受支持", ErrInvalidRuntimeDeliveryGroupTransaction, phase)
		}
		if err != nil {
			writeRuntimeDeliveryGroupHTTPError(writer, runtimeDeliveryGroupHTTPStatus(err), err)
			return
		}
		receipt := RuntimeDeliveryGroupTransactionReceipt{Version: envelope.Version, GroupID: envelope.GroupID, InvocationID: envelope.InvocationID, FenceID: envelope.FenceID, FenceRevision: envelope.FenceRevision, MembersDigest: envelope.MembersDigest, Phase: phase, Duplicate: duplicate}
		writeRuntimeDeliveryGroupJSON(writer, http.StatusOK, receipt)
	})
}

func (r *RuntimeDeliveryGroupReceiver) ServeStatusHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeDeliveryGroupHTTPError(writer, http.StatusInternalServerError, ErrInvalidRuntimeDeliveryGroupTransaction)
		return
	}
	if request.Method != http.MethodPost {
		writeRuntimeDeliveryGroupHTTPError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeDeliveryGroupTransaction))
		return
	}
	envelope, err := r.authenticate(request)
	if err != nil {
		writeRuntimeDeliveryGroupHTTPError(writer, runtimeDeliveryGroupHTTPStatus(err), err)
		return
	}
	status, err := r.statusForEnvelope(request.Context(), envelope)
	if err != nil {
		writeRuntimeDeliveryGroupHTTPError(writer, runtimeDeliveryGroupHTTPStatus(err), err)
		return
	}
	signed, err := SignRuntimeDeliveryGroupStatus(status, r.SharedSecret)
	if err != nil {
		writeRuntimeDeliveryGroupHTTPError(writer, http.StatusInternalServerError, err)
		return
	}
	writeRuntimeDeliveryGroupJSON(writer, http.StatusOK, signed)
}

func (r *RuntimeDeliveryGroupReceiver) statusForEnvelope(ctx context.Context, envelope RuntimeDeliveryGroupEnvelope) (RuntimeDeliveryGroupRemoteStatus, error) {
	transaction, err := r.Inbox.GetRuntimeDeliveryGroupTransaction(ctx, envelope.GroupID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return RuntimeDeliveryGroupRemoteStatus{}, err
	}
	if errors.Is(err, ErrNotFound) {
		return NewRuntimeDeliveryGroupRemoteStatus(envelope, "absent", false, time.Time{})
	}
	if !transaction.Matches(envelope) {
		return RuntimeDeliveryGroupRemoteStatus{}, ErrConflict
	}
	return NewRuntimeDeliveryGroupRemoteStatus(envelope, transaction.Status, true, transaction.UpdatedAt)
}

func runtimeDeliveryGroupHTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrRuntimeDeliveryGroupTransactionAuth), errors.Is(err, ErrRuntimeDeliveryGroupTransactionStale), errors.Is(err, ErrRuntimeDeliveryFenceStale):
		return http.StatusUnauthorized
	case errors.Is(err, ErrConflict), errors.Is(err, ErrRuntimeDeliveryFenceConflict), errors.Is(err, ErrRuntimeDeliveryFenceRequired):
		return http.StatusConflict
	case errors.Is(err, ErrRuntimeDeliveryGroupTransactionUnavailable):
		return http.StatusNotImplemented
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}

func writeRuntimeDeliveryGroupJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeRuntimeDeliveryGroupHTTPError(writer http.ResponseWriter, status int, err error) {
	message := "Runtime delivery group transaction error"
	if err != nil {
		message = SanitizeRuntimeString(err.Error())
	}
	writeRuntimeDeliveryGroupJSON(writer, status, map[string]string{"error": message})
}
