package runtime

// This file defines the opt-in cross-Runtime configuration-directory body
// protocol.  It is intentionally separate from config_delivery.go: the older
// protocol carries only a metadata snapshot lock, while this protocol carries
// one bounded, public profile/persona entry.  Provider credentials, prompt
// requests, tool arguments and bindings never cross this boundary.

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
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"Abot/internal/config"
)

const (
	RuntimeConfigDirectoryEnvelopeVersion = 1
	RuntimeConfigDirectoryEntryVersion    = 1

	RuntimeConfigDirectoryKindProfile RuntimeConfigDirectoryKind = "profile"
	RuntimeConfigDirectoryKindPersona RuntimeConfigDirectoryKind = "persona"
	// Short aliases make the public protocol pleasant to use without changing
	// the wire values.
	RuntimeConfigDirectoryProfileKind = RuntimeConfigDirectoryKindProfile
	RuntimeConfigDirectoryPersonaKind = RuntimeConfigDirectoryKindPersona

	MaxRuntimeConfigDirectoryEnvelopeBytes  = 64 << 10
	MaxRuntimeConfigDirectoryEntryBytes     = 256 << 10
	MaxRuntimeConfigDirectoryResponseBytes  = 32 << 10
	MaxRuntimeConfigDirectoryIDLength       = 128
	MaxRuntimeConfigDirectoryNameLength     = 200
	MaxRuntimeConfigDirectoryDescription    = 2 << 10
	MaxRuntimeConfigDirectoryInstruction    = 64 << 10
	MaxRuntimeConfigDirectorySourceLength   = MaxRuntimeEventDeliverySourceLength
	MaxRuntimeConfigDirectoryTargetLength   = MaxRuntimeEventDeliveryTargetLength
	MaxRuntimeConfigDirectoryDeliveryID     = MaxRuntimeEventDeliveryIDLength
	MaxRuntimeConfigDirectoryCorrelation    = 512
	MaxRuntimeConfigDirectoryIdempotency    = 200
	DefaultRuntimeConfigDirectoryMaxAge     = 10 * time.Minute
	DefaultRuntimeConfigDirectoryFutureSkew = 30 * time.Second
)

var (
	ErrInvalidRuntimeConfigDirectory     = errors.New("Runtime 配置目录投递无效")
	ErrRuntimeConfigDirectoryAuth        = errors.New("Runtime 配置目录投递认证失败")
	ErrRuntimeConfigDirectoryStale       = errors.New("Runtime 配置目录投递已过期或版本过旧")
	ErrRuntimeConfigDirectoryConflict    = errors.New("Runtime 配置目录投递冲突")
	ErrRuntimeConfigDirectoryUnavailable = errors.New("Runtime 配置目录投递不可用")
)

// RuntimeConfigDirectoryKind identifies the independently versioned catalog
// entry carried by an envelope.
type RuntimeConfigDirectoryKind string

func (k RuntimeConfigDirectoryKind) valid() bool {
	return k == RuntimeConfigDirectoryKindProfile || k == RuntimeConfigDirectoryKindPersona
}

// RuntimeConfigDirectoryEntry is a public, bounded catalog entry.  IsDefault
// and bindings are deliberately absent: importing content must not silently
// change destination selection.  Enabled is meaningful for personas; profile
// entries leave it nil.
type RuntimeConfigDirectoryEntry struct {
	Version     int                        `json:"version"`
	Kind        RuntimeConfigDirectoryKind `json:"kind"`
	ID          string                     `json:"id"`
	Revision    int                        `json:"revision"`
	Name        string                     `json:"name"`
	Description string                     `json:"description,omitempty"`
	Instruction string                     `json:"instruction,omitempty"`
	Values      map[string]any             `json:"values,omitempty"`
	Enabled     *bool                      `json:"enabled,omitempty"`
}

// RuntimeConfigDirectoryRecord is the destination's latest durable copy of
// one kind/id pair.  Envelope metadata is retained for audit, but callers
// should treat Entry as the only usable catalog content.
type RuntimeConfigDirectoryRecord struct {
	Envelope   RuntimeConfigDirectoryEnvelope `json:"envelope"`
	Entry      RuntimeConfigDirectoryEntry    `json:"entry"`
	ReceivedAt time.Time                      `json:"received_at"`
}

// RuntimeConfigDirectoryEnvelope binds a body to source/destination and a
// monotonic revision. IssuedAt is refreshed for retries; Timestamp remains the
// immutable publication time used by the delivery identity.
type RuntimeConfigDirectoryEnvelope struct {
	Version        int                        `json:"version"`
	Source         string                     `json:"source"`
	Destination    string                     `json:"destination"`
	DeliveryID     string                     `json:"delivery_id"`
	Kind           RuntimeConfigDirectoryKind `json:"kind"`
	EntryID        string                     `json:"entry_id"`
	Revision       int                        `json:"revision"`
	PreviousDigest string                     `json:"previous_digest,omitempty"`
	BodyDigest     string                     `json:"body_digest"`
	CorrelationID  string                     `json:"correlation_id,omitempty"`
	IdempotencyKey string                     `json:"idempotency_key,omitempty"`
	Timestamp      time.Time                  `json:"timestamp"`
	IssuedAt       time.Time                  `json:"issued_at"`
	Signature      string                     `json:"signature,omitempty"`
}

type RuntimeConfigDirectoryDelivery struct {
	Envelope RuntimeConfigDirectoryEnvelope `json:"envelope"`
	Entry    RuntimeConfigDirectoryEntry    `json:"entry"`
}

// RuntimeConfigDirectoryReceipt is signed so a source cannot mistake an
// intermediary's successful HTTP response for destination acceptance.
type RuntimeConfigDirectoryReceipt struct {
	Version     int                        `json:"version"`
	Source      string                     `json:"source"`
	Destination string                     `json:"destination"`
	DeliveryID  string                     `json:"delivery_id"`
	Kind        RuntimeConfigDirectoryKind `json:"kind"`
	EntryID     string                     `json:"entry_id"`
	Revision    int                        `json:"revision"`
	BodyDigest  string                     `json:"body_digest"`
	Duplicate   bool                       `json:"duplicate"`
	ReceivedAt  time.Time                  `json:"received_at"`
	Signature   string                     `json:"signature,omitempty"`
}

