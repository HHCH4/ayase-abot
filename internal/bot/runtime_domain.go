package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
)

// PlatformEventType 是平台事件网关对不同平台事件使用的稳定枚举。
// 业务层只依赖这些值，不直接判断 OneBot 或 Telegram 的原始字段。
type PlatformEventType string

const (
	PlatformEventMessageCreated       PlatformEventType = "message.created"
	PlatformEventMessageRecalled      PlatformEventType = "message.recalled"
	PlatformEventReactionAdded        PlatformEventType = "reaction.added"
	PlatformEventReactionRemoved      PlatformEventType = "reaction.removed"
	PlatformEventMemberJoined         PlatformEventType = "member.joined"
	PlatformEventMemberLeft           PlatformEventType = "member.left"
	PlatformEventPokeReceived         PlatformEventType = "poke.received"
	PlatformEventPlatformConnected    PlatformEventType = "platform.connected"
	PlatformEventPlatformDisconnected PlatformEventType = "platform.disconnected"
	PlatformEventApprovalCallback     PlatformEventType = "approval.callback"
)

// PlatformCapabilities 描述适配器可以安全执行的抽象动作。
// 业务层不得根据平台名称拼接 CQ 码、HTML 或原始 JSON。
type PlatformCapabilities struct {
	PlainText       bool `json:"plain_text"`
	RichText        bool `json:"rich_text"`
	Reply           bool `json:"reply"`
	Mention         bool `json:"mention"`
	Reaction        bool `json:"reaction"`
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	File            bool `json:"file"`
	Poke            bool `json:"poke"`
	Recall          bool `json:"recall"`
	TypingIndicator bool `json:"typing_indicator"`
	MaxTextLength   int  `json:"max_text_length"`
}

// PlatformEvent 是所有平台输入的统一事实。Message 只承载能够进入聊天入口的文本消息，
// 其他事件仍会被 Runtime 记录并用于生命周期、关系和审计，不会被错误地送进模型。
type PlatformEvent struct {
	ID                 string               `json:"id"`
	BotID              string               `json:"bot_id"`
	Platform           Type                 `json:"platform"`
	SourceUMO          string               `json:"source_umo"`
	ChatType           string               `json:"chat_type"`
	ChatID             string               `json:"chat_id"`
	UserID             string               `json:"user_id"`
	UserName           string               `json:"user_name,omitempty"`
	EventType          PlatformEventType    `json:"event_type"`
	MessageID          string               `json:"message_id,omitempty"`
	ReplyToMessageID   string               `json:"reply_to_message_id,omitempty"`
	MentionedUserIDs   []string             `json:"mentioned_user_ids,omitempty"`
	MentionsBot        bool                 `json:"mentions_bot"`
	Text               string               `json:"text,omitempty"`
	Attachments        []agent.Attachment   `json:"attachments,omitempty"`
	NativeCapabilities PlatformCapabilities `json:"native_capabilities"`
	OccurredAt         time.Time            `json:"occurred_at"`
	ReceivedAt         time.Time            `json:"received_at"`
	RawRef             string               `json:"raw_ref,omitempty"`
	Message            *Message             `json:"-"`
}

// EventHandler 是平台事件网关的统一回调。
type EventHandler func(context.Context, PlatformEvent) error

// PlatformEventRecorder 持久化不能进入 Agent 的平台事件，保证撤回、反应和连接状态
// 在 WebUI/日志中仍然可追踪。它是可选接口，避免内存嵌入方被迫实现数据库功能。
type PlatformEventRecorder interface {
	RecordPlatformEvent(context.Context, PlatformEvent) error
}

// PlatformEventDeduplicator 在写入平台事件时返回本次是否首次接收；事件网关据此
// 避免平台重投事件再次刷新来源、重复进入消息聚合器或重复触发审批。
type PlatformEventDeduplicator interface {
	RecordPlatformEventIfNew(context.Context, PlatformEvent) (bool, error)
}

// PlatformEventRuntime 是所有平台适配器必须实现的统一事件入口；消息、回调和
// 生命周期事件都必须先归一化为 PlatformEvent，再进入 Bot Runtime。
type PlatformEventRuntime interface {
	RunEvents(context.Context, EventHandler) error
	Capabilities() PlatformCapabilities
}

