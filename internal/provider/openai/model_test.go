package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestConvertRequestsPreservesHistoryAndTools(t *testing.T) {
	temperature := float32(0.2)
	maxOutputTokens := int32(128)
	req := &adkmodel.LLMRequest{
		Model: "demo-model",
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("你是一个工具助手。", genai.RoleUser),
			Temperature:       &temperature,
			MaxOutputTokens:   maxOutputTokens,
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name:        "lookup_weather",
				Description: "查询天气",
				Parameters: &genai.Schema{
					Type:       genai.TypeObject,
					Properties: map[string]*genai.Schema{"city": {Type: genai.TypeString}},
					Required:   []string{"city"},
				},
			}}}},
			ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAny,
			}},
		},
		Contents: []*genai.Content{
			genai.NewContentFromText("北京天气怎么样？", genai.RoleUser),
			genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "call-1", Name: "lookup_weather", Args: map[string]any{"city": "北京"},
			}}}, genai.RoleModel),
			// 故意省略 FunctionResponse.ID，验证转换层能从最近的函数调用补齐 call_id。
			genai.NewContentFromParts([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				Name: "lookup_weather", Response: map[string]any{"temperature": 22},
			}}}, genai.RoleUser),
		},
	}

	chat, err := ConvertChatRequest(req)
	if err != nil {
		t.Fatalf("转换 Chat 请求失败: %v", err)
	}
	if len(chat.Messages) != 4 {
		t.Fatalf("Chat 消息数 = %d，期望 4", len(chat.Messages))
	}
	if chat.Messages[0].Role != "system" || chat.Messages[1].Role != "user" || chat.Messages[2].Role != "assistant" {
		t.Fatalf("Chat 消息角色不正确: %#v", chat.Messages)
	}
	if got := chat.Messages[3].ToolCallID; got != "call-1" {
		t.Fatalf("Chat tool_call_id = %q，期望 call-1", got)
	}
	if chat.Messages[2].ToolCalls[0].Function.Name != "lookup_weather" {
		t.Fatalf("Chat 函数调用名称不正确: %#v", chat.Messages[2].ToolCalls)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Parameters["type"] != "object" {
		t.Fatalf("Chat 工具 schema 没有转换为 OpenAI JSON Schema: %#v", chat.Tools)
	}
	if choice, ok := chat.ToolChoice.(string); !ok || choice != "required" {
		t.Fatalf("Chat tool_choice = %#v，期望 required", chat.ToolChoice)
	}

	responses, err := ConvertResponsesRequest(req)
	if err != nil {
		t.Fatalf("转换 Responses 请求失败: %v", err)
	}
	if responses.Instructions != "你是一个工具助手。" || len(responses.Input) != 3 {
		t.Fatalf("Responses instructions/input 不正确: %#v", responses)
	}
	if responses.Input[1]["type"] != "function_call" || responses.Input[1]["call_id"] != "call-1" {
		t.Fatalf("Responses 函数调用不正确: %#v", responses.Input[1])
	}
	if responses.Input[2]["type"] != "function_call_output" || responses.Input[2]["call_id"] != "call-1" {
		t.Fatalf("Responses 函数返回没有匹配调用 ID: %#v", responses.Input[2])
	}
}

func TestConvertRequestPlanFieldsOnlyWhenProjected(t *testing.T) {
	req := &adkmodel.LLMRequest{
		Model: "reasoning-model",
		Config: &genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
			ThinkingConfig:   &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh, IncludeThoughts: true},
		},
		Contents: []*genai.Content{genai.NewContentFromText("返回 JSON", genai.RoleUser)},
	}
	chat, err := ConvertChatRequest(req)
	if err != nil {
		t.Fatalf("Chat 请求转换失败: %v", err)
	}
	if chat.ResponseFormat == nil || chat.ResponseFormat.Type != "json_object" || chat.ReasoningEffort != "high" {
		t.Fatalf("Chat 未正确投影请求计划: %#v", chat)
	}
	responses, err := ConvertResponsesRequest(req)
	if err != nil {
		t.Fatalf("Responses 请求转换失败: %v", err)
	}
	if responses.Text == nil || responses.Text.Format == nil || responses.Text.Format.Type != "json_object" || responses.Reasoning == nil || responses.Reasoning.Effort != "high" || responses.Reasoning.Summary != "auto" {
		t.Fatalf("Responses 未正确投影请求计划: %#v", responses)
	}

	plain, err := ConvertChatRequest(&adkmodel.LLMRequest{Model: "plain", Config: &genai.GenerateContentConfig{}})
	if err != nil {
		t.Fatalf("普通 Chat 请求转换失败: %v", err)
	}
	if plain.ResponseFormat != nil || plain.ReasoningEffort != "" {
		t.Fatalf("未协商字段不应被盲发: %#v", plain)
	}
}