// RuntimeConfigDirectoryRepository is the durable destination boundary.  An
// implementation must apply the same monotonic rules as Memory/SQLite:
// duplicate identity is idempotent, lower revisions are stale, same-revision
// forks conflict, and a higher revision must name the currently visible body
// digest in PreviousDigest.
type RuntimeConfigDirectoryRepository interface {
	AcceptRuntimeConfigDirectory(context.Context, RuntimeConfigDirectoryEnvelope, RuntimeConfigDirectoryEntry) (duplicate bool, err error)
	GetRuntimeConfigDirectory(context.Context, RuntimeConfigDirectoryKind, string) (RuntimeConfigDirectoryRecord, error)
}

// RuntimeConfigDirectoryTransport is an explicit source-side push adapter.
// It is not installed by default and carries one catalog entry per request.
type RuntimeConfigDirectoryTransport interface {
	Deliver(context.Context, RuntimeConfigDirectoryEnvelope, RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryReceipt, error)
}

func cloneRuntimeConfigDirectoryMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = cloneRuntimeConfigDirectoryValue(value)
	}
	return result
}

func cloneRuntimeConfigDirectoryValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return cloneRuntimeConfigDirectoryMap(item)
	case []any:
		result := make([]any, len(item))
		for index := range item {
			result[index] = cloneRuntimeConfigDirectoryValue(item[index])
		}
		return result
	default:
		// Config values are normally map[string]any/[]any, but embedders may
		// provide typed map aliases or json.RawMessage. A JSON round-trip gives
		// those values a defensive copy and lets Normalize's canonical scan
		// enforce the same secret/type policy.
		encoded, err := json.Marshal(value)
		if err != nil {
			return value
		}
		var copied any
		if err := json.Unmarshal(encoded, &copied); err != nil {
			return value
		}
		return copied
	}
}

func cloneRuntimeConfigDirectoryEntry(entry RuntimeConfigDirectoryEntry) RuntimeConfigDirectoryEntry {
	entry.Values = cloneRuntimeConfigDirectoryMap(entry.Values)
	if entry.Enabled != nil {
		value := *entry.Enabled
		entry.Enabled = &value
	}
	return entry
}

func cloneRuntimeConfigDirectoryEnvelope(envelope RuntimeConfigDirectoryEnvelope) RuntimeConfigDirectoryEnvelope {
	return envelope
}

func cloneRuntimeConfigDirectoryRecord(record RuntimeConfigDirectoryRecord) RuntimeConfigDirectoryRecord {
	record.Envelope = cloneRuntimeConfigDirectoryEnvelope(record.Envelope)
	record.Entry = cloneRuntimeConfigDirectoryEntry(record.Entry)
	return record
}

// NewRuntimeConfigDirectoryProfileEntry converts the config service's public
// Profile object into a body suitable for transport. It still rejects secret-
// shaped keys rather than silently guessing which values are safe.
func NewRuntimeConfigDirectoryProfileEntry(profile config.Profile) (RuntimeConfigDirectoryEntry, error) {
	entry := RuntimeConfigDirectoryEntry{
		Version: RuntimeConfigDirectoryEntryVersion, Kind: RuntimeConfigDirectoryKindProfile,
		ID: profile.ID, Revision: profile.Revision, Name: profile.Name, Values: cloneRuntimeConfigDirectoryMap(profile.Values),
	}
	return entry.Normalize()
}

// NewRuntimeConfigDirectoryPersonaEntry converts a persona catalog object.
// Default selection and bot/conversation bindings are intentionally omitted.
func NewRuntimeConfigDirectoryPersonaEntry(persona config.Persona) (RuntimeConfigDirectoryEntry, error) {
	enabled := persona.Enabled
	entry := RuntimeConfigDirectoryEntry{
		Version: RuntimeConfigDirectoryEntryVersion, Kind: RuntimeConfigDirectoryKindPersona,
		ID: persona.ID, Revision: persona.Revision, Name: persona.Name,
		Description: persona.Description, Instruction: persona.Instruction, Enabled: &enabled,
	}
	return entry.Normalize()
}

func validateRuntimeConfigDirectoryText(value string, max int, label string, required bool) error {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return fmt.Errorf("%w: %s 不能为空", ErrInvalidRuntimeConfigDirectory, label)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s 必须是有效 UTF-8", ErrInvalidRuntimeConfigDirectory, label)
	}
	if len(value) > max {
		return fmt.Errorf("%w: %s 不能超过 %d 个字节", ErrInvalidRuntimeConfigDirectory, label, max)
	}
	return nil
}

func validateRuntimeConfigDirectoryID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > MaxRuntimeConfigDirectoryIDLength || !utf8.ValidString(id) {
		return fmt.Errorf("%w: entry id 无效", ErrInvalidRuntimeConfigDirectory)
	}
	for _, value := range id {
		if value == '/' || value == '\\' || value < 0x20 || value == 0x7f {
			return fmt.Errorf("%w: entry id 含有非法字符", ErrInvalidRuntimeConfigDirectory)
		}
	}
	return nil
}

var runtimeConfigDirectorySecretKeyWords = map[string]struct{}{
	"api_key": {}, "apikey": {}, "password": {}, "passwd": {}, "secret": {},
	"private_key": {}, "privatekey": {}, "credential": {}, "credentials": {},
	"cookie": {}, "authorization": {}, "access_token": {}, "accesstoken": {},
	"refresh_token": {}, "refreshtoken": {}, "auth_token": {}, "authtoken": {},
	"token": {},
}

func runtimeConfigDirectorySecretKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if _, ok := runtimeConfigDirectorySecretKeyWords[lower]; ok {
		return true
	}
	// Treat punctuation as segment separators so nested keys such as
	// "provider.api-key" cannot evade the deny-list. Do not reject ordinary
	// policy keys containing "tokens" (for example max_output_tokens).
	segments := strings.FieldsFunc(lower, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ':' || r == '/' || r == ' '
	})
	for _, segment := range segments {
		if segment == "password" || segment == "passwd" || segment == "secret" || segment == "credential" || segment == "credentials" || segment == "cookie" || segment == "authorization" || segment == "key" {
			return true
		}
	}
	for index := 0; index+1 < len(segments); index++ {
		if segments[index] == "api" && segments[index+1] == "key" {
			return true
		}
	}
	return false
}

