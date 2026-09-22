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

func TestConfigCenterAPIProfilesBindingsAndSystemSettings(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	registry, err := provider.NewRegistry(context.Background(), store.ProviderRepository(), nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	service, err := configsvc.NewService(store.ConfigRepository(), func(_ context.Context, providerID, _ string) error {
		if providerID == "" {
			return nil
		}
		_, err := registry.Get(providerID)
		return err
	})
	if err != nil {
		t.Fatalf("创建配置服务失败: %v", err)
	}
	if err := service.EnsureDefault(context.Background()); err != nil {
		t.Fatalf("创建默认配置失败: %v", err)
	}
	server := NewServer(registry, nil)
	server.SetConfigService(service)
	handler := server.Handler()

	schema := callHTTP(handler, http.MethodGet, "/api/v1/config/schema", "")
	if schema.Code != http.StatusOK || !strings.Contains(schema.Body.String(), "context.compaction.enabled") || !strings.Contains(schema.Body.String(), "context.compaction.sliding_interval") {
		t.Fatalf("Schema 接口异常: %d %s", schema.Code, schema.Body.String())
	}
	created := callHTTP(handler, http.MethodPost, "/api/v1/config-profiles", `{"name":"开发配置","values":{"ai.enabled":true,"persona.system_prompt":"只回复测试","context.compaction.trigger_ratio":0.7,"context.compaction.sliding_interval":4,"context.compaction.sliding_overlap":1,"workspace.exec_enabled":false}}`)
	if created.Code != http.StatusOK {
		t.Fatalf("创建配置文件失败: %d %s", created.Code, created.Body.String())
	}
	var profile configsvc.Profile
	if err := json.Unmarshal(created.Body.Bytes(), &profile); err != nil {
		t.Fatalf("解析配置文件响应失败: %v", err)
	}
	if profile.Revision != 1 || profile.Name != "开发配置" {
		t.Fatalf("配置文件响应不正确: %#v", profile)
	}
	updated := callHTTP(handler, http.MethodPut, "/api/v1/config-profiles/"+profile.ID, `{"name":"开发配置","values":{"persona.system_prompt":"第二版","context.compaction.trigger_ratio":0.7,"context.compaction.sliding_interval":4,"context.compaction.sliding_overlap":1,"workspace.exec_enabled":false}}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"revision":2`) {
		t.Fatalf("配置修订没有递增: %d %s", updated.Code, updated.Body.String())
	}
	revisions := callHTTP(handler, http.MethodGet, "/api/v1/config-profiles/"+profile.ID+"/revisions", "")
	if revisions.Code != http.StatusOK || strings.Count(revisions.Body.String(), `"revision"`) != 2 {
		t.Fatalf("配置修订历史异常: %d %s", revisions.Code, revisions.Body.String())
	}

	invalid := callHTTP(handler, http.MethodPost, "/api/v1/config-profiles/"+profile.ID+"/validate", `{"id":"`+profile.ID+`","name":"开发配置","values":{"unknown.key":true}}`)
	if invalid.Code != http.StatusOK || !strings.Contains(invalid.Body.String(), `"valid":false`) {
		t.Fatalf("无效配置草稿没有被拒绝: %d %s", invalid.Code, invalid.Body.String())
	}
	defaultSet := callHTTP(handler, http.MethodPost, "/api/v1/config-profiles/"+profile.ID+"/default", "{}")
	if defaultSet.Code != http.StatusOK {
		t.Fatalf("设置配置默认值失败: %d %s", defaultSet.Code, defaultSet.Body.String())
	}
	resolved, err := service.Resolve(context.Background(), "bot-a", "conversation-a")
	if err != nil {
		t.Fatalf("解析默认配置失败: %v", err)
	}
	if resolved.Instruction != "第二版" || resolved.CompactionRatio != 0.7 || resolved.CompactionInterval != 4 || resolved.CompactionOverlap != 1 || resolved.WorkspaceExecEnabled {
		t.Fatalf("默认配置没有进入运行时: %#v", resolved)
	}
	compatSettingsView := callHTTP(handler, http.MethodGet, "/api/v1/settings", "")
	if compatSettingsView.Code != http.StatusOK || !strings.Contains(compatSettingsView.Body.String(), `"sliding_interval":4`) || !strings.Contains(compatSettingsView.Body.String(), `"sliding_overlap":1`) {
		t.Fatalf("兼容设置端点未暴露滑动窗口配置: %d %s", compatSettingsView.Code, compatSettingsView.Body.String())
	}

	bound := callHTTP(handler, http.MethodPut, "/api/v1/conversations/conversation-a/config-profile", `{"profile_id":"`+profile.ID+`"}`)
	if bound.Code != http.StatusOK {
		t.Fatalf("绑定对话配置失败: %d %s", bound.Code, bound.Body.String())
	}
	if err := service.Bind(context.Background(), configsvc.BindingBot, "bot-a", ""); err != nil {
		t.Fatalf("解除机器人配置绑定失败: %v", err)
	}
	settings := callHTTP(handler, http.MethodPut, "/api/v1/system-settings", `{"log_level":"debug","request_timeout_seconds":60,"modal_fallback_enabled":true,"modal_fallback_provider_id":" fallback ","modal_fallback_vision_model":" vision ","modal_fallback_audio_model":" audio ","subagent_enabled":false,"subagent_profiles":{"document_image":{"provider_id":"fallback","model_id":"vision","reasoning_effort":"high","max_output_tokens":512}}}`)
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), `"log_level":"debug"`) || !strings.Contains(settings.Body.String(), `"modal_fallback_enabled":true`) || !strings.Contains(settings.Body.String(), `"modal_fallback_provider_id":"fallback"`) || !strings.Contains(settings.Body.String(), `"subagent_enabled":false`) || !strings.Contains(settings.Body.String(), `"subagent_profiles"`) {
		t.Fatalf("系统设置保存失败: %d %s", settings.Code, settings.Body.String())
	}
	settingsView := callHTTP(handler, http.MethodGet, "/api/v1/system-settings", "")
	if settingsView.Code != http.StatusOK || !strings.Contains(settingsView.Body.String(), `"restart_required":false`) || !strings.Contains(settingsView.Body.String(), `"modal_fallback_audio_model":"audio"`) || !strings.Contains(settingsView.Body.String(), `"subagent_enabled":false`) || !strings.Contains(settingsView.Body.String(), `"subagent_profile_schema"`) || !strings.Contains(settingsView.Body.String(), `"reasoning_effort":"high"`) {
		t.Fatalf("系统设置 Schema 缺少热更新元数据: %d %s", settingsView.Code, settingsView.Body.String())
	}
	invalidSettings := callHTTP(handler, http.MethodPut, "/api/v1/system-settings", `{"log_level":"trace","request_timeout_seconds":60}`)
	if invalidSettings.Code != http.StatusBadRequest {
		t.Fatalf("无效系统设置状态码 = %d，响应=%s", invalidSettings.Code, invalidSettings.Body.String())
	}

	if deleted := callHTTP(handler, http.MethodDelete, "/api/v1/config-profiles/"+profile.ID, ""); deleted.Code != http.StatusConflict {
		t.Fatalf("删除默认配置状态码 = %d，响应=%s", deleted.Code, deleted.Body.String())
	}
	if unbound := callHTTP(handler, http.MethodPut, "/api/v1/conversations/conversation-a/config-profile", `{"profile_id":""}`); unbound.Code != http.StatusOK {
		t.Fatalf("解除对话配置绑定失败: %d %s", unbound.Code, unbound.Body.String())
	}
	if err := service.SetDefaultProfile(context.Background(), "default"); err != nil {
		t.Fatalf("恢复系统默认配置失败: %v", err)
	}
	if deleted := callHTTP(handler, http.MethodDelete, "/api/v1/config-profiles/"+profile.ID, ""); deleted.Code != http.StatusNoContent {
		t.Fatalf("删除非默认配置状态码 = %d，响应=%s", deleted.Code, deleted.Body.String())
	}
}
