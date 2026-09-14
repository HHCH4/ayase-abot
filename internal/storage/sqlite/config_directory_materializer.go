package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/config"
	"gorm.io/gorm"
)

// MaterializeRuntimeConfigDirectory applies an already accepted directory
// record to the real profile/persona catalog. It is intentionally separate
// from the runtime directory inbox: callers must opt into this capability and
// the transaction never changes default markers or binding rows.
func (r *configRepository) MaterializeRuntimeConfigDirectory(ctx context.Context, record agentruntime.RuntimeConfigDirectoryRecord) (bool, error) {
	envelope, entry, err := agentruntime.NormalizeRuntimeConfigDirectoryPair(record.Envelope, record.Entry)
	if err != nil {
		return false, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	validator, err := config.NewService(r, nil)
	if err != nil {
		return false, err
	}
	if entry.Kind == agentruntime.RuntimeConfigDirectoryKindProfile {
		if err := validator.ValidateValues(ctx, config.Values(entry.Values)); err != nil {
			return false, fmt.Errorf("%w: profile values 校验失败: %v", agentruntime.ErrInvalidRuntimeConfigDirectoryMaterialization, err)
		}
	}
	now := time.Now().UTC()
	if record.ReceivedAt.IsZero() {
		record.ReceivedAt = now
	}
	return materializeRuntimeConfigDirectoryInTransaction(ctx, r.db, envelope, entry, record.ReceivedAt.UTC(), now)
}

func materializeRuntimeConfigDirectoryInTransaction(ctx context.Context, db *gorm.DB, envelope agentruntime.RuntimeConfigDirectoryEnvelope, entry agentruntime.RuntimeConfigDirectoryEntry, receivedAt, now time.Time) (bool, error) {
	var duplicate bool
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		switch entry.Kind {
		case agentruntime.RuntimeConfigDirectoryKindProfile:
			var row configProfileRow
			findErr := tx.Where("id = ?", entry.ID).First(&row).Error
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				if envelope.PreviousDigest != "" {
					return fmt.Errorf("%w: 新 profile 不能携带 previous_digest", agentruntime.ErrRuntimeConfigDirectoryConflict)
				}
				if err := ensureProfileMaterializationNameAvailable(tx, entry.Name, entry.ID); err != nil {
					return err
				}
				valuesJSON, err := marshalDirectoryValues(entry.Values)
				if err != nil {
					return err
				}
				createdAt := now
				row = configProfileRow{ID: entry.ID, Name: entry.Name, Revision: entry.Revision, Values: valuesJSON, IsDefault: false, CreatedAt: createdAt, UpdatedAt: now}
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
				return tx.Create(&configRevisionRow{ProfileID: entry.ID, Revision: entry.Revision, Values: valuesJSON, CreatedAt: receivedAt}).Error
			}
			if findErr != nil {
				return findErr
			}
			currentProfile, err := profileFromRow(row)
			if err != nil {
				return err
			}
			currentEntry, err := agentruntime.NewRuntimeConfigDirectoryProfileEntry(currentProfile)
			if err != nil {
				return err
			}
			currentDigest, err := agentruntime.RuntimeConfigDirectoryEntryDigest(currentEntry)
			if err != nil {
				return err
			}
			if row.Revision > entry.Revision {
				return agentruntime.ErrRuntimeConfigDirectoryStale
			}
			if row.Revision == entry.Revision {
				if currentDigest != envelope.BodyDigest {
					return fmt.Errorf("%w: 相同 revision 的 profile 正文不一致", agentruntime.ErrRuntimeConfigDirectoryConflict)
				}
				duplicate = true
				return nil
			}
			if envelope.PreviousDigest == "" || envelope.PreviousDigest != currentDigest {
				return fmt.Errorf("%w: profile previous_digest 不匹配当前目录", agentruntime.ErrRuntimeConfigDirectoryConflict)
			}
			if err := ensureProfileMaterializationNameAvailable(tx, entry.Name, entry.ID); err != nil {
				return err
			}
			valuesJSON, err := marshalDirectoryValues(entry.Values)
			if err != nil {
				return err
			}
			updated := configProfileRow{ID: entry.ID, Name: entry.Name, Revision: entry.Revision, Values: valuesJSON, IsDefault: row.IsDefault, CreatedAt: row.CreatedAt, UpdatedAt: now}
			result := tx.Model(&configProfileRow{}).Where("id = ? AND revision = ?", row.ID, row.Revision).Updates(map[string]any{"name": updated.Name, "revision": updated.Revision, "values": updated.Values, "updated_at": updated.UpdatedAt})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			return tx.Create(&configRevisionRow{ProfileID: entry.ID, Revision: entry.Revision, Values: valuesJSON, CreatedAt: receivedAt}).Error

		case agentruntime.RuntimeConfigDirectoryKindPersona:
			var row personaRow
			findErr := tx.Where("id = ?", entry.ID).First(&row).Error
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				if envelope.PreviousDigest != "" {
					return fmt.Errorf("%w: 新 persona 不能携带 previous_digest", agentruntime.ErrRuntimeConfigDirectoryConflict)
				}
				if err := ensurePersonaMaterializationNameAvailable(tx, entry.Name, entry.ID); err != nil {
					return err
				}
				enabled := true
				if entry.Enabled != nil {
					enabled = *entry.Enabled
				}
				row = personaRow{ID: entry.ID, Name: entry.Name, Description: entry.Description, Instruction: entry.Instruction, Revision: entry.Revision, IsDefault: false, Enabled: enabled, CreatedAt: now, UpdatedAt: now}
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
				return tx.Create(&personaRevisionRow{PersonaID: entry.ID, Revision: entry.Revision, Name: entry.Name, Description: entry.Description, Instruction: entry.Instruction, Enabled: enabled, CreatedAt: receivedAt}).Error
			}
			if findErr != nil {
				return findErr
			}
			currentEntry, err := agentruntime.NewRuntimeConfigDirectoryPersonaEntry(personaFromRow(row))
			if err != nil {
				return err
			}
			currentDigest, err := agentruntime.RuntimeConfigDirectoryEntryDigest(currentEntry)
			if err != nil {
				return err
			}
			if row.Revision > entry.Revision {
				return agentruntime.ErrRuntimeConfigDirectoryStale
			}
			if row.Revision == entry.Revision {
				if currentDigest != envelope.BodyDigest {
					return fmt.Errorf("%w: 相同 revision 的 persona 正文不一致", agentruntime.ErrRuntimeConfigDirectoryConflict)
				}
				duplicate = true
				return nil
			}
			if envelope.PreviousDigest == "" || envelope.PreviousDigest != currentDigest {
				return fmt.Errorf("%w: persona previous_digest 不匹配当前目录", agentruntime.ErrRuntimeConfigDirectoryConflict)
			}
			if err := ensurePersonaMaterializationNameAvailable(tx, entry.Name, entry.ID); err != nil {
				return err
			}
			enabled := true
			if entry.Enabled != nil {
				enabled = *entry.Enabled
			}
			result := tx.Model(&personaRow{}).Where("id = ? AND revision = ?", row.ID, row.Revision).Updates(map[string]any{"name": entry.Name, "description": entry.Description, "instruction": entry.Instruction, "revision": entry.Revision, "enabled": enabled, "updated_at": now})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return agentruntime.ErrConflict
			}
			return tx.Create(&personaRevisionRow{PersonaID: entry.ID, Revision: entry.Revision, Name: entry.Name, Description: entry.Description, Instruction: entry.Instruction, Enabled: enabled, CreatedAt: receivedAt}).Error
		default:
			return fmt.Errorf("%w: 不支持的 directory kind", agentruntime.ErrInvalidRuntimeConfigDirectoryMaterialization)
		}
	})
	return duplicate, err
}

func marshalDirectoryValues(values map[string]any) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("%w: profile values 编码失败", agentruntime.ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	if len(encoded) > agentruntime.MaxRuntimeConfigDirectoryEntryBytes {
		return "", fmt.Errorf("%w: profile values 超过字节上限", agentruntime.ErrInvalidRuntimeConfigDirectoryMaterialization)
	}
	return string(encoded), nil
}

func ensureProfileMaterializationNameAvailable(tx *gorm.DB, name, id string) error {
	var count int64
	if err := tx.Model(&configProfileRow{}).Where("lower(name) = lower(?) AND id <> ?", strings.TrimSpace(name), strings.TrimSpace(id)).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: profile 名称已存在", agentruntime.ErrRuntimeConfigDirectoryConflict)
	}
	return nil
}

func ensurePersonaMaterializationNameAvailable(tx *gorm.DB, name, id string) error {
	var count int64
	if err := tx.Model(&personaRow{}).Where("lower(name) = lower(?) AND id <> ?", strings.TrimSpace(name), strings.TrimSpace(id)).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: persona 名称已存在", agentruntime.ErrRuntimeConfigDirectoryConflict)
	}
	return nil
}

var _ agentruntime.RuntimeConfigDirectoryMaterializer = (*configRepository)(nil)