func validateRuntimeConfigDirectoryValue(value any, path string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("%w: values nesting 超过 32 层", ErrInvalidRuntimeConfigDirectory)
	}
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		if rv.Kind() == reflect.Float32 || rv.Kind() == reflect.Float64 {
			value := rv.Float()
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("%w: values %s 包含非有限数字", ErrInvalidRuntimeConfigDirectory, path)
			}
		}
		return nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("%w: values %s 的 map key 必须是字符串", ErrInvalidRuntimeConfigDirectory, path)
		}
		iter := rv.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			if runtimeConfigDirectorySecretKey(key) {
				return fmt.Errorf("%w: values %s.%s 疑似敏感字段，拒绝跨 Runtime 传输", ErrInvalidRuntimeConfigDirectory, path, key)
			}
			if err := validateRuntimeConfigDirectoryValue(iter.Value().Interface(), path+"."+key, depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice, reflect.Array:
		for index := 0; index < rv.Len(); index++ {
			if err := validateRuntimeConfigDirectoryValue(rv.Index(index).Interface(), fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: values %s 类型 %s 不受支持", ErrInvalidRuntimeConfigDirectory, path, rv.Type())
	}
}

func validateRuntimeConfigDirectoryJSON(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%w: canonical JSON 无效", ErrInvalidRuntimeConfigDirectory)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%w: canonical JSON 包含多个文档", ErrInvalidRuntimeConfigDirectory)
	}
	return validateRuntimeConfigDirectoryValue(value, "entry", 0)
}

func (entry RuntimeConfigDirectoryEntry) Normalize() (RuntimeConfigDirectoryEntry, error) {
	entry.Kind = RuntimeConfigDirectoryKind(strings.TrimSpace(strings.ToLower(string(entry.Kind))))
	entry.ID = strings.TrimSpace(entry.ID)
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Description = strings.TrimSpace(entry.Description)
	entry.Instruction = strings.TrimSpace(entry.Instruction)
	if entry.Version != RuntimeConfigDirectoryEntryVersion || !entry.Kind.valid() {
		return RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: entry version/kind 不受支持", ErrInvalidRuntimeConfigDirectory)
	}
	if err := validateRuntimeConfigDirectoryID(entry.ID); err != nil {
		return RuntimeConfigDirectoryEntry{}, err
	}
	if entry.Revision <= 0 {
		return RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: revision 必须为正数", ErrInvalidRuntimeConfigDirectory)
	}
	if err := validateRuntimeConfigDirectoryText(entry.Name, MaxRuntimeConfigDirectoryNameLength, "name", true); err != nil {
		return RuntimeConfigDirectoryEntry{}, err
	}
	if err := validateRuntimeConfigDirectoryText(entry.Description, MaxRuntimeConfigDirectoryDescription, "description", false); err != nil {
		return RuntimeConfigDirectoryEntry{}, err
	}
	switch entry.Kind {
	case RuntimeConfigDirectoryKindProfile:
		if entry.Description != "" || entry.Instruction != "" || entry.Enabled != nil {
			return RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: profile entry 不允许 description/instruction/enabled", ErrInvalidRuntimeConfigDirectory)
		}
		if entry.Values == nil {
			entry.Values = map[string]any{}
		}
		for key, value := range entry.Values {
			if strings.TrimSpace(key) == "" || len(key) > MaxRuntimeConfigDirectoryNameLength {
				return RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: values key 无效", ErrInvalidRuntimeConfigDirectory)
			}
			if runtimeConfigDirectorySecretKey(key) {
				return RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: values.%s 疑似敏感字段，拒绝跨 Runtime 传输", ErrInvalidRuntimeConfigDirectory, key)
			}
			if err := validateRuntimeConfigDirectoryValue(value, "values."+key, 1); err != nil {
				return RuntimeConfigDirectoryEntry{}, err
			}
		}
	case RuntimeConfigDirectoryKindPersona:
		if entry.Values != nil {
			return RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: persona entry 不允许 values", ErrInvalidRuntimeConfigDirectory)
		}
		if err := validateRuntimeConfigDirectoryText(entry.Instruction, MaxRuntimeConfigDirectoryInstruction, "instruction", true); err != nil {
			return RuntimeConfigDirectoryEntry{}, err
		}
		if entry.Enabled == nil {
			enabled := true
			entry.Enabled = &enabled
		}
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: entry 编码失败: %v", ErrInvalidRuntimeConfigDirectory, err)
	}
	if len(encoded) > MaxRuntimeConfigDirectoryEntryBytes {
		return RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: entry 超过 %d 字节上限", ErrInvalidRuntimeConfigDirectory, MaxRuntimeConfigDirectoryEntryBytes)
	}
	if err := validateRuntimeConfigDirectoryJSON(encoded); err != nil {
		return RuntimeConfigDirectoryEntry{}, err
	}
	entry.Values = cloneRuntimeConfigDirectoryMap(entry.Values)
	return entry, nil
}

func canonicalRuntimeConfigDirectoryEntry(entry RuntimeConfigDirectoryEntry) ([]byte, RuntimeConfigDirectoryEntry, error) {
	normalized, err := entry.Normalize()
	if err != nil {
		return nil, RuntimeConfigDirectoryEntry{}, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: entry canonical JSON 失败", ErrInvalidRuntimeConfigDirectory)
	}
	return encoded, normalized, nil
}

