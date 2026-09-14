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

func validSQLiteRuntimeDeliveryGroupMemberKind(kind agentruntime.RuntimeDeliveryKind) bool {
	switch kind {
	case agentruntime.RuntimeDeliveryKindEvent, agentruntime.RuntimeDeliveryKindCheckpoint, agentruntime.RuntimeDeliveryKindConfig, agentruntime.RuntimeDeliveryKindRejection:
		return true
	default:
		return false
	}
}

func validSQLiteRuntimeDeliveryGroupMemberStatus(status agentruntime.RuntimeDeliveryGroupMemberStatus) bool {
	switch status {
	case agentruntime.RuntimeDeliveryGroupMemberPending, agentruntime.RuntimeDeliveryGroupMemberCompleted, agentruntime.RuntimeDeliveryGroupMemberFailed:
		return true
	default:
		return false
	}
}

// ensureRuntimeDeliveryGroupSettlementTx is the transaction-local idempotent
// insert used when the last source group member is marked terminal. It never
// copies a family payload; the settlement row contains only group metadata.
func ensureRuntimeDeliveryGroupSettlementTx(tx *gorm.DB, item agentruntime.RuntimeDeliveryGroupSettlement) (agentruntime.RuntimeDeliveryGroupSettlement, error) {
	now := time.Now().UTC()
	normalized, err := item.Normalize(now)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, err
	}
	row, err := runtimeDeliveryGroupSettlementToRow(normalized)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, err
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if result.Error != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, result.Error
	}
	var winner runtimeDeliveryGroupSettlementRow
	if err := tx.Where("id = ?", normalized.ID).First(&winner).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeDeliveryGroupSettlement{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeDeliveryGroupSettlement{}, err
	}
	stored, err := runtimeDeliveryGroupSettlementFromRow(winner)
	if err != nil {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, err
	}
	if !stored.MatchesIdentity(normalized) {
		return agentruntime.RuntimeDeliveryGroupSettlement{}, agentruntime.ErrConflict
	}
	return stored, nil
}

