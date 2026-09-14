// Package artifact owns bounded, immutable content references. It deliberately
// has no dependency on Agent Runtime or Workspace so a large object can be
// stored without copying its bytes into an event/session row.
package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	DefaultMaxBytes  int64 = 64 << 20
	MaxNameBytes           = 255
	MaxMIMEBytes           = 128
	MaxMetadataBytes       = 16 << 10
	MaxPreviewBytes        = 32 << 10
)

var (
	ErrInvalidRequest = errors.New("artifact 请求无效")
	ErrNotFound       = errors.New("artifact 不存在")
	ErrForbidden      = errors.New("无权访问 artifact")
	ErrConflict       = errors.New("artifact 冲突")
	ErrTooLarge       = errors.New("artifact 超过大小上限")
	ErrExpired        = errors.New("artifact 已过期")
	ErrQuarantined    = errors.New("artifact 处于隔离状态")
	ErrObjectMissing  = errors.New("artifact 对象缺失")
)

type Kind string

const (
	KindInputAttachment Kind = "input_attachment"
	KindCommandLog      Kind = "command_log"
	KindDiff            Kind = "diff"
	KindTestReport      Kind = "test_report"
	KindGeneratedDoc    Kind = "generated_document"
	KindImage           Kind = "image"
	KindArchive         Kind = "archive"
	KindBinary          Kind = "binary"
	KindContextExtract  Kind = "context_extract"
	KindTraceExport     Kind = "trace_export"
)

type Status string

const (
	StatusUploading   Status = "uploading"
	StatusReady       Status = "ready"
	StatusQuarantined Status = "quarantined"
	StatusFailed      Status = "failed"
	StatusDeleting    Status = "deleting"
)