// platformEventFromMessage 将适配器已经归一化的消息补充为统一事件；非消息平台事件
// 由适配器直接构造 PlatformEvent，不能绕过事件网关。
func platformEventFromMessage(message Message, eventType PlatformEventType, capabilities PlatformCapabilities) PlatformEvent {
	received := time.Now().UTC()
	eventID := runtimeInboxID(message)
	// 审批回调的消息 ID 由平台回调构造；再次使用 CallbackID 作为显式
	// 去重因子，避免某些嵌入适配器复用原消息 ID 时吞掉后续选择。
	if message.Control != nil && strings.TrimSpace(message.Control.CallbackID) != "" {
		eventID = strings.TrimSpace(message.AdapterID) + ":" + string(message.Platform) + ":callback:" + strings.TrimSpace(message.Control.CallbackID)
	}
	return PlatformEvent{
		ID: eventID, BotID: message.AdapterID, Platform: message.Platform, SourceUMO: messageSource(message),
		ChatType: message.ChatType, ChatID: message.ChatID, UserID: message.UserID, UserName: message.AutoName,
		EventType: eventType, MessageID: message.ID, ReplyToMessageID: message.ReplyToMessageID,
		MentionedUserIDs: append([]string(nil), message.Mentions...), MentionsBot: message.Mentioned, Text: message.Text,
		Attachments: append([]agent.Attachment(nil), message.Attachments...), NativeCapabilities: capabilities,
		OccurredAt: received, ReceivedAt: received, Message: &message,
	}
}

// PlatformAction 描述一次与平台交互的抽象动作。
type PlatformAction struct {
	Type       string            `json:"type"`
	Message    Message           `json:"message"`
	Text       string            `json:"text,omitempty"`
	Attachment *agent.Attachment `json:"attachment,omitempty"`
	// 结构化交互也必须经过动作计划；适配器只负责选择按钮或文本渲染。
	Approval  *ApprovalPrompt                `json:"approval,omitempty"`
	UserInput *agentruntime.UserInputRequest `json:"user_input,omitempty"`
	TargetID  string                         `json:"target_id,omitempty"`
	Emoji     string                         `json:"emoji,omitempty"`
	Payload   map[string]any                 `json:"payload,omitempty"`
}

// PlatformActionResult 是适配器返回的最小可审计结果。
type PlatformActionResult struct {
	MessageID string `json:"message_id,omitempty"`
	Raw       string `json:"raw,omitempty"`
}

// PlatformActionDispatcher 把抽象动作投影为具体平台调用。
type PlatformActionDispatcher interface {
	DispatchAction(context.Context, PlatformAction) (PlatformActionResult, error)
}

