package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	configsvc "Abot/internal/config"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
)

func TestPersonaCatalogAPIAndSelectionBindings(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, err := configsvc.NewPersonaService(store.PersonaRepository())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureDefault(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), nil)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(registry, nil)
	server.SetPersonaService(service)
	configService, err := configsvc.NewService(store.ConfigRepository(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configService.EnsureDefault(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.SetConfigService(configService)
	handler := server.Handler()

	list := callHTTP(handler, http.MethodGet, "/api/v1/personas", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"default_persona_id":"default"`) || !strings.Contains(list.Body.String(), `"schema_version":1`) {
		t.Fatalf("人格目录列表异常: %d %s", list.Code, list.Body.String())
	}
	created := callHTTP(handler, http.MethodPost, "/api/v1/personas", `{"id":"persona-api","name":"API 人格","description":"HTTP 测试","instruction":"你是 API 测试人格。"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("创建人格失败: %d %s", created.Code, created.Body.String())
	}
	var item configsvc.Persona
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if item.ID != "persona-api" || item.Revision != 1 || !item.Enabled {
		t.Fatalf("创建人格响应异常: %#v", item)
	}
	updated := callHTTP(handler, http.MethodPut, "/api/v1/personas/persona-api", `{"name":"API 人格","instruction":"第二版 API 指令。","enabled":false}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"revision":2`) || !strings.Contains(updated.Body.String(), `"enabled":false`) {
		t.Fatalf("更新人格失败: %d %s", updated.Code, updated.Body.String())
	}
	revisions := callHTTP(handler, http.MethodGet, "/api/v1/personas/persona-api/revisions", "")
	if revisions.Code != http.StatusOK || strings.Count(revisions.Body.String(), `"revision"`) != 2 {
		t.Fatalf("人格修订接口异常: %d %s", revisions.Code, revisions.Body.String())
	}
	if mismatch := callHTTP(handler, http.MethodPut, "/api/v1/personas/persona-api", `{"id":"other","name":"x","instruction":"x"}`); mismatch.Code != http.StatusBadRequest {
		t.Fatalf("路径/请求 ID 不一致应返回 400，得到 %d %s", mismatch.Code, mismatch.Body.String())
	}
	if disabledBind := callHTTP(handler, http.MethodPut, "/api/v1/bots/bot-api/persona", `{"persona_id":"persona-api"}`); disabledBind.Code != http.StatusBadRequest {
		t.Fatalf("禁用人格绑定应返回 400，得到 %d %s", disabledBind.Code, disabledBind.Body.String())
	}

	active := callHTTP(handler, http.MethodPost, "/api/v1/personas", `{"id":"persona-active","name":"活跃人格","instruction":"活跃指令"}`)
	if active.Code != http.StatusCreated {
		t.Fatalf("创建活跃人格失败: %d %s", active.Code, active.Body.String())
	}
	schema := callHTTP(handler, http.MethodGet, "/api/v1/config/schema", "")
	if schema.Code != http.StatusOK || !strings.Contains(schema.Body.String(), `"key":"persona.id"`) || !strings.Contains(schema.Body.String(), `"value":"persona-active"`) {
		t.Fatalf("配置 Schema 未暴露可选人格目录: %d %s", schema.Code, schema.Body.String())
	}
	if bound := callHTTP(handler, http.MethodPut, "/api/v1/bots/bot-api/persona", `{"persona_id":"persona-active"}`); bound.Code != http.StatusOK || !strings.Contains(bound.Body.String(), `"persona_id":"persona-active"`) {
		t.Fatalf("绑定机器人人格失败: %d %s", bound.Code, bound.Body.String())
	}
	if bound := callHTTP(handler, http.MethodPut, "/api/v1/conversations/conversation-api/persona", `{"persona_id":"default"}`); bound.Code != http.StatusOK {
		t.Fatalf("绑定对话人格失败: %d %s", bound.Code, bound.Body.String())
	}
	selected, ok, err := service.ResolveSelection(context.Background(), "bot-api", "conversation-api", "")
	if err != nil || !ok || selected.ID != "default" {
		t.Fatalf("API 绑定优先级异常: %#v ok=%v err=%v", selected, ok, err)
	}
	if got := callHTTP(handler, http.MethodGet, "/api/v1/bots/bot-api/persona", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"persona_id":"persona-active"`) {
		t.Fatalf("读取机器人人格绑定失败: %d %s", got.Code, got.Body.String())
	}
	if unbound := callHTTP(handler, http.MethodPut, "/api/v1/conversations/conversation-api/persona", `{"persona_id":""}`); unbound.Code != http.StatusOK || !strings.Contains(unbound.Body.String(), `"persona_id":""`) {
		t.Fatalf("解除对话人格绑定失败: %d %s", unbound.Code, unbound.Body.String())
	}
	if deleted := callHTTP(handler, http.MethodDelete, "/api/v1/personas/persona-active", ""); deleted.Code != http.StatusConflict {
		t.Fatalf("仍绑定的人格删除应返回 409，得到 %d %s", deleted.Code, deleted.Body.String())
	}
	if unbound := callHTTP(handler, http.MethodPut, "/api/v1/bots/bot-api/persona", `{"persona_id":""}`); unbound.Code != http.StatusOK {
		t.Fatalf("解除机器人人格绑定失败: %d %s", unbound.Code, unbound.Body.String())
	}
	if setDefault := callHTTP(handler, http.MethodPost, "/api/v1/personas/persona-active/default", "{}"); setDefault.Code != http.StatusOK {
		t.Fatalf("设置默认人格失败: %d %s", setDefault.Code, setDefault.Body.String())
	}
	if deleteDefault := callHTTP(handler, http.MethodDelete, "/api/v1/personas/persona-active", ""); deleteDefault.Code != http.StatusConflict {
		t.Fatalf("默认人格删除应返回 409，得到 %d %s", deleteDefault.Code, deleteDefault.Body.String())
	}
	if deleteOld := callHTTP(handler, http.MethodDelete, "/api/v1/personas/persona-api", ""); deleteOld.Code != http.StatusNoContent {
		t.Fatalf("未绑定非默认人格删除失败: %d %s", deleteOld.Code, deleteOld.Body.String())
	}
}
