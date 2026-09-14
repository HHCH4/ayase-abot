package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
)

// runtimeConfigDirectoryOutboxRow stores the bounded public body alongside
// its source cursor. Keeping the body in the cursor makes retries independent
// of later local profile/persona edits; no credentials or bindings are copied.
type runtimeConfigDirectoryOutboxRow struct {
	ID               string `gorm:"primaryKey;size:240"`
	FanoutID         string `gorm:"index;size:240"`
	Source           string `gorm:"index;size:160;not null"`
	Destination      string `gorm:"index;size:160;not null"`
	DeliveryID       string `gorm:"uniqueIndex;size:220;not null"`
	Kind             string `gorm:"index;size:16;not null"`
	EntryID          string `gorm:"index;size:128;not null"`
	EntryRevision    int    `gorm:"not null"`
	PreviousDigest   string `gorm:"size:71"`
	BodyDigest       string `gorm:"size:71;not null"`
	CorrelationID    string `gorm:"size:512"`
	IdempotencyKey   string `gorm:"size:200"`
	EntryName        string `gorm:"size:200;not null"`
	EntryDescription string `gorm:"type:text;not null"`
	EntryInstruction string `gorm:"type:text;not null"`
	EntryValuesJSON  string `gorm:"type:text;not null"`
	EntryEnabled     *bool
	Status           string     `gorm:"index;size:32;not null"`
	Attempt          int        `gorm:"not null"`
	Revision         int64      `gorm:"not null"`
	AvailableAt      time.Time  `gorm:"index"`
	LeaseOwner       string     `gorm:"index;size:160"`
	LeaseExpiresAt   *time.Time `gorm:"index"`
	LastError        string     `gorm:"type:text"`
	CreatedAt        time.Time  `gorm:"index"`
	UpdatedAt        time.Time
}

func (runtimeConfigDirectoryOutboxRow) TableName() string {
	return "abot_runtime_config_directory_outbox"
}

func runtimeConfigDirectoryOutboxFromRow(row runtimeConfigDirectoryOutboxRow) (agentruntime.RuntimeConfigDirectoryOutbox, error) {
	values := map[string]any(nil)
	if strings.TrimSpace(row.EntryValuesJSON) != "" && strings.TrimSpace(row.EntryValuesJSON) != "null" {
		if err := json.Unmarshal([]byte(row.EntryValuesJSON), &values); err != nil {
			return agentruntime.RuntimeConfigDirectoryOutbox{}, fmt.Errorf("解析 Runtime 配置目录 outbox 正文失败: %w", err)
		}
	}
	item := agentruntime.RuntimeConfigDirectoryOutbox{
		ID: row.ID, FanoutID: row.FanoutID, Source: row.Source, Destination: row.Destination, DeliveryID: row.DeliveryID,
		Kind: agentruntime.RuntimeConfigDirectoryKind(row.Kind), EntryID: row.EntryID, EntryRevision: row.EntryRevision,
		PreviousDigest: row.PreviousDigest, BodyDigest: row.BodyDigest, CorrelationID: row.CorrelationID,
		IdempotencyKey: row.IdempotencyKey,
		Entry:          agentruntime.RuntimeConfigDirectoryEntry{Version: agentruntime.RuntimeConfigDirectoryEntryVersion, Kind: agentruntime.RuntimeConfigDirectoryKind(row.Kind), ID: row.EntryID, Revision: row.EntryRevision, Name: row.EntryName, Description: row.EntryDescription, Instruction: row.EntryInstruction, Values: values, Enabled: cloneBoolPtr(row.EntryEnabled)},
		Status:         agentruntime.RuntimeConfigDirectoryOutboxStatus(row.Status), Attempt: row.Attempt, Revision: row.Revision,
		AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner, LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	return item.Normalize(time.Now().UTC())
}

func runtimeConfigDirectoryOutboxToRow(item agentruntime.RuntimeConfigDirectoryOutbox) (*runtimeConfigDirectoryOutboxRow, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized.Entry.Values)
	if err != nil {
		return nil, fmt.Errorf("%w: values 编码失败", agentruntime.ErrInvalidRuntimeConfigDirectory)
	}
	if len(encoded) > agentruntime.MaxRuntimeConfigDirectoryEntryBytes {
		return nil, fmt.Errorf("%w: values 超过字节上限", agentruntime.ErrInvalidRuntimeConfigDirectory)
	}
	return &runtimeConfigDirectoryOutboxRow{
		ID: normalized.ID, FanoutID: normalized.FanoutID, Source: normalized.Source, Destination: normalized.Destination, DeliveryID: normalized.DeliveryID,
		Kind: string(normalized.Kind), EntryID: normalized.EntryID, EntryRevision: normalized.EntryRevision, PreviousDigest: normalized.PreviousDigest,
		BodyDigest: normalized.BodyDigest, CorrelationID: normalized.CorrelationID, IdempotencyKey: normalized.IdempotencyKey,
		EntryName: normalized.Entry.Name, EntryDescription: normalized.Entry.Description, EntryInstruction: normalized.Entry.Instruction,
		EntryValuesJSON: string(encoded), EntryEnabled: cloneBoolPtr(normalized.Entry.Enabled), Status: string(normalized.Status), Attempt: normalized.Attempt,
		Revision: normalized.Revision, AvailableAt: normalized.AvailableAt, LeaseOwner: normalized.LeaseOwner, LeaseExpiresAt: cloneTimePtr(normalized.LeaseExpiresAt), LastError: normalized.LastError,
		CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt,
	}, nil
}

