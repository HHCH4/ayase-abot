package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/config"
	"gorm.io/gorm"
)

// configProfileRow 保存当前配置修订；Values 使用 JSON，便于 Schema 增加字段而不频繁改表结构。
type configProfileRow struct {
	ID        string `gorm:"primaryKey;size:64"`
	Name      string `gorm:"size:200;not null"`
	Revision  int
	Values    string `gorm:"type:text;not null"`
	IsDefault bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (configProfileRow) TableName() string { return "abot_config_profiles" }

// configRevisionRow 是追加式修订历史，删除配置文件时一并物理清理。
type configRevisionRow struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	ProfileID string `gorm:"index;size:64;not null"`
	Revision  int
	Values    string `gorm:"type:text;not null"`
	CreatedAt time.Time
}

func (configRevisionRow) TableName() string { return "abot_config_revisions" }

// configBindingRow 分开保存机器人和对话绑定，空 profile_id 表示解除绑定且不保留空记录。
type configBindingRow struct {
	Scope     string `gorm:"primaryKey;size:32"`
	TargetID  string `gorm:"primaryKey;size:200"`
	ProfileID string `gorm:"index;size:64;not null"`
	UpdatedAt time.Time
}

func (configBindingRow) TableName() string { return "abot_config_bindings" }

// personaRow stores the current independently selectable persona revision.
type personaRow struct {
	ID          string `gorm:"primaryKey;size:64"`
	Name        string `gorm:"size:200;not null"`
	Description string `gorm:"type:text;not null"`
	Instruction string `gorm:"type:text;not null"`
	Revision    int
	IsDefault   bool
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (personaRow) TableName() string { return "abot_personas" }

// personaRevisionRow is append-only and never replaces an earlier revision.
type personaRevisionRow struct {
	ID          uint   `gorm:"primaryKey;autoIncrement"`
	PersonaID   string `gorm:"index;size:64;not null"`
	Revision    int
	Name        string `gorm:"size:200;not null"`
	Description string `gorm:"type:text;not null"`
	Instruction string `gorm:"type:text;not null"`
	Enabled     bool
	CreatedAt   time.Time
}

func (personaRevisionRow) TableName() string { return "abot_persona_revisions" }

// personaBindingRow keeps bot and conversation selection separate from the
// persona catalog. An absent row means the caller should continue inheritance.
type personaBindingRow struct {
	Scope     string `gorm:"primaryKey;size:32"`
	TargetID  string `gorm:"primaryKey;size:200"`
	PersonaID string `gorm:"index;size:64;not null"`
	UpdatedAt time.Time
}

func (personaBindingRow) TableName() string { return "abot_persona_bindings" }

type systemSettingsRow struct {
	ID        uint   `gorm:"primaryKey"`
	Values    string `gorm:"type:text;not null"`
	UpdatedAt time.Time
}

func (systemSettingsRow) TableName() string { return "abot_system_settings" }

type configRepository struct {
	db *gorm.DB
}

var _ config.Repository = (*configRepository)(nil)
var _ config.PersonaRepository = (*configRepository)(nil)

func (r *configRepository) ListProfiles(ctx context.Context) ([]config.Profile, error) {
	var rows []configProfileRow
	if err := r.db.WithContext(ctx).Order("is_default DESC, name ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]config.Profile, 0, len(rows))
	for _, row := range rows {
		item, err := profileFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *configRepository) GetProfile(ctx context.Context, id string) (config.Profile, error) {
	var row configProfileRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return config.Profile{}, config.ErrNotFound
		}
		return config.Profile{}, err
	}
	return profileFromRow(row)
}

// ListRevisions 读取配置文件的不可变历史快照，配置文件删除时由同一事务清理。
func (r *configRepository) ListRevisions(ctx context.Context, profileID string) ([]config.Revision, error) {
	var rows []configRevisionRow
	if err := r.db.WithContext(ctx).Where("profile_id = ?", strings.TrimSpace(profileID)).Order("revision DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]config.Revision, 0, len(rows))
	for _, row := range rows {
		values := config.Values{}
		if strings.TrimSpace(row.Values) != "" {
			if err := json.Unmarshal([]byte(row.Values), &values); err != nil {
				return nil, fmt.Errorf("解析配置修订 %d 失败: %w", row.Revision, err)
			}
		}
		result = append(result, config.Revision{ProfileID: row.ProfileID, Number: row.Revision, Values: values, CreatedAt: row.CreatedAt})
	}
	return result, nil
}

func (r *configRepository) SaveProfile(ctx context.Context, item config.Profile, revision config.Revision) error {
	values, err := json.Marshal(item.Values)
	if err != nil {
		return fmt.Errorf("编码配置值失败: %w", err)
	}
	revisionValues, err := json.Marshal(revision.Values)
	if err != nil {
		return fmt.Errorf("编码配置修订失败: %w", err)
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old configProfileRow
		findErr := tx.Where("id = ?", item.ID).First(&old).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		row := configProfileRow{
			ID: item.ID, Name: item.Name, Revision: item.Revision, Values: string(values),
			IsDefault: item.IsDefault, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}
		if findErr == nil {
			row.CreatedAt = old.CreatedAt
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Now().UTC()
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = row.CreatedAt
		}
		if row.IsDefault {
			if err := tx.Model(&configProfileRow{}).Where("id <> ?", row.ID).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		revisionRow := configRevisionRow{ProfileID: item.ID, Revision: revision.Number, Values: string(revisionValues), CreatedAt: revision.CreatedAt}
		if revisionRow.CreatedAt.IsZero() {
			revisionRow.CreatedAt = time.Now().UTC()
		}
		if err := tx.Create(&revisionRow).Error; err != nil {
			return err
		}
		return nil
	})
}

func (r *configRepository) DeleteProfile(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var profile configProfileRow
		if err := tx.Where("id = ?", id).First(&profile).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return config.ErrNotFound
			}
			return err
		}
		var bindings int64
		if err := tx.Model(&configBindingRow{}).Where("profile_id = ?", id).Count(&bindings).Error; err != nil {
			return err
		}
		if bindings > 0 {
			return config.ErrProfileInUse
		}
		if err := tx.Where("profile_id = ?", id).Delete(&configRevisionRow{}).Error; err != nil {
			return err
		}
		if result := tx.Where("id = ?", id).Delete(&configProfileRow{}); result.Error != nil {
			return result.Error
		} else if result.RowsAffected == 0 {
			return config.ErrNotFound
		}
		return nil
	})
}

