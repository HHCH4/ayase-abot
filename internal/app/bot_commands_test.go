package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"Abot/internal/bot"
	configsvc "Abot/internal/config"
	"Abot/internal/conversation"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
	"Abot/internal/workspace"
)

// newCommandBridgeFixture builds the bridge over real services so the
// conversation-level override path is exercised end to end instead of mocked.
func newCommandBridgeFixture(t *testing.T) (*commandRuntimeBridge, *conversation.Service) {
	t.Helper()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), nil)
	if err != nil {
		t.Fatal(err)
	}
	apiKey := "secret"
	if _, err := registry.Save(context.Background(), provider.SaveRequest{
		Provider: provider.Provider{
			ID: "provider-a", Name: "A", Protocol: provider.ProtocolOpenAICompatible,
			BaseURL: "https://example.invalid/v1",
			Models: []provider.Model{
				{ID: "model-a", DisplayName: "model-a", Enabled: true},
				{ID: "model-b", DisplayName: "model-b", Enabled: true},
			},
		},
		APIKey: &apiKey,
	}); err != nil {
		t.Fatal(err)
	}

	conversationService, err := conversation.NewService(store.ConversationRepository(), store.SessionService(), "abot")
	if err != nil {
		t.Fatal(err)
	}
	workspaceService, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceService.Save(context.Background(), workspace.SaveRequest{Workspace: workspace.Workspace{
		ID: "project-a", Name: "项目 A", Type: workspace.TypeLocal, RootPath: t.TempDir(), Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	conversationService.SetWorkspaceValidator(func(ctx context.Context, workspaceID string) error {
		item, getErr := workspaceService.Get(ctx, workspaceID)
		if getErr != nil {
			return getErr
		}
		if !item.Enabled {
			return errors.New("工作区已停用")
		}
		return nil
	})

	personaService, err := configsvc.NewPersonaService(store.PersonaRepository())
	if err != nil {
		t.Fatal(err)
	}
	configService, err := configsvc.NewService(store.ConfigRepository(), func(context.Context, string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := configService.EnsureDefault(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridge := &commandRuntimeBridge{
		providers: registry, config: configService, personas: personaService,
		workspaces: workspaceService, conversations: conversationService,
	}
	return bridge, conversationService
}

func TestCommandBridgeSwitchesModelPerConversation(t *testing.T) {
	bridge, conversations := newCommandBridgeFixture(t)
	ctx := context.Background()
	item, err := conversations.Create(ctx, conversation.CreateRequest{UserID: "user-1", Title: "会话"})
	if err != nil {
		t.Fatal(err)
	}

	// 未设置覆盖时，读取回落到配置解析结果（此处没有绑定，因此为空）。
	if _, modelID, err := bridge.CurrentModel(ctx, "bot-1", "user-1", item.ID); err != nil || modelID != "" {
		t.Fatalf("初始模型应为空: %q err=%v", modelID, err)
	}

	options, err := bridge.AvailableModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 2 {
		t.Fatalf("可用模型应为两个: %+v", options)
	}

	if err := bridge.SetModel(ctx, "bot-1", "user-1", item.ID, "model-b"); err != nil {
		t.Fatal(err)
	}
	providerID, modelID, err := bridge.CurrentModel(ctx, "bot-1", "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if providerID != "provider-a" || modelID != "model-b" {
		t.Fatalf("会话覆盖未生效: provider=%q model=%q", providerID, modelID)
	}
	// 覆盖只影响这个会话。
	other, err := conversations.Create(ctx, conversation.CreateRequest{UserID: "user-1", Title: "另一个会话"})
	if err != nil {
		t.Fatal(err)
	}
	if _, otherModel, err := bridge.CurrentModel(ctx, "bot-1", "user-1", other.ID); err != nil || otherModel != "" {
		t.Fatalf("覆盖不应影响其他会话: %q err=%v", otherModel, err)
	}

	// 目录外的模型必须被拒绝，而不是写入一条无法解析的覆盖。
	if err := bridge.SetModel(ctx, "bot-1", "user-1", item.ID, "nope"); err == nil || !strings.Contains(err.Error(), "没有供应商提供模型") {
		t.Fatalf("未知模型必须被拒绝, got %v", err)
	}
}

func TestCommandBridgeBindsWorkspaceThroughValidator(t *testing.T) {
	bridge, conversations := newCommandBridgeFixture(t)
	ctx := context.Background()
	item, err := conversations.Create(ctx, conversation.CreateRequest{UserID: "user-1", Title: "会话"})
	if err != nil {
		t.Fatal(err)
	}

	if err := bridge.SetWorkspace(ctx, "user-1", item.ID, "project-a"); err != nil {
		t.Fatal(err)
	}
	workspaceID, name, err := bridge.CurrentWorkspace(ctx, "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceID != "project-a" || name != "项目 A" {
		t.Fatalf("工作区绑定未生效: id=%q name=%q", workspaceID, name)
	}

	// 不存在的工作区被拒绝，且保持原绑定。
	if err := bridge.SetWorkspace(ctx, "user-1", item.ID, "project-missing"); err == nil {
		t.Fatal("未知工作区必须被拒绝")
	}
	if current, _, _ := bridge.CurrentWorkspace(ctx, "user-1", item.ID); current != "project-a" {
		t.Fatalf("被拒绝的绑定不应改变现状: %q", current)
	}
}

// The bridge must satisfy both halves of the command configuration contract.
var (
	_ bot.CommandRuntimeInfo  = (*commandRuntimeBridge)(nil)
	_ bot.CommandRuntimeAdmin = (*commandRuntimeBridge)(nil)
)
