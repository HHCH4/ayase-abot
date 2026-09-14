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
	"gorm.io/gorm/clause"
)

// runtimeConfigDirectoryRebindConfirmationRow is the durable, source-local
// record produced by an explicit user confirmation. The idempotency key is
// retained for stable identity checks but never appears in public JSON.
type runtimeConfigDirectoryRebindConfirmationRow struct {
	ID                   string     `gorm:"primaryKey;size:240"`
	PlanID               string     `gorm:"index;size:240;not null"`
	InvocationID         string     `gorm:"index;size:512;not null"`
	Source               string     `gorm:"index;size:160;not null"`
	Destination          string     `gorm:"index;size:160;not null"`
	ConfigSnapshotDigest string     `gorm:"size:71;not null"`
	CapabilitiesDigest   string     `gorm:"size:71;not null"`
	Status               string     `gorm:"index;size:32;not null"`
	Revision             int64      `gorm:"not null"`
	ExpiresAt            time.Time  `gorm:"index;not null"`
	CreatedAt            time.Time  `gorm:"index"`
	ConfirmedAt          time.Time  `gorm:"not null"`
	ConsumedAt           *time.Time `gorm:"index"`
	UpdatedAt            time.Time
	IdempotencyKey       string `gorm:"index;size:200;not null"`
}

func (runtimeConfigDirectoryRebindConfirmationRow) TableName() string {
	return "abot_runtime_config_directory_rebind_confirmations"
}

func runtimeConfigDirectoryRebindConfirmationFromRow(row runtimeConfigDirectoryRebindConfirmationRow) (agentruntime.RuntimeConfigDirectoryRebindConfirmation, error) {
	item := agentruntime.RuntimeConfigDirectoryRebindConfirmation{
		ID: row.ID, PlanID: row.PlanID, InvocationID: row.InvocationID, Source: row.Source,
		Destination: row.Destination, ConfigSnapshotDigest: row.ConfigSnapshotDigest,
		CapabilitiesDigest: row.CapabilitiesDigest, Status: agentruntime.RuntimeConfigDirectoryRebindConfirmationStatus(row.Status),
		Revision: row.Revision, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, ConfirmedAt: row.ConfirmedAt,
		ConsumedAt: cloneTimePtr(row.ConsumedAt), IdempotencyKey: row.IdempotencyKey,
	}
	now := row.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return item.Normalize(now)
}

func runtimeConfigDirectoryRebindConfirmationToRow(item agentruntime.RuntimeConfigDirectoryRebindConfirmation, now time.Time) (*runtimeConfigDirectoryRebindConfirmationRow, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	normalized, err := item.Normalize(now)
	if err != nil {
		return nil, err
	}
	return &runtimeConfigDirectoryRebindConfirmationRow{
		ID: normalized.ID, PlanID: normalized.PlanID, InvocationID: normalized.InvocationID,
		Source: normalized.Source, Destination: normalized.Destination,
		ConfigSnapshotDigest: normalized.ConfigSnapshotDigest, CapabilitiesDigest: normalized.CapabilitiesDigest,
		Status: string(normalized.Status), Revision: normalized.Revision, ExpiresAt: normalized.ExpiresAt,
		CreatedAt: normalized.CreatedAt, ConfirmedAt: normalized.ConfirmedAt,
		UpdatedAt:  normalized.ConfirmedAt,
		ConsumedAt: cloneTimePtr(normalized.ConsumedAt), IdempotencyKey: normalized.IdempotencyKey,
	}, nil
}

