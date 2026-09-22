package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/bot"
	configsvc "Abot/internal/config"
	"Abot/internal/conversation"
	"Abot/internal/provider"
	"Abot/internal/webui"
	"Abot/internal/workspace"
)

// commandRuntimeBridge adapts the existing application services to the narrow
// interfaces chat commands use to read and change configuration. Keeping the
// bridge here means the bot package never learns about provider or persona
// internals.
type commandRuntimeBridge struct {
	providers     *provider.Registry
	config        *configsvc.Service
	personas      *configsvc.PersonaService
	workspaces    *workspace.Service
	conversations *conversation.Service
	runtime       *agentruntime.Coordinator
}

var (
	_ bot.CommandRuntimeInfo          = (*commandRuntimeBridge)(nil)
	_ bot.CommandRuntimeSubAgentInfo  = (*commandRuntimeBridge)(nil)
	_ bot.CommandRuntimeAdmin         = (*commandRuntimeBridge)(nil)
	_ bot.CommandRuntimeSubAgentAdmin = (*commandRuntimeBridge)(nil)
	_ bot.CommandRuntimeStats         = (*commandRuntimeBridge)(nil)
	_ bot.CommandDashboardUpdater     = (*commandRuntimeBridge)(nil)
)

// CurrentModel reports the model that is actually in effect for the chat, which
// already accounts for conversation and bot level configuration bindings.
func (b *commandRuntimeBridge) CurrentModel(ctx context.Context, botID, userID, conversationID string) (string, string, error) {
	// A per-conversation override is what the chat is actually running, so it
	// is reported ahead of the resolved default.
	if item, err := b.conversations.Get(ctx, userID, conversationID); err == nil {
		if modelID := strings.TrimSpace(item.ModelID); modelID != "" {
			return strings.TrimSpace(item.ProviderID), modelID, nil
		}
	}
	runtime, err := b.config.Resolve(ctx, botID, conversationID)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(runtime.ProviderID), strings.TrimSpace(runtime.ModelID), nil
}

func (b *commandRuntimeBridge) CurrentPersona(ctx context.Context, botID, _ string, conversationID string) (string, string, error) {
	persona, selected, err := b.personas.ResolveSelection(ctx, botID, conversationID, "")
	if err != nil {
		return "", "", err
	}
	if !selected {
		return "", "", nil
	}
	return persona.ID, persona.Name, nil
}

// CurrentWorkspace resolves the workspace bound to the conversation. The path
// itself never leaves this boundary: only the id and display name are returned.
func (b *commandRuntimeBridge) CurrentWorkspace(ctx context.Context, userID, conversationID string) (string, string, error) {
	item, err := b.conversations.Get(ctx, userID, conversationID)
	if err != nil {
		return "", "", err
	}
	workspaceID := strings.TrimSpace(item.WorkspaceID)
	if workspaceID == "" {
		return "", "", nil
	}
	name := ""
	if resolved, getErr := b.workspaces.Get(ctx, workspaceID); getErr == nil {
		name = resolved.Name
	}
	return workspaceID, name, nil
}

// SubAgentStatus returns both the effective value and the source of the value,
// so chat commands can distinguish an explicit session override from the
// global system default.
func (b *commandRuntimeBridge) SubAgentStatus(ctx context.Context, _, userID, conversationID string) (bool, *bool, bool, error) {
	settings, err := b.config.GetSystemSettings(ctx)
	if err != nil {
		return false, nil, false, err
	}
	defaultEnabled := settings.IsSubAgentEnabled()
	if item, getErr := b.conversations.Get(ctx, userID, conversationID); getErr == nil {
		if item.SubAgentEnabled != nil {
			override := *item.SubAgentEnabled
			return override, &override, defaultEnabled, nil
		}
	}
	return defaultEnabled, nil, defaultEnabled, nil
}

