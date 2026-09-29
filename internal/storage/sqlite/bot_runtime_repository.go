package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
	"Abot/internal/bot"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type botRuntimeInboxRow struct {
	ID             string    `gorm:"primaryKey;size:320"`
	BotID          string    `gorm:"index:idx_bot_runtime_inbox_bot_status_created,priority:1;size:120;not null"`
	Source         string    `gorm:"index;size:320;not null"`
	UserID         string    `gorm:"size:160"`
	ConversationID string    `gorm:"index;size:220"`
	EventType      string    `gorm:"size:64"`
	Payload        []byte    `gorm:"not null"`
	Status         string    `gorm:"index:idx_bot_runtime_inbox_bot_status_created,priority:2;size:24;not null"`
	QueueLimit     int       `gorm:"not null;default:0"`
	CreatedAt      time.Time `gorm:"index:idx_bot_runtime_inbox_bot_status_created,priority:3"`
	UpdatedAt      time.Time
}

func (botRuntimeInboxRow) TableName() string { return "abot_bot_runtime_inbox" }

type botRuntimeContextRow struct {
	ID        string    `gorm:"primaryKey;size:320"`
	BotID     string    `gorm:"index;size:120;not null"`
	Source    string    `gorm:"index:idx_bot_runtime_context_source_created,priority:1;size:320;not null"`
	UserID    string    `gorm:"size:160"`
	Text      string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:"index:idx_bot_runtime_context_source_created,priority:2"`
}

func (botRuntimeContextRow) TableName() string { return "abot_bot_runtime_context" }

type botRuntimeSourceRow struct {
	BotID                 string    `gorm:"primaryKey;size:120"`
	Source                string    `gorm:"primaryKey;size:320"`
	ChatType              string    `gorm:"size:40"`
	Mode                  string    `gorm:"size:40"`
	CurrentTopic          string    `gorm:"size:300"`
	TopicParticipants     string    `gorm:"type:text"`
	LastUserID            string    `gorm:"size:160"`
	LastSeenAt            time.Time `gorm:"index"`
	LastTurnAt            *time.Time
	LastUserTurnAt        *time.Time
	LastBotActionAt       *time.Time
	RecentActivityScore   float64
	SilenceUntil          *time.Time
	ReplyBudget           int
	ReplyBudgetWindowAt   *time.Time
	CurrentConversationID string `gorm:"index;size:220"`
	QueuedTurnCount       int
	Revision              int64 `gorm:"not null"`
}

func (botRuntimeSourceRow) TableName() string { return "abot_bot_runtime_sources" }

type botRuntimeDecisionRow struct {
	ID              string `gorm:"primaryKey;size:360"`
	BotID           string `gorm:"index:idx_bot_runtime_decision_bot_created,priority:1;size:120;not null"`
	Source          string `gorm:"index;size:320;not null"`
	EventID         string `gorm:"size:220"`
	ConversationID  string `gorm:"index;size:220"`
	TurnID          string `gorm:"index;size:220"`
	Action          string `gorm:"size:40;not null"`
	Reason          string `gorm:"size:200;not null"`
	ReasonCodes     string `gorm:"type:text"`
	Confidence      float64
	RequiresAgent   bool
	AllowedTools    string    `gorm:"type:text"`
	ResponseUrgency string    `gorm:"size:32"`
	FollowUpPolicy  string    `gorm:"size:64"`
	SafetyPolicy    string    `gorm:"size:64"`
	CreatedAt       time.Time `gorm:"index:idx_bot_runtime_decision_bot_created,priority:2"`
}

func (botRuntimeDecisionRow) TableName() string { return "abot_bot_runtime_decisions" }

// botPlatformEventRow 只保存平台事件的可审计摘要，不把附件二进制和完整原始
// payload 复制进 SQLite；RawRef 用于回到平台日志或对象存储定位原文。
type botPlatformEventRow struct {
	ID         string    `gorm:"primaryKey;size:360"`
	BotID      string    `gorm:"index:idx_bot_platform_event_bot_created,priority:1;size:120;not null"`
	Platform   string    `gorm:"size:40;not null"`
	Source     string    `gorm:"index;size:320"`
	ChatType   string    `gorm:"size:40"`
	ChatID     string    `gorm:"size:200"`
	UserID     string    `gorm:"size:160"`
	EventType  string    `gorm:"index;size:64;not null"`
	MessageID  string    `gorm:"size:220"`
	Text       string    `gorm:"type:text"`
	RawRef     string    `gorm:"size:200"`
	Payload    string    `gorm:"type:text"`
	OccurredAt time.Time `gorm:"index"`
	ReceivedAt time.Time `gorm:"index:idx_bot_platform_event_bot_created,priority:2"`
}

func (botPlatformEventRow) TableName() string { return "abot_bot_platform_events" }

type botRuntimeTurnRow struct {
	ID             string    `gorm:"primaryKey;size:220"`
	BotID          string    `gorm:"index;size:120;not null"`
	Source         string    `gorm:"index:idx_bot_runtime_turn_source_created,priority:1;size:320;not null"`
	UserID         string    `gorm:"size:160"`
	ConversationID string    `gorm:"index;size:220"`
	Text           string    `gorm:"type:text"`
	Status         string    `gorm:"size:32;not null"`
	CreatedAt      time.Time `gorm:"index:idx_bot_runtime_turn_source_created,priority:2"`
	UpdatedAt      time.Time
}

func (botRuntimeTurnRow) TableName() string { return "abot_bot_runtime_turns" }

type botRuntimeTurnEventRow struct {
	TurnID   string `gorm:"primaryKey;size:220"`
	EventID  string `gorm:"primaryKey;size:320"`
	Sequence int    `gorm:"not null"`
}

func (botRuntimeTurnEventRow) TableName() string { return "abot_bot_runtime_turn_events" }

type botRelationRow struct {
	BotID             string    `gorm:"primaryKey;size:120"`
	ScopeType         string    `gorm:"primaryKey;size:32"`
	ScopeID           string    `gorm:"primaryKey;size:320"`
	PreferredName     string    `gorm:"size:160"`
	StablePreferences string    `gorm:"type:text"`
	InteractionStyle  string    `gorm:"size:200"`
	TrustLevel        string    `gorm:"size:32"`
	RecentTopics      string    `gorm:"type:text"`
	Commitments       string    `gorm:"type:text"`
	LastInteractionAt time.Time `gorm:"index"`
	Revision          int64     `gorm:"not null"`
}

func (botRelationRow) TableName() string { return "abot_bot_relations" }

type botActionPlanRow struct {
	ID             string    `gorm:"primaryKey;size:220"`
	BotID          string    `gorm:"index:idx_bot_action_plan_bot_created,priority:1;size:120;not null"`
	Source         string    `gorm:"index;size:320"`
	TurnID         string    `gorm:"index;size:220"`
	ConversationID string    `gorm:"index;size:220"`
	Status         string    `gorm:"size:32;not null"`
	CreatedAt      time.Time `gorm:"index:idx_bot_action_plan_bot_created,priority:2"`
	// UpdatedAt 用于恢复和审计查询；旧数据库启动时由 AutoMigrate 自动补齐此列。
	UpdatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

func (botActionPlanRow) TableName() string { return "abot_bot_action_plans" }

type botActionRow struct {
	ID             string     `gorm:"primaryKey;size:220"`
	PlanID         string     `gorm:"index;size:220;not null"`
	Type           string     `gorm:"size:40;not null"`
	Sequence       int        `gorm:"not null"`
	IdempotencyKey string     `gorm:"uniqueIndex;size:320;not null"`
	Status         string     `gorm:"index;size:32;not null"`
	Payload        string     `gorm:"type:text"`
	ReplyTarget    string     `gorm:"size:220"`
	NotBefore      *time.Time `gorm:"index"`
	PayloadSummary string     `gorm:"type:text"`
	PlatformResult string     `gorm:"type:text"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (botActionRow) TableName() string { return "abot_bot_actions" }

type botInstanceStateRow struct {
	BotID               string `gorm:"primaryKey;size:120"`
	LifecycleState      string `gorm:"size:32;not null"`
	Availability        string `gorm:"size:32;not null"`
	CurrentLoad         int
	ActiveSources       int
	LastHeartbeatAt     time.Time
	LastPlatformEventAt time.Time
	LastActionAt        time.Time
	ConfigRevision      int64
	PersonaRevision     int64
	Revision            int64 `gorm:"not null"`
}

func (botInstanceStateRow) TableName() string { return "abot_bot_instance_states" }

type botAdminRouteRow struct {
	BotID     string `gorm:"primaryKey;size:120"`
	UserID    string `gorm:"primaryKey;size:160"`
	Platform  string `gorm:"size:40;not null"`
	ChatID    string `gorm:"size:200;not null"`
	UpdatedAt time.Time
}

func (botAdminRouteRow) TableName() string { return "abot_bot_admin_routes" }

type botApprovalDeliveryRow struct {
	ApprovalID string `gorm:"primaryKey;size:220"`
	AdminID    string `gorm:"primaryKey;size:160"`
	SentAt     time.Time
}

func (botApprovalDeliveryRow) TableName() string { return "abot_bot_approval_deliveries" }

type botApprovalMessageBindingRow struct {
	ID         string    `gorm:"primaryKey;size:220"`
	BotID      string    `gorm:"index:idx_bot_approval_message_ref_v2,priority:1;size:120;not null"`
	ApprovalID string    `gorm:"index;size:220;not null"`
	AdminID    string    `gorm:"size:160;not null"`
	Platform   string    `gorm:"index:idx_bot_approval_message_ref_v2,priority:2;size:40;not null"`
	ChatID     string    `gorm:"index:idx_bot_approval_message_ref_v2,priority:3;size:200;not null"`
	MessageID  string    `gorm:"index:idx_bot_approval_message_ref_v2,priority:4;size:220;not null"`
	CreatedAt  time.Time `gorm:"index"`
}

func (botApprovalMessageBindingRow) TableName() string { return "abot_bot_approval_message_bindings" }

type botDeleteConfirmationRow struct {
	ID             string    `gorm:"primaryKey;size:220"`
	BotID          string    `gorm:"index;size:120;not null"`
	AdminUserID    string    `gorm:"index;size:160;not null"`
	ConversationID string    `gorm:"index;size:220;not null"`
	Revision       int64     `gorm:"not null"`
	Code           string    `gorm:"size:32;not null"`
	ExpiresAt      time.Time `gorm:"index;not null"`
	CreatedAt      time.Time
}

func (botDeleteConfirmationRow) TableName() string { return "abot_bot_delete_confirmations" }

// subAgentGroupRow/subAgentRunRow 保存的是有界元数据和结果摘要，图片二进制、
// prompt 原文以及 API 凭据都不会进入 SQLite；实际输入仍由父 Invocation 的
// Artifact 引用和当前 Context 负责传递。
type subAgentGroupRow struct {
	ID                 string `gorm:"primaryKey;size:220"`
	RootInvocationID   string `gorm:"index;size:220"`
	ParentInvocationID string `gorm:"index;size:220"`
	ConversationID     string `gorm:"index;size:220"`
	ParentNodeID       string `gorm:"size:220"`
	Profile            string `gorm:"size:64;not null"`
	Purpose            string `gorm:"size:300"`
	Status             string `gorm:"index;size:32;not null"`
	FailurePolicy      string `gorm:"size:32;not null"`
	ExpectedCount      int
	QueuedCount        int
	RunningCount       int
	CompletedCount     int
	FailedCount        int
	CancelledCount     int
	MinimumSuccesses   int
	MaxConcurrency     int
	BudgetJSON         string    `gorm:"type:text"`
	CapabilitiesJSON   string    `gorm:"type:text"`
	SourceScopeJSON    string    `gorm:"type:text"`
	QueryDigest        string    `gorm:"size:100"`
	ParentPlanID       string    `gorm:"size:220"`
	UsageJSON          string    `gorm:"type:text"`
	ResultText         string    `gorm:"type:text"`
	ResultDigest       string    `gorm:"size:100"`
	ErrorCode          string    `gorm:"size:80"`
	ErrorMessage       string    `gorm:"type:text"`
	CreatedAt          time.Time `gorm:"index"`
	StartedAt          time.Time
	FinishedAt         time.Time
	UpdatedAt          time.Time `gorm:"index"`
	Revision           int64     `gorm:"not null"`
}

func (subAgentGroupRow) TableName() string { return "abot_subagent_groups" }

type subAgentRunRow struct {
	ID                     string `gorm:"primaryKey;size:220"`
	GroupID                string `gorm:"index;size:220;not null"`
	RootInvocationID       string `gorm:"index;size:220"`
	ParentNodeID           string `gorm:"size:220"`
	Ordinal                int
	Stage                  string `gorm:"size:80"`
	DependsOnRunIDsJSON    string `gorm:"type:text"`
	Profile                string `gorm:"size:64;not null"`
	Status                 string `gorm:"index;size:32;not null"`
	Attempt                int
	IdempotencyKey         string `gorm:"uniqueIndex;size:320;not null"`
	ProviderID             string `gorm:"size:120"`
	ModelID                string `gorm:"size:220"`
	CapabilitiesJSON       string `gorm:"type:text"`
	InputArtifactRefsJSON  string `gorm:"type:text"`
	InputMetadataJSON      string `gorm:"type:text"`
	InputDigest            string `gorm:"size:100"`
	ResultText             string `gorm:"type:text"`
	ResultArtifactRefsJSON string `gorm:"type:text"`
	ResultMetadataJSON     string `gorm:"type:text"`
	ResultDigest           string `gorm:"size:100"`
	ErrorCode              string `gorm:"size:80"`
	ErrorMessage           string `gorm:"type:text"`
	ErrorRetryable         bool
	LeaseOwner             string `gorm:"size:160"`
	LeaseExpiresAt         time.Time
	CreatedAt              time.Time `gorm:"index"`
	QueuedAt               time.Time
	StartedAt              time.Time
	FinishedAt             time.Time
	UpdatedAt              time.Time `gorm:"index"`
	Revision               int64     `gorm:"not null"`
}

func (subAgentRunRow) TableName() string { return "abot_subagent_runs" }

type botRuntimeRepository struct{ db *gorm.DB }

var _ bot.RuntimeStateRepository = (*botRuntimeRepository)(nil)
var _ bot.RuntimeInboxActivator = (*botRuntimeRepository)(nil)
var _ bot.RuntimeInboxReader = (*botRuntimeRepository)(nil)
var _ bot.RuntimeInboxReceivedReader = (*botRuntimeRepository)(nil)
var _ bot.RuntimeMaintenanceRepository = (*botRuntimeRepository)(nil)
var _ bot.RuntimeProactiveQuotaRepository = (*botRuntimeRepository)(nil)
var _ bot.RuntimeSourceCounter = (*botRuntimeRepository)(nil)
var _ bot.RuntimeRelationManager = (*botRuntimeRepository)(nil)
var _ agent.SubAgentRuntimeStore = (*botRuntimeRepository)(nil)

// CountActiveSources 只统计最近活跃的来源，避免历史来源数量冒充当前负载。
func (r *botRuntimeRepository) CountActiveSources(ctx context.Context, botID string, since time.Time) (int, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&botRuntimeSourceRow{}).Where("bot_id = ? AND last_seen_at >= ?", strings.TrimSpace(botID), since.UTC()).Count(&count).Error
	return int(count), err
}

