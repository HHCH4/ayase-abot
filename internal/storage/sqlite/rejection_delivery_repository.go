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

// The rejection-delivery tables intentionally contain only authenticated
// metadata. Human reason text, tool arguments and command bodies remain in the
// local Runtime/Workspace stores.
type runtimeApprovalRejectionDeliveryOutboxRow struct {
	ID             string `gorm:"primaryKey;size:220"`
	GroupID        string `gorm:"index;size:220"`
	ApprovalID     string `gorm:"index:idx_runtime_rejection_outbox_approval_route,priority:1;size:220;not null"`
	InvocationID   string `gorm:"index:idx_runtime_rejection_outbox_invocation,priority:1;size:512;not null"`
	ToolCallID     string `gorm:"size:512"`
	OperationID    string `gorm:"size:512"`
	Source         string `gorm:"index:idx_runtime_rejection_outbox_approval_route,priority:2;size:160;not null"`
	Destination    string `gorm:"index:idx_runtime_rejection_outbox_approval_route,priority:3;size:160;not null"`
	DeliveryID     string `gorm:"uniqueIndex;size:220;not null"`
	Decision       string `gorm:"size:32;not null"`
	ReasonDigest   string `gorm:"size:64;not null"`
	DecisionAt     time.Time
	Status         string     `gorm:"index;size:32;not null"`
	Attempt        int        `gorm:"not null"`
	Revision       int64      `gorm:"not null"`
	AvailableAt    time.Time  `gorm:"index"`
	LeaseOwner     string     `gorm:"index;size:160"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	LastError      string     `gorm:"type:text"`
	CreatedAt      time.Time  `gorm:"index:idx_runtime_rejection_outbox_invocation,priority:2"`
	UpdatedAt      time.Time
}

func (runtimeApprovalRejectionDeliveryOutboxRow) TableName() string {
	return "abot_agent_approval_rejection_delivery_outbox"
}

type runtimeApprovalRejectionDeliveryInboxRow struct {
	Version      int    `gorm:"not null"`
	DeliveryID   string `gorm:"primaryKey;size:220"`
	ApprovalID   string `gorm:"index:idx_runtime_rejection_inbox_approval_route,priority:1;size:220;not null"`
	InvocationID string `gorm:"index;size:512;not null"`
	ToolCallID   string `gorm:"size:512"`
	OperationID  string `gorm:"size:512"`
	Source       string `gorm:"index:idx_runtime_rejection_inbox_approval_route,priority:2;size:160;not null"`
	Destination  string `gorm:"index:idx_runtime_rejection_inbox_approval_route,priority:3;size:160;not null"`
	Decision     string `gorm:"size:32;not null"`
	ReasonDigest string `gorm:"size:64;not null"`
	DecisionAt   time.Time
	Signature    string `gorm:"size:64;not null"`
	ReceivedAt   time.Time
}

func (runtimeApprovalRejectionDeliveryInboxRow) TableName() string {
	return "abot_agent_approval_rejection_delivery_inbox"
}

type runtimeApprovalRejectionDeliveryTransactionRow struct {
	Version      int    `gorm:"not null"`
	DeliveryID   string `gorm:"primaryKey;size:220"`
	ApprovalID   string `gorm:"index;size:220;not null"`
	InvocationID string `gorm:"index;size:512;not null"`
	ToolCallID   string `gorm:"size:512"`
	OperationID  string `gorm:"size:512"`
	Source       string `gorm:"index;size:160;not null"`
	Destination  string `gorm:"index;size:160;not null"`
	Decision     string `gorm:"size:32;not null"`
	ReasonDigest string `gorm:"size:64;not null"`
	DecisionAt   time.Time
	IssuedAt     time.Time
	Signature    string    `gorm:"size:64"`
	Status       string    `gorm:"index;size:32;not null"`
	ExpiresAt    time.Time `gorm:"index"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (runtimeApprovalRejectionDeliveryTransactionRow) TableName() string {
	return "abot_agent_approval_rejection_delivery_transactions"
}

func runtimeApprovalRejectionDeliveryOutboxFromRow(row runtimeApprovalRejectionDeliveryOutboxRow) agentruntime.RuntimeApprovalRejectionDeliveryOutbox {
	return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{ID: row.ID, GroupID: row.GroupID, ApprovalID: row.ApprovalID, InvocationID: row.InvocationID, ToolCallID: row.ToolCallID, OperationID: row.OperationID, Source: row.Source, Destination: row.Destination, DeliveryID: row.DeliveryID, Decision: row.Decision, ReasonDigest: row.ReasonDigest, DecisionAt: row.DecisionAt, Status: agentruntime.RuntimeApprovalRejectionDeliveryOutboxStatus(row.Status), Attempt: row.Attempt, Revision: row.Revision, AvailableAt: row.AvailableAt, LeaseOwner: row.LeaseOwner, LeaseExpiresAt: cloneTimePtr(row.LeaseExpiresAt), LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func runtimeApprovalRejectionDeliveryOutboxToRow(item agentruntime.RuntimeApprovalRejectionDeliveryOutbox) *runtimeApprovalRejectionDeliveryOutboxRow {
	return &runtimeApprovalRejectionDeliveryOutboxRow{ID: item.ID, GroupID: item.GroupID, ApprovalID: item.ApprovalID, InvocationID: item.InvocationID, ToolCallID: item.ToolCallID, OperationID: item.OperationID, Source: item.Source, Destination: item.Destination, DeliveryID: item.DeliveryID, Decision: item.Decision, ReasonDigest: item.ReasonDigest, DecisionAt: item.DecisionAt, Status: string(item.Status), Attempt: item.Attempt, Revision: item.Revision, AvailableAt: item.AvailableAt, LeaseOwner: item.LeaseOwner, LeaseExpiresAt: cloneTimePtr(item.LeaseExpiresAt), LastError: item.LastError, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func runtimeApprovalRejectionDeliveryInboxFromRow(row runtimeApprovalRejectionDeliveryInboxRow) agentruntime.RuntimeApprovalRejectionDeliveryInboxRecord {
	return agentruntime.RuntimeApprovalRejectionDeliveryInboxRecord{Version: row.Version, DeliveryID: row.DeliveryID, ApprovalID: row.ApprovalID, InvocationID: row.InvocationID, ToolCallID: row.ToolCallID, OperationID: row.OperationID, Source: row.Source, Destination: row.Destination, Decision: row.Decision, ReasonDigest: row.ReasonDigest, DecisionAt: row.DecisionAt, Signature: row.Signature, ReceivedAt: row.ReceivedAt}
}

func runtimeApprovalRejectionDeliveryInboxToRow(item agentruntime.RuntimeApprovalRejectionDeliveryInboxRecord) *runtimeApprovalRejectionDeliveryInboxRow {
	return &runtimeApprovalRejectionDeliveryInboxRow{Version: item.Version, DeliveryID: item.DeliveryID, ApprovalID: item.ApprovalID, InvocationID: item.InvocationID, ToolCallID: item.ToolCallID, OperationID: item.OperationID, Source: item.Source, Destination: item.Destination, Decision: item.Decision, ReasonDigest: item.ReasonDigest, DecisionAt: item.DecisionAt, Signature: item.Signature, ReceivedAt: item.ReceivedAt}
}

func runtimeApprovalRejectionDeliveryTransactionFromRow(row runtimeApprovalRejectionDeliveryTransactionRow) agentruntime.RuntimeApprovalRejectionDeliveryTransaction {
	return agentruntime.RuntimeApprovalRejectionDeliveryTransaction{Version: row.Version, DeliveryID: row.DeliveryID, ApprovalID: row.ApprovalID, InvocationID: row.InvocationID, ToolCallID: row.ToolCallID, OperationID: row.OperationID, Source: row.Source, Destination: row.Destination, Decision: row.Decision, ReasonDigest: row.ReasonDigest, DecisionAt: row.DecisionAt, IssuedAt: row.IssuedAt, Signature: row.Signature, Status: agentruntime.RuntimeApprovalRejectionDeliveryTransactionStatus(row.Status), ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func runtimeApprovalRejectionDeliveryTransactionToRow(item agentruntime.RuntimeApprovalRejectionDeliveryTransaction) *runtimeApprovalRejectionDeliveryTransactionRow {
	return &runtimeApprovalRejectionDeliveryTransactionRow{Version: item.Version, DeliveryID: item.DeliveryID, ApprovalID: item.ApprovalID, InvocationID: item.InvocationID, ToolCallID: item.ToolCallID, OperationID: item.OperationID, Source: item.Source, Destination: item.Destination, Decision: item.Decision, ReasonDigest: item.ReasonDigest, DecisionAt: item.DecisionAt, IssuedAt: item.IssuedAt, Signature: item.Signature, Status: string(item.Status), ExpiresAt: item.ExpiresAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func createRuntimeApprovalRejectionDeliveryOutboxTx(tx *gorm.DB, item agentruntime.RuntimeApprovalRejectionDeliveryOutbox) error {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return err
	}
	var existing runtimeApprovalRejectionDeliveryOutboxRow
	findErr := tx.Where("id = ?", normalized.ID).First(&existing).Error
	if findErr == nil {
		if !runtimeApprovalRejectionDeliveryOutboxFromRow(existing).Matches(normalized) {
			return agentruntime.ErrConflict
		}
		return nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return findErr
	}
	var approvalRoute runtimeApprovalRejectionDeliveryOutboxRow
	findErr = tx.Where("approval_id = ? AND source = ? AND destination = ?", normalized.ApprovalID, normalized.Source, normalized.Destination).First(&approvalRoute).Error
	if findErr == nil {
		if !runtimeApprovalRejectionDeliveryOutboxFromRow(approvalRoute).Matches(normalized) {
			return agentruntime.ErrConflict
		}
		return nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return findErr
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(runtimeApprovalRejectionDeliveryOutboxToRow(normalized))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var winner runtimeApprovalRejectionDeliveryOutboxRow
	if err := tx.Where("id = ?", normalized.ID).First(&winner).Error; err != nil {
		return err
	}
	if !runtimeApprovalRejectionDeliveryOutboxFromRow(winner).Matches(normalized) {
		return agentruntime.ErrConflict
	}
	return nil
}

func (r *runtimeRepository) CommitApprovalRejectionWithDelivery(ctx context.Context, commit agentruntime.ApprovalResumeCommit, delivery agentruntime.RuntimeApprovalRejectionDeliveryOutbox) (agentruntime.Approval, agentruntime.Invocation, []agentruntime.AgentEvent, error) {
	commit.RejectionDelivery = &delivery
	return r.commitApprovalResume(ctx, commit, true)
}

func (r *runtimeRepository) EnqueueRuntimeApprovalRejectionDeliveryOutbox(ctx context.Context, item agentruntime.RuntimeApprovalRejectionDeliveryOutbox) (agentruntime.RuntimeApprovalRejectionDeliveryOutbox, error) {
	normalized, err := item.Normalize(time.Now().UTC())
	if err != nil {
		return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, err
	}
	var saved agentruntime.RuntimeApprovalRejectionDeliveryOutbox
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approval approvalRow
		if err := tx.Where("id = ?", normalized.ApprovalID).First(&approval).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		if agentruntime.ApprovalStatus(approval.Status) != agentruntime.ApprovalRejected || approval.InvocationID != normalized.InvocationID || strings.TrimSpace(approval.ToolCallID) != normalized.ToolCallID || strings.TrimSpace(approval.OperationID) != normalized.OperationID {
			return agentruntime.ErrConflict
		}
		if err := tx.Where("id = ?", normalized.InvocationID).First(&invocationRow{}).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		deliveryGroup, groupErr := agentruntime.NewRuntimeDeliveryGroup(normalized.Source, normalized.Destination, normalized.InvocationID, []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindRejection, OutboxID: normalized.ID, DeliveryID: normalized.DeliveryID}}, normalized.DecisionAt)
		if groupErr != nil {
			return groupErr
		}
		normalized.GroupID = deliveryGroup.ID
		if err := createRuntimeApprovalRejectionDeliveryOutboxTx(tx, normalized); err != nil {
			return err
		}
		if err := ensureRuntimeDeliveryGroupTx(tx, deliveryGroup); err != nil {
			return err
		}
		if updateErr := tx.Model(&runtimeApprovalRejectionDeliveryOutboxRow{}).Where("id = ?", normalized.ID).Update("group_id", deliveryGroup.ID).Error; updateErr != nil {
			return updateErr
		}
		var row runtimeApprovalRejectionDeliveryOutboxRow
		if err := tx.Where("id = ?", normalized.ID).First(&row).Error; err != nil {
			return err
		}
		saved = runtimeApprovalRejectionDeliveryOutboxFromRow(row)
		return nil
	})
	return saved, err
}

func (r *runtimeRepository) GetRuntimeApprovalRejectionDeliveryOutbox(ctx context.Context, id string) (agentruntime.RuntimeApprovalRejectionDeliveryOutbox, error) {
	var row runtimeApprovalRejectionDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, err
	}
	return runtimeApprovalRejectionDeliveryOutboxFromRow(row), nil
}

func (r *runtimeRepository) ListRuntimeApprovalRejectionDeliveryOutbox(ctx context.Context, invocationID string, status agentruntime.RuntimeApprovalRejectionDeliveryOutboxStatus, limit int) ([]agentruntime.RuntimeApprovalRejectionDeliveryOutbox, error) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID != "" {
		if err := r.db.WithContext(ctx).Where("id = ?", invocationID).First(&invocationRow{}).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, agentruntime.ErrNotFound
		} else if err != nil {
			return nil, err
		}
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&runtimeApprovalRejectionDeliveryOutboxRow{})
	if invocationID != "" {
		query = query.Where("invocation_id = ?", invocationID)
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []runtimeApprovalRejectionDeliveryOutboxRow
	if err := query.Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]agentruntime.RuntimeApprovalRejectionDeliveryOutbox, 0, len(rows))
	for _, row := range rows {
		items = append(items, runtimeApprovalRejectionDeliveryOutboxFromRow(row))
	}
	return items, nil
}

func (r *runtimeRepository) ClaimRuntimeApprovalRejectionDeliveryOutbox(ctx context.Context, owner string, now time.Time, ttl time.Duration) (agentruntime.RuntimeApprovalRejectionDeliveryOutbox, bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeApprovalRejectionDeliveryOwnerLength || ttl <= 0 || ttl > agentruntime.MaxRuntimeApprovalRejectionDeliveryLease {
		return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for round := 0; round < 4; round++ {
		var rows []runtimeApprovalRejectionDeliveryOutboxRow
		if err := r.db.WithContext(ctx).Where("(status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))", string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxQueued), now, string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxProcessing), now).Order("available_at ASC").Order("created_at ASC").Order("id ASC").Limit(64).Find(&rows).Error; err != nil {
			return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, false, err
		}
		if len(rows) == 0 {
			return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, false, nil
		}
		for _, row := range rows {
			if row.Attempt >= agentruntime.MaxRuntimeApprovalRejectionDeliveryAttempts {
				result := r.db.WithContext(ctx).Model(&runtimeApprovalRejectionDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status IN (?, ?)", row.ID, row.Revision, string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxQueued), string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxProcessing)).Updates(map[string]any{"status": string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxFailed), "lease_owner": "", "lease_expires_at": nil, "last_error": "approval rejection outbox 达到最大投递次数", "revision": row.Revision + 1, "updated_at": now})
				if result.Error != nil {
					return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, false, result.Error
				}
				continue
			}
			expires := now.Add(ttl)
			result := r.db.WithContext(ctx).Model(&runtimeApprovalRejectionDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))", row.ID, row.Revision, string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxQueued), now, string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxProcessing), now).Updates(map[string]any{"status": string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxProcessing), "attempt": row.Attempt + 1, "revision": row.Revision + 1, "lease_owner": owner, "lease_expires_at": &expires, "updated_at": now})
			if result.Error != nil {
				return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, false, result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			row.Status = string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxProcessing)
			row.Attempt++
			row.Revision++
			row.LeaseOwner = owner
			row.LeaseExpiresAt = &expires
			row.UpdatedAt = now
			return runtimeApprovalRejectionDeliveryOutboxFromRow(row), true, nil
		}
	}
	return agentruntime.RuntimeApprovalRejectionDeliveryOutbox{}, false, nil
}

