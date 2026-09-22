package sqlite

import (
	"context"
	"errors"
	"time"

	"Abot/internal/conversation"
	"gorm.io/gorm"
)

// conversationRow 保存对话元数据；删除时整行清除，不设置 deleted_at 软删除字段。
type conversationRow struct {
	ID          string `gorm:"primaryKey;size:100"`
	AppName     string `gorm:"index;size:100;not null"`
	UserID      string `gorm:"index;size:300;not null"`
	Source      string `gorm:"index;size:500"`
	Platform    string `gorm:"index;size:100"`
	ChatType    string `gorm:"index;size:32"`
	ChatID      string `gorm:"index;size:300"`
	WorkspaceID string `gorm:"index;size:64"`
	// Per-conversation model override; empty inherits the resolved default.
	ProviderID string `gorm:"size:100"`
	ModelID    string `gorm:"size:200"`
	Title      string `gorm:"size:500;not null"`
	Status     string `gorm:"index;size:32;not null"`
	ArchivedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (conversationRow) TableName() string { return "abot_conversations" }

type conversationRepository struct {
	db *gorm.DB
}

func (r *conversationRepository) List(ctx context.Context, userID, workspaceID string, includeArchived bool) ([]conversation.Conversation, error) {
	query := r.db.WithContext(ctx).Where("user_id = ?", userID)
	switch workspaceID {
	case "":
		query = query.Where("workspace_id = '' OR workspace_id IS NULL")
	case "*":
		// 不追加工作区过滤，返回用户在所有工作区下的对话。
	default:
		query = query.Where("workspace_id = ?", workspaceID)
	}
	if !includeArchived {
		query = query.Where("status = ?", string(conversation.StatusActive))
	}
	var rows []conversationRow
	if err := query.Order("updated_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]conversation.Conversation, 0, len(rows))
	for _, row := range rows {
		result = append(result, conversationFromRow(row))
	}
	return result, nil
}

// ListAll 返回实例内的全部会话；该方法只被明确的管理查询调用。
func (r *conversationRepository) ListAll(ctx context.Context, workspaceID string, includeArchived bool) ([]conversation.Conversation, error) {
	query := r.db.WithContext(ctx)
	switch workspaceID {
	case "", "*":
		// 空值和星号都表示不限定工作区，便于管理台复用统一查询入口。
	default:
		query = query.Where("workspace_id = ?", workspaceID)
	}
	if !includeArchived {
		query = query.Where("status = ?", string(conversation.StatusActive))
	}
	var rows []conversationRow
	if err := query.Order("updated_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]conversation.Conversation, 0, len(rows))
	for _, row := range rows {
		result = append(result, conversationFromRow(row))
	}
	return result, nil
}

func (r *conversationRepository) Get(ctx context.Context, userID, id string) (conversation.Conversation, error) {
	var row conversationRow
	if err := r.db.WithContext(ctx).Where("user_id = ? AND id = ?", userID, id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return conversation.Conversation{}, conversation.ErrNotFound
		}
		return conversation.Conversation{}, err
	}
	return conversationFromRow(row), nil
}

func (r *conversationRepository) GetByID(ctx context.Context, id string) (conversation.Conversation, error) {
	var row conversationRow
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return conversation.Conversation{}, conversation.ErrNotFound
		}
		return conversation.Conversation{}, err
	}
	return conversationFromRow(row), nil
}

func (r *conversationRepository) CountByWorkspace(ctx context.Context, workspaceID string) (int, error) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&conversationRow{}).Where("workspace_id = ?", workspaceID).Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