// CreateSubAgentGroup 持久化 Group 请求事实，幂等由上层调用方的唯一 ID 保证。
func (r *botRuntimeRepository) CreateSubAgentGroup(ctx context.Context, group agent.SubAgentGroup) error {
	row, err := subAgentGroupRowFromDomain(group)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(&row).Error
}

// UpdateSubAgentGroup 使用 revision 条件更新结果屏障，避免重复 worker 覆盖较新的终态。
func (r *botRuntimeRepository) UpdateSubAgentGroup(ctx context.Context, group agent.SubAgentGroup, expectedRevision int64) error {
	row, err := subAgentGroupRowFromDomain(group)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current subAgentGroupRow
		if err := tx.Where("id = ?", row.ID).First(&current).Error; err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return fmt.Errorf("子 Agent Group revision 冲突: expected=%d actual=%d", expectedRevision, current.Revision)
		}
		return tx.Save(&row).Error
	})
}

func (r *botRuntimeRepository) CreateSubAgentRun(ctx context.Context, run agent.SubAgentRun) error {
	row, err := subAgentRunRowFromDomain(run)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(&row).Error
}

// UpdateSubAgentRun 使用 revision 条件写入运行状态、结果和错误摘要。
func (r *botRuntimeRepository) UpdateSubAgentRun(ctx context.Context, run agent.SubAgentRun, expectedRevision int64) error {
	row, err := subAgentRunRowFromDomain(run)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current subAgentRunRow
		if err := tx.Where("id = ?", row.ID).First(&current).Error; err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return fmt.Errorf("子 Agent Run revision 冲突: expected=%d actual=%d", expectedRevision, current.Revision)
		}
		return tx.Save(&row).Error
	})
}

func (r *botRuntimeRepository) GetSubAgentGroup(ctx context.Context, id string) (agent.SubAgentGroup, error) {
	var row subAgentGroupRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		return agent.SubAgentGroup{}, err
	}
	return subAgentGroupRowToDomain(row)
}

