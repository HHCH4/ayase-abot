package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
)

// The three tables deliberately retain only the bounded RuntimeConfigSnapshot
// projection and envelope metadata. Secrets, prompt text, tool arguments and
// command output never cross this boundary.
type runtimeConfigDeliveryInboxRow struct {
	Version         int    `gorm:"not null"`
	InvocationID    string `gorm:"primaryKey;size:512"`
	DeliveryID      string `gorm:"index;size:220;not null"`
	Source          string `gorm:"index;size:160;not null"`
	Destination     string `gorm:"index;size:160;not null"`
	ExpectedDigest  string `gorm:"size:71;not null"`
	SnapshotDigest  string `gorm:"size:71;not null"`
	SnapshotVersion int    `gorm:"not null"`
	IdempotencyKey  string `gorm:"size:200"`
	ProjectionJSON  string `gorm:"type:text;not null"`
	Signature       string `gorm:"size:64"`
	ReceivedAt      time.Time
}

func (runtimeConfigDeliveryInboxRow) TableName() string {
	return "abot_agent_config_delivery_inbox"
}

type runtimeConfigDeliveryOutboxRow struct {
	ID              string     `gorm:"primaryKey;size:220"`
	GroupID         string     `gorm:"index;size:220"`
	InvocationID    string     `gorm:"index:idx_runtime_config_outbox_invocation,priority:1;size:512;not null"`
	Source          string     `gorm:"index;size:160;not null"`
	Destination     string     `gorm:"index;size:160;not null"`
	DeliveryID      string     `gorm:"index;size:220;not null"`
	ExpectedDigest  string     `gorm:"size:71;not null"`
	SnapshotDigest  string     `gorm:"size:71;not null"`
	SnapshotVersion int        `gorm:"not null"`
	IdempotencyKey  string     `gorm:"size:200"`
	ProjectionJSON  string     `gorm:"type:text;not null"`
	Status          string     `gorm:"index;size:32;not null"`
	Attempt         int        `gorm:"not null"`
	Revision        int64      `gorm:"not null"`
	AvailableAt     time.Time  `gorm:"index"`
	LeaseOwner      string     `gorm:"index;size:160"`
	LeaseExpiresAt  *time.Time `gorm:"index"`
	LastError       string     `gorm:"type:text"`
	CreatedAt       time.Time  `gorm:"index:idx_runtime_config_outbox_invocation,priority:2"`
	UpdatedAt       time.Time
}

func (runtimeConfigDeliveryOutboxRow) TableName() string {
	return "abot_agent_config_delivery_outbox"
}

