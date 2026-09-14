package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestCloneRequestWithoutDisplayNames(t *testing.T) {
	request := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromParts([]*genai.Part{
			{InlineData: &genai.Blob{DisplayName: "截图.png", MIMEType: "image/png", Data: []byte{1}}},
			{FileData: &genai.FileData{DisplayName: "说明.pdf", MIMEType: "application/pdf", FileURI: "gs://bucket/file"}},
		}, genai.RoleUser)},
		Config: &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromParts([]*genai.Part{
			{InlineData: &genai.Blob{DisplayName: "system.txt", MIMEType: "text/plain", Data: []byte{2}}},
		}, genai.RoleUser)},
	}

	clone := cloneRequestWithoutDisplayNames(request)
	if clone == request || clone.Contents[0] == request.Contents[0] || clone.Contents[0].Parts[0] == request.Contents[0].Parts[0] {
		t.Fatal("Gemini 请求应复制内容结构，不能直接复用原始指针")
	}
	if clone.Contents[0].Parts[0].InlineData.DisplayName != "" || clone.Contents[0].Parts[1].FileData.DisplayName != "" {
		t.Fatalf("Gemini 请求仍携带文件名: %#v", clone.Contents[0].Parts)
	}
	if clone.Config.SystemInstruction.Parts[0].InlineData.DisplayName != "" {
		t.Fatalf("Gemini 系统指令仍携带文件名: %#v", clone.Config.SystemInstruction.Parts[0])
	}
	if request.Contents[0].Parts[0].InlineData.DisplayName != "截图.png" || request.Config.SystemInstruction.Parts[0].InlineData.DisplayName != "system.txt" {
		t.Fatal("清理 Gemini 请求不应修改原始请求")
	}
}

func TestModelCapabilityProfileNormalizesLegacySchemaState(t *testing.T) {
	legacy := provider.Model{ID: "gemini-legacy", Capabilities: &provider.ModelCapabilityProfile{
		ProviderID: "gemini", ModelID: "gemini-legacy", Protocol: provider.ProtocolGemini,
		ToolCalling: provider.Support{State: provider.SupportSupported},
	}}
	got := modelCapabilityProfile(provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini}, legacy)
	if got.StructuredOutputSchema.State != provider.SupportUnknown || got.StructuredOutputSchema.Source != "legacy_default" {
		t.Fatalf("旧 Gemini capability profile 应保守补齐 schema unknown: %#v", got.StructuredOutputSchema)
	}
}