// ListSubAgentGroups 按根 Invocation 返回有界的 Group 列表，供 Runtime 管理视图
// 展示任务树；查询只返回元数据和有界结果摘要，不读取图片或 prompt 原文。
func (r *botRuntimeRepository) ListSubAgentGroups(ctx context.Context, invocationID string, limit int) ([]agent.SubAgentGroup, error) {
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	var rows []subAgentGroupRow
	if err := r.db.WithContext(ctx).Where("root_invocation_id = ? OR parent_invocation_id = ?", strings.TrimSpace(invocationID), strings.TrimSpace(invocationID)).Order("created_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agent.SubAgentGroup, 0, len(rows))
	for _, row := range rows {
		item, err := subAgentGroupRowToDomain(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

// ListSubAgentGroupsByStatuses 返回启动恢复需要检查的活动 Group。查询只允许
// 固定状态集合，避免把已完成历史重新当成未知执行处理。
func (r *botRuntimeRepository) ListSubAgentGroupsByStatuses(ctx context.Context, statuses []agent.SubAgentStatus, limit int) ([]agent.SubAgentGroup, error) {
	if len(statuses) == 0 {
		return []agent.SubAgentGroup{}, nil
	}
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	values := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if strings.TrimSpace(string(status)) != "" {
			values = append(values, string(status))
		}
	}
	if len(values) == 0 {
		return []agent.SubAgentGroup{}, nil
	}
	var rows []subAgentGroupRow
	if err := r.db.WithContext(ctx).Where("status IN ?", values).Order("created_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agent.SubAgentGroup, 0, len(rows))
	for _, row := range rows {
		item, err := subAgentGroupRowToDomain(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *botRuntimeRepository) GetSubAgentRun(ctx context.Context, id string) (agent.SubAgentRun, error) {
	var row subAgentRunRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		return agent.SubAgentRun{}, err
	}
	return subAgentRunRowToDomain(row)
}

func (r *botRuntimeRepository) ListSubAgentRuns(ctx context.Context, groupID string, limit int) ([]agent.SubAgentRun, error) {
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	var rows []subAgentRunRow
	if err := r.db.WithContext(ctx).Where("group_id = ?", strings.TrimSpace(groupID)).Order("ordinal ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agent.SubAgentRun, 0, len(rows))
	for _, row := range rows {
		item, err := subAgentRunRowToDomain(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *botRuntimeRepository) DeleteSubAgentStateByInvocation(ctx context.Context, invocationID string) error {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var groups []subAgentGroupRow
		if err := tx.Where("root_invocation_id = ? OR parent_invocation_id = ?", invocationID, invocationID).Find(&groups).Error; err != nil {
			return err
		}
		ids := make([]string, 0, len(groups))
		for _, group := range groups {
			ids = append(ids, group.ID)
		}
		if len(ids) > 0 {
			if err := tx.Where("group_id IN ? OR root_invocation_id = ?", ids, invocationID).Delete(&subAgentRunRow{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", ids).Delete(&subAgentGroupRow{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("root_invocation_id = ?", invocationID).Delete(&subAgentRunRow{}).Error
	})
}

func (r *botRuntimeRepository) DeleteSubAgentStateByConversation(ctx context.Context, conversationID string) error {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var groups []subAgentGroupRow
		if err := tx.Where("conversation_id = ?", conversationID).Find(&groups).Error; err != nil {
			return err
		}
		ids := make([]string, 0, len(groups))
		for _, group := range groups {
			ids = append(ids, group.ID)
		}
		if len(ids) > 0 {
			if err := tx.Where("group_id IN ?", ids).Delete(&subAgentRunRow{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", ids).Delete(&subAgentGroupRow{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteTerminalSubAgentStateBefore 按批次回收已经结束的子任务，保留活动
// Group/Run 供父任务恢复和管理台排障使用。删除在同一事务中完成，避免留下孤儿 Run。
func (r *botRuntimeRepository) DeleteTerminalSubAgentStateBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 1024 {
		limit = 256
	}
	terminalStatuses := []string{string(agent.SubAgentCompleted), string(agent.SubAgentPartialFailed), string(agent.SubAgentFailed), string(agent.SubAgentCancelled), string(agent.SubAgentExpired)}
	var deleted int
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var groups []subAgentGroupRow
		if err := tx.Where("status IN ? AND finished_at > ? AND finished_at <= ?", terminalStatuses, time.Time{}, before.UTC()).Order("finished_at ASC").Limit(limit).Find(&groups).Error; err != nil {
			return err
		}
		if len(groups) == 0 {
			return nil
		}
		ids := make([]string, 0, len(groups))
		for _, group := range groups {
			ids = append(ids, group.ID)
		}
		if err := tx.Where("group_id IN ?", ids).Delete(&subAgentRunRow{}).Error; err != nil {
			return err
		}
		if err := tx.Where("id IN ?", ids).Delete(&subAgentGroupRow{}).Error; err != nil {
			return err
		}
		deleted = len(ids)
		return nil
	})
	return deleted, err
}

func subAgentGroupRowFromDomain(value agent.SubAgentGroup) (subAgentGroupRow, error) {
	budget, err := json.Marshal(value.Budget)
	if err != nil {
		return subAgentGroupRow{}, err
	}
	capabilities, err := json.Marshal(value.Capabilities)
	if err != nil {
		return subAgentGroupRow{}, err
	}
	sourceScope, err := json.Marshal(value.SourceScope)
	if err != nil {
		return subAgentGroupRow{}, err
	}
	usage, err := json.Marshal(value.Usage)
	if err != nil {
		return subAgentGroupRow{}, err
	}
	return subAgentGroupRow{
		ID: value.ID, RootInvocationID: value.RootInvocationID, ParentInvocationID: value.ParentInvocationID,
		ConversationID: value.ConversationID, ParentNodeID: value.ParentNodeID, Profile: string(value.Profile), Purpose: value.Purpose,
		Status: string(value.Status), FailurePolicy: string(value.FailurePolicy), ExpectedCount: value.ExpectedCount,
		QueuedCount: value.QueuedCount, RunningCount: value.RunningCount, CompletedCount: value.CompletedCount,
		FailedCount: value.FailedCount, CancelledCount: value.CancelledCount, MinimumSuccesses: value.MinimumSuccesses,
		MaxConcurrency: value.MaxConcurrency, BudgetJSON: string(budget), CapabilitiesJSON: string(capabilities), SourceScopeJSON: string(sourceScope),
		QueryDigest: value.QueryDigest, ParentPlanID: value.ParentPlanID, UsageJSON: string(usage), ResultText: value.ResultText,
		ResultDigest: value.ResultDigest, ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage, CreatedAt: value.CreatedAt,
		StartedAt: value.StartedAt, FinishedAt: value.FinishedAt, UpdatedAt: value.UpdatedAt, Revision: value.Revision,
	}, nil
}

func subAgentGroupRowToDomain(row subAgentGroupRow) (agent.SubAgentGroup, error) {
	var value agent.SubAgentGroup
	if err := json.Unmarshal([]byte(row.BudgetJSON), &value.Budget); err != nil {
		return agent.SubAgentGroup{}, err
	}
	if err := json.Unmarshal([]byte(row.CapabilitiesJSON), &value.Capabilities); err != nil {
		return agent.SubAgentGroup{}, err
	}
	if err := json.Unmarshal([]byte(row.SourceScopeJSON), &value.SourceScope); err != nil {
		return agent.SubAgentGroup{}, err
	}
	if err := json.Unmarshal([]byte(row.UsageJSON), &value.Usage); err != nil {
		return agent.SubAgentGroup{}, err
	}
	value.ID, value.RootInvocationID, value.ParentInvocationID, value.ConversationID = row.ID, row.RootInvocationID, row.ParentInvocationID, row.ConversationID
	value.ParentNodeID, value.Profile, value.Purpose = row.ParentNodeID, agent.SubAgentProfile(row.Profile), row.Purpose
	value.Status, value.FailurePolicy = agent.SubAgentStatus(row.Status), agent.SubAgentFailurePolicy(row.FailurePolicy)
	value.ExpectedCount, value.QueuedCount, value.RunningCount, value.CompletedCount = row.ExpectedCount, row.QueuedCount, row.RunningCount, row.CompletedCount
	value.FailedCount, value.CancelledCount, value.MinimumSuccesses, value.MaxConcurrency = row.FailedCount, row.CancelledCount, row.MinimumSuccesses, row.MaxConcurrency
	value.QueryDigest, value.ParentPlanID, value.ResultText, value.ResultDigest = row.QueryDigest, row.ParentPlanID, row.ResultText, row.ResultDigest
	value.ErrorCode, value.ErrorMessage, value.CreatedAt, value.StartedAt = row.ErrorCode, row.ErrorMessage, row.CreatedAt, row.StartedAt
	value.FinishedAt, value.UpdatedAt, value.Revision = row.FinishedAt, row.UpdatedAt, row.Revision
	return value, nil
}

func subAgentRunRowFromDomain(value agent.SubAgentRun) (subAgentRunRow, error) {
	depends, err := json.Marshal(value.DependsOnRunIDs)
	if err != nil {
		return subAgentRunRow{}, err
	}
	capabilities, err := json.Marshal(value.Capabilities)
	if err != nil {
		return subAgentRunRow{}, err
	}
	inputMetadata, err := json.Marshal(value.InputMetadata)
	if err != nil {
		return subAgentRunRow{}, err
	}
	inputArtifactRefs, err := json.Marshal(value.InputArtifactRefs)
	if err != nil {
		return subAgentRunRow{}, err
	}
	resultArtifactRefs, err := json.Marshal(value.ResultArtifactRefs)
	if err != nil {
		return subAgentRunRow{}, err
	}
	resultMetadata, err := json.Marshal(value.ResultMetadata)
	if err != nil {
		return subAgentRunRow{}, err
	}
	return subAgentRunRow{
		ID: value.ID, GroupID: value.GroupID, RootInvocationID: value.RootInvocationID, ParentNodeID: value.ParentNodeID,
		Ordinal: value.Ordinal, Stage: value.Stage, DependsOnRunIDsJSON: string(depends), Profile: string(value.Profile), Status: string(value.Status),
		Attempt: value.Attempt, IdempotencyKey: value.IdempotencyKey, ProviderID: value.ProviderID, ModelID: value.ModelID,
		CapabilitiesJSON: string(capabilities), InputArtifactRefsJSON: string(inputArtifactRefs), InputMetadataJSON: string(inputMetadata), InputDigest: value.InputDigest,
		ResultText: value.ResultText, ResultArtifactRefsJSON: string(resultArtifactRefs), ResultMetadataJSON: string(resultMetadata), ResultDigest: value.ResultDigest, ErrorCode: value.ErrorCode,
		ErrorMessage: value.ErrorMessage, ErrorRetryable: value.ErrorRetryable, LeaseOwner: value.LeaseOwner, LeaseExpiresAt: value.LeaseExpiresAt,
		CreatedAt: value.CreatedAt, QueuedAt: value.QueuedAt, StartedAt: value.StartedAt, FinishedAt: value.FinishedAt, UpdatedAt: value.UpdatedAt, Revision: value.Revision,
	}, nil
}

func subAgentRunRowToDomain(row subAgentRunRow) (agent.SubAgentRun, error) {
	var value agent.SubAgentRun
	if err := json.Unmarshal([]byte(row.DependsOnRunIDsJSON), &value.DependsOnRunIDs); err != nil {
		return agent.SubAgentRun{}, err
	}
	if err := json.Unmarshal([]byte(row.CapabilitiesJSON), &value.Capabilities); err != nil {
		return agent.SubAgentRun{}, err
	}
	if err := json.Unmarshal([]byte(row.InputMetadataJSON), &value.InputMetadata); err != nil {
		return agent.SubAgentRun{}, err
	}
	if strings.TrimSpace(row.InputArtifactRefsJSON) != "" {
		if err := json.Unmarshal([]byte(row.InputArtifactRefsJSON), &value.InputArtifactRefs); err != nil {
			return agent.SubAgentRun{}, err
		}
	}
	if strings.TrimSpace(row.ResultArtifactRefsJSON) != "" {
		if err := json.Unmarshal([]byte(row.ResultArtifactRefsJSON), &value.ResultArtifactRefs); err != nil {
			return agent.SubAgentRun{}, err
		}
	}
	if err := json.Unmarshal([]byte(row.ResultMetadataJSON), &value.ResultMetadata); err != nil {
		return agent.SubAgentRun{}, err
	}
	value.ID, value.GroupID, value.RootInvocationID, value.ParentNodeID = row.ID, row.GroupID, row.RootInvocationID, row.ParentNodeID
	value.Ordinal, value.Stage, value.Profile, value.Status = row.Ordinal, row.Stage, agent.SubAgentProfile(row.Profile), agent.SubAgentStatus(row.Status)
	value.Attempt, value.IdempotencyKey, value.ProviderID, value.ModelID = row.Attempt, row.IdempotencyKey, row.ProviderID, row.ModelID
	value.InputDigest, value.ResultText, value.ResultDigest = row.InputDigest, row.ResultText, row.ResultDigest
	value.ErrorCode, value.ErrorMessage, value.ErrorRetryable, value.LeaseOwner, value.LeaseExpiresAt = row.ErrorCode, row.ErrorMessage, row.ErrorRetryable, row.LeaseOwner, row.LeaseExpiresAt
	value.CreatedAt, value.QueuedAt, value.StartedAt, value.FinishedAt, value.UpdatedAt, value.Revision = row.CreatedAt, row.QueuedAt, row.StartedAt, row.FinishedAt, row.UpdatedAt, row.Revision
	return value, nil
}

// RecordPlatformEvent 保存归一化事件，幂等键由适配器事件 ID 或 Runtime 生成。
func (r *botRuntimeRepository) RecordPlatformEvent(ctx context.Context, event bot.PlatformEvent) error {
	_, err := r.RecordPlatformEventIfNew(ctx, event)
	return err
}

// RecordPlatformEventIfNew 以数据库唯一键作为最终去重边界；只有首次写入的
// 平台事件才允许 Runtime 继续向下游发布，避免网络重试产生重复回复。
func (r *botRuntimeRepository) RecordPlatformEventIfNew(ctx context.Context, event bot.PlatformEvent) (bool, error) {
	if strings.TrimSpace(event.ID) == "" {
		return false, errors.New("平台事件 ID 不能为空")
	}
	row, err := platformEventRowFromDomain(event)
	if err != nil {
		return false, err
	}
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	return result.RowsAffected > 0, result.Error
}

func platformEventRowFromDomain(event bot.PlatformEvent) (botPlatformEventRow, error) {
	payload, err := json.Marshal(map[string]any{
		"mentioned_user_ids": event.MentionedUserIDs,
		"mentions_bot":       event.MentionsBot,
		"capabilities":       event.NativeCapabilities,
		"attachment_count":   len(event.Attachments),
	})
	if err != nil {
		return botPlatformEventRow{}, err
	}
	row := botPlatformEventRow{
		ID: event.ID, BotID: event.BotID, Platform: string(event.Platform), Source: event.SourceUMO,
		ChatType: event.ChatType, ChatID: event.ChatID, UserID: event.UserID, EventType: string(event.EventType),
		MessageID: event.MessageID, Text: trimRuntimeEventText(event.Text), RawRef: event.RawRef, Payload: string(payload),
		OccurredAt: event.OccurredAt, ReceivedAt: event.ReceivedAt,
	}
	if row.ReceivedAt.IsZero() {
		row.ReceivedAt = time.Now().UTC()
	}
	if row.OccurredAt.IsZero() {
		row.OccurredAt = row.ReceivedAt
	}
	return row, nil
}

func trimRuntimeEventText(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 4000 {
		runes = runes[:4000]
	}
	return string(runes)
}

func (r *botRuntimeRepository) SaveInbox(ctx context.Context, event bot.InboxEvent) (bool, error) {
	status := strings.TrimSpace(event.Status)
	if status == "" {
		status = "pending"
	}
	row := botRuntimeInboxRow{ID: event.ID, BotID: event.BotID, Source: event.Source, UserID: event.UserID, ConversationID: event.ConversationID, EventType: string(event.EventType), Payload: event.Payload, Status: status, QueueLimit: event.QueueLimit, CreatedAt: event.CreatedAt, UpdatedAt: event.UpdatedAt}
	created := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先读是为了保留已有事实的状态和队列语义；真正写入再使用
		// 数据库唯一键兜底，避免两个平台重投协程同时通过 Count 检查。
		var existing botRuntimeInboxRow
		lookupErr := tx.Where("id = ?", event.ID).First(&existing).Error
		if lookupErr == nil {
			return nil
		}
		if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return lookupErr
		}
		if event.QueueLimit > 0 {
			var pending int64
			if err := tx.Model(&botRuntimeInboxRow{}).Where("bot_id = ? AND source = ? AND status = ?", event.BotID, event.Source, "pending").Count(&pending).Error; err != nil {
				return err
			}
			if pending >= int64(event.QueueLimit) {
				return bot.ErrSourceQueueFull
			}
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		created = result.RowsAffected > 0
		return nil
	})
	return created, err
}

// GetInbox 返回单条消息事实，供平台重复投递和附件失败恢复复用持久化载荷。
func (r *botRuntimeRepository) GetInbox(ctx context.Context, id string) (bot.InboxEvent, error) {
	var row botRuntimeInboxRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		return bot.InboxEvent{}, err
	}
	return bot.InboxEvent{
		ID: row.ID, BotID: row.BotID, Source: row.Source, UserID: row.UserID,
		ConversationID: row.ConversationID, EventType: bot.PlatformEventType(row.EventType),
		Payload: row.Payload, Status: row.Status, QueueLimit: row.QueueLimit,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// UpdateInboxPayload 在消息处理阶段把 Artifact 引用和最终会话 ID回写到已先落库的
// Inbox，避免重启恢复时再次读取已经失效的平台临时路径。
func (r *botRuntimeRepository) UpdateInboxPayload(ctx context.Context, event bot.InboxEvent) error {
	if strings.TrimSpace(event.ID) == "" {
		return errors.New("Bot Runtime Inbox ID 不能为空")
	}
	status := strings.TrimSpace(event.Status)
	if status != "received" && status != "pending" {
		status = "pending"
	}
	updates := map[string]any{
		"source":          event.Source,
		"user_id":         event.UserID,
		"conversation_id": event.ConversationID,
		"payload":         event.Payload,
		"status":          status,
		"queue_limit":     event.QueueLimit,
		"updated_at":      time.Now().UTC(),
	}
	return r.db.WithContext(ctx).Model(&botRuntimeInboxRow{}).Where("id = ? AND status IN ?", event.ID, []string{"received", "pending"}).Updates(updates).Error
}

// ActivateInbox 将已接收事实原子地转入 pending。队列上限只在这一刻检查，
// 所以观察消息即使很多，也不会在准入判断完成前阻塞同一来源的真正任务。
func (r *botRuntimeRepository) ActivateInbox(ctx context.Context, event bot.InboxEvent) error {
	if strings.TrimSpace(event.ID) == "" {
		return errors.New("Bot Runtime Inbox ID 不能为空")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row botRuntimeInboxRow
		if err := tx.Where("id = ?", event.ID).First(&row).Error; err != nil {
			return err
		}
		if row.Status == "processed" {
			return nil
		}
		queueLimit := row.QueueLimit
		if event.QueueLimit > 0 {
			queueLimit = event.QueueLimit
		}
		if row.Status == "received" && queueLimit > 0 {
			var pending int64
			if err := tx.Model(&botRuntimeInboxRow{}).
				Where("bot_id = ? AND source = ? AND status = ? AND id <> ?", event.BotID, event.Source, "pending", event.ID).
				Count(&pending).Error; err != nil {
				return err
			}
			if pending >= int64(queueLimit) {
				return bot.ErrSourceQueueFull
			}
		}
		updates := map[string]any{
			"source":          event.Source,
			"user_id":         event.UserID,
			"conversation_id": event.ConversationID,
			"payload":         event.Payload,
			"status":          "pending",
			"queue_limit":     queueLimit,
			"updated_at":      time.Now().UTC(),
		}
		return tx.Model(&botRuntimeInboxRow{}).Where("id = ? AND status IN ?", event.ID, []string{"received", "pending"}).Updates(updates).Error
	})
}

func (r *botRuntimeRepository) ListPendingInbox(ctx context.Context, botID string) ([]bot.InboxEvent, error) {
	query := r.db.WithContext(ctx).Where("status IN ?", []string{"received", "pending"})
	if strings.TrimSpace(botID) != "" {
		query = query.Where("bot_id = ?", strings.TrimSpace(botID))
	}
	var rows []botRuntimeInboxRow
	if err := query.Order("created_at ASC").Limit(1000).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]bot.InboxEvent, 0, len(rows))
	for _, row := range rows {
		items = append(items, bot.InboxEvent{ID: row.ID, BotID: row.BotID, Source: row.Source, UserID: row.UserID, ConversationID: row.ConversationID, EventType: bot.PlatformEventType(row.EventType), Payload: row.Payload, Status: row.Status, QueueLimit: row.QueueLimit, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
	}
	return items, nil
}

// ListReceivedInbox 只读取尚未完成来源准入的事实。它与 ListPendingInbox 分开，
// 让心跳可以低频重试队列满或准入窗口崩溃留下的 received 记录，而不会重复扫描
// 已经进入自然话轮队列的 pending 消息。
func (r *botRuntimeRepository) ListReceivedInbox(ctx context.Context, botID string, limit int) ([]bot.InboxEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Where("status = ?", "received")
	if strings.TrimSpace(botID) != "" {
		query = query.Where("bot_id = ?", strings.TrimSpace(botID))
	}
	var rows []botRuntimeInboxRow
	if err := query.Order("created_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]bot.InboxEvent, 0, len(rows))
	for _, row := range rows {
		items = append(items, bot.InboxEvent{ID: row.ID, BotID: row.BotID, Source: row.Source, UserID: row.UserID, ConversationID: row.ConversationID, EventType: bot.PlatformEventType(row.EventType), Payload: row.Payload, Status: row.Status, QueueLimit: row.QueueLimit, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
	}
	return items, nil
}

func (r *botRuntimeRepository) MarkInboxProcessed(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&botRuntimeInboxRow{}).Where("id IN ?", ids).Updates(map[string]any{"status": "processed", "payload": []byte{}, "updated_at": time.Now().UTC()}).Error
}

// MarkUnfinishedActionsUnknown 只处理进程重启前遗留的 pending/running 动作。
// 由于外部平台调用和 SQLite 不是同一事务，无法证明这些动作尚未成功；保守
// 标记为 unknown 比自动重发更安全，管理员可以从 Runtime 总览继续核对。
func (r *botRuntimeRepository) MarkUnfinishedActionsUnknown(ctx context.Context, botID string, now time.Time) error {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&botActionRow{}).
			Where("plan_id IN (?)", tx.Model(&botActionPlanRow{}).Select("id").Where("bot_id = ?", botID)).
			Where("status IN ?", []string{"pending", "running"}).
			Where("type <> ?", "start_agent").
			Updates(map[string]any{"status": "unknown", "platform_result": "recovery_required", "updated_at": now.UTC()}).Error; err != nil {
			return err
		}
		return tx.Model(&botActionPlanRow{}).
			Where("bot_id = ? AND status = ?", botID, "running").
			Updates(map[string]any{"status": "recovery_required", "updated_at": now.UTC()}).Error
	})
}

// CleanupRuntime 删除已消费且超过保留期的运行时事实；pending Inbox、活动来源、
// 运行中动作和未完成 Follow-up 不在清理范围内，避免心跳误伤可恢复任务。
func (r *botRuntimeRepository) CleanupRuntime(ctx context.Context, botID string, cutoff time.Time) error {
	cutoff = cutoff.UTC()
	botID = strings.TrimSpace(botID)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		botScope := func(query *gorm.DB) *gorm.DB {
			if botID != "" {
				return query.Where("bot_id = ?", botID)
			}
			return query
		}
		if err := botScope(tx.Where("status = ? AND updated_at < ?", "processed", cutoff)).Delete(&botRuntimeInboxRow{}).Error; err != nil {
			return err
		}
		if err := botScope(tx.Where("created_at < ?", cutoff)).Delete(&botRuntimeContextRow{}).Error; err != nil {
			return err
		}
		if err := botScope(tx.Where("created_at < ?", cutoff)).Delete(&botRuntimeDecisionRow{}).Error; err != nil {
			return err
		}
		if err := botScope(tx.Where("received_at < ?", cutoff)).Delete(&botPlatformEventRow{}).Error; err != nil {
			return err
		}
		if err := botScope(tx.Where("created_at < ?", cutoff)).Delete(&botRuntimeTurnRow{}).Error; err != nil {
			return err
		}
		if err := botScope(tx.Where("last_seen_at < ?", cutoff)).Delete(&botRuntimeSourceRow{}).Error; err != nil {
			return err
		}
		if err := botScope(tx.Where("last_interaction_at < ?", cutoff)).Delete(&botRelationRow{}).Error; err != nil {
			return err
		}
		var planIDs []string
		planQuery := botScope(tx.Model(&botActionPlanRow{}).Where("created_at < ? AND status IN ?", cutoff, []string{"completed", "failed", "cancelled"}))
		if err := planQuery.Pluck("id", &planIDs).Error; err != nil {
			return err
		}
		if len(planIDs) > 0 {
			if err := tx.Where("plan_id IN ?", planIDs).Delete(&botActionRow{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", planIDs).Delete(&botActionPlanRow{}).Error; err != nil {
				return err
			}
		}
		// Turn 事件绑定没有独立生命周期，父 Turn 删除后清掉孤儿以控制表体积。
		return tx.Exec("DELETE FROM abot_bot_runtime_turn_events WHERE turn_id NOT IN (SELECT id FROM abot_bot_runtime_turns)").Error
	})
}

func (r *botRuntimeRepository) AppendContext(ctx context.Context, entry bot.ContextEntry, limit int) error {
	if limit <= 0 {
		limit = 300
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := botRuntimeContextRow{ID: entry.ID, BotID: entry.BotID, Source: entry.Source, UserID: entry.UserID, Text: entry.Text, CreatedAt: entry.CreatedAt}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		return tx.Exec("DELETE FROM abot_bot_runtime_context WHERE source = ? AND id NOT IN (SELECT id FROM abot_bot_runtime_context WHERE source = ? ORDER BY created_at DESC LIMIT ?)", entry.Source, entry.Source, limit).Error
	})
}

func (r *botRuntimeRepository) ListContext(ctx context.Context, source string, limit int) ([]bot.ContextEntry, error) {
	var rows []botRuntimeContextRow
	if err := r.db.WithContext(ctx).Where("source = ?", source).Order("created_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]bot.ContextEntry, 0, len(rows))
	for index := len(rows) - 1; index >= 0; index-- {
		row := rows[index]
		items = append(items, bot.ContextEntry{ID: row.ID, BotID: row.BotID, Source: row.Source, UserID: row.UserID, Text: row.Text, CreatedAt: row.CreatedAt})
	}
	return items, nil
}

func (r *botRuntimeRepository) DeleteContext(ctx context.Context, source string) error {
	return r.db.WithContext(ctx).Where("source = ?", strings.TrimSpace(source)).Delete(&botRuntimeContextRow{}).Error
}

func (r *botRuntimeRepository) TouchSource(ctx context.Context, state bot.SourceState) error {
	row := botRuntimeSourceRow{BotID: state.BotID, Source: state.Source, ChatType: state.ChatType, Mode: state.Mode, CurrentTopic: state.CurrentTopic, TopicParticipants: state.TopicParticipants, LastUserID: state.LastUserID, LastSeenAt: state.LastSeenAt, LastTurnAt: state.LastTurnAt, LastUserTurnAt: state.LastUserTurnAt, LastBotActionAt: state.LastBotActionAt, RecentActivityScore: state.RecentActivityScore, SilenceUntil: state.SilenceUntil, ReplyBudget: state.ReplyBudget, ReplyBudgetWindowAt: state.ReplyBudgetWindowAt, CurrentConversationID: state.CurrentConversationID, Revision: 1}
	// TouchSource 同时承载完整状态写入和局部队列刷新；局部刷新不能用空值
	// 覆盖已有聊天类型、话题、用户和时间事实，否则管理台会出现闪烁或回退。
	updates := map[string]any{"revision": gorm.Expr("revision + 1")}
	if state.QueuedTurnCountSet {
		row.QueuedTurnCount = state.QueuedTurnCount
		updates["queued_turn_count"] = state.QueuedTurnCount
	}
	if strings.TrimSpace(state.ChatType) != "" {
		updates["chat_type"] = state.ChatType
	}
	if strings.TrimSpace(state.LastUserID) != "" {
		updates["last_user_id"] = state.LastUserID
	}
	if !state.LastSeenAt.IsZero() {
		updates["last_seen_at"] = state.LastSeenAt
	}
	if strings.TrimSpace(state.Mode) != "" {
		updates["mode"] = state.Mode
	}
	if strings.TrimSpace(state.CurrentTopic) != "" {
		updates["current_topic"] = state.CurrentTopic
	}
	if strings.TrimSpace(state.TopicParticipants) != "" {
		updates["topic_participants"] = state.TopicParticipants
	}
	if state.RecentActivityScore != 0 {
		updates["recent_activity_score"] = state.RecentActivityScore
	}
	if state.ReplyBudget != 0 {
		updates["reply_budget"] = state.ReplyBudget
	}
	if state.ReplyBudgetWindowAt != nil {
		updates["reply_budget_window_at"] = state.ReplyBudgetWindowAt
	}
	if strings.TrimSpace(state.CurrentConversationID) != "" {
		updates["current_conversation_id"] = state.CurrentConversationID
	}
	if state.LastTurnAt != nil {
		updates["last_turn_at"] = state.LastTurnAt
	}
	if state.LastUserTurnAt != nil {
		updates["last_user_turn_at"] = state.LastUserTurnAt
	}
	if state.LastBotActionAt != nil {
		updates["last_bot_action_at"] = state.LastBotActionAt
	}
	if state.SilenceUntil != nil {
		updates["silence_until"] = state.SilenceUntil
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "bot_id"}, {Name: "source"}}, DoUpdates: clause.Assignments(updates)}).Create(&row).Error
}

// ReserveProactiveDelivery 在同一 SQLite 事务中检查并领取来源级主动投递额度。
// ReplyBudget 表示当前一小时窗口已经领取的次数；窗口起点和最近动作时间也
// 一并落库，使重启、多个心跳和多个调度任务不会重复消耗或绕过限制。
func (r *botRuntimeRepository) ReserveProactiveDelivery(ctx context.Context, botID, source, chatType string, limit int, cooldown time.Duration, now time.Time) (time.Time, bool, error) {
	botID = strings.TrimSpace(botID)
	source = strings.TrimSpace(source)
	if botID == "" || source == "" {
		return time.Time{}, false, errors.New("主动投递额度缺少机器人或来源")
	}
	now = now.UTC()
	allowed := false
	reservedAt := now
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row botRuntimeSourceRow
		err := tx.Where("bot_id = ? AND source = ?", botID, source).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			windowStart := now
			row = botRuntimeSourceRow{BotID: botID, Source: source, ChatType: chatType, LastSeenAt: now, ReplyBudgetWindowAt: &windowStart, Revision: 1}
			if createErr := tx.Create(&row).Error; createErr != nil {
				return createErr
			}
			err = nil
		}
		if err != nil {
			return err
		}
		count := row.ReplyBudget
		windowStart := now
		if row.ReplyBudgetWindowAt != nil && now.Sub(row.ReplyBudgetWindowAt.UTC()) < time.Hour {
			windowStart = row.ReplyBudgetWindowAt.UTC()
		} else {
			count = 0
		}
		if cooldown > 0 && row.LastBotActionAt != nil && now.Sub(row.LastBotActionAt.UTC()) < cooldown {
			return nil
		}
		if limit > 0 && count >= limit {
			return nil
		}
		count++
		updates := map[string]any{
			"chat_type":              chatType,
			"last_seen_at":           now,
			"last_bot_action_at":     reservedAt,
			"reply_budget":           count,
			"reply_budget_window_at": windowStart,
			"revision":               gorm.Expr("revision + 1"),
		}
		if err := tx.Model(&botRuntimeSourceRow{}).Where("bot_id = ? AND source = ?", botID, source).Updates(updates).Error; err != nil {
			return err
		}
		allowed = true
		return nil
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return reservedAt, allowed, nil
}

// RollbackProactiveDelivery 只回滚仍然是本次领取者的额度。若并发任务已经在
// 其后成功投递，最近动作时间会变化，此时保留事实，避免把别人的额度撤销。
func (r *botRuntimeRepository) RollbackProactiveDelivery(ctx context.Context, botID, source string, reservedAt time.Time) error {
	botID = strings.TrimSpace(botID)
	source = strings.TrimSpace(source)
	if botID == "" || source == "" || reservedAt.IsZero() {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row botRuntimeSourceRow
		if err := tx.Where("bot_id = ? AND source = ?", botID, source).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if row.LastBotActionAt == nil || !row.LastBotActionAt.UTC().Equal(reservedAt.UTC()) || row.ReplyBudget <= 0 {
			return nil
		}
		updates := map[string]any{
			"reply_budget":       row.ReplyBudget - 1,
			"last_bot_action_at": nil,
			"revision":           gorm.Expr("revision + 1"),
		}
		if row.ReplyBudget <= 1 {
			updates["reply_budget_window_at"] = nil
		}
		return tx.Model(&botRuntimeSourceRow{}).Where("bot_id = ? AND source = ? AND last_bot_action_at = ?", botID, source, row.LastBotActionAt).Updates(updates).Error
	})
}

func (r *botRuntimeRepository) RecordDecision(ctx context.Context, decision bot.BehaviorDecision) error {
	reasonCodes, err := json.Marshal(decision.ReasonCodes)
	if err != nil {
		return err
	}
	allowedTools, err := json.Marshal(decision.AllowedTools)
	if err != nil {
		return err
	}
	row := botRuntimeDecisionRow{ID: decision.ID, BotID: decision.BotID, Source: decision.Source, EventID: decision.EventID, ConversationID: decision.ConversationID, TurnID: decision.TurnID, Action: decision.Action, Reason: decision.Reason, ReasonCodes: string(reasonCodes), Confidence: decision.Confidence, RequiresAgent: decision.RequiresAgent, AllowedTools: string(allowedTools), ResponseUrgency: decision.ResponseUrgency, FollowUpPolicy: decision.FollowUpPolicy, SafetyPolicy: decision.SafetyPolicy, CreatedAt: decision.CreatedAt}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

func (r *botRuntimeRepository) CommitTurn(ctx context.Context, turn bot.BotTurn, eventIDs []string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := botRuntimeTurnRow{ID: turn.ID, BotID: turn.BotID, Source: turn.Source, UserID: turn.UserID, ConversationID: turn.ConversationID, Text: turn.Text, Status: turn.Status, CreatedAt: turn.CreatedAt, UpdatedAt: turn.UpdatedAt}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		for index, eventID := range eventIDs {
			binding := botRuntimeTurnEventRow{TurnID: turn.ID, EventID: eventID, Sequence: index + 1}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&binding).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *botRuntimeRepository) TouchRelation(ctx context.Context, state bot.RelationState) error {
	row := botRelationRow{BotID: state.BotID, ScopeType: state.ScopeType, ScopeID: state.ScopeID, PreferredName: state.PreferredName, StablePreferences: state.StablePreferences, InteractionStyle: state.InteractionStyle, TrustLevel: state.TrustLevel, RecentTopics: state.RecentTopics, Commitments: state.Commitments, LastInteractionAt: state.LastInteractionAt, Revision: 1}
	updates := map[string]any{"last_interaction_at": state.LastInteractionAt, "revision": gorm.Expr("revision + 1")}
	if strings.TrimSpace(state.PreferredName) != "" {
		updates["preferred_name"] = state.PreferredName
	}
	if strings.TrimSpace(state.InteractionStyle) != "" {
		updates["interaction_style"] = state.InteractionStyle
	}
	if strings.TrimSpace(state.StablePreferences) != "" {
		updates["stable_preferences"] = state.StablePreferences
	}
	if strings.TrimSpace(state.TrustLevel) != "" {
		updates["trust_level"] = state.TrustLevel
	}
	if strings.TrimSpace(state.RecentTopics) != "" {
		updates["recent_topics"] = state.RecentTopics
	}
	if strings.TrimSpace(state.Commitments) != "" {
		updates["commitments"] = state.Commitments
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "bot_id"}, {Name: "scope_type"}, {Name: "scope_id"}}, DoUpdates: clause.Assignments(updates)}).Create(&row).Error
}