func (r *runtimeRepository) CompleteRuntimeApprovalRejectionDeliveryOutbox(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeApprovalRejectionDeliveryOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeApprovalRejectionDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	result := r.db.WithContext(ctx).Model(&runtimeApprovalRejectionDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxProcessing), owner, now).Updates(map[string]any{"status": string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxCompleted), "lease_owner": "", "lease_expires_at": nil, "revision": row.Revision + 1, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) RetryRuntimeApprovalRejectionDeliveryOutbox(ctx context.Context, id, owner string, now time.Time, message string) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > agentruntime.MaxRuntimeApprovalRejectionDeliveryOwnerLength {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var row runtimeApprovalRejectionDeliveryOutboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, agentruntime.ErrNotFound
		}
		return false, err
	}
	message = agentruntime.SanitizeRuntimeString(message)
	if len(message) > 4096 {
		message = message[:4096]
	}
	status := agentruntime.RuntimeApprovalRejectionDeliveryOutboxQueued
	availableAt := now.Add(agentruntime.RuntimeApprovalRejectionDeliveryBackoff(row.Attempt))
	if row.Attempt >= agentruntime.MaxRuntimeApprovalRejectionDeliveryAttempts {
		status = agentruntime.RuntimeApprovalRejectionDeliveryOutboxFailed
		availableAt = row.AvailableAt
	}
	result := r.db.WithContext(ctx).Model(&runtimeApprovalRejectionDeliveryOutboxRow{}).Where("id = ? AND revision = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", row.ID, row.Revision, string(agentruntime.RuntimeApprovalRejectionDeliveryOutboxProcessing), owner, now).Updates(map[string]any{"status": string(status), "available_at": availableAt, "lease_owner": "", "lease_expires_at": nil, "last_error": message, "revision": row.Revision + 1, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *runtimeRepository) AcceptRuntimeApprovalRejectionDelivery(ctx context.Context, envelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return false, err
	}
	receivedAt := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existing runtimeApprovalRejectionDeliveryInboxRow
		findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&existing).Error
		if findErr == nil {
			if !runtimeApprovalRejectionDeliveryInboxFromRow(existing).Matches(normalized) {
				return agentruntime.ErrConflict
			}
			duplicate = true
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		var approvalRoute runtimeApprovalRejectionDeliveryInboxRow
		findErr = tx.Where("approval_id = ? AND source = ? AND destination = ?", normalized.ApprovalID, normalized.Source, normalized.Destination).First(&approvalRoute).Error
		if findErr == nil {
			if !runtimeApprovalRejectionDeliveryInboxFromRow(approvalRoute).Matches(normalized) {
				return agentruntime.ErrConflict
			}
			duplicate = true
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(runtimeApprovalRejectionDeliveryInboxToRow(agentruntime.RuntimeApprovalRejectionDeliveryInboxRecordFromEnvelope(normalized, receivedAt)))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			return nil
		}
		var winner runtimeApprovalRejectionDeliveryInboxRow
		if err := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&winner).Error; err != nil {
			return err
		}
		if !runtimeApprovalRejectionDeliveryInboxFromRow(winner).Matches(normalized) {
			return agentruntime.ErrConflict
		}
		duplicate = true
		return nil
	})
	return duplicate, err
}

