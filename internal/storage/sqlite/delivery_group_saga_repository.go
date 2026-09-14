package sqlite

import (
	"context"
	"errors"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type runtimeDeliveryGroupSagaRow struct {
	ID           string     `gorm:"primaryKey;size:240"`
	GroupID      string     `gorm:"index;size:220;not null"`
	InvocationID string     `gorm:"index;size:512;not null"`
	Source       string     `gorm:"index;size:160;not null"`
	Destination  string     `gorm:"index;size:160;not null"`
	Decision     string     `gorm:"index;size:32;not null"`
	State        string     `gorm:"index;size:48;not null"`
	RemotePhase  string     `gorm:"index;size:32"`
	Revision     int64      `gorm:"not null"`
	LastError    string     `gorm:"type:text"`
	CreatedAt    time.Time  `gorm:"index"`
	UpdatedAt    time.Time  `gorm:"index"`
	CompletedAt  *time.Time `gorm:"index"`
}

func (runtimeDeliveryGroupSagaRow) TableName() string {
	return "abot_agent_runtime_delivery_group_sagas"
}

func runtimeDeliveryGroupSagaFromRow(row runtimeDeliveryGroupSagaRow) (agentruntime.RuntimeDeliveryGroupSaga, error) {
	item := agentruntime.RuntimeDeliveryGroupSaga{
		ID: row.ID, GroupID: row.GroupID, InvocationID: row.InvocationID, Source: row.Source,
		Destination: row.Destination, Decision: agentruntime.RuntimeDeliveryGroupSagaDecision(row.Decision),
		State: agentruntime.RuntimeDeliveryGroupSagaState(row.State), RemotePhase: agentruntime.RuntimeDeliveryGroupSettlementRemotePhase(row.RemotePhase),
		Revision: row.Revision, LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		CompletedAt: cloneTimePtr(row.CompletedAt),
	}
	return item.Normalize(time.Now().UTC())
}

func runtimeDeliveryGroupSagaToRow(item agentruntime.RuntimeDeliveryGroupSaga) (*runtimeDeliveryGroupSagaRow, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return &runtimeDeliveryGroupSagaRow{
		ID: normalized.ID, GroupID: normalized.GroupID, InvocationID: normalized.InvocationID,
		Source: normalized.Source, Destination: normalized.Destination, Decision: string(normalized.Decision),
		State: string(normalized.State), RemotePhase: string(normalized.RemotePhase), Revision: normalized.Revision,
		LastError: normalized.LastError, CreatedAt: normalized.CreatedAt, UpdatedAt: normalized.UpdatedAt,
		CompletedAt: cloneTimePtr(normalized.CompletedAt),
	}, nil
}

func (r *runtimeRepository) EnqueueRuntimeDeliveryGroupSaga(ctx context.Context, item agentruntime.RuntimeDeliveryGroupSaga) (agentruntime.RuntimeDeliveryGroupSaga, error) {
	now := time.Now().UTC()
	normalized, err := item.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSaga{}, err
	}
	row, err := runtimeDeliveryGroupSagaToRow(normalized)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSaga{}, err
	}
	var saved agentruntime.RuntimeDeliveryGroupSaga
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		var winner runtimeDeliveryGroupSagaRow
		if findErr := tx.Where("id = ?", normalized.ID).First(&winner).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		stored, decodeErr := runtimeDeliveryGroupSagaFromRow(winner)
		if decodeErr != nil {
			return decodeErr
		}
		if !stored.MatchesIdentity(normalized) {
			return agentruntime.ErrConflict
		}
		saved = stored
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSaga{}, err
	}
	return saved, nil
}

func (r *runtimeRepository) GetRuntimeDeliveryGroupSaga(ctx context.Context, id string) (agentruntime.RuntimeDeliveryGroupSaga, error) {
	var row runtimeDeliveryGroupSagaRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeDeliveryGroupSaga{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeDeliveryGroupSaga{}, err
	}
	return runtimeDeliveryGroupSagaFromRow(row)
}