type Artifact struct {
	ID             string         `json:"id"`
	Version        int            `json:"version"`
	UserID         string         `json:"user_id"`
	ConversationID string         `json:"conversation_id,omitempty"`
	InvocationID   string         `json:"invocation_id,omitempty"`
	ProducerType   string         `json:"producer_type,omitempty"`
	ProducerID     string         `json:"producer_id,omitempty"`
	Kind           Kind           `json:"kind"`
	Name           string         `json:"name"`
	MIMEType       string         `json:"mime_type"`
	Size           int64          `json:"size"`
	Digest         string         `json:"digest"`
	StorageKey     string         `json:"-"`
	SecurityClass  string         `json:"security_class,omitempty"`
	Status         Status         `json:"status"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Error          string         `json:"error,omitempty"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type ArtifactRef struct {
	ID       string `json:"id"`
	Version  int    `json:"version"`
	Digest   string `json:"digest"`
	Kind     Kind   `json:"kind"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Name     string `json:"name,omitempty"`
	Preview  string `json:"preview,omitempty"`
}

type PutRequest struct {
	UserID         string
	ConversationID string
	InvocationID   string
	ProducerType   string
	ProducerID     string
	Kind           Kind
	Name           string
	MIMEType       string
	SecurityClass  string
	Metadata       map[string]any
	ExpiresAt      *time.Time
	MaxBytes       int64
}

type ByteRange struct {
	Start    int64
	Length   int64
	HasRange bool
}

func (r ByteRange) Validate(size int64) error {
	if !r.HasRange {
		return nil
	}
	if r.Start < 0 || r.Length <= 0 || r.Start >= size || r.Length > size-r.Start {
		return fmt.Errorf("%w: Range 超出 artifact 大小", ErrInvalidRequest)
	}
	return nil
}

type ObjectInfo struct {
	Key         string
	Digest      string
	Size        int64
	RangeStart  int64
	RangeLength int64
}

type StoredObject struct {
	Key      string
	Digest   string
	Size     int64
	MIMEType string
	// Created reports whether this call actually created the object. A store
	// that cannot tell leaves it false, which makes the cleanup paths keep the
	// object for the garbage collector instead of risking a concurrent writer's
	// content-addressed object.
	Created bool
}

type ObjectStore interface {
	Put(context.Context, PutRequest, io.Reader) (StoredObject, error)
	Open(context.Context, string, ByteRange) (io.ReadCloser, ObjectInfo, error)
	Delete(context.Context, string) error
}

type Repository interface {
	Create(context.Context, Artifact) error
	Update(context.Context, Artifact, int64) error
	Get(context.Context, string) (Artifact, error)
	List(context.Context, string, string) ([]Artifact, error)
	FindReadyByProducerDigest(context.Context, string, string, string) (Artifact, error)
	CountByStorageKey(context.Context, string) (int, error)
	Delete(context.Context, string) error
	DeleteByConversation(context.Context, string) ([]Artifact, error)
}

func validKind(kind Kind) bool {
	switch kind {
	case KindInputAttachment, KindCommandLog, KindDiff, KindTestReport, KindGeneratedDoc, KindImage, KindArchive, KindBinary, KindContextExtract, KindTraceExport:
		return true
	default:
		return false
	}
}

func validStatus(status Status) bool {
	switch status {
	case StatusUploading, StatusReady, StatusQuarantined, StatusFailed, StatusDeleting:
		return true
	default:
		return false
	}
}

func (item Artifact) Validate() error {
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.UserID) == "" || item.Version <= 0 {
		return fmt.Errorf("%w: id、user_id 和 version 不能为空", ErrInvalidRequest)
	}
	if !validKind(item.Kind) || !validStatus(item.Status) {
		return fmt.Errorf("%w: kind 或 status 无效", ErrInvalidRequest)
	}
	if strings.TrimSpace(item.Name) == "" || len([]byte(item.Name)) > MaxNameBytes || strings.ContainsAny(item.Name, "/\\\x00\r\n") {
		return fmt.Errorf("%w: artifact name 无效", ErrInvalidRequest)
	}
	if (item.Status == StatusReady && len([]byte(item.MIMEType)) == 0) || len([]byte(item.MIMEType)) > MaxMIMEBytes || strings.ContainsAny(item.MIMEType, "\r\n\x00") {
		return fmt.Errorf("%w: MIME type 无效", ErrInvalidRequest)
	}
	if item.Size < 0 || item.Size > DefaultMaxBytes {
		return fmt.Errorf("%w: artifact size 无效", ErrInvalidRequest)
	}
	if len(item.SecurityClass) > 64 || strings.ContainsAny(item.SecurityClass, "\r\n\x00") {
		return fmt.Errorf("%w: security class 无效", ErrInvalidRequest)
	}
	if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: artifact 时间不能为空", ErrInvalidRequest)
	}
	if item.Status == StatusReady {
		if !validDigest(item.Digest) || strings.TrimSpace(item.StorageKey) == "" || item.Size < 0 {
			return fmt.Errorf("%w: ready artifact 缺少对象身份", ErrInvalidRequest)
		}
	}
	if item.ExpiresAt != nil && item.ExpiresAt.Before(item.CreatedAt) {
		return fmt.Errorf("%w: expires_at 早于 created_at", ErrInvalidRequest)
	}
	if err := validateMetadata(item.Metadata); err != nil {
		return err
	}
	return nil
}

func validDigest(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func validateMetadata(metadata map[string]any) error {
	if len(metadata) == 0 {
		return nil
	}
	for key := range metadata {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "" || len([]byte(key)) > 128 || strings.ContainsAny(key, "\r\n\x00") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") {
			return fmt.Errorf("%w: metadata 包含非法或敏感键", ErrInvalidRequest)
		}
	}
	raw, err := json.Marshal(metadata)
	if err != nil || len(raw) > MaxMetadataBytes {
		return fmt.Errorf("%w: metadata 过大或不可序列化", ErrInvalidRequest)
	}
	return nil
}

func (request PutRequest) Validate() error {
	if strings.TrimSpace(request.UserID) == "" || !validKind(request.Kind) {
		return fmt.Errorf("%w: user_id 和 kind 不能为空", ErrInvalidRequest)
	}
	if strings.TrimSpace(request.Name) == "" || len([]byte(request.Name)) > MaxNameBytes || strings.ContainsAny(request.Name, "/\\\x00\r\n") {
		return fmt.Errorf("%w: artifact name 无效", ErrInvalidRequest)
	}
	if len(request.MIMEType) > MaxMIMEBytes || strings.ContainsAny(request.MIMEType, "\r\n\x00") {
		return fmt.Errorf("%w: MIME type 无效", ErrInvalidRequest)
	}
	if request.MaxBytes < 0 || request.MaxBytes > DefaultMaxBytes {
		return fmt.Errorf("%w: max bytes 无效", ErrInvalidRequest)
	}
	if len(request.SecurityClass) > 64 || strings.ContainsAny(request.SecurityClass, "\r\n\x00") {
		return fmt.Errorf("%w: security class 无效", ErrInvalidRequest)
	}
	return validateMetadata(request.Metadata)
}

func (item Artifact) Ref() ArtifactRef {
	preview := ""
	if item.Metadata != nil {
		if value, ok := item.Metadata["preview"].(string); ok {
			preview = truncatePreview(value)
		}
	}
	return ArtifactRef{ID: item.ID, Version: item.Version, Digest: item.Digest, Kind: item.Kind, MIMEType: item.MIMEType, Size: item.Size, Name: item.Name, Preview: preview}
}

func truncatePreview(value string) string {
	if len([]byte(value)) <= MaxPreviewBytes {
		return value
	}
	return string([]byte(value)[:MaxPreviewBytes]) + "…"
}

func newID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("artifact-%d", time.Now().UnixNano())
	}
	return "artifact-" + hex.EncodeToString(value)
}

func sanitizeName(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, character := range value {
		if unicode.IsControl(character) || character == '/' || character == '\\' {
			continue
		}
		builder.WriteRune(character)
		if len([]byte(builder.String())) >= MaxNameBytes {
			break
		}
	}
	return strings.TrimSpace(builder.String())
}

type Service struct {
	repository Repository
	store      ObjectStore
	now        func() time.Time

	policyMu sync.RWMutex
	policy   MaintenancePolicy
}

func NewService(repository Repository, store ObjectStore) (*Service, error) {
	if repository == nil || store == nil {
		return nil, fmt.Errorf("%w: artifact repository 和 object store 不能为空", ErrInvalidRequest)
	}
	return &Service{repository: repository, store: store, now: func() time.Time { return time.Now().UTC() }, policy: DefaultMaintenancePolicy()}, nil
}

func (s *Service) Put(ctx context.Context, request PutRequest, reader io.Reader) (Artifact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if reader == nil {
		return Artifact{}, fmt.Errorf("%w: 上传内容不能为空", ErrInvalidRequest)
	}
	request.Name = sanitizeName(request.Name)
	if err := request.Validate(); err != nil {
		return Artifact{}, err
	}
	now := s.now().UTC()
	item := Artifact{ID: newID(), Version: 1, UserID: strings.TrimSpace(request.UserID), ConversationID: strings.TrimSpace(request.ConversationID), InvocationID: strings.TrimSpace(request.InvocationID), ProducerType: strings.TrimSpace(request.ProducerType), ProducerID: strings.TrimSpace(request.ProducerID), Kind: request.Kind, Name: request.Name, MIMEType: normalizeMIME(request.MIMEType), SecurityClass: strings.TrimSpace(request.SecurityClass), Status: StatusUploading, Metadata: cloneMetadata(request.Metadata), ExpiresAt: cloneTime(request.ExpiresAt), CreatedAt: now, UpdatedAt: now}
	if err := item.Validate(); err != nil {
		return Artifact{}, err
	}
	if err := s.repository.Create(ctx, item); err != nil {
		return Artifact{}, err
	}
	scannedReader := newSecretScanReader(reader)
	object, err := s.store.Put(ctx, request, scannedReader)
	if err != nil {
		item.Status = StatusFailed
		item.Version++
		item.Error = safeError(err)
		item.UpdatedAt = s.now().UTC()
		_ = s.repository.Update(context.WithoutCancel(ctx), item, 1)
		return Artifact{}, err
	}
	// The object is on disk before the metadata row exists. Enforce the local
	// quota here so a refused upload never becomes visible or downloadable.
	if quotaErr := s.enforceQuota(ctx, object); quotaErr != nil {
		s.cleanupUncommitted(context.WithoutCancel(ctx), item.ID, object.Key, object.Created)
		return Artifact{}, quotaErr
	}
	item.Digest, item.Size, item.StorageKey = object.Digest, object.Size, object.Key
	if item.MIMEType == "" {
		item.MIMEType = object.MIMEType
	}
	if rule := scannedReader.matchedRule(); rule != "" {
		item.Status = StatusQuarantined
		item.SecurityClass = "secret"
		item.Error = "内容安全扫描命中规则: " + rule
	} else {
		item.Status = StatusReady
	}
	item.Version++
	item.UpdatedAt = s.now().UTC()
	if item.ProducerType != "" && item.ProducerID != "" {
		if existing, findErr := s.repository.FindReadyByProducerDigest(ctx, item.ProducerType, item.ProducerID, item.Digest); findErr == nil && existing.ID != item.ID {
			if existing.StorageKey != item.StorageKey && object.Created {
				_ = s.store.Delete(context.WithoutCancel(ctx), item.StorageKey)
			}
			_ = s.repository.Delete(context.WithoutCancel(ctx), item.ID)
			if existing.UserID != item.UserID || existing.ConversationID != item.ConversationID {
				return Artifact{}, ErrConflict
			}
			return existing, nil
		} else if findErr != nil && !errors.Is(findErr, ErrNotFound) {
			_ = s.repository.Delete(context.WithoutCancel(ctx), item.ID)
			// Content-addressed stores may have committed the object under a key
			// shared by a previously stored artifact. Leave it in place when the
			// repository lookup itself failed; retention cleanup will reclaim any
			// truly unreferenced object.
			return Artifact{}, findErr
		}
	}
	if err := item.Validate(); err != nil {
		s.cleanupUncommitted(context.WithoutCancel(ctx), item.ID, item.StorageKey, object.Created)
		return Artifact{}, err
	}
	if err := s.repository.Update(ctx, item, 1); err != nil {
		s.cleanupUncommitted(context.WithoutCancel(ctx), item.ID, item.StorageKey, object.Created)
		return Artifact{}, err
	}
	return item, nil
}

func (s *Service) cleanupUncommitted(ctx context.Context, id, storageKey string, created bool) {
	_ = s.repository.Delete(ctx, id)
	if strings.TrimSpace(storageKey) == "" || !created {
		// An object this request did not create may belong to a concurrent
		// writer that has not published its metadata row yet. Leave it for the
		// garbage collector instead of deleting someone else's content.
		return
	}
	count, err := s.repository.CountByStorageKey(ctx, storageKey)
	if err == nil && count == 0 {
		_ = s.store.Delete(ctx, storageKey)
	}
}

// getOwnedArtifact reads one artifact and enforces ownership. Expiry is only
// enforced for read paths: an owner must always be able to delete content after
// its retention window closed, otherwise an expired artifact could never be
// reclaimed and would keep consuming the storage quota.
func (s *Service) getOwnedArtifact(ctx context.Context, userID, id string, enforceExpiry bool) (Artifact, error) {
	item, err := s.repository.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return Artifact{}, err
	}
	if strings.TrimSpace(userID) == "" || item.UserID != strings.TrimSpace(userID) {
		return Artifact{}, ErrForbidden
	}
	if enforceExpiry && item.ExpiresAt != nil && !item.ExpiresAt.After(s.now().UTC()) {
		return Artifact{}, ErrExpired
	}
	return item, nil
}

func (s *Service) Get(ctx context.Context, userID, id string) (Artifact, error) {
	return s.getOwnedArtifact(ctx, userID, id, true)
}

func (s *Service) List(ctx context.Context, userID, conversationID string) ([]Artifact, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrForbidden
	}
	return s.repository.List(ctx, strings.TrimSpace(userID), strings.TrimSpace(conversationID))
}

func (s *Service) Open(ctx context.Context, userID, id string, byteRange ByteRange) (io.ReadCloser, ObjectInfo, Artifact, error) {
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, ObjectInfo{}, Artifact{}, err
	}
	if item.Status == StatusQuarantined {
		return nil, ObjectInfo{}, Artifact{}, ErrQuarantined
	}
	if item.Status != StatusReady {
		return nil, ObjectInfo{}, Artifact{}, fmt.Errorf("%w: artifact 当前状态为 %s", ErrConflict, item.Status)
	}
	if err := byteRange.Validate(item.Size); err != nil {
		return nil, ObjectInfo{}, Artifact{}, err
	}
	reader, info, err := s.store.Open(ctx, item.StorageKey, byteRange)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrObjectMissing) {
		return nil, ObjectInfo{}, Artifact{}, ErrObjectMissing
	}
	if err != nil {
		return nil, ObjectInfo{}, Artifact{}, err
	}
	if info.Size != item.Size || (info.Digest != "" && info.Digest != item.Digest) {
		_ = reader.Close()
		return nil, ObjectInfo{}, Artifact{}, ErrObjectMissing
	}
	return reader, info, item, nil
}

// Delete removes one artifact. The metadata row is the privacy boundary and is
// always removed once the caller has asked for deletion; a failure to reclaim
// the content-addressed object is handed to the durable delete outbox instead
// of rejecting the request. Repositories without the outbox extension keep the
// previous behaviour and report the storage error.
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	// Deletion deliberately ignores the retention window: expiry closes the
	// read path, it must not make the content immortal.
	item, err := s.getOwnedArtifact(ctx, userID, id, false)
	if err != nil {
		return err
	}
	if item.Status != StatusDeleting {
		expectedVersion := int64(item.Version)
		item.Status = StatusDeleting
		item.Version++
		item.UpdatedAt = s.now().UTC()
		if err := s.repository.Update(ctx, item, expectedVersion); err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
	}
	if item.StorageKey != "" {
		count, countErr := s.repository.CountByStorageKey(ctx, item.StorageKey)
		if countErr != nil {
			return countErr
		}
		if count <= 1 {
			if deleteErr := s.store.Delete(ctx, item.StorageKey); deleteErr != nil && !errors.Is(deleteErr, ErrObjectMissing) {
				if enqueueErr := s.enqueueObjectDeletion(context.WithoutCancel(ctx), item.StorageKey, s.now()); enqueueErr != nil {
					return deleteErr
				}
			}
		}
	}
	if err := s.repository.Delete(ctx, item.ID); errors.Is(err, ErrNotFound) {
		return nil
	} else {
		return err
	}
}

// DeleteConversation physically removes all metadata owned by a conversation
// and then reclaims content-addressed objects that have no remaining refs.
// Metadata deletion is deliberate: conversation deletion is a physical
// privacy boundary, while an object-delete failure can be retried by a later
// garbage-collection pass without resurrecting the references.
func (s *Service) DeleteConversation(ctx context.Context, conversationID string) error {
	items, err := s.repository.DeleteByConversation(ctx, strings.TrimSpace(conversationID))
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		key := strings.TrimSpace(item.StorageKey)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		count, countErr := s.repository.CountByStorageKey(ctx, key)
		if countErr != nil {
			return countErr
		}
		if count == 0 {
			if deleteErr := s.store.Delete(ctx, key); deleteErr != nil && !errors.Is(deleteErr, ErrObjectMissing) {
				// The conversation is already gone; keep the object deletion
				// durable instead of failing the privacy boundary.
				if enqueueErr := s.enqueueObjectDeletion(context.WithoutCancel(ctx), key, s.now()); enqueueErr != nil {
					return deleteErr
				}
			}
		}
	}
	return nil
}

func cloneMetadata(value map[string]any) map[string]any {
	if len(value) == 0 {
		return nil
	}
	raw, err := json.Marshal(value)
	if err == nil {
		var result map[string]any
		if json.Unmarshal(raw, &result) == nil {
			return result
		}
	}
	// validateMetadata rejects values that cannot be JSON encoded. Keep a
	// defensive shallow copy here for callers constructing an invalid object
	// directly so returned maps are still not the original map.
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := value.UTC()
	return &copyValue
}

func normalizeMIME(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	return value
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 512 {
		return value[:512] + "…"
	}
	return value
}