func (r *botRuntimeRepository) ListRelations(ctx context.Context, botID, chatID, userID string, group bool) ([]bot.RelationState, error) {
	targets := []struct{ scopeType, scopeID string }{{"user", userID}}
	if group {
		targets = append(targets, struct{ scopeType, scopeID string }{"group", chatID}, struct{ scopeType, scopeID string }{"group_member", chatID + ":" + userID})
	}
	items := make([]bot.RelationState, 0, len(targets))
	for _, target := range targets {
		var row botRelationRow
		err := r.db.WithContext(ctx).Where("bot_id = ? AND scope_type = ? AND scope_id = ?", botID, target.scopeType, target.scopeID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		items = append(items, bot.RelationState{BotID: row.BotID, ScopeType: row.ScopeType, ScopeID: row.ScopeID, PreferredName: row.PreferredName, StablePreferences: row.StablePreferences, InteractionStyle: row.InteractionStyle, TrustLevel: row.TrustLevel, RecentTopics: row.RecentTopics, Commitments: row.Commitments, LastInteractionAt: row.LastInteractionAt, Revision: row.Revision})
	}
	return items, nil
}

// ListAllRelations 返回管理视图所需的有界关系目录；排序稳定，避免前端刷新时
// 因为数据库默认顺序变化造成错误编辑目标。
func (r *botRuntimeRepository) ListAllRelations(ctx context.Context, botID string, limit int) ([]bot.RelationState, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []botRelationRow
	if err := r.db.WithContext(ctx).Where("bot_id = ?", strings.TrimSpace(botID)).Order("last_interaction_at DESC, scope_type ASC, scope_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]bot.RelationState, 0, len(rows))
	for _, row := range rows {
		items = append(items, bot.RelationState{
			BotID: row.BotID, ScopeType: row.ScopeType, ScopeID: row.ScopeID,
			PreferredName: row.PreferredName, StablePreferences: row.StablePreferences,
			InteractionStyle: row.InteractionStyle, TrustLevel: row.TrustLevel,
			RecentTopics: row.RecentTopics, Commitments: row.Commitments,
			LastInteractionAt: row.LastInteractionAt, Revision: row.Revision,
		})
	}
	return items, nil
}

// SaveRelation 使用 revision CAS 保存管理修改；新关系使用 expectedRevision=0，
// 已存在关系必须提交读取到的修订号，避免并发编辑静默覆盖。
func (r *botRuntimeRepository) SaveRelation(ctx context.Context, state bot.RelationState, expectedRevision int64) error {
	state.BotID = strings.TrimSpace(state.BotID)
	state.ScopeType = strings.TrimSpace(state.ScopeType)
	state.ScopeID = strings.TrimSpace(state.ScopeID)
	if state.BotID == "" || state.ScopeType == "" || state.ScopeID == "" {
		return errors.New("关系状态标识不能为空")
	}
	if state.LastInteractionAt.IsZero() {
		state.LastInteractionAt = time.Now().UTC()
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row botRelationRow
		err := tx.Where("bot_id = ? AND scope_type = ? AND scope_id = ?", state.BotID, state.ScopeType, state.ScopeID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if expectedRevision != 0 {
				return bot.ErrRelationConflict
			}
			row = botRelationRow{
				BotID: state.BotID, ScopeType: state.ScopeType, ScopeID: state.ScopeID,
				PreferredName: state.PreferredName, StablePreferences: state.StablePreferences,
				InteractionStyle: state.InteractionStyle, TrustLevel: state.TrustLevel,
				RecentTopics: state.RecentTopics, Commitments: state.Commitments,
				LastInteractionAt: state.LastInteractionAt, Revision: 1,
			}
			return tx.Create(&row).Error
		}
		if err != nil {
			return err
		}
		if expectedRevision <= 0 || row.Revision != expectedRevision {
			return bot.ErrRelationConflict
		}
		result := tx.Model(&botRelationRow{}).
			Where("bot_id = ? AND scope_type = ? AND scope_id = ? AND revision = ?", state.BotID, state.ScopeType, state.ScopeID, expectedRevision).
			Updates(map[string]any{
				"preferred_name": state.PreferredName, "stable_preferences": state.StablePreferences,
				"interaction_style": state.InteractionStyle, "trust_level": state.TrustLevel,
				"recent_topics": state.RecentTopics, "commitments": state.Commitments,
				"last_interaction_at": state.LastInteractionAt, "revision": gorm.Expr("revision + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return bot.ErrRelationConflict
		}
		return nil
	})
}

// DeleteRelation 同样使用 revision CAS；删除关系不会删除来源上下文或会话历史。
func (r *botRuntimeRepository) DeleteRelation(ctx context.Context, botID, scopeType, scopeID string, expectedRevision int64) error {
	result := r.db.WithContext(ctx).Where("bot_id = ? AND scope_type = ? AND scope_id = ? AND revision = ?", strings.TrimSpace(botID), strings.TrimSpace(scopeType), strings.TrimSpace(scopeID), expectedRevision).Delete(&botRelationRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return bot.ErrRelationConflict
	}
	return nil
}

func (r *botRuntimeRepository) SaveActionPlan(ctx context.Context, plan bot.BotActionPlan, action bot.BotAction) (bot.BotAction, error) {
	var stored bot.BotAction
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先按稳定幂等键读取，避免同一动作因为网络重试生成第二条计划；
		// SQLite 单写连接和短事务把并发重试收敛到同一个事实记录。
		var existing botActionRow
		lookupErr := tx.Where("idempotency_key = ?", action.IdempotencyKey).First(&existing).Error
		if lookupErr == nil {
			stored = bot.BotAction{ID: existing.ID, PlanID: existing.PlanID, Type: existing.Type, Sequence: existing.Sequence, IdempotencyKey: existing.IdempotencyKey, Status: existing.Status, Payload: existing.Payload, ReplyTarget: existing.ReplyTarget, NotBefore: existing.NotBefore, PayloadSummary: existing.PayloadSummary, PlatformResult: existing.PlatformResult, CreatedAt: existing.CreatedAt, UpdatedAt: existing.UpdatedAt}
			return nil
		}
		if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return lookupErr
		}
		planRow := botActionPlanRow{ID: plan.ID, BotID: plan.BotID, Source: plan.Source, TurnID: plan.TurnID, ConversationID: plan.ConversationID, Status: plan.Status, CreatedAt: plan.CreatedAt, StartedAt: plan.StartedAt, FinishedAt: plan.FinishedAt}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&planRow).Error; err != nil {
			return err
		}
		actionRow := botActionRow{ID: action.ID, PlanID: action.PlanID, Type: action.Type, Sequence: action.Sequence, IdempotencyKey: action.IdempotencyKey, Status: action.Status, Payload: action.Payload, ReplyTarget: action.ReplyTarget, NotBefore: action.NotBefore, PayloadSummary: action.PayloadSummary, PlatformResult: action.PlatformResult, CreatedAt: action.CreatedAt, UpdatedAt: action.UpdatedAt}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&actionRow).Error; err != nil {
			return err
		}
		if err := tx.Where("idempotency_key = ?", action.IdempotencyKey).First(&actionRow).Error; err != nil {
			return err
		}
		stored = bot.BotAction{ID: actionRow.ID, PlanID: actionRow.PlanID, Type: actionRow.Type, Sequence: actionRow.Sequence, IdempotencyKey: actionRow.IdempotencyKey, Status: actionRow.Status, Payload: actionRow.Payload, ReplyTarget: actionRow.ReplyTarget, NotBefore: actionRow.NotBefore, PayloadSummary: actionRow.PayloadSummary, PlatformResult: actionRow.PlatformResult, CreatedAt: actionRow.CreatedAt, UpdatedAt: actionRow.UpdatedAt}
		return nil
	})
	return stored, err
}

