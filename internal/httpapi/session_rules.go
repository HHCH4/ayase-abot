package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"Abot/internal/sessionrule"
)

// sessionRulePayload 使用指针字段支持编辑页只提交发生变化的开关。
type sessionRulePayload struct {
	Source                      string           `json:"source"`
	ProcessEnabled              *bool            `json:"process_enabled"`
	LLMEnabled                  *bool            `json:"llm_enabled"`
	TTSEnabled                  *bool            `json:"tts_enabled"`
	Note                        *string          `json:"note"`
	ChatModel                   *string          `json:"chat_model"`
	STTModel                    *string          `json:"stt_model"`
	TTSModel                    *string          `json:"tts_model"`
	FollowProfile               *bool            `json:"follow_profile"`
	ProfileID                   *string          `json:"profile_id"`
	PersonaID                   *string          `json:"persona_id"`
	DisabledPlugins             *[]string        `json:"disabled_plugins"`
	KnowledgeBases              *[]string        `json:"knowledge_bases"`
	KnowledgeTopK               *int             `json:"knowledge_top_k"`
	KnowledgeRerank             *bool            `json:"knowledge_rerank"`
	PrivateMode                 *string          `json:"private_mode"`
	GroupParticipationMode      *string          `json:"group_participation_mode"`
	RecordUnaddressedMessages   *bool            `json:"record_unaddressed_messages"`
	ReactionEnabled             *bool            `json:"reaction_enabled"`
	GroupContextEnabled         *bool            `json:"group_context_enabled"`
	ProactiveEnabled            *bool            `json:"proactive_enabled"`
	PrivateHourlyReplyLimit     *int             `json:"private_hourly_reply_limit"`
	GroupHourlyReplyLimit       *int             `json:"group_hourly_reply_limit"`
	QuietHoursTimezone          *string          `json:"quiet_hours_timezone"`
	QuietHoursStart             *string          `json:"quiet_hours_start"`
	QuietHoursEnd               *string          `json:"quiet_hours_end"`
	EmergencyBypassQuietHours   *bool            `json:"emergency_bypass_quiet_hours"`
	ReplyQuote                  *bool            `json:"reply_quote"`
	PrivateReplyQuote           *bool            `json:"private_reply_quote"`
	FollowUpEnabled             *bool            `json:"follow_up_enabled"`
	RelationEnabled             *bool            `json:"relation_enabled"`
	RuntimeEnabled              *bool            `json:"runtime_enabled"`
	RuntimeMaxConcurrency       *int             `json:"runtime_max_concurrency"`
	SourceQueueLimit            *int             `json:"source_queue_limit"`
	TurnWaitMilliseconds        *int             `json:"turn_wait_ms"`
	GroupTurnWaitMilliseconds   *int             `json:"group_turn_wait_ms"`
	AttachmentWaitMilliseconds  *int             `json:"attachment_wait_ms"`
	MaxTurnMessages             *int             `json:"max_turn_messages"`
	GroupMessageMaxCount        *int             `json:"group_message_max_count"`
	GroupImageCaption           *bool            `json:"group_image_caption"`
	GroupImageCaptionModel      *string          `json:"group_image_caption_model"`
	ProactiveDegree             *string          `json:"proactive_degree"`
	CooldownSeconds             *int             `json:"cooldown_seconds"`
	HeartbeatSeconds            *int             `json:"heartbeat_seconds"`
	RelationRetentionSeconds    *int             `json:"relation_retention_seconds"`
	FollowUpMax                 *int             `json:"follow_up_max"`
	FollowUpMaxRetries          *int             `json:"follow_up_max_retries"`
	FollowUpRetryDelaySeconds   *int             `json:"follow_up_retry_delay_seconds"`
	FollowUpMaxDelaySeconds     *int             `json:"follow_up_max_delay_seconds"`
	FollowUpAllowedSources      *[]string        `json:"follow_up_allowed_sources"`
	ExpressionEnabled           *bool            `json:"expression_enabled"`
	ExpressionMaxSegments       *int             `json:"expression_max_segments"`
	ExpressionLongThreshold     *int             `json:"expression_long_threshold"`
	ExpressionDelayMilliseconds *int             `json:"expression_delay_ms"`
	ReplyMention                *bool            `json:"reply_mention"`
	AgentOnDemandEnabled        *bool            `json:"agent_on_demand_enabled"`
	AllowedReadOnlyTools        *[]string        `json:"allowed_read_only_tools"`
	ToolBudget                  *int             `json:"tool_budget"`
	SubAgentEnabled             *bool            `json:"subagent_enabled"`
	ActionPermissions           *map[string]bool `json:"action_permissions"`
	MessageStyle                *string          `json:"message_style"`
}

