package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"Abot/internal/artifact"
	"gorm.io/gorm/clause"
)

// maxStaleArtifactScan bounds one sweep query. The maintenance pass is
// repeatable, so a large backlog is drained over several passes instead of
// loading every stale row at once.
const maxStaleArtifactScan = 1000

// artifactObjectDeletionRow is the durable delete outbox for content-addressed
// objects whose metadata row is already gone. Only the opaque storage key is
// stored; no user supplied path, name or content ever reaches this table.
type artifactObjectDeletionRow struct {
	StorageKey    string    `gorm:"primaryKey;size:160"`
	Attempts      int       `gorm:"not null"`
	LastError     string    `gorm:"type:text"`
	RequestedAt   time.Time `gorm:"not null"`
	NextAttemptAt time.Time `gorm:"index;not null"`
	UpdatedAt     time.Time
}

func (artifactObjectDeletionRow) TableName() string { return "abot_artifact_object_deletions" }

var (
	_ artifact.UsageRepository                 = (*artifactRepository)(nil)
	_ artifact.ActiveReferenceRepository       = (*artifactRepository)(nil)
	_ artifact.StaleArtifactRepository         = (*artifactRepository)(nil)
	_ artifact.ObjectDeletionRepository        = (*artifactRepository)(nil)
	_ artifact.PendingObjectDeletionRepository = (*artifactRepository)(nil)
)

// StoredUsage reports the de-duplicated physical footprint. Rows sharing one
// content-addressed key are counted once, so uploading the same bytes twice
// does not consume the quota twice.
func (r *artifactRepository) StoredUsage(ctx context.Context) (artifact.StoredUsage, error) {
	db := r.db.WithContext(nonNilContext(ctx))
	usage := artifact.StoredUsage{}
	aggregate := struct {
		Bytes   int64
		Objects int
	}{}
	query := "SELECT COALESCE(SUM(size), 0) AS bytes, COUNT(*) AS objects FROM (" +
		"SELECT storage_key, MAX(size) AS size FROM abot_artifacts WHERE status <> ? AND storage_key <> '' GROUP BY storage_key)"
	if err := db.Raw(query, string(artifact.StatusFailed)).Scan(&aggregate).Error; err != nil {
		return artifact.StoredUsage{}, err
	}
	usage.Bytes = aggregate.Bytes
	usage.Objects = aggregate.Objects
	var rows int64
	if err := db.Model(&artifactRow{}).Where("status <> ?", string(artifact.StatusFailed)).Count(&rows).Error; err != nil {
		return artifact.StoredUsage{}, err
	}
	usage.Artifacts = int(rows)
	return usage, nil
}

// CountActiveByStorageKey intentionally excludes deleting rows: a row that is
// already on its way out no longer requires the object to exist, which lets a
// garbage-collection pass finish an interrupted deletion.
func (r *artifactRepository) CountActiveByStorageKey(ctx context.Context, storageKey string) (int, error) {
	var count int64
	err := r.db.WithContext(nonNilContext(ctx)).Model(&artifactRow{}).
		Where("storage_key = ? AND status NOT IN ?", storageKey, []string{string(artifact.StatusFailed), string(artifact.StatusDeleting)}).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (r *artifactRepository) ListStaleUploading(ctx context.Context, before time.Time, limit int) ([]artifact.Artifact, error) {
	return r.listStaleArtifacts(ctx, artifact.StatusUploading, before, limit)
}

func (r *artifactRepository) ListStaleDeleting(ctx context.Context, before time.Time, limit int) ([]artifact.Artifact, error) {
	return r.listStaleArtifacts(ctx, artifact.StatusDeleting, before, limit)
}

func (r *artifactRepository) listStaleArtifacts(ctx context.Context, status artifact.Status, before time.Time, limit int) ([]artifact.Artifact, error) {
	if limit <= 0 || limit > maxStaleArtifactScan {
		limit = maxStaleArtifactScan
	}
	var rows []artifactRow
	if err := r.db.WithContext(nonNilContext(ctx)).
		Where("status = ? AND updated_at <= ?", string(status), before.UTC()).
		Order("updated_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
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

// EnqueueObjectDeletion is idempotent: an already queued key keeps its original
// request time and attempt counter.
func (r *artifactRepository) EnqueueObjectDeletion(ctx context.Context, storageKey string, now time.Time) error {
	key := strings.TrimSpace(storageKey)
	if key == "" {
		return fmt.Errorf("%w: storage key 不能为空", artifact.ErrInvalidRequest)
	}
	now = now.UTC()
	row := artifactObjectDeletionRow{StorageKey: key, RequestedAt: now, NextAttemptAt: now, UpdatedAt: now}
	return r.db.WithContext(nonNilContext(ctx)).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

func (r *artifactRepository) ListDueObjectDeletions(ctx context.Context, now time.Time, limit int) ([]artifact.ObjectDeletion, error) {
	if limit <= 0 || limit > maxStaleArtifactScan {
		limit = maxStaleArtifactScan
	}
	var rows []artifactObjectDeletionRow
	if err := r.db.WithContext(nonNilContext(ctx)).
		Where("next_attempt_at <= ?", now.UTC()).
		Order("next_attempt_at ASC, storage_key ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]artifact.ObjectDeletion, 0, len(rows))
	for _, row := range rows {
		result = append(result, artifact.ObjectDeletion{
			StorageKey:    row.StorageKey,
			Attempts:      row.Attempts,
			RequestedAt:   row.RequestedAt,
			NextAttemptAt: row.NextAttemptAt,
			LastError:     row.LastError,
		})
	}
	return result, nil
}

func (r *artifactRepository) CompleteObjectDeletion(ctx context.Context, storageKey string) error {
	result := r.db.WithContext(nonNilContext(ctx)).
		Where("storage_key = ?", strings.TrimSpace(storageKey)).Delete(&artifactObjectDeletionRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return artifact.ErrNotFound
	}
	return nil
}

func (r *artifactRepository) RescheduleObjectDeletion(ctx context.Context, deletion artifact.ObjectDeletion, now time.Time) error {
	key := strings.TrimSpace(deletion.StorageKey)
	if key == "" {
		return fmt.Errorf("%w: storage key 不能为空", artifact.ErrInvalidRequest)
	}
	if deletion.Attempts < 0 {
		return fmt.Errorf("%w: attempt 计数无效", artifact.ErrInvalidRequest)
	}
	result := r.db.WithContext(nonNilContext(ctx)).Model(&artifactObjectDeletionRow{}).Where("storage_key = ?", key).
		Updates(map[string]any{
			"attempts":        deletion.Attempts,
			"last_error":      deletion.LastError,
			"next_attempt_at": deletion.NextAttemptAt.UTC(),
			"updated_at":      now.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return artifact.ErrNotFound
	}
	return nil
}

func (r *artifactRepository) HasObjectDeletion(ctx context.Context, storageKey string) (bool, error) {
	var count int64
	err := r.db.WithContext(nonNilContext(ctx)).Model(&artifactObjectDeletionRow{}).
		Where("storage_key = ?", strings.TrimSpace(storageKey)).Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