func TestConvertStructuredSchemaAndReasoningRequests(t *testing.T) {
	req := &adkmodel.LLMRequest{
		Model: "schema-model",
		Config: &genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
			ResponseJsonSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
				"required":             []string{"ok"},
				"additionalProperties": false,
			},
			ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow, IncludeThoughts: true},
		},
		Contents: []*genai.Content{genai.NewContentFromText("按 schema 返回", genai.RoleUser)},
	}

	chat, err := ConvertChatRequest(req)
	if err != nil {
		t.Fatalf("Chat schema 请求转换失败: %v", err)
	}
	if chat.ResponseFormat == nil || chat.ResponseFormat.Type != "json_schema" || chat.ResponseFormat.JSONSchema == nil {
		t.Fatalf("Chat 未投影 json_schema: %#v", chat.ResponseFormat)
	}
	if chat.ResponseFormat.JSONSchema.Name != "abot_response" || !chat.ResponseFormat.JSONSchema.Strict || chat.ResponseFormat.JSONSchema.Schema["type"] != "object" || chat.ReasoningEffort != "low" {
		t.Fatalf("Chat schema/reasoning 投影不正确: %#v", chat)
	}

	responses, err := ConvertResponsesRequest(req)
	if err != nil {
		t.Fatalf("Responses schema 请求转换失败: %v", err)
	}
	if responses.Text == nil || responses.Text.Format == nil || responses.Text.Format.Type != "json_schema" || responses.Text.Format.Name != "abot_response" || !responses.Text.Format.Strict || responses.Text.Format.Schema["type"] != "object" {
		t.Fatalf("Responses 未投影 json_schema: %#v", responses.Text)
	}
	if responses.Reasoning == nil || responses.Reasoning.Effort != "low" || responses.Reasoning.Summary != "auto" {
		t.Fatalf("Responses reasoning 投影不正确: %#v", responses.Reasoning)
	}

	bad := *req
	bad.Config = &genai.GenerateContentConfig{ResponseJsonSchema: "not-an-object"}
	if _, err := ConvertChatRequest(&bad); err == nil {
		t.Fatal("非对象 JSON Schema 应被拒绝")
	}
	if _, err := ConvertResponsesRequest(&bad); err == nil {
		t.Fatal("Responses 非对象 JSON Schema 应被拒绝")
	}
	tooLarge := *req
	tooLarge.Config = &genai.GenerateContentConfig{ResponseJsonSchema: map[string]any{"description": strings.Repeat("x", maxResponseJSONSchemaBytes)}}
	if _, err := ConvertChatRequest(&tooLarge); err == nil || !strings.Contains(err.Error(), "字节上限") {
		t.Fatalf("超大 JSON Schema 应被有界拒绝，得到 %v", err)
	}
}

func TestParseOpenAIReasoningUsage(t *testing.T) {
	chat, err := parseChatResponse([]byte(`{"id":"chat-reasoning","model":"reasoning-model","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"completion_tokens_details":{"reasoning_tokens":2}}}`))
	if err != nil {
		t.Fatalf("解析 Chat reasoning usage 失败: %v", err)
	}
	if chat.UsageMetadata == nil || chat.UsageMetadata.ThoughtsTokenCount != 2 {
		t.Fatalf("Chat reasoning token 未投影: %#v", chat.UsageMetadata)
	}

	responses, err := parseResponsesResponse([]byte(`{"id":"resp-reasoning","model":"reasoning-model","status":"completed","output_text":"OK","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8,"output_tokens_details":{"reasoning_tokens":2}}}`))
	if err != nil {
		t.Fatalf("解析 Responses reasoning usage 失败: %v", err)
	}
	if responses.UsageMetadata == nil || responses.UsageMetadata.ThoughtsTokenCount != 2 {
		t.Fatalf("Responses reasoning token 未投影: %#v", responses.UsageMetadata)
	}
}

