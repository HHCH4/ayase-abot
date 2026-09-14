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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RuntimeLongRunningToolStatusVersion is the authenticated status protocol
// version used between Abot Runtime and an external Tool Runtime.
const RuntimeLongRunningToolStatusVersion = 1

const (
	DefaultRuntimeLongRunningToolStatusSource      = "agent-runtime"
	DefaultRuntimeLongRunningToolStatusDestination = "tool-runtime"

	MaxRuntimeLongRunningToolStatusSourceLength      = MaxRuntimeEventDeliverySourceLength
	MaxRuntimeLongRunningToolStatusDestinationLength = MaxRuntimeEventDeliveryTargetLength
	MaxRuntimeLongRunningToolStatusQueryIDLength     = 220
	MaxRuntimeLongRunningToolStatusInvocationLength  = 220
	MaxRuntimeLongRunningToolStatusWaitIDLength      = 512
	MaxRuntimeLongRunningToolStatusToolNameLength    = 160
	MaxRuntimeLongRunningToolStatusReasonLength      = 1024
	MaxRuntimeLongRunningToolStatusCodeLength        = 160
	MaxRuntimeLongRunningToolStatusBodyBytes         = 320 << 10
	MaxRuntimeLongRunningToolStatusRequestBytes      = 32 << 10
	MaxRuntimeLongRunningToolStatusFutureSkew        = 30 * time.Second
)

var (
	ErrInvalidRuntimeLongRunningToolStatus        = errors.New("Runtime long-running tool status 无效")
	ErrRuntimeLongRunningToolStatusAuth           = errors.New("Runtime long-running tool status 认证失败")
	ErrRuntimeLongRunningToolStatusUnavailable    = errors.New("Runtime long-running tool status 查询未配置")
	ErrRuntimeLongRunningToolStatusTargetMismatch = errors.New("Runtime long-running tool status 目标不匹配")
)

// RuntimeLongRunningToolStatusState is the conservative state returned by an
// external Tool Runtime. Unknown is deliberately distinct from failed: the
// caller cannot safely infer whether an external side effect happened.
type RuntimeLongRunningToolStatusState string

const (
	RuntimeLongRunningToolStatusPending   RuntimeLongRunningToolStatusState = "pending"
	RuntimeLongRunningToolStatusSucceeded RuntimeLongRunningToolStatusState = "succeeded"
	RuntimeLongRunningToolStatusFailed    RuntimeLongRunningToolStatusState = "failed"
	RuntimeLongRunningToolStatusUnknown   RuntimeLongRunningToolStatusState = "unknown"
)

// RuntimeLongRunningToolStatusQuery binds one status lookup to the exact
// Invocation/wait boundary and the immutable ToolRequested request digest.
// It carries no prompt, arguments or credentials.
type RuntimeLongRunningToolStatusQuery struct {
	Version       int       `json:"version"`
	Source        string    `json:"source"`
	Destination   string    `json:"destination"`
	QueryID       string    `json:"query_id"`
	InvocationID  string    `json:"invocation_id"`
	WaitID        string    `json:"wait_id"`
	ToolName      string    `json:"tool_name"`
	RequestDigest string    `json:"request_digest"`
	IssuedAt      time.Time `json:"issued_at"`
	Signature     string    `json:"signature,omitempty"`
}

// RuntimeLongRunningToolStatus is a bounded, authenticated status proof. A
// succeeded response must include a result digest; the result body is
// optional and, when present, is bounded and digest-checked. Querying this
// value never resumes an Invocation automatically.
type RuntimeLongRunningToolStatus struct {
	Version         int                               `json:"version"`
	Source          string                            `json:"source"`
	Destination     string                            `json:"destination"`
	QueryID         string                            `json:"query_id"`
	InvocationID    string                            `json:"invocation_id"`
	WaitID          string                            `json:"wait_id"`
	ToolName        string                            `json:"tool_name"`
	RequestDigest   string                            `json:"request_digest"`
	Status          RuntimeLongRunningToolStatusState `json:"status"`
	Result          map[string]any                    `json:"result,omitempty"`
	ResultDigest    string                            `json:"result_digest,omitempty"`
	ResultAvailable bool                              `json:"result_available,omitempty"`
	ErrorCode       string                            `json:"error_code,omitempty"`
	ErrorMessage    string                            `json:"error_message,omitempty"`
	Reason          string                            `json:"reason,omitempty"`
	ObservedAt      time.Time                         `json:"observed_at"`
	Signature       string                            `json:"signature,omitempty"`
}

// RuntimeLongRunningToolStatusReconcileResult is the outcome of an explicit
// status reconciliation request. A status query alone is always read-only;
// this wrapper reports whether the caller's explicit reconcile request was
// allowed to submit the bounded result to the current waiting boundary.
type RuntimeLongRunningToolStatusReconcileResult struct {
	Status     RuntimeLongRunningToolStatus `json:"status"`
	Resumed    bool                         `json:"resumed"`
	Invocation *Invocation                  `json:"invocation,omitempty"`
	Reason     string                       `json:"reason,omitempty"`
}

