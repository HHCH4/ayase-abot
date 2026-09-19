package config

import "testing"

func TestSchemaRuntimeSettingsAreWired(t *testing.T) {
	schema := buildSchema()
	keys := make(map[string]Field, len(schema.Fields))
	for _, field := range schema.Fields {
		keys[field.Key] = field
	}
	for _, key := range []string{
		"ai.temperature", "ai.top_p", "ai.max_output_tokens", "ai.request_retries", "ai.reasoning_effort",
		"persona.id", "persona.system_prompt",
		"context.compaction.unknown_window_tokens", "context.compaction.sliding_interval", "context.compaction.sliding_overlap", "agent.max_tool_calls", "agent.tool_schema_budget_tokens",
		"workspace.git_enabled", "message.streaming_enabled", "message.prompt_prefix",
		"memory.enabled", "memory.auto_retrieve", "memory.max_results",
	} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("Schema 缺少实际运行配置项 %q", key)
		}
	}

	runtime := runtimeFromValues(Values{
		"ai.temperature":       0.25,
		"ai.top_p":             0.9,
		"ai.max_output_tokens": 2048,
		"ai.request_retries":   3,
		"ai.reasoning_effort":  "high",
		"persona.id":           "persona-reviewer",
		"context.compaction.unknown_window_tokens": 16384,
		"context.compaction.sliding_interval":      4,
		"context.compaction.sliding_overlap":       1,
		"agent.max_tool_calls":                     7,
		"agent.tool_schema_budget_tokens":          2048,
		"workspace.git_enabled":                    false,
		"message.streaming_enabled":                false,
		"message.prompt_prefix":                    "回答要简洁",
		"memory.enabled":                           true,
		"memory.auto_retrieve":                     true,
		"memory.max_results":                       12,
	})
	if runtime.AITemperature != 0.25 || runtime.AITopP != 0.9 || runtime.AIMaxOutputTokens != 2048 || runtime.AIRequestRetries != 3 {
		t.Fatalf("AI 生成参数没有从配置解析: %#v", runtime)
	}
	if runtime.AIReasoningEffort != "high" {
		t.Fatalf("思考强度没有从配置解析: %#v", runtime)
	}
	if runtime.PersonaID != "persona-reviewer" {
		t.Fatalf("人格 ID 没有从配置解析: %#v", runtime)
	}
	if runtime.CompactionUnknownWindowTokens != 16384 || runtime.CompactionInterval != 4 || runtime.CompactionOverlap != 1 || runtime.AgentMaxToolCalls != 7 || runtime.ToolSchemaBudgetTokens != 2048 {
		t.Fatalf("Agent 上下文参数没有从配置解析: %#v", runtime)
	}
	if runtime.WorkspaceGitEnabled || runtime.MessageStreamingEnabled {
		t.Fatalf("布尔运行配置没有从配置解析: %#v", runtime)
	}
	if runtime.MessagePromptPrefix != "回答要简洁" {
		t.Fatalf("消息前缀没有从配置解析: %#v", runtime)
	}
	if !runtime.MemoryEnabled || !runtime.MemoryAutoRetrieve || runtime.MemoryMaxResults != 12 {
		t.Fatalf("长期记忆运行配置没有从配置解析: %#v", runtime)
	}
}

func TestSlidingCompactionOverlapValidation(t *testing.T) {
	cases := []Values{
		{"context.compaction.sliding_overlap": 1},
		{"context.compaction.sliding_interval": 2, "context.compaction.sliding_overlap": 3},
	}
	for _, values := range cases {
		if err := validateSlidingCompactionValues(values); err == nil {
			t.Fatalf("无效滑动窗口配置未被拒绝: %#v", values)
		}
	}
	if err := validateSlidingCompactionValues(Values{"context.compaction.sliding_interval": 2, "context.compaction.sliding_overlap": 1}); err != nil {
		t.Fatalf("有效滑动窗口配置被拒绝: %v", err)
	}
}