// MarkRuntimeDeliveryGroupMemberWithSettlement atomically updates the source
// group and, if the update makes it terminal, records the remote settlement
// intent in the same SQLite transaction. It is optional for compatibility;
// callers use the ordinary member method when this capability is absent.
func (r *runtimeRepository) MarkRuntimeDeliveryGroupMemberWithSettlement(ctx context.Context, id string, expectedRevision int64, kind agentruntime.RuntimeDeliveryKind, outboxID, deliveryID string, status agentruntime.RuntimeDeliveryGroupMemberStatus, message string, now time.Time) (agentruntime.RuntimeDeliveryGroup, bool, *agentruntime.RuntimeDeliveryGroupSettlement, error) {
	kind = agentruntime.RuntimeDeliveryKind(strings.TrimSpace(string(kind)))
	if expectedRevision <= 0 || !validSQLiteRuntimeDeliveryGroupMemberKind(kind) || !validSQLiteRuntimeDeliveryGroupMemberStatus(status) || strings.TrimSpace(outboxID) == "" || strings.TrimSpace(deliveryID) == "" {
		return agentruntime.RuntimeDeliveryGroup{}, false, nil, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var saved agentruntime.RuntimeDeliveryGroup
	var duplicate bool
	var settlement *agentruntime.RuntimeDeliveryGroupSettlement
	err := withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeDeliveryGroupRow
		if err := tx.Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		item, err := runtimeDeliveryGroupFromRow(row)
		if err != nil {
			return err
		}
		if item.Revision != expectedRevision {
			return agentruntime.ErrConflict
		}
		memberIndex := -1
		for index, member := range item.Members {
			if member.Kind == kind && member.OutboxID == strings.TrimSpace(outboxID) && member.DeliveryID == strings.TrimSpace(deliveryID) {
				memberIndex = index
				break
			}
		}
		if memberIndex < 0 {
			return agentruntime.ErrNotFound
		}
		if item.Status == agentruntime.RuntimeDeliveryGroupCompleted || item.Status == agentruntime.RuntimeDeliveryGroupFailed {
			if item.Members[memberIndex].Status != status {
				return agentruntime.ErrConflict
			}
			created, createErr := agentruntime.NewRuntimeDeliveryGroupSettlement(item, now)
			if createErr != nil {
				return createErr
			}
			stored, ensureErr := ensureRuntimeDeliveryGroupSettlementTx(tx, created)
			if ensureErr != nil {
				return ensureErr
			}
			saved, duplicate, settlement = item, true, &stored
			return nil
		}

		current := item.Members[memberIndex].Status
		changed := false
		if current == status {
			allCompleted, anyFailed := runtimeDeliveryGroupMembersCompletedForSQLite(item.Members)
			if anyFailed && item.Status != agentruntime.RuntimeDeliveryGroupFailed {
				item.Status = agentruntime.RuntimeDeliveryGroupFailed
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.CompletedAt = nil
				if item.LastError == "" {
					item.LastError = item.Members[memberIndex].LastError
				}
				changed = true
			} else if allCompleted && item.Status != agentruntime.RuntimeDeliveryGroupCompleted {
				item.Status = agentruntime.RuntimeDeliveryGroupCompleted
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				finished := now
				item.CompletedAt = &finished
				changed = true
			}
		} else {
			if current != agentruntime.RuntimeDeliveryGroupMemberPending {
				return agentruntime.ErrConflict
			}
			item.Members[memberIndex].Status = status
			item.Members[memberIndex].LastError = agentruntime.SanitizeRuntimeString(strings.TrimSpace(message))
			if len(item.Members[memberIndex].LastError) > agentruntime.MaxRuntimeDeliveryGroupMemberErrorLength {
				item.Members[memberIndex].LastError = item.Members[memberIndex].LastError[:agentruntime.MaxRuntimeDeliveryGroupMemberErrorLength]
			}
			item.Members[memberIndex].UpdatedAt = now
			allCompleted, anyFailed := runtimeDeliveryGroupMembersCompletedForSQLite(item.Members)
			if anyFailed {
				item.Status = agentruntime.RuntimeDeliveryGroupFailed
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				item.CompletedAt = nil
				if item.LastError == "" {
					item.LastError = item.Members[memberIndex].LastError
				}
			} else if allCompleted {
				item.Status = agentruntime.RuntimeDeliveryGroupCompleted
				item.LeaseOwner = ""
				item.LeaseExpiresAt = nil
				finished := now
				item.CompletedAt = &finished
			}
			changed = true
		}

		if changed {
			item.Revision++
			item.UpdatedAt = now
			encoded, marshalErr := json.Marshal(item.Members)
			if marshalErr != nil {
				return marshalErr
			}
			result := tx.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", item.ID, expectedRevision).Updates(map[string]any{
				"members_json": string(encoded), "status": string(item.Status), "lease_owner": item.LeaseOwner,
				"lease_expires_at": cloneTimePtr(item.LeaseExpiresAt), "last_error": item.LastError,
				"revision": item.Revision, "updated_at": item.UpdatedAt, "completed_at": cloneTimePtr(item.CompletedAt),
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			if item.Status == agentruntime.RuntimeDeliveryGroupCompleted || item.Status == agentruntime.RuntimeDeliveryGroupFailed {
				created, createErr := agentruntime.NewRuntimeDeliveryGroupSettlement(item, now)
				if createErr != nil {
					return createErr
				}
				stored, ensureErr := ensureRuntimeDeliveryGroupSettlementTx(tx, created)
				if ensureErr != nil {
					return ensureErr
				}
				settlement = &stored
			}
			saved = item
			duplicate = false
			return nil
		}

		if item.Status == agentruntime.RuntimeDeliveryGroupCompleted || item.Status == agentruntime.RuntimeDeliveryGroupFailed {
			created, createErr := agentruntime.NewRuntimeDeliveryGroupSettlement(item, now)
			if createErr != nil {
				return createErr
			}
			stored, ensureErr := ensureRuntimeDeliveryGroupSettlementTx(tx, created)
			if ensureErr != nil {
				return ensureErr
			}
			settlement = &stored
		}
		saved, duplicate = item, true
		return nil
	})
	if err != nil {
		return agentruntime.RuntimeDeliveryGroup{}, false, nil, err
	}
	return saved, duplicate, settlement, nil
}

var _ agentruntime.RuntimeDeliveryGroupSettlementCommitRepository = (*runtimeRepository)(nil)