func RuntimeConfigDirectoryEntryDigest(entry RuntimeConfigDirectoryEntry) (string, error) {
	encoded, _, err := canonicalRuntimeConfigDirectoryEntry(entry)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func runtimeConfigDirectoryDigestValid(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func RuntimeConfigDirectoryDeliveryID(source, destination string, kind RuntimeConfigDirectoryKind, entryID string, revision int, previousDigest, bodyDigest, idempotencyKey string) string {
	material := strings.Join([]string{strings.TrimSpace(source), strings.TrimSpace(destination), strings.TrimSpace(string(kind)), strings.TrimSpace(entryID), fmt.Sprintf("%d", revision), strings.TrimSpace(strings.ToLower(previousDigest)), strings.TrimSpace(strings.ToLower(bodyDigest)), strings.TrimSpace(idempotencyKey)}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return "runtime-config-directory:" + hex.EncodeToString(sum[:])
}

func (e RuntimeConfigDirectoryEnvelope) Normalize() (RuntimeConfigDirectoryEnvelope, error) {
	e.Source = strings.TrimSpace(e.Source)
	e.Destination = strings.TrimSpace(e.Destination)
	e.DeliveryID = strings.TrimSpace(e.DeliveryID)
	e.Kind = RuntimeConfigDirectoryKind(strings.TrimSpace(strings.ToLower(string(e.Kind))))
	e.EntryID = strings.TrimSpace(e.EntryID)
	e.PreviousDigest = strings.TrimSpace(strings.ToLower(e.PreviousDigest))
	e.BodyDigest = strings.TrimSpace(strings.ToLower(e.BodyDigest))
	e.CorrelationID = strings.TrimSpace(e.CorrelationID)
	e.IdempotencyKey = strings.TrimSpace(e.IdempotencyKey)
	e.Signature = strings.TrimSpace(strings.ToLower(e.Signature))
	if e.Version != RuntimeConfigDirectoryEnvelopeVersion || !e.Kind.valid() || e.Source == "" || e.Destination == "" || e.DeliveryID == "" || e.EntryID == "" || e.Revision <= 0 || e.BodyDigest == "" || e.Timestamp.IsZero() || e.IssuedAt.IsZero() {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: envelope 必填 metadata 不完整", ErrInvalidRuntimeConfigDirectory)
	}
	if len(e.Source) > MaxRuntimeConfigDirectorySourceLength || len(e.Destination) > MaxRuntimeConfigDirectoryTargetLength || len(e.DeliveryID) > MaxRuntimeConfigDirectoryDeliveryID || len(e.EntryID) > MaxRuntimeConfigDirectoryIDLength || len(e.CorrelationID) > MaxRuntimeConfigDirectoryCorrelation || len(e.IdempotencyKey) > MaxRuntimeConfigDirectoryIdempotency {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: envelope metadata 超出长度限制", ErrInvalidRuntimeConfigDirectory)
	}
	if err := validateRuntimeConfigDirectoryID(e.EntryID); err != nil {
		return RuntimeConfigDirectoryEnvelope{}, err
	}
	if !runtimeConfigDirectoryDigestValid(e.BodyDigest) || (e.PreviousDigest != "" && !runtimeConfigDirectoryDigestValid(e.PreviousDigest)) {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: body/previous digest 无效", ErrInvalidRuntimeConfigDirectory)
	}
	if e.DeliveryID != RuntimeConfigDirectoryDeliveryID(e.Source, e.Destination, e.Kind, e.EntryID, e.Revision, e.PreviousDigest, e.BodyDigest, e.IdempotencyKey) {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: delivery_id 与 metadata 不一致", ErrRuntimeConfigDirectoryAuth)
	}
	if e.Signature != "" && !isRuntimeEventDeliveryDigest(e.Signature) {
		return RuntimeConfigDirectoryEnvelope{}, fmt.Errorf("%w: signature 无效", ErrInvalidRuntimeConfigDirectory)
	}
	e.Timestamp = e.Timestamp.UTC()
	e.IssuedAt = e.IssuedAt.UTC()
	return e, nil
}

func NormalizeRuntimeConfigDirectoryEnvelope(envelope RuntimeConfigDirectoryEnvelope) (RuntimeConfigDirectoryEnvelope, error) {
	return envelope.Normalize()
}

func NewRuntimeConfigDirectoryEnvelope(source, destination string, entry RuntimeConfigDirectoryEntry, previousDigest, correlationID, idempotencyKey string, timestamp time.Time) (RuntimeConfigDirectoryEnvelope, error) {
	encoded, normalized, err := canonicalRuntimeConfigDirectoryEntry(entry)
	if err != nil {
		return RuntimeConfigDirectoryEnvelope{}, err
	}
	bodySum := sha256.Sum256(encoded)
	bodyDigest := "sha256:" + hex.EncodeToString(bodySum[:])
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	} else {
		timestamp = timestamp.UTC()
	}
	envelope := RuntimeConfigDirectoryEnvelope{
		Version: RuntimeConfigDirectoryEnvelopeVersion, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination),
		Kind: normalized.Kind, EntryID: normalized.ID, Revision: normalized.Revision, PreviousDigest: previousDigest,
		BodyDigest: bodyDigest, CorrelationID: correlationID, IdempotencyKey: idempotencyKey, Timestamp: timestamp, IssuedAt: timestamp,
	}
	envelope.DeliveryID = RuntimeConfigDirectoryDeliveryID(envelope.Source, envelope.Destination, envelope.Kind, envelope.EntryID, envelope.Revision, envelope.PreviousDigest, envelope.BodyDigest, envelope.IdempotencyKey)
	return envelope.Normalize()
}

func (e RuntimeConfigDirectoryEnvelope) canonicalUnsigned() ([]byte, error) {
	normalized, err := e.Normalize()
	if err != nil {
		return nil, err
	}
	normalized.Signature = ""
	return json.Marshal(normalized)
}

func SignRuntimeConfigDirectoryEnvelope(envelope RuntimeConfigDirectoryEnvelope, secret []byte) (RuntimeConfigDirectoryEnvelope, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeConfigDirectoryEnvelope{}, err
	}
	canonical, err := envelope.canonicalUnsigned()
	if err != nil {
		return RuntimeConfigDirectoryEnvelope{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	envelope.Signature = hex.EncodeToString(mac.Sum(nil))
	return envelope.Normalize()
}

func VerifyRuntimeConfigDirectoryEnvelope(envelope RuntimeConfigDirectoryEnvelope, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	normalized, err := envelope.Normalize()
	if err != nil {
		return err
	}
	if normalized.Signature == "" {
		return fmt.Errorf("%w: 缺少 signature", ErrRuntimeConfigDirectoryAuth)
	}
	canonical, err := normalized.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(normalized.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature 编码无效", ErrRuntimeConfigDirectoryAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: HMAC 不匹配", ErrRuntimeConfigDirectoryAuth)
	}
	return nil
}

func normalizeRuntimeConfigDirectoryPair(envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryEnvelope, RuntimeConfigDirectoryEntry, error) {
	normalizedEnvelope, err := envelope.Normalize()
	if err != nil {
		return RuntimeConfigDirectoryEnvelope{}, RuntimeConfigDirectoryEntry{}, err
	}
	normalizedEntry, err := entry.Normalize()
	if err != nil {
		return RuntimeConfigDirectoryEnvelope{}, RuntimeConfigDirectoryEntry{}, err
	}
	digest, err := RuntimeConfigDirectoryEntryDigest(normalizedEntry)
	if err != nil {
		return RuntimeConfigDirectoryEnvelope{}, RuntimeConfigDirectoryEntry{}, err
	}
	if normalizedEnvelope.Kind != normalizedEntry.Kind || normalizedEnvelope.EntryID != normalizedEntry.ID || normalizedEnvelope.Revision != normalizedEntry.Revision || normalizedEnvelope.BodyDigest != digest {
		return RuntimeConfigDirectoryEnvelope{}, RuntimeConfigDirectoryEntry{}, fmt.Errorf("%w: envelope 与正文 metadata/digest 不一致", ErrRuntimeConfigDirectoryAuth)
	}
	return normalizedEnvelope, normalizedEntry, nil
}

// NormalizeRuntimeConfigDirectoryPair validates and canonicalizes the
// envelope/body pair for storage adapters and transports.
func NormalizeRuntimeConfigDirectoryPair(envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryEnvelope, RuntimeConfigDirectoryEntry, error) {
	return normalizeRuntimeConfigDirectoryPair(envelope, entry)
}

func runtimeConfigDirectoryRecordMatches(record RuntimeConfigDirectoryRecord, envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) bool {
	return record.Envelope.Source == envelope.Source && record.Envelope.Destination == envelope.Destination && record.Envelope.DeliveryID == envelope.DeliveryID && record.Envelope.Kind == envelope.Kind && record.Envelope.EntryID == envelope.EntryID && record.Envelope.Revision == envelope.Revision && record.Envelope.PreviousDigest == envelope.PreviousDigest && record.Envelope.BodyDigest == envelope.BodyDigest && record.Envelope.IdempotencyKey == envelope.IdempotencyKey && entryDigestMatches(record.Entry, envelope.BodyDigest) && entryDigestMatches(entry, envelope.BodyDigest)
}

func entryDigestMatches(entry RuntimeConfigDirectoryEntry, expected string) bool {
	digest, err := RuntimeConfigDirectoryEntryDigest(entry)
	return err == nil && digest == expected
}

// AcceptRuntimeConfigDirectoryRecord applies the destination monotonic CAS
// rules to an optional existing record. It is exported for SQL-backed stores
// so every repository uses exactly one conflict policy.
func AcceptRuntimeConfigDirectoryRecord(existing *RuntimeConfigDirectoryRecord, envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry, receivedAt time.Time) (bool, error) {
	normalizedEnvelope, normalizedEntry, err := NormalizeRuntimeConfigDirectoryPair(envelope, entry)
	if err != nil {
		return false, err
	}
	envelope, entry = normalizedEnvelope, normalizedEntry
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	} else {
		receivedAt = receivedAt.UTC()
	}
	if existing == nil || existing.Envelope.DeliveryID == "" {
		if existing == nil {
			return false, fmt.Errorf("%w: existing record 不能为空", ErrRuntimeConfigDirectoryConflict)
		}
		if envelope.PreviousDigest != "" {
			return false, fmt.Errorf("%w: 不存在的 entry 不能携带 previous_digest", ErrRuntimeConfigDirectoryConflict)
		}
		*existing = RuntimeConfigDirectoryRecord{Envelope: envelope, Entry: cloneRuntimeConfigDirectoryEntry(entry), ReceivedAt: receivedAt.UTC()}
		return false, nil
	}
	if runtimeConfigDirectoryRecordMatches(*existing, envelope, entry) {
		return true, nil
	}
	if existing.Envelope.Source != envelope.Source || existing.Envelope.Destination != envelope.Destination {
		return false, fmt.Errorf("%w: source/destination 不能改变", ErrRuntimeConfigDirectoryConflict)
	}
	if envelope.Revision < existing.Envelope.Revision {
		return false, ErrRuntimeConfigDirectoryStale
	}
	if envelope.Revision == existing.Envelope.Revision {
		return false, fmt.Errorf("%w: 相同 revision 的正文或 identity 不一致", ErrRuntimeConfigDirectoryConflict)
	}
	if envelope.PreviousDigest == "" || envelope.PreviousDigest != existing.Envelope.BodyDigest {
		return false, fmt.Errorf("%w: 新 revision 未连续指向当前正文", ErrRuntimeConfigDirectoryConflict)
	}
	*existing = RuntimeConfigDirectoryRecord{Envelope: envelope, Entry: cloneRuntimeConfigDirectoryEntry(entry), ReceivedAt: receivedAt.UTC()}
	return false, nil
}