// AcceptRuntimeApprovalRejectionDeliveryAndApply is the explicit destination
// path for a Workspace-aware receiver. The sidecar transition and inbox
// insertion share one SQLite transaction, so a crash cannot acknowledge a
// rejection while leaving a queued command runnable (or vice versa).
func (r *runtimeRepository) AcceptRuntimeApprovalRejectionDeliveryAndApply(ctx context.Context, envelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return false, err
	}
	receivedAt := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		if err := applyRuntimeApprovalRejectionWorkspaceTx(tx, normalized, receivedAt); err != nil {
			return err
		}
		var applyErr error
		duplicate, applyErr = acceptRuntimeApprovalRejectionDeliveryTx(tx, normalized, receivedAt)
		return applyErr
	})
	return duplicate, err
}

func (r *runtimeRepository) GetRuntimeApprovalRejectionDelivery(ctx context.Context, deliveryID string) (agentruntime.RuntimeApprovalRejectionDeliveryInboxRecord, error) {
	var row runtimeApprovalRejectionDeliveryInboxRow
	if err := r.db.WithContext(ctx).Where("delivery_id = ?", strings.TrimSpace(deliveryID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeApprovalRejectionDeliveryInboxRecord{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeApprovalRejectionDeliveryInboxRecord{}, err
	}
	return runtimeApprovalRejectionDeliveryInboxFromRow(row), nil
}

func (r *runtimeRepository) PrepareRuntimeApprovalRejectionDelivery(ctx context.Context, envelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var existingRow runtimeApprovalRejectionDeliveryTransactionRow
		findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&existingRow).Error
		if findErr == nil {
			existing := runtimeApprovalRejectionDeliveryTransactionFromRow(existingRow)
			if !existing.Matches(normalized) {
				return agentruntime.ErrConflict
			}
			if existing.Status == agentruntime.RuntimeApprovalRejectionDeliveryTransactionCommitted {
				duplicate = true
				return nil
			}
			if existing.Status != agentruntime.RuntimeApprovalRejectionDeliveryTransactionPrepared {
				return agentruntime.ErrConflict
			}
			if existing.ExpiresAt.After(now) {
				duplicate = true
				return nil
			}
		}
		prepared, createErr := agentruntime.NewRuntimeApprovalRejectionDeliveryTransaction(normalized, now)
		if createErr != nil {
			return createErr
		}
		row := runtimeApprovalRejectionDeliveryTransactionToRow(prepared)
		if findErr == nil {
			return tx.Model(&runtimeApprovalRejectionDeliveryTransactionRow{}).Where("delivery_id = ?", normalized.DeliveryID).Updates(map[string]any{"version": row.Version, "approval_id": row.ApprovalID, "invocation_id": row.InvocationID, "tool_call_id": row.ToolCallID, "operation_id": row.OperationID, "source": row.Source, "destination": row.Destination, "decision": row.Decision, "reason_digest": row.ReasonDigest, "decision_at": row.DecisionAt, "issued_at": row.IssuedAt, "signature": row.Signature, "status": row.Status, "expires_at": row.ExpiresAt, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}).Error
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		return tx.Create(row).Error
	})
	return duplicate, err
}