func TestAdapterProbeCapabilitiesUsesRouteAndNeverExecutesTool(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		body := `{"id":"base","model":"demo-model","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
		if payload["stream"] == true {
			body = "data: {\"id\":\"stream\",\"model\":\"demo-model\",\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n" +
				"data: {\"id\":\"stream\",\"model\":\"demo-model\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
				"data: [DONE]\n"
		} else if _, ok := payload["tools"]; ok {
			body = `{"id":"tool","model":"demo-model","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"abot_capability_probe_noop","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
		} else if _, ok := payload["response_format"]; ok {
			body = `{"id":"json","model":"demo-model","choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	adapter := NewAdapter(client)
	p := provider.Provider{ID: "openai", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatChat, BaseURL: "https://example.invalid/v1", APIKey: "secret"}
	m := provider.Model{ID: "demo-model", Enabled: true}
	result, err := adapter.ProbeCapabilities(context.Background(), p, m, provider.CapabilityProbeOptions{})
	if err != nil {
		t.Fatalf("OpenAI adapter 能力探测失败: %v", err)
	}
	if result.Profile.Route != string(WireFormatChat) || result.Profile.ToolCalling.State != provider.SupportSupported || result.Profile.StructuredOutput.State != provider.SupportSupported || result.Profile.Streaming.State != provider.SupportSupported {
		t.Fatalf("OpenAI adapter 探测 profile 不正确: %#v", result.Profile)
	}
	if calls != 4 {
		t.Fatalf("OpenAI adapter 探测请求数 = %d，期望 4", calls)
	}
}

func TestAdapterProbeCapabilitiesUsesResponsesRoute(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if request.URL.Path != "/v1/responses" || payload["model"] != "demo-model" {
			return nil, fmt.Errorf("Responses probe route/payload 不正确: path=%s payload=%#v", request.URL.Path, payload)
		}
		body := `{"id":"base","model":"demo-model","status":"completed","output_text":"OK","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`
		if payload["stream"] == true {
			body = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"stream\",\"model\":\"demo-model\"}}\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"stream\",\"model\":\"demo-model\",\"status\":\"completed\"}}\n" +
				"data: [DONE]\n"
		} else if _, ok := payload["tools"]; ok {
			body = `{"id":"tool","model":"demo-model","status":"completed","output":[{"type":"function_call","id":"call-1","call_id":"call-1","name":"abot_capability_probe_noop","arguments":"{}"}]}`
		} else if _, ok := payload["text"]; ok {
			body = `{"id":"json","model":"demo-model","status":"completed","output_text":"{}"}`
		}
		contentType := "application/json"
		if payload["stream"] == true {
			contentType = "text/event-stream"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	adapter := NewAdapter(client)
	p := provider.Provider{ID: "openai", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses, BaseURL: "https://example.invalid/v1", APIKey: "secret"}
	m := provider.Model{ID: "demo-model", Enabled: true}
	result, err := adapter.ProbeCapabilities(context.Background(), p, m, provider.CapabilityProbeOptions{})
	if err != nil {
		t.Fatalf("OpenAI Responses adapter 能力探测失败: %v", err)
	}
	if result.Profile.Route != string(WireFormatResponses) || result.Profile.ToolCalling.State != provider.SupportSupported || result.Profile.StructuredOutput.State != provider.SupportSupported || result.Profile.Streaming.State != provider.SupportSupported {
		t.Fatalf("OpenAI Responses 探测 profile 不正确: %#v", result.Profile)
	}
	if calls != 4 {
		t.Fatalf("OpenAI Responses 探测请求数 = %d，期望 4", calls)
	}
}

func TestAdapterProbeCapabilitiesOptionallyProbesGatewayTokenCount(t *testing.T) {
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path == "/v1/responses/input_tokens" {
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				return nil, err
			}
			if payload["model"] != "demo-model" || payload["input"] == nil || payload["max_output_tokens"] != nil {
				return nil, fmt.Errorf("gateway token-count probe payload 不正确: %#v", payload)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"response.input_tokens","input_tokens":23}`))}, nil
		}
		if request.URL.Path != "/v1/responses" {
			return nil, fmt.Errorf("gateway capability probe 路径不正确: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"probe","model":"demo-model","status":"completed","output_text":"OK"}`))}, nil
	})}
	adapter := NewAdapter(client)
	p := provider.Provider{ID: "gateway", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses, BaseURL: "https://gateway.example/v1", APIKey: "secret"}
	result, err := adapter.ProbeCapabilities(context.Background(), p, provider.Model{ID: "demo-model", Enabled: true}, provider.CapabilityProbeOptions{ExplicitOptionalSelection: true, IncludeTokenCount: true})
	if err != nil {
		t.Fatalf("兼容网关 token-count 能力探测失败: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/v1/responses" || paths[1] != "/v1/responses/input_tokens" {
		t.Fatalf("兼容网关探测请求序列不正确: %#v", paths)
	}
	if !result.Profile.Tokenizer.Known || result.Profile.Tokenizer.Source != "provider" || result.Profile.Tokenizer.Quality != "provider" {
		t.Fatalf("成功 token-count probe 未建立 provider tokenizer: %#v", result.Profile.Tokenizer)
	}
	observation := findProbeObservation(result.Observations, "token_count")
	if observation.State != provider.SupportSupported || observation.Route != string(WireFormatResponses) {
		t.Fatalf("token-count observation 不正确: %#v", observation)
	}
}

func TestAdapterProbeCapabilitiesTokenCountFailureKeepsGatewayHeuristic(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/responses/input_tokens" {
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"route not found"}}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"probe","model":"demo-model","status":"completed","output_text":"OK"}`))}, nil
	})}
	adapter := NewAdapter(client)
	p := provider.Provider{ID: "gateway", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses, BaseURL: "https://gateway.example/v1", APIKey: "secret"}
	result, err := adapter.ProbeCapabilities(context.Background(), p, provider.Model{ID: "demo-model", Enabled: true}, provider.CapabilityProbeOptions{ExplicitOptionalSelection: true, IncludeTokenCount: true})
	if err != nil {
		t.Fatalf("token-count endpoint 缺失不应让能力探测失败: %v", err)
	}
	if result.Profile.Tokenizer.Known || result.Profile.Tokenizer.Quality != provider.DefaultTokenizerQuality {
		t.Fatalf("token-count endpoint 缺失时应保留 heuristic: %#v", result.Profile.Tokenizer)
	}
	observation := findProbeObservation(result.Observations, "token_count")
	if observation.State != provider.SupportUnsupported || strings.Contains(observation.Reason, "route not found") {
		t.Fatalf("缺失 endpoint 应记录脱敏 unsupported 证据: %#v", observation)
	}
}

func TestAdapterCountTokensUsesResponsesInputTokenEndpoint(t *testing.T) {
	var gotPayload map[string]any
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses/input_tokens" {
			return nil, fmt.Errorf("OpenAI token count 路径或方法不正确: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer test-key" {
			return nil, fmt.Errorf("OpenAI token count 未携带认证")
		}
		if err := json.NewDecoder(request.Body).Decode(&gotPayload); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"object":"response.input_tokens","input_tokens":37}`))}, nil
	})}
	adapter := NewAdapter(client)
	temperature := float32(0.2)
	request := &adkmodel.LLMRequest{
		Model: "demo-model",
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("系统约束", genai.RoleUser),
			Temperature:       &temperature,
			MaxOutputTokens:   128,
			Tools:             []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "read_file"}}}},
			ThinkingConfig:    &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow},
		},
		Contents: []*genai.Content{genai.NewContentFromText("统计这段内容", genai.RoleUser)},
	}
	count, err := adapter.CountTokens(context.Background(), provider.Provider{ID: "openai", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses, BaseURL: "https://example.invalid/v1", APIKey: "test-key"}, provider.Model{ID: "demo-model"}, request)
	if err != nil {
		t.Fatalf("OpenAI Responses token count 失败: %v", err)
	}
	if count.Tokens != 37 || !count.Exact || count.Name != provider.OpenAIInputTokenCounterName || count.Version != provider.OpenAIInputTokenCounterVersion {
		t.Fatalf("OpenAI token count metadata 不正确: %#v", count)
	}
	if gotPayload["model"] != "demo-model" || gotPayload["instructions"] != "系统约束" || gotPayload["input"] == nil || gotPayload["tools"] == nil || gotPayload["reasoning"] == nil {
		t.Fatalf("OpenAI token count 未携带完整上下文: %#v", gotPayload)
	}
	for _, generationOnly := range []string{"temperature", "max_output_tokens", "stream"} {
		if _, exists := gotPayload[generationOnly]; exists {
			t.Fatalf("OpenAI token count 不应携带生成字段 %q: %#v", generationOnly, gotPayload)
		}
	}
}