// runtimeConfigDirectoryRebindApplyRow stores only a bounded metadata
// projection. The projection is private to the source queue and is sent only
// through the explicitly configured, signed transport.
type runtimeConfigDirectoryRebindApplyRow struct {
	ID                   string     `gorm:"primaryKey;size:260"`
	ParentID             string     `gorm:"index;size:260"`
	PlanID               string     `gorm:"index;size:240;not null"`
	ConfirmationID       string     `gorm:"index;size:240;not null"`
	ConfirmationDigest   string     `gorm:"size:64;not null"`
	Source               string     `gorm:"index;size:160;not null"`
	Destination          string     `gorm:"index;size:160;not null"`
	InvocationID         string     `gorm:"index;size:512;not null"`
	BoundaryStatus       string     `gorm:"index;size:40;not null"`
	ConfigSnapshotDigest string     `gorm:"size:71;not null"`
	SnapshotRevision     int64      `gorm:"not null"`
	EventSequence        int64      `gorm:"not null"`
	SnapshotDigest       string     `gorm:"size:64;not null"`
	ProjectionJSON       string     `gorm:"type:text;not null"`
	Status               string     `gorm:"index;size:32;not null"`
	Attempt              int        `gorm:"not null"`
	Revision             int64      `gorm:"not null"`
	AvailableAt          time.Time  `gorm:"index;not null"`
	LeaseOwner           string     `gorm:"index;size:160"`
	LeaseExpiresAt       *time.Time `gorm:"index"`
	LastError            string     `gorm:"type:text"`
	IdempotencyKey       string     `gorm:"index;size:200;not null"`
	CreatedAt            time.Time  `gorm:"index"`
	UpdatedAt            time.Time
	CompletedAt          *time.Time `gorm:"index"`
}

func (runtimeConfigDirectoryRebindApplyRow) TableName() string {
	return "abot_runtime_config_directory_rebind_applies"
}

func runtimeConfigDirectoryRebindApplyFromRow(row runtimeConfigDirectoryRebindApplyRow) (agentruntime.RuntimeConfigDirectoryRebindApply, error) {
	var projection agentruntime.RuntimeCheckpointProjection
	if strings.TrimSpace(row.ProjectionJSON) == "" {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("解析 Runtime rebind apply projection 失败: body 为空")
	}
	if err := json.Unmarshal([]byte(row.ProjectionJSON), &projection); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, fmt.Errorf("解析 Runtime rebind apply projection 失败: %w", err)
	}
	item := agentruntime.RuntimeConfigDirectoryRebindApply{
		ID: row.ID, ParentID: row.ParentID, PlanID: row.PlanID, ConfirmationID: row.ConfirmationID, ConfirmationDigest: row.ConfirmationDigest,
		Source: row.Source, Destination: row.Destination, InvocationID: row.InvocationID,
		BoundaryStatus: agentruntime.InvocationStatus(row.BoundaryStatus), ConfigSnapshotDigest: row.ConfigSnapshotDigest,
		SnapshotRevision: row.SnapshotRevision, EventSequence: row.EventSequence, SnapshotDigest: row.SnapshotDigest,
		Projection: projection, Status: agentruntime.RuntimeConfigDirectoryRebindApplyStatus(row.Status), Attempt: row.Attempt,
		Revision: row.Revision, AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner,
		LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError, CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt, CompletedAt: cloneTimePtr(row.CompletedAt), IdempotencyKey: row.IdempotencyKey,
	}
	now := row.UpdatedAt
	if now.IsZero() {
		now = row.CreatedAt
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return item.Normalize(now)
}

