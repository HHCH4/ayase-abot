// Package sessionrule 提供按消息会话来源覆盖运行配置的规则目录。
package sessionrule

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound 表示没有找到指定会话来源规则或分组。
	ErrNotFound = errors.New("会话规则不存在")
	// ErrInvalidRequest 表示规则输入不符合边界约束。
	ErrInvalidRequest = errors.New("会话规则请求无效")
	// ErrConflict 表示会话来源或分组名称重复。
	ErrConflict = errors.New("会话规则已存在")
)

const (
	// 以下键是独立可清除的会话覆盖项；它们对应 WebUI 表单字段和 Runtime
	// 能理解的内置配置，不包含任何外部插件执行器。
	OverrideProcessEnabled              = "process_enabled"
	OverrideLLMEnabled                  = "llm_enabled"
	OverrideTTSEnabled                  = "tts_enabled"
	OverrideNote                        = "note"
	OverrideChatModel                   = "chat_model"
	OverrideSTTModel                    = "stt_model"
	OverrideTTSModel                    = "tts_model"
	OverrideFollowProfile               = "follow_profile"
	OverrideProfileID                   = "profile_id"
	OverridePersonaID                   = "persona_id"
	OverrideDisabledPlugins             = "disabled_plugins"
	OverrideKnowledgeBases              = "knowledge_bases"
	OverrideKnowledgeTopK               = "knowledge_top_k"
	OverrideKnowledgeRerank             = "knowledge_rerank"
	OverridePrivateMode                 = "private_mode"
	OverrideGroupParticipationMode      = "group_participation_mode"
	OverrideRecordUnaddressedMessages   = "record_unaddressed_messages"
	OverrideReactionEnabled             = "reaction_enabled"
	OverrideGroupContextEnabled         = "group_context_enabled"
	OverrideProactiveEnabled            = "proactive_enabled"
	OverridePrivateHourlyReplyLimit     = "private_hourly_reply_limit"
	OverrideGroupHourlyReplyLimit       = "group_hourly_reply_limit"
	OverrideQuietHoursTimezone          = "quiet_hours_timezone"
	OverrideQuietHoursStart             = "quiet_hours_start"
	OverrideQuietHoursEnd               = "quiet_hours_end"
	OverrideEmergencyBypassQuietHours   = "emergency_bypass_quiet_hours"
	OverrideReplyQuote                  = "reply_quote"
	OverridePrivateReplyQuote           = "private_reply_quote"
	OverrideFollowUpEnabled             = "follow_up_enabled"
	OverrideRelationEnabled             = "relation_enabled"
	OverrideRuntimeEnabled              = "runtime_enabled"
	OverrideRuntimeMaxConcurrency       = "runtime_max_concurrency"
	OverrideSourceQueueLimit            = "source_queue_limit"
	OverrideTurnWaitMilliseconds        = "turn_wait_ms"
	OverrideGroupTurnWaitMilliseconds   = "group_turn_wait_ms"
	OverrideAttachmentWaitMilliseconds  = "attachment_wait_ms"
	OverrideMaxTurnMessages             = "max_turn_messages"
	OverrideGroupMessageMaxCount        = "group_message_max_count"
	OverrideGroupImageCaption           = "group_image_caption"
	OverrideGroupImageCaptionModel      = "group_image_caption_model"
	OverrideProactiveDegree             = "proactive_degree"
	OverrideCooldownSeconds             = "cooldown_seconds"
	OverrideHeartbeatSeconds            = "heartbeat_seconds"
	OverrideRelationRetentionSeconds    = "relation_retention_seconds"
	OverrideFollowUpMax                 = "follow_up_max"
	OverrideFollowUpMaxRetries          = "follow_up_max_retries"
	OverrideFollowUpRetryDelaySeconds   = "follow_up_retry_delay_seconds"
	OverrideFollowUpMaxDelaySeconds     = "follow_up_max_delay_seconds"
	OverrideFollowUpAllowedSources      = "follow_up_allowed_sources"
	OverrideExpressionEnabled           = "expression_enabled"
	OverrideExpressionMaxSegments       = "expression_max_segments"
	OverrideExpressionLongThreshold     = "expression_long_threshold"
	OverrideExpressionDelayMilliseconds = "expression_delay_ms"
	OverrideReplyMention                = "reply_mention"
	OverrideAgentOnDemandEnabled        = "agent_on_demand_enabled"
	OverrideAllowedReadOnlyTools        = "allowed_read_only_tools"
	OverrideToolBudget                  = "tool_budget"
	OverrideSubAgentEnabled             = "subagent_enabled"
	OverrideActionPermissions           = "action_permissions"
	OverrideMessageStyle                = "message_style"
)