func (r *runtimeRepository) CommitRuntimeApprovalRejectionDelivery(ctx context.Context, envelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeApprovalRejectionDeliveryTransactionRow
		if findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		transaction := runtimeApprovalRejectionDeliveryTransactionFromRow(row)
		if !transaction.Matches(normalized) {
			return agentruntime.ErrConflict
		}
		if transaction.Status == agentruntime.RuntimeApprovalRejectionDeliveryTransactionCommitted {
			duplicate = true
			return nil
		}
		if transaction.Status != agentruntime.RuntimeApprovalRejectionDeliveryTransactionPrepared {
			return agentruntime.ErrConflict
		}
		if !transaction.ExpiresAt.After(now) {
			return agentruntime.ErrRuntimeApprovalRejectionDeliveryStale
		}
		duplicate, err = acceptRuntimeApprovalRejectionDeliveryTx(tx, transaction.Envelope(), now)
		if err != nil {
			return err
		}
		result := tx.Model(&runtimeApprovalRejectionDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeApprovalRejectionDeliveryTransactionPrepared)).Updates(map[string]any{"status": string(agentruntime.RuntimeApprovalRejectionDeliveryTransactionCommitted), "updated_at": now})
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

// CommitRuntimeApprovalRejectionDeliveryAndApply is the transactional
// destination path. Prepare remains metadata-only; Commit applies the local
// Workspace sidecar and publishes the inbox visibility in one transaction.
func (r *runtimeRepository) CommitRuntimeApprovalRejectionDeliveryAndApply(ctx context.Context, envelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope) (bool, error) {
	normalized, err := envelope.Normalize()
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeApprovalRejectionDeliveryTransactionRow
		if findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		transaction := runtimeApprovalRejectionDeliveryTransactionFromRow(row)
		if !transaction.Matches(normalized) {
			return agentruntime.ErrConflict
		}
		if transaction.Status == agentruntime.RuntimeApprovalRejectionDeliveryTransactionCommitted {
			if err := applyRuntimeApprovalRejectionWorkspaceTx(tx, transaction.Envelope(), now); err != nil {
				return err
			}
			duplicate = true
			return nil
		}
		if transaction.Status != agentruntime.RuntimeApprovalRejectionDeliveryTransactionPrepared {
			return agentruntime.ErrConflict
		}
		if !transaction.ExpiresAt.After(now) {
			return agentruntime.ErrRuntimeApprovalRejectionDeliveryStale
		}
		if err := applyRuntimeApprovalRejectionWorkspaceTx(tx, transaction.Envelope(), now); err != nil {
			return err
		}
		var applyErr error
		duplicate, applyErr = acceptRuntimeApprovalRejectionDeliveryTx(tx, transaction.Envelope(), now)
		if applyErr != nil {
			return applyErr
		}
		result := tx.Model(&runtimeApprovalRejectionDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeApprovalRejectionDeliveryTransactionPrepared)).Updates(map[string]any{"status": string(agentruntime.RuntimeApprovalRejectionDeliveryTransactionCommitted), "updated_at": now})
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