// ClaimAction 使用 pending/failed -> running 的条件更新实现动作单领取，
// 避免重复事件在同一时间窗口内重复调用平台；进程崩溃后的 running 状态
// 由启动恢复流程标记为 unknown，不会把不确定的外部副作用自动重放。
func (r *botRuntimeRepository) ClaimAction(ctx context.Context, actionID string, claimedAt time.Time) (bool, error) {
	result := r.db.WithContext(ctx).Model(&botActionRow{}).
		Where("id = ? AND status IN ?", strings.TrimSpace(actionID), []string{"pending", "failed"}).
		Updates(map[string]any{"status": "running", "updated_at": claimedAt.UTC()})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *botRuntimeRepository) FinishAction(ctx context.Context, actionID, status, result string, finishedAt time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var action botActionRow
		if err := tx.Where("id = ?", actionID).First(&action).Error; err != nil {
			return err
		}
		if err := tx.Model(&botActionRow{}).Where("id = ? AND status IN ?", actionID, []string{"pending", "running", "failed"}).Updates(map[string]any{"status": status, "platform_result": result, "updated_at": finishedAt}).Error; err != nil {
			return err
		}
		planStatus := "completed"
		if status == "failed" {
			planStatus = "failed"
		}
		return tx.Model(&botActionPlanRow{}).Where("id = ?", action.PlanID).Updates(map[string]any{"status": planStatus, "finished_at": finishedAt}).Error
	})
}