func TestAdapterCountTokensSupportsChatThroughResponsesInputTokenEndpoint(t *testing.T) {
	var gotPayload map[string]any
	adapter := NewAdapter(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses/input_tokens" {
			return nil, fmt.Errorf("Chat token count 路径或方法不正确: %s %s", request.Method, request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&gotPayload); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"object":"response.input_tokens","input_tokens":19}`))}, nil
	})})
	request := &adkmodel.LLMRequest{Model: "demo-model", Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	count, err := adapter.CountTokens(context.Background(), provider.Provider{ID: "openai", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatChat, BaseURL: "https://example.invalid/v1", APIKey: "test-key"}, provider.Model{ID: "demo-model"}, request)
	if err != nil {
		t.Fatalf("Chat token count 不应因线路格式失败: %v", err)
	}
	if count.Tokens != 19 || !count.Exact || count.Name != provider.OpenAIInputTokenCounterName {
		t.Fatalf("Chat token count metadata 不正确: %#v", count)
	}
	if gotPayload["model"] != "demo-model" || gotPayload["input"] == nil {
		t.Fatalf("Chat token count 未投影到 Responses input: %#v", gotPayload)
	}
}

func TestAdapterCountTokensUsesExplicitAnthropicRoute(t *testing.T) {
	var payload map[string]any
	adapter := NewAdapter(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/messages/count_tokens" {
			return nil, fmt.Errorf("显式 Anthropic token count 路径不正确: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("x-api-key") != "anthropic-key" || request.Header.Get("Authorization") != "" || request.Header.Get("anthropic-version") != "2023-06-01" {
			return nil, fmt.Errorf("显式 Anthropic token count 认证不正确: %#v", request.Header)
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"input_tokens":29}`))}, nil
	})})
	request := &adkmodel.LLMRequest{Model: "claude-sonnet-4", Config: &genai.GenerateContentConfig{MaxOutputTokens: 2048}, Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	count, err := adapter.CountTokens(context.Background(), provider.Provider{
		ID: "gateway", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses,
		TokenCountProtocol: provider.TokenCountProtocolAnthropic, BaseURL: "https://gateway.example/v1", APIKey: "anthropic-key",
	}, provider.Model{ID: "claude-sonnet-4"}, request)
	if err != nil {
		t.Fatalf("显式 Anthropic token count 失败: %v", err)
	}
	if count.Tokens != 29 || !count.Exact || count.Name != provider.AnthropicTokenCounterName || count.Version != provider.AnthropicTokenCounterVersion {
		t.Fatalf("显式 Anthropic token count metadata 不正确: %#v", count)
	}
	if payload["model"] != "claude-sonnet-4" || payload["messages"] == nil {
		t.Fatalf("显式 Anthropic 请求未投影模型/消息: %#v", payload)
	}
	if _, ok := payload["max_output_tokens"]; ok {
		t.Fatalf("Anthropic token count 不应携带 generation-only 字段: %#v", payload)
	}
}