// CountAll 给状态总览提供实例级会话统计，不按用户泄露任何会话内容。
func (r *conversationRepository) CountAll(ctx context.Context, includeArchived bool) (int, error) {
	query := r.db.WithContext(ctx).Model(&conversationRow{})
	if !includeArchived {
		query = query.Where("status = ?", string(conversation.StatusActive))
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

func (r *conversationRepository) Save(ctx context.Context, item conversation.Conversation) error {
	row := conversationRow{
		ID: item.ID, AppName: item.AppName, UserID: item.UserID, Source: item.Source,
		Platform: item.Platform, ChatType: item.ChatType, ChatID: item.ChatID, WorkspaceID: item.WorkspaceID,
		ProviderID: item.ProviderID, ModelID: item.ModelID,
		Title: item.Title, Status: string(item.Status), ArchivedAt: item.ArchivedAt,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

func (r *conversationRepository) Delete(ctx context.Context, userID, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 待审批记录、命令输出和 diff 属于对话关联信息，随对话一并物理删除。
		if err := tx.Where("conversation_id = ?", id).Delete(&workspaceOperationRow{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM abot_workspace_command_output_chunks WHERE command_run_id IN (SELECT id FROM abot_workspace_command_runs WHERE conversation_id = ?)", id).Error; err != nil {
			return err
		}
		if err := tx.Where("conversation_id = ?", id).Delete(&workspaceCommandRunRow{}).Error; err != nil {
			return err
		}
		// 从对话显式导入的长期记忆也属于该对话的信息，删除对话时一并物理清理。
		if err := tx.Where("conversation_id = ?", id).Delete(&memoryRow{}).Error; err != nil {
			return err
		}
		// 配置绑定也是对话关联信息；一并清理，避免已删除对话阻止配置文件彻底删除。
		if err := tx.Where("scope = ? AND target_id = ?", "conversation", id).Delete(&configBindingRow{}).Error; err != nil {
			return err
		}
		// Runtime 的所有持久化记录都通过 Invocation 关联对话；先删子表，
		// 再删 Invocation，避免数据与日志页继续显示已删除会话的孤儿记录。
		if err := tx.Exec("DELETE FROM abot_agent_subagent_evidence WHERE group_id IN (SELECT id FROM abot_agent_subagent_groups WHERE invocation_id IN (SELECT id FROM abot_agent_invocations WHERE conversation_id = ?))", id).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM abot_agent_subagent_runs WHERE group_id IN (SELECT id FROM abot_agent_subagent_groups WHERE invocation_id IN (SELECT id FROM abot_agent_invocations WHERE conversation_id = ?))", id).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM abot_agent_subagent_groups WHERE invocation_id IN (SELECT id FROM abot_agent_invocations WHERE conversation_id = ?)", id).Error; err != nil {
			return err
		}
		deleteRuntimeByInvocation := func(table string) error {
			return tx.Exec("DELETE FROM "+table+" WHERE invocation_id IN (SELECT id FROM abot_agent_invocations WHERE conversation_id = ?)", id).Error
		}
		runtimeTables := []string{
			"abot_agent_invocation_resumes",
			"abot_agent_invocation_resume_outbox",
			"abot_agent_worktree_baselines",
			"abot_agent_events",
			"abot_agent_event_outbox",
			"abot_agent_event_delivery_inbox",
			"abot_agent_event_delivery_transactions",
			"abot_agent_checkpoint_delivery_inbox",
			"abot_agent_checkpoint_delivery_outbox",
			"abot_agent_checkpoint_delivery_transactions",
			"abot_agent_approval_rejection_delivery_outbox",
			"abot_agent_approval_rejection_delivery_inbox",
			"abot_agent_approval_rejection_delivery_transactions",
			"abot_agent_config_delivery_inbox",
			"abot_agent_config_delivery_outbox",
			"abot_agent_config_delivery_transactions",
			"abot_runtime_config_directory_rebind_plans",
			"abot_runtime_config_directory_rebind_confirmations",
			"abot_runtime_config_directory_rebind_applies",
			"abot_runtime_config_directory_rebind_multi_confirmations",
			"abot_runtime_config_directory_rebind_multi_applies",
			"abot_runtime_config_directory_rebind_inbox",
			"abot_agent_runtime_delivery_attempts",
			"abot_agent_runtime_delivery_groups",
			"abot_agent_runtime_delivery_group_transactions",
			"abot_agent_runtime_delivery_group_fences",
			"abot_agent_runtime_delivery_group_settlements",
			"abot_agent_runtime_delivery_group_sagas",
			"abot_agent_runtime_delivery_compensations",
			"abot_agent_approvals",
			"abot_agent_tool_calls",
			"abot_agent_task_plans",
			"abot_agent_task_contracts",
			"abot_agent_instruction_snapshots",
			"abot_agent_context_manifests",
			"abot_agent_working_sets",
			"abot_agent_verification_runs",
			"abot_agent_runtime_snapshots",
			"abot_agent_toolset_snapshots",
			"abot_agent_model_capability_snapshots",
			"abot_agent_eval_runs",
		}
		for _, table := range runtimeTables {
			if err := deleteRuntimeByInvocation(table); err != nil {
				return err
			}
		}
		if err := tx.Where("conversation_id = ?", id).Delete(&scheduledTaskRow{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM abot_agent_invocations WHERE user_id = ? AND conversation_id = ?", userID, id).Error; err != nil {
			return err
		}
		result := tx.Where("user_id = ? AND id = ?", userID, id).Delete(&conversationRow{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return conversation.ErrNotFound
		}
		return nil
	})
}

func conversationFromRow(row conversationRow) conversation.Conversation {
	return conversation.Conversation{
		ID: row.ID, AppName: row.AppName, UserID: row.UserID, Source: row.Source,
		Platform: row.Platform, ChatType: row.ChatType, ChatID: row.ChatID, WorkspaceID: row.WorkspaceID,
		ProviderID: row.ProviderID, ModelID: row.ModelID,
		Title: row.Title, Status: conversation.Status(row.Status), ArchivedAt: row.ArchivedAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