func (r *botRuntimeRepository) UpdateInstanceState(ctx context.Context, state bot.BotInstanceState) error {
	row := botInstanceStateRow{BotID: state.BotID, LifecycleState: state.LifecycleState, Availability: state.Availability, CurrentLoad: state.CurrentLoad, ActiveSources: state.ActiveSources, LastHeartbeatAt: state.LastHeartbeatAt, LastPlatformEventAt: state.LastPlatformEventAt, LastActionAt: state.LastActionAt, ConfigRevision: state.ConfigRevision, PersonaRevision: state.PersonaRevision, Revision: 1}
	updates := map[string]any{"lifecycle_state": state.LifecycleState, "availability": state.Availability, "current_load": state.CurrentLoad, "active_sources": state.ActiveSources, "revision": gorm.Expr("revision + 1")}
	if state.ConfigRevision != 0 {
		updates["config_revision"] = state.ConfigRevision
	}
	if state.PersonaRevision != 0 {
		updates["persona_revision"] = state.PersonaRevision
	}
	if !state.LastHeartbeatAt.IsZero() {
		updates["last_heartbeat_at"] = state.LastHeartbeatAt
	}
	if !state.LastPlatformEventAt.IsZero() {
		updates["last_platform_event_at"] = state.LastPlatformEventAt
	}
	if !state.LastActionAt.IsZero() {
		updates["last_action_at"] = state.LastActionAt
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "bot_id"}}, DoUpdates: clause.Assignments(updates)}).Create(&row).Error
}

