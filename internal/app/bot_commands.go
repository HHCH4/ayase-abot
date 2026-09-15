package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"Abot/internal/bot"
	configsvc "Abot/internal/config"
	"Abot/internal/conversation"
	"Abot/internal/provider"
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
}

var (
	_ bot.CommandRuntimeInfo  = (*commandRuntimeBridge)(nil)
	_ bot.CommandRuntimeAdmin = (*commandRuntimeBridge)(nil)
)

// CurrentModel reports the model that is actually in effect for the chat, which
// already accounts for conversation and bot level configuration bindings.
func (b *commandRuntimeBridge) CurrentModel(ctx context.Context, botID, _ string, conversationID string) (string, string, error) {
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

// SetModel and SetWorkspace report that chat-side switching is not available in
// this build rather than pretending to succeed. Both remain WebUI operations
// until the conversation layer can persist a per-conversation override.
func (b *commandRuntimeBridge) SetModel(context.Context, string, string, string, string) error {
	return fmt.Errorf("当前版本请在 WebUI 的任务配置中切换模型")
}

func (b *commandRuntimeBridge) SetWorkspace(context.Context, string, string, string) error {
	return fmt.Errorf("当前版本请在 WebUI 中为任务绑定工作区")
}