func validateRuntimeConfigDirectoryTimestamp(timestamp, now time.Time, maxAge, futureSkew time.Duration) error {
	if maxAge <= 0 || futureSkew < 0 {
		return fmt.Errorf("%w: timestamp policy 无效", ErrInvalidRuntimeConfigDirectory)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	timestamp = timestamp.UTC()
	if timestamp.After(now.Add(futureSkew)) {
		return fmt.Errorf("%w: issued_at 位于未来窗口之外", ErrRuntimeConfigDirectoryAuth)
	}
	if now.Sub(timestamp) > maxAge {
		return ErrRuntimeConfigDirectoryStale
	}
	return nil
}

func (r RuntimeConfigDirectoryReceipt) canonicalUnsigned() ([]byte, error) {
	copy := r
	copy.Signature = ""
	return json.Marshal(copy)
}

func SignRuntimeConfigDirectoryReceipt(receipt RuntimeConfigDirectoryReceipt, secret []byte) (RuntimeConfigDirectoryReceipt, error) {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return RuntimeConfigDirectoryReceipt{}, err
	}
	canonical, err := receipt.canonicalUnsigned()
	if err != nil {
		return RuntimeConfigDirectoryReceipt{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	receipt.Signature = hex.EncodeToString(mac.Sum(nil))
	return receipt, nil
}

func VerifyRuntimeConfigDirectoryReceipt(receipt RuntimeConfigDirectoryReceipt, secret []byte) error {
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return err
	}
	if receipt.Signature == "" || !isRuntimeEventDeliveryDigest(receipt.Signature) {
		return fmt.Errorf("%w: receipt signature 无效", ErrRuntimeConfigDirectoryAuth)
	}
	canonical, err := receipt.canonicalUnsigned()
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(strings.TrimSpace(strings.ToLower(receipt.Signature)))
	if err != nil {
		return fmt.Errorf("%w: receipt signature 编码无效", ErrRuntimeConfigDirectoryAuth)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("%w: receipt HMAC 不匹配", ErrRuntimeConfigDirectoryAuth)
	}
	return nil
}

func (r RuntimeConfigDirectoryReceipt) ValidateAgainst(envelope RuntimeConfigDirectoryEnvelope) error {
	normalized, err := envelope.Normalize()
	if err != nil {
		return err
	}
	if r.Version != normalized.Version || strings.TrimSpace(r.Source) != normalized.Source || strings.TrimSpace(r.Destination) != normalized.Destination || strings.TrimSpace(r.DeliveryID) != normalized.DeliveryID || r.Kind != normalized.Kind || strings.TrimSpace(r.EntryID) != normalized.EntryID || r.Revision != normalized.Revision || strings.TrimSpace(strings.ToLower(r.BodyDigest)) != normalized.BodyDigest || r.ReceivedAt.IsZero() {
		return fmt.Errorf("%w: receipt metadata 不一致", ErrRuntimeConfigDirectoryAuth)
	}
	return nil
}

// RuntimeConfigDirectoryReceiver verifies, bounds and persists one signed
// entry.  It never applies defaults/bindings or starts a Worker.
type RuntimeConfigDirectoryReceiver struct {
	Source        string
	Destination   string
	SharedSecret  []byte
	Repository    RuntimeConfigDirectoryRepository
	Materializer  RuntimeConfigDirectoryMaterializer
	MaxAge        time.Duration
	MaxFutureSkew time.Duration
	MaxBodyBytes  int64
	Now           func() time.Time
}

func NewRuntimeConfigDirectoryReceiver(source, destination string, secret []byte, repository RuntimeConfigDirectoryRepository) (*RuntimeConfigDirectoryReceiver, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" || len(strings.TrimSpace(source)) > MaxRuntimeConfigDirectorySourceLength || len(strings.TrimSpace(destination)) > MaxRuntimeConfigDirectoryTargetLength {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeConfigDirectory)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if repository == nil {
		return nil, fmt.Errorf("%w: repository 不能为空", ErrInvalidRuntimeConfigDirectory)
	}
	return &RuntimeConfigDirectoryReceiver{Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Repository: repository, MaxAge: DefaultRuntimeConfigDirectoryMaxAge, MaxFutureSkew: DefaultRuntimeConfigDirectoryFutureSkew, MaxBodyBytes: MaxRuntimeConfigDirectoryEntryBytes + MaxRuntimeConfigDirectoryEnvelopeBytes, Now: time.Now}, nil
}