// RuntimeLongRunningToolStatusBatchReconcileResult is the all-or-nothing
// outcome for a waiting boundary containing more than one external tool call.
// The result list is returned even when one proof is unproven, so a caller can
// render the mixed boundary without guessing which side effect is safe.
type RuntimeLongRunningToolStatusBatchReconcileResult struct {
	Statuses   []RuntimeLongRunningToolStatus `json:"statuses"`
	Resumed    bool                           `json:"resumed"`
	Invocation *Invocation                    `json:"invocation,omitempty"`
	Reason     string                         `json:"reason,omitempty"`
}

// RuntimeLongRunningToolStatusResolver is implemented by a Tool Runtime
// adapter. Implementations must only inspect the bound query and return a
// status proof; they must not replay the tool on a missing/unknown record.
type RuntimeLongRunningToolStatusResolver interface {
	QueryLongRunningToolStatus(context.Context, RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error)
}

// RuntimeLongRunningToolStatusResolverFunc adapts a function to the resolver
// interface for in-process Tool Runtime implementations and deterministic
// tests.
type RuntimeLongRunningToolStatusResolverFunc func(context.Context, RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error)

func (f RuntimeLongRunningToolStatusResolverFunc) QueryLongRunningToolStatus(ctx context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
	if f == nil {
		return RuntimeLongRunningToolStatus{}, ErrRuntimeLongRunningToolStatusUnavailable
	}
	return f(ctx, query)
}

// RuntimeLongRunningToolStatusRouteProvider lets a transport expose the
// signed route that the Coordinator should place in each query.
type RuntimeLongRunningToolStatusRouteProvider interface {
	RuntimeLongRunningToolStatusRoute() (source, destination string)
}

func validRuntimeLongRunningToolStatusDigest(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	return isRuntimeEventDeliveryDigest(strings.TrimPrefix(value, "sha256:"))
}

