package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestCountAnthropicTokensProjectsBoundedRequest(t *testing.T) {
	var payload map[string]any
	client := &http.Client{Transport: anthropicRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/messages/count_tokens" {
			return nil, fmt.Errorf("Anthropic count route 不正确: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("x-api-key") != "secret-key" || request.Header.Get("anthropic-version") != "2023-06-01" || request.Header.Get("anthropic-beta") != "token-counting-2024-11-01" {
			return nil, fmt.Errorf("Anthropic count headers 不正确: %#v", request.Header)
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		return anthropicHTTPResponse(`{"input_tokens":57}`, http.StatusOK), nil
	})}
	request := &adkmodel.LLMRequest{
		Model: "claude-sonnet-4",
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("系统约束", genai.RoleUser),
			MaxOutputTokens:   2048,
			Temperature:       float32Ptr(0.2),
			ThinkingConfig:    &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: int32Ptr(256)},
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name: "lookup_weather", Description: "查询天气",
				Parameters: &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"city": {Type: genai.TypeString}}, Required: []string{"city"}},
			}}}},
		},
		Contents: []*genai.Content{
			genai.NewContentFromParts([]*genai.Part{
				{Text: "请查询北京天气"},
				{InlineData: &genai.Blob{Data: []byte{1, 2, 3}, MIMEType: "image/png"}},
			}, genai.RoleUser),
			genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "lookup_weather", Args: map[string]any{"city": "北京"}}}}, genai.RoleModel),
			genai.NewContentFromParts([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "lookup_weather", Response: map[string]any{"temperature": 22}}}}, genai.RoleUser),
		},
	}
	count, err := CountAnthropicTokens(context.Background(), Provider{BaseURL: "https://api.anthropic.com", APIKey: "secret-key"}, Model{ID: "claude-sonnet-4"}, request, client)
	if err != nil {
		t.Fatalf("Anthropic token count 失败: %v", err)
	}
	if count.Tokens != 57 || !count.Exact || count.Name != AnthropicTokenCounterName || count.Version != AnthropicTokenCounterVersion {
		t.Fatalf("Anthropic token count metadata 不正确: %#v", count)
	}
	if payload["model"] != "claude-sonnet-4" {
		t.Fatalf("model 未投影: %#v", payload)
	}
	if _, ok := payload["temperature"]; ok {
		t.Fatalf("count 请求不应携带 temperature: %#v", payload)
	}
	system, ok := payload["system"].([]any)
	if !ok || len(system) != 1 || system[0].(map[string]any)["text"] != "系统约束" {
		t.Fatalf("system 投影不正确: %#v", payload["system"])
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("messages 投影不正确: %#v", payload["messages"])
	}
	if messages[0].(map[string]any)["role"] != "user" || messages[1].(map[string]any)["role"] != "assistant" || messages[2].(map[string]any)["role"] != "user" {
		t.Fatalf("message role 投影不正确: %#v", messages)
	}
	assistantBlocks := messages[1].(map[string]any)["content"].([]any)
	if assistantBlocks[0].(map[string]any)["type"] != "tool_use" || assistantBlocks[0].(map[string]any)["id"] != "call-1" {
		t.Fatalf("tool_use 投影不正确: %#v", assistantBlocks)
	}
	resultBlocks := messages[2].(map[string]any)["content"].([]any)
	if resultBlocks[0].(map[string]any)["type"] != "tool_result" || resultBlocks[0].(map[string]any)["tool_use_id"] != "call-1" {
		t.Fatalf("tool_result 投影不正确: %#v", resultBlocks)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["name"] != "lookup_weather" {
		t.Fatalf("tools 投影不正确: %#v", payload["tools"])
	}
	if schema := tools[0].(map[string]any)["input_schema"].(map[string]any); schema["type"] != "OBJECT" {
		t.Fatalf("工具 schema 投影不正确: %#v", schema)
	}
	if thinking := payload["thinking"].(map[string]any); thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(256) {
		t.Fatalf("thinking 投影不正确: %#v", thinking)
	}
}

func TestAnthropicCountTokensEndpointShapes(t *testing.T) {
	cases := map[string]string{
		"":                                     "https://api.anthropic.com/v1/messages/count_tokens",
		"https://example.invalid":              "https://example.invalid/v1/messages/count_tokens",
		"https://example.invalid/v1":           "https://example.invalid/v1/messages/count_tokens",
		"https://example.invalid/v1/":          "https://example.invalid/v1/messages/count_tokens",
		"https://example.invalid/v1/messages":  "https://example.invalid/v1/messages/count_tokens",
		"https://example.invalid/v1/messages/": "https://example.invalid/v1/messages/count_tokens",
		"https://example.invalid/v1/messages/count_tokens": "https://example.invalid/v1/messages/count_tokens",
	}
	for base, want := range cases {
		got, err := anthropicCountTokensEndpoint(base)
		if err != nil || got != want {
			t.Fatalf("base=%q endpoint=%q err=%v want=%q", base, got, err, want)
		}
	}
	for _, base := range []string{"ftp://example.invalid", "not a URL", "https://"} {
		if _, err := anthropicCountTokensEndpoint(base); err == nil {
			t.Fatalf("无效 base URL 应拒绝: %q", base)
		}
	}
}