// BotRuntimeConfig 是机器人页面上的显式覆盖配置。指针字段区分“继承绑定配置”和
// “明确覆盖为零值/关闭”，从而保持系统默认→配置文件→机器人→会话的优先级。
type BotRuntimeConfig struct {
	RuntimeEnabled              *bool           `json:"runtime_enabled,omitempty"`
	RuntimeMaxConcurrency       *int            `json:"runtime_max_concurrency,omitempty"`
	SourceQueueLimit            *int            `json:"source_queue_limit,omitempty"`
	TurnWaitMilliseconds        *int            `json:"turn_wait_ms,omitempty"`
	GroupTurnWaitMilliseconds   *int            `json:"group_turn_wait_ms,omitempty"`
	AttachmentWaitMilliseconds  *int            `json:"attachment_wait_ms,omitempty"`
	MaxTurnMessages             *int            `json:"max_turn_messages,omitempty"`
	PrivateMode                 *string         `json:"private_mode,omitempty"`
	GroupParticipationMode      *string         `json:"group_participation_mode,omitempty"`
	RecordUnaddressedMessages   *bool           `json:"record_unaddressed_messages,omitempty"`
	ReactionEnabled             *bool           `json:"reaction_enabled,omitempty"`
	GroupContextEnabled         *bool           `json:"group_context_enabled,omitempty"`
	GroupMessageMaxCount        *int            `json:"group_message_max_count,omitempty"`
	GroupImageCaption           *bool           `json:"group_image_caption,omitempty"`
	GroupImageCaptionModel      *string         `json:"group_image_caption_model,omitempty"`
	ProactiveEnabled            *bool           `json:"proactive_enabled,omitempty"`
	ProactiveDegree             *string         `json:"proactive_degree,omitempty"`
	CooldownSeconds             *int            `json:"cooldown_seconds,omitempty"`
	PrivateHourlyReplyLimit     *int            `json:"private_hourly_reply_limit,omitempty"`
	GroupHourlyReplyLimit       *int            `json:"group_hourly_reply_limit,omitempty"`
	HeartbeatSeconds            *int            `json:"heartbeat_seconds,omitempty"`
	QuietHoursTimezone          *string         `json:"quiet_hours_timezone,omitempty"`
	QuietHoursStart             *string         `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd               *string         `json:"quiet_hours_end,omitempty"`
	EmergencyBypassQuietHours   *bool           `json:"emergency_bypass_quiet_hours,omitempty"`
	RelationEnabled             *bool           `json:"relation_enabled,omitempty"`
	RelationRetentionSeconds    *int            `json:"relation_retention_seconds,omitempty"`
	FollowUpEnabled             *bool           `json:"follow_up_enabled,omitempty"`
	FollowUpMax                 *int            `json:"follow_up_max,omitempty"`
	FollowUpMaxRetries          *int            `json:"follow_up_max_retries,omitempty"`
	FollowUpRetryDelaySeconds   *int            `json:"follow_up_retry_delay_seconds,omitempty"`
	FollowUpMaxDelaySeconds     *int            `json:"follow_up_max_delay_seconds,omitempty"`
	FollowUpAllowedSources      []string        `json:"follow_up_allowed_sources,omitempty"`
	ExpressionEnabled           *bool           `json:"expression_enabled,omitempty"`
	ExpressionMaxSegments       *int            `json:"expression_max_segments,omitempty"`
	ExpressionLongThreshold     *int            `json:"expression_long_threshold,omitempty"`
	ExpressionDelayMilliseconds *int            `json:"expression_delay_ms,omitempty"`
	ReplyMention                *bool           `json:"reply_mention,omitempty"`
	ReplyQuote                  *bool           `json:"reply_quote,omitempty"`
	PrivateReplyQuote           *bool           `json:"private_reply_quote,omitempty"`
	AgentOnDemandEnabled        *bool           `json:"agent_on_demand_enabled,omitempty"`
	AllowedReadOnlyTools        []string        `json:"allowed_read_only_tools,omitempty"`
	ToolBudget                  *int            `json:"tool_budget,omitempty"`
	SubAgentEnabled             *bool           `json:"subagent_enabled,omitempty"`
	ActionPermissions           map[string]bool `json:"action_permissions,omitempty"`
	MessageStyle                *string         `json:"message_style,omitempty"`
	ConfigRevision              int64           `json:"config_revision,omitempty"`
	PersonaRevision             int64           `json:"persona_revision,omitempty"`
}

// Validate 检查机器人级 Runtime 覆盖值，避免 WebUI 的异常数值绕过配置 Schema
// 直接进入消息入口、动作权限和主动调度器。
func (c BotRuntimeConfig) Validate() error {
	checkInt := func(name string, value *int, min, max int) error {
		if value == nil {
			return nil
		}
		if *value < min || *value > max {
			return fmt.Errorf("%s 必须在 %d-%d 之间", name, min, max)
		}
		return nil
	}
	checks := []struct {
		name     string
		value    *int
		min, max int
	}{
		{"runtime_max_concurrency", c.RuntimeMaxConcurrency, 1, 64}, {"source_queue_limit", c.SourceQueueLimit, 1, 10000},
		{"turn_wait_ms", c.TurnWaitMilliseconds, 0, 120000}, {"group_turn_wait_ms", c.GroupTurnWaitMilliseconds, 0, 120000},
		{"attachment_wait_ms", c.AttachmentWaitMilliseconds, 0, 180000}, {"max_turn_messages", c.MaxTurnMessages, 1, 100},
		{"group_message_max_count", c.GroupMessageMaxCount, 1, 300}, {"cooldown_seconds", c.CooldownSeconds, 0, 604800},
		{"private_hourly_reply_limit", c.PrivateHourlyReplyLimit, 0, 100000}, {"group_hourly_reply_limit", c.GroupHourlyReplyLimit, 0, 100000},
		{"heartbeat_seconds", c.HeartbeatSeconds, 5, 86400}, {"relation_retention_seconds", c.RelationRetentionSeconds, 3600, 31536000},
		{"follow_up_max", c.FollowUpMax, 1, 10000}, {"follow_up_max_retries", c.FollowUpMaxRetries, 0, 10},
		{"follow_up_retry_delay_seconds", c.FollowUpRetryDelaySeconds, 0, 86400}, {"follow_up_max_delay_seconds", c.FollowUpMaxDelaySeconds, 0, 604800},
		{"expression_max_segments", c.ExpressionMaxSegments, 1, 8},
		{"expression_long_threshold", c.ExpressionLongThreshold, 100, 10000}, {"expression_delay_ms", c.ExpressionDelayMilliseconds, 0, 5000},
		{"tool_budget", c.ToolBudget, 0, 100},
	}
	for _, item := range checks {
		if err := checkInt(item.name, item.value, item.min, item.max); err != nil {
			return err
		}
	}
	if c.PrivateMode != nil && *c.PrivateMode != "" && *c.PrivateMode != "responsive" && *c.PrivateMode != "observe_only" {
		return fmt.Errorf("private_mode %q 无效", *c.PrivateMode)
	}
	if c.GroupParticipationMode != nil && *c.GroupParticipationMode != "" && *c.GroupParticipationMode != "addressed_only" && *c.GroupParticipationMode != "observe_only" {
		return fmt.Errorf("group_participation_mode %q 无效", *c.GroupParticipationMode)
	}
	if c.ProactiveDegree != nil && *c.ProactiveDegree != "" && *c.ProactiveDegree != "off" && *c.ProactiveDegree != "low" && *c.ProactiveDegree != "normal" && *c.ProactiveDegree != "high" {
		return fmt.Errorf("proactive_degree %q 无效", *c.ProactiveDegree)
	}
	if c.MessageStyle != nil && *c.MessageStyle != "" && *c.MessageStyle != "natural" && *c.MessageStyle != "structured" {
		return fmt.Errorf("message_style %q 无效", *c.MessageStyle)
	}
	if c.QuietHoursTimezone != nil && strings.TrimSpace(*c.QuietHoursTimezone) != "" {
		if _, err := time.LoadLocation(strings.TrimSpace(*c.QuietHoursTimezone)); err != nil {
			return fmt.Errorf("quiet_hours_timezone 无效: %w", err)
		}
	}
	if err := validateRuntimeTimePair(c.QuietHoursStart, c.QuietHoursEnd); err != nil {
		return err
	}
	knownActions := map[string]bool{"send_text": true, "send_image": true, "send_audio": true, "send_file": true, "add_reaction": true, "poke": true, "recall_message": true, "start_agent": true, "create_follow_up": true, "update_relation": true, "update_source_state": true}
	for action := range c.ActionPermissions {
		if !knownActions[action] {
			return fmt.Errorf("action_permissions 包含未知动作 %q", action)
		}
	}
	return nil
}

func validateRuntimeTimePair(start, end *string) error {
	startValue := ""
	endValue := ""
	if start != nil {
		startValue = strings.TrimSpace(*start)
	}
	if end != nil {
		endValue = strings.TrimSpace(*end)
	}
	if startValue == "" && endValue == "" {
		return nil
	}
	if startValue == "" || endValue == "" {
		return fmt.Errorf("quiet_hours_start 和 quiet_hours_end 必须同时设置")
	}
	for name, value := range map[string]string{"quiet_hours_start": startValue, "quiet_hours_end": endValue} {
		if _, err := time.Parse("15:04", value); err != nil {
			return fmt.Errorf("%s 必须是 HH:MM", name)
		}
	}
	return nil
}

// NormalizeRuntimeConfig 去重和裁剪可配置列表；权限判断不依赖这些列表的顺序。
func (c BotRuntimeConfig) NormalizeRuntimeConfig() BotRuntimeConfig {
	c.PrivateMode = trimRuntimeString(c.PrivateMode)
	c.GroupParticipationMode = trimRuntimeString(c.GroupParticipationMode)
	c.GroupImageCaptionModel = trimRuntimeString(c.GroupImageCaptionModel)
	c.ProactiveDegree = trimRuntimeString(c.ProactiveDegree)
	c.QuietHoursTimezone = trimRuntimeString(c.QuietHoursTimezone)
	c.QuietHoursStart = trimRuntimeString(c.QuietHoursStart)
	c.QuietHoursEnd = trimRuntimeString(c.QuietHoursEnd)
	c.MessageStyle = trimRuntimeString(c.MessageStyle)
	c.FollowUpAllowedSources = normalizeRuntimeList(c.FollowUpAllowedSources)
	c.AllowedReadOnlyTools = normalizeRuntimeList(c.AllowedReadOnlyTools)
	return c
}

func trimRuntimeString(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func normalizeRuntimeList(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