func TestAdapterProbeCapabilitiesUsesExplicitAnthropicTokenCountRoute(t *testing.T) {
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/v1/responses":
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"probe","model":"claude-sonnet-4","status":"completed","output_text":"OK"}`))}, nil
		case "/v1/messages/count_tokens":
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"input_tokens":13}`))}, nil
		default:
			return nil, fmt.Errorf("意外的 probe 路径: %s", request.URL.Path)
		}
	})}
	adapter := NewAdapter(client)
	result, err := adapter.ProbeCapabilities(context.Background(), provider.Provider{
		ID: "gateway", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses,
		TokenCountProtocol: provider.TokenCountProtocolAnthropic, BaseURL: "https://gateway.example/v1", APIKey: "secret",
	}, provider.Model{ID: "claude-sonnet-4", Enabled: true}, provider.CapabilityProbeOptions{ExplicitOptionalSelection: true, IncludeTokenCount: true})
	if err != nil {
		t.Fatalf("显式 Anthropic token-count probe 失败: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/v1/responses" || paths[1] != "/v1/messages/count_tokens" {
		t.Fatalf("显式 Anthropic probe 路径序列不正确: %#v", paths)
	}
	if !result.Profile.Tokenizer.Known || result.Profile.Tokenizer.Name != provider.AnthropicTokenCounterName || result.Profile.Tokenizer.Confidence != 0.95 {
		t.Fatalf("显式 Anthropic probe 未建立 exact tokenizer: %#v", result.Profile.Tokenizer)
	}
	observation := findProbeObservation(result.Observations, "token_count")
	if observation.State != provider.SupportSupported || observation.Route != string(WireFormatResponses) {
		t.Fatalf("显式 Anthropic token-count observation 不正确: %#v", observation)
	}
}

func TestAdapterCountTokensRedactsErrorsForResponsesAndChat(t *testing.T) {
	adapter := NewAdapter(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"bad test-key"}`))}
		return response, nil
	})})
	request := &adkmodel.LLMRequest{Model: "demo-model", Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	for _, format := range []provider.OpenAIFormat{provider.OpenAIFormatResponses, provider.OpenAIFormatChat} {
		_, err := adapter.CountTokens(context.Background(), provider.Provider{ID: "openai", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: format, BaseURL: "https://example.invalid/v1", APIKey: "test-key"}, provider.Model{ID: "demo-model"}, request)
		if err == nil || errors.Is(err, provider.ErrTokenCounterUnavailable) || strings.Contains(err.Error(), "test-key") || !strings.Contains(err.Error(), "REDACTED") {
			t.Fatalf("format=%s OpenAI token count 错误未正确脱敏或不应 unavailable: %v", format, err)
		}
	}
}

func TestAdapterCountTokensRejectsMalformedResponses(t *testing.T) {
	request := &adkmodel.LLMRequest{Model: "demo-model", Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	cases := []struct {
		name string
		body string
	}{
		{name: "missing input_tokens", body: `{"object":"response.input_tokens"}`},
		{name: "negative input_tokens", body: `{"object":"response.input_tokens","input_tokens":-1}`},
		{name: "wrong object", body: `{"object":"response.output_text","input_tokens":3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewAdapter(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})})
			_, err := adapter.CountTokens(context.Background(), provider.Provider{ID: "openai", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses, BaseURL: "https://example.invalid/v1", APIKey: "test-key"}, provider.Model{ID: "demo-model"}, request)
			if err == nil {
				t.Fatalf("malformed response should fail: %s", tc.body)
			}
		})
	}
}

