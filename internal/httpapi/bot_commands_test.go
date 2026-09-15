package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"Abot/internal/agent"
	"Abot/internal/bot"
	"Abot/internal/conversation"
	"Abot/internal/provider"
	"Abot/internal/provider/openai"
	"Abot/internal/storage/sqlite"
)

func newBotCommandHandler(t *testing.T) (http.Handler, *bot.Manager) {
	t.Helper()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), map[provider.Protocol]provider.Adapter{
		provider.ProtocolOpenAICompatible: openai.NewAdapter(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationService, err := conversation.NewService(store.ConversationRepository(), store.SessionService(), "abot")
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{AppName: "abot", SessionService: store.SessionService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := bot.NewManager(context.Background(), store.BotRepository(), kernel, conversationService)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	created := callHTTP(NewServerWithServices(registry, kernel, nil, conversationService, manager).Handler(),
		http.MethodPost, "/api/v1/bots", `{"id":"cmd-bot","name":"指令机器人","type":"telegram","telegram_token":"token","enabled":false}`)
	if created.Code != http.StatusOK {
		t.Fatalf("创建机器人失败: %d %s", created.Code, created.Body.String())
	}
	return NewServerWithServices(registry, kernel, nil, conversationService, manager).Handler(), manager
}

func TestBotCommandCatalogPolicyAndAuditAPI(t *testing.T) {
	handler, _ := newBotCommandHandler(t)

	listed := callHTTP(handler, http.MethodGet, "/api/v1/bots/cmd-bot/commands", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("列出指令状态码 = %d，响应 = %s", listed.Code, listed.Body.String())
	}
	var payload struct {
		Commands []bot.EffectiveCommand `json:"commands"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Commands) < 13 {
		t.Fatalf("指令目录不完整: %d", len(payload.Commands))
	}
	// 默认策略必须来自描述符，且未标记覆盖。
	for _, command := range payload.Commands {
		if command.Overridden {
			t.Fatalf("初始状态不应有覆盖: %+v", command)
		}
	}
	workspaceFound := false
	for _, command := range payload.Commands {
		if command.ID == "workspace" {
			workspaceFound = true
			if command.Permission != bot.PermissionGlobalAdmin {
				t.Fatalf("/workspace 必须默认仅全局管理员: %s", command.Permission)
			}
		}
	}
	if !workspaceFound {
		t.Fatal("目录缺少 /workspace")
	}

	// 停用 /id，并把 /new 放宽给所有人。
	disabled := callHTTP(handler, http.MethodPut, "/api/v1/bots/cmd-bot/commands/id", `{"permission":"everyone","enabled":false}`)
	if disabled.Code != http.StatusOK {
		t.Fatalf("停用指令状态码 = %d，响应 = %s", disabled.Code, disabled.Body.String())
	}
	var afterDisable struct {
		Commands []bot.EffectiveCommand `json:"commands"`
	}
	if err := json.Unmarshal(disabled.Body.Bytes(), &afterDisable); err != nil {
		t.Fatal(err)
	}
	for _, command := range afterDisable.Commands {
		if command.ID == "id" && (command.Enabled || !command.Overridden) {
			t.Fatalf("停用未生效: %+v", command)
		}
	}

	widened := callHTTP(handler, http.MethodPut, "/api/v1/bots/cmd-bot/commands/new", `{"permission":"everyone","enabled":true}`)
	if widened.Code != http.StatusOK {
		t.Fatalf("放宽权限状态码 = %d，响应 = %s", widened.Code, widened.Body.String())
	}

	// 多词指令用一段路径参数表示，ServeMux 负责解码分隔符。
	adminPolicy := callHTTP(handler, http.MethodPut, "/api/v1/bots/cmd-bot/commands/admin%20add", `{"permission":"global_admin","enabled":true}`)
	if adminPolicy.Code != http.StatusOK {
		t.Fatalf("多词指令策略状态码 = %d，响应 = %s", adminPolicy.Code, adminPolicy.Body.String())
	}

	// 未知指令必须被拒绝，而不是写入一条无主策略。
	unknown := callHTTP(handler, http.MethodPut, "/api/v1/bots/cmd-bot/commands/nope", `{"permission":"everyone","enabled":true}`)
	if unknown.Code == http.StatusOK {
		t.Fatalf("未知指令不应被接受: %s", unknown.Body.String())
	}
	// 非法权限级别同样被拒绝。
	badPermission := callHTTP(handler, http.MethodPut, "/api/v1/bots/cmd-bot/commands/id", `{"permission":"root","enabled":true}`)
	if badPermission.Code == http.StatusOK {
		t.Fatalf("非法权限级别不应被接受: %s", badPermission.Body.String())
	}

	audits := callHTTP(handler, http.MethodGet, "/api/v1/bots/cmd-bot/commands/audit", "")
	if audits.Code != http.StatusOK {
		t.Fatalf("审计状态码 = %d，响应 = %s", audits.Code, audits.Body.String())
	}
	var auditPayload struct {
		Audits []bot.CommandAudit `json:"audits"`
	}
	if err := json.Unmarshal(audits.Body.Bytes(), &auditPayload); err != nil {
		t.Fatal(err)
	}
	if len(auditPayload.Audits) < 3 {
		t.Fatalf("策略改动必须写入审计: %+v", auditPayload.Audits)
	}
	for _, entry := range auditPayload.Audits {
		if entry.Action != bot.AuditCommandPolicy {
			t.Fatalf("意外的审计动作: %+v", entry)
		}
	}
}

func TestBotGroupAdminAPI(t *testing.T) {
	handler, _ := newBotCommandHandler(t)

	added := callHTTP(handler, http.MethodPost, "/api/v1/bots/cmd-bot/group-admins", `{"chat_id":"group-1","user_id":"10001"}`)
	if added.Code != http.StatusOK {
		t.Fatalf("添加群管理员状态码 = %d，响应 = %s", added.Code, added.Body.String())
	}
	if !strings.Contains(added.Body.String(), "10001") {
		t.Fatalf("响应应包含新管理员: %s", added.Body.String())
	}
	// 重复添加必须幂等。
	again := callHTTP(handler, http.MethodPost, "/api/v1/bots/cmd-bot/group-admins", `{"chat_id":"group-1","user_id":"10001"}`)
	if again.Code != http.StatusOK || strings.Count(again.Body.String(), "10001") != 1 {
		t.Fatalf("重复添加应幂等: %d %s", again.Code, again.Body.String())
	}

	listed := callHTTP(handler, http.MethodGet, "/api/v1/bots/cmd-bot/group-admins?chat_id=group-1", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "10001") {
		t.Fatalf("列出群管理员失败: %d %s", listed.Code, listed.Body.String())
	}
	// 另一个群不共享管理员。
	other := callHTTP(handler, http.MethodGet, "/api/v1/bots/cmd-bot/group-admins?chat_id=group-2", "")
	if strings.Contains(other.Body.String(), "10001") {
		t.Fatalf("群管理员必须按群独立: %s", other.Body.String())
	}

	removed := callHTTP(handler, http.MethodDelete, "/api/v1/bots/cmd-bot/group-admins/10001?chat_id=group-1", "")
	if removed.Code != http.StatusOK || strings.Contains(removed.Body.String(), "10001") {
		t.Fatalf("移除群管理员失败: %d %s", removed.Code, removed.Body.String())
	}

	// 缺少 chat_id 时必须明确拒绝。
	missing := callHTTP(handler, http.MethodGet, "/api/v1/bots/cmd-bot/group-admins", "")
	if missing.Code == http.StatusOK {
		t.Fatalf("缺少 chat_id 不应成功: %s", missing.Body.String())
	}
}

func TestBotGlobalAdministratorsAreConfiguredFromTheConsole(t *testing.T) {
	handler, _ := newBotCommandHandler(t)

	// 创建时配置管理员，并验证规范化（去重、去空白、排序）。
	created := callHTTP(handler, http.MethodPost, "/api/v1/bots",
		`{"id":"admin-bot","name":"管理员机器人","type":"telegram","telegram_token":"token","admin_user_ids":["  20002 ","10001","20002",""],"enabled":false}`)
	if created.Code != http.StatusOK {
		t.Fatalf("创建机器人失败: %d %s", created.Code, created.Body.String())
	}
	if !strings.Contains(created.Body.String(), `"admin_user_ids":["10001","20002"]`) {
		t.Fatalf("管理员列表未规范化: %s", created.Body.String())
	}

	// 局部更新（未提供该字段）必须保留已有管理员，否则一次改名就会锁死自己。
	renamed := callHTTP(handler, http.MethodPut, "/api/v1/bots/admin-bot", `{"name":"改名后","type":"telegram","enabled":false}`)
	if renamed.Code != http.StatusOK {
		t.Fatalf("更新机器人失败: %d %s", renamed.Code, renamed.Body.String())
	}
	if !strings.Contains(renamed.Body.String(), `"admin_user_ids":["10001","20002"]`) {
		t.Fatalf("未提供的字段不应清空管理员: %s", renamed.Body.String())
	}

	// 显式空数组表示清空。
	cleared := callHTTP(handler, http.MethodPut, "/api/v1/bots/admin-bot", `{"name":"改名后","type":"telegram","admin_user_ids":[],"enabled":false}`)
	if cleared.Code != http.StatusOK {
		t.Fatalf("清空管理员失败: %d %s", cleared.Code, cleared.Body.String())
	}
	if strings.Contains(cleared.Body.String(), `"admin_user_ids"`) {
		t.Fatalf("显式清空后不应返回管理员: %s", cleared.Body.String())
	}

	// 读取时也应返回当前管理员。
	withAdmins := callHTTP(handler, http.MethodPut, "/api/v1/bots/admin-bot", `{"name":"改名后","type":"telegram","admin_user_ids":["30003"],"enabled":false}`)
	if withAdmins.Code != http.StatusOK {
		t.Fatalf("设置管理员失败: %d %s", withAdmins.Code, withAdmins.Body.String())
	}
	fetched := callHTTP(handler, http.MethodGet, "/api/v1/bots/admin-bot", "")
	if !strings.Contains(fetched.Body.String(), `"admin_user_ids":["30003"]`) {
		t.Fatalf("读取机器人应返回管理员: %s", fetched.Body.String())
	}
}
