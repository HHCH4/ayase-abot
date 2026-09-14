package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// runtimeEventDeliveryTransactionRow is the destination-side prepare/commit
// ledger. It stores only authenticated event metadata; event bodies remain in
// the source event log.
type runtimeEventDeliveryTransactionRow struct {
	Version      int       `gorm:"not null"`
	DeliveryID   string    `gorm:"primaryKey;size:220"`
	EventID      string    `gorm:"index;size:512;not null"`
	Source       string    `gorm:"index;size:160;not null"`
	Destination  string    `gorm:"index;size:160;not null"`
	InvocationID string    `gorm:"index;size:512;not null"`
	Sequence     int64     `gorm:"not null"`
	Type         string    `gorm:"size:80;not null"`
	Timestamp    time.Time `gorm:"index"`
	IssuedAt     time.Time `gorm:"index"`
	EventDigest  string    `gorm:"size:64;not null"`
	Signature    string    `gorm:"size:64"`
	Status       string    `gorm:"index;size:32;not null"`
	ExpiresAt    time.Time `gorm:"index"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (runtimeEventDeliveryTransactionRow) TableName() string {
	return "abot_agent_event_delivery_transactions"
}

func runtimeEventDeliveryTransactionFromRow(row runtimeEventDeliveryTransactionRow) (agentruntime.RuntimeEventDeliveryTransaction, error) {
	item := agentruntime.RuntimeEventDeliveryTransaction{
		Version: row.Version, DeliveryID: row.DeliveryID, EventID: row.EventID, Source: row.Source,
		Destination: row.Destination, InvocationID: row.InvocationID, Sequence: row.Sequence, Type: row.Type,
		Timestamp: row.Timestamp, IssuedAt: row.IssuedAt, EventDigest: row.EventDigest, Signature: row.Signature,
		Status: agentruntime.RuntimeEventDeliveryTransactionStatus(row.Status), ExpiresAt: row.ExpiresAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if _, err := agentruntime.NormalizeRuntimeEventDeliveryEnvelope(item.Envelope()); err != nil {
		return agentruntime.RuntimeEventDeliveryTransaction{}, err
	}
	switch item.Status {
	case agentruntime.RuntimeEventDeliveryTransactionPrepared, agentruntime.RuntimeEventDeliveryTransactionCommitted, agentruntime.RuntimeEventDeliveryTransactionAborted:
	default:
		return agentruntime.RuntimeEventDeliveryTransaction{}, fmt.Errorf("%w: event transaction status %q 不受支持", agentruntime.ErrInvalidRuntimeEventDelivery, item.Status)
	}
	return item, nil
}

func runtimeEventDeliveryTransactionToRow(item agentruntime.RuntimeEventDeliveryTransaction) (*runtimeEventDeliveryTransactionRow, error) {
	if _, err := agentruntime.NormalizeRuntimeEventDeliveryEnvelope(item.Envelope()); err != nil {
		return nil, err
	}
	return &runtimeEventDeliveryTransactionRow{
		Version: item.Version, DeliveryID: item.DeliveryID, EventID: item.EventID, Source: item.Source,
		Destination: item.Destination, InvocationID: item.InvocationID, Sequence: item.Sequence, Type: item.Type,
		Timestamp: item.Timestamp, IssuedAt: item.IssuedAt, EventDigest: item.EventDigest, Signature: item.Signature,
		Status: string(item.Status), ExpiresAt: item.ExpiresAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

// acceptRuntimeEventDeliveryTx is shared by direct one-phase receipt and
// transactional commit so a commit can atomically update the inbox and the
// transaction status in one SQLite transaction.
func acceptRuntimeEventDeliveryTx(tx *gorm.DB, normalized agentruntime.RuntimeEventDeliveryEnvelope, receivedAt time.Time) (bool, error) {
	var existing runtimeEventDeliveryInboxRow
	findErr := tx.Where("event_id = ?", normalized.EventID).First(&existing).Error
	if findErr == nil {
		if !runtimeEventDeliveryInboxFromRow(existing).Matches(normalized) {
			return false, agentruntime.ErrConflict
		}
		return true, nil
	}
	if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return false, findErr
	}
	row := runtimeEventDeliveryInboxToRow(agentruntime.RuntimeEventDeliveryInboxRecordFromEnvelope(normalized, receivedAt))
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 1 {
		return false, nil
	}
	if err := tx.Where("event_id = ?", normalized.EventID).First(&existing).Error; err != nil {
		return false, err
	}
	if !runtimeEventDeliveryInboxFromRow(existing).Matches(normalized) {
		return false, agentruntime.ErrConflict
	}
	return true, nil
}

func eventDeliveryTransactionExpired(item agentruntime.RuntimeEventDeliveryTransaction, now time.Time) bool {
	return !item.ExpiresAt.After(now)
}

func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "sqlite_busy")
}

// withSQLiteBusyRetry keeps concurrent idempotency callers from surfacing a
// transient SQLite writer lock as a delivery failure. The transaction body is
// safe to replay because both prepare and commit are idempotent by
// DeliveryID, and the retry budget remains bounded.
func withSQLiteBusyRetry(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		lastErr = db.WithContext(ctx).Transaction(fn)
		if !isSQLiteBusyError(lastErr) {
			return lastErr
		}
		if attempt == 7 {
			break
		}
		delay := time.Duration(5*(1<<uint(attempt))) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func prepareRuntimeEventDeliveryTx(tx *gorm.DB, normalized agentruntime.RuntimeEventDeliveryEnvelope, now time.Time) (bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var existingRow runtimeEventDeliveryTransactionRow
		findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&existingRow).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			prepared, err := agentruntime.NewRuntimeEventDeliveryTransaction(normalized, now)
			if err != nil {
				return false, err
			}
			row, err := runtimeEventDeliveryTransactionToRow(prepared)
			if err != nil {
				return false, err
			}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
			if result.Error != nil {
				return false, result.Error
			}
			if result.RowsAffected == 1 {
				return false, nil
			}
			continue
		}
		if findErr != nil {
			return false, findErr
		}
		existing, err := runtimeEventDeliveryTransactionFromRow(existingRow)
		if err != nil {
			return false, err
		}
		if !existing.Matches(normalized) {
			return false, agentruntime.ErrConflict
		}
		switch existing.Status {
		case agentruntime.RuntimeEventDeliveryTransactionCommitted:
			return true, nil
		case agentruntime.RuntimeEventDeliveryTransactionPrepared:
			if !eventDeliveryTransactionExpired(existing, now) {
				return true, nil
			}
			prepared, createErr := agentruntime.NewRuntimeEventDeliveryTransaction(normalized, now)
			if createErr != nil {
				return false, createErr
			}
			prepared.CreatedAt = existing.CreatedAt
			row, rowErr := runtimeEventDeliveryTransactionToRow(prepared)
			if rowErr != nil {
				return false, rowErr
			}
			result := tx.Model(&runtimeEventDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", normalized.DeliveryID, string(agentruntime.RuntimeEventDeliveryTransactionPrepared)).Updates(map[string]any{
				"version": row.Version, "event_id": row.EventID, "source": row.Source, "destination": row.Destination,
				"invocation_id": row.InvocationID, "sequence": row.Sequence, "type": row.Type, "timestamp": row.Timestamp, "issued_at": row.IssuedAt,
				"event_digest": row.EventDigest, "signature": row.Signature, "status": row.Status, "expires_at": row.ExpiresAt,
				"created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
			})
			if result.Error != nil {
				return false, result.Error
			}
			if result.RowsAffected == 1 {
				return false, nil
			}
			continue
		case agentruntime.RuntimeEventDeliveryTransactionAborted:
			return false, agentruntime.ErrConflict
		default:
			return false, agentruntime.ErrConflict
		}
	}
	return false, agentruntime.ErrConflict
}

func (r *runtimeRepository) PrepareRuntimeEventDelivery(ctx context.Context, envelope agentruntime.RuntimeEventDeliveryEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var prepareErr error
		duplicate, prepareErr = prepareRuntimeEventDeliveryTx(tx, normalized, now)
		return prepareErr
	})
	return duplicate, err
}

func (r *runtimeRepository) CommitRuntimeEventDelivery(ctx context.Context, envelope agentruntime.RuntimeEventDeliveryEnvelope) (bool, error) {
	normalized, err := agentruntime.NormalizeRuntimeEventDeliveryEnvelope(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	duplicate := false
	err = withSQLiteBusyRetry(ctx, r.db, func(tx *gorm.DB) error {
		var row runtimeEventDeliveryTransactionRow
		if findErr := tx.Where("delivery_id = ?", normalized.DeliveryID).First(&row).Error; findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return findErr
		}
		transaction, decodeErr := runtimeEventDeliveryTransactionFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !transaction.Matches(normalized) {
			return agentruntime.ErrConflict
		}
		switch transaction.Status {
		case agentruntime.RuntimeEventDeliveryTransactionCommitted:
			duplicate = true
			return nil
		case agentruntime.RuntimeEventDeliveryTransactionPrepared:
			if eventDeliveryTransactionExpired(transaction, now) {
				return agentruntime.ErrRuntimeEventDeliveryStale
			}
		default:
			return agentruntime.ErrConflict
		}
		duplicate, err = acceptRuntimeEventDeliveryTx(tx, transaction.Envelope(), now)
		if err != nil {
			return err
		}
		result := tx.Model(&runtimeEventDeliveryTransactionRow{}).Where("delivery_id = ? AND status = ?", transaction.DeliveryID, string(agentruntime.RuntimeEventDeliveryTransactionPrepared)).Updates(map[string]any{
			"status": string(agentruntime.RuntimeEventDeliveryTransactionCommitted), "updated_at": now,
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

func (r *runtimeRepository) GetRuntimeEventDeliveryTransaction(ctx context.Context, deliveryID string) (agentruntime.RuntimeEventDeliveryTransaction, error) {
	var row runtimeEventDeliveryTransactionRow
	if err := r.db.WithContext(ctx).Where("delivery_id = ?", strings.TrimSpace(deliveryID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeEventDeliveryTransaction{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeEventDeliveryTransaction{}, err
	}
	return runtimeEventDeliveryTransactionFromRow(row)
}

var _ agentruntime.RuntimeEventDeliveryTransactionRepository = (*runtimeRepository)(nil)