func TestAdapterProbeCapabilitiesUsesGeminiGenerateContentRoutes(t *testing.T) {
	var calls int
	client := &http.Client{Transport: geminiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || !strings.Contains(request.URL.Path, "/v1beta/models/gemini-2.5-flash:") {
			return nil, fmt.Errorf("Gemini probe route 不正确: %s %s", request.Method, request.URL.String())
		}
		wantOperation := "generateContent"
		if request.URL.Query().Get("alt") == "sse" {
			wantOperation = "streamGenerateContent"
		}
		if !strings.HasSuffix(request.URL.Path, ":"+wantOperation) {
			return nil, fmt.Errorf("Gemini probe operation 不正确: path=%s want=%s", request.URL.Path, wantOperation)
		}
		if request.Header.Get("x-goog-api-key") != "test-key" {
			return nil, fmt.Errorf("Gemini probe 未携带 API key")
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, fmt.Errorf("Gemini probe 请求 JSON 无效: %w", err)
		}
		if payload["contents"] == nil {
			return nil, fmt.Errorf("Gemini probe 缺少 contents")
		}

		if request.URL.Query().Get("alt") == "sse" {
			body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n"
			return geminiHTTPResponse(body, "text/event-stream"), nil
		}

		// RunCapabilityProbe 的四个请求顺序固定为 text、stream、tool、JSON；
		// 这里仅按请求序号选择安全 fixture，并且不执行任何工具。
		var body string
		switch calls {
		case 1:
			body = `{"candidates":[{"content":{"parts":[{"text":"OK"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
		case 2:
			return nil, fmt.Errorf("Gemini probe 非流式请求顺序错误")
		case 3:
			body = `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"abot_capability_probe_noop","args":{}}}],"role":"model"},"finishReason":"STOP"}]}`
		case 4:
			body = `{"candidates":[{"content":{"parts":[{"text":"{}"}],"role":"model"},"finishReason":"STOP"}]}`
		default:
			return nil, fmt.Errorf("Gemini probe 请求数超出预期: %d", calls)
		}
		return geminiHTTPResponse(body, "application/json"), nil
	})}
	adapter := NewAdapter(client)
	p := provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini, BaseURL: "https://example.invalid", APIKey: "test-key"}
	m := provider.Model{ID: "gemini-2.5-flash", Enabled: true}
	result, err := adapter.ProbeCapabilities(context.Background(), p, m, provider.CapabilityProbeOptions{})
	if err != nil {
		t.Fatalf("Gemini adapter 能力探测失败: %v", err)
	}
	if result.Profile.Route != "gemini" || result.Profile.ToolCalling.State != provider.SupportSupported || result.Profile.StructuredOutput.State != provider.SupportSupported || result.Profile.Streaming.State != provider.SupportSupported {
		t.Fatalf("Gemini adapter 探测 profile 不正确: %#v", result.Profile)
	}
	if calls != 4 {
		t.Fatalf("Gemini adapter 探测请求数 = %d，期望 4", calls)
	}
}

func TestAdapterProbeCapabilitiesOptionallyProbesTokenCount(t *testing.T) {
	var calls int
	client := &http.Client{Transport: geminiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if !strings.Contains(request.URL.Path, "/v1beta/models/gemini-2.5-flash:") {
			return nil, fmt.Errorf("Gemini token-count probe 路径不正确: %s", request.URL.Path)
		}
		if strings.HasSuffix(request.URL.Path, ":countTokens") {
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				return nil, err
			}
			if payload["contents"] == nil {
				return nil, fmt.Errorf("Gemini token-count probe 缺少 contents")
			}
			return geminiHTTPResponse(`{"totalTokens":17}`, "application/json"), nil
		}
		if !strings.HasSuffix(request.URL.Path, ":generateContent") {
			return nil, fmt.Errorf("Gemini 基础 probe 操作不正确: %s", request.URL.Path)
		}
		return geminiHTTPResponse(`{"candidates":[{"content":{"parts":[{"text":"OK"}],"role":"model"},"finishReason":"STOP"}]}`, "application/json"), nil
	})}
	adapter := NewAdapter(client)
	p := provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini, BaseURL: "https://example.invalid", APIKey: "test-key"}
	result, err := adapter.ProbeCapabilities(context.Background(), p, provider.Model{ID: "gemini-2.5-flash", Enabled: true}, provider.CapabilityProbeOptions{ExplicitOptionalSelection: true, IncludeTokenCount: true})
	if err != nil {
		t.Fatalf("Gemini token-count 能力探测失败: %v", err)
	}
	if calls != 2 || !result.Profile.Tokenizer.Known || result.Profile.Tokenizer.Name != provider.GeminiTokenCounterName {
		t.Fatalf("Gemini token-count probe 未建立精确 tokenizer: calls=%d tokenizer=%#v", calls, result.Profile.Tokenizer)
	}
	observation := findGeminiProbeObservation(result.Observations, "token_count")
	if observation.State != provider.SupportSupported || observation.Route != "gemini" {
		t.Fatalf("Gemini token-count observation 不正确: %#v", observation)
	}
}

func TestAdapterProbeCapabilitiesOptionalProbesUseGeminiRequestFields(t *testing.T) {
	var calls int
	client := &http.Client{Transport: geminiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, ":generateContent") {
			return nil, fmt.Errorf("Gemini optional probe route 不正确: %s %s", request.Method, request.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(payload)
		body := `{"candidates":[{"content":{"parts":[{"text":"OK"}],"role":"model"},"finishReason":"STOP"}]}`
		switch {
		case strings.Contains(string(encoded), `"responseJsonSchema"`):
			body = `{"candidates":[{"content":{"parts":[{"text":"{\"ok\":true}"}],"role":"model"},"finishReason":"STOP"}]}`
		case strings.Contains(string(encoded), `"thinkingConfig"`):
			body = `{"candidates":[{"content":{"parts":[{"thought":true},{"text":"OK"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"thoughtsTokenCount":1,"totalTokenCount":3}}`
		case strings.Contains(string(encoded), `"mimeType":"image/png"`), strings.Contains(string(encoded), `"mimeType":"application/pdf"`):
			body = `{"candidates":[{"content":{"parts":[{"text":"OK"}],"role":"model"},"finishReason":"STOP"}]}`
		}
		return geminiHTTPResponse(body, "application/json"), nil
	})}
	adapter := NewAdapter(client)
	p := provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini, BaseURL: "https://example.invalid/v1beta", APIKey: "test-key"}
	m := provider.Model{ID: "gemini-2.5-flash", Enabled: true}
	result, err := adapter.ProbeCapabilities(context.Background(), p, m, provider.CapabilityProbeOptions{
		ExplicitOptionalSelection: true,
		IncludeStructuredSchema:   true,
		IncludeReasoning:          true,
		IncludeImages:             true,
		IncludeInputFiles:         true,
	})
	if err != nil {
		t.Fatalf("Gemini optional 能力探测失败: %v", err)
	}
	if calls != 5 {
		t.Fatalf("Gemini optional 探测请求数=%d，期望基础、schema、reasoning、image、file 五次", calls)
	}
	if findGeminiProbeObservation(result.Observations, "structured_output_schema").State != provider.SupportSupported {
		t.Fatalf("Gemini schema 证据不正确: %#v", result.Observations)
	}
	if result.Profile.StructuredOutputSchema.State != provider.SupportSupported {
		t.Fatalf("Gemini schema 能力状态未应用: %#v", result.Profile.StructuredOutputSchema)
	}
	if result.Profile.JSONSchemaDialect != "gemini-json-schema-subset" {
		t.Fatalf("Gemini schema dialect 未绑定当前线路: %q", result.Profile.JSONSchemaDialect)
	}
	if findGeminiProbeObservation(result.Observations, "reasoning_effort").State != provider.SupportSupported {
		t.Fatalf("Gemini reasoning 证据不正确: %#v", result.Observations)
	}
	if result.Profile.Images.State != provider.SupportSupported || result.Profile.InputFiles.State != provider.SupportSupported {
		t.Fatalf("Gemini 多模态能力未应用: %#v", result.Profile)
	}
}

func TestAdapterProbeCapabilitiesGeminiProviderSpecificRejectionMatrix(t *testing.T) {
	tests := []struct {
		name       string
		feature    string
		configure  func(*provider.CapabilityProbeOptions)
		requestHas func(string) bool
	}{
		{
			name:    "schema subset",
			feature: "structured_output_schema",
			configure: func(options *provider.CapabilityProbeOptions) {
				options.IncludeStructuredSchema = true
			},
			requestHas: func(encoded string) bool { return strings.Contains(encoded, `"responseJsonSchema"`) },
		},
		{
			name:    "image input",
			feature: "images",
			configure: func(options *provider.CapabilityProbeOptions) {
				options.IncludeImages = true
			},
			requestHas: func(encoded string) bool { return strings.Contains(encoded, `"mimeType":"image/png"`) },
		},
		{
			name:    "file input",
			feature: "input_files",
			configure: func(options *provider.CapabilityProbeOptions) {
				options.IncludeInputFiles = true
			},
			requestHas: func(encoded string) bool { return strings.Contains(encoded, `"mimeType":"application/pdf"`) },
		},
		{
			name:    "reasoning",
			feature: "reasoning_effort",
			configure: func(options *provider.CapabilityProbeOptions) {
				options.IncludeReasoning = true
			},
			requestHas: func(encoded string) bool { return strings.Contains(encoded, `"thinkingConfig"`) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: geminiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				var payload map[string]any
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					return nil, err
				}
				encoded, _ := json.Marshal(payload)
				if tt.requestHas(string(encoded)) {
					response := geminiHTTPResponse(`{"error":{"message":"requested field is unsupported"}}`, "application/json")
					response.StatusCode = http.StatusBadRequest
					return response, nil
				}
				return geminiHTTPResponse(`{"candidates":[{"content":{"parts":[{"text":"OK"}],"role":"model"},"finishReason":"STOP"}]}`, "application/json"), nil
			})}
			adapter := NewAdapter(client)
			p := provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini, BaseURL: "https://example.invalid/v1beta", APIKey: "secret"}
			options := provider.CapabilityProbeOptions{ExplicitOptionalSelection: true}
			tt.configure(&options)
			result, err := adapter.ProbeCapabilities(context.Background(), p, provider.Model{ID: "gemini-2.5-flash", Enabled: true}, options)
			if err != nil {
				t.Fatalf("Gemini provider-specific rejection probe 失败: %v", err)
			}
			observation := findGeminiProbeObservation(result.Observations, tt.feature)
			if observation.State != provider.SupportUnsupported {
				t.Fatalf("%s 应为 unsupported，实际=%#v", tt.feature, observation)
			}
			if tt.feature == "structured_output_schema" && result.Profile.JSONSchemaDialect != "" {
				t.Fatalf("schema 拒绝后不应残留 dialect: %q", result.Profile.JSONSchemaDialect)
			}
		})
	}
}

func findGeminiProbeObservation(observations []provider.CapabilityObservation, feature string) provider.CapabilityObservation {
	for _, observation := range observations {
		if observation.Feature == feature {
			return observation
		}
	}
	return provider.CapabilityObservation{}
}

func TestAdapterCountTokensUsesOfficialGeminiEndpoint(t *testing.T) {
	var gotPath string
	var gotPayload map[string]any
	client := &http.Client{Transport: geminiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		gotPath = request.URL.Path
		if request.Method != http.MethodPost || request.Header.Get("x-goog-api-key") != "test-key" {
			return nil, fmt.Errorf("Gemini token count 请求方法或认证不正确")
		}
		if err := json.NewDecoder(request.Body).Decode(&gotPayload); err != nil {
			return nil, err
		}
		return geminiHTTPResponse(`{"totalTokens":37,"cachedContentTokenCount":2}`, "application/json"), nil
	})}
	adapter := NewAdapter(client)
	request := &adkmodel.LLMRequest{
		Model: "gemini-2.5-flash",
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("系统约束", genai.RoleUser),
			Tools:             []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "read_file"}}}},
		},
		Contents: []*genai.Content{genai.NewContentFromText("统计这段内容", genai.RoleUser)},
	}
	count, err := adapter.CountTokens(context.Background(), provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini, BaseURL: "https://example.invalid/v1beta", APIKey: "test-key"}, provider.Model{ID: "gemini-2.5-flash"}, request)
	if err != nil {
		t.Fatalf("Gemini 官方 token count 失败: %v", err)
	}
	if count.Tokens != 37 || !count.Exact || count.Source != "provider" || count.Name != provider.GeminiTokenCounterName {
		t.Fatalf("Gemini token count metadata 不正确: %#v", count)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash:countTokens" {
		t.Fatalf("Gemini token count 路径=%q", gotPath)
	}
	if gotPayload["contents"] == nil || gotPayload["systemInstruction"] == nil || gotPayload["tools"] == nil {
		t.Fatalf("Gemini token count 未携带完整请求形状: %#v", gotPayload)
	}
}

func TestAdapterCountTokensUsesExplicitAnthropicRoute(t *testing.T) {
	var gotPayload map[string]any
	client := &http.Client{Transport: geminiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/messages/count_tokens" {
			return nil, fmt.Errorf("Gemini 显式 Anthropic token count 路径不正确: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("x-api-key") != "anthropic-key" || request.Header.Get("x-goog-api-key") != "" || request.Header.Get("anthropic-version") != "2023-06-01" {
			return nil, fmt.Errorf("Gemini 显式 Anthropic token count 认证不正确: %#v", request.Header)
		}
		if err := json.NewDecoder(request.Body).Decode(&gotPayload); err != nil {
			return nil, err
		}
		return geminiHTTPResponse(`{"input_tokens":31}`, "application/json"), nil
	})}
	adapter := NewAdapter(client)
	request := &adkmodel.LLMRequest{Model: "claude-sonnet-4", Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	count, err := adapter.CountTokens(context.Background(), provider.Provider{
		ID: "gateway", Protocol: provider.ProtocolGemini, TokenCountProtocol: provider.TokenCountProtocolAnthropic,
		BaseURL: "https://gateway.example/v1", APIKey: "anthropic-key",
	}, provider.Model{ID: "claude-sonnet-4"}, request)
	if err != nil {
		t.Fatalf("Gemini 适配器显式 Anthropic token count 失败: %v", err)
	}
	if count.Tokens != 31 || !count.Exact || count.Name != provider.AnthropicTokenCounterName || count.Version != provider.AnthropicTokenCounterVersion {
		t.Fatalf("Gemini 适配器显式 Anthropic metadata 不正确: %#v", count)
	}
	if gotPayload["model"] != "claude-sonnet-4" || gotPayload["messages"] == nil {
		t.Fatalf("Gemini 适配器显式 Anthropic 请求未投影模型/消息: %#v", gotPayload)
	}
}

func TestAdapterCountTokensRedactsAPIKeyFromError(t *testing.T) {
	client := &http.Client{Transport: geminiRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := geminiHTTPResponse(`{"error":"bad test-key"}`, "application/json")
		response.StatusCode = http.StatusBadRequest
		return response, nil
	})}
	adapter := NewAdapter(client)
	_, err := adapter.CountTokens(context.Background(), provider.Provider{ID: "gemini", Protocol: provider.ProtocolGemini, BaseURL: "https://example.invalid", APIKey: "test-key"}, provider.Model{ID: "gemini-2.5-flash"}, &adkmodel.LLMRequest{Model: "gemini-2.5-flash"})
	if err == nil || strings.Contains(err.Error(), "test-key") || !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("Gemini token count 错误未脱敏: %v", err)
	}
}

func TestCountTokensEndpointAcceptsGeminiBaseURLShapes(t *testing.T) {
	cases := map[string]string{
		"https://example.invalid":               "https://example.invalid/v1beta/models/gemini-2.5-flash:countTokens",
		"https://example.invalid/v1beta":        "https://example.invalid/v1beta/models/gemini-2.5-flash:countTokens",
		"https://example.invalid/v1beta/":       "https://example.invalid/v1beta/models/gemini-2.5-flash:countTokens",
		"https://example.invalid/v1beta/models": "https://example.invalid/v1beta/models/gemini-2.5-flash:countTokens",
	}
	for base, want := range cases {
		got, err := countTokensEndpoint(base, "gemini-2.5-flash")
		if err != nil || got != want {
			t.Fatalf("base=%q endpoint=%q err=%v want=%q", base, got, err, want)
		}
	}
}

type geminiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f geminiRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func geminiHTTPResponse(body, contentType string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
