package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// RuntimeFollowUpCreator 是动作层创建持久 Follow-up 的宿主回调；Bot 包只依赖
// 抽象回调，避免把调度器实现和平台事件入口耦合在一起。
type RuntimeFollowUpCreator func(context.Context, PlatformAction) (string, error)

// SetRuntimeFollowUpCreator 安装 Follow-up 创建器。未安装时，模型或嵌入方请求
// create_follow_up 会得到明确错误，不会伪造一个已经创建的任务。
func (m *Manager) SetRuntimeFollowUpCreator(creator RuntimeFollowUpCreator) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.runtimeFollowUpCreator = creator
	m.mu.Unlock()
}

// DispatchAction 是 Bot Runtime 的统一动作入口。它先校验权限、能力和幂等键，
// 再把动作计划写入持久层，最后才调用平台或状态仓储执行副作用。
func (m *Manager) DispatchAction(ctx context.Context, action PlatformAction) (PlatformActionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	name := strings.TrimSpace(action.Type)
	if name == "" {
		return PlatformActionResult{}, errors.New("Bot Runtime 动作类型不能为空")
	}
	action.Type = name
	message := action.Message
	if strings.TrimSpace(message.AdapterID) == "" {
		return PlatformActionResult{}, errors.New("Bot Runtime 动作缺少 adapter_id")
	}
	if !m.runtimeActionAllowed(ctx, message, name) {
		return PlatformActionResult{}, fmt.Errorf("Bot Runtime 动作 %q 未获允许", name)
	}

	summary := action.Text
	if summary == "" {
		encoded, marshalErr := json.Marshal(map[string]any{"target_id": action.TargetID, "emoji": action.Emoji, "attachment": action.Attachment, "payload": action.Payload})
		if marshalErr == nil {
			summary = string(encoded)
		}
	}
	idempotencyKey := runtimeActionIdempotencyKey(action)
	if name == "create_follow_up" {
		// Follow-up 会跨进程存活；把动作幂等键显式传给调度器，重试时
		// 更新同一条任务而不是再次创建一条未来任务。
		if action.Payload == nil {
			action.Payload = make(map[string]any)
		}
		if strings.TrimSpace(runtimeStringValue(action.Payload, "idempotency_key")) == "" {
			action.Payload["idempotency_key"] = idempotencyKey
		}
	}
	stored, execute, prepareErr := m.prepareRuntimeAction(ctx, message, name, idempotencyKey, summary)
	if prepareErr != nil {
		return PlatformActionResult{}, fmt.Errorf("保存 Bot Runtime 动作失败: %w", prepareErr)
	}
	if !execute {
		return PlatformActionResult{Raw: stored.PlatformResult}, nil
	}

	result, executeErr := m.executeRuntimeAction(ctx, action)
	if executeErr != nil {
		m.finishRuntimeAction(ctx, stored, "failed", executeErr.Error())
		slog.Error("Bot Runtime 动作执行失败", "adapter_id", message.AdapterID, "action", name, "source", messageSource(message), "error", executeErr)
		return PlatformActionResult{}, executeErr
	}
	encodedResult, marshalErr := json.Marshal(result)
	resultText := result.Raw
	if marshalErr == nil {
		resultText = string(encodedResult)
	}
	m.finishRuntimeAction(ctx, stored, "completed", resultText)
	m.updateInstanceState(ctx, message.AdapterID, "online", "available", false, true)
	return result, nil
}