func (b *commandRuntimeBridge) AvailableModels(context.Context) ([]bot.CommandOption, error) {
	result := make([]bot.CommandOption, 0)
	for _, item := range b.providers.List() {
		for _, model := range item.Models {
			if !model.Enabled {
				continue
			}
			result = append(result, bot.CommandOption{ID: model.ID, Name: model.ID})
		}
	}
	return result, nil
}

func (b *commandRuntimeBridge) AvailablePersonas(ctx context.Context) ([]bot.CommandOption, error) {
	personas, err := b.personas.ListPersonas(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]bot.CommandOption, 0, len(personas))
	for _, item := range personas {
		if !item.Enabled {
			continue
		}
		result = append(result, bot.CommandOption{ID: item.ID, Name: item.Name})
	}
	return result, nil
}

func (b *commandRuntimeBridge) AvailableWorkspaces(ctx context.Context) ([]bot.CommandOption, error) {
	items, err := b.workspaces.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]bot.CommandOption, 0, len(items))
	for _, item := range items {
		if !item.Enabled {
			continue
		}
		result = append(result, bot.CommandOption{ID: item.ID, Name: item.Name})
	}
	return result, nil
}

// SetPersona binds a persona to the conversation. It reuses the same binding
// the WebUI writes, so both surfaces observe one source of truth.
func (b *commandRuntimeBridge) SetPersona(ctx context.Context, _, _, conversationID, personaID string) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("会话尚未建立")
	}
	return b.personas.Bind(ctx, configsvc.BindingConversation, conversationID, strings.TrimSpace(personaID))
}

// SetModel stores a per-conversation model override. The provider is resolved
// from the model catalog so a caller cannot pair a model with a wrong provider.
func (b *commandRuntimeBridge) SetModel(ctx context.Context, _, userID, conversationID, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return errors.New("模型不能为空")
	}
	providerID, err := b.providerForModel(modelID)
	if err != nil {
		return err
	}
	_, err = b.conversations.SetRuntimeOverride(ctx, userID, conversationID, providerID, modelID)
	return err
}

// SetWorkspace binds a workspace to the conversation. Validation happens inside
// the conversation service so both the chat and the WebUI share one rule.
func (b *commandRuntimeBridge) SetWorkspace(ctx context.Context, userID, conversationID, workspaceID string) error {
	_, err := b.conversations.SetWorkspace(ctx, userID, conversationID, strings.TrimSpace(workspaceID))
	return err
}

func (b *commandRuntimeBridge) SetSubAgentEnabled(ctx context.Context, _, userID, conversationID string, enabled *bool) error {
	_, err := b.conversations.SetSubAgentEnabled(ctx, userID, conversationID, enabled)
	return err
}

// ConversationUsage 聚合当前会话的所有 Runtime invocation，保持 /stats 与管理台用量投影使用同一份事实源。
func (b *commandRuntimeBridge) ConversationUsage(ctx context.Context, userID, conversationID string) (bot.CommandUsageStats, error) {
	if b.runtime == nil {
		return bot.CommandUsageStats{}, errors.New("Runtime 尚未装配")
	}
	items, err := b.runtime.ListInvocations(ctx, strings.TrimSpace(userID), nil)
	if err != nil {
		return bot.CommandUsageStats{}, err
	}
	stats := bot.CommandUsageStats{}
	initializeCommandUsageValues(&stats)
	for _, item := range items {
		if item.ConversationID != strings.TrimSpace(conversationID) {
			continue
		}
		stats.InvocationCount++
		usage, usageErr := b.runtime.GetInvocationUsage(ctx, item.ID)
		if usageErr != nil {
			return bot.CommandUsageStats{}, usageErr
		}
		mergeCommandUsage(&stats, usage)
	}
	return stats, nil
}

