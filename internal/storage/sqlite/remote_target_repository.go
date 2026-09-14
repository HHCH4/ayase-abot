package sqlite

import (
	"context"
	"errors"
	"time"

	"Abot/internal/workspace"
	"gorm.io/gorm"
)

// remoteTargetRow 保存可复用的远程主机连接；秘密字段不由 HTTP 层直接序列化。
type remoteTargetRow struct {
	ID                 string `gorm:"primaryKey;size:64"`
	Name               string `gorm:"size:200;not null"`
	Transport          string `gorm:"size:32;not null"`
	Host               string `gorm:"size:500;not null"`
	Port               int
	User               string `gorm:"size:300;not null"`
	AuthType           string `gorm:"size:32;not null"`
	KeyPath            string `gorm:"size:2000"`
	Password           string `gorm:"size:4000"`
	HostKeyFingerprint string `gorm:"size:300"`
	Enabled            bool
	Status             string `gorm:"size:32;not null"`
	StatusMessage      string `gorm:"size:1000"`
	LastCheckedAt      *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (remoteTargetRow) TableName() string { return "abot_remote_targets" }

type remoteTargetRepository struct {
	db *gorm.DB
}

func (r *remoteTargetRepository) List(ctx context.Context) ([]workspace.RemoteTarget, error) {
	var rows []remoteTargetRow
	if err := r.db.WithContext(ctx).Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]workspace.RemoteTarget, 0, len(rows))
	for _, row := range rows {
		result = append(result, remoteTargetFromRow(row))
	}
	return result, nil
}

func (r *remoteTargetRepository) Get(ctx context.Context, id string) (workspace.RemoteTarget, error) {
	var row remoteTargetRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return workspace.RemoteTarget{}, workspace.ErrRemoteTargetNotFound
		}
		return workspace.RemoteTarget{}, err
	}
	return remoteTargetFromRow(row), nil
}

func (r *remoteTargetRepository) Save(ctx context.Context, item workspace.RemoteTarget) error {
	row := remoteTargetRow{
		ID: item.ID, Name: item.Name, Transport: string(item.Transport), Host: item.Host, Port: item.Port,
		User: item.User, AuthType: string(item.AuthType), KeyPath: item.KeyPath, Password: item.Password,
		HostKeyFingerprint: item.HostKeyFingerprint, Enabled: item.Enabled, Status: string(item.Status),
		StatusMessage: item.StatusMessage, LastCheckedAt: item.LastCheckedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

func (r *remoteTargetRepository) Delete(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Where("id = ?", id).Delete(&remoteTargetRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return workspace.ErrRemoteTargetNotFound
	}
	return nil
}

func remoteTargetFromRow(row remoteTargetRow) workspace.RemoteTarget {
	return workspace.RemoteTarget{
		ID: row.ID, Name: row.Name, Transport: workspace.RemoteTransport(row.Transport), Host: row.Host, Port: row.Port,
		User: row.User, AuthType: workspace.AuthType(row.AuthType), KeyPath: row.KeyPath, Password: row.Password,
		PasswordConfigured: row.Password != "", HostKeyFingerprint: row.HostKeyFingerprint, Enabled: row.Enabled,
		Status: workspace.RemoteTargetStatus(row.Status), StatusMessage: row.StatusMessage, LastCheckedAt: row.LastCheckedAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