func runtimeActionIdempotencyKey(action PlatformAction) string {
	if action.Payload != nil {
		if value, ok := action.Payload["idempotency_key"].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	attachmentName := ""
	if action.Attachment != nil {
		attachmentName = action.Attachment.Name + ":" + action.Attachment.MIMEType
		if action.Attachment.Ref != nil {
			attachmentName += ":" + action.Attachment.Ref.ID + ":" + fmt.Sprint(action.Attachment.Ref.Version)
		}
	}
	return newRuntimeID("runtime-action", action.Message.AdapterID, string(action.Message.Platform), messageSource(action.Message), action.Message.ID, action.Message.RuntimeTurnID, action.Type, action.TargetID, action.Emoji, attachmentName, action.Text)
}

// executeRuntimeAction 只负责执行已经持久化的动作；调用方不能通过它绕过
// DispatchAction 的权限、能力和幂等校验。
func (m *Manager) executeRuntimeAction(ctx context.Context, action PlatformAction) (PlatformActionResult, error) {
	name := strings.TrimSpace(action.Type)
	switch name {
	case "update_relation":
		return m.executeRelationAction(ctx, action)
	case "update_source_state":
		return m.executeSourceAction(ctx, action)
	case "create_follow_up":
		m.mu.RLock()
		creator := m.runtimeFollowUpCreator
		m.mu.RUnlock()
		if creator == nil {
			return PlatformActionResult{}, errors.New("Follow-up 创建服务未装配")
		}
		id, err := creator(ctx, action)
		if err != nil {
			return PlatformActionResult{}, err
		}
		return PlatformActionResult{Raw: id}, nil
	case "start_agent":
		return PlatformActionResult{}, errors.New("start_agent 必须通过自然话轮入口执行")
	}

	m.mu.RLock()
	entry, ok := m.runtimes[action.Message.AdapterID]
	m.mu.RUnlock()
	if !ok {
		return PlatformActionResult{}, ErrNotRunning
	}
	if err := validatePlatformActionCapability(entry.platform, action); err != nil {
		return PlatformActionResult{}, err
	}
	slog.Info("Bot Runtime 平台动作请求", "adapter_id", action.Message.AdapterID, "action", action)
	return entry.platform.DispatchAction(ctx, action)
}

func validatePlatformActionCapability(adapter platform, action PlatformAction) error {
	// platform 接口强制提供统一能力描述；动作层不能因为适配器没有旧式
	// 能力扩展就跳过检查，否则会把不支持的媒体或轻动作伪装成成功。
	caps := adapter.Capabilities()
	capable := true
	switch strings.TrimSpace(action.Type) {
	case "send_text":
		capable = caps.PlainText
	case "send_image":
		capable = caps.Image
	case "send_audio":
		capable = caps.Audio
	case "send_file":
		capable = caps.File
	case "add_reaction":
		capable = caps.Reaction
	case "poke":
		capable = caps.Poke
	case "recall_message":
		capable = caps.Recall
	}
	if !capable {
		return fmt.Errorf("平台不支持抽象动作 %q", action.Type)
	}
	return nil
}

func (m *Manager) executeRelationAction(ctx context.Context, action PlatformAction) (PlatformActionResult, error) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return PlatformActionResult{}, errors.New("Bot Runtime 状态仓储未装配")
	}
	value := action.Payload
	scopeType, _ := value["scope_type"].(string)
	scopeID, _ := value["scope_id"].(string)
	if strings.TrimSpace(scopeType) == "" {
		scopeType = "user"
	}
	if strings.TrimSpace(scopeID) == "" {
		scopeID = action.Message.UserID
	}
	state := RelationState{BotID: action.Message.AdapterID, ScopeType: strings.TrimSpace(scopeType), ScopeID: strings.TrimSpace(scopeID), PreferredName: runtimeStringValue(value, "preferred_name"), StablePreferences: runtimeStringValue(value, "stable_preferences"), InteractionStyle: runtimeStringValue(value, "interaction_style"), TrustLevel: runtimeStringValue(value, "trust_level"), RecentTopics: runtimeStringValue(value, "recent_topics"), Commitments: runtimeStringValue(value, "commitments"), LastInteractionAt: time.Now().UTC()}
	if err := repository.TouchRelation(ctx, state); err != nil {
		return PlatformActionResult{}, err
	}
	return PlatformActionResult{Raw: "relation_updated"}, nil
}

func (m *Manager) executeSourceAction(ctx context.Context, action PlatformAction) (PlatformActionResult, error) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return PlatformActionResult{}, errors.New("Bot Runtime 状态仓储未装配")
	}
	source := messageSource(action.Message)
	if configured, ok := action.Payload["source"].(string); ok && strings.TrimSpace(configured) != "" {
		source = strings.TrimSpace(configured)
	}
	now := time.Now().UTC()
	state := SourceState{BotID: action.Message.AdapterID, Source: source, ChatType: action.Message.ChatType, LastUserID: action.Message.UserID, LastSeenAt: now}
	if silence, ok := action.Payload["silence_until"].(string); ok && strings.TrimSpace(silence) != "" {
		if parsed, err := time.Parse(time.RFC3339, silence); err == nil {
			state.SilenceUntil = &parsed
		}
	}
	if err := repository.TouchSource(ctx, state); err != nil {
		return PlatformActionResult{}, err
	}
	return PlatformActionResult{Raw: "source_updated"}, nil
}

func runtimeStringValue(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}