// initializeCommandUsageValues 把“尚未聚合”和“已知为零”区分开，避免空会话被误报成未知。
func initializeCommandUsageValues(stats *bot.CommandUsageStats) {
	stats.TotalTokens.Known = true
	stats.PromptTokens.Known = true
	stats.CachedInputTokens.Known = true
	stats.ToolUsePromptTokens.Known = true
	stats.OutputTokens.Known = true
	stats.ReasoningTokens.Known = true
}

// mergeCommandUsage 合并普通模型、上下文压缩和模态回退的持久化用量。
func mergeCommandUsage(stats *bot.CommandUsageStats, usage agentruntime.UsageSummary) {
	mergeUsageTotals(stats, usage.ModelCalls, usage.UnknownCalls, usage.PromptTokens, usage.CachedInputTokens, usage.ToolUsePromptTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens)
	stats.CompactionModelCalls += usage.Compaction.ModelCalls
	mergeUsageTotals(stats, usage.Compaction.ModelCalls, usage.Compaction.UnknownCalls, usage.Compaction.PromptTokens, usage.Compaction.CachedInputTokens, usage.Compaction.ToolUsePromptTokens, usage.Compaction.OutputTokens, usage.Compaction.ReasoningTokens, usage.Compaction.TotalTokens)
	mergeUsageTotals(stats, usage.ModalFallback.ModelCalls, usage.ModalFallback.UnknownCalls, usage.ModalFallback.PromptTokens, usage.ModalFallback.CachedInputTokens, usage.ModalFallback.ToolUsePromptTokens, usage.ModalFallback.OutputTokens, usage.ModalFallback.ReasoningTokens, usage.ModalFallback.TotalTokens)
}

// mergeUsageTotals 只把有模型调用的分类计入总数，避免空分类的 Known=false 污染真实用量。
func mergeUsageTotals(stats *bot.CommandUsageStats, modelCalls, unknownCalls int, prompt, cached, toolPrompt, output, reasoning, total agentruntime.UsageValue) {
	if modelCalls <= 0 {
		return
	}
	stats.ModelCalls += modelCalls
	stats.UnknownCalls += unknownCalls
	mergeCommandUsageValue(&stats.PromptTokens, prompt)
	mergeCommandUsageValue(&stats.CachedInputTokens, cached)
	mergeCommandUsageValue(&stats.ToolUsePromptTokens, toolPrompt)
	mergeCommandUsageValue(&stats.OutputTokens, output)
	mergeCommandUsageValue(&stats.ReasoningTokens, reasoning)
	mergeCommandUsageValue(&stats.TotalTokens, total)
}

// mergeCommandUsageValue 在同一字段任一调用未知时保留未知状态，不把缺失数据假装成零。
func mergeCommandUsageValue(target *bot.CommandUsageValue, value agentruntime.UsageValue) {
	if !value.Known {
		target.Known = false
		return
	}
	if target.Known {
		target.Value += value.Value
	}
}

// UpdateDashboard 检查内嵌静态资源是否完整；Abot 不在运行时下载或替换前端文件。
func (b *commandRuntimeBridge) UpdateDashboard(ctx context.Context) (bot.DashboardUpdateResult, error) {
	if err := ctx.Err(); err != nil {
		return bot.DashboardUpdateResult{}, err
	}
	if err := webui.CheckEmbeddedDashboard(); err != nil {
		return bot.DashboardUpdateResult{}, err
	}
	return bot.DashboardUpdateResult{
		Updated: false,
		Message: "管理台资源检查完成：当前 WebUI 随 Abot 二进制内嵌，当前进程无需在线下载或替换；升级 Abot 后重启即可生效。",
	}, nil
}

// providerForModel finds the provider that owns a model id.
func (b *commandRuntimeBridge) providerForModel(modelID string) (string, error) {
	for _, item := range b.providers.List() {
		for _, model := range item.Models {
			if model.Enabled && model.ID == modelID {
				return item.ID, nil
			}
		}
	}
	return "", fmt.Errorf("没有供应商提供模型 %s", modelID)
}