func runtimeConfigDirectoryRebindApplyToRow(item agentruntime.RuntimeConfigDirectoryRebindApply, now time.Time) (*runtimeConfigDirectoryRebindApplyRow, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	normalized, err := item.Normalize(now)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized.Projection)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime rebind apply projection 失败: %w", err)
	}
	if len(encoded) > agentruntime.MaxRuntimeConfigDirectoryRebindApplyProjectionBytes {
		return nil, fmt.Errorf("%w: projection 超过字节上限", agentruntime.ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	return &runtimeConfigDirectoryRebindApplyRow{
		ID: normalized.ID, ParentID: normalized.ParentID, PlanID: normalized.PlanID, ConfirmationID: normalized.ConfirmationID,
		ConfirmationDigest: normalized.ConfirmationDigest, Source: normalized.Source, Destination: normalized.Destination,
		InvocationID: normalized.InvocationID, BoundaryStatus: string(normalized.BoundaryStatus),
		ConfigSnapshotDigest: normalized.ConfigSnapshotDigest, SnapshotRevision: normalized.SnapshotRevision,
		EventSequence: normalized.EventSequence, SnapshotDigest: normalized.SnapshotDigest, ProjectionJSON: string(encoded),
		Status: string(normalized.Status), Attempt: normalized.Attempt, Revision: normalized.Revision,
		AvailableAt: normalized.AvailableAt, LeaseOwner: normalized.LeaseOwner, LeaseExpiresAt: cloneTimePtr(normalized.LeaseExpiresAt),
		LastError: normalized.LastError, IdempotencyKey: normalized.IdempotencyKey, CreatedAt: normalized.CreatedAt,
		UpdatedAt: normalized.UpdatedAt, CompletedAt: cloneTimePtr(normalized.CompletedAt),
	}, nil
}

type runtimeConfigDirectoryRebindInboxRow struct {
	Version              int       `gorm:"not null"`
	Source               string    `gorm:"index;size:160;not null"`
	Destination          string    `gorm:"index;size:160;not null"`
	PlanID               string    `gorm:"index;size:240;not null"`
	ApplyID              string    `gorm:"primaryKey;size:260"`
	ConfirmationID       string    `gorm:"index;size:240;not null"`
	ConfirmationDigest   string    `gorm:"size:64;not null"`
	InvocationID         string    `gorm:"index;size:512;not null"`
	BoundaryStatus       string    `gorm:"size:40;not null"`
	ConfigSnapshotDigest string    `gorm:"size:71;not null"`
	SnapshotRevision     int64     `gorm:"not null"`
	EventSequence        int64     `gorm:"not null"`
	SnapshotDigest       string    `gorm:"size:64;not null"`
	ProjectionJSON       string    `gorm:"type:text;not null"`
	Signature            string    `gorm:"size:64;not null"`
	ReceivedAt           time.Time `gorm:"index;not null"`
}

func (runtimeConfigDirectoryRebindInboxRow) TableName() string {
	return "abot_runtime_config_directory_rebind_inbox"
}

func runtimeConfigDirectoryRebindInboxFromRow(row runtimeConfigDirectoryRebindInboxRow) (agentruntime.RuntimeConfigDirectoryRebindInboxRecord, error) {
	var projection agentruntime.RuntimeCheckpointProjection
	if err := json.Unmarshal([]byte(row.ProjectionJSON), &projection); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindInboxRecord{}, fmt.Errorf("解析 Runtime rebind inbox projection 失败: %w", err)
	}
	item := agentruntime.RuntimeConfigDirectoryRebindInboxRecord{
		Version: row.Version, Source: row.Source, Destination: row.Destination, PlanID: row.PlanID, ApplyID: row.ApplyID,
		ConfirmationID: row.ConfirmationID, ConfirmationDigest: row.ConfirmationDigest, InvocationID: row.InvocationID,
		BoundaryStatus: agentruntime.InvocationStatus(row.BoundaryStatus), ConfigSnapshotDigest: row.ConfigSnapshotDigest,
		SnapshotRevision: row.SnapshotRevision, EventSequence: row.EventSequence, SnapshotDigest: row.SnapshotDigest,
		Projection: projection, Signature: row.Signature, ReceivedAt: row.ReceivedAt,
	}
	if err := projection.Validate(); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindInboxRecord{}, err
	}
	if _, _, err := agentruntime.NormalizeRuntimeConfigDirectoryRebindApplyPair(agentruntime.RuntimeConfigDirectoryRebindApplyEnvelope{
		Version: agentruntime.RuntimeConfigDirectoryRebindApplyEnvelopeVersion, Source: row.Source, Destination: row.Destination,
		PlanID: row.PlanID, ApplyID: row.ApplyID, ConfirmationID: row.ConfirmationID, ConfirmationDigest: row.ConfirmationDigest,
		InvocationID: row.InvocationID, BoundaryStatus: agentruntime.InvocationStatus(row.BoundaryStatus), ConfigSnapshotDigest: row.ConfigSnapshotDigest,
		SnapshotRevision: row.SnapshotRevision, EventSequence: row.EventSequence, SnapshotDigest: row.SnapshotDigest,
		Timestamp: row.ReceivedAt, IssuedAt: row.ReceivedAt,
	}, projection); err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindInboxRecord{}, err
	}
	return item, nil
}

