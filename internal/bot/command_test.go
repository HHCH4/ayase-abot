package bot

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"Abot/internal/agent"
	"Abot/internal/conversation"
	"google.golang.org/adk/v2/session"
)

// commandTestRepository extends the runtime-delivery fixture with the optional
// command permission stores. Keeping it in-memory keeps these tests about the
// permission rules rather than about SQLite.
type commandTestRepository struct {
	*botTestRepository

	mu       sync.Mutex
	admins   map[string]map[string]string // adapter -> chat -> user
	policies map[string]map[string]CommandPolicy
	audits   []CommandAudit
}

func newCommandTestRepository() *commandTestRepository {
	return &commandTestRepository{
		botTestRepository: &botTestRepository{items: map[string]Bot{}},
		admins:            map[string]map[string]string{},
		policies:          map[string]map[string]CommandPolicy{},
	}
}

func (r *commandTestRepository) ListGroupAdmins(_ context.Context, adapterID, chatID string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]string, 0)
	for user := range r.admins[adapterID+"\x00"+chatID] {
		result = append(result, user)
	}
	sort.Strings(result)
	return result, nil
}

func (r *commandTestRepository) AddGroupAdmin(_ context.Context, admin GroupAdmin) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := admin.AdapterID + "\x00" + admin.ChatID
	if r.admins[key] == nil {
		r.admins[key] = map[string]string{}
	}
	r.admins[key][admin.UserID] = admin.AddedBy
	return nil
}

func (r *commandTestRepository) RemoveGroupAdmin(_ context.Context, adapterID, chatID, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := adapterID + "\x00" + chatID
	if _, ok := r.admins[key][userID]; !ok {
		return errors.New("not found")
	}
	delete(r.admins[key], userID)
	return nil
}

func (r *commandTestRepository) ListCommandPolicies(_ context.Context, adapterID string) ([]CommandPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]CommandPolicy, 0)
	for _, policy := range r.policies[adapterID] {
		result = append(result, policy)
	}
	return result, nil
}

func (r *commandTestRepository) SaveCommandPolicy(_ context.Context, adapterID string, policy CommandPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.policies[adapterID] == nil {
		r.policies[adapterID] = map[string]CommandPolicy{}
	}
	r.policies[adapterID][policy.CommandID] = policy
	return nil
}

func (r *commandTestRepository) AppendCommandAudit(_ context.Context, entry CommandAudit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, entry)
	return nil
}