func (r *runtimeRepository) ListRuntimeDeliveryGroupSagas(ctx context.Context, invocationID string, state agentruntime.RuntimeDeliveryGroupSagaState, limit int) ([]agentruntime.RuntimeDeliveryGroupSaga, error) {
	invocationID = strings.TrimSpace(invocationID)
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	if invocationID != "" {
		var invocation invocationRow
		if err := r.db.WithContext(ctx).Where("id = ?", invocationID).First(&invocation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, agentruntime.ErrNotFound
			}
			return nil, err
		}
	}
	state = agentruntime.RuntimeDeliveryGroupSagaState(strings.TrimSpace(strings.ToLower(string(state))))
	if state != "" && !validSQLiteRuntimeDeliveryGroupSagaState(state) {
		return nil, agentruntime.ErrInvalidRuntimeDeliveryGroupSaga
	}
	query := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupSagaRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if state != "" {
		query = query.Where("state = ?", string(state))
	}
	var rows []runtimeDeliveryGroupSagaRow
	if err := query.Order("updated_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeDeliveryGroupSaga, 0, len(rows))
	for _, row := range rows {
		item, decodeErr := runtimeDeliveryGroupSagaFromRow(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		items = append(items, item)
	}
	return items, nil
}

func validSQLiteRuntimeDeliveryGroupSagaState(state agentruntime.RuntimeDeliveryGroupSagaState) bool {
	switch state {
	case agentruntime.RuntimeDeliveryGroupSagaDecided, agentruntime.RuntimeDeliveryGroupSagaPreparing,
		agentruntime.RuntimeDeliveryGroupSagaPrepared, agentruntime.RuntimeDeliveryGroupSagaCommitting,
		agentruntime.RuntimeDeliveryGroupSagaAborting, agentruntime.RuntimeDeliveryGroupSagaCommitted,
		agentruntime.RuntimeDeliveryGroupSagaAborted, agentruntime.RuntimeDeliveryGroupSagaCompensationRequired,
		agentruntime.RuntimeDeliveryGroupSagaFailed:
		return true
	default:
		return false
	}
}

func (r *runtimeRepository) AdvanceRuntimeDeliveryGroupSaga(ctx context.Context, id string, expectedRevision int64, state agentruntime.RuntimeDeliveryGroupSagaState, remotePhase agentruntime.RuntimeDeliveryGroupSettlementRemotePhase, message string, now time.Time) (agentruntime.RuntimeDeliveryGroupSaga, bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeDeliveryGroupSagaRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeDeliveryGroupSaga{}, false, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeDeliveryGroupSaga{}, false, err
	}
	item, err := runtimeDeliveryGroupSagaFromRow(row)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSaga{}, false, err
	}
	updated, duplicate, err := agentruntime.AdvanceRuntimeDeliveryGroupSaga(item, expectedRevision, state, remotePhase, message, now)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSaga{}, false, err
	}
	if duplicate {
		return updated, true, nil
	}
	result := r.db.WithContext(ctx).Model(&runtimeDeliveryGroupSagaRow{}).Where("id = ? AND revision = ?", item.ID, item.Revision).Updates(map[string]any{
		"state": string(updated.State), "remote_phase": string(updated.RemotePhase), "last_error": updated.LastError,
		"revision": updated.Revision, "updated_at": updated.UpdatedAt, "completed_at": cloneTimePtr(updated.CompletedAt),
	})
	if result.Error != nil {
		return agentruntime.RuntimeDeliveryGroupSaga{}, false, result.Error
	}
	if result.RowsAffected != 1 {
		return agentruntime.RuntimeDeliveryGroupSaga{}, false, agentruntime.ErrConflict
	}
	return updated, false, nil
}

var _ agentruntime.RuntimeDeliveryGroupSagaRepository = (*runtimeRepository)(nil)