func runtimeConfigDirectoryRebindInboxToRow(envelope agentruntime.RuntimeConfigDirectoryRebindApplyEnvelope, projection agentruntime.RuntimeCheckpointProjection, receivedAt time.Time) (*runtimeConfigDirectoryRebindInboxRow, error) {
	normalized, projection, err := agentruntime.NormalizeRuntimeConfigDirectoryRebindApplyPair(envelope, projection)
	if err != nil {
		return nil, err
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	} else {
		receivedAt = receivedAt.UTC()
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return nil, fmt.Errorf("编码 Runtime rebind inbox projection 失败: %w", err)
	}
	if len(encoded) > agentruntime.MaxRuntimeConfigDirectoryRebindApplyProjectionBytes {
		return nil, fmt.Errorf("%w: projection 超过字节上限", agentruntime.ErrInvalidRuntimeConfigDirectoryRebindApply)
	}
	return &runtimeConfigDirectoryRebindInboxRow{
		Version: normalized.Version, Source: normalized.Source, Destination: normalized.Destination, PlanID: normalized.PlanID,
		ApplyID: normalized.ApplyID, ConfirmationID: normalized.ConfirmationID, ConfirmationDigest: normalized.ConfirmationDigest,
		InvocationID: normalized.InvocationID, BoundaryStatus: string(normalized.BoundaryStatus), ConfigSnapshotDigest: normalized.ConfigSnapshotDigest,
		SnapshotRevision: normalized.SnapshotRevision, EventSequence: normalized.EventSequence, SnapshotDigest: normalized.SnapshotDigest,
		ProjectionJSON: string(encoded), Signature: normalized.Signature, ReceivedAt: receivedAt,
	}, nil
}