func (r *runtimeRepository) EnqueueRuntimeConfigDirectoryOutbox(ctx context.Context, item agentruntime.RuntimeConfigDirectoryOutbox) (agentruntime.RuntimeConfigDirectoryOutbox, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryOutbox{}, err
	}
	var saved agentruntime.RuntimeConfigDirectoryOutbox
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing runtimeConfigDirectoryOutboxRow
		findErr := tx.Where("id = ?", normalized.ID).First(&existing).Error
		if findErr == nil {
			stored, decodeErr := runtimeConfigDirectoryOutboxFromRow(existing)
			if decodeErr != nil {
				return decodeErr
			}
			if !stored.Matches(normalized) {
				return agentruntime.ErrConflict
			}
			saved = stored
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var same runtimeConfigDirectoryOutboxRow
		identityErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&same).Error
		if identityErr == nil {
			stored, decodeErr := runtimeConfigDirectoryOutboxFromRow(same)
			if decodeErr != nil {
				return decodeErr
			}
			if !stored.Matches(normalized) {
				return agentruntime.ErrConflict
			}
			saved = stored
			return nil
		}
		if !errors.Is(identityErr, gorm.ErrRecordNotFound) {
			return identityErr
		}
		if normalized.Status != agentruntime.RuntimeConfigDirectoryOutboxQueued || normalized.Attempt != 0 {
			return agentruntime.ErrConflict
		}
		row, rowErr := runtimeConfigDirectoryOutboxToRow(normalized)
		if rowErr != nil {
			return rowErr
		}
		if err := tx.Create(row).Error; err != nil {
			return err
		}
		saved = normalized
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryOutbox{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeConfigDirectoryOutbox(ctx context.Context, id string) (agentruntime.RuntimeConfigDirectoryOutbox, error) {
	var row runtimeConfigDirectoryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryOutbox{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryOutbox{}, err
	}
	return runtimeConfigDirectoryOutboxFromRow(row)
}

func (r *runtimeRepository) ListRuntimeConfigDirectoryOutbox(ctx context.Context, kind agentruntime.RuntimeConfigDirectoryKind, entryID string, status agentruntime.RuntimeConfigDirectoryOutboxStatus, limit int) ([]agentruntime.RuntimeConfigDirectoryOutbox, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryOutboxRow{})
	if strings.TrimSpace(string(kind)) != "" {
		query = query.Where("kind = ?", strings.TrimSpace(strings.ToLower(string(kind))))
	}
	if strings.TrimSpace(entryID) != "" {
		query = query.Where("entry_id = ?", strings.TrimSpace(entryID))
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeConfigDirectoryOutboxRow
	if err := query.Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeConfigDirectoryOutbox, 0, len(rows))
	for _, row := range rows {
		item, err := runtimeConfigDirectoryOutboxFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *runtimeRepository) ClaimRuntimeConfigDirectoryOutbox(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeConfigDirectoryOutbox, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDirectoryOutboxOwner || ttl <= 0 || ttl > agentruntime.MaxRuntimeConfigDirectoryOutboxLease {
		return agentruntime.RuntimeConfigDirectoryOutbox{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for round := 0; round < 4; round++ {
		var rows []runtimeConfigDirectoryOutboxRow
		if err := r.db.WithContext(ctx).Where("(status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))", string(agentruntime.RuntimeConfigDirectoryOutboxQueued), now, string(agentruntime.RuntimeConfigDirectoryOutboxProcessing), now).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return agentruntime.RuntimeConfigDirectoryOutbox{}, false, err
		}
		if len(rows) == 0 {
			return agentruntime.RuntimeConfigDirectoryOutbox{}, false, nil
		}
		for _, row := range rows {
			if row.Status == string(agentruntime.RuntimeConfigDirectoryOutboxProcessing) && (strings.TrimSpace(row.LeaseOwner) == "" || row.LeaseExpiresAt == nil || row.LeaseExpiresAt.IsZero()) {
				result := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryOutboxRow{}).Where("id = ? AND revision = ? AND status = ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryOutboxProcessing)).Updates(map[string]any{"status": string(agentruntime.RuntimeConfigDirectoryOutboxFailed), "lease_owner": "", "lease_expires_at": nil, "last_error": "config directory outbox processing 元数据无效", "revision": row.Revision + 1, "updated_at": now})
				if result.Error != nil {
					return agentruntime.RuntimeConfigDirectoryOutbox{}, false, result.Error
				}
				continue
			}
			if row.Attempt >= agentruntime.MaxRuntimeConfigDirectoryOutboxAttempts {
				result := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryOutboxRow{}).Where("id = ? AND revision = ? AND status IN (?, ?)", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryOutboxQueued), string(agentruntime.RuntimeConfigDirectoryOutboxProcessing)).Updates(map[string]any{"status": string(agentruntime.RuntimeConfigDirectoryOutboxFailed), "lease_owner": "", "lease_expires_at": nil, "last_error": "Runtime config directory outbox 达到最大投递次数", "revision": row.Revision + 1, "updated_at": now})
				if result.Error != nil {
					return agentruntime.RuntimeConfigDirectoryOutbox{}, false, result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			result := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryOutboxRow{}).Where("id = ? AND revision = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryOutboxQueued), now, string(agentruntime.RuntimeConfigDirectoryOutboxProcessing), now).Updates(map[string]any{"status": string(agentruntime.RuntimeConfigDirectoryOutboxProcessing), "attempt": row.Attempt + 1, "revision": row.Revision + 1, "lease_owner": owner, "lease_expires_at": &expires, "updated_at": now})
			if result.Error != nil {
				return agentruntime.RuntimeConfigDirectoryOutbox{}, false, result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			row.Status = string(agentruntime.RuntimeConfigDirectoryOutboxProcessing)
			row.Attempt++
			row.Revision++
			row.LeaseOwner = owner
			row.LeaseExpiresAt = &expires
			row.UpdatedAt = now
			item, err := runtimeConfigDirectoryOutboxFromRow(row)
			if err != nil {
				return agentruntime.RuntimeConfigDirectoryOutbox{}, false, err
			}
			return item, true, nil
		}
	}
	return agentruntime.RuntimeConfigDirectoryOutbox{}, false, nil
}

func (r *runtimeRepository) CompleteRuntimeConfigDirectoryOutbox(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDirectoryOutboxOwner {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeConfigDirectoryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	result := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryOutboxProcessing), owner, now).Updates(map[string]any{"status": string(agentruntime.RuntimeConfigDirectoryOutboxCompleted), "lease_owner": "", "lease_expires_at": nil, "revision": row.Revision + 1, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) RetryRuntimeConfigDirectoryOutbox(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDirectoryOutboxOwner {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeConfigDirectoryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > agentruntime.MaxRuntimeConfigDirectoryOutboxError {
		message = message[:agentruntime.MaxRuntimeConfigDirectoryOutboxError]
	}
	status := agentruntime.RuntimeConfigDirectoryOutboxQueued
	availableAt := now.Add(agentruntime.RuntimeConfigDirectoryOutboxBackoff(row.Attempt))
	if row.Attempt >= agentruntime.MaxRuntimeConfigDirectoryOutboxAttempts {
		status = agentruntime.RuntimeConfigDirectoryOutboxFailed
		availableAt = row.AvailableAt
	}
	result := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryOutboxProcessing), owner, now).Updates(map[string]any{"status": string(status), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil, "last_error": message, "revision": row.Revision + 1, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

var _ agentruntime.RuntimeConfigDirectoryOutboxRepository = (*runtimeRepository)(nil)
