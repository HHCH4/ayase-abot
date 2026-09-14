package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/artifact"
	"gorm.io/gorm"
)

// artifactRow stores only metadata and the opaque content-addressed key. The
// key is never built from a user supplied path and is not included in API JSON.
type artifactRow struct {
	ID             string     `gorm:"primaryKey;size:100"`
	Version        int        `gorm:"not null"`
	UserID         string     `gorm:"index;size:300;not null"`
	ConversationID string     `gorm:"index;size:100"`
	InvocationID   string     `gorm:"index;size:100"`
	ProducerType   string     `gorm:"index;size:100"`
	ProducerID     string     `gorm:"index;size:200"`
	Kind           string     `gorm:"index;size:48;not null"`
	Name           string     `gorm:"size:255;not null"`
	MIMEType       string     `gorm:"size:128;not null"`
	Size           int64      `gorm:"not null"`
	Digest         string     `gorm:"index;size:71;not null"`
	StorageKey     string     `gorm:"index;size:160;not null"`
	SecurityClass  string     `gorm:"size:64"`
	Status         string     `gorm:"index;size:32;not null"`
	MetadataJSON   string     `gorm:"type:text"`
	Error          string     `gorm:"type:text"`
	ExpiresAt      *time.Time `gorm:"index"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (artifactRow) TableName() string { return "abot_artifacts" }

type artifactRepository struct {
	db *gorm.DB
}

var _ artifact.Repository = (*artifactRepository)(nil)

func (r *artifactRepository) Create(ctx context.Context, item artifact.Artifact) error {
	if err := item.Validate(); err != nil {
		return err
	}
	row, err := artifactRowFromDomain(item)
	if err != nil {
		return err
	}
	if err := r.db.WithContext(nonNilContext(ctx)).Create(&row).Error; err != nil {
		return mapArtifactDBError(err)
	}
	return nil
}

func (r *artifactRepository) Update(ctx context.Context, item artifact.Artifact, expectedVersion int64) error {
	if expectedVersion <= 0 || int64(item.Version) != expectedVersion+1 {
		return artifact.ErrConflict
	}
	if err := item.Validate(); err != nil {
		return err
	}
	row, err := artifactRowFromDomain(item)
	if err != nil {
		return err
	}
	var current artifactRow
	db := r.db.WithContext(nonNilContext(ctx))
	if err := db.Where("id = ?", item.ID).First(&current).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return artifact.ErrNotFound
		}
		return err
	}
	if int64(current.Version) != expectedVersion || current.Version+1 != item.Version {
		return artifact.ErrConflict
	}
	values := map[string]any{
		"version": item.Version, "user_id": item.UserID, "conversation_id": item.ConversationID, "invocation_id": item.InvocationID,
		"producer_type": item.ProducerType, "producer_id": item.ProducerID, "kind": string(item.Kind),
		"name": item.Name, "mime_type": item.MIMEType, "size": item.Size, "digest": item.Digest,
		"storage_key": item.StorageKey, "security_class": item.SecurityClass, "status": string(item.Status),
		"metadata_json": row.MetadataJSON, "error": item.Error, "expires_at": item.ExpiresAt,
		"created_at": item.CreatedAt, "updated_at": item.UpdatedAt,
	}
	result := db.Model(&artifactRow{}).Where("id = ? AND version = ?", item.ID, expectedVersion).Updates(values)
	if result.Error != nil {
		return mapArtifactDBError(result.Error)
	}
	if result.RowsAffected == 0 {
		return artifact.ErrConflict
	}
	return nil
}

func (r *artifactRepository) Get(ctx context.Context, id string) (artifact.Artifact, error) {
	var row artifactRow
	if err := r.db.WithContext(nonNilContext(ctx)).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return artifact.Artifact{}, artifact.ErrNotFound
		}
		return artifact.Artifact{}, err
	}
	return artifactFromRow(row)
}

func (r *artifactRepository) List(ctx context.Context, userID, conversationID string) ([]artifact.Artifact, error) {
	query := r.db.WithContext(nonNilContext(ctx)).Where("user_id = ?", strings.TrimSpace(userID)).Where("status NOT IN ?", []string{string(artifact.StatusFailed), string(artifact.StatusDeleting)})
	if strings.TrimSpace(conversationID) != "" {
		query = query.Where("conversation_id = ?", strings.TrimSpace(conversationID))
	}
	var rows []artifactRow
	if err := query.Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]artifact.Artifact, 0, len(rows))
	for _, row := range rows {
		item, err := artifactFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *artifactRepository) FindReadyByProducerDigest(ctx context.Context, producerType, producerID, digest string) (artifact.Artifact, error) {
	var row artifactRow
	query := r.db.WithContext(nonNilContext(ctx)).Where("producer_type = ? AND producer_id = ? AND digest = ? AND status = ?", producerType, producerID, digest, string(artifact.StatusReady)).Order("created_at ASC, id ASC")
	if err := query.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return artifact.Artifact{}, artifact.ErrNotFound
		}
		return artifact.Artifact{}, err
	}
	return artifactFromRow(row)
}

func (r *artifactRepository) CountByStorageKey(ctx context.Context, storageKey string) (int, error) {
	var count int64
	if err := r.db.WithContext(nonNilContext(ctx)).Model(&artifactRow{}).Where("storage_key = ? AND status <> ?", storageKey, string(artifact.StatusFailed)).Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

func (r *artifactRepository) Delete(ctx context.Context, id string) error {
	result := r.db.WithContext(nonNilContext(ctx)).Where("id = ?", strings.TrimSpace(id)).Delete(&artifactRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return artifact.ErrNotFound
	}
	return nil
}

func (r *artifactRepository) DeleteByConversation(ctx context.Context, conversationID string) ([]artifact.Artifact, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil, fmt.Errorf("%w: conversation_id 不能为空", artifact.ErrInvalidRequest)
	}
	var result []artifact.Artifact
	db := r.db.WithContext(nonNilContext(ctx))
	err := db.Transaction(func(tx *gorm.DB) error {
		var rows []artifactRow
		if err := tx.Where("conversation_id = ?", conversationID).Find(&rows).Error; err != nil {
			return err
		}
		result = make([]artifact.Artifact, 0, len(rows))
		for _, row := range rows {
			item, err := artifactFromRow(row)
			if err != nil {
				return err
			}
			result = append(result, item)
		}
		return tx.Where("conversation_id = ?", conversationID).Delete(&artifactRow{}).Error
	})
	return result, err
}

func artifactRowFromDomain(item artifact.Artifact) (artifactRow, error) {
	metadata := ""
	if item.Metadata != nil {
		encoded, err := json.Marshal(item.Metadata)
		if err != nil {
			return artifactRow{}, fmt.Errorf("%w: metadata 编码失败", artifact.ErrInvalidRequest)
		}
		metadata = string(encoded)
	}
	return artifactRow{
		ID: item.ID, Version: item.Version, UserID: item.UserID, ConversationID: item.ConversationID,
		InvocationID: item.InvocationID, ProducerType: item.ProducerType, ProducerID: item.ProducerID,
		Kind: string(item.Kind), Name: item.Name, MIMEType: item.MIMEType, Size: item.Size, Digest: item.Digest,
		StorageKey: item.StorageKey, SecurityClass: item.SecurityClass, Status: string(item.Status), MetadataJSON: metadata,
		Error: item.Error, ExpiresAt: item.ExpiresAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

func artifactFromRow(row artifactRow) (artifact.Artifact, error) {
	var metadata map[string]any
	if strings.TrimSpace(row.MetadataJSON) != "" {
		if err := json.Unmarshal([]byte(row.MetadataJSON), &metadata); err != nil {
			return artifact.Artifact{}, fmt.Errorf("解析 artifact metadata 失败: %w", err)
		}
	}
	return artifact.Artifact{
		ID: row.ID, Version: row.Version, UserID: row.UserID, ConversationID: row.ConversationID,
		InvocationID: row.InvocationID, ProducerType: row.ProducerType, ProducerID: row.ProducerID,
		Kind: artifact.Kind(row.Kind), Name: row.Name, MIMEType: row.MIMEType, Size: row.Size, Digest: row.Digest,
		StorageKey: row.StorageKey, SecurityClass: row.SecurityClass, Status: artifact.Status(row.Status), Metadata: metadata,
		Error: row.Error, ExpiresAt: cloneTimePtr(row.ExpiresAt), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func mapArtifactDBError(err error) error {
	if err == nil {
		return nil
	}
	// SQLite's unique primary-key error is intentionally normalized so callers
	// can retry without depending on the driver text.
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
		return artifact.ErrConflict
	}
	return err
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