func (r *runtimeRepository) CreateRuntimeConfigDirectoryRebindConfirmation(ctx context.Context, plan agentruntime.RuntimeConfigDirectoryRebindPlan, idempotencyKey string, now time.Time) (agentruntime.RuntimeConfigDirectoryRebindConfirmation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	normalizedPlan, err := plan.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindConfirmation{}, err
	}
	if normalizedPlan.Status != agentruntime.RuntimeConfigDirectoryRebindValidated {
		return agentruntime.RuntimeConfigDirectoryRebindConfirmation{}, agentruntime.ErrRuntimeConfigDirectoryRebindConflict
	}
	confirmation, err := agentruntime.NewRuntimeConfigDirectoryRebindConfirmation(normalizedPlan, idempotencyKey, now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindConfirmation{}, err
	}
	var saved agentruntime.RuntimeConfigDirectoryRebindConfirmation
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existingRow runtimeConfigDirectoryRebindConfirmationRow
		findErr := tx.Where("id = ?", confirmation.ID).First(&existingRow).Error
		if findErr == nil {
			existing, decodeErr := runtimeConfigDirectoryRebindConfirmationFromRow(existingRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !runtimeConfigDirectoryRebindConfirmationIdentityMatches(existing, confirmation) {
				return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict
			}
			if existing.Status == agentruntime.RuntimeConfigDirectoryRebindConfirmationConfirmed && !existing.ExpiresAt.After(now) {
				result := tx.Model(&runtimeConfigDirectoryRebindConfirmationRow{}).Where("id = ? AND revision = ? AND status = ?", existing.ID, existing.Revision, string(agentruntime.RuntimeConfigDirectoryRebindConfirmationConfirmed)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeConfigDirectoryRebindConfirmationExpired), "revision": existing.Revision + 1,
					"updated_at": now,
				})
				if result.Error != nil {
					return result.Error
				}
				return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationExpired
			}
			saved = existing
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var activeRows []runtimeConfigDirectoryRebindConfirmationRow
		if err := tx.Where("plan_id = ? AND status = ?", confirmation.PlanID, string(agentruntime.RuntimeConfigDirectoryRebindConfirmationConfirmed)).Order("created_at ASC").Find(&activeRows).Error; err != nil {
			return err
		}
		for _, activeRow := range activeRows {
			active, decodeErr := runtimeConfigDirectoryRebindConfirmationFromRow(activeRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !active.ExpiresAt.After(now) {
				_ = tx.Model(&runtimeConfigDirectoryRebindConfirmationRow{}).Where("id = ? AND revision = ? AND status = ?", active.ID, active.Revision, string(agentruntime.RuntimeConfigDirectoryRebindConfirmationConfirmed)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeConfigDirectoryRebindConfirmationExpired), "revision": active.Revision + 1,
					"updated_at": now,
				})
				continue
			}
			return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict
		}
		row, rowErr := runtimeConfigDirectoryRebindConfirmationToRow(confirmation, now)
		if rowErr != nil {
			return rowErr
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			saved = confirmation
			return nil
		}
		if err := tx.Where("id = ?", confirmation.ID).First(&existingRow).Error; err != nil {
			return err
		}
		existing, decodeErr := runtimeConfigDirectoryRebindConfirmationFromRow(existingRow)
		if decodeErr != nil {
			return decodeErr
		}
		if !runtimeConfigDirectoryRebindConfirmationIdentityMatches(existing, confirmation) {
			return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict
		}
		saved = existing
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindConfirmation{}, err
	}
	return saved, nil
}

func runtimeConfigDirectoryRebindConfirmationIdentityMatches(left, right agentruntime.RuntimeConfigDirectoryRebindConfirmation) bool {
	leftNormalized, leftErr := left.Normalize(time.Time{})
	rightNormalized, rightErr := right.Normalize(time.Time{})
	if leftErr != nil || rightErr != nil {
		return false
	}
	return leftNormalized.ID == rightNormalized.ID && leftNormalized.PlanID == rightNormalized.PlanID && leftNormalized.InvocationID == rightNormalized.InvocationID && leftNormalized.Source == rightNormalized.Source && leftNormalized.Destination == rightNormalized.Destination && leftNormalized.ConfigSnapshotDigest == rightNormalized.ConfigSnapshotDigest && leftNormalized.CapabilitiesDigest == rightNormalized.CapabilitiesDigest && leftNormalized.IdempotencyKey == rightNormalized.IdempotencyKey
}

func (r *runtimeRepository) GetRuntimeConfigDirectoryRebindConfirmation(ctx context.Context, id string) (agentruntime.RuntimeConfigDirectoryRebindConfirmation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var row runtimeConfigDirectoryRebindConfirmationRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryRebindConfirmation{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryRebindConfirmation{}, err
	}
	return runtimeConfigDirectoryRebindConfirmationFromRow(row)
}

