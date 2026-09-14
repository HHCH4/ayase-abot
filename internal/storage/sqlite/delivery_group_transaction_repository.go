package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type runtimeDeliveryGroupTransactionRow struct {
	Version       int       `gorm:"not null"`
	GroupID       string    `gorm:"primaryKey;size:220"`
	Source        string    `gorm:"index;size:160;not null"`
	Destination   string    `gorm:"index;size:160;not null"`
	InvocationID  string    `gorm:"index;size:512;not null"`
	FenceID       string    `gorm:"index;size:220"`
	FenceRevision int64     `gorm:"index"`
	MembersJSON   string    `gorm:"type:text;not null"`
	MembersDigest string    `gorm:"size:72;not null"`
	Timestamp     time.Time `gorm:"index"`
	IssuedAt      time.Time `gorm:"index"`
	Signature     string    `gorm:"size:64"`
	Status        string    `gorm:"index;size:32;not null"`
	ExpiresAt     time.Time `gorm:"index"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (runtimeDeliveryGroupTransactionRow) TableName() string {
	return "abot_agent_runtime_delivery_group_transactions"
}

func runtimeDeliveryGroupTransactionFromRow(row runtimeDeliveryGroupTransactionRow) (agentruntime.RuntimeDeliveryGroupTransaction, error) {
	var members []agentruntime.RuntimeDeliveryGroupMemberEnvelope
	if err := json.Unmarshal([]byte(row.MembersJSON), &members); err != nil {
		return agentruntime.RuntimeDeliveryGroupTransaction{}, err
	}
	item := agentruntime.RuntimeDeliveryGroupTransaction{
		Version: row.Version, GroupID: row.GroupID, Source: row.Source, Destination: row.Destination,
		InvocationID: row.InvocationID, FenceID: row.FenceID, FenceRevision: row.FenceRevision, Members: members, MembersDigest: row.MembersDigest,
		Timestamp: row.Timestamp, IssuedAt: row.IssuedAt, Signature: row.Signature,
		Status: agentruntime.RuntimeDeliveryGroupTransactionStatus(row.Status), ExpiresAt: row.ExpiresAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	return item.Normalize(time.Now().UTC())
}

func runtimeDeliveryGroupTransactionToRow(item agentruntime.RuntimeDeliveryGroupTransaction) (*runtimeDeliveryGroupTransactionRow, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized.Members)
	if err != nil {
		return nil, err
	}
	if len(encoded) > agentruntime.MaxRuntimeDeliveryGroupEnvelopeBytes {
		return nil, agentruntime.ErrInvalidRuntimeDeliveryGroupTransaction
	}
	return &runtimeDeliveryGroupTransactionRow{
		Version: normalized.Version, GroupID: normalized.GroupID, Source: normalized.Source, Destination: normalized.Destination,
		InvocationID: normalized.InvocationID, FenceID: normalized.FenceID, FenceRevision: normalized.FenceRevision, MembersJSON: string(encoded), MembersDigest: normalized.MembersDigest,
		Timestamp: normalized.Timestamp, IssuedAt: normalized.IssuedAt, Signature: normalized.Signature,
		Status: string(normalized.Status), ExpiresAt: normalized.ExpiresAt, CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt,
	}, nil
}

func (r *runtimeRepository) PrepareRuntimeDeliveryGroup(ctx context.Context, envelope agentruntime.RuntimeDeliveryGroupEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeDeliveryGroupEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeDeliveryGroupTransactionRow
		findErr := tx.Where("group_id = ?", normalized.GroupID).First(&row).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			created, createErr := agentruntime.NewRuntimeDeliveryGroupTransaction(normalized, now)
			if createErr != nil {
				return createErr
			}
			if err := ensureRuntimeDeliveryGroupFenceTx(tx, normalized, now, true); err != nil {
				return err
			}
			createdRow, rowErr := runtimeDeliveryGroupTransactionToRow(created)
			if rowErr != nil {
				return rowErr
			}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(createdRow)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 {
				return nil
			}
			findErr = tx.Where("group_id = ?", normalized.GroupID).First(&row).Error
		}
		if findErr != nil {
			return findErr
		}
		existing, decodeErr := runtimeDeliveryGroupTransactionFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !existing.Matches(normalized) {
			return agentruntime.ErrConflict
		}
		if err := ensureRuntimeDeliveryGroupFenceTx(tx, normalized, now, true); err != nil {
			return err
		}
		switch existing.Status {
		case agentruntime.RuntimeDeliveryGroupTransactionCommitted:
			duplicate = true
			return nil
		case agentruntime.RuntimeDeliveryGroupTransactionPrepared:
			if existing.ExpiresAt.After(now) {
				duplicate = true
				return nil
			}
			refreshed, refreshErr := agentruntime.NewRuntimeDeliveryGroupTransaction(normalized, now)
			if refreshErr != nil {
				return refreshErr
			}
			refreshed.CreatedAt = existing.CreatedAt
			refreshedRow, rowErr := runtimeDeliveryGroupTransactionToRow(refreshed)
			if rowErr != nil {
				return rowErr
			}
			result := tx.Model(&runtimeDeliveryGroupTransactionRow{}).Where("group_id = ? AND status = ?", normalized.GroupID, string(agentruntime.RuntimeDeliveryGroupTransactionPrepared)).Updates(map[string]any{
				"version": refreshedRow.Version, "source": refreshedRow.Source, "destination": refreshedRow.Destination,
				"invocation_id": refreshedRow.InvocationID, "members_json": refreshedRow.MembersJSON, "members_digest": refreshedRow.MembersDigest,
				"timestamp": refreshedRow.Timestamp, "issued_at": refreshedRow.IssuedAt, "signature": refreshedRow.Signature,
				"status": refreshedRow.Status, "expires_at": refreshedRow.ExpiresAt, "created_at": refreshedRow.CreatedAt, "updated_at": refreshedRow.UpdatedAt,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			return nil
		case agentruntime.RuntimeDeliveryGroupTransactionAborted:
			return agentruntime.ErrConflict
		default:
			return agentruntime.ErrConflict
		}
	})
	return duplicate, err
}

func (r *runtimeRepository) CommitRuntimeDeliveryGroup(ctx context.Context, envelope agentruntime.RuntimeDeliveryGroupEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeDeliveryGroupEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeDeliveryGroupTransactionRow
		if findErr := tx.Where("group_id = ?", normalized.GroupID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		existing, decodeErr := runtimeDeliveryGroupTransactionFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !existing.Matches(normalized) {
			return agentruntime.ErrConflict
		}
		if err := ensureRuntimeDeliveryGroupFenceTx(tx, normalized, now, true); err != nil {
			return err
		}
		switch existing.Status {
		case agentruntime.RuntimeDeliveryGroupTransactionCommitted:
			duplicate = true
			return nil
		case agentruntime.RuntimeDeliveryGroupTransactionPrepared:
			if !existing.ExpiresAt.After(now) {
				return agentruntime.ErrRuntimeDeliveryGroupTransactionStale
			}
		default:
			return agentruntime.ErrConflict
		}
		result := tx.Model(&runtimeDeliveryGroupTransactionRow{}).Where("group_id = ? AND status = ?", normalized.GroupID, string(agentruntime.RuntimeDeliveryGroupTransactionPrepared)).Updates(map[string]any{"status": string(agentruntime.RuntimeDeliveryGroupTransactionCommitted), "updated_at": now})
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

func (r *runtimeRepository) AbortRuntimeDeliveryGroup(ctx context.Context, envelope agentruntime.RuntimeDeliveryGroupEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeDeliveryGroupEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeDeliveryGroupTransactionRow
		if findErr := tx.Where("group_id = ?", normalized.GroupID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		existing, decodeErr := runtimeDeliveryGroupTransactionFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !existing.Matches(normalized) {
			return agentruntime.ErrConflict
		}
		if err := ensureRuntimeDeliveryGroupFenceTx(tx, normalized, now, true); err != nil {
			return err
		}
		switch existing.Status {
		case agentruntime.RuntimeDeliveryGroupTransactionAborted:
			duplicate = true
			return nil
		case agentruntime.RuntimeDeliveryGroupTransactionCommitted:
			return agentruntime.ErrConflict
		case agentruntime.RuntimeDeliveryGroupTransactionPrepared:
			result := tx.Model(&runtimeDeliveryGroupTransactionRow{}).Where("group_id = ? AND status = ?", normalized.GroupID, string(agentruntime.RuntimeDeliveryGroupTransactionPrepared)).Updates(map[string]any{"status": string(agentruntime.RuntimeDeliveryGroupTransactionAborted), "updated_at": now})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			return nil
		default:
			return agentruntime.ErrConflict
		}
	})
	return duplicate, err
}

func (r *runtimeRepository) GetRuntimeDeliveryGroupTransaction(ctx context.Context, groupID string) (agentruntime.RuntimeDeliveryGroupTransaction, error) {
	var row runtimeDeliveryGroupTransactionRow
	if err := r.db.WithContext(ctx).Where("group_id = ?", strings.TrimSpace(groupID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeDeliveryGroupTransaction{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeDeliveryGroupTransaction{}, err
	}
	return runtimeDeliveryGroupTransactionFromRow(row)
}

// RuntimeDeliveryGroupMemberPhase returns only the phase of an existing
// family-specific destination transaction. It never returns the transaction
// payload or projection.
func (r *runtimeRepository) RuntimeDeliveryGroupMemberPhase(ctx context.Context, member agentruntime.RuntimeDeliveryGroupMemberEnvelope) (agentruntime.RuntimeDeliveryGroupTransactionStatus, error) {
	member.Kind = agentruntime.RuntimeDeliveryKind(strings.TrimSpace(string(member.Kind)))
	member.OutboxID = strings.TrimSpace(member.OutboxID)
	member.DeliveryID = strings.TrimSpace(member.DeliveryID)
	if member.Kind == "" || member.DeliveryID == "" {
		return "", agentruntime.ErrInvalidRuntimeDeliveryGroupTransaction
	}
	switch member.Kind {
	case agentruntime.RuntimeDeliveryKindEvent:
		var row runtimeEventDeliveryTransactionRow
		if err := r.db.WithContext(ctx).Where("delivery_id = ?", member.DeliveryID).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", agentruntime.ErrNotFound
			}
			return "", err
		}
		item, err := runtimeEventDeliveryTransactionFromRow(row)
		if err != nil {
			return "", err
		}
		switch item.Status {
		case agentruntime.RuntimeEventDeliveryTransactionPrepared:
			return agentruntime.RuntimeDeliveryGroupTransactionPrepared, nil
		case agentruntime.RuntimeEventDeliveryTransactionCommitted:
			return agentruntime.RuntimeDeliveryGroupTransactionCommitted, nil
		case agentruntime.RuntimeEventDeliveryTransactionAborted:
			return agentruntime.RuntimeDeliveryGroupTransactionAborted, nil
		}
	case agentruntime.RuntimeDeliveryKindCheckpoint:
		var row runtimeCheckpointDeliveryTransactionRow
		if err := r.db.WithContext(ctx).Where("delivery_id = ?", member.DeliveryID).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", agentruntime.ErrNotFound
			}
			return "", err
		}
		item, err := runtimeCheckpointDeliveryTransactionFromRow(row)
		if err != nil {
			return "", err
		}
		switch item.Status {
		case agentruntime.RuntimeCheckpointDeliveryTransactionPrepared:
			return agentruntime.RuntimeDeliveryGroupTransactionPrepared, nil
		case agentruntime.RuntimeCheckpointDeliveryTransactionCommitted:
			return agentruntime.RuntimeDeliveryGroupTransactionCommitted, nil
		case agentruntime.RuntimeCheckpointDeliveryTransactionAborted:
			return agentruntime.RuntimeDeliveryGroupTransactionAborted, nil
		}
	case agentruntime.RuntimeDeliveryKindConfig:
		var row runtimeConfigDeliveryTransactionRow
		if err := r.db.WithContext(ctx).Where("delivery_id = ?", member.DeliveryID).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", agentruntime.ErrNotFound
			}
			return "", err
		}
		item, err := runtimeConfigDeliveryTransactionFromRow(row)
		if err != nil {
			return "", err
		}
		switch item.Status {
		case agentruntime.RuntimeConfigDeliveryTransactionPrepared:
			return agentruntime.RuntimeDeliveryGroupTransactionPrepared, nil
		case agentruntime.RuntimeConfigDeliveryTransactionCommitted:
			return agentruntime.RuntimeDeliveryGroupTransactionCommitted, nil
		case agentruntime.RuntimeConfigDeliveryTransactionAborted:
			return agentruntime.RuntimeDeliveryGroupTransactionAborted, nil
		}
	case agentruntime.RuntimeDeliveryKindRejection:
		var row runtimeApprovalRejectionDeliveryTransactionRow
		if err := r.db.WithContext(ctx).Where("delivery_id = ?", member.DeliveryID).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", agentruntime.ErrNotFound
			}
			return "", err
		}
		item := runtimeApprovalRejectionDeliveryTransactionFromRow(row)
		switch item.Status {
		case agentruntime.RuntimeApprovalRejectionDeliveryTransactionPrepared:
			return agentruntime.RuntimeDeliveryGroupTransactionPrepared, nil
		case agentruntime.RuntimeApprovalRejectionDeliveryTransactionCommitted:
			return agentruntime.RuntimeDeliveryGroupTransactionCommitted, nil
		case agentruntime.RuntimeApprovalRejectionDeliveryTransactionAborted:
			return agentruntime.RuntimeDeliveryGroupTransactionAborted, nil
		}
	}
	return "", agentruntime.ErrConflict
}

var _ agentruntime.RuntimeDeliveryGroupTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupMemberPhaseResolver = (*runtimeRepository)(nil)
