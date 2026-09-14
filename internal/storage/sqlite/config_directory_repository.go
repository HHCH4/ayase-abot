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

// runtimeConfigDirectoryRow stores one latest public profile/persona entry.
// The body is retained as JSON so future catalog fields can be added without
// copying provider credentials into columns; the envelope metadata remains
// queryable and is checked against the body digest on every read/write.
type runtimeConfigDirectoryRow struct {
	Kind           string `gorm:"primaryKey;size:16"`
	EntryID        string `gorm:"primaryKey;size:128"`
	Version        int    `gorm:"not null"`
	Revision       int    `gorm:"not null"`
	Name           string `gorm:"size:200;not null"`
	Description    string `gorm:"type:text;not null"`
	Instruction    string `gorm:"type:text;not null"`
	ValuesJSON     string `gorm:"type:text;not null"`
	Enabled        *bool
	Source         string `gorm:"index;size:160;not null"`
	Destination    string `gorm:"index;size:160;not null"`
	DeliveryID     string `gorm:"index;size:220;not null"`
	PreviousDigest string `gorm:"size:71"`
	BodyDigest     string `gorm:"size:71;not null"`
	CorrelationID  string `gorm:"size:512"`
	IdempotencyKey string `gorm:"size:200"`
	Timestamp      time.Time
	IssuedAt       time.Time
	Signature      string `gorm:"size:64"`
	ReceivedAt     time.Time
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (runtimeConfigDirectoryRow) TableName() string { return "abot_runtime_config_directory" }

func runtimeConfigDirectoryRecordFromRow(row runtimeConfigDirectoryRow) (agentruntime.RuntimeConfigDirectoryRecord, error) {
	values := map[string]any(nil)
	if strings.TrimSpace(row.ValuesJSON) != "" {
		if err := json.Unmarshal([]byte(row.ValuesJSON), &values); err != nil {
			return agentruntime.RuntimeConfigDirectoryRecord{}, fmt.Errorf("解析 Runtime 配置目录正文失败: %w", err)
		}
	}
	entry := agentruntime.RuntimeConfigDirectoryEntry{
		Version: row.Version, Kind: agentruntime.RuntimeConfigDirectoryKind(row.Kind), ID: row.EntryID,
		Revision: row.Revision, Name: row.Name, Description: row.Description, Instruction: row.Instruction,
		Values: values, Enabled: cloneBoolPtr(row.Enabled),
	}
	envelope := agentruntime.RuntimeConfigDirectoryEnvelope{
		Version: row.Version, Source: row.Source, Destination: row.Destination, DeliveryID: row.DeliveryID,
		Kind: agentruntime.RuntimeConfigDirectoryKind(row.Kind), EntryID: row.EntryID, Revision: row.Revision,
		PreviousDigest: row.PreviousDigest, BodyDigest: row.BodyDigest, CorrelationID: row.CorrelationID,
		IdempotencyKey: row.IdempotencyKey, Timestamp: row.Timestamp, IssuedAt: row.IssuedAt, Signature: row.Signature,
	}
	normalizedEnvelope, normalizedEntry, err := normalizeSQLiteRuntimeConfigDirectoryPair(envelope, entry)
	if err != nil {
		return agentruntime.RuntimeConfigDirectoryRecord{}, err
	}
	return agentruntime.RuntimeConfigDirectoryRecord{Envelope: normalizedEnvelope, Entry: normalizedEntry, ReceivedAt: row.ReceivedAt}, nil
}

func normalizeSQLiteRuntimeConfigDirectoryPair(envelope agentruntime.RuntimeConfigDirectoryEnvelope, entry agentruntime.RuntimeConfigDirectoryEntry) (agentruntime.RuntimeConfigDirectoryEnvelope, agentruntime.RuntimeConfigDirectoryEntry, error) {
	return agentruntime.NormalizeRuntimeConfigDirectoryPair(envelope, entry)
}

func runtimeConfigDirectoryRecordToRow(record agentruntime.RuntimeConfigDirectoryRecord) (*runtimeConfigDirectoryRow, error) {
	envelope, entry, err := normalizeSQLiteRuntimeConfigDirectoryPair(record.Envelope, record.Entry)
	if err != nil {
		return nil, err
	}
	valuesJSON := ""
	if entry.Values != nil {
		encoded, marshalErr := json.Marshal(entry.Values)
		if marshalErr != nil {
			return nil, fmt.Errorf("%w: values 编码失败", agentruntime.ErrInvalidRuntimeConfigDirectory)
		}
		if len(encoded) > agentruntime.MaxRuntimeConfigDirectoryEntryBytes {
			return nil, fmt.Errorf("%w: values 超过字节上限", agentruntime.ErrInvalidRuntimeConfigDirectory)
		}
		valuesJSON = string(encoded)
	}
	return &runtimeConfigDirectoryRow{
		Kind: string(entry.Kind), EntryID: entry.ID, Version: entry.Version, Revision: entry.Revision,
		Name: entry.Name, Description: entry.Description, Instruction: entry.Instruction, ValuesJSON: valuesJSON,
		Enabled: cloneBoolPtr(entry.Enabled), Source: envelope.Source, Destination: envelope.Destination,
		DeliveryID: envelope.DeliveryID, PreviousDigest: envelope.PreviousDigest, BodyDigest: envelope.BodyDigest,
		CorrelationID: envelope.CorrelationID, IdempotencyKey: envelope.IdempotencyKey, Timestamp: envelope.Timestamp,
		IssuedAt: envelope.IssuedAt, Signature: envelope.Signature, ReceivedAt: record.ReceivedAt.UTC(),
	}, nil
}

func runtimeConfigDirectoryRowUpdates(row *runtimeConfigDirectoryRow) map[string]any {
	return map[string]any{
		"version": row.Version, "revision": row.Revision, "name": row.Name, "description": row.Description,
		"instruction": row.Instruction, "values_json": row.ValuesJSON, "enabled": row.Enabled,
		"source": row.Source, "destination": row.Destination, "delivery_id": row.DeliveryID,
		"previous_digest": row.PreviousDigest, "body_digest": row.BodyDigest, "correlation_id": row.CorrelationID,
		"idempotency_key": row.IdempotencyKey, "timestamp": row.Timestamp, "issued_at": row.IssuedAt,
		"signature": row.Signature, "received_at": row.ReceivedAt,
	}
}

func (r *runtimeRepository) AcceptRuntimeConfigDirectory(ctx context.Context, envelope agentruntime.RuntimeConfigDirectoryEnvelope, entry agentruntime.RuntimeConfigDirectoryEntry) (bool, error) {
	normalizedEnvelope, normalizedEntry, err := normalizeSQLiteRuntimeConfigDirectoryPair(envelope, entry)
	if err != nil {
		return false, err
	}
	var duplicate bool
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row runtimeConfigDirectoryRow
		findErr := tx.Where("kind = ? AND entry_id = ?", normalizedEnvelope.Kind, normalizedEnvelope.EntryID).First(&row).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			var record agentruntime.RuntimeConfigDirectoryRecord
			_, acceptErr := agentruntime.AcceptRuntimeConfigDirectoryRecord(&record, normalizedEnvelope, normalizedEntry, time.Now().UTC())
			if acceptErr != nil {
				return acceptErr
			}
			stored, rowErr := runtimeConfigDirectoryRecordToRow(record)
			if rowErr != nil {
				return rowErr
			}
			return tx.Create(stored).Error
		}
		if findErr != nil {
			return findErr
		}
		existing, decodeErr := runtimeConfigDirectoryRecordFromRow(row)
		if decodeErr != nil {
			return decodeErr
		}
		var acceptErr error
		duplicate, acceptErr = agentruntime.AcceptRuntimeConfigDirectoryRecord(&existing, normalizedEnvelope, normalizedEntry, time.Now().UTC())
		if acceptErr != nil || duplicate {
			return acceptErr
		}
		updated, rowErr := runtimeConfigDirectoryRecordToRow(existing)
		if rowErr != nil {
			return rowErr
		}
		result := tx.Model(&runtimeConfigDirectoryRow{}).Where("kind = ? AND entry_id = ? AND revision = ?", row.Kind, row.EntryID, row.Revision).Updates(runtimeConfigDirectoryRowUpdates(updated))
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

func (r *runtimeRepository) GetRuntimeConfigDirectory(ctx context.Context, kind agentruntime.RuntimeConfigDirectoryKind, id string) (agentruntime.RuntimeConfigDirectoryRecord, error) {
	var row runtimeConfigDirectoryRow
	if err := r.db.WithContext(ctx).Where("kind = ? AND entry_id = ?", strings.TrimSpace(strings.ToLower(string(kind))), strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentruntime.RuntimeConfigDirectoryRecord{}, agentruntime.ErrNotFound
		}
		return agentruntime.RuntimeConfigDirectoryRecord{}, err
	}
	return runtimeConfigDirectoryRecordFromRow(row)
}
