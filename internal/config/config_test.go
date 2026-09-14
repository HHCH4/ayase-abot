package config

import "testing"

func TestSchemaRuntimeSettingsAreWired(t *testing.T) {
	schema := buildSchema()
	keys := make(map[string]Field, len(schema.Fields))
	for _, field := range schema.Fields {
		keys[field.Key] = field
	}
	for _, key := range []string{
		"ai.temperature", "ai.top_p", "ai.max_output_tokens", "ai.request_retries",
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