func (r *RuntimeConfigDirectoryReceiver) Handler() http.Handler { return r }

func readRuntimeConfigDirectoryDelivery(request *http.Request, maxBody int64) (RuntimeConfigDirectoryDelivery, error) {
	if request == nil || request.Body == nil {
		return RuntimeConfigDirectoryDelivery{}, fmt.Errorf("%w: request body 为空", ErrInvalidRuntimeConfigDirectory)
	}
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDirectoryEntryBytes+MaxRuntimeConfigDirectoryEnvelopeBytes {
		maxBody = MaxRuntimeConfigDirectoryEntryBytes + MaxRuntimeConfigDirectoryEnvelopeBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		return RuntimeConfigDirectoryDelivery{}, fmt.Errorf("%w: request body 超过上限或读取失败", ErrInvalidRuntimeConfigDirectory)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload RuntimeConfigDirectoryDelivery
	if err := decoder.Decode(&payload); err != nil {
		return RuntimeConfigDirectoryDelivery{}, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeConfigDirectory)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDirectoryDelivery{}, fmt.Errorf("%w: 请求包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectory)
	}
	return payload, nil
}

func runtimeConfigDirectoryHTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrRuntimeConfigDirectoryAuth):
		return http.StatusUnauthorized
	case errors.Is(err, ErrRuntimeConfigDirectoryStale), errors.Is(err, ErrRuntimeConfigDirectoryConflict):
		return http.StatusConflict
	case errors.Is(err, ErrRuntimeConfigDirectoryUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

func writeRuntimeConfigDirectoryJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (r *RuntimeConfigDirectoryReceiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r == nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusInternalServerError, map[string]string{"error": ErrInvalidRuntimeConfigDirectory.Error()})
		return
	}
	if request != nil && request.URL != nil {
		path := strings.TrimRight(request.URL.Path, "/")
		if strings.HasSuffix(path, "/status") {
			r.ServeStatusHTTP(writer, request)
			return
		}
		if strings.HasSuffix(path, "/materialize") {
			r.ServeMaterializeHTTP(writer, request)
			return
		}
	}
	if request.Method != http.MethodPost {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	payload, err := readRuntimeConfigDirectoryDelivery(request, r.MaxBodyBytes)
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, runtimeConfigDirectoryHTTPStatus(err), map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	envelope, entry, err := normalizeRuntimeConfigDirectoryPair(payload.Envelope, payload.Entry)
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, runtimeConfigDirectoryHTTPStatus(err), map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	if envelope.Source != r.Source || envelope.Destination != r.Destination {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusForbidden, map[string]string{"error": ErrRuntimeConfigDirectoryAuth.Error()})
		return
	}
	if header := strings.TrimSpace(request.Header.Get("X-Abot-Config-Directory-Version")); header != "" && header != fmt.Sprintf("%d", envelope.Version) {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusBadRequest, map[string]string{"error": "version header 与 body 不一致"})
		return
	}
	if header := strings.TrimSpace(strings.ToLower(request.Header.Get("X-Abot-Config-Directory-Signature"))); header != "" && header != envelope.Signature {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusUnauthorized, map[string]string{"error": ErrRuntimeConfigDirectoryAuth.Error()})
		return
	}
	if header := strings.TrimSpace(request.Header.Get("Idempotency-Key")); header != "" && header != envelope.DeliveryID {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusBadRequest, map[string]string{"error": "idempotency key 与 delivery_id 不一致"})
		return
	}
	if err := VerifyRuntimeConfigDirectoryEnvelope(envelope, r.SharedSecret); err != nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusUnauthorized, map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if err := validateRuntimeConfigDirectoryTimestamp(envelope.IssuedAt, now, r.MaxAge, r.MaxFutureSkew); err != nil {
		writeRuntimeConfigDirectoryJSON(writer, runtimeConfigDirectoryHTTPStatus(err), map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	duplicate, err := r.Repository.AcceptRuntimeConfigDirectory(request.Context(), envelope, entry)
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, runtimeConfigDirectoryHTTPStatus(err), map[string]string{"error": SanitizeRuntimeString(err.Error())})
		return
	}
	receipt, err := SignRuntimeConfigDirectoryReceipt(RuntimeConfigDirectoryReceipt{Version: envelope.Version, Source: envelope.Source, Destination: envelope.Destination, DeliveryID: envelope.DeliveryID, Kind: envelope.Kind, EntryID: envelope.EntryID, Revision: envelope.Revision, BodyDigest: envelope.BodyDigest, Duplicate: duplicate, ReceivedAt: now}, r.SharedSecret)
	if err != nil {
		writeRuntimeConfigDirectoryJSON(writer, http.StatusInternalServerError, map[string]string{"error": ErrInvalidRuntimeConfigDirectory.Error()})
		return
	}
	writeRuntimeConfigDirectoryJSON(writer, http.StatusOK, receipt)
}