func (r *configRepository) SetDefaultProfile(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var profile configProfileRow
		if err := tx.Where("id = ?", id).First(&profile).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return config.ErrNotFound
			}
			return err
		}
		if err := tx.Model(&configProfileRow{}).Where("id <> ?", id).Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&configProfileRow{}).Where("id = ?", id).Update("is_default", true).Error
	})
}

func (r *configRepository) DefaultProfileID(ctx context.Context) (string, error) {
	var row configProfileRow
	if err := r.db.WithContext(ctx).Where("is_default = ?", true).Order("id ASC").First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", config.ErrNotFound
		}
		return "", err
	}
	return row.ID, nil
}

func (r *configRepository) Bind(ctx context.Context, scope config.BindingScope, targetID, profileID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if strings.TrimSpace(profileID) == "" {
			return tx.Where("scope = ? AND target_id = ?", string(scope), targetID).Delete(&configBindingRow{}).Error
		}
		row := configBindingRow{Scope: string(scope), TargetID: targetID, ProfileID: profileID, UpdatedAt: time.Now().UTC()}
		return tx.Where("scope = ? AND target_id = ?", row.Scope, row.TargetID).Assign(row).FirstOrCreate(&row).Error
	})
}

func (r *configRepository) GetBinding(ctx context.Context, scope config.BindingScope, targetID string) (string, error) {
	var row configBindingRow
	if err := r.db.WithContext(ctx).Where("scope = ? AND target_id = ?", string(scope), targetID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return row.ProfileID, nil
}

func (r *configRepository) SaveSystemSettings(ctx context.Context, settings config.SystemSettings) error {
	values, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	row := systemSettingsRow{ID: 1, Values: string(values), UpdatedAt: time.Now().UTC()}
	return r.db.WithContext(ctx).Where("id = ?", 1).Assign(row).FirstOrCreate(&row).Error
}

func (r *configRepository) GetSystemSettings(ctx context.Context) (config.SystemSettings, error) {
	var row systemSettingsRow
	if err := r.db.WithContext(ctx).Where("id = ?", 1).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return config.SystemSettings{}, nil
		}
		return config.SystemSettings{}, err
	}
	var result config.SystemSettings
	if err := json.Unmarshal([]byte(row.Values), &result); err != nil {
		return config.SystemSettings{}, fmt.Errorf("解析系统设置失败: %w", err)
	}
	return result, nil
}