// collectInvocationIDs 扩展父 Invocation 的后代集合。虽然当前通用子 Agent 不再
// 建立独立 Invocation，但旧工作流或恢复数据仍可能存在 parent_invocation_id；
// 删除 Bot/会话时必须把整棵 Runtime 任务树一起清理，不能留下孤儿审批和事件。
func collectInvocationIDs(tx *gorm.DB, query *gorm.DB) ([]string, error) {
	var ids []string
	if err := query.Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	for offset := 0; offset < len(ids); {
		end := len(ids)
		if end > offset+100 {
			end = offset + 100
		}
		var children []string
		if err := tx.Model(&invocationRow{}).Where("parent_invocation_id IN ?", ids[offset:end]).Pluck("id", &children).Error; err != nil {
			return nil, err
		}
		for _, id := range children {
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		offset = end
	}
	return ids, nil
}

// deleteInvocationScopedRows 删除与 Invocation 强关联的 Runtime 审计、恢复、
// 投递和工作区执行记录。Conversation/Artifact 不在这里删除：它们有自己的
// 生命周期，Bot 删除不能误删用户历史，Conversation 删除则由上层 Artifact 钩子负责。
func deleteInvocationScopedRows(tx *gorm.DB, invocationIDs []string) error {
	if len(invocationIDs) == 0 {
		return nil
	}
	// Group/Run 使用 root_invocation_id 关系，而不是重复创建 Invocation；
	// 删除父任务时先删子结果，避免管理台留下无法定位的运行树。
	if err := tx.Exec("DELETE FROM abot_subagent_runs WHERE root_invocation_id IN ? OR group_id IN (SELECT id FROM abot_subagent_groups WHERE root_invocation_id IN ? OR parent_invocation_id IN ?)", invocationIDs, invocationIDs, invocationIDs).Error; err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("清理子 Agent Run 失败: %w", err)
		}
	}
	if err := tx.Exec("DELETE FROM abot_subagent_groups WHERE root_invocation_id IN ? OR parent_invocation_id IN ?", invocationIDs, invocationIDs).Error; err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("清理子 Agent Group 失败: %w", err)
		}
	}
	// 审批通知通过 approval_id 间接关联 Invocation，必须在删除审批前先清理。
	for _, statement := range []string{
		"DELETE FROM abot_bot_approval_deliveries WHERE approval_id IN (SELECT id FROM abot_agent_approvals WHERE invocation_id IN ?)",
		"DELETE FROM abot_bot_approval_message_bindings WHERE approval_id IN (SELECT id FROM abot_agent_approvals WHERE invocation_id IN ?)",
	} {
		if err := tx.Exec(statement, invocationIDs).Error; err != nil {
			return err
		}
	}
	// 命令输出块通过 command_run_id 间接关联 Invocation，先读取有限 ID 再删除，
	// 避免 SQLite 在复杂子查询中重复展开参数造成锁和兼容性问题。
	var commandRunIDs []string
	if err := tx.Table("abot_workspace_command_runs").Where("invocation_id IN ?", invocationIDs).Pluck("id", &commandRunIDs).Error; err != nil {
		return err
	}
	if len(commandRunIDs) > 0 {
		if err := tx.Exec("DELETE FROM abot_workspace_command_output_chunks WHERE command_run_id IN ?", commandRunIDs).Error; err != nil {
			return err
		}
	}
	for _, table := range []string{
		"abot_agent_invocation_resumes",
		"abot_agent_invocation_resume_outbox",
		"abot_agent_worktree_baselines",
		"abot_agent_events",
		"abot_agent_event_outbox",
		"abot_agent_event_delivery_inbox",
		"abot_agent_checkpoint_delivery_inbox",
		"abot_agent_checkpoint_delivery_outbox",
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
		"abot_agent_config_delivery_inbox",
		"abot_agent_config_delivery_outbox",
		"abot_agent_config_delivery_transactions",
		"abot_agent_approval_rejection_delivery_outbox",
		"abot_agent_approval_rejection_delivery_inbox",
		"abot_agent_approval_rejection_delivery_transactions",
		"abot_agent_event_delivery_transactions",
		"abot_agent_checkpoint_delivery_transactions",
		"abot_runtime_config_directory_rebind_plans",
		"abot_runtime_config_directory_rebind_confirmations",
		"abot_runtime_config_directory_rebind_applies",
		"abot_runtime_config_directory_rebind_multi_confirmations",
		"abot_runtime_config_directory_rebind_multi_applies",
		"abot_runtime_config_directory_rebind_inbox",
		"abot_agent_runtime_delivery_attempts",
		"abot_agent_runtime_delivery_groups",
		"abot_agent_runtime_delivery_group_fences",
		"abot_agent_runtime_delivery_group_settlements",
		"abot_agent_runtime_delivery_group_sagas",
		"abot_agent_runtime_delivery_compensations",
		"abot_workspace_command_runs",
		"abot_workspace_operations",
		"abot_web_search_usage",
	} {
		if !tx.Migrator().HasTable(table) {
			continue
		}
		if err := tx.Exec("DELETE FROM `"+table+"` WHERE invocation_id IN ?", invocationIDs).Error; err != nil {
			return fmt.Errorf("清理 Invocation 关联表 %s 失败: %w", table, err)
		}
	}
	if err := tx.Exec("DELETE FROM abot_agent_invocations WHERE id IN ?", invocationIDs).Error; err != nil {
		return fmt.Errorf("清理 Invocation 主记录失败: %w", err)
	}
	return nil
}

func (r *botRuntimeRepository) DeleteBotState(ctx context.Context, botID string) error {
	botID = strings.TrimSpace(botID)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		invocationIDs, err := collectInvocationIDs(tx, tx.Model(&invocationRow{}).Where("bot_id = ?", botID))
		if err != nil {
			return err
		}
		if err := deleteInvocationScopedRows(tx, invocationIDs); err != nil {
			return err
		}
		var plans []string
		if err := tx.Model(&botActionPlanRow{}).Where("bot_id = ?", botID).Pluck("id", &plans).Error; err != nil {
			return err
		}
		if len(plans) > 0 {
			if err := tx.Where("plan_id IN ?", plans).Delete(&botActionRow{}).Error; err != nil {
				return err
			}
		}
		for _, deletion := range []struct {
			query any
			where string
		}{
			{&botActionPlanRow{}, "bot_id = ?"}, {&botRuntimeDecisionRow{}, "bot_id = ?"}, {&botRuntimeTurnRow{}, "bot_id = ?"},
			{&botRelationRow{}, "bot_id = ?"}, {&botRuntimeSourceRow{}, "bot_id = ?"}, {&botRuntimeContextRow{}, "bot_id = ?"},
			{&botRuntimeInboxRow{}, "bot_id = ?"}, {&botAdminRouteRow{}, "bot_id = ?"}, {&botDeleteConfirmationRow{}, "bot_id = ?"},
			{&botApprovalMessageBindingRow{}, "bot_id = ?"},
			{&botPlatformEventRow{}, "bot_id = ?"},
			{&botInstanceStateRow{}, "bot_id = ?"},
		} {
			if err := tx.Where(deletion.where, botID).Delete(deletion.query).Error; err != nil {
				return err
			}
		}
		// Follow-up 属于 Bot Runtime 的主动任务；删除机器人时必须一并取消并
		// 清理，不能让调度器在机器人配置删除后继续向旧聊天投递。
		if err := tx.Exec("DELETE FROM abot_bot_follow_ups WHERE adapter_id = ?", botID).Error; err != nil {
			return err
		}
		// Turn 绑定和审批投递使用间接标识，清理失去父记录的孤儿。
		if err := tx.Exec("DELETE FROM abot_bot_runtime_turn_events WHERE turn_id NOT IN (SELECT id FROM abot_bot_runtime_turns)").Error; err != nil {
			return err
		}
		return tx.Exec("DELETE FROM abot_bot_approval_deliveries WHERE approval_id NOT IN (SELECT id FROM abot_agent_approvals)").Error
	})
}

// DeleteConversationState 清理会话专属的 Runtime 投影；来源上下文、关系状态和
// 机器人生命周期状态不依赖单个会话，因此刻意保留，避免删除一个会话误伤同一来源的新会话。
func (r *botRuntimeRepository) DeleteConversationState(ctx context.Context, conversationID string) error {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		invocationIDs, err := collectInvocationIDs(tx, tx.Model(&invocationRow{}).Where("conversation_id = ?", conversationID))
		if err != nil {
			return err
		}
		if err := deleteInvocationScopedRows(tx, invocationIDs); err != nil {
			return err
		}
		var turnIDs []string
		if err := tx.Model(&botRuntimeTurnRow{}).Where("conversation_id = ?", conversationID).Pluck("id", &turnIDs).Error; err != nil {
			return err
		}
		var planIDs []string
		if err := tx.Model(&botActionPlanRow{}).Where("conversation_id = ?", conversationID).Pluck("id", &planIDs).Error; err != nil {
			return err
		}
		var inboxIDs []string
		if err := tx.Model(&botRuntimeInboxRow{}).Where("conversation_id = ?", conversationID).Pluck("id", &inboxIDs).Error; err != nil {
			return err
		}
		if len(inboxIDs) > 0 {
			if err := tx.Where("event_id IN ?", inboxIDs).Delete(&botRuntimeDecisionRow{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("conversation_id = ?", conversationID).Delete(&botRuntimeDecisionRow{}).Error; err != nil {
			return err
		}
		if len(planIDs) > 0 {
			if err := tx.Where("plan_id IN ?", planIDs).Delete(&botActionRow{}).Error; err != nil {
				return err
			}
		}
		if len(turnIDs) > 0 {
			if err := tx.Where("turn_id IN ?", turnIDs).Delete(&botRuntimeTurnEventRow{}).Error; err != nil {
				return err
			}
			if err := tx.Where("turn_id IN ?", turnIDs).Delete(&botActionPlanRow{}).Error; err != nil {
				return err
			}
		}
		if len(planIDs) > 0 {
			if err := tx.Where("id IN ?", planIDs).Delete(&botActionPlanRow{}).Error; err != nil {
				return err
			}
		}
		if len(turnIDs) > 0 {
			if err := tx.Where("id IN ?", turnIDs).Delete(&botRuntimeTurnRow{}).Error; err != nil {
				return err
			}
		}
		if len(inboxIDs) > 0 {
			if err := tx.Where("id IN ?", inboxIDs).Delete(&botRuntimeInboxRow{}).Error; err != nil {
				return err
			}
		}
		// 审批绑定通过 Invocation 间接关联会话；会话仓储随后删除 Approval 本身。
		if err := tx.Exec("DELETE FROM abot_bot_approval_deliveries WHERE approval_id IN (SELECT a.id FROM abot_agent_approvals a JOIN abot_agent_invocations i ON i.id = a.invocation_id WHERE i.conversation_id = ?)", conversationID).Error; err != nil {
			return err
		}
		// 会话物理删除必须移除来源于该会话的 Follow-up，避免已删除会话在
		// 到期后重新创建隐含的 Agent 输入。
		if err := tx.Exec("DELETE FROM abot_bot_follow_ups WHERE conversation_id = ?", conversationID).Error; err != nil {
			return err
		}
		return tx.Exec("DELETE FROM abot_bot_approval_message_bindings WHERE approval_id IN (SELECT a.id FROM abot_agent_approvals a JOIN abot_agent_invocations i ON i.id = a.invocation_id WHERE i.conversation_id = ?)", conversationID).Error
	})
}

func (r *botRuntimeRepository) SaveAdminRoute(ctx context.Context, route bot.AdminRoute) error {
	row := botAdminRouteRow{BotID: route.BotID, UserID: route.UserID, Platform: string(route.Platform), ChatID: route.ChatID, UpdatedAt: route.UpdatedAt}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "bot_id"}, {Name: "user_id"}}, DoUpdates: clause.AssignmentColumns([]string{"platform", "chat_id", "updated_at"})}).Create(&row).Error
}