func TestInputTokensEndpointAcceptsOpenAIBaseURLShapes(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com":                            "https://api.openai.com/v1/responses/input_tokens",
		"https://api.openai.com/v1":                         "https://api.openai.com/v1/responses/input_tokens",
		"https://example.invalid/v1/":                       "https://example.invalid/v1/responses/input_tokens",
		"https://example.invalid/v1/responses":              "https://example.invalid/v1/responses/input_tokens",
		"https://example.invalid/v1/responses/":             "https://example.invalid/v1/responses/input_tokens",
		"https://example.invalid/v1/responses/input_tokens": "https://example.invalid/v1/responses/input_tokens",
	}
	for base, want := range cases {
		got, err := inputTokensEndpoint(base)
		if err != nil || got != want {
			t.Fatalf("base=%q endpoint=%q err=%v want=%q", base, got, err, want)
		}
	}
}

func TestAdapterProbeCapabilitiesOptionalProbesProjectProviderFields(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if request.URL.Path != "/v1/chat/completions" || payload["model"] != "demo-model" {
			return nil, fmt.Errorf("Chat optional probe route/payload 不正确: path=%s payload=%#v", request.URL.Path, payload)
		}
		body := `{"id":"base","model":"demo-model","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`
		responseFormat, _ := payload["response_format"].(map[string]any)
		messagesJSON, _ := json.Marshal(payload["messages"])
		switch {
		case responseFormat["type"] == "json_schema":
			body = `{"id":"schema","model":"demo-model","choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}]}`
		case payload["reasoning_effort"] == "low":
			body = `{"id":"reasoning","model":"demo-model","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"completion_tokens_details":{"reasoning_tokens":1}}}`
		case strings.Contains(string(messagesJSON), `"image_url"`), strings.Contains(string(messagesJSON), `"file_data"`):
			body = `{"id":"multimodal","model":"demo-model","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	adapter := NewAdapter(client)
	p := provider.Provider{ID: "openai", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatChat, BaseURL: "https://example.invalid/v1", APIKey: "secret"}
	m := provider.Model{ID: "demo-model", Enabled: true}
	result, err := adapter.ProbeCapabilities(context.Background(), p, m, provider.CapabilityProbeOptions{
		ExplicitOptionalSelection: true,
		IncludeStructuredSchema:   true,
		IncludeReasoning:          true,
		IncludeImages:             true,
		IncludeInputFiles:         true,
	})
	if err != nil {
		t.Fatalf("OpenAI optional 能力探测失败: %v", err)
	}
	if calls != 5 {
		t.Fatalf("OpenAI optional 探测请求数=%d，期望基础、schema、reasoning、image、file 五次", calls)
	}
	if findProbeObservation(result.Observations, "structured_output_schema").State != provider.SupportSupported {
		t.Fatalf("OpenAI schema 证据不正确: %#v", result.Observations)
	}
	if result.Profile.StructuredOutputSchema.State != provider.SupportSupported {
		t.Fatalf("OpenAI schema 能力状态未应用: %#v", result.Profile.StructuredOutputSchema)
	}
	if result.Profile.JSONSchemaDialect != "openai-chat-json-schema" {
		t.Fatalf("OpenAI schema dialect 未绑定 Chat 线路: %q", result.Profile.JSONSchemaDialect)
	}
	if result.Profile.Images.State != provider.SupportSupported || result.Profile.InputFiles.State != provider.SupportSupported {
		t.Fatalf("OpenAI 多模态能力未应用: %#v", result.Profile)
	}
	if findProbeObservation(result.Observations, "reasoning_effort").State != provider.SupportSupported {
		t.Fatalf("OpenAI reasoning 证据不正确: %#v", result.Observations)
	}
}

func TestAdapterProbeCapabilitiesProviderSpecificRejectionMatrix(t *testing.T) {
	tests := []struct {
		name       string
		format     WireFormat
		feature    string
		configure  func(*provider.CapabilityProbeOptions)
		requestHas func(string) bool
	}{
		{
			name:    "chat schema",
			format:  WireFormatChat,
			feature: "structured_output_schema",
			configure: func(options *provider.CapabilityProbeOptions) {
				options.IncludeStructuredSchema = true
			},
			requestHas: func(encoded string) bool { return strings.Contains(encoded, `"response_format"`) },
		},
		{
			name:    "responses schema",
			format:  WireFormatResponses,
			feature: "structured_output_schema",
			configure: func(options *provider.CapabilityProbeOptions) {
				options.IncludeStructuredSchema = true
			},
			requestHas: func(encoded string) bool { return strings.Contains(encoded, `"text":{"format"`) },
		},
		{
			name:    "chat image",
			format:  WireFormatChat,
			feature: "images",
			configure: func(options *provider.CapabilityProbeOptions) {
				options.IncludeImages = true
			},
			requestHas: func(encoded string) bool { return strings.Contains(encoded, `"image_url"`) },
		},
		{
			name:    "responses file",
			format:  WireFormatResponses,
			feature: "input_files",
			configure: func(options *provider.CapabilityProbeOptions) {
				options.IncludeInputFiles = true
			},
			requestHas: func(encoded string) bool { return strings.Contains(encoded, `"input_file"`) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				var payload map[string]any
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					return nil, err
				}
				encoded, _ := json.Marshal(payload)
				if tt.requestHas(string(encoded)) {
					return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"requested field is unsupported"}}`))}, nil
				}
				body := `{"id":"base","model":"demo-model","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`
				if tt.format == WireFormatResponses {
					body = `{"id":"base","model":"demo-model","status":"completed","output_text":"OK"}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			adapter := NewAdapter(client)
			p := provider.Provider{ID: "openai", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormat(tt.format), BaseURL: "https://example.invalid/v1", APIKey: "secret"}
			options := provider.CapabilityProbeOptions{ExplicitOptionalSelection: true}
			tt.configure(&options)
			result, err := adapter.ProbeCapabilities(context.Background(), p, provider.Model{ID: "demo-model", Enabled: true}, options)
			if err != nil {
				t.Fatalf("provider-specific rejection probe 失败: %v", err)
			}
			observation := findProbeObservation(result.Observations, tt.feature)
			if observation.State != provider.SupportUnsupported {
				t.Fatalf("%s 应为 unsupported，实际=%#v", tt.feature, observation)
			}
			if tt.feature == "structured_output_schema" && result.Profile.JSONSchemaDialect != "" {
				t.Fatalf("schema 拒绝后不应残留 dialect: %q", result.Profile.JSONSchemaDialect)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func findProbeObservation(observations []provider.CapabilityObservation, feature string) provider.CapabilityObservation {
	for _, observation := range observations {
		if observation.Feature == feature {
			return observation
		}
	}
	return provider.CapabilityObservation{}
}

func TestConvertMultimodalContentForChatAndResponses(t *testing.T) {
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromText("请分析图片和文件"),
			{InlineData: &genai.Blob{MIMEType: "image/png", DisplayName: "截图.png", Data: []byte{1, 2, 3}}},
			{InlineData: &genai.Blob{MIMEType: "application/pdf", DisplayName: "说明.pdf", Data: []byte{4, 5, 6}}},
		}, genai.RoleUser),
	}}

	chat, err := ConvertChatRequest(req)
	if err != nil {
		t.Fatalf("转换 Chat 多模态请求失败: %v", err)
	}
	parts, ok := chat.Messages[0].Content.([]ChatContentPart)
	if !ok || len(parts) != 3 {
		t.Fatalf("Chat 多模态 content 不正确: %#v", chat.Messages[0].Content)
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("Chat 图片没有转换成 data URL: %#v", parts[1])
	}
	if parts[2].Type != "file" || parts[2].File == nil || parts[2].File.Filename != "说明.pdf" || !strings.HasPrefix(parts[2].File.FileData, "data:application/pdf;base64,") {
		t.Fatalf("Chat 文件没有转换成 file_data: %#v", parts[2])
	}

	responses, err := ConvertResponsesRequest(req)
	if err != nil {
		t.Fatalf("转换 Responses 多模态请求失败: %v", err)
	}
	content, ok := responses.Input[0]["content"].([]map[string]any)
	if !ok || len(content) != 3 {
		t.Fatalf("Responses 多模态 content 不正确: %#v", responses.Input[0]["content"])
	}
	if content[1]["type"] != "input_image" || !strings.HasPrefix(content[1]["image_url"].(string), "data:image/png;base64,") {
		t.Fatalf("Responses 图片没有转换成 input_image: %#v", content[1])
	}
	if content[2]["type"] != "input_file" || content[2]["filename"] != "说明.pdf" || !strings.HasPrefix(content[2]["file_data"].(string), "data:application/pdf;base64,") {
		t.Fatalf("Responses 文件没有转换成 input_file: %#v", content[2])
	}
}

func TestModelGenerateContentChatAndResponses(t *testing.T) {
	tests := []struct {
		name       string
		format     WireFormat
		path       string
		response   string
		wantText   string
		wantFormat string
	}{
		{
			name:       "chat completions",
			format:     WireFormatChat,
			path:       "/v1/chat/completions",
			response:   `{"id":"chat-1","model":"demo-model","choices":[{"message":{"role":"assistant","content":"你好，Chat。"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
			wantText:   "你好，Chat。",
			wantFormat: string(WireFormatChat),
		},
		{
			name:       "responses",
			format:     WireFormatResponses,
			path:       "/v1/responses",
			response:   `{"id":"resp-1","model":"demo-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"你好，Responses。"}]}],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}`,
			wantText:   "你好，Responses。",
			wantFormat: string(WireFormatResponses),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != tt.path {
					t.Errorf("收到上游请求 %s %s，期望 POST %s", request.Method, request.URL.Path, tt.path)
				}
				if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
					t.Errorf("Authorization = %q，期望 Bearer test-key", got)
				}
				var payload map[string]any
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Errorf("上游请求 JSON 无效: %v", err)
				}
				if payload["model"] != "demo-model" {
					t.Errorf("上游 model = %#v", payload["model"])
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(tt.response))
			}))
			defer server.Close()

			llm, err := NewModel(context.Background(), "demo-model", &ClientConfig{
				APIKey: "test-key", BaseURL: server.URL + "/v1", Format: tt.format, HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatalf("创建模型失败: %v", err)
			}
			responses := collectResponses(llm.GenerateContent(context.Background(), &adkmodel.LLMRequest{
				Contents: []*genai.Content{genai.NewContentFromText("测试", genai.RoleUser)},
			}, false))
			if responses.err != nil {
				t.Fatalf("运行模型失败: %v", responses.err)
			}
			if len(responses.items) != 1 || responses.items[0].Content == nil {
				t.Fatalf("模型响应数量/content 不正确: %#v", responses.items)
			}
			if got := contentText(responses.items[0].Content); got != tt.wantText {
				t.Fatalf("响应文本 = %q，期望 %q", got, tt.wantText)
			}
			if got := responses.items[0].CustomMetadata["openai_wire_format"]; got != tt.wantFormat {
				t.Fatalf("线路元数据 = %#v，期望 %q", got, tt.wantFormat)
			}
			if !responses.items[0].TurnComplete {
				t.Fatal("非流式响应应标记 TurnComplete")
			}
		})
	}
}