func profileFromRow(row configProfileRow) (config.Profile, error) {
	values := config.Values{}
	if strings.TrimSpace(row.Values) != "" {
		if err := json.Unmarshal([]byte(row.Values), &values); err != nil {
			return config.Profile{}, fmt.Errorf("解析配置文件 %q 失败: %w", row.ID, err)
		}
	}
	return config.Profile{ID: row.ID, Name: row.Name, Revision: row.Revision, Values: values, IsDefault: row.IsDefault, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

func (r *configRepository) ListPersonas(ctx context.Context) ([]config.Persona, error) {
	var rows []personaRow
	if err := r.db.WithContext(ctx).Order("is_default DESC, name ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]config.Persona, 0, len(rows))
	for _, row := range rows {
		items = append(items, personaFromRow(row))
	}
	return items, nil
}

func (r *configRepository) GetPersona(ctx context.Context, id string) (config.Persona, error) {
	var row personaRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return config.Persona{}, config.ErrNotFound
		}
		return config.Persona{}, err
	}
	return personaFromRow(row), nil
}

func (r *configRepository) ListPersonaRevisions(ctx context.Context, personaID string) ([]config.PersonaRevision, error) {
	var rows []personaRevisionRow
	if err := r.db.WithContext(ctx).Where("persona_id = ?", strings.TrimSpace(personaID)).Order("revision DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]config.PersonaRevision, 0, len(rows))
	for _, row := range rows {
		items = append(items, config.PersonaRevision{
			PersonaID: row.PersonaID, Number: row.Revision, Name: row.Name, Description: row.Description,
			Instruction: row.Instruction, Enabled: row.Enabled, CreatedAt: row.CreatedAt,
		})
	}
	return items, nil
}

func (r *configRepository) SavePersona(ctx context.Context, item config.Persona, revision config.PersonaRevision) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old personaRow
		findErr := tx.Where("id = ?", item.ID).First(&old).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		row := personaRow{
			ID: item.ID, Name: item.Name, Description: item.Description, Instruction: item.Instruction,
			Revision: item.Revision, IsDefault: item.IsDefault, Enabled: item.Enabled,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}
		if findErr == nil {
			row.CreatedAt = old.CreatedAt
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Now().UTC()
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = row.CreatedAt
		}
		if row.IsDefault {
			if err := tx.Model(&personaRow{}).Where("id <> ?", row.ID).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		revisionRow := personaRevisionRow{
			PersonaID: revision.PersonaID, Revision: revision.Number, Name: revision.Name,
			Description: revision.Description, Instruction: revision.Instruction,
			Enabled: revision.Enabled, CreatedAt: revision.CreatedAt,
		}
		if revisionRow.PersonaID == "" {
			revisionRow.PersonaID = row.ID
		}
		if revisionRow.Name == "" {
			revisionRow.Name = row.Name
		}
		if revisionRow.Description == "" {
			revisionRow.Description = row.Description
		}
		if revisionRow.Instruction == "" {
			revisionRow.Instruction = row.Instruction
		}
		if revisionRow.Revision == 0 {
			revisionRow.Revision = row.Revision
		}
		if revisionRow.CreatedAt.IsZero() {
			revisionRow.CreatedAt = row.UpdatedAt
		}
		return tx.Create(&revisionRow).Error
	})
}

func (r *configRepository) DeletePersona(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		id = strings.TrimSpace(id)
		var item personaRow
		if err := tx.Where("id = ?", id).First(&item).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return config.ErrNotFound
			}
			return err
		}
		var bindings int64
		if err := tx.Model(&personaBindingRow{}).Where("persona_id = ?", id).Count(&bindings).Error; err != nil {
			return err
		}
		if bindings > 0 {
			return config.ErrPersonaInUse
		}
		if err := tx.Where("persona_id = ?", id).Delete(&personaRevisionRow{}).Error; err != nil {
			return err
		}
		if result := tx.Where("id = ?", id).Delete(&personaRow{}); result.Error != nil {
			return result.Error
		} else if result.RowsAffected == 0 {
			return config.ErrNotFound
		}
		return nil
	})
}

func (r *configRepository) SetDefaultPersona(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		id = strings.TrimSpace(id)
		var item personaRow
		if err := tx.Where("id = ?", id).First(&item).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return config.ErrNotFound
			}
			return err
		}
		if !item.Enabled {
			return fmt.Errorf("%w: 人格 %q 已禁用，不能设为默认人格", config.ErrInvalidRequest, id)
		}
		if err := tx.Model(&personaRow{}).Where("id <> ?", id).Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&personaRow{}).Where("id = ?", id).Update("is_default", true).Error
	})
}

func (r *configRepository) DefaultPersonaID(ctx context.Context) (string, error) {
	var item personaRow
	if err := r.db.WithContext(ctx).Where("is_default = ?", true).Order("id ASC").First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", config.ErrNotFound
		}
		return "", err
	}
	return item.ID, nil
}

func (r *configRepository) BindPersona(ctx context.Context, scope config.BindingScope, targetID, personaID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		scopeValue, targetValue := string(scope), strings.TrimSpace(targetID)
		if strings.TrimSpace(personaID) == "" {
			return tx.Where("scope = ? AND target_id = ?", scopeValue, targetValue).Delete(&personaBindingRow{}).Error
		}
		row := personaBindingRow{Scope: scopeValue, TargetID: targetValue, PersonaID: strings.TrimSpace(personaID), UpdatedAt: time.Now().UTC()}
		return tx.Where("scope = ? AND target_id = ?", row.Scope, row.TargetID).Assign(row).FirstOrCreate(&row).Error
	})
}

func (r *configRepository) GetPersonaBinding(ctx context.Context, scope config.BindingScope, targetID string) (string, error) {
	var row personaBindingRow
	if err := r.db.WithContext(ctx).Where("scope = ? AND target_id = ?", string(scope), strings.TrimSpace(targetID)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return row.PersonaID, nil
}

func personaFromRow(row personaRow) config.Persona {
	return config.Persona{
		ID: row.ID, Name: row.Name, Description: row.Description, Instruction: row.Instruction,
		Revision: row.Revision, IsDefault: row.IsDefault, Enabled: row.Enabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