func TestSystemSettingsArtifactLifecycleBounds(t *testing.T) {
	schema := SystemSchema()
	fields := make(map[string]Field, len(schema.Fields))
	for _, field := range schema.Fields {
		fields[field.Key] = field
	}
	quota, ok := fields["artifact_quota_bytes"]
	if !ok || quota.Default != DefaultArtifactQuotaBytes {
		t.Fatalf("Schema 缺少 Artifact 配额默认值: %+v", quota)
	}
	staleUpload, ok := fields["artifact_stale_upload_seconds"]
	if !ok || staleUpload.Default != DefaultArtifactStaleUploadSeconds {
		t.Fatalf("Schema 缺少未完成上传保留时长默认值: %+v", staleUpload)
	}

	// 旧记录和全新配置没有任何 Artifact 字段，此时必须补上保守默认值。
	defaulted := normalizeSystemSettings(SystemSettings{})
	if defaulted.ArtifactQuotaBytes != DefaultArtifactQuotaBytes || defaulted.ArtifactStaleUploadSeconds != DefaultArtifactStaleUploadSeconds {
		t.Fatalf("零值系统设置未补默认 Artifact 边界: %+v", defaulted)
	}
	if err := validateSystemSettings(defaulted); err != nil {
		t.Fatalf("默认 Artifact 边界必须合法: %v", err)
	}
	for _, key := range []string{"modal_fallback_enabled", "modal_fallback_provider_id", "modal_fallback_vision_model", "modal_fallback_audio_model"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("Schema 缺少多模态降级设置 %q", key)
		}
	}
	fallback := normalizeSystemSettings(SystemSettings{
		LogLevel: "INFO", RequestTimeoutSeconds: 300, ArtifactStaleUploadSeconds: DefaultArtifactStaleUploadSeconds,
		ModalFallbackEnabled: true, ModalFallbackProviderID: " fallback ", ModalFallbackVisionModel: " vision ", ModalFallbackAudioModel: " audio ",
	})
	if !fallback.ModalFallbackEnabled || fallback.ModalFallbackProviderID != "fallback" || fallback.ModalFallbackVisionModel != "vision" || fallback.ModalFallbackAudioModel != "audio" {
		t.Fatalf("多模态降级设置未规范化: %+v", fallback)
	}
	if err := validateSystemSettings(fallback); err != nil {
		t.Fatalf("有效多模态降级设置必须通过校验: %v", err)
	}

	// 用户显式关闭配额（0 配额 + 合法保留时长）不能被默认值覆盖。
	disabled := normalizeSystemSettings(SystemSettings{ArtifactQuotaBytes: 0, ArtifactStaleUploadSeconds: DefaultArtifactStaleUploadSeconds})
	if disabled.ArtifactQuotaBytes != 0 {
		t.Fatalf("显式关闭的配额被默认值覆盖: %+v", disabled)
	}
	if err := validateSystemSettings(disabled); err != nil {
		t.Fatalf("关闭配额必须合法: %v", err)
	}

	invalid := []SystemSettings{
		{LogLevel: "info", RequestTimeoutSeconds: 300, ArtifactQuotaBytes: -1, ArtifactStaleUploadSeconds: DefaultArtifactStaleUploadSeconds},
		{LogLevel: "info", RequestTimeoutSeconds: 300, ArtifactQuotaBytes: MaxArtifactQuotaBytes + 1, ArtifactStaleUploadSeconds: DefaultArtifactStaleUploadSeconds},
		{LogLevel: "info", RequestTimeoutSeconds: 300, ArtifactQuotaBytes: DefaultArtifactQuotaBytes, ArtifactStaleUploadSeconds: MinArtifactStaleUploadSeconds - 1},
		{LogLevel: "info", RequestTimeoutSeconds: 300, ArtifactQuotaBytes: DefaultArtifactQuotaBytes, ArtifactStaleUploadSeconds: MaxArtifactStaleUploadSeconds + 1},
	}
	for index, settings := range invalid {
		if err := validateSystemSettings(settings); err == nil {
			t.Fatalf("非法 Artifact 设置 #%d 必须被拒绝: %+v", index, settings)
		}
	}
}