func (r *runtimeRepository) CommitRuntimeConfigDirectoryRebindApply(ctx context.Context, confirmationID string, apply agentruntime.RuntimeConfigDirectoryRebindApply, now time.Time) (agentruntime.RuntimeConfigDirectoryRebindApply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	normalized, err := apply.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, err
	}
	confirmationID = strings.TrimSpace(confirmationID)
	if confirmationID == "" || confirmationID != normalized.ConfirmationID {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict
	}
	var saved agentruntime.RuntimeConfigDirectoryRebindApply
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existingRow runtimeConfigDirectoryRebindApplyRow
		findErr := tx.Where("id = ?", normalized.ID).First(&existingRow).Error
		if findErr == nil {
			existing, decodeErr := runtimeConfigDirectoryRebindApplyFromRow(existingRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !existing.MatchesIdentity(normalized) {
				return agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
			}
			saved = existing
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var confirmationRow runtimeConfigDirectoryRebindConfirmationRow
		if err := tx.Where("id = ?", confirmationID).First(&confirmationRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		confirmation, decodeErr := runtimeConfigDirectoryRebindConfirmationFromRow(confirmationRow)
		if decodeErr != nil {
			return decodeErr
		}
		if confirmation.Status == agentruntime.RuntimeConfigDirectoryRebindConfirmationConfirmed && !confirmation.ExpiresAt.After(now) {
			result := tx.Model(&runtimeConfigDirectoryRebindConfirmationRow{}).Where("id = ? AND revision = ? AND status = ?", confirmation.ID, confirmation.Revision, string(agentruntime.RuntimeConfigDirectoryRebindConfirmationConfirmed)).Updates(map[string]any{
				"status": string(agentruntime.RuntimeConfigDirectoryRebindConfirmationExpired), "revision": confirmation.Revision + 1, "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationExpired
		}
		if confirmation.Status != agentruntime.RuntimeConfigDirectoryRebindConfirmationConfirmed {
			return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict
		}
		confirmationDigest, digestErr := agentruntime.RuntimeConfigDirectoryRebindConfirmationDigest(confirmation)
		if digestErr != nil || confirmationDigest != normalized.ConfirmationDigest {
			return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict
		}
		if confirmation.PlanID != normalized.PlanID || confirmation.InvocationID != normalized.InvocationID || confirmation.Source != normalized.Source || confirmation.Destination != normalized.Destination || confirmation.ConfigSnapshotDigest != normalized.ConfigSnapshotDigest {
			return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict
		}
		var pendingCount int64
		if err := tx.Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("plan_id = ? AND destination = ? AND snapshot_revision = ? AND event_sequence = ? AND snapshot_digest = ? AND status <> ?", normalized.PlanID, normalized.Destination, normalized.SnapshotRevision, normalized.EventSequence, normalized.SnapshotDigest, string(agentruntime.RuntimeConfigDirectoryRebindApplyFailed)).Count(&pendingCount).Error; err != nil {
			return err
		}
		if pendingCount > 0 {
			return agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
		}
		row, rowErr := runtimeConfigDirectoryRebindApplyToRow(normalized, now)
		if rowErr != nil {
			return rowErr
		}
		consumedAt := now
		result := tx.Model(&runtimeConfigDirectoryRebindConfirmationRow{}).Where("id = ? AND revision = ? AND status = ?", confirmation.ID, confirmation.Revision, string(agentruntime.RuntimeConfigDirectoryRebindConfirmationConfirmed)).Updates(map[string]any{
			"status": string(agentruntime.RuntimeConfigDirectoryRebindConfirmationConsumed), "consumed_at": &consumedAt,
			"revision": confirmation.Revision + 1, "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict
		}
		if err := tx.Create(row).Error; err != nil {
			return err
		}
		saved = normalized
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) EnqueueRuntimeConfigDirectoryRebindApply(ctx context.Context, item agentruntime.RuntimeConfigDirectoryRebindApply) (agentruntime.RuntimeConfigDirectoryRebindApply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC()
	normalized, err := item.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, err
	}
	if normalized.Status != agentruntime.RuntimeConfigDirectoryRebindApplyQueued || normalized.Attempt != 0 {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	row, err := runtimeConfigDirectoryRebindApplyToRow(normalized, now)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, err
	}
	var saved agentruntime.RuntimeConfigDirectoryRebindApply
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		var storedRow runtimeConfigDirectoryRebindApplyRow
		if err := tx.Where("id = ?", normalized.ID).First(&storedRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		stored, decodeErr := runtimeConfigDirectoryRebindApplyFromRow(storedRow)
		if decodeErr != nil {
			return decodeErr
		}
		if !stored.MatchesIdentity(normalized) {
			return agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
		}
		saved = stored
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeConfigDirectoryRebindApply(ctx context.Context, id string) (agentruntime.RuntimeConfigDirectoryRebindApply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var row runtimeConfigDirectoryRebindApplyRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryRebindApply{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, err
	}
	return runtimeConfigDirectoryRebindApplyFromRow(row)
}

func (r *runtimeRepository) ListRuntimeConfigDirectoryRebindApplies(ctx context.Context, planID, invocationID string, status agentruntime.RuntimeConfigDirectoryRebindApplyStatus, limit int) ([]agentruntime.RuntimeConfigDirectoryRebindApply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if status != "" {
		switch status {
		case agentruntime.RuntimeConfigDirectoryRebindApplyQueued, agentruntime.RuntimeConfigDirectoryRebindApplyProcessing, agentruntime.RuntimeConfigDirectoryRebindApplyCompleted, agentruntime.RuntimeConfigDirectoryRebindApplyFailed:
		default:
			return nil, agentruntime.ErrInvalidRuntimeConfigDirectoryRebindApply
		}
	}
	query := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryRebindApplyRow{})
	if planID = strings.TrimSpace(planID); planID != "" {
		query = query.Where("plan_id = ?", planID)
	}
	if invocationID = strings.TrimSpace(invocationID); invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeConfigDirectoryRebindApplyRow
	if err := query.Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeConfigDirectoryRebindApply, 0, len(rows))
	for _, row := range rows {
		item, err := runtimeConfigDirectoryRebindApplyFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *runtimeRepository) ClaimRuntimeConfigDirectoryRebindApply(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeConfigDirectoryRebindApply, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDirectoryRebindApplyOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeConfigDirectoryRebindApplyLease {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, false, agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var claimed agentruntime.RuntimeConfigDirectoryRebindApply
	var won bool
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var rows []runtimeConfigDirectoryRebindApplyRow
		if err := tx.Where("(status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))", string(agentruntime.RuntimeConfigDirectoryRebindApplyQueued), now, string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing), now).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if row.Status == string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing) && (strings.TrimSpace(row.LeaseOwner) == "" || row.LeaseExpiresAt == nil || row.LeaseExpiresAt.IsZero()) {
				result := tx.Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("id = ? AND revision = ? AND status = ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeConfigDirectoryRebindApplyFailed), "lease_owner": "", "lease_expires_at": nil,
					"last_error": "Runtime rebind apply processing 元数据无效", "revision": row.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return result.Error
				}
				continue
			}
			if row.Attempt >= agentruntime.MaxRuntimeConfigDirectoryRebindApplyAttempts {
				result := tx.Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("id = ? AND revision = ? AND status IN (?, ?)", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryRebindApplyQueued), string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeConfigDirectoryRebindApplyFailed), "lease_owner": "", "lease_expires_at": nil,
					"last_error": "Runtime rebind apply 达到最大投递次数", "revision": row.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			result := tx.Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("id = ? AND revision = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryRebindApplyQueued), now, string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing), now).Updates(map[string]any{
				"status": string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing), "attempt": row.Attempt + 1, "revision": row.Revision + 1,
				"lease_owner": owner, "lease_expires_at": &expires, "completed_at": nil, "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			row.Status = string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing)
			row.Attempt++
			row.Revision++
			row.LeaseOwner = owner
			row.LeaseExpiresAt = &expires
			row.UpdatedAt = now
			item, decodeErr := runtimeConfigDirectoryRebindApplyFromRow(row)
			if decodeErr != nil {
				return decodeErr
			}
			claimed = item
			won = true
			return nil
		}
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRebindApply{}, false, err
	}
	return claimed, won, nil
}

func (r *runtimeRepository) CompleteRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	owner, now, err := normalizeRuntimeConfigDirectoryRebindApplyMutation(owner, now)
	if err != nil {
		return false, err
	}
	var row runtimeConfigDirectoryRebindApplyRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	finished := now
	result := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeConfigDirectoryRebindApplyCompleted), "lease_owner": "", "lease_expires_at": nil,
		"completed_at": &finished, "revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) RetryRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner, now, err := normalizeRuntimeConfigDirectoryRebindApplyMutation(owner, now)
	if err != nil {
		return false, err
	}
	var row runtimeConfigDirectoryRebindApplyRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > agentruntime.MaxRuntimeConfigDirectoryRebindApplyErrorLength {
		message = message[:agentruntime.MaxRuntimeConfigDirectoryRebindApplyErrorLength]
	}
	status := agentruntime.RuntimeConfigDirectoryRebindApplyQueued
	availableAt := now.Add(agentruntime.RuntimeConfigDirectoryRebindApplyBackoff(row.Attempt))
	if row.Attempt >= agentruntime.MaxRuntimeConfigDirectoryRebindApplyAttempts {
		status = agentruntime.RuntimeConfigDirectoryRebindApplyFailed
		availableAt = row.AvailableAt
	}
	result := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing), owner, now).Updates(map[string]any{
		"status": string(status), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil,
		"last_error": message, "revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) FailRuntimeConfigDirectoryRebindApply(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner, now, err := normalizeRuntimeConfigDirectoryRebindApplyMutation(owner, now)
	if err != nil {
		return false, err
	}
	var row runtimeConfigDirectoryRebindApplyRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > agentruntime.MaxRuntimeConfigDirectoryRebindApplyErrorLength {
		message = message[:agentruntime.MaxRuntimeConfigDirectoryRebindApplyErrorLength]
	}
	result := r.db.WithContext(ctx).Model(&runtimeConfigDirectoryRebindApplyRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDirectoryRebindApplyProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeConfigDirectoryRebindApplyFailed), "lease_owner": "", "lease_expires_at": nil,
		"last_error": message, "revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func normalizeRuntimeConfigDirectoryRebindApplyMutation(owner string, now time.Time) (string, time.Time, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDirectoryRebindApplyOwnerLength {
		return "", time.Time{}, agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return owner, now, nil
}

func (r *runtimeRepository) AcceptRuntimeConfigDirectoryRebind(ctx context.Context, envelope agentruntime.RuntimeConfigDirectoryRebindApplyEnvelope, projection agentruntime.RuntimeCheckpointProjection) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, projection, err := agentruntime.NormalizeRuntimeConfigDirectoryRebindApplyPair(envelope, projection)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	row, err := runtimeConfigDirectoryRebindInboxToRow(normalized, projection, now)
	if err != nil {
		return false, err
	}
	var duplicate bool
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existingRow runtimeConfigDirectoryRebindInboxRow
		findErr := tx.Where("apply_id = ?", normalized.ApplyID).First(&existingRow).Error
		if findErr == nil {
			existing, decodeErr := runtimeConfigDirectoryRebindInboxFromRow(existingRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !existing.Matches(normalized, projection) {
				return agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
			}
			duplicate = true
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			duplicate = false
			return nil
		}
		if err := tx.Where("apply_id = ?", normalized.ApplyID).First(&existingRow).Error; err != nil {
			return err
		}
		existing, decodeErr := runtimeConfigDirectoryRebindInboxFromRow(existingRow)
		if decodeErr != nil {
			return decodeErr
		}
		if !existing.Matches(normalized, projection) {
			return agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict
		}
		duplicate = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return duplicate, nil
}

func (r *runtimeRepository) GetRuntimeConfigDirectoryRebind(ctx context.Context, id string) (agentruntime.RuntimeConfigDirectoryRebindInboxRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var row runtimeConfigDirectoryRebindInboxRow
	if err := r.db.WithContext(ctx).Where("apply_id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryRebindInboxRecord{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryRebindInboxRecord{}, err
	}
	return runtimeConfigDirectoryRebindInboxFromRow(row)
}

var _ agentruntime.RuntimeConfigDirectoryRebindConfirmationRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindApplyRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindApplyCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindInbox = (*runtimeRepository)(nil)