// RuntimeConfigDirectoryHTTPTransport is the explicit one-entry HTTP client.
type RuntimeConfigDirectoryHTTPTransport struct {
	Endpoint     string
	Source       string
	Destination  string
	SharedSecret []byte
	Client       *http.Client
	MaxBodyBytes int64
}

func NewRuntimeConfigDirectoryHTTPTransport(endpoint, source, destination string, secret []byte, client *http.Client) (*RuntimeConfigDirectoryHTTPTransport, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeConfigDirectory)
	}
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("%w: source/destination 无效", ErrInvalidRuntimeConfigDirectory)
	}
	if err := validateRuntimeEventDeliverySecret(secret); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &RuntimeConfigDirectoryHTTPTransport{Endpoint: endpoint, Source: strings.TrimSpace(source), Destination: strings.TrimSpace(destination), SharedSecret: append([]byte(nil), secret...), Client: client, MaxBodyBytes: MaxRuntimeConfigDirectoryResponseBytes}, nil
}

func (t *RuntimeConfigDirectoryHTTPTransport) Deliver(ctx context.Context, envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryReceipt, error) {
	if t == nil {
		return RuntimeConfigDirectoryReceipt{}, ErrRuntimeConfigDirectoryUnavailable
	}
	normalized, normalizedEntry, err := normalizeRuntimeConfigDirectoryPair(envelope, entry)
	if err != nil {
		return RuntimeConfigDirectoryReceipt{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeConfigDirectoryReceipt{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDirectoryAuth)
	}
	signed, err := SignRuntimeConfigDirectoryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeConfigDirectoryReceipt{}, err
	}
	body, err := json.Marshal(RuntimeConfigDirectoryDelivery{Envelope: signed, Entry: normalizedEntry})
	if err != nil || int64(len(body)) > MaxRuntimeConfigDirectoryEntryBytes+MaxRuntimeConfigDirectoryEnvelopeBytes {
		return RuntimeConfigDirectoryReceipt{}, fmt.Errorf("%w: request 编码超过上限", ErrInvalidRuntimeConfigDirectory)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeConfigDirectoryReceipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Config-Directory-Version", fmt.Sprintf("%d", signed.Version))
	request.Header.Set("X-Abot-Config-Directory-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return RuntimeConfigDirectoryReceipt{}, fmt.Errorf("%w: Runtime config directory 请求失败: %v", ErrRuntimeConfigDirectoryUnavailable, err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDirectoryResponseBytes {
		maxBody = MaxRuntimeConfigDirectoryResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeConfigDirectoryReceipt{}, fmt.Errorf("%w: response 超过字节上限或读取失败", ErrInvalidRuntimeConfigDirectory)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented || response.StatusCode == http.StatusServiceUnavailable {
			return RuntimeConfigDirectoryReceipt{}, ErrRuntimeConfigDirectoryUnavailable
		}
		return RuntimeConfigDirectoryReceipt{}, fmt.Errorf("Runtime config directory 返回 HTTP %d: %s", response.StatusCode, SanitizeRuntimeString(strings.TrimSpace(string(responseBody))))
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var receipt RuntimeConfigDirectoryReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeConfigDirectoryReceipt{}, fmt.Errorf("%w: receipt JSON 无效", ErrInvalidRuntimeConfigDirectory)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDirectoryReceipt{}, fmt.Errorf("%w: receipt 包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectory)
	}
	if err := VerifyRuntimeConfigDirectoryReceipt(receipt, t.SharedSecret); err != nil {
		return RuntimeConfigDirectoryReceipt{}, err
	}
	if err := receipt.ValidateAgainst(signed); err != nil {
		return RuntimeConfigDirectoryReceipt{}, err
	}
	return receipt, nil
}

// ReconcileRuntimeConfigDirectory asks the destination for proof that this
// exact body identity was accepted. It is intentionally metadata-only and
// can be omitted by older transports.
func (t *RuntimeConfigDirectoryHTTPTransport) ReconcileRuntimeConfigDirectory(ctx context.Context, envelope RuntimeConfigDirectoryEnvelope, entry RuntimeConfigDirectoryEntry) (RuntimeConfigDirectoryStatus, error) {
	if t == nil {
		return RuntimeConfigDirectoryStatus{}, ErrRuntimeConfigDirectoryUnavailable
	}
	normalized, normalizedEntry, err := normalizeRuntimeConfigDirectoryPair(envelope, entry)
	if err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	if normalized.Source != t.Source || normalized.Destination != t.Destination {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: source/destination 不匹配", ErrRuntimeConfigDirectoryStatusAuth)
	}
	_ = normalizedEntry // body identity is checked locally; status carries no body.
	signed, err := SignRuntimeConfigDirectoryEnvelope(normalized, t.SharedSecret)
	if err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	body, err := json.Marshal(signed)
	if err != nil || len(body) > MaxRuntimeConfigDirectoryEnvelopeBytes {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: status request 编码超过上限", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	endpoint, err := runtimeConfigDirectoryStatusEndpoint(t.Endpoint)
	if err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Abot-Config-Directory-Version", fmt.Sprintf("%d", signed.Version))
	request.Header.Set("X-Abot-Config-Directory-Signature", signed.Signature)
	request.Header.Set("Idempotency-Key", signed.DeliveryID)
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: Runtime config directory status 请求失败: %v", ErrRuntimeConfigDirectoryUnavailable, err)
	}
	defer response.Body.Close()
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 || maxBody > MaxRuntimeConfigDirectoryResponseBytes {
		maxBody = MaxRuntimeConfigDirectoryResponseBytes
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || int64(len(responseBody)) > maxBody {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: status response 超过上限或读取失败", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented || response.StatusCode == http.StatusServiceUnavailable {
			return RuntimeConfigDirectoryStatus{}, ErrRuntimeConfigDirectoryUnavailable
		}
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("Runtime config directory status 返回 HTTP %d: %s", response.StatusCode, SanitizeRuntimeString(strings.TrimSpace(string(responseBody))))
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var status RuntimeConfigDirectoryStatus
	if err := decoder.Decode(&status); err != nil {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: status JSON 无效", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RuntimeConfigDirectoryStatus{}, fmt.Errorf("%w: status 包含多个 JSON 文档", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	if err := VerifyRuntimeConfigDirectoryStatus(status, t.SharedSecret); err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	if err := status.ValidateAgainst(signed); err != nil {
		return RuntimeConfigDirectoryStatus{}, err
	}
	return status, nil
}

func runtimeConfigDirectoryStatusEndpoint(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: endpoint 必须是 http/https URL", ErrInvalidRuntimeConfigDirectoryStatus)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/status"
	return u.String(), nil
}

// DeliverRuntimeConfigDirectory is a small Coordinator convenience boundary.
// It deliberately performs one explicit push; durable source retries remain a
// separate future outbox so a caller cannot mistake this for global atomicity.
func (c *Coordinator) DeliverRuntimeConfigDirectory(ctx context.Context, transport RuntimeConfigDirectoryTransport, source, destination string, entry RuntimeConfigDirectoryEntry, previousDigest, correlationID, idempotencyKey string, at time.Time) (RuntimeConfigDirectoryReceipt, error) {
	if c == nil || transport == nil {
		return RuntimeConfigDirectoryReceipt{}, ErrRuntimeConfigDirectoryUnavailable
	}
	envelope, err := NewRuntimeConfigDirectoryEnvelope(source, destination, entry, previousDigest, correlationID, idempotencyKey, at)
	if err != nil {
		return RuntimeConfigDirectoryReceipt{}, err
	}
	return transport.Deliver(ctx, envelope, entry)
}

// EnqueueRuntimeConfigDirectory registers one immutable public directory body
// in the source-side durable cursor. The body is normalized and retained by
// the repository so retries do not reread mutable profile/persona state. It
// does not contact the destination or materialize defaults/bindings; callers
// must configure a route-bound transport and invoke the shared dispatcher.
func (c *Coordinator) EnqueueRuntimeConfigDirectory(ctx context.Context, source, destination string, entry RuntimeConfigDirectoryEntry, previousDigest, correlationID, idempotencyKey string, at time.Time) (RuntimeConfigDirectoryOutbox, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryOutbox{}, ErrRuntimeConfigDirectoryUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryOutboxRepository)
	if !ok {
		return RuntimeConfigDirectoryOutbox{}, ErrRuntimeConfigDirectoryUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	item, err := NewRuntimeConfigDirectoryOutbox(source, destination, entry, previousDigest, correlationID, idempotencyKey, at)
	if err != nil {
		return RuntimeConfigDirectoryOutbox{}, err
	}
	return repo.EnqueueRuntimeConfigDirectoryOutbox(ctx, item)
}

// EnqueueRuntimeConfigDirectoryFanout atomically registers one metadata-only
// parent and one durable child cursor per destination. The repository must
// explicitly implement RuntimeConfigDirectoryFanoutRepository; falling back
// to a loop would make a partial fan-out look successful after a crash.
func (c *Coordinator) EnqueueRuntimeConfigDirectoryFanout(ctx context.Context, source string, destinations []string, entry RuntimeConfigDirectoryEntry, previousDigest, correlationID, idempotencyKey string, at time.Time) (RuntimeConfigDirectoryFanoutPlan, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrRuntimeConfigDirectoryUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryFanoutRepository)
	if !ok {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrRuntimeConfigDirectoryUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plan, children, err := NewRuntimeConfigDirectoryFanoutPlan(source, destinations, entry, previousDigest, correlationID, idempotencyKey, at)
	if err != nil {
		return RuntimeConfigDirectoryFanoutPlan{}, err
	}
	return repo.EnqueueRuntimeConfigDirectoryFanout(ctx, plan, children)
}

func (c *Coordinator) GetRuntimeConfigDirectoryFanout(ctx context.Context, id string) (RuntimeConfigDirectoryFanoutPlan, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrRuntimeConfigDirectoryUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryFanoutRepository)
	if !ok {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrRuntimeConfigDirectoryUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.GetRuntimeConfigDirectoryFanout(ctx, id)
}

func (c *Coordinator) ListRuntimeConfigDirectoryFanouts(ctx context.Context, source string, status RuntimeConfigDirectoryFanoutStatus, limit int) ([]RuntimeConfigDirectoryFanoutPlan, error) {
	if c == nil || c.repo == nil {
		return nil, ErrRuntimeConfigDirectoryUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryFanoutRepository)
	if !ok {
		return nil, ErrRuntimeConfigDirectoryUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.ListRuntimeConfigDirectoryFanouts(ctx, source, status, limit)
}

func (c *Coordinator) ReconcileRuntimeConfigDirectoryFanout(ctx context.Context, id string, now time.Time) (RuntimeConfigDirectoryFanoutPlan, error) {
	if c == nil || c.repo == nil {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrRuntimeConfigDirectoryUnavailable
	}
	repo, ok := c.repo.(RuntimeConfigDirectoryFanoutRepository)
	if !ok {
		return RuntimeConfigDirectoryFanoutPlan{}, ErrRuntimeConfigDirectoryUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return repo.ReconcileRuntimeConfigDirectoryFanout(ctx, id, now)
}

var _ RuntimeConfigDirectoryTransport = (*RuntimeConfigDirectoryHTTPTransport)(nil)