func acceptRuntimeApprovalRejectionDeliveryTx(tx *gorm.DB, envelope agentruntime.RuntimeApprovalRejectionDeliveryEnvelope, receivedAt time.Time) (bool, error) {
	var existing runtimeApprovalRejectionDeliveryInboxRow
	findErr := tx.Where("delivery_id = ?", envelope.DeliveryID).First(&existing).Error
	if findErr == nil {
		if !runtimeApprovalRejectionDeliveryInboxFromRow(existing).Matches(envelope) {
			return false, agentruntime.ErrConflict
		}
		return true, nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return false, findErr
	}
	var approvalRoute runtimeApprovalRejectionDeliveryInboxRow
	findErr = tx.Where("approval_id = ? AND source = ? AND destination = ?", envelope.ApprovalID, envelope.Source, envelope.Destination).First(&approvalRoute).Error
	if findErr == nil {
		if !runtimeApprovalRejectionDeliveryInboxFromRow(approvalRoute).Matches(envelope) {
			return false, agentruntime.ErrConflict
		}
		return true, nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return false, findErr
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(runtimeApprovalRejectionDeliveryInboxToRow(agentruntime.RuntimeApprovalRejectionDeliveryInboxRecordFromEnvelope(envelope, receivedAt)))
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 1 {
		return false, nil
	}
	if err := tx.Where("delivery_id = ?", envelope.DeliveryID).First(&existing).Error; err != nil {
		return false, err
	}
	if !runtimeApprovalRejectionDeliveryInboxFromRow(existing).Matches(envelope) {
		return false, agentruntime.ErrConflict
	}
	return true, nil
}

func (r *runtimeRepository) GetRuntimeApprovalRejectionDeliveryTransaction(ctx context.Context, deliveryID string) (agentruntime.RuntimeApprovalRejectionDeliveryTransaction, error) {
	var row runtimeApprovalRejectionDeliveryTransactionRow
	if err := r.db.WithContext(ctx).Where("delivery_id = ?", strings.TrimSpace(deliveryID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeApprovalRejectionDeliveryTransaction{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeApprovalRejectionDeliveryTransaction{}, err
	}
	return runtimeApprovalRejectionDeliveryTransactionFromRow(row), nil
}

var _ agentruntime.RuntimeApprovalRejectionDeliveryOutboxRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeApprovalRejectionDeliveryInbox = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeApprovalRejectionDeliveryTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeApprovalRejectionDeliveryApplyRepository = (*runtimeRepository)(nil)
var _ agentruntime.ApprovalRejectionDeliveryCommitRepository = (*runtimeRepository)(nil)