func TestCountAnthropicTokensRejectsUnsupportedShapeAndBoundsBody(t *testing.T) {
	request := &adkmodel.LLMRequest{Model: "claude", Contents: []*genai.Content{genai.NewContentFromParts([]*genai.Part{{InlineData: &genai.Blob{MIMEType: "audio/wav", Data: []byte{1}}}}, genai.RoleUser)}}
	called := false
	client := &http.Client{Transport: anthropicRoundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return anthropicHTTPResponse(`{"input_tokens":1}`, http.StatusOK), nil
	})}
	if _, err := CountAnthropicTokens(context.Background(), Provider{}, Model{ID: "claude"}, request, client); err == nil || !strings.Contains(err.Error(), "不支持 MIME") || called {
		t.Fatalf("不支持 MIME 应在请求前拒绝: err=%v called=%v", err, called)
	}

	huge := &adkmodel.LLMRequest{Model: "claude", Contents: []*genai.Content{genai.NewContentFromParts([]*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: make([]byte, maxAnthropicTokenCountBody)}}}, genai.RoleUser)}}
	if _, err := CountAnthropicTokens(context.Background(), Provider{}, Model{ID: "claude"}, huge, client); err == nil || !strings.Contains(err.Error(), "字节上限") || called {
		t.Fatalf("超大请求应在 HTTP 前拒绝: err=%v called=%v", err, called)
	}

	for _, media := range []*genai.FileData{
		{FileURI: "https://example.invalid/image.png", MIMEType: "image/png"},
		{FileURI: "https://example.invalid/file.pdf", MIMEType: "application/pdf"},
	} {
		remote := &adkmodel.LLMRequest{Model: "claude", Contents: []*genai.Content{genai.NewContentFromParts([]*genai.Part{{FileData: media}}, genai.RoleUser)}}
		if _, err := CountAnthropicTokens(context.Background(), Provider{}, Model{ID: "claude"}, remote, client); err == nil || !strings.Contains(err.Error(), "URL/file") || called {
			t.Fatalf("Anthropic count 不应接受 URL/file media: media=%#v err=%v called=%v", media, err, called)
		}
	}
}

func TestCountAnthropicTokensErrorsAreBoundedAndValidated(t *testing.T) {
	request := &adkmodel.LLMRequest{Model: "claude", Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "negative", body: `{"input_tokens":-1}`, status: http.StatusOK},
		{name: "missing", body: `{}`, status: http.StatusOK},
		{name: "malformed", body: `{`, status: http.StatusOK},
		{name: "redacted", body: `{"error":"bad secret-key api_key=secret-key"}`, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: anthropicRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return anthropicHTTPResponse(tc.body, tc.status), nil
			})}
			_, err := CountAnthropicTokens(context.Background(), Provider{BaseURL: "https://example.invalid", APIKey: "secret-key"}, Model{ID: "claude"}, request, client)
			if err == nil {
				t.Fatal("非法响应应返回错误")
			}
			if strings.Contains(err.Error(), "secret-key") || (tc.name == "redacted" && !strings.Contains(err.Error(), "REDACTED")) {
				t.Fatalf("错误正文未正确脱敏: %v", err)
			}
		})
	}

	missingID := &adkmodel.LLMRequest{Model: "claude", Contents: []*genai.Content{genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "tool"}}}, genai.RoleModel)}}
	if _, err := CountAnthropicTokens(context.Background(), Provider{}, Model{ID: "claude"}, missingID, nil); err == nil || !strings.Contains(err.Error(), "稳定 ID") {
		t.Fatalf("缺少 function call ID 应拒绝精确计数: %v", err)
	}
}

func TestAnthropicTokenCounterProfileIsExactProviderEvidence(t *testing.T) {
	profile := AnthropicTokenCounterProfile()
	if !profile.Known || profile.Source != "provider" || profile.Quality != "provider" || profile.Name != AnthropicTokenCounterName {
		t.Fatalf("Anthropic tokenizer profile 不正确: %#v", profile)
	}
}

type anthropicRoundTripFunc func(*http.Request) (*http.Response, error)

func (f anthropicRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func anthropicHTTPResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func float32Ptr(value float32) *float32 { return &value }

func int32Ptr(value int32) *int32 { return &value }