func TestModelGenerateContentStreaming(t *testing.T) {
	tests := []struct {
		name   string
		format WireFormat
		path   string
		body   string
	}{
		{
			name:   "chat stream",
			format: WireFormatChat,
			path:   "/v1/chat/completions",
			body: "data: {\"id\":\"chat-stream\",\"model\":\"demo-model\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:   "responses stream",
			format: WireFormatResponses,
			path:   "/v1/responses",
			body: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stream\",\"model\":\"demo-model\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stream\",\"model\":\"demo-model\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"你好\"}]}]}}\n\n" +
				"data: [DONE]\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != tt.path {
					t.Errorf("流式请求路径 = %q，期望 %q", request.URL.Path, tt.path)
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = writer.Write([]byte(tt.body))
			}))
			defer server.Close()

			llm, err := NewModel(context.Background(), "demo-model", &ClientConfig{
				APIKey: "test-key", BaseURL: server.URL + "/v1", Format: tt.format, HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatalf("创建模型失败: %v", err)
			}
			responses := collectResponses(llm.GenerateContent(context.Background(), &adkmodel.LLMRequest{
				Contents: []*genai.Content{genai.NewContentFromText("测试", genai.RoleUser)},
			}, true))
			if responses.err != nil {
				t.Fatalf("运行流式模型失败: %v", responses.err)
			}
			if len(responses.items) < 2 {
				t.Fatalf("流式响应数量 = %d，至少应有增量和最终响应", len(responses.items))
			}
			if !responses.items[len(responses.items)-1].TurnComplete {
				t.Fatal("流式最终响应应标记 TurnComplete")
			}
			if got := contentText(responses.items[len(responses.items)-1].Content); got != "你好" {
				t.Fatalf("流式最终文本 = %q，期望 你好", got)
			}
		})
	}
}