func (r *commandTestRepository) ListCommandAudits(_ context.Context, adapterID string, limit int) ([]CommandAudit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]CommandAudit, 0, len(r.audits))
	for _, entry := range r.audits {
		if adapterID == "" || entry.AdapterID == adapterID {
			result = append(result, entry)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// newCommandManager builds a manager whose bot has no administrators, so each
// test states the administrative setup it actually relies on.
func newCommandManager(t *testing.T, item Bot) (*Manager, *commandTestRepository, *botTestPlatform) {
	t.Helper()
	ctx := context.Background()
	repository := newCommandTestRepository()
	item.ID = "bot-1"
	repository.items[item.ID] = item
	conversationService, err := conversation.NewService(newBotConversationRepository(), session.InMemoryService(), "abot")
	if err != nil {
		t.Fatalf("创建测试 Conversation Service 失败: %v", err)
	}
	manager, err := NewManager(ctx, repository, &agent.Kernel{}, conversationService)
	if err != nil {
		t.Fatalf("创建测试 Bot Manager 失败: %v", err)
	}
	platform := &botTestPlatform{}
	manager.mu.Lock()
	manager.runtimes[item.ID] = runtimeEntry{platform: platform}
	manager.mu.Unlock()
	return manager, repository, platform
}

func groupMessage(chatID, userID, text string) Message {
	return Message{ID: "m-" + userID, AdapterID: "bot-1", Platform: TypeOneBot11, UserID: userID, ChatID: chatID, ChatType: "group", Mentioned: true, Text: text}
}

func privateMessage(userID, text string) Message {
	return Message{ID: "m-" + userID, AdapterID: "bot-1", Platform: TypeTelegram, UserID: userID, ChatID: "chat-" + userID, ChatType: "private", Text: text}
}

func TestCommandRegistryRejectsConflicts(t *testing.T) {
	registry, err := NewBuiltinCommandRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(CommandDescriptor{ID: "dup", Name: "new", Category: CategoryInfo, Description: "冲突", DefaultPermission: PermissionEveryone, Scope: ScopeChat, DefaultEnabled: true}); err == nil {
		t.Fatal("与内置指令重名必须硬失败")
	}
	if err := registry.RegisterFrom("plugin:alpha", "插件 A", []CommandDescriptor{
		{ID: "alpha.echo", Name: "echo", Category: CategoryInfo, Description: "回显", DefaultPermission: PermissionEveryone, Scope: ScopeChat, DefaultEnabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	// 插件不能自我授权：声明的 everyone 必须被收紧。
	descriptor, ok := registry.Lookup("echo")
	if !ok {
		t.Fatal("插件指令应注册成功")
	}
	if descriptor.DefaultPermission != PermissionGlobalAdmin {
		t.Fatalf("插件指令默认权限必须收紧，实际 %s", descriptor.DefaultPermission)
	}
	if err := registry.RegisterFrom("plugin:beta", "插件 B", []CommandDescriptor{
		{ID: "beta.echo", Name: "echo", Category: CategoryInfo, Description: "重名", DefaultPermission: PermissionEveryone, Scope: ScopeChat, DefaultEnabled: true},
	}); err == nil {
		t.Fatal("插件之间重名必须硬失败")
	}
	if err := registry.UnregisterSource("plugin:alpha"); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup("echo"); ok {
		t.Fatal("按来源摘除后指令必须消失")
	}
	if _, ok := registry.Lookup("help"); !ok {
		t.Fatal("摘除插件不能影响内置指令")
	}
}

func TestCommandRegistryResolvesMultiWordSubcommands(t *testing.T) {
	registry, err := NewBuiltinCommandRegistry()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, arg, ok := registry.Resolve("admin", "list")
	if !ok || descriptor.ID != "admin list" {
		t.Fatalf("多词指令解析失败: %+v", descriptor)
	}
	if arg != "" {
		t.Fatalf("子命令不应留在参数里: %q", arg)
	}
	descriptor, arg, ok = registry.Resolve("admin", "add 12345")
	if !ok || descriptor.ID != "admin add" || arg != "12345" {
		t.Fatalf("带参数的多词指令解析失败: %+v arg=%q", descriptor, arg)
	}
	if _, _, ok := registry.Resolve("admin", "nope"); ok {
		t.Fatal("未知子命令不应匹配")
	}
}

func TestCommandPermissionMatrixAndPrivateMapping(t *testing.T) {
	// 群成员、群管理员、全局管理员三种身份，覆盖三级权限。
	manager, repository, platform := newCommandManager(t, Bot{Name: "权限", Type: TypeTelegram, AdminUserIDs: []string{"global-user"}})
	ctx := context.Background()
	if err := repository.AddGroupAdmin(ctx, GroupAdmin{AdapterID: "bot-1", ChatID: "group-a", UserID: "group-admin"}); err != nil {
		t.Fatal(err)
	}

	// everyone 级：任何群成员可用。
	if err := manager.handleMessage(ctx, groupMessage("group-a", "member", "/id")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "group-a") {
		t.Fatalf("/id 应显示聊天标识: %q", platform.lastSent())
	}

	// group_admin 级：群成员被拒绝，群管理员放行。
	if err := manager.handleMessage(ctx, groupMessage("group-a", "member", "/new")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "群管理员权限") {
		t.Fatalf("群成员执行 /new 必须被拒绝: %q", platform.lastSent())
	}
	if err := manager.handleMessage(ctx, groupMessage("group-a", "group-admin", "/status")); err != nil {
		t.Fatal(err)
	}

	// global_admin 级：群管理员仍被拒绝。
	if err := manager.handleMessage(ctx, groupMessage("group-a", "group-admin", "/workspace")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "全局管理员权限") {
		t.Fatalf("群管理员执行 /workspace 必须被拒绝: %q", platform.lastSent())
	}

	// 被拒绝的动作要留下审计。
	audits, err := repository.ListCommandAudits(ctx, "bot-1", 50)
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for _, entry := range audits {
		if entry.Action == AuditCommandDenied {
			denied++
		}
	}
	if denied < 2 {
		t.Fatalf("权限拒绝必须写入审计: %+v", audits)
	}

	// 私聊映射：group_admin 级收紧为 global_admin，非管理员无法执行。
	if err := manager.handleMessage(ctx, privateMessage("member", "/new")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "全局管理员") {
		t.Fatalf("私聊中 group_admin 级必须收紧为全局管理员: %q", platform.lastSent())
	}
	// 全局管理员在私聊中放行。
	if err := manager.handleMessage(ctx, privateMessage("global-user", "/status")); err != nil {
		t.Fatal(err)
	}
}

func TestCommandGroupAdminDelegation(t *testing.T) {
	manager, repository, platform := newCommandManager(t, Bot{Name: "委派", Type: TypeTelegram, AdminUserIDs: []string{"global-user"}})
	ctx := context.Background()

	// 群管理员不能自行授权。
	if err := manager.handleMessage(ctx, groupMessage("group-a", "member", "/admin add 555")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "全局管理员权限") {
		t.Fatalf("非全局管理员不能授权: %q", platform.lastSent())
	}

	// 全局管理员在群里授权。
	if err := manager.handleMessage(ctx, groupMessage("group-a", "global-user", "/admin add 555")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "555") {
		t.Fatalf("授权应回执目标: %q", platform.lastSent())
	}
	admins, err := repository.ListGroupAdmins(ctx, "bot-1", "group-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(admins) != 1 || admins[0] != "555" {
		t.Fatalf("群管理员未持久化: %+v", admins)
	}

	// 被授权者立即获得群管理员能力。
	if err := manager.handleMessage(ctx, groupMessage("group-a", "555", "/status")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(platform.lastSent(), "权限") {
		t.Fatalf("被授权者应可用群管理员指令: %q", platform.lastSent())
	}

	// 按群独立：同一用户在另一个群没有权限。
	if err := manager.handleMessage(ctx, groupMessage("group-b", "555", "/new")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "群管理员权限") {
		t.Fatalf("群管理员必须按群独立: %q", platform.lastSent())
	}

	// 自己退出。
	if err := manager.handleMessage(ctx, groupMessage("group-a", "555", "/admin leave")); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := repository.ListGroupAdmins(ctx, "bot-1", "group-a"); len(remaining) != 0 {
		t.Fatalf("退出后不应保留授权: %+v", remaining)
	}

	// 全局管理员不能通过 leave 退出。
	if err := manager.handleMessage(ctx, groupMessage("group-a", "global-user", "/admin leave")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "WebUI") {
		t.Fatalf("全局管理员退出应指向 WebUI: %q", platform.lastSent())
	}
}

func TestCommandPolicyDisableAndPermissionOverride(t *testing.T) {
	manager, repository, platform := newCommandManager(t, Bot{Name: "策略", Type: TypeTelegram, AdminUserIDs: []string{"global-user"}})
	ctx := context.Background()

	// 默认 /id 对所有人可用。
	if err := manager.handleMessage(ctx, privateMessage("member", "/id")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(platform.lastSent(), "已停用") {
		t.Fatalf("/id 默认应启用: %q", platform.lastSent())
	}

	// 停用后按停用处理，且不泄漏管理细节。
	if err := repository.SaveCommandPolicy(ctx, "bot-1", CommandPolicy{CommandID: "id", Permission: PermissionEveryone, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := manager.handleMessage(ctx, privateMessage("member", "/id")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "已停用") {
		t.Fatalf("停用指令必须提示已停用: %q", platform.lastSent())
	}

	// 权限放宽：把 /new 放开给所有人后，普通成员在群里可用。
	if err := repository.SaveCommandPolicy(ctx, "bot-1", CommandPolicy{CommandID: "new", Permission: PermissionEveryone, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.handleMessage(ctx, groupMessage("group-a", "member", "/new")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(platform.lastSent(), "权限") {
		t.Fatalf("放宽后普通成员应可用 /new: %q", platform.lastSent())
	}
}

func TestUnknownCommandNeverReachesTheModel(t *testing.T) {
	manager, _, platform := newCommandManager(t, Bot{Name: "未知", Type: TypeTelegram})
	ctx := context.Background()
	if err := manager.handleMessage(ctx, privateMessage("user-1", "/nope")); err != nil {
		t.Fatal(err)
	}
	sent := platform.lastSent()
	if !strings.Contains(sent, "未知命令") {
		t.Fatalf("未知命令必须显式提示: %q", sent)
	}
	// 提示里只应出现调用者有权使用的指令。
	if strings.Contains(sent, "/workspace") {
		t.Fatalf("未知命令提示不应泄漏无权指令: %q", sent)
	}
	if !strings.Contains(sent, "/help") {
		t.Fatalf("未知命令提示应包含可用指令: %q", sent)
	}
}

func TestHelpReflectsPolicyAndPermission(t *testing.T) {
	manager, repository, platform := newCommandManager(t, Bot{Name: "帮助", Type: TypeTelegram, AdminUserIDs: []string{"global-user"}})
	ctx := context.Background()

	// 普通成员看到 everyone 级指令，看不到管理员指令。
	if err := manager.handleMessage(ctx, privateMessage("member", "/help")); err != nil {
		t.Fatal(err)
	}
	sent := platform.lastSent()
	if !strings.Contains(sent, "/help") || !strings.Contains(sent, "/id") {
		t.Fatalf("帮助应包含公开指令: %q", sent)
	}
	if strings.Contains(sent, "/workspace") || strings.Contains(sent, "/admin add") {
		t.Fatalf("帮助不应包含无权指令: %q", sent)
	}

	// 全局管理员能看到管理指令。
	if err := manager.handleMessage(ctx, privateMessage("global-user", "/help")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "/workspace") {
		t.Fatalf("全局管理员应看到 /workspace: %q", platform.lastSent())
	}

	// 停用后从帮助里消失。
	if err := repository.SaveCommandPolicy(ctx, "bot-1", CommandPolicy{CommandID: "id", Permission: PermissionEveryone, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := manager.handleMessage(ctx, privateMessage("member", "/help")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(platform.lastSent(), "/id  ") {
		t.Fatalf("停用指令不应出现在帮助里: %q", platform.lastSent())
	}
}

func TestCommandConfigurationRequiresAdaptersAndAuditsSwitches(t *testing.T) {
	manager, repository, platform := newCommandManager(t, Bot{Name: "配置", Type: TypeTelegram, AdminUserIDs: []string{"global-user"}})
	ctx := context.Background()

	// 未装配配置服务时给出明确说明，而不是假数据。
	if err := manager.handleMessage(ctx, privateMessage("global-user", "/model")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "未提供") {
		t.Fatalf("缺少配置服务时应明确说明: %q", platform.lastSent())
	}

	info := &commandTestRuntimeInfo{modelID: "model-a", models: []CommandOption{{ID: "model-a", Name: "model-a"}, {ID: "model-b", Name: "model-b"}}}
	admin := &commandTestRuntimeAdmin{}
	manager.SetCommandRuntimeInfo(info)
	manager.SetCommandRuntimeAdmin(admin)

	if err := manager.handleMessage(ctx, privateMessage("global-user", "/model")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "model-a") {
		t.Fatalf("应展示当前模型: %q", platform.lastSent())
	}
	if err := manager.handleMessage(ctx, privateMessage("global-user", "/model model-b")); err != nil {
		t.Fatal(err)
	}
	if admin.modelID != "model-b" {
		t.Fatalf("模型切换未生效: %q", admin.modelID)
	}
	audits, err := repository.ListCommandAudits(ctx, "bot-1", 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range audits {
		if entry.Action == AuditModelSwitch && entry.Target == "model-b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("模型切换必须写入审计: %+v", audits)
	}

	// 未知模型给出可读提示。
	if err := manager.handleMessage(ctx, privateMessage("global-user", "/model nope")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "没有找到模型") {
		t.Fatalf("未知目标应给出提示: %q", platform.lastSent())
	}
}

type commandTestRuntimeInfo struct {
	modelID string
	models  []CommandOption
}

func (i *commandTestRuntimeInfo) CurrentModel(context.Context, string, string, string) (string, string, error) {
	return "provider-1", i.modelID, nil
}
func (i *commandTestRuntimeInfo) CurrentPersona(context.Context, string, string, string) (string, string, error) {
	return "", "", nil
}
func (i *commandTestRuntimeInfo) CurrentWorkspace(context.Context, string, string) (string, string, error) {
	return "", "", nil
}
func (i *commandTestRuntimeInfo) AvailableModels(context.Context) ([]CommandOption, error) {
	return i.models, nil
}
func (i *commandTestRuntimeInfo) AvailablePersonas(context.Context) ([]CommandOption, error) {
	return nil, nil
}
func (i *commandTestRuntimeInfo) AvailableWorkspaces(context.Context) ([]CommandOption, error) {
	return nil, nil
}

type commandTestRuntimeAdmin struct {
	modelID string
}

func (a *commandTestRuntimeAdmin) SetModel(_ context.Context, _, _, _, modelID string) error {
	a.modelID = modelID
	return nil
}
func (a *commandTestRuntimeAdmin) SetPersona(context.Context, string, string, string, string) error {
	return nil
}
func (a *commandTestRuntimeAdmin) SetWorkspace(context.Context, string, string, string) error {
	return nil
}

func TestGroupAdminIsScopedToBotAndChat(t *testing.T) {
	ctx := context.Background()
	repository := newCommandTestRepository()
	// 两个机器人拥有相同的聊天标识与用户标识，只有 adapter 不同。
	repository.items["bot-1"] = Bot{ID: "bot-1", Name: "一号", Type: TypeOneBot11, AdminUserIDs: []string{"global-1"}}
	repository.items["bot-2"] = Bot{ID: "bot-2", Name: "二号", Type: TypeOneBot11, AdminUserIDs: []string{"global-1"}}
	conversationService, err := conversation.NewService(newBotConversationRepository(), session.InMemoryService(), "abot")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ctx, repository, &agent.Kernel{}, conversationService)
	if err != nil {
		t.Fatal(err)
	}
	platform := &botTestPlatform{}
	manager.mu.Lock()
	manager.runtimes["bot-1"] = runtimeEntry{platform: platform}
	manager.runtimes["bot-2"] = runtimeEntry{platform: platform}
	manager.mu.Unlock()

	// 在 bot-1 的 group-x 授权 555。
	if err := manager.handleMessage(ctx, Message{ID: "a", AdapterID: "bot-1", Platform: TypeOneBot11, UserID: "global-1", ChatID: "group-x", ChatType: "group", Mentioned: true, Text: "/admin add 555"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "555") {
		t.Fatalf("授权失败: %q", platform.lastSent())
	}

	// 同一个 chat_id 与 user_id，在 bot-2 上不获得权限。
	if err := manager.handleMessage(ctx, Message{ID: "b", AdapterID: "bot-2", Platform: TypeOneBot11, UserID: "555", ChatID: "group-x", ChatType: "group", Mentioned: true, Text: "/new"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform.lastSent(), "群管理员权限") {
		t.Fatalf("群管理员标识必须绑定 adapter，跨机器人不得生效: %q", platform.lastSent())
	}
	// 在 bot-1 上同一身份可用。
	if err := manager.handleMessage(ctx, Message{ID: "c", AdapterID: "bot-1", Platform: TypeOneBot11, UserID: "555", ChatID: "group-x", ChatType: "group", Mentioned: true, Text: "/status"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(platform.lastSent(), "权限") {
		t.Fatalf("授权应在来源机器人生效: %q", platform.lastSent())
	}
}