func (r *botRuntimeRepository) ListAdminRoutes(ctx context.Context, botID string) ([]bot.AdminRoute, error) {
	var rows []botAdminRouteRow
	if err := r.db.WithContext(ctx).Where("bot_id = ?", strings.TrimSpace(botID)).Order("updated_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]bot.AdminRoute, 0, len(rows))
	for _, row := range rows {
		items = append(items, bot.AdminRoute{BotID: row.BotID, UserID: row.UserID, Platform: bot.Type(row.Platform), ChatID: row.ChatID, UpdatedAt: row.UpdatedAt})
	}
	return items, nil
}

func (r *botRuntimeRepository) ApprovalDelivered(ctx context.Context, approvalID, adminID string) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&botApprovalDeliveryRow{}).Where("approval_id = ? AND admin_id = ?", approvalID, adminID).Count(&count).Error
	return count > 0, err
}

func (r *botRuntimeRepository) MarkApprovalDelivered(ctx context.Context, approvalID, adminID string, sentAt time.Time) error {
	row := botApprovalDeliveryRow{ApprovalID: approvalID, AdminID: adminID, SentAt: sentAt}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

func (r *botRuntimeRepository) SaveApprovalMessageBinding(ctx context.Context, binding bot.ApprovalMessageBinding) error {
	row := botApprovalMessageBindingRow{ID: binding.ID, BotID: binding.BotID, ApprovalID: binding.ApprovalID, AdminID: binding.AdminID, Platform: string(binding.Platform), ChatID: binding.ChatID, MessageID: binding.MessageID, CreatedAt: binding.CreatedAt}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

func (r *botRuntimeRepository) FindApprovalMessageBinding(ctx context.Context, botID string, platform bot.Type, chatID, messageID string) (bot.ApprovalMessageBinding, error) {
	var row botApprovalMessageBindingRow
	err := r.db.WithContext(ctx).Where("bot_id = ? AND platform = ? AND chat_id = ? AND message_id = ?", strings.TrimSpace(botID), string(platform), strings.TrimSpace(chatID), strings.TrimSpace(messageID)).Order("created_at DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return bot.ApprovalMessageBinding{}, nil
	}
	if err != nil {
		return bot.ApprovalMessageBinding{}, err
	}
	return bot.ApprovalMessageBinding{ID: row.ID, BotID: row.BotID, ApprovalID: row.ApprovalID, AdminID: row.AdminID, Platform: bot.Type(row.Platform), ChatID: row.ChatID, MessageID: row.MessageID, CreatedAt: row.CreatedAt}, nil
}

// GetRuntimeOverview 读取有界的运行状态投影，查询只读且不持有跨网络调用的事务。
func (r *botRuntimeRepository) GetRuntimeOverview(ctx context.Context, botID string, limit int) (bot.RuntimeOverview, error) {
	botID = strings.TrimSpace(botID)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	result := bot.RuntimeOverview{BotID: botID, Sources: []bot.SourceState{}, Decisions: []bot.BehaviorDecision{}, Actions: []bot.BotAction{}, Relations: []bot.RelationState{}}
	var instance botInstanceStateRow
	if err := r.db.WithContext(ctx).Where("bot_id = ?", botID).First(&instance).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return bot.RuntimeOverview{}, err
	}
	result.Instance = bot.BotInstanceState{BotID: instance.BotID, LifecycleState: instance.LifecycleState, Availability: instance.Availability, CurrentLoad: instance.CurrentLoad, ActiveSources: instance.ActiveSources, LastHeartbeatAt: instance.LastHeartbeatAt, LastPlatformEventAt: instance.LastPlatformEventAt, LastActionAt: instance.LastActionAt, ConfigRevision: instance.ConfigRevision, PersonaRevision: instance.PersonaRevision, Revision: instance.Revision}

	var sourceRows []botRuntimeSourceRow
	if err := r.db.WithContext(ctx).Where("bot_id = ?", botID).Order("last_seen_at DESC").Limit(limit).Find(&sourceRows).Error; err != nil {
		return bot.RuntimeOverview{}, err
	}
	queueCounts := make(map[string]int)
	var counts []struct {
		Source string
		Count  int
	}
	if err := r.db.WithContext(ctx).Model(&botRuntimeInboxRow{}).Select("source, COUNT(*) AS count").Where("bot_id = ? AND status = ?", botID, "pending").Group("source").Find(&counts).Error; err != nil {
		return bot.RuntimeOverview{}, err
	}
	for _, item := range counts {
		queueCounts[item.Source] = item.Count
	}
	for _, row := range sourceRows {
		queued := queueCounts[row.Source]
		result.Sources = append(result.Sources, bot.SourceState{BotID: row.BotID, Source: row.Source, ChatType: row.ChatType, Mode: row.Mode, CurrentTopic: row.CurrentTopic, TopicParticipants: row.TopicParticipants, LastUserID: row.LastUserID, LastSeenAt: row.LastSeenAt, LastTurnAt: row.LastTurnAt, LastUserTurnAt: row.LastUserTurnAt, LastBotActionAt: row.LastBotActionAt, RecentActivityScore: row.RecentActivityScore, SilenceUntil: row.SilenceUntil, ReplyBudget: row.ReplyBudget, ReplyBudgetWindowAt: row.ReplyBudgetWindowAt, CurrentConversationID: row.CurrentConversationID, QueuedTurnCount: queued, Revision: row.Revision})
	}
	if active, countErr := r.CountActiveSources(ctx, botID, time.Now().UTC().Add(-24*time.Hour)); countErr == nil {
		result.Instance.ActiveSources = active
	} else {
		result.Instance.ActiveSources = len(result.Sources)
	}

	var decisionRows []botRuntimeDecisionRow
	if err := r.db.WithContext(ctx).Where("bot_id = ?", botID).Order("created_at DESC").Limit(limit).Find(&decisionRows).Error; err != nil {
		return bot.RuntimeOverview{}, err
	}
	for _, row := range decisionRows {
		var reasonCodes []string
		var allowedTools []string
		_ = json.Unmarshal([]byte(row.ReasonCodes), &reasonCodes)
		_ = json.Unmarshal([]byte(row.AllowedTools), &allowedTools)
		result.Decisions = append(result.Decisions, bot.BehaviorDecision{ID: row.ID, BotID: row.BotID, Source: row.Source, EventID: row.EventID, ConversationID: row.ConversationID, TurnID: row.TurnID, Action: row.Action, Reason: row.Reason, ReasonCodes: reasonCodes, Confidence: row.Confidence, RequiresAgent: row.RequiresAgent, AllowedTools: allowedTools, ResponseUrgency: row.ResponseUrgency, FollowUpPolicy: row.FollowUpPolicy, SafetyPolicy: row.SafetyPolicy, CreatedAt: row.CreatedAt})
	}

	var actionRows []botActionRow
	actionQuery := r.db.WithContext(ctx).Where("plan_id IN (?)", r.db.Model(&botActionPlanRow{}).Select("id").Where("bot_id = ?", botID)).Order("updated_at DESC").Limit(limit)
	if err := actionQuery.Find(&actionRows).Error; err != nil {
		return bot.RuntimeOverview{}, err
	}
	for _, row := range actionRows {
		result.Actions = append(result.Actions, bot.BotAction{ID: row.ID, PlanID: row.PlanID, Type: row.Type, Sequence: row.Sequence, IdempotencyKey: row.IdempotencyKey, Status: row.Status, Payload: row.Payload, ReplyTarget: row.ReplyTarget, NotBefore: row.NotBefore, PayloadSummary: row.PayloadSummary, PlatformResult: row.PlatformResult, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
	}

	var relationRows []botRelationRow
	if err := r.db.WithContext(ctx).Where("bot_id = ?", botID).Order("last_interaction_at DESC").Limit(limit).Find(&relationRows).Error; err != nil {
		return bot.RuntimeOverview{}, err
	}
	for _, row := range relationRows {
		result.Relations = append(result.Relations, bot.RelationState{BotID: row.BotID, ScopeType: row.ScopeType, ScopeID: row.ScopeID, PreferredName: row.PreferredName, StablePreferences: row.StablePreferences, InteractionStyle: row.InteractionStyle, TrustLevel: row.TrustLevel, RecentTopics: row.RecentTopics, Commitments: row.Commitments, LastInteractionAt: row.LastInteractionAt, Revision: row.Revision})
	}
	return result, nil
}

func (r *botRuntimeRepository) SaveDeleteConfirmation(ctx context.Context, confirmation bot.DeleteConfirmation) error {
	row := botDeleteConfirmationRow{ID: confirmation.ID, BotID: confirmation.BotID, AdminUserID: confirmation.AdminUserID, ConversationID: confirmation.ConversationID, Revision: confirmation.Revision, Code: confirmation.Code, ExpiresAt: confirmation.ExpiresAt, CreatedAt: confirmation.CreatedAt}
	return r.db.WithContext(ctx).Save(&row).Error
}

func (r *botRuntimeRepository) ConsumeDeleteConfirmation(ctx context.Context, botID, adminID, conversationID, code string, now time.Time) (bot.DeleteConfirmation, error) {
	var result bot.DeleteConfirmation
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row botDeleteConfirmationRow
		if err := tx.Where("bot_id = ? AND admin_user_id = ? AND conversation_id = ? AND code = ? AND expires_at > ?", botID, adminID, conversationID, code, now).Order("created_at DESC").First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return bot.ErrDeleteConfirmationNotFound()
			}
			return err
		}
		result = bot.DeleteConfirmation{ID: row.ID, BotID: row.BotID, AdminUserID: row.AdminUserID, ConversationID: row.ConversationID, Revision: row.Revision, Code: row.Code, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt}
		return tx.Delete(&row).Error
	})
	return result, err
}