func normalizeRuntimeLongRunningToolStatusQuery(query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatusQuery, error) {
	query.Source = strings.TrimSpace(query.Source)
	query.Destination = strings.TrimSpace(query.Destination)
	query.QueryID = strings.TrimSpace(query.QueryID)
	query.InvocationID = strings.TrimSpace(query.InvocationID)
	query.WaitID = strings.TrimSpace(query.WaitID)
	query.ToolName = strings.TrimSpace(query.ToolName)
	query.RequestDigest = strings.TrimSpace(strings.ToLower(query.RequestDigest))
	query.Signature = strings.TrimSpace(strings.ToLower(query.Signature))
	if query.Version != RuntimeLongRunningToolStatusVersion {
		return RuntimeLongRunningToolStatusQuery{}, fmt.Errorf("%w: version %d 不受支持", ErrInvalidRuntimeLongRunningToolStatus, query.Version)
	}
	if query.Source == "" || query.Destination == "" || query.QueryID == "" || query.InvocationID == "" || query.WaitID == "" || query.ToolName == "" || query.RequestDigest == "" || query.IssuedAt.IsZero() {
		return RuntimeLongRunningToolStatusQuery{}, fmt.Errorf("%w: query metadata 不完整", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if len(query.Source) > MaxRuntimeLongRunningToolStatusSourceLength || len(query.Destination) > MaxRuntimeLongRunningToolStatusDestinationLength || len(query.QueryID) > MaxRuntimeLongRunningToolStatusQueryIDLength || len(query.InvocationID) > MaxRuntimeLongRunningToolStatusInvocationLength || len(query.WaitID) > MaxRuntimeLongRunningToolStatusWaitIDLength || len(query.ToolName) > MaxRuntimeLongRunningToolStatusToolNameLength {
		return RuntimeLongRunningToolStatusQuery{}, fmt.Errorf("%w: query metadata 超出长度限制", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if !validRuntimeLongRunningToolStatusDigest(query.RequestDigest) {
		return RuntimeLongRunningToolStatusQuery{}, fmt.Errorf("%w: request_digest 无效", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if query.Signature != "" && !isRuntimeEventDeliveryDigest(query.Signature) {
		return RuntimeLongRunningToolStatusQuery{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeLongRunningToolStatus)
	}
	query.IssuedAt = query.IssuedAt.UTC()
	return query, nil
}

// NormalizeRuntimeLongRunningToolStatusQuery validates and trims a query for
// custom transports and receiver implementations.
func NormalizeRuntimeLongRunningToolStatusQuery(query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatusQuery, error) {
	return normalizeRuntimeLongRunningToolStatusQuery(query)
}

func normalizeRuntimeLongRunningToolStatus(status RuntimeLongRunningToolStatus) (RuntimeLongRunningToolStatus, error) {
	status.Source = strings.TrimSpace(status.Source)
	status.Destination = strings.TrimSpace(status.Destination)
	status.QueryID = strings.TrimSpace(status.QueryID)
	status.InvocationID = strings.TrimSpace(status.InvocationID)
	status.WaitID = strings.TrimSpace(status.WaitID)
	status.ToolName = strings.TrimSpace(status.ToolName)
	status.RequestDigest = strings.TrimSpace(strings.ToLower(status.RequestDigest))
	status.Status = RuntimeLongRunningToolStatusState(strings.TrimSpace(strings.ToLower(string(status.Status))))
	status.ResultDigest = strings.TrimSpace(strings.ToLower(status.ResultDigest))
	status.ErrorCode = sanitizeRuntimeString(strings.TrimSpace(status.ErrorCode))
	status.ErrorMessage = sanitizeRuntimeString(strings.TrimSpace(status.ErrorMessage))
	status.Reason = sanitizeRuntimeString(strings.TrimSpace(status.Reason))
	status.Signature = strings.TrimSpace(strings.ToLower(status.Signature))
	if status.Version != RuntimeLongRunningToolStatusVersion {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: version %d 不受支持", ErrInvalidRuntimeLongRunningToolStatus, status.Version)
	}
	if status.Source == "" || status.Destination == "" || status.QueryID == "" || status.InvocationID == "" || status.WaitID == "" || status.ToolName == "" || status.RequestDigest == "" || status.ObservedAt.IsZero() {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: status metadata 不完整", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if len(status.Source) > MaxRuntimeLongRunningToolStatusSourceLength || len(status.Destination) > MaxRuntimeLongRunningToolStatusDestinationLength || len(status.QueryID) > MaxRuntimeLongRunningToolStatusQueryIDLength || len(status.InvocationID) > MaxRuntimeLongRunningToolStatusInvocationLength || len(status.WaitID) > MaxRuntimeLongRunningToolStatusWaitIDLength || len(status.ToolName) > MaxRuntimeLongRunningToolStatusToolNameLength || len(status.ErrorCode) > MaxRuntimeLongRunningToolStatusCodeLength || len(status.ErrorMessage) > MaxRuntimeLongRunningToolStatusReasonLength || len(status.Reason) > MaxRuntimeLongRunningToolStatusReasonLength {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: status metadata 超出长度限制", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if !validRuntimeLongRunningToolStatusDigest(status.RequestDigest) {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: request_digest 无效", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if status.Signature != "" && !isRuntimeEventDeliveryDigest(status.Signature) {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeLongRunningToolStatus)
	}
	status.ObservedAt = status.ObservedAt.UTC()
	switch status.Status {
	case RuntimeLongRunningToolStatusPending:
		if status.Result != nil || status.ResultAvailable || status.ResultDigest != "" {
			return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: pending status 不能携带 result proof", ErrInvalidRuntimeLongRunningToolStatus)
		}
	case RuntimeLongRunningToolStatusSucceeded:
		if status.Result != nil {
			status.Result = sanitizeRuntimeMap(status.Result)
			encoded, err := json.Marshal(status.Result)
			if err != nil {
				return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: result 无法编码", ErrInvalidRuntimeLongRunningToolStatus)
			}
			if len(encoded) > maxInvocationResumeResponseBytes {
				return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: result 超过 %d 字节上限", ErrInvalidRuntimeLongRunningToolStatus, maxInvocationResumeResponseBytes)
			}
			sum := sha256.Sum256(encoded)
			expectedDigest := "sha256:" + hex.EncodeToString(sum[:])
			if status.ResultDigest == "" {
				status.ResultDigest = expectedDigest
			}
			if !validRuntimeLongRunningToolStatusDigest(status.ResultDigest) {
				return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: result_digest 无效", ErrInvalidRuntimeLongRunningToolStatus)
			}
			if status.ResultDigest != expectedDigest {
				return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: result_digest 与 result 不匹配", ErrRuntimeLongRunningToolStatusAuth)
			}
			status.ResultAvailable = true
		} else {
			if status.ResultDigest == "" {
				return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: succeeded status 缺少 result_digest", ErrInvalidRuntimeLongRunningToolStatus)
			}
			if !validRuntimeLongRunningToolStatusDigest(status.ResultDigest) {
				return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: result_digest 无效", ErrInvalidRuntimeLongRunningToolStatus)
			}
			if status.ResultAvailable {
				return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: result_available=true 但 result 缺失", ErrInvalidRuntimeLongRunningToolStatus)
			}
		}
	case RuntimeLongRunningToolStatusFailed, RuntimeLongRunningToolStatusUnknown:
		if status.Result != nil || status.ResultAvailable || status.ResultDigest != "" {
			return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: %s status 不能携带 result proof", ErrInvalidRuntimeLongRunningToolStatus, status.Status)
		}
	default:
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: status %q 不受支持", ErrInvalidRuntimeLongRunningToolStatus, status.Status)
	}
	return status, nil
}

// NormalizeRuntimeLongRunningToolStatus validates and trims a status proof.
func NormalizeRuntimeLongRunningToolStatus(status RuntimeLongRunningToolStatus) (RuntimeLongRunningToolStatus, error) {
	return normalizeRuntimeLongRunningToolStatus(status)
}

func (status RuntimeLongRunningToolStatus) Validate() error {
	_, err := normalizeRuntimeLongRunningToolStatus(status)
	return err
}

func (query RuntimeLongRunningToolStatusQuery) canonicalUnsigned() ([]byte, error) {
	normalized, err := normalizeRuntimeLongRunningToolStatusQuery(query)
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeLongRunningToolStatusQuery(query RuntimeLongRunningToolStatusQuery, secret []byte) (RuntimeLongRunningToolStatusQuery, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeLongRunningToolStatusQuery{}, fmt.Errorf("%w: %v", ErrInvalidRuntimeLongRunningToolStatus, err)
	}
	canonical, err := query.canonicalUnsigned()
	if err != nil {
		return RuntimeLongRunningToolStatusQuery{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	query.Signature = hex.EncodeToString(mac.Sum(nil))
	return normalizeRuntimeLongRunningToolStatusQuery(query)
}

func VerifyRuntimeLongRunningToolStatusQuery(query RuntimeLongRunningToolStatusQuery, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := normalizeRuntimeLongRunningToolStatusQuery(query)
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeLongRunningToolStatusAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeLongRunningToolStatusAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeLongRunningToolStatusAuth)
	}
	return nil
}

func (status RuntimeLongRunningToolStatus) canonicalUnsigned() ([]byte, error) {
	normalized, err := normalizeRuntimeLongRunningToolStatus(status)
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeLongRunningToolStatus(status RuntimeLongRunningToolStatus, secret []byte) (RuntimeLongRunningToolStatus, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: %v", ErrInvalidRuntimeLongRunningToolStatus, err)
	}
	canonical, err := status.canonicalUnsigned()
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	status.Signature = hex.EncodeToString(mac.Sum(nil))
	return normalizeRuntimeLongRunningToolStatus(status)
}

func VerifyRuntimeLongRunningToolStatus(status RuntimeLongRunningToolStatus, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := normalizeRuntimeLongRunningToolStatus(status)
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeLongRunningToolStatusAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeLongRunningToolStatusAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeLongRunningToolStatusAuth)
	}
	return nil
}

func bindRuntimeLongRunningToolStatus(query RuntimeLongRunningToolStatusQuery, status RuntimeLongRunningToolStatus) (RuntimeLongRunningToolStatus, error) {
	query, err := normalizeRuntimeLongRunningToolStatusQuery(query)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	if status.Version == 0 {
		status.Version = query.Version
	}
	bind := func(value *string, expected, label string) error {
		if strings.TrimSpace(*value) == "" {
			*value = expected
			return nil
		}
		if strings.TrimSpace(*value) != expected {
			return fmt.Errorf("%w: %s 不匹配", ErrRuntimeLongRunningToolStatusTargetMismatch, label)
		}
		*value = strings.TrimSpace(*value)
		return nil
	}
	if status.Version != query.Version {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: version 不匹配", ErrRuntimeLongRunningToolStatusAuth)
	}
	for _, item := range []struct {
		value    *string
		expected string
		label    string
	}{
		{&status.Source, query.Source, "source"}, {&status.Destination, query.Destination, "destination"},
		{&status.QueryID, query.QueryID, "query_id"}, {&status.InvocationID, query.InvocationID, "invocation_id"},
		{&status.WaitID, query.WaitID, "wait_id"}, {&status.ToolName, query.ToolName, "tool_name"}, {&status.RequestDigest, query.RequestDigest, "request_digest"},
	} {
		if err := bind(item.value, item.expected, item.label); err != nil {
			return RuntimeLongRunningToolStatus{}, err
		}
	}
	if status.ObservedAt.IsZero() {
		status.ObservedAt = time.Now().UTC()
	}
	return normalizeRuntimeLongRunningToolStatus(status)
}

// runtimeLongRunningToolRequestDigest creates the stable digest persisted on
// tool.requested events. Internal ToolCall IDs and a prior digest are omitted
// so assigning local audit IDs does not alter the external request identity.
func runtimeLongRunningToolRequestDigest(invocationID string, data map[string]any) (string, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return "", fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidRuntimeLongRunningToolStatus)
	}
	canonicalData := make(map[string]any, len(data))
	for key, value := range data {
		if key == "request_digest" || key == "tool_call_id" {
			continue
		}
		canonicalData[key] = sanitizeRuntimeValue(value)
	}
	payload := struct {
		InvocationID string         `json:"invocation_id"`
		Request      map[string]any `json:"request"`
	}{InvocationID: invocationID, Request: canonicalData}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("%w: request digest 编码失败", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if len(encoded) > MaxRuntimeLongRunningToolStatusRequestBytes {
		return "", fmt.Errorf("%w: request metadata 超过 %d 字节上限", ErrInvalidRuntimeLongRunningToolStatus, MaxRuntimeLongRunningToolStatusRequestBytes)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// QueryLongRunningToolStatus is the explicit status reconciliation boundary.
// It only reads the current waiting boundary and delegates to the configured
// resolver. Pending, unknown, unavailable and even succeeded proofs never
// mutate Invocation state or automatically submit a FunctionResponse.
func (c *Coordinator) QueryLongRunningToolStatus(ctx context.Context, invocationID, waitID string) (RuntimeLongRunningToolStatus, error) {
	if c == nil || c.repo == nil {
		return RuntimeLongRunningToolStatus{}, errors.New("Runtime Coordinator 不能为空")
	}
	if err := c.ensureStarted(); err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	invocationID = strings.TrimSpace(invocationID)
	waitID = strings.TrimSpace(waitID)
	if invocationID == "" || waitID == "" {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: invocation_id/wait_id 不能为空", ErrInvalidRuntimeLongRunningToolStatus)
	}
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	if invocation.Status != InvocationWaitingTool {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: invocation 当前状态为 %s", ErrResumeMismatch, invocation.Status)
	}
	events, err := c.listAllInvocationEvents(ctx, invocationID, maxRuntimeEventReplay)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	names, err := waitingToolBoundaryFromEvents(events)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	toolName, ok := names[waitID]
	if !ok || strings.TrimSpace(toolName) == "" {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: wait_id=%s 不属于当前等待边界", ErrRuntimeLongRunningToolStatusTargetMismatch, waitID)
	}
	var requestEvent *AgentEvent
	for index := range events {
		event := events[index]
		if event.Type != EventToolRequested || event.Data == nil {
			continue
		}
		callID, _ := event.Data["call_id"].(string)
		if strings.TrimSpace(callID) != waitID {
			continue
		}
		candidate := event
		requestEvent = &candidate
	}
	if requestEvent == nil {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: wait_id=%s 缺少持久化 tool.requested", ErrRuntimeLongRunningToolStatusTargetMismatch, waitID)
	}
	requestDigest, _ := requestEvent.Data["request_digest"].(string)
	if !validRuntimeLongRunningToolStatusDigest(requestDigest) {
		requestDigest, err = runtimeLongRunningToolRequestDigest(invocationID, requestEvent.Data)
		if err != nil {
			return RuntimeLongRunningToolStatus{}, err
		}
	}
	c.mu.Lock()
	resolver := c.toolStatusResolver
	source := strings.TrimSpace(c.toolStatusSource)
	destination := strings.TrimSpace(c.toolStatusDestination)
	c.mu.Unlock()
	if resolver == nil {
		return RuntimeLongRunningToolStatus{}, ErrRuntimeLongRunningToolStatusUnavailable
	}
	if source == "" {
		source = DefaultRuntimeLongRunningToolStatusSource
	}
	if destination == "" {
		destination = DefaultRuntimeLongRunningToolStatusDestination
	}
	query := RuntimeLongRunningToolStatusQuery{
		Version: RuntimeLongRunningToolStatusVersion, Source: source, Destination: destination,
		QueryID: newID("tool-status-query"), InvocationID: invocationID, WaitID: waitID,
		ToolName: toolName, RequestDigest: requestDigest, IssuedAt: time.Now().UTC(),
	}
	query, err = normalizeRuntimeLongRunningToolStatusQuery(query)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	status, err := resolver.QueryLongRunningToolStatus(ctx, query)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	status, err = bindRuntimeLongRunningToolStatus(query, status)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	return status, nil
}

// runtimeLongRunningToolStatusResumeIdempotencyKey derives a stable key from
// the immutable wait/request identity and the proved result digest. Query IDs
// are intentionally excluded: a retry may issue a fresh status query, but the
// same external result must still map to one Runtime resume attempt.
func runtimeLongRunningToolStatusResumeIdempotencyKey(invocationID, waitID, requestDigest, resultDigest string) string {
	material := strings.TrimSpace(invocationID) + "\x00" + strings.TrimSpace(waitID) + "\x00" + strings.TrimSpace(requestDigest) + "\x00" + strings.TrimSpace(resultDigest)
	sum := sha256.Sum256([]byte(material))
	return "tool-status-resume-" + hex.EncodeToString(sum[:16])
}

// ReconcileLongRunningToolStatus is an explicit, caller-triggered safe
// recovery boundary. Only a succeeded proof that includes a bounded result
// body can be submitted to ResumeInvocation. Pending, failed, unknown and
// digest-only succeeded proofs remain read-only; in particular unknown never
// causes a blind replay of an external side effect. A waiting boundary with
// more than one pending wait is also rejected by ResumeInvocation, so this
// first slice cannot partially resume a multi-call boundary.
func (c *Coordinator) ReconcileLongRunningToolStatus(ctx context.Context, invocationID, waitID string) (RuntimeLongRunningToolStatusReconcileResult, error) {
	status, err := c.QueryLongRunningToolStatus(ctx, invocationID, waitID)
	if err != nil {
		return RuntimeLongRunningToolStatusReconcileResult{}, err
	}
	result := RuntimeLongRunningToolStatusReconcileResult{Status: status}
	switch status.Status {
	case RuntimeLongRunningToolStatusPending:
		result.Reason = "pending_no_resume"
		return result, nil
	case RuntimeLongRunningToolStatusFailed:
		result.Reason = "failed_requires_policy"
		return result, nil
	case RuntimeLongRunningToolStatusUnknown:
		result.Reason = "unknown_requires_manual_recovery"
		return result, nil
	case RuntimeLongRunningToolStatusSucceeded:
		if !status.ResultAvailable || status.Result == nil {
			result.Reason = "result_digest_only"
			return result, nil
		}
	default:
		return RuntimeLongRunningToolStatusReconcileResult{}, fmt.Errorf("%w: status %q 不受支持", ErrInvalidRuntimeLongRunningToolStatus, status.Status)
	}

	idempotencyKey := runtimeLongRunningToolStatusResumeIdempotencyKey(invocationID, waitID, status.RequestDigest, status.ResultDigest)
	resumeRequest := InvocationResumeRequest{
		WaitID: waitID, Name: status.ToolName, Response: status.Result, IdempotencyKey: idempotencyKey,
		statusQueryIDs: []string{status.QueryID}, statusResultDigest: []string{status.ResultDigest},
	}
	invocation, err := c.ResumeInvocation(ctx, invocationID, resumeRequest)
	if err != nil {
		return RuntimeLongRunningToolStatusReconcileResult{}, err
	}
	result.Resumed = true
	result.Invocation = &invocation
	result.Reason = "succeeded_result_submitted"
	return result, nil
}

// runtimeLongRunningToolStatusBatchResumeIdempotencyKey derives one stable
// idempotency key for the complete proof set. Sorting only affects the key;
// the original wait_ids order is retained for the ADK response parts.
func runtimeLongRunningToolStatusBatchResumeIdempotencyKey(invocationID string, statuses []RuntimeLongRunningToolStatus) string {
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, strings.TrimSpace(status.WaitID)+"\x00"+strings.TrimSpace(status.RequestDigest)+"\x00"+strings.TrimSpace(status.ResultDigest))
	}
	sort.Strings(parts)
	material := strings.TrimSpace(invocationID) + "\x00" + strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "tool-status-batch-resume-" + hex.EncodeToString(sum[:16])
}

// ReconcileLongRunningToolStatuses performs an all-or-nothing explicit
// reconciliation for a multi-wait boundary. Every supplied wait ID must
// resolve to a succeeded proof with a bounded result body; otherwise no
// FunctionResponse is submitted. This prevents a partially observed external
// boundary from advancing an ADK Session with an incomplete response set.
func (c *Coordinator) ReconcileLongRunningToolStatuses(ctx context.Context, invocationID string, waitIDs []string) (RuntimeLongRunningToolStatusBatchReconcileResult, error) {
	if len(waitIDs) == 0 || len(waitIDs) > maxInvocationResumeItems {
		return RuntimeLongRunningToolStatusBatchReconcileResult{}, fmt.Errorf("%w: wait_ids 数量必须在 1-%d 之间", ErrInvalidRuntimeLongRunningToolStatus, maxInvocationResumeItems)
	}
	seen := make(map[string]struct{}, len(waitIDs))
	normalizedIDs := make([]string, 0, len(waitIDs))
	for _, waitID := range waitIDs {
		waitID = strings.TrimSpace(waitID)
		if waitID == "" {
			return RuntimeLongRunningToolStatusBatchReconcileResult{}, fmt.Errorf("%w: wait_id 不能为空", ErrInvalidRuntimeLongRunningToolStatus)
		}
		if _, exists := seen[waitID]; exists {
			return RuntimeLongRunningToolStatusBatchReconcileResult{}, fmt.Errorf("%w: wait_id=%s 重复", ErrInvalidRuntimeLongRunningToolStatus, waitID)
		}
		seen[waitID] = struct{}{}
		normalizedIDs = append(normalizedIDs, waitID)
	}
	result := RuntimeLongRunningToolStatusBatchReconcileResult{Statuses: make([]RuntimeLongRunningToolStatus, 0, len(normalizedIDs))}
	for _, waitID := range normalizedIDs {
		status, err := c.QueryLongRunningToolStatus(ctx, invocationID, waitID)
		if err != nil {
			return result, err
		}
		result.Statuses = append(result.Statuses, status)
	}
	allSucceeded := true
	allResultsAvailable := true
	responses := make([]InvocationResumeItem, 0, len(result.Statuses))
	for _, status := range result.Statuses {
		if status.Status != RuntimeLongRunningToolStatusSucceeded {
			allSucceeded = false
		}
		if !status.ResultAvailable || status.Result == nil {
			allResultsAvailable = false
			continue
		}
		responses = append(responses, InvocationResumeItem{WaitID: status.WaitID, Name: status.ToolName, Response: status.Result})
	}
	if !allSucceeded {
		result.Reason = "unproven_status_no_resume"
		return result, nil
	}
	if !allResultsAvailable || len(responses) != len(result.Statuses) {
		result.Reason = "result_digest_only"
		return result, nil
	}
	queryIDs := make([]string, 0, len(result.Statuses))
	resultDigests := make([]string, 0, len(result.Statuses))
	for _, status := range result.Statuses {
		queryIDs = append(queryIDs, status.QueryID)
		resultDigests = append(resultDigests, status.ResultDigest)
	}
	resumeRequest := InvocationResumeRequest{
		Responses: responses, IdempotencyKey: runtimeLongRunningToolStatusBatchResumeIdempotencyKey(invocationID, result.Statuses),
		statusQueryIDs: queryIDs, statusResultDigest: resultDigests,
	}
	invocation, err := c.ResumeInvocation(ctx, invocationID, resumeRequest)
	if err != nil {
		return result, err
	}
	result.Resumed = true
	result.Invocation = &invocation
	result.Reason = "succeeded_results_submitted"
	return result, nil
}

// SetRuntimeLongRunningToolStatusResolver installs or clears the explicit
// external status adapter. A transport may expose its signed route through
// RuntimeLongRunningToolStatusRouteProvider.
func (c *Coordinator) SetRuntimeLongRunningToolStatusResolver(resolver RuntimeLongRunningToolStatusResolver) {
	if c == nil {
		return
	}
	source, destination := "", ""
	if route, ok := resolver.(RuntimeLongRunningToolStatusRouteProvider); ok {
		source, destination = route.RuntimeLongRunningToolStatusRoute()
	}
	c.mu.Lock()
	c.toolStatusResolver = resolver
	if strings.TrimSpace(source) != "" {
		c.toolStatusSource = strings.TrimSpace(source)
	}
	if strings.TrimSpace(destination) != "" {
		c.toolStatusDestination = strings.TrimSpace(destination)
	}
	c.mu.Unlock()
}

// SetRuntimeLongRunningToolStatusRoute changes the signed route used for
// subsequent queries. It does not alter an already-issued query.
func (c *Coordinator) SetRuntimeLongRunningToolStatusRoute(source, destination string) error {
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	if source == "" || destination == "" || len(source) > MaxRuntimeLongRunningToolStatusSourceLength || len(destination) > MaxRuntimeLongRunningToolStatusDestinationLength {
		return fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if c == nil {
		return errors.New("Runtime Coordinator 不能为空")
	}
	c.mu.Lock()
	c.toolStatusSource, c.toolStatusDestination = source, destination
	c.mu.Unlock()
	return nil
}

// RuntimeLongRunningToolStatusLookup is the receiver-side read-only lookup.
type RuntimeLongRunningToolStatusLookup func(context.Context, RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error)

// RuntimeLongRunningToolStatusReceiver exposes the authenticated status
// endpoint for a Tool Runtime. It never executes or replays a tool.
type RuntimeLongRunningToolStatusReceiver struct {
	Source        string
	Destination   string
	SharedSecret  []byte
	Lookup        RuntimeLongRunningToolStatusLookup
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	MaxBodyBytes  int64
	Now           func() time.Time
}

func NewRuntimeLongRunningToolStatusReceiver(source, destination string, secret []byte, lookup RuntimeLongRunningToolStatusLookup) (*RuntimeLongRunningToolStatusReceiver, error) {
	source = strings.TrimSpace(source)
	destination = strings.TrimSpace(destination)
	if source == "" || destination == "" || len(source) > MaxRuntimeLongRunningToolStatusSourceLength || len(destination) > MaxRuntimeLongRunningToolStatusDestinationLength {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRuntimeLongRunningToolStatus, err)
	}
	if lookup == nil {
		return nil, fmt.Errorf("%w: lookup 不能为空", ErrInvalidRuntimeLongRunningToolStatus)
	}
	return &RuntimeLongRunningToolStatusReceiver{Source: source, Destination: destination, SharedSecret: append([]byte(nil), secret...), Lookup: lookup, MaxAge: DefaultRuntimeEventDeliveryMaxAge, MaxFutureSkew: MaxRuntimeLongRunningToolStatusFutureSkew, MaxBodyBytes: MaxRuntimeLongRunningToolStatusBodyBytes, Now: time.Now}, nil
}

func (r *RuntimeLongRunningToolStatusReceiver) Handler() http.Handler { return r }

func (r *RuntimeLongRunningToolStatusReceiver) RuntimeLongRunningToolStatusRoute() (string, string) {
	if r == nil {
		return "", ""
	}
	return r.Source, r.Destination
}

func (r *RuntimeLongRunningToolStatusReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusInternalServerError, ErrInvalidRuntimeLongRunningToolStatus)
		return
	}
	if request == nil || request.Method != http.MethodPost {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusMethodNotAllowed, fmt.Errorf("%w: 只支持 POST", ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	maxBody := r.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeLongRunningToolStatusBodyBytes {
		maxBody = MaxRuntimeLongRunningToolStatusBodyBytes
	}
	if request.Body == nil {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusBadRequest, fmt.Errorf("%w: body 为空", ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusRequestEntityTooLarge, fmt.Errorf("%w: body 超过上限", ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var query RuntimeLongRunningToolStatusQuery
	if err := decoder.Decode(&query); err != nil {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusBadRequest, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusBadRequest, fmt.Errorf("%w: 请求包含多个 JSON 文档", ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	query, err = normalizeRuntimeLongRunningToolStatusQuery(query)
	if err != nil {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusBadRequest, err)
		return
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Tool-Status-Version")); header != "" && header != strconv.Itoa(query.Version) {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusBadRequest, fmt.Errorf("%w: header version 与 body 不一致", ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Tool-Status-Signature"))); header != "" && header != query.Signature {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusUnauthorized, fmt.Errorf("%w: header signature 与 body 不一致", ErrRuntimeLongRunningToolStatusAuth))
		return
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != query.QueryID {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusBadRequest, fmt.Errorf("%w: idempotency key 与 query_id 不一致", ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	if query.Source != r.Source || query.Destination != r.Destination {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusForbidden, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeLongRunningToolStatusAuth))
		return
	}
	if err := VerifyRuntimeLongRunningToolStatusQuery(query, r.SharedSecret); err != nil {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusUnauthorized, err)
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	maxAge, futureSkew := r.MaxAge, r.MaxFutureSkew
	if maxAge <= 0 {
		maxAge = DefaultRuntimeEventDeliveryMaxAge
	}
	if futureSkew <= 0 {
		futureSkew = MaxRuntimeLongRunningToolStatusFutureSkew
	}
	if err := validateRuntimeEventDeliveryTimestamp(query.IssuedAt, now, maxAge, futureSkew); err != nil {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusUnauthorized, err)
		return
	}
	status, err := r.Lookup(request.Context(), query)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, ErrRuntimeLongRunningToolStatusUnavailable) {
			code = http.StatusNotImplemented
		}
		writeRuntimeLongRunningToolStatusError(writer, code, err)
		return
	}
	status, err = bindRuntimeLongRunningToolStatus(query, status)
	if err != nil {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusConflict, err)
		return
	}
	signed, err := SignRuntimeLongRunningToolStatus(status, r.SharedSecret)
	if err != nil {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusInternalServerError, err)
		return
	}
	encoded, err := json.Marshal(signed)
	if err != nil || len(encoded) > MaxRuntimeLongRunningToolStatusBodyBytes {
		writeRuntimeLongRunningToolStatusError(writer, http.StatusInternalServerError, fmt.Errorf("%w: response 超过上限", ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	writeRuntimeLongRunningToolStatusJSON(writer, http.StatusOK, signed)
}

// RuntimeLongRunningToolStatusHTTPTransport is the signed client adapter for
// a remote Tool Runtime status endpoint.
type RuntimeLongRunningToolStatusHTTPTransport struct {
	Endpoint      string
	Source        string
	Destination   string
	SharedSecret  []byte
	Client        *http.Client
	MaxBodyBytes  int64
	MaxFutureSkew time.Duration
	Now           func() time.Time
}

func (t *RuntimeLongRunningToolStatusHTTPTransport) RuntimeLongRunningToolStatusRoute() (string, string) {
	if t == nil {
		return "", ""
	}
	return strings.TrimSpace(t.Source), strings.TrimSpace(t.Destination)
}

func (t *RuntimeLongRunningToolStatusHTTPTransport) QueryLongRunningToolStatus(ctx context.Context, query RuntimeLongRunningToolStatusQuery) (RuntimeLongRunningToolStatus, error) {
	if t == nil {
		return RuntimeLongRunningToolStatus{}, ErrRuntimeLongRunningToolStatusUnavailable
	}
	query.Source = strings.TrimSpace(query.Source)
	query.Destination = strings.TrimSpace(query.Destination)
	if query.Source == "" {
		query.Source = strings.TrimSpace(t.Source)
	}
	if query.Destination == "" {
		query.Destination = strings.TrimSpace(t.Destination)
	}
	if query.Source != strings.TrimSpace(t.Source) || query.Destination != strings.TrimSpace(t.Destination) {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: transport route 与 query 不匹配", ErrRuntimeLongRunningToolStatusAuth)
	}
	if query.IssuedAt.IsZero() {
		query.IssuedAt = time.Now().UTC()
		if t.Now != nil {
			query.IssuedAt = t.Now().UTC()
		}
	}
	query, err := normalizeRuntimeLongRunningToolStatusQuery(query)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	signedQuery, err := SignRuntimeLongRunningToolStatusQuery(query, t.SharedSecret)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	body, err := json.Marshal(signedQuery)
	if err != nil || len(body) > MaxRuntimeLongRunningToolStatusBodyBytes {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: query body 超过上限", ErrInvalidRuntimeLongRunningToolStatus)
	}
	endpoint, err := runtimeLongRunningToolStatusEndpoint(t.Endpoint)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Tool-Status-Version", strconv.Itoa(signedQuery.Version))
	request.Header.Set("X-Abot-Tool-Status-Signature", signedQuery.Signature)
	request.Header.Set("Idempotency-Key", signedQuery.QueryID)
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("Runtime tool status 请求失败: %w", err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeLongRunningToolStatusBodyBytes {
		maxBody = MaxRuntimeLongRunningToolStatusBodyBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: response 超过上限或读取失败", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
			return RuntimeLongRunningToolStatus{}, ErrRuntimeLongRunningToolStatusUnavailable
		}
		message := sanitizeRuntimeString(strings.TrimSpace(string(responseBody)))
		if len(message) > 512 {
			message = message[:512]
		}
		if message == "" {
			message = "tool runtime returned no diagnostic"
		}
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("Runtime tool status 返回 HTTP %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var status RuntimeLongRunningToolStatus
	if err := decoder.Decode(&status); err != nil {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: status JSON 无效", ErrInvalidRuntimeLongRunningToolStatus)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: status 包含多个 JSON 文档", ErrInvalidRuntimeLongRunningToolStatus)
	}
	if err := VerifyRuntimeLongRunningToolStatus(status, t.SharedSecret); err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	status, err = bindRuntimeLongRunningToolStatus(signedQuery, status)
	if err != nil {
		return RuntimeLongRunningToolStatus{}, err
	}
	now := time.Now().UTC()
	if t.Now != nil {
		now = t.Now().UTC()
	}
	futureSkew := t.MaxFutureSkew
	if futureSkew <= 0 {
		futureSkew = MaxRuntimeLongRunningToolStatusFutureSkew
	}
	if status.ObservedAt.After(now.Add(futureSkew)) {
		return RuntimeLongRunningToolStatus{}, fmt.Errorf("%w: observed_at 超出未来时间窗", ErrRuntimeLongRunningToolStatusAuth)
	}
	return status, nil
}

func runtimeLongRunningToolStatusEndpoint(base string) (string, error) {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeLongRunningToolStatus)
	}
	return u.String(), nil
}

func writeRuntimeLongRunningToolStatusJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeRuntimeLongRunningToolStatusError(writer http.ResponseWriter, status int, err error) {
	message := "Runtime tool status 请求失败"
	if err != nil {
		message = sanitizeRuntimeString(err.Error())
	}
	writeRuntimeLongRunningToolStatusJSON(writer, status, map[string]string{"error": message})
}