type runtimeConfigDeliveryTransactionRow struct {
	Version         int    `gorm:"not null"`
	DeliveryID      string `gorm:"primaryKey;size:220"`
	Source          string `gorm:"index;size:160;not null"`
	Destination     string `gorm:"index;size:160;not null"`
	InvocationID    string `gorm:"index;size:512;not null"`
	ExpectedDigest  string `gorm:"size:71;not null"`
	SnapshotDigest  string `gorm:"size:71;not null"`
	SnapshotVersion int    `gorm:"not null"`
	IdempotencyKey  string `gorm:"size:200"`
	Signature       string `gorm:"size:64"`
	Timestamp       time.Time
	IssuedAt        time.Time
	ProjectionJSON  string    `gorm:"type:text;not null"`
	Status          string    `gorm:"index;size:32;not null"`
	ExpiresAt       time.Time `gorm:"index"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (runtimeConfigDeliveryTransactionRow) TableName() string {
	return "abot_agent_config_delivery_transactions"
}

func normalizeSQLiteRuntimeConfigDeliveryPair(envelope agentruntime.RuntimeConfigDeliveryEnvelope, projection agentruntime.RuntimeConfigDeliveryProjection) (agentruntime.RuntimeConfigDeliveryEnvelope, agentruntime.RuntimeConfigDeliveryProjection, error) {
	normalized, err := agentruntime.NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return agentruntime.RuntimeConfigDeliveryEnvelope{}, agentruntime.RuntimeConfigDeliveryProjection{}, err
	}
	encoded, err := json.Marshal(projection)
	if err != nil || len(encoded) > agentruntime.MaxRuntimeConfigDeliveryProjectionBytes {
		return agentruntime.RuntimeConfigDeliveryEnvelope{}, agentruntime.RuntimeConfigDeliveryProjection{}, fmt.Errorf("%w: projection 编码无效或超限", agentruntime.ErrInvalidRuntimeConfigDelivery)
	}
	parsed, err := agent.ParseRuntimeConfigSnapshot(string(encoded))
	if err != nil {
		return agentruntime.RuntimeConfigDeliveryEnvelope{}, agentruntime.RuntimeConfigDeliveryProjection{}, err
	}
	canonical, err := json.Marshal(parsed)
	if err != nil || agent.RuntimeConfigSnapshotDigest(string(canonical)) != normalized.SnapshotDigest || parsed.Version != normalized.SnapshotVersion {
		return agentruntime.RuntimeConfigDeliveryEnvelope{}, agentruntime.RuntimeConfigDeliveryProjection{}, fmt.Errorf("%w: projection digest/version 不一致", agentruntime.ErrRuntimeConfigDeliveryAuth)
	}
	return normalized, parsed, nil
}

func runtimeConfigDeliveryInboxFromRow(row runtimeConfigDeliveryInboxRow) (agentruntime.RuntimeConfigDeliveryInboxRecord, error) {
	var projection agentruntime.RuntimeConfigDeliveryProjection
	if err := json.Unmarshal([]byte(row.ProjectionJSON), &projection); err != nil {
		return agentruntime.RuntimeConfigDeliveryInboxRecord{}, err
	}
	return agentruntime.RuntimeConfigDeliveryInboxRecord{
		Version: row.Version, DeliveryID: row.DeliveryID, Source: row.Source, Destination: row.Destination,
		InvocationID: row.InvocationID, ExpectedDigest: row.ExpectedDigest, SnapshotDigest: row.SnapshotDigest,
		SnapshotVersion: row.SnapshotVersion, IdempotencyKey: row.IdempotencyKey, Projection: projection,
		Signature: row.Signature, ReceivedAt: row.ReceivedAt,
	}, nil
}

func runtimeConfigDeliveryInboxToRow(item agentruntime.RuntimeConfigDeliveryInboxRecord) (*runtimeConfigDeliveryInboxRow, error) {
	encoded, err := json.Marshal(item.Projection)
	if err != nil {
		return nil, err
	}
	if len(encoded) > agentruntime.MaxRuntimeConfigDeliveryProjectionBytes {
		return nil, fmt.Errorf("%w: projection 超过字节上限", agentruntime.ErrInvalidRuntimeConfigDelivery)
	}
	return &runtimeConfigDeliveryInboxRow{
		Version: item.Version, InvocationID: item.InvocationID, DeliveryID: item.DeliveryID, Source: item.Source,
		Destination: item.Destination, ExpectedDigest: item.ExpectedDigest, SnapshotDigest: item.SnapshotDigest,
		SnapshotVersion: item.SnapshotVersion, IdempotencyKey: item.IdempotencyKey, ProjectionJSON: string(encoded),
		Signature: item.Signature, ReceivedAt: item.ReceivedAt,
	}, nil
}

func runtimeConfigDeliveryOutboxFromRow(row runtimeConfigDeliveryOutboxRow) (agentruntime.RuntimeConfigDeliveryOutbox, error) {
	var projection agentruntime.RuntimeConfigDeliveryProjection
	if err := json.Unmarshal([]byte(row.ProjectionJSON), &projection); err != nil {
		return agentruntime.RuntimeConfigDeliveryOutbox{}, err
	}
	item := agentruntime.RuntimeConfigDeliveryOutbox{
		ID: row.ID, GroupID: row.GroupID, InvocationID: row.InvocationID, Source: row.Source, Destination: row.Destination,
		DeliveryID: row.DeliveryID, ExpectedDigest: row.ExpectedDigest, SnapshotDigest: row.SnapshotDigest,
		SnapshotVersion: row.SnapshotVersion, IdempotencyKey: row.IdempotencyKey, Projection: projection,
		Status: agentruntime.RuntimeConfigDeliveryOutboxStatus(row.Status), Attempt: row.Attempt, Revision: row.Revision,
		AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner, LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt),
		LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	return item.Normalize(time.Now().UTC())
}

func runtimeConfigDeliveryOutboxToRow(item agentruntime.RuntimeConfigDeliveryOutbox) (*runtimeConfigDeliveryOutboxRow, error) {
	encoded, err := json.Marshal(item.Projection)
	if err != nil {
		return nil, err
	}
	if len(encoded) > agentruntime.MaxRuntimeConfigDeliveryProjectionBytes {
		return nil, fmt.Errorf("%w: projection 超过字节上限", agentruntime.ErrInvalidRuntimeConfigDelivery)
	}
	return &runtimeConfigDeliveryOutboxRow{
		ID: item.ID, GroupID: item.GroupID, InvocationID: item.InvocationID, Source: item.Source, Destination: item.Destination,
		DeliveryID: item.DeliveryID, ExpectedDigest: item.ExpectedDigest, SnapshotDigest: item.SnapshotDigest,
		SnapshotVersion: item.SnapshotVersion, IdempotencyKey: item.IdempotencyKey, ProjectionJSON: string(encoded),
		Status: string(item.Status), Attempt: item.Attempt, Revision: item.Revision, AvailableAt: item.AvailableAt,
		LeaseOwner: item.LeaseOwner, LeaseExpiresAt: cloneTimePtr(item.LeaseExpiresAt), LastError: item.LastError,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

func runtimeConfigDeliveryTransactionFromRow(row runtimeConfigDeliveryTransactionRow) (agentruntime.RuntimeConfigDeliveryTransaction, error) {
	var projection agentruntime.RuntimeConfigDeliveryProjection
	if err := json.Unmarshal([]byte(row.ProjectionJSON), &projection); err != nil {
		return agentruntime.RuntimeConfigDeliveryTransaction{}, err
	}
	return agentruntime.RuntimeConfigDeliveryTransaction{
		Version: row.Version, DeliveryID: row.DeliveryID, Source: row.Source, Destination: row.Destination,
		InvocationID: row.InvocationID, ExpectedDigest: row.ExpectedDigest, SnapshotDigest: row.SnapshotDigest,
		SnapshotVersion: row.SnapshotVersion, IdempotencyKey: row.IdempotencyKey, Signature: row.Signature,
		Timestamp: row.Timestamp, IssuedAt: row.IssuedAt, Projection: projection,
		Status: agentruntime.RuntimeConfigDeliveryTransactionStatus(row.Status), ExpiresAt: row.ExpiresAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func runtimeConfigDeliveryTransactionToRow(item agentruntime.RuntimeConfigDeliveryTransaction) (*runtimeConfigDeliveryTransactionRow, error) {
	encoded, err := json.Marshal(item.Projection)
	if err != nil {
		return nil, err
	}
	if len(encoded) > agentruntime.MaxRuntimeConfigDeliveryProjectionBytes {
		return nil, fmt.Errorf("%w: projection 超过字节上限", agentruntime.ErrInvalidRuntimeConfigDelivery)
	}
	return &runtimeConfigDeliveryTransactionRow{
		Version: item.Version, DeliveryID: item.DeliveryID, Source: item.Source, Destination: item.Destination,
		InvocationID: item.InvocationID, ExpectedDigest: item.ExpectedDigest, SnapshotDigest: item.SnapshotDigest,
		SnapshotVersion: item.SnapshotVersion, IdempotencyKey: item.IdempotencyKey, Signature: item.Signature,
		Timestamp: item.Timestamp, IssuedAt: item.IssuedAt, ProjectionJSON: string(encoded), Status: string(item.Status),
		ExpiresAt: item.ExpiresAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

func acceptRuntimeConfigDeliveryTx(tx *gorm.DB, envelope agentruntime.RuntimeConfigDeliveryEnvelope, projection agentruntime.RuntimeConfigDeliveryProjection, receivedAt time.Time) (bool, error) {
	record := agentruntime.RuntimeConfigDeliveryInboxRecordFromEnvelope(envelope, projection, receivedAt)
	row, err := runtimeConfigDeliveryInboxToRow(record)
	if err != nil {
		return false, err
	}
	var existingRow runtimeConfigDeliveryInboxRow
	findErr := tx.Where("invocation_id = ?", envelope.InvocationID).First(&existingRow).Error
	if findErr == nil {
		existing, decodeErr := runtimeConfigDeliveryInboxFromRow(existingRow)
		if decodeErr != nil {
			return false, decodeErr
		}
		if existing.Matches(envelope, projection) {
			return true, nil
		}
		if existing.Source != envelope.Source || existing.Destination != envelope.Destination {
			return false, agentruntime.ErrConflict
		}
		if envelope.ExpectedDigest != existing.SnapshotDigest {
			return false, agentruntime.ErrRuntimeConfigDeliveryStale
		}
		result := tx.Model(&runtimeConfigDeliveryInboxRow{}).Where("invocation_id = ?", envelope.InvocationID).Updates(map[string]any{
			"version": envelope.Version, "delivery_id": envelope.DeliveryID, "source": envelope.Source, "destination": envelope.Destination,
			"expected_digest": envelope.ExpectedDigest, "snapshot_digest": envelope.SnapshotDigest, "snapshot_version": envelope.SnapshotVersion,
			"idempotency_key": envelope.IdempotencyKey, "projection_json": row.ProjectionJSON, "signature": envelope.Signature, "received_at": row.ReceivedAt,
		})
		if result.Error != nil {
			return false, result.Error
		}
		if result.RowsAffected != 1 {
			return false, agentruntime.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return false, findErr
	}
	return false, tx.Create(row).Error
}

func (r *runtimeRepository) AcceptRuntimeConfigDelivery(ctx context.Context, envelope agentruntime.RuntimeConfigDeliveryEnvelope, projection agentruntime.RuntimeConfigDeliveryProjection) (bool, error) {
	normalized, parsed, err := normalizeSQLiteRuntimeConfigDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	duplicate := false
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var acceptErr error
		duplicate, acceptErr = acceptRuntimeConfigDeliveryTx(tx, normalized, parsed, time.Now().UTC())
		return acceptErr
	})
	return duplicate, err
}

func (r *runtimeRepository) GetRuntimeConfigDelivery(ctx context.Context, invocationID string) (agentruntime.RuntimeConfigDeliveryInboxRecord, error) {
	var row runtimeConfigDeliveryInboxRow
	if err := r.db.WithContext(ctx).Where("invocation_id = ?", strings.TrimSpace(invocationID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDeliveryInboxRecord{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDeliveryInboxRecord{}, err
	}
	return runtimeConfigDeliveryInboxFromRow(row)
}

func (r *runtimeRepository) PrepareRuntimeConfigDelivery(ctx context.Context, envelope agentruntime.RuntimeConfigDeliveryEnvelope, projection agentruntime.RuntimeConfigDeliveryProjection) (bool, error) {
	normalized, parsed, err := normalizeSQLiteRuntimeConfigDeliveryPair(envelope, projection)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existingRow runtimeConfigDeliveryTransactionRow
		findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&existingRow).Error
		if findErr == nil {
			existing, decodeErr := runtimeConfigDeliveryTransactionFromRow(existingRow)
			if decodeErr != nil {
				return decodeErr
			}
			if !existing.Matches(normalized, parsed) {
				return agentruntime.ErrConflict
			}
			if existing.Status == agentruntime.RuntimeConfigDeliveryTransactionCommitted {
				duplicate = true
				return nil
			}
			if existing.Status != agentruntime.RuntimeConfigDeliveryTransactionPrepared {
				return agentruntime.ErrConflict
			}
			if existing.ExpiresAt.After(now) {
				duplicate = true
				return nil
			}
		}
		prepared, createErr := agentruntime.NewRuntimeConfigDeliveryTransaction(normalized, parsed, now)
		if createErr != nil {
			return createErr
		}
		row, rowErr := runtimeConfigDeliveryTransactionToRow(prepared)
		if rowErr != nil {
			return rowErr
		}
		if findErr == nil {
			return tx.Model(&runtimeConfigDeliveryTransactionRow{}).Where("delivery_id = ?", normalized.DeliveryID).Updates(map[string]any{
				"version": row.Version, "source": row.Source, "destination": row.Destination, "invocation_id": row.InvocationID,
				"expected_digest": row.ExpectedDigest, "snapshot_digest": row.SnapshotDigest, "snapshot_version": row.SnapshotVersion,
				"idempotency_key": row.IdempotencyKey, "signature": row.Signature, "timestamp": row.Timestamp, "issued_at": row.IssuedAt,
				"projection_json": row.ProjectionJSON, "status": row.Status, "expires_at": row.ExpiresAt, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
			}).Error
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		return tx.Create(row).Error
	})
	return duplicate, err
}

func (r *runtimeRepository) CommitRuntimeConfigDelivery(ctx context.Context, envelope agentruntime.RuntimeConfigDeliveryEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeConfigDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row runtimeConfigDeliveryTransactionRow
		if findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		transaction, decodeErr := runtimeConfigDeliveryTransactionFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !transaction.Matches(normalized, transaction.Projection) {
			return agentruntime.ErrConflict
		}
		if transaction.Status == agentruntime.RuntimeConfigDeliveryTransactionCommitted {
			duplicate = true
			return nil
		}
		if transaction.Status != agentruntime.RuntimeConfigDeliveryTransactionPrepared {
			return agentruntime.ErrConflict
		}
		if !transaction.ExpiresAt.After(now) {
			return agentruntime.ErrRuntimeConfigDeliveryStale
		}
		duplicate, err = acceptRuntimeConfigDeliveryTx(tx, transaction.Envelope(), transaction.Projection, now)
		if err != nil {
			return err
		}
		result := tx.Model(&runtimeConfigDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeConfigDeliveryTransactionPrepared)).Updates(map[string]any{
			"status": string(agentruntime.RuntimeConfigDeliveryTransactionCommitted), "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentruntime.ErrConflict
		}
		return nil
	})
	return duplicate, err
}

func (r *runtimeRepository) GetRuntimeConfigDeliveryTransaction(ctx context.Context, deliveryID string) (agentruntime.RuntimeConfigDeliveryTransaction, error) {
	var row runtimeConfigDeliveryTransactionRow
	if err := r.db.WithContext(ctx).Where("delivery_id = ?", strings.TrimSpace(deliveryID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDeliveryTransaction{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDeliveryTransaction{}, err
	}
	return runtimeConfigDeliveryTransactionFromRow(row)
}

func (r *runtimeRepository) EnqueueRuntimeConfigDeliveryOutbox(ctx context.Context, item agentruntime.RuntimeConfigDeliveryOutbox) (agentruntime.RuntimeConfigDeliveryOutbox, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.RuntimeConfigDeliveryOutbox{}, err
	}
	var saved agentruntime.RuntimeConfigDeliveryOutbox
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing runtimeConfigDeliveryOutboxRow
		findErr := tx.Where("id = ?", normalized.ID).First(&existing).Error
		if findErr == nil {
			stored, decodeErr := runtimeConfigDeliveryOutboxFromRow(existing)
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
		var same runtimeConfigDeliveryOutboxRow
		identityErr := tx.Where("invocation_id = ? AND source = ? AND destination = ? AND expected_digest = ?", normalized.InvocationID, normalized.Source, normalized.Destination, normalized.ExpectedDigest).First(&same).Error
		if identityErr == nil {
			stored, decodeErr := runtimeConfigDeliveryOutboxFromRow(same)
			if decodeErr != nil {
				return decodeErr
			}
			if stored.SnapshotDigest != normalized.SnapshotDigest || stored.IdempotencyKey != normalized.IdempotencyKey {
				return agentruntime.ErrConflict
			}
			saved = stored
			return nil
		}
		if !errors.Is(identityErr, gorm.ErrRecordNotFound) {
			return identityErr
		}
		if normalized.Status != agentruntime.RuntimeConfigDeliveryOutboxQueued || normalized.Attempt != 0 {
			return agentruntime.ErrConflict
		}
		row, rowErr := runtimeConfigDeliveryOutboxToRow(normalized)
		if rowErr != nil {
			return rowErr
		}
		if createErr := tx.Create(row).Error; createErr != nil {
			return createErr
		}
		saved = normalized
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeConfigDeliveryOutbox{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeConfigDeliveryOutbox(ctx context.Context, id string) (agentruntime.RuntimeConfigDeliveryOutbox, error) {
	var row runtimeConfigDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDeliveryOutbox{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDeliveryOutbox{}, err
	}
	return runtimeConfigDeliveryOutboxFromRow(row)
}

func (r *runtimeRepository) ListRuntimeConfigDeliveryOutbox(ctx context.Context, invocationID string, status agentruntime.RuntimeConfigDeliveryOutboxStatus, limit int) ([]agentruntime.RuntimeConfigDeliveryOutbox, error) {
	invocationID = strings.TrimSpace(invocationID)
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&runtimeConfigDeliveryOutboxRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeConfigDeliveryOutboxRow
	if err := query.Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeConfigDeliveryOutbox, 0, len(rows))
	for _, row := range rows {
		item, err := runtimeConfigDeliveryOutboxFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *runtimeRepository) ClaimRuntimeConfigDeliveryOutbox(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeConfigDeliveryOutbox, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDeliveryOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeConfigDeliveryLease {
		return agentruntime.RuntimeConfigDeliveryOutbox{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for round := 0; round < 4; round++ {
		var rows []runtimeConfigDeliveryOutboxRow
		if err := r.db.WithContext(ctx).Where("(status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))", string(agentruntime.RuntimeConfigDeliveryOutboxQueued), now, string(agentruntime.RuntimeConfigDeliveryOutboxProcessing), now).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return agentruntime.RuntimeConfigDeliveryOutbox{}, false, err
		}
		if len(rows) == 0 {
			return agentruntime.RuntimeConfigDeliveryOutbox{}, false, nil
		}
		for _, row := range rows {
			if row.Status == string(agentruntime.RuntimeConfigDeliveryOutboxProcessing) && (strings.TrimSpace(row.LeaseOwner) == "" || row.LeaseExpiresAt == nil || row.LeaseExpiresAt.IsZero()) {
				result := r.db.WithContext(ctx).Model(&runtimeConfigDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDeliveryOutboxProcessing)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeConfigDeliveryOutboxFailed), "lease_owner": "", "lease_expires_at": nil, "last_error": "config outbox processing 元数据无效", "revision": row.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return agentruntime.RuntimeConfigDeliveryOutbox{}, false, result.Error
				}
				continue
			}
			if row.Attempt >= agentruntime.MaxRuntimeConfigDeliveryAttempts {
				result := r.db.WithContext(ctx).Model(&runtimeConfigDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status IN (?, ?)", row.ID, row.Revision, string(agentruntime.RuntimeConfigDeliveryOutboxQueued), string(agentruntime.RuntimeConfigDeliveryOutboxProcessing)).Updates(map[string]any{
					"status": string(agentruntime.RuntimeConfigDeliveryOutboxFailed), "lease_owner": "", "lease_expires_at": nil, "last_error": "Runtime config delivery 达到最大投递次数", "revision": row.Revision + 1, "updated_at": now,
				})
				if result.Error != nil {
					return agentruntime.RuntimeConfigDeliveryOutbox{}, false, result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			result := r.db.WithContext(ctx).Model(&runtimeConfigDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", row.ID, row.Revision, string(agentruntime.RuntimeConfigDeliveryOutboxQueued), now, string(agentruntime.RuntimeConfigDeliveryOutboxProcessing), now).Updates(map[string]any{
				"status": string(agentruntime.RuntimeConfigDeliveryOutboxProcessing), "attempt": row.Attempt + 1, "revision": row.Revision + 1, "lease_owner": owner, "lease_expires_at": &expires, "updated_at": now,
			})
			if result.Error != nil {
				return agentruntime.RuntimeConfigDeliveryOutbox{}, false, result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			row.Status = string(agentruntime.RuntimeConfigDeliveryOutboxProcessing)
			row.Attempt++
			row.Revision++
			row.LeaseOwner = owner
			row.LeaseExpiresAt = &expires
			row.UpdatedAt = now
			item, err := runtimeConfigDeliveryOutboxFromRow(row)
			if err != nil {
				return agentruntime.RuntimeConfigDeliveryOutbox{}, false, err
			}
			return item, true, nil
		}
	}
	return agentruntime.RuntimeConfigDeliveryOutbox{}, false, nil
}

func (r *runtimeRepository) CompleteRuntimeConfigDeliveryOutbox(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDeliveryOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeConfigDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	result := r.db.WithContext(ctx).Model(&runtimeConfigDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDeliveryOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(agentruntime.RuntimeConfigDeliveryOutboxCompleted), "lease_owner": "", "lease_expires_at": nil, "revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) RetryRuntimeConfigDeliveryOutbox(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeConfigDeliveryOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeConfigDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
	if len(message) > 4096 {
		message = message[:4096]
	}
	status := agentruntime.RuntimeConfigDeliveryOutboxQueued
	availableAt := now.Add(agentruntime.RuntimeConfigDeliveryOutboxBackoff(row.Attempt))
	if row.Attempt >= agentruntime.MaxRuntimeConfigDeliveryAttempts {
		status = agentruntime.RuntimeConfigDeliveryOutboxFailed
		availableAt = row.AvailableAt
	}
	result := r.db.WithContext(ctx).Model(&runtimeConfigDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeConfigDeliveryOutboxProcessing), owner, now).Updates(map[string]any{
		"status": string(status), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil, "last_error": message, "revision": row.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

var _ agentruntime.RuntimeConfigDeliveryInbox = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDeliveryTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDeliveryOutboxRepository = (*runtimeRepository)(nil)
