package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/provider"
	"gorm.io/gorm"
)

// providerRow 是数据库模型，API 密钥只保存在此处和运行时 Provider 中，不会进入响应 DTO。
type providerRow struct {
	ID                 string `gorm:"primaryKey;size:64"`
	Name               string `gorm:"size:200;not null"`
	BaseURL            string `gorm:"size:1000"`
	Protocol           string `gorm:"size:64;not null"`
	OpenAIFormat       string `gorm:"size:32"`
	TokenCountProtocol string `gorm:"size:32"`
	APIKey             string `gorm:"size:4000"`
	Status             string `gorm:"size:32;not null"`
	StatusMessage      string `gorm:"size:1000"`
	LastCheckedAt      *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (providerRow) TableName() string { return "abot_providers" }

type modelRow struct {
	ProviderID       string `gorm:"primaryKey;size:64"`
	ModelID          string `gorm:"primaryKey;size:300"`
	DisplayName      string `gorm:"size:300"`
	Enabled          bool
	Source           string `gorm:"size:32"`
	ContextWindow    int
	MaxOutputTokens  int
	CapabilitiesJSON string `gorm:"type:text"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (modelRow) TableName() string { return "abot_provider_models" }

type capabilityObservationRow struct {
	ID         string    `gorm:"primaryKey;size:160"`
	ProviderID string    `gorm:"index:idx_capability_observations_model;size:64;not null"`
	ModelID    string    `gorm:"index:idx_capability_observations_model;size:300;not null"`
	Protocol   string    `gorm:"size:64;not null"`
	Route      string    `gorm:"size:64"`
	Feature    string    `gorm:"index;size:100;not null"`
	State      string    `gorm:"index;size:24;not null"`
	Source     string    `gorm:"size:80;not null"`
	Confidence float64   `gorm:"not null"`
	Reason     string    `gorm:"size:240"`
	ObservedAt time.Time `gorm:"index;not null"`
	CreatedAt  time.Time `gorm:"index;not null"`
}

func (capabilityObservationRow) TableName() string { return "abot_model_capability_observations" }

type settingRow struct {
	Key       string `gorm:"primaryKey;size:100"`
	Value     string `gorm:"size:500"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (settingRow) TableName() string { return "abot_settings" }

type providerRepository struct {
	db *gorm.DB
}

func (r *providerRepository) List(ctx context.Context) ([]provider.Provider, error) {
	var rows []providerRow
	if err := r.db.WithContext(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]provider.Provider, 0, len(rows))
	for _, row := range rows {
		item, err := r.toDomain(ctx, row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *providerRepository) Get(ctx context.Context, id string) (provider.Provider, error) {
	var row providerRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return provider.Provider{}, provider.ErrNotFound
		}
		return provider.Provider{}, err
	}
	return r.toDomain(ctx, row)
}

func (r *providerRepository) Save(ctx context.Context, item provider.Provider) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old providerRow
		err := tx.Where("id = ?", item.ID).First(&old).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		row := fromDomain(item)
		if err == nil {
			row.CreatedAt = old.CreatedAt
		}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		if err := tx.Where("provider_id = ?", item.ID).Delete(&modelRow{}).Error; err != nil {
			return err
		}
		for _, model := range item.Models {
			rowModel := modelRow{
				ProviderID:      item.ID,
				ModelID:         model.ID,
				DisplayName:     model.DisplayName,
				Enabled:         model.Enabled,
				Source:          model.Source,
				ContextWindow:   model.ContextWindow,
				MaxOutputTokens: model.MaxOutputTokens,
				CreatedAt:       model.CreatedAt,
				UpdatedAt:       model.UpdatedAt,
			}
			if model.Capabilities != nil {
				if encoded, marshalErr := json.Marshal(model.Capabilities); marshalErr != nil {
					return marshalErr
				} else {
					rowModel.CapabilitiesJSON = string(encoded)
				}
			}
			if rowModel.Source == "" {
				rowModel.Source = "manual"
			}
			if rowModel.CreatedAt.IsZero() {
				rowModel.CreatedAt = time.Now().UTC()
			}
			if rowModel.UpdatedAt.IsZero() {
				rowModel.UpdatedAt = rowModel.CreatedAt
			}
			if err := tx.Create(&rowModel).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *providerRepository) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("provider_id = ?", id).Delete(&modelRow{}).Error; err != nil {
			return err
		}
		if err := tx.Where("provider_id = ?", id).Delete(&capabilityObservationRow{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ?", id).Delete(&providerRow{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return provider.ErrNotFound
		}
		var current settingRow
		if err := tx.Where("key = ?", "default_provider_id").First(&current).Error; err == nil && current.Value == id {
			if err := tx.Where("key IN ?", []string{"default_provider_id", "default_model_id"}).Delete(&settingRow{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveCapabilityObservations appends metadata-only probe evidence with a
// bounded per-provider/model history. The deterministic observation ID makes
// retries idempotent while the prune step prevents an always-on probe from
// growing the SQLite file without limit.
func (r *providerRepository) SaveCapabilityObservations(ctx context.Context, items []provider.CapabilityObservationRecord) error {
	if len(items) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		groups := make(map[string]struct{})
		for _, item := range items {
			if err := item.Validate(); err != nil {
				return err
			}
			row := capabilityObservationRow{
				ID: item.ID, ProviderID: item.ProviderID, ModelID: item.ModelID, Protocol: string(item.Protocol), Route: item.Route,
				Feature: item.Feature, State: string(item.State), Source: item.Source, Confidence: item.Confidence,
				Reason: item.Reason, ObservedAt: item.ObservedAt, CreatedAt: item.CreatedAt,
			}
			if err := tx.Save(&row).Error; err != nil {
				return err
			}
			groups[item.ProviderID+"\x00"+item.ModelID] = struct{}{}
		}
		for group := range groups {
			parts := strings.SplitN(group, "\x00", 2)
			if len(parts) != 2 {
				continue
			}
			var rows []capabilityObservationRow
			if err := tx.Where("provider_id = ? AND model_id = ?", parts[0], parts[1]).Order("observed_at DESC, created_at DESC, id DESC").Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) <= provider.MaxCapabilityObservationHistory {
				continue
			}
			for _, row := range rows[provider.MaxCapabilityObservationHistory:] {
				if err := tx.Delete(&row).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (r *providerRepository) ListCapabilityObservations(ctx context.Context, providerID, modelID string, limit int) ([]provider.CapabilityObservationRecord, error) {
	limit = provider.NormalizeCapabilityObservationLimit(limit)
	var rows []capabilityObservationRow
	if err := r.db.WithContext(ctx).Where("provider_id = ? AND model_id = ?", strings.TrimSpace(providerID), strings.TrimSpace(modelID)).Order("observed_at DESC, created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]provider.CapabilityObservationRecord, 0, len(rows))
	for _, row := range rows {
		result = append(result, provider.CapabilityObservationRecord{
			ID: row.ID, ProviderID: row.ProviderID, ModelID: row.ModelID, Protocol: provider.Protocol(row.Protocol), Route: row.Route,
			Feature: row.Feature, State: provider.SupportState(row.State), Source: row.Source, Confidence: row.Confidence,
			Reason: row.Reason, ObservedAt: row.ObservedAt, CreatedAt: row.CreatedAt,
		})
	}
	return result, nil
}

func (r *providerRepository) GetDefault(ctx context.Context) (provider.DefaultRef, error) {
	var rows []settingRow
	if err := r.db.WithContext(ctx).Where("key IN ?", []string{"default_provider_id", "default_model_id"}).Find(&rows).Error; err != nil {
		return provider.DefaultRef{}, err
	}
	var result provider.DefaultRef
	for _, row := range rows {
		switch row.Key {
		case "default_provider_id":
			result.ProviderID = row.Value
		case "default_model_id":
			result.ModelID = row.Value
		}
	}
	return result, nil
}

func (r *providerRepository) SetDefault(ctx context.Context, ref provider.DefaultRef) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for key, value := range map[string]string{
			"default_provider_id": ref.ProviderID,
			"default_model_id":    ref.ModelID,
		} {
			row := settingRow{Key: key, Value: value}
			if err := tx.Where("key = ?", key).Assign(settingRow{Value: value, UpdatedAt: time.Now().UTC()}).FirstOrCreate(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *providerRepository) toDomain(ctx context.Context, row providerRow) (provider.Provider, error) {
	var models []modelRow
	if err := r.db.WithContext(ctx).Where("provider_id = ?", row.ID).Order("model_id ASC").Find(&models).Error; err != nil {
		return provider.Provider{}, fmt.Errorf("加载供应商 %q 模型目录失败: %w", row.ID, err)
	}
	result := provider.Provider{
		ID:                 row.ID,
		Name:               row.Name,
		BaseURL:            row.BaseURL,
		Protocol:           provider.Protocol(row.Protocol),
		OpenAIFormat:       provider.OpenAIFormat(row.OpenAIFormat),
		TokenCountProtocol: provider.TokenCountProtocol(row.TokenCountProtocol),
		APIKey:             row.APIKey,
		Status:             provider.Status(row.Status),
		StatusMessage:      row.StatusMessage,
		LastCheckedAt:      row.LastCheckedAt,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
		Models:             make([]provider.Model, 0, len(models)),
	}
	for _, model := range models {
		result.Models = append(result.Models, provider.Model{
			ID:              model.ModelID,
			DisplayName:     model.DisplayName,
			Enabled:         model.Enabled,
			Source:          model.Source,
			ContextWindow:   model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens,
			CreatedAt:       model.CreatedAt,
			UpdatedAt:       model.UpdatedAt,
		})
		if strings.TrimSpace(model.CapabilitiesJSON) != "" {
			var profile provider.ModelCapabilityProfile
			if err := json.Unmarshal([]byte(model.CapabilitiesJSON), &profile); err != nil {
				return provider.Provider{}, fmt.Errorf("加载供应商 %q 模型 %q 能力失败: %w", row.ID, model.ModelID, err)
			}
			result.Models[len(result.Models)-1].Capabilities = &profile
		}
	}
	return result, nil
}

func fromDomain(item provider.Provider) providerRow {
	return providerRow{
		ID:                 item.ID,
		Name:               item.Name,
		BaseURL:            item.BaseURL,
		Protocol:           string(item.Protocol),
		OpenAIFormat:       string(item.OpenAIFormat),
		TokenCountProtocol: string(item.TokenCountProtocol),
		APIKey:             item.APIKey,
		Status:             string(item.Status),
		StatusMessage:      item.StatusMessage,
		LastCheckedAt:      item.LastCheckedAt,
		CreatedAt:          item.CreatedAt,
		UpdatedAt:          item.UpdatedAt,
	}
}