// Rule 是一个消息会话来源的独立覆盖规则。来源使用 UMO 或 /sid 返回的稳定标识。
type Rule struct {
	Source          string   `json:"source"`
	ProcessEnabled  bool     `json:"process_enabled"`
	LLMEnabled      bool     `json:"llm_enabled"`
	TTSEnabled      bool     `json:"tts_enabled"`
	Note            string   `json:"note,omitempty"`
	ChatModel       string   `json:"chat_model,omitempty"`
	STTModel        string   `json:"stt_model,omitempty"`
	TTSModel        string   `json:"tts_model,omitempty"`
	FollowProfile   bool     `json:"follow_profile"`
	ProfileID       string   `json:"profile_id,omitempty"`
	PersonaID       string   `json:"persona_id,omitempty"`
	DisabledPlugins []string `json:"disabled_plugins,omitempty"`
	KnowledgeBases  []string `json:"knowledge_bases,omitempty"`
	KnowledgeTopK   int      `json:"knowledge_top_k"`
	KnowledgeRerank bool     `json:"knowledge_rerank"`
	// 以下字段是按来源生效的 Bot Runtime 策略。它们与模型/人格同属会话规则，
	// 通过 ConfiguredFields 区分“明确关闭”与“继续继承机器人配置”。
	PrivateMode               string `json:"private_mode,omitempty"`
	GroupParticipationMode    string `json:"group_participation_mode,omitempty"`
	RecordUnaddressedMessages bool   `json:"record_unaddressed_messages"`
	ReactionEnabled           bool   `json:"reaction_enabled"`
	GroupContextEnabled       bool   `json:"group_context_enabled"`
	ProactiveEnabled          bool   `json:"proactive_enabled"`
	PrivateHourlyReplyLimit   int    `json:"private_hourly_reply_limit"`
	GroupHourlyReplyLimit     int    `json:"group_hourly_reply_limit"`
	QuietHoursTimezone        string `json:"quiet_hours_timezone,omitempty"`
	QuietHoursStart           string `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd             string `json:"quiet_hours_end,omitempty"`
	EmergencyBypassQuietHours bool   `json:"emergency_bypass_quiet_hours"`
	ReplyQuote                bool   `json:"reply_quote"`
	PrivateReplyQuote         bool   `json:"private_reply_quote"`
	FollowUpEnabled           bool   `json:"follow_up_enabled"`
	RelationEnabled           bool   `json:"relation_enabled"`
	// 以下字段把机器人级 Bot Runtime 的可覆盖项完整映射到会话来源，
	// 这样 WebUI 规则不会出现“可以保存但运行时不生效”的假配置。
	RuntimeEnabled              bool            `json:"runtime_enabled"`
	RuntimeMaxConcurrency       int             `json:"runtime_max_concurrency"`
	SourceQueueLimit            int             `json:"source_queue_limit"`
	TurnWaitMilliseconds        int             `json:"turn_wait_ms"`
	GroupTurnWaitMilliseconds   int             `json:"group_turn_wait_ms"`
	AttachmentWaitMilliseconds  int             `json:"attachment_wait_ms"`
	MaxTurnMessages             int             `json:"max_turn_messages"`
	GroupMessageMaxCount        int             `json:"group_message_max_count"`
	GroupImageCaption           bool            `json:"group_image_caption"`
	GroupImageCaptionModel      string          `json:"group_image_caption_model,omitempty"`
	ProactiveDegree             string          `json:"proactive_degree,omitempty"`
	CooldownSeconds             int             `json:"cooldown_seconds"`
	HeartbeatSeconds            int             `json:"heartbeat_seconds"`
	RelationRetentionSeconds    int             `json:"relation_retention_seconds"`
	FollowUpMax                 int             `json:"follow_up_max"`
	FollowUpMaxRetries          int             `json:"follow_up_max_retries"`
	FollowUpRetryDelaySeconds   int             `json:"follow_up_retry_delay_seconds"`
	FollowUpMaxDelaySeconds     int             `json:"follow_up_max_delay_seconds"`
	FollowUpAllowedSources      []string        `json:"follow_up_allowed_sources,omitempty"`
	ExpressionEnabled           bool            `json:"expression_enabled"`
	ExpressionMaxSegments       int             `json:"expression_max_segments"`
	ExpressionLongThreshold     int             `json:"expression_long_threshold"`
	ExpressionDelayMilliseconds int             `json:"expression_delay_ms"`
	ReplyMention                bool            `json:"reply_mention"`
	AgentOnDemandEnabled        bool            `json:"agent_on_demand_enabled"`
	AllowedReadOnlyTools        []string        `json:"allowed_read_only_tools,omitempty"`
	ToolBudget                  int             `json:"tool_budget"`
	SubAgentEnabled             bool            `json:"subagent_enabled"`
	ActionPermissions           map[string]bool `json:"action_permissions,omitempty"`
	MessageStyle                string          `json:"message_style,omitempty"`
	// ConfiguredFields 区分“该字段覆盖了全局配置”和“该字段恢复继承”。
	// 空值表示本条规则没有任何显式覆盖；所有运行时字段都必须通过该集合
	// 明确开启，避免旧配置零值在破坏性迁移后意外关闭新行为。
	ConfiguredFields []string  `json:"configured_fields,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Group 是一组可批量应用规则的会话来源集合。
type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Members     []string  `json:"members"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// BatchUpdate 描述批量修改的范围和需要覆盖的字段。
type BatchUpdate struct {
	Scope        string   `json:"scope"`
	Sources      []string `json:"sources,omitempty"`
	LLMEnabled   *bool    `json:"llm_enabled,omitempty"`
	TTSEnabled   *bool    `json:"tts_enabled,omitempty"`
	ChatModel    *string  `json:"chat_model,omitempty"`
	ProcessState *bool    `json:"process_enabled,omitempty"`
}

// Repository 是会话规则的最小持久化边界，具体 SQLite 实现在 storage/sqlite。
type Repository interface {
	List(context.Context, string) ([]Rule, error)
	Get(context.Context, string) (Rule, error)
	Save(context.Context, Rule) error
	Delete(context.Context, string) error
	ListGroups(context.Context) ([]Group, error)
	GetGroup(context.Context, string) (Group, error)
	SaveGroup(context.Context, Group) error
	DeleteGroup(context.Context, string) error
}

// SourceLister 返回已经从消息入口观察到的 UMO 列表。它是可选扩展，避免
// 会话规则服务反向依赖 Bot 包，同时让批量规则能够覆盖尚未创建规则的来源。
type SourceLister func(context.Context, string) ([]string, error)

// Service 负责校验、排序和批量更新，保证 HTTP 层不直接操作仓储。
type Service struct {
	repository Repository
	writeMu    sync.Mutex
	sourceList SourceLister
}

// NewService 创建会话规则服务。
func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, errors.New("会话规则 Repository 不能为空")
	}
	return &Service{repository: repository}, nil
}

// SetSourceLister 装配来源目录查询；生产应用使用 Bot 的持久化 UMO 目录，
// 没有该扩展的嵌入方仍按历史规则列表工作。
func (s *Service) SetSourceLister(lister SourceLister) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.sourceList = lister
}

// List 返回按来源排序的规则，可用 query 对来源和备注做模糊过滤。
func (s *Service) List(ctx context.Context, query string) ([]Rule, error) {
	items, err := s.repository.List(ctx, strings.TrimSpace(query))
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i] = normalizeRule(items[i])
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Source < items[j].Source })
	return items, nil
}

// Get 读取一个会话来源规则。
func (s *Service) Get(ctx context.Context, source string) (Rule, error) {
	source = strings.TrimSpace(source)
	if err := validateSource(source); err != nil {
		return Rule{}, err
	}
	item, err := s.repository.Get(ctx, source)
	if err != nil {
		return Rule{}, err
	}
	return normalizeRule(item), nil
}

// Resolve 返回规则是否存在；不存在不是错误，调用方应继续使用全局配置。
func (s *Service) Resolve(ctx context.Context, source string) (Rule, bool, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return Rule{}, false, nil
	}
	item, err := s.repository.Get(ctx, source)
	if errors.Is(err, ErrNotFound) {
		return Rule{}, false, nil
	}
	if err != nil {
		return Rule{}, false, err
	}
	return normalizeRule(item), true, nil
}

// Save 新增或更新规则；来源是稳定主键，重复保存只产生一次当前状态。
func (s *Service) Save(ctx context.Context, item Rule) (Rule, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	item = normalizeRule(item)
	if err := validateRule(item); err != nil {
		return Rule{}, err
	}
	old, err := s.repository.Get(ctx, item.Source)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Rule{}, err
	}
	now := time.Now().UTC()
	if err == nil {
		item.CreatedAt = old.CreatedAt
	} else {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	if err := s.repository.Save(ctx, item); err != nil {
		return Rule{}, fmt.Errorf("保存会话规则失败: %w", err)
	}
	return item, nil
}

// ResetField 清除一个独立覆盖项；最后一个覆盖项被清除后删除规则行，
// 这样列表中只保留真正有会话偏好的来源，行为与删除对应偏好键一致。
func (s *Service) ResetField(ctx context.Context, source, key string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	source = strings.TrimSpace(source)
	key = strings.TrimSpace(key)
	if err := validateSource(source); err != nil {
		return err
	}
	if !isOverrideKey(key) {
		return fmt.Errorf("%w: 不支持清除规则项 %q", ErrInvalidRequest, key)
	}
	item, err := s.repository.Get(ctx, source)
	if err != nil {
		return err
	}
	item.ConfiguredFields = removeString(item.ConfiguredFields, key)
	if len(item.ConfiguredFields) == 0 {
		if err := s.repository.Delete(ctx, source); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	}
	item = normalizeRule(item)
	item.UpdatedAt = time.Now().UTC()
	if err := s.repository.Save(ctx, item); err != nil {
		return fmt.Errorf("清除会话规则项失败: %w", err)
	}
	return nil
}

// Delete 删除指定来源规则。
func (s *Service) Delete(ctx context.Context, source string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := validateSource(strings.TrimSpace(source)); err != nil {
		return err
	}
	return s.repository.Delete(ctx, strings.TrimSpace(source))
}

// ApplyBatch 按选中会话、所有会话、所有群聊或所有私聊更新规则。
// 批量范围以完整 UMO 来源目录为准；没有旧规则的来源会在这里创建默认规则，
// 这与“批量覆盖偏好”行为一致。
func (s *Service) ApplyBatch(ctx context.Context, update BatchUpdate) ([]Rule, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	update.Scope = strings.ToLower(strings.TrimSpace(update.Scope))
	if update.Scope != "selected" && update.Scope != "all" && update.Scope != "groups" && update.Scope != "private" {
		return nil, fmt.Errorf("%w: 批量范围必须是 selected、all、groups 或 private", ErrInvalidRequest)
	}
	items, err := s.repository.List(ctx, "")
	if err != nil {
		return nil, err
	}
	existing := make(map[string]Rule, len(items))
	existingByLower := make(map[string]string, len(items))
	for _, item := range items {
		item = normalizeRule(item)
		existing[item.Source] = item
		existingByLower[strings.ToLower(item.Source)] = item.Source
	}
	targets := make(map[string]struct{})
	addTarget := func(source string) {
		source = strings.TrimSpace(source)
		if source == "" {
			return
		}
		if existingSource, ok := existingByLower[strings.ToLower(source)]; ok {
			source = existingSource
		}
		targets[source] = struct{}{}
	}
	if update.Scope == "selected" {
		for _, source := range update.Sources {
			addTarget(source)
		}
	} else {
		// 先把没有规则的来源加入候选集，再合并旧规则，兼容升级前的历史数据。
		if s.sourceList != nil {
			knownSources, listErr := s.sourceList(ctx, "")
			if listErr != nil {
				return nil, fmt.Errorf("读取消息来源目录失败: %w", listErr)
			}
			for _, source := range knownSources {
				if batchMatches(update.Scope, source, nil) {
					addTarget(source)
				}
			}
		}
		for source := range existing {
			if batchMatches(update.Scope, source, nil) {
				addTarget(source)
			}
		}
	}
	changed := make([]Rule, 0, len(targets))
	for source := range targets {
		item, ok := existing[source]
		if !ok {
			item = defaultRule(source)
		}
		if update.LLMEnabled != nil {
			item.LLMEnabled = *update.LLMEnabled
			item.ConfiguredFields = addString(item.ConfiguredFields, OverrideLLMEnabled)
		}
		if update.TTSEnabled != nil {
			item.TTSEnabled = *update.TTSEnabled
			item.ConfiguredFields = addString(item.ConfiguredFields, OverrideTTSEnabled)
		}
		if update.ChatModel != nil {
			item.ChatModel = strings.TrimSpace(*update.ChatModel)
			item.ConfiguredFields = addString(item.ConfiguredFields, OverrideChatModel)
		}
		if update.ProcessState != nil {
			item.ProcessEnabled = *update.ProcessState
			item.ConfiguredFields = addString(item.ConfiguredFields, OverrideProcessEnabled)
		}
		item = normalizeRule(item)
		if err := validateRule(item); err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		item.UpdatedAt = now
		if err := s.repository.Save(ctx, item); err != nil {
			return nil, fmt.Errorf("批量保存会话规则失败: %w", err)
		}
		changed = append(changed, item)
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Source < changed[j].Source })
	return changed, nil
}

// defaultRule 是首次对某个已知 UMO 执行批量操作时的安全初始状态；只有
// 批量请求显式覆盖的字段会改变它，其余字段沿用全局内置 AI 配置。
func defaultRule(source string) Rule {
	return Rule{
		Source: source, ProcessEnabled: true, LLMEnabled: true, TTSEnabled: false,
		FollowProfile: true, KnowledgeTopK: 5, ConfiguredFields: []string{},
	}
}

// ListGroups 返回分组列表。
func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	items, err := s.repository.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i] = normalizeGroup(items[i])
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// SaveGroup 新增或更新规则分组。
func (s *Service) SaveGroup(ctx context.Context, item Group) (Group, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	item = normalizeGroup(item)
	if item.Name == "" {
		return Group{}, fmt.Errorf("%w: 分组名称不能为空", ErrInvalidRequest)
	}
	if len(item.Name) > 200 || len(item.Description) > 2000 {
		return Group{}, fmt.Errorf("%w: 分组文本过长", ErrInvalidRequest)
	}
	if item.ID == "" {
		item.ID = newID("group")
	}
	old, err := s.repository.GetGroup(ctx, item.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Group{}, err
	}
	if err == nil {
		item.CreatedAt = old.CreatedAt
	} else {
		item.CreatedAt = time.Now().UTC()
	}
	items, listErr := s.repository.ListGroups(ctx)
	if listErr != nil {
		return Group{}, listErr
	}
	for _, existing := range items {
		if existing.ID != item.ID && strings.EqualFold(strings.TrimSpace(existing.Name), item.Name) {
			return Group{}, fmt.Errorf("%w: 分组名称重复", ErrConflict)
		}
	}
	item.UpdatedAt = time.Now().UTC()
	if err := s.repository.SaveGroup(ctx, item); err != nil {
		return Group{}, fmt.Errorf("保存会话分组失败: %w", err)
	}
	return item, nil
}

// DeleteGroup 删除会话分组。
func (s *Service) DeleteGroup(ctx context.Context, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: 分组 ID 不能为空", ErrInvalidRequest)
	}
	return s.repository.DeleteGroup(ctx, strings.TrimSpace(id))
}

func batchMatches(scope, source string, selected map[string]struct{}) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	switch scope {
	case "selected":
		_, ok := selected[source]
		return ok
	case "groups":
		parts := strings.Split(source, ":")
		if len(parts) > 1 {
			return parts[1] == strings.ToLower("GroupMessage")
		}
		return strings.Contains(source, "group") || strings.Contains(source, "guild") || strings.Contains(source, "群")
	case "private":
		return !batchMatches("groups", source, nil)
	default:
		return true
	}
}

func normalizeRule(item Rule) Rule {
	item.Source = strings.TrimSpace(item.Source)
	item.Note = strings.TrimSpace(item.Note)
	item.ChatModel = strings.TrimSpace(item.ChatModel)
	item.STTModel = strings.TrimSpace(item.STTModel)
	item.TTSModel = strings.TrimSpace(item.TTSModel)
	item.ProfileID = strings.TrimSpace(item.ProfileID)
	item.PersonaID = strings.TrimSpace(item.PersonaID)
	item.PrivateMode = strings.TrimSpace(item.PrivateMode)
	item.GroupParticipationMode = strings.TrimSpace(item.GroupParticipationMode)
	item.QuietHoursTimezone = strings.TrimSpace(item.QuietHoursTimezone)
	item.QuietHoursStart = strings.TrimSpace(item.QuietHoursStart)
	item.QuietHoursEnd = strings.TrimSpace(item.QuietHoursEnd)
	item.GroupImageCaptionModel = strings.TrimSpace(item.GroupImageCaptionModel)
	item.ProactiveDegree = strings.TrimSpace(item.ProactiveDegree)
	item.MessageStyle = strings.TrimSpace(item.MessageStyle)
	item.DisabledPlugins = uniqueStrings(item.DisabledPlugins)
	item.KnowledgeBases = uniqueStrings(item.KnowledgeBases)
	item.FollowUpAllowedSources = uniqueStrings(item.FollowUpAllowedSources)
	item.AllowedReadOnlyTools = uniqueStrings(item.AllowedReadOnlyTools)
	if item.ActionPermissions != nil {
		permissions := make(map[string]bool, len(item.ActionPermissions))
		for key, enabled := range item.ActionPermissions {
			key = strings.TrimSpace(key)
			if key != "" {
				permissions[key] = enabled
			}
		}
		item.ActionPermissions = permissions
	}
	if item.ConfiguredFields != nil {
		item.ConfiguredFields = uniqueStrings(item.ConfiguredFields)
	}
	if item.KnowledgeTopK <= 0 {
		item.KnowledgeTopK = 5
	}
	return item
}

// HasOverride 判断一个规则字段是否真正覆盖全局配置。破坏性迁移后不再根据
// 旧规则的整行零值猜测覆盖范围，只有 WebUI/HTTP 明确标记的字段才会生效。
func (item Rule) HasOverride(key string) bool {
	for _, configured := range item.ConfiguredFields {
		if configured == key {
			return true
		}
	}
	return false
}

// MarkOverride 将一个 HTTP 表单字段标记为显式覆盖；未标记的字段保持继承。
func (item *Rule) MarkOverride(key string) {
	if item == nil || !isOverrideKey(key) {
		return
	}
	if item.ConfiguredFields == nil {
		item.ConfiguredFields = make([]string, 0, 1)
	}
	item.ConfiguredFields = addString(item.ConfiguredFields, key)
}

func isOverrideKey(key string) bool {
	for _, item := range allOverrideKeys() {
		if item == key {
			return true
		}
	}
	return false
}

// allOverrideKeys 只用于校验 HTTP 清除字段，字段是否真正生效仍由
// ConfiguredFields 决定，不会把整条规则误判为全量覆盖。
func allOverrideKeys() []string {
	return []string{
		OverrideProcessEnabled, OverrideLLMEnabled, OverrideTTSEnabled, OverrideNote,
		OverrideChatModel, OverrideSTTModel, OverrideTTSModel, OverrideFollowProfile,
		OverrideProfileID, OverridePersonaID, OverrideDisabledPlugins, OverrideKnowledgeBases,
		OverrideKnowledgeTopK, OverrideKnowledgeRerank, OverridePrivateMode, OverrideGroupParticipationMode,
		OverrideRecordUnaddressedMessages, OverrideReactionEnabled, OverrideGroupContextEnabled,
		OverrideProactiveEnabled, OverridePrivateHourlyReplyLimit, OverrideGroupHourlyReplyLimit,
		OverrideQuietHoursTimezone, OverrideQuietHoursStart, OverrideQuietHoursEnd,
		OverrideEmergencyBypassQuietHours, OverrideReplyQuote, OverridePrivateReplyQuote,
		OverrideFollowUpEnabled, OverrideRelationEnabled, OverrideRuntimeEnabled,
		OverrideRuntimeMaxConcurrency, OverrideSourceQueueLimit, OverrideTurnWaitMilliseconds,
		OverrideGroupTurnWaitMilliseconds, OverrideAttachmentWaitMilliseconds, OverrideMaxTurnMessages,
		OverrideGroupMessageMaxCount, OverrideGroupImageCaption, OverrideGroupImageCaptionModel,
		OverrideProactiveDegree, OverrideCooldownSeconds, OverrideHeartbeatSeconds,
		OverrideRelationRetentionSeconds, OverrideFollowUpMax, OverrideFollowUpMaxRetries,
		OverrideFollowUpRetryDelaySeconds, OverrideFollowUpMaxDelaySeconds, OverrideFollowUpAllowedSources,
		OverrideExpressionEnabled, OverrideExpressionMaxSegments, OverrideExpressionLongThreshold,
		OverrideExpressionDelayMilliseconds, OverrideReplyMention, OverrideAgentOnDemandEnabled,
		OverrideAllowedReadOnlyTools, OverrideToolBudget, OverrideSubAgentEnabled,
		OverrideActionPermissions, OverrideMessageStyle,
	}
}

func addString(values []string, value string) []string {
	for _, item := range values {
		if item == value {
			return values
		}
	}
	return append(values, value)
}

func removeString(values []string, value string) []string {
	result := make([]string, 0, len(values))
	for _, item := range values {
		if item != value {
			result = append(result, item)
		}
	}
	return result
}

func normalizeGroup(item Group) Group {
	item.ID = strings.TrimSpace(item.ID)
	item.Name = strings.TrimSpace(item.Name)
	item.Description = strings.TrimSpace(item.Description)
	item.Members = uniqueStrings(item.Members)
	return item
}

func validateRule(item Rule) error {
	if err := validateSource(item.Source); err != nil {
		return err
	}
	if len(item.Note) > 4000 || len(item.ChatModel) > 300 || len(item.STTModel) > 300 || len(item.TTSModel) > 300 {
		return fmt.Errorf("%w: 规则文本过长", ErrInvalidRequest)
	}
	if item.KnowledgeTopK < 1 || item.KnowledgeTopK > 100 {
		return fmt.Errorf("%w: 知识库 Top K 必须在 1-100 之间", ErrInvalidRequest)
	}
	if item.PrivateMode != "" && item.PrivateMode != "responsive" && item.PrivateMode != "observe_only" {
		return fmt.Errorf("%w: private_mode 无效", ErrInvalidRequest)
	}
	if item.GroupParticipationMode != "" && item.GroupParticipationMode != "addressed_only" && item.GroupParticipationMode != "observe_only" {
		return fmt.Errorf("%w: group_participation_mode 无效", ErrInvalidRequest)
	}
	if item.PrivateHourlyReplyLimit < 0 || item.PrivateHourlyReplyLimit > 100000 || item.GroupHourlyReplyLimit < 0 || item.GroupHourlyReplyLimit > 100000 {
		return fmt.Errorf("%w: 每小时主动回复上限必须在 0-100000 之间", ErrInvalidRequest)
	}
	if item.QuietHoursTimezone != "" {
		if _, err := time.LoadLocation(item.QuietHoursTimezone); err != nil {
			return fmt.Errorf("%w: quiet_hours_timezone 无效", ErrInvalidRequest)
		}
	}
	if item.ProactiveDegree != "" && item.ProactiveDegree != "off" && item.ProactiveDegree != "low" && item.ProactiveDegree != "normal" && item.ProactiveDegree != "high" {
		return fmt.Errorf("%w: proactive_degree 无效", ErrInvalidRequest)
	}
	if item.MessageStyle != "" && item.MessageStyle != "natural" && item.MessageStyle != "structured" {
		return fmt.Errorf("%w: message_style 无效", ErrInvalidRequest)
	}
	intRanges := []struct {
		name  string
		value int
		min   int
		max   int
	}{
		{"runtime_max_concurrency", item.RuntimeMaxConcurrency, 1, 64},
		{"source_queue_limit", item.SourceQueueLimit, 1, 10000},
		{"turn_wait_ms", item.TurnWaitMilliseconds, 0, 120000},
		{"group_turn_wait_ms", item.GroupTurnWaitMilliseconds, 0, 120000},
		{"attachment_wait_ms", item.AttachmentWaitMilliseconds, 0, 180000},
		{"max_turn_messages", item.MaxTurnMessages, 1, 100},
		{"group_message_max_count", item.GroupMessageMaxCount, 1, 300},
		{"cooldown_seconds", item.CooldownSeconds, 0, 604800},
		{"heartbeat_seconds", item.HeartbeatSeconds, 5, 86400},
		{"relation_retention_seconds", item.RelationRetentionSeconds, 3600, 31536000},
		{"follow_up_max", item.FollowUpMax, 1, 10000},
		{"follow_up_max_retries", item.FollowUpMaxRetries, 0, 10},
		{"follow_up_retry_delay_seconds", item.FollowUpRetryDelaySeconds, 0, 86400},
		{"follow_up_max_delay_seconds", item.FollowUpMaxDelaySeconds, 0, 604800},
		{"expression_max_segments", item.ExpressionMaxSegments, 1, 8},
		{"expression_long_threshold", item.ExpressionLongThreshold, 100, 10000},
		{"expression_delay_ms", item.ExpressionDelayMilliseconds, 0, 5000},
		{"tool_budget", item.ToolBudget, 0, 100},
	}
	for _, candidate := range intRanges {
		// 新规则只有显式标记的字段才校验零值；未配置字段使用零值占位，
		// 不能因为继承状态而被错误判成超出最小值。非零历史值仍然校验，
		// 防止旧数据把明显越界的配置带入运行时。
		if candidate.value == 0 && !item.HasOverride(candidate.name) {
			continue
		}
		if candidate.value < candidate.min || candidate.value > candidate.max {
			return fmt.Errorf("%w: %s 必须在 %d-%d 之间", ErrInvalidRequest, candidate.name, candidate.min, candidate.max)
		}
	}
	knownActions := map[string]struct{}{"send_text": {}, "send_image": {}, "send_audio": {}, "send_file": {}, "add_reaction": {}, "poke": {}, "recall_message": {}, "start_agent": {}, "create_follow_up": {}, "update_relation": {}, "update_source_state": {}}
	for action := range item.ActionPermissions {
		if _, ok := knownActions[action]; !ok {
			return fmt.Errorf("%w: action_permissions 包含未知动作 %q", ErrInvalidRequest, action)
		}
	}
	if item.QuietHoursStart != "" || item.QuietHoursEnd != "" {
		if item.QuietHoursStart == "" || item.QuietHoursEnd == "" {
			return fmt.Errorf("%w: 安静时段开始和结束必须同时设置", ErrInvalidRequest)
		}
		if _, err := time.Parse("15:04", item.QuietHoursStart); err != nil {
			return fmt.Errorf("%w: quiet_hours_start 必须是 HH:MM", ErrInvalidRequest)
		}
		if _, err := time.Parse("15:04", item.QuietHoursEnd); err != nil {
			return fmt.Errorf("%w: quiet_hours_end 必须是 HH:MM", ErrInvalidRequest)
		}
	}
	return nil
}

func validateSource(source string) error {
	if source == "" || len(source) > 500 {
		return fmt.Errorf("%w: 消息会话来源不能为空且不能超过 500 个字符", ErrInvalidRequest)
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
}
