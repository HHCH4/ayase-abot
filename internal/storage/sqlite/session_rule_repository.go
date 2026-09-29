package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"Abot/internal/sessionrule"
	"gorm.io/gorm"
)

// sessionRuleRow 保存会话来源规则；列表字段使用 JSON，便于规则字段平滑扩展。
type sessionRuleRow struct {
	Source                      string `gorm:"primaryKey;size:500"`
	ProcessEnabled              bool
	LLMEnabled                  bool
	TTSEnabled                  bool
	Note                        string `gorm:"type:text"`
	ChatModel                   string `gorm:"size:300"`
	STTModel                    string `gorm:"size:300"`
	TTSModel                    string `gorm:"size:300"`
	FollowProfile               bool
	ProfileID                   string `gorm:"size:64"`
	PersonaID                   string `gorm:"size:64"`
	DisabledPluginsJSON         string `gorm:"type:text"`
	KnowledgeBasesJSON          string `gorm:"type:text"`
	KnowledgeTopK               int
	KnowledgeRerank             bool
	PrivateMode                 string `gorm:"size:32"`
	GroupParticipationMode      string `gorm:"size:32"`
	RecordUnaddressedMessages   bool
	ReactionEnabled             bool
	GroupContextEnabled         bool
	ProactiveEnabled            bool
	PrivateHourlyReplyLimit     int
	GroupHourlyReplyLimit       int
	QuietHoursTimezone          string `gorm:"size:64"`
	QuietHoursStart             string `gorm:"size:5"`
	QuietHoursEnd               string `gorm:"size:5"`
	EmergencyBypassQuietHours   bool
	ReplyQuote                  bool
	PrivateReplyQuote           bool
	FollowUpEnabled             bool
	RelationEnabled             bool
	RuntimeEnabled              bool
	RuntimeMaxConcurrency       int
	SourceQueueLimit            int
	TurnWaitMilliseconds        int
	GroupTurnWaitMilliseconds   int
	AttachmentWaitMilliseconds  int
	MaxTurnMessages             int
	GroupMessageMaxCount        int
	GroupImageCaption           bool
	GroupImageCaptionModel      string `gorm:"size:300"`
	ProactiveDegree             string `gorm:"size:16"`
	CooldownSeconds             int
	HeartbeatSeconds            int
	RelationRetentionSeconds    int
	FollowUpMax                 int
	FollowUpMaxRetries          int
	FollowUpRetryDelaySeconds   int
	FollowUpMaxDelaySeconds     int
	FollowUpAllowedSourcesJSON  string `gorm:"type:text"`
	ExpressionEnabled           bool
	ExpressionMaxSegments       int
	ExpressionLongThreshold     int
	ExpressionDelayMilliseconds int
	ReplyMention                bool
	AgentOnDemandEnabled        bool
	AllowedReadOnlyToolsJSON    string `gorm:"type:text"`
	ToolBudget                  int
	SubAgentEnabled             bool
	ActionPermissionsJSON       string `gorm:"type:text"`
	MessageStyle                string `gorm:"size:16"`
	ConfiguredFieldsJSON        string `gorm:"type:text"`
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

func (sessionRuleRow) TableName() string { return "abot_session_rules" }

// sessionRuleGroupRow 保存分组和成员来源；成员是规则来源标识而不是复制规则。
type sessionRuleGroupRow struct {
	ID          string `gorm:"primaryKey;size:100"`
	Name        string `gorm:"size:200;uniqueIndex;not null"`
	Description string `gorm:"type:text"`
	MembersJSON string `gorm:"type:text"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (sessionRuleGroupRow) TableName() string { return "abot_session_rule_groups" }

type sessionRuleRepository struct{ db *gorm.DB }

func (r *sessionRuleRepository) List(ctx context.Context, query string) ([]sessionrule.Rule, error) {
	db := r.db.WithContext(ctx)
	query = strings.TrimSpace(query)
	if query != "" {
		like := "%" + query + "%"
		db = db.Where("source LIKE ? OR note LIKE ?", like, like)
	}
	var rows []sessionRuleRow
	if err := db.Order("source ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]sessionrule.Rule, 0, len(rows))
	for _, row := range rows {
		item, err := ruleFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *sessionRuleRepository) Get(ctx context.Context, source string) (sessionrule.Rule, error) {
	var row sessionRuleRow
	if err := r.db.WithContext(ctx).Where("source = ?", strings.TrimSpace(source)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return sessionrule.Rule{}, sessionrule.ErrNotFound
		}
		return sessionrule.Rule{}, err
	}
	return ruleFromRow(row)
}

func (r *sessionRuleRepository) Save(ctx context.Context, item sessionrule.Rule) error {
	disabled, err := json.Marshal(item.DisabledPlugins)
	if err != nil {
		return err
	}
	knowledge, err := json.Marshal(item.KnowledgeBases)
	if err != nil {
		return err
	}
	configuredFields, err := json.Marshal(item.ConfiguredFields)
	if err != nil {
		return err
	}
	followUpSources, err := json.Marshal(item.FollowUpAllowedSources)
	if err != nil {
		return err
	}
	readOnlyTools, err := json.Marshal(item.AllowedReadOnlyTools)
	if err != nil {
		return err
	}
	actionPermissions, err := json.Marshal(item.ActionPermissions)
	if err != nil {
		return err
	}
	row := sessionRuleRow{
		Source: item.Source, ProcessEnabled: item.ProcessEnabled, LLMEnabled: item.LLMEnabled, TTSEnabled: item.TTSEnabled,
		Note: item.Note, ChatModel: item.ChatModel, STTModel: item.STTModel, TTSModel: item.TTSModel,
		FollowProfile: item.FollowProfile, ProfileID: item.ProfileID, PersonaID: item.PersonaID,
		DisabledPluginsJSON: string(disabled), KnowledgeBasesJSON: string(knowledge), KnowledgeTopK: item.KnowledgeTopK,
		KnowledgeRerank: item.KnowledgeRerank, PrivateMode: item.PrivateMode, GroupParticipationMode: item.GroupParticipationMode,
		RecordUnaddressedMessages: item.RecordUnaddressedMessages, ReactionEnabled: item.ReactionEnabled, GroupContextEnabled: item.GroupContextEnabled,
		ProactiveEnabled: item.ProactiveEnabled, PrivateHourlyReplyLimit: item.PrivateHourlyReplyLimit, GroupHourlyReplyLimit: item.GroupHourlyReplyLimit,
		QuietHoursTimezone: item.QuietHoursTimezone, QuietHoursStart: item.QuietHoursStart, QuietHoursEnd: item.QuietHoursEnd,
		EmergencyBypassQuietHours: item.EmergencyBypassQuietHours, ReplyQuote: item.ReplyQuote, PrivateReplyQuote: item.PrivateReplyQuote,
		FollowUpEnabled: item.FollowUpEnabled, RelationEnabled: item.RelationEnabled,
		RuntimeEnabled: item.RuntimeEnabled, RuntimeMaxConcurrency: item.RuntimeMaxConcurrency, SourceQueueLimit: item.SourceQueueLimit,
		TurnWaitMilliseconds: item.TurnWaitMilliseconds, GroupTurnWaitMilliseconds: item.GroupTurnWaitMilliseconds,
		AttachmentWaitMilliseconds: item.AttachmentWaitMilliseconds, MaxTurnMessages: item.MaxTurnMessages,
		GroupMessageMaxCount: item.GroupMessageMaxCount, GroupImageCaption: item.GroupImageCaption, GroupImageCaptionModel: item.GroupImageCaptionModel,
		ProactiveDegree: item.ProactiveDegree, CooldownSeconds: item.CooldownSeconds, HeartbeatSeconds: item.HeartbeatSeconds,
		RelationRetentionSeconds: item.RelationRetentionSeconds, FollowUpMax: item.FollowUpMax, FollowUpMaxRetries: item.FollowUpMaxRetries,
		FollowUpRetryDelaySeconds: item.FollowUpRetryDelaySeconds, FollowUpMaxDelaySeconds: item.FollowUpMaxDelaySeconds,
		FollowUpAllowedSourcesJSON: string(followUpSources), ExpressionEnabled: item.ExpressionEnabled,
		ExpressionMaxSegments: item.ExpressionMaxSegments, ExpressionLongThreshold: item.ExpressionLongThreshold,
		ExpressionDelayMilliseconds: item.ExpressionDelayMilliseconds, ReplyMention: item.ReplyMention,
		AgentOnDemandEnabled: item.AgentOnDemandEnabled, AllowedReadOnlyToolsJSON: string(readOnlyTools),
		ToolBudget: item.ToolBudget, SubAgentEnabled: item.SubAgentEnabled, ActionPermissionsJSON: string(actionPermissions),
		MessageStyle:         item.MessageStyle,
		ConfiguredFieldsJSON: string(configuredFields), CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

func (r *sessionRuleRepository) Delete(ctx context.Context, source string) error {
	result := r.db.WithContext(ctx).Where("source = ?", strings.TrimSpace(source)).Delete(&sessionRuleRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return sessionrule.ErrNotFound
	}
	return nil
}

func (r *sessionRuleRepository) ListGroups(ctx context.Context) ([]sessionrule.Group, error) {
	var rows []sessionRuleGroupRow
	if err := r.db.WithContext(ctx).Order("name ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]sessionrule.Group, 0, len(rows))
	for _, row := range rows {
		item, err := groupFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (r *sessionRuleRepository) GetGroup(ctx context.Context, id string) (sessionrule.Group, error) {
	var row sessionRuleGroupRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return sessionrule.Group{}, sessionrule.ErrNotFound
		}
		return sessionrule.Group{}, err
	}
	return groupFromRow(row)
}

func (r *sessionRuleRepository) SaveGroup(ctx context.Context, item sessionrule.Group) error {
	members, err := json.Marshal(item.Members)
	if err != nil {
		return err
	}
	row := sessionRuleGroupRow{ID: item.ID, Name: item.Name, Description: item.Description, MembersJSON: string(members), CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	return r.db.WithContext(ctx).Save(&row).Error
}

func (r *sessionRuleRepository) DeleteGroup(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).Delete(&sessionRuleGroupRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return sessionrule.ErrNotFound
	}
	return nil
}

func ruleFromRow(row sessionRuleRow) (sessionrule.Rule, error) {
	var disabled, knowledge, configuredFields []string
	var followUpSources, readOnlyTools []string
	var actionPermissions map[string]bool
	if strings.TrimSpace(row.DisabledPluginsJSON) != "" {
		if err := json.Unmarshal([]byte(row.DisabledPluginsJSON), &disabled); err != nil {
			return sessionrule.Rule{}, err
		}
	}
	if strings.TrimSpace(row.KnowledgeBasesJSON) != "" {
		if err := json.Unmarshal([]byte(row.KnowledgeBasesJSON), &knowledge); err != nil {
			return sessionrule.Rule{}, err
		}
	}
	if strings.TrimSpace(row.ConfiguredFieldsJSON) != "" {
		if err := json.Unmarshal([]byte(row.ConfiguredFieldsJSON), &configuredFields); err != nil {
			return sessionrule.Rule{}, err
		}
	}
	if strings.TrimSpace(row.FollowUpAllowedSourcesJSON) != "" {
		if err := json.Unmarshal([]byte(row.FollowUpAllowedSourcesJSON), &followUpSources); err != nil {
			return sessionrule.Rule{}, err
		}
	}
	if strings.TrimSpace(row.AllowedReadOnlyToolsJSON) != "" {
		if err := json.Unmarshal([]byte(row.AllowedReadOnlyToolsJSON), &readOnlyTools); err != nil {
			return sessionrule.Rule{}, err
		}
	}
	if strings.TrimSpace(row.ActionPermissionsJSON) != "" {
		if err := json.Unmarshal([]byte(row.ActionPermissionsJSON), &actionPermissions); err != nil {
			return sessionrule.Rule{}, err
		}
	}
	return sessionrule.Rule{
		Source: row.Source, ProcessEnabled: row.ProcessEnabled, LLMEnabled: row.LLMEnabled, TTSEnabled: row.TTSEnabled,
		Note: row.Note, ChatModel: row.ChatModel, STTModel: row.STTModel, TTSModel: row.TTSModel,
		FollowProfile: row.FollowProfile, ProfileID: row.ProfileID, PersonaID: row.PersonaID,
		DisabledPlugins: disabled, KnowledgeBases: knowledge, KnowledgeTopK: row.KnowledgeTopK,
		KnowledgeRerank: row.KnowledgeRerank, PrivateMode: row.PrivateMode, GroupParticipationMode: row.GroupParticipationMode,
		RecordUnaddressedMessages: row.RecordUnaddressedMessages, ReactionEnabled: row.ReactionEnabled, GroupContextEnabled: row.GroupContextEnabled,
		ProactiveEnabled: row.ProactiveEnabled, PrivateHourlyReplyLimit: row.PrivateHourlyReplyLimit, GroupHourlyReplyLimit: row.GroupHourlyReplyLimit,
		QuietHoursTimezone: row.QuietHoursTimezone, QuietHoursStart: row.QuietHoursStart, QuietHoursEnd: row.QuietHoursEnd,
		EmergencyBypassQuietHours: row.EmergencyBypassQuietHours, ReplyQuote: row.ReplyQuote, PrivateReplyQuote: row.PrivateReplyQuote,
		FollowUpEnabled: row.FollowUpEnabled, RelationEnabled: row.RelationEnabled,
		RuntimeEnabled: row.RuntimeEnabled, RuntimeMaxConcurrency: row.RuntimeMaxConcurrency, SourceQueueLimit: row.SourceQueueLimit,
		TurnWaitMilliseconds: row.TurnWaitMilliseconds, GroupTurnWaitMilliseconds: row.GroupTurnWaitMilliseconds,
		AttachmentWaitMilliseconds: row.AttachmentWaitMilliseconds, MaxTurnMessages: row.MaxTurnMessages,
		GroupMessageMaxCount: row.GroupMessageMaxCount, GroupImageCaption: row.GroupImageCaption, GroupImageCaptionModel: row.GroupImageCaptionModel,
		ProactiveDegree: row.ProactiveDegree, CooldownSeconds: row.CooldownSeconds, HeartbeatSeconds: row.HeartbeatSeconds,
		RelationRetentionSeconds: row.RelationRetentionSeconds, FollowUpMax: row.FollowUpMax, FollowUpMaxRetries: row.FollowUpMaxRetries,
		FollowUpRetryDelaySeconds: row.FollowUpRetryDelaySeconds, FollowUpMaxDelaySeconds: row.FollowUpMaxDelaySeconds,
		FollowUpAllowedSources: followUpSources, ExpressionEnabled: row.ExpressionEnabled,
		ExpressionMaxSegments: row.ExpressionMaxSegments, ExpressionLongThreshold: row.ExpressionLongThreshold,
		ExpressionDelayMilliseconds: row.ExpressionDelayMilliseconds, ReplyMention: row.ReplyMention,
		AgentOnDemandEnabled: row.AgentOnDemandEnabled, AllowedReadOnlyTools: readOnlyTools,
		ToolBudget: row.ToolBudget, SubAgentEnabled: row.SubAgentEnabled, ActionPermissions: actionPermissions, MessageStyle: row.MessageStyle,
		ConfiguredFields: configuredFields, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func groupFromRow(row sessionRuleGroupRow) (sessionrule.Group, error) {
	var members []string
	if strings.TrimSpace(row.MembersJSON) != "" {
		if err := json.Unmarshal([]byte(row.MembersJSON), &members); err != nil {
			return sessionrule.Group{}, err
		}
	}
	return sessionrule.Group{ID: row.ID, Name: row.Name, Description: row.Description, Members: members, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}