func (s *Server) requireSessionRules() (*sessionrule.Service, error) {
	if s.sessionRules == nil {
		return nil, errors.New("会话规则服务尚未装配")
	}
	return s.sessionRules, nil
}

func (s *Server) listSessionRules(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.List(request.Context(), request.URL.Query().Get("q"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"rules": items, "count": len(items)})
}

func (s *Server) createSessionRule(writer http.ResponseWriter, request *http.Request) {
	s.saveSessionRule(writer, request, "")
}

func (s *Server) updateSessionRule(writer http.ResponseWriter, request *http.Request) {
	s.saveSessionRule(writer, request, request.PathValue("source"))
}

func (s *Server) saveSessionRule(writer http.ResponseWriter, request *http.Request, pathSource string) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload sessionRulePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	source := strings.TrimSpace(payload.Source)
	if pathSource != "" {
		if source != "" && source != pathSource {
			writeError(writer, fmt.Errorf("%w: 路径中的会话来源与请求体不一致", sessionrule.ErrInvalidRequest))
			return
		}
		source = pathSource
	}
	// 新建规则显式使用空覆盖掩码；这样后续可以按单个规则项清除，而不是
	// 把所有默认值误当成用户覆盖。
	item := sessionrule.Rule{Source: source, ProcessEnabled: true, LLMEnabled: true, TTSEnabled: false, FollowProfile: true, KnowledgeTopK: 5, ConfiguredFields: []string{}}
	if pathSource != "" {
		item, err = service.Get(request.Context(), pathSource)
		if err != nil {
			writeError(writer, err)
			return
		}
	}
	mergeSessionRulePayload(&item, payload)
	saved, err := service.Save(request.Context(), item)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if pathSource == "" {
		status = http.StatusCreated
	}
	writeJSON(writer, status, saved)
}

func (s *Server) getSessionRule(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := service.Get(request.Context(), request.PathValue("source"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) deleteSessionRule(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.Delete(request.Context(), request.PathValue("source")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// resetSessionRuleField 清除指定来源的单个覆盖项；它不会删除消息来源目录，
// 只会在最后一个覆盖项也被清除时删除规则行。
func (s *Server) resetSessionRuleField(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if err := service.ResetField(request.Context(), request.PathValue("source"), payload.Key); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) batchSessionRules(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload sessionrule.BatchUpdate
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	items, err := service.ApplyBatch(request.Context(), payload)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"rules": items, "updated": len(items)})
}

type sessionRuleGroupPayload struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Members     []string `json:"members"`
}

func (s *Server) listSessionRuleGroups(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := service.ListGroups(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"groups": items})
}

func (s *Server) createSessionRuleGroup(writer http.ResponseWriter, request *http.Request) {
	s.saveSessionRuleGroup(writer, request, "")
}

func (s *Server) updateSessionRuleGroup(writer http.ResponseWriter, request *http.Request) {
	s.saveSessionRuleGroup(writer, request, request.PathValue("id"))
}