type responseCollection struct {
	items []*adkmodel.LLMResponse
	err   error
}

// collectResponses 将迭代器收集成测试结果，便于同时验证错误和最终事件。
func collectResponses(sequence func(func(*adkmodel.LLMResponse, error) bool)) responseCollection {
	result := responseCollection{}
	sequence(func(response *adkmodel.LLMResponse, err error) bool {
		if err != nil {
			result.err = err
			return false
		}
		if response != nil {
			result.items = append(result.items, response)
		}
		return true
	})
	return result
}

func TestParseChatResponseRejectsMalformedToolArguments(t *testing.T) {
	_, err := parseChatResponse([]byte(`{"id":"chat-1","choices":[{"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"demo","arguments":"{bad"}}]}}]}`))
	if err == nil || !strings.Contains(err.Error(), "解析 Chat 工具") {
		t.Fatalf("畸形工具参数错误 = %v", err)
	}
}

func TestAutoFormatUsesResponsesEndpointSuffix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("自动线路请求路径 = %q，期望 /v1/responses", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"id":"resp-1","model":"demo","status":"completed","output_text":"ok"}`)
	}))
	defer server.Close()

	llm, err := NewModel(context.Background(), "demo", &ClientConfig{BaseURL: server.URL + "/v1/responses", HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("创建自动线路模型失败: %v", err)
	}
	responses := collectResponses(llm.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false))
	if responses.err != nil || len(responses.items) != 1 || contentText(responses.items[0].Content) != "ok" {
		t.Fatalf("自动 Responses 线路结果不正确: %#v", responses)
	}
}