func (s *Server) saveSessionRuleGroup(writer http.ResponseWriter, request *http.Request, pathID string) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload sessionRuleGroupPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		payload.ID = pathID
	}
	item, err := service.SaveGroup(request.Context(), sessionrule.Group{ID: payload.ID, Name: payload.Name, Description: payload.Description, Members: payload.Members})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) deleteSessionRuleGroup(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireSessionRules()
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := service.DeleteGroup(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func mergeSessionRulePayload(item *sessionrule.Rule, payload sessionRulePayload) {
	if payload.Source != "" {
		item.Source = strings.TrimSpace(payload.Source)
	}
	if payload.ProcessEnabled != nil {
		item.ProcessEnabled = *payload.ProcessEnabled
		item.MarkOverride("process_enabled")
	}
	if payload.LLMEnabled != nil {
		item.LLMEnabled = *payload.LLMEnabled
		item.MarkOverride("llm_enabled")
	}
	if payload.TTSEnabled != nil {
		item.TTSEnabled = *payload.TTSEnabled
		item.MarkOverride("tts_enabled")
	}
	if payload.Note != nil {
		item.Note = *payload.Note
		item.MarkOverride("note")
	}
	if payload.ChatModel != nil {
		item.ChatModel = *payload.ChatModel
		item.MarkOverride("chat_model")
	}
	if payload.STTModel != nil {
		item.STTModel = *payload.STTModel
		item.MarkOverride("stt_model")
	}
	if payload.TTSModel != nil {
		item.TTSModel = *payload.TTSModel
		item.MarkOverride("tts_model")
	}
	if payload.FollowProfile != nil {
		item.FollowProfile = *payload.FollowProfile
		item.MarkOverride("follow_profile")
	}
	if payload.ProfileID != nil {
		item.ProfileID = *payload.ProfileID
		item.MarkOverride("profile_id")
	}
	if payload.PersonaID != nil {
		item.PersonaID = *payload.PersonaID
		item.MarkOverride("persona_id")
	}
	if payload.DisabledPlugins != nil {
		item.DisabledPlugins = *payload.DisabledPlugins
		item.MarkOverride("disabled_plugins")
	}
	if payload.KnowledgeBases != nil {
		item.KnowledgeBases = *payload.KnowledgeBases
		item.MarkOverride("knowledge_bases")
	}
	if payload.KnowledgeTopK != nil {
		item.KnowledgeTopK = *payload.KnowledgeTopK
		item.MarkOverride("knowledge_top_k")
	}
	if payload.KnowledgeRerank != nil {
		item.KnowledgeRerank = *payload.KnowledgeRerank
		item.MarkOverride("knowledge_rerank")
	}
	if payload.PrivateMode != nil {
		item.PrivateMode = *payload.PrivateMode
		item.MarkOverride(sessionrule.OverridePrivateMode)
	}
	if payload.GroupParticipationMode != nil {
		item.GroupParticipationMode = *payload.GroupParticipationMode
		item.MarkOverride(sessionrule.OverrideGroupParticipationMode)
	}
	if payload.RecordUnaddressedMessages != nil {
		item.RecordUnaddressedMessages = *payload.RecordUnaddressedMessages
		item.MarkOverride(sessionrule.OverrideRecordUnaddressedMessages)
	}
	if payload.ReactionEnabled != nil {
		item.ReactionEnabled = *payload.ReactionEnabled
		item.MarkOverride(sessionrule.OverrideReactionEnabled)
	}
	if payload.GroupContextEnabled != nil {
		item.GroupContextEnabled = *payload.GroupContextEnabled
		item.MarkOverride(sessionrule.OverrideGroupContextEnabled)
	}
	if payload.ProactiveEnabled != nil {
		item.ProactiveEnabled = *payload.ProactiveEnabled
		item.MarkOverride(sessionrule.OverrideProactiveEnabled)
	}
	if payload.PrivateHourlyReplyLimit != nil {
		item.PrivateHourlyReplyLimit = *payload.PrivateHourlyReplyLimit
		item.MarkOverride(sessionrule.OverridePrivateHourlyReplyLimit)
	}
	if payload.GroupHourlyReplyLimit != nil {
		item.GroupHourlyReplyLimit = *payload.GroupHourlyReplyLimit
		item.MarkOverride(sessionrule.OverrideGroupHourlyReplyLimit)
	}
	if payload.QuietHoursTimezone != nil {
		item.QuietHoursTimezone = *payload.QuietHoursTimezone
		item.MarkOverride(sessionrule.OverrideQuietHoursTimezone)
	}
	if payload.QuietHoursStart != nil {
		item.QuietHoursStart = *payload.QuietHoursStart
		item.MarkOverride(sessionrule.OverrideQuietHoursStart)
	}
	if payload.QuietHoursEnd != nil {
		item.QuietHoursEnd = *payload.QuietHoursEnd
		item.MarkOverride(sessionrule.OverrideQuietHoursEnd)
	}
	if payload.EmergencyBypassQuietHours != nil {
		item.EmergencyBypassQuietHours = *payload.EmergencyBypassQuietHours
		item.MarkOverride(sessionrule.OverrideEmergencyBypassQuietHours)
	}
	if payload.ReplyQuote != nil {
		item.ReplyQuote = *payload.ReplyQuote
		item.MarkOverride(sessionrule.OverrideReplyQuote)
	}
	if payload.PrivateReplyQuote != nil {
		item.PrivateReplyQuote = *payload.PrivateReplyQuote
		item.MarkOverride(sessionrule.OverridePrivateReplyQuote)
	}
	if payload.FollowUpEnabled != nil {
		item.FollowUpEnabled = *payload.FollowUpEnabled
		item.MarkOverride(sessionrule.OverrideFollowUpEnabled)
	}
	if payload.RelationEnabled != nil {
		item.RelationEnabled = *payload.RelationEnabled
		item.MarkOverride(sessionrule.OverrideRelationEnabled)
	}
	if payload.RuntimeEnabled != nil {
		item.RuntimeEnabled = *payload.RuntimeEnabled
		item.MarkOverride(sessionrule.OverrideRuntimeEnabled)
	}
	if payload.RuntimeMaxConcurrency != nil {
		item.RuntimeMaxConcurrency = *payload.RuntimeMaxConcurrency
		item.MarkOverride(sessionrule.OverrideRuntimeMaxConcurrency)
	}
	if payload.SourceQueueLimit != nil {
		item.SourceQueueLimit = *payload.SourceQueueLimit
		item.MarkOverride(sessionrule.OverrideSourceQueueLimit)
	}
	if payload.TurnWaitMilliseconds != nil {
		item.TurnWaitMilliseconds = *payload.TurnWaitMilliseconds
		item.MarkOverride(sessionrule.OverrideTurnWaitMilliseconds)
	}
	if payload.GroupTurnWaitMilliseconds != nil {
		item.GroupTurnWaitMilliseconds = *payload.GroupTurnWaitMilliseconds
		item.MarkOverride(sessionrule.OverrideGroupTurnWaitMilliseconds)
	}
	if payload.AttachmentWaitMilliseconds != nil {
		item.AttachmentWaitMilliseconds = *payload.AttachmentWaitMilliseconds
		item.MarkOverride(sessionrule.OverrideAttachmentWaitMilliseconds)
	}
	if payload.MaxTurnMessages != nil {
		item.MaxTurnMessages = *payload.MaxTurnMessages
		item.MarkOverride(sessionrule.OverrideMaxTurnMessages)
	}
	if payload.GroupMessageMaxCount != nil {
		item.GroupMessageMaxCount = *payload.GroupMessageMaxCount
		item.MarkOverride(sessionrule.OverrideGroupMessageMaxCount)
	}
	if payload.GroupImageCaption != nil {
		item.GroupImageCaption = *payload.GroupImageCaption
		item.MarkOverride(sessionrule.OverrideGroupImageCaption)
	}
	if payload.GroupImageCaptionModel != nil {
		item.GroupImageCaptionModel = *payload.GroupImageCaptionModel
		item.MarkOverride(sessionrule.OverrideGroupImageCaptionModel)
	}
	if payload.ProactiveDegree != nil {
		item.ProactiveDegree = *payload.ProactiveDegree
		item.MarkOverride(sessionrule.OverrideProactiveDegree)
	}
	if payload.CooldownSeconds != nil {
		item.CooldownSeconds = *payload.CooldownSeconds
		item.MarkOverride(sessionrule.OverrideCooldownSeconds)
	}
	if payload.HeartbeatSeconds != nil {
		item.HeartbeatSeconds = *payload.HeartbeatSeconds
		item.MarkOverride(sessionrule.OverrideHeartbeatSeconds)
	}
	if payload.RelationRetentionSeconds != nil {
		item.RelationRetentionSeconds = *payload.RelationRetentionSeconds
		item.MarkOverride(sessionrule.OverrideRelationRetentionSeconds)
	}
	if payload.FollowUpMax != nil {
		item.FollowUpMax = *payload.FollowUpMax
		item.MarkOverride(sessionrule.OverrideFollowUpMax)
	}
	if payload.FollowUpMaxRetries != nil {
		item.FollowUpMaxRetries = *payload.FollowUpMaxRetries
		item.MarkOverride(sessionrule.OverrideFollowUpMaxRetries)
	}
	if payload.FollowUpRetryDelaySeconds != nil {
		item.FollowUpRetryDelaySeconds = *payload.FollowUpRetryDelaySeconds
		item.MarkOverride(sessionrule.OverrideFollowUpRetryDelaySeconds)
	}
	if payload.FollowUpMaxDelaySeconds != nil {
		item.FollowUpMaxDelaySeconds = *payload.FollowUpMaxDelaySeconds
		item.MarkOverride(sessionrule.OverrideFollowUpMaxDelaySeconds)
	}
	if payload.FollowUpAllowedSources != nil {
		item.FollowUpAllowedSources = *payload.FollowUpAllowedSources
		item.MarkOverride(sessionrule.OverrideFollowUpAllowedSources)
	}
	if payload.ExpressionEnabled != nil {
		item.ExpressionEnabled = *payload.ExpressionEnabled
		item.MarkOverride(sessionrule.OverrideExpressionEnabled)
	}
	if payload.ExpressionMaxSegments != nil {
		item.ExpressionMaxSegments = *payload.ExpressionMaxSegments
		item.MarkOverride(sessionrule.OverrideExpressionMaxSegments)
	}
	if payload.ExpressionLongThreshold != nil {
		item.ExpressionLongThreshold = *payload.ExpressionLongThreshold
		item.MarkOverride(sessionrule.OverrideExpressionLongThreshold)
	}
	if payload.ExpressionDelayMilliseconds != nil {
		item.ExpressionDelayMilliseconds = *payload.ExpressionDelayMilliseconds
		item.MarkOverride(sessionrule.OverrideExpressionDelayMilliseconds)
	}
	if payload.ReplyMention != nil {
		item.ReplyMention = *payload.ReplyMention
		item.MarkOverride(sessionrule.OverrideReplyMention)
	}
	if payload.AgentOnDemandEnabled != nil {
		item.AgentOnDemandEnabled = *payload.AgentOnDemandEnabled
		item.MarkOverride(sessionrule.OverrideAgentOnDemandEnabled)
	}
	if payload.AllowedReadOnlyTools != nil {
		item.AllowedReadOnlyTools = *payload.AllowedReadOnlyTools
		item.MarkOverride(sessionrule.OverrideAllowedReadOnlyTools)
	}
	if payload.ToolBudget != nil {
		item.ToolBudget = *payload.ToolBudget
		item.MarkOverride(sessionrule.OverrideToolBudget)
	}
	if payload.SubAgentEnabled != nil {
		item.SubAgentEnabled = *payload.SubAgentEnabled
		item.MarkOverride(sessionrule.OverrideSubAgentEnabled)
	}
	if payload.ActionPermissions != nil {
		item.ActionPermissions = *payload.ActionPermissions
		item.MarkOverride(sessionrule.OverrideActionPermissions)
	}
	if payload.MessageStyle != nil {
		item.MessageStyle = *payload.MessageStyle
		item.MarkOverride(sessionrule.OverrideMessageStyle)
	}
}
