package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// WireFormat 是 OpenAI 兼容服务实际采用的线路格式。
type WireFormat string

const (
	WireFormatAuto      WireFormat = "auto"
	WireFormatChat      WireFormat = "chat"
	WireFormatResponses WireFormat = "responses"
)

// ClientConfig 配置 OpenAI 兼容模型。BaseURL 可以是 API 根地址，也可以已经带有
// /v1、/chat/completions 或 /responses 后缀。
type ClientConfig struct {
	APIKey     string
	BaseURL    string
	Format     WireFormat
	HTTPClient *http.Client
	Timeout    time.Duration
}

// Adapter 是供应商管理层使用的 OpenAI 协议适配器。
type Adapter struct {
	client *http.Client
}

// NewAdapter 创建适配器；传入自定义 HTTP Client 便于代理和测试注入。
func NewAdapter(client *http.Client) *Adapter {
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return &Adapter{client: client}
}

// BuildModel 将供应商实体转成 ADK 的统一 LLM 接口。
func (a *Adapter) BuildModel(ctx context.Context, p provider.Provider, modelID string) (adkmodel.LLM, error) {
	return NewModel(ctx, modelID, &ClientConfig{
		APIKey:     p.APIKey,
		BaseURL:    p.BaseURL,
		Format:     WireFormat(p.OpenAIFormat),
		HTTPClient: a.client,
	})
}

// ProbeCapabilities runs the provider-neutral, bounded capability probes
// against the exact OpenAI-compatible route that BuildModel would use. The
// probe only sends a no-op declaration for tool support and never executes a
// returned function call.
func (a *Adapter) ProbeCapabilities(ctx context.Context, p provider.Provider, m provider.Model, options provider.CapabilityProbeOptions) (provider.CapabilityProbeResult, error) {
	llm, err := a.BuildModel(ctx, p, m.ID)
	if err != nil {
		return provider.CapabilityProbeResult{}, err
	}
	profile := modelCapabilityProfile(p, m)
	if model, ok := llm.(*Model); ok {
		profile.Route = string(model.effectiveFormat())
	}
	result, probeErr := provider.RunCapabilityProbe(ctx, llm, profile, options)
	if probeErr != nil || !options.IncludeTokenCount {
		return result, probeErr
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tokenCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	countRequest := &adkmodel.LLMRequest{
		Model:    m.ID,
		Contents: []*genai.Content{genai.NewContentFromText("Count this bounded probe.", genai.RoleUser)},
	}
	count, countErr := a.CountTokens(tokenCtx, p, m, countRequest)
	provider.ApplyTokenCountProbe(&result, profile.Route, count, countErr)
	return result, nil
}

// CountTokens uses OpenAI's Responses input-token count endpoint for the same
// materialized request shape that the model call would send. Chat Completions
// requests are projected to the equivalent Responses input shape because the
// official OpenAI count endpoint is shared by both routes. Arbitrary compatible
// gateways may not expose that endpoint; those cases return the upstream error
// (or ErrTokenCounterUnavailable when the protocol has no counter) and the
// Context Builder retains its bounded heuristic estimate.
func (a *Adapter) CountTokens(ctx context.Context, p provider.Provider, m provider.Model, request *adkmodel.LLMRequest) (provider.TokenCount, error) {
	if p.TokenCountProtocol == provider.TokenCountProtocolAnthropic {
		return provider.CountAnthropicTokens(ctx, p, m, request, a.client)
	}
	if request == nil {
		return provider.TokenCount{}, fmt.Errorf("OpenAI token count 请求不能为空")
	}
	modelID := strings.TrimSpace(m.ID)
	if modelID == "" {
		modelID = strings.TrimSpace(request.Model)
	}
	if modelID == "" {
		return provider.TokenCount{}, fmt.Errorf("OpenAI token count 缺少模型 ID")
	}
	model, err := NewModel(ctx, modelID, &ClientConfig{
		APIKey: p.APIKey, BaseURL: p.BaseURL, Format: WireFormat(p.OpenAIFormat), HTTPClient: a.client,
	})
	if err != nil {
		return provider.TokenCount{}, err
	}
	if _, ok := model.(*Model); !ok {
		return provider.TokenCount{}, provider.ErrTokenCounterUnavailable
	}
	converted, err := ConvertResponsesRequest(request)
	if err != nil {
		return provider.TokenCount{}, err
	}
	if strings.TrimSpace(converted.Model) == "" {
		converted.Model = modelID
	}
	// The input-token endpoint accepts the request's context-bearing fields,
	// but generation-only fields (temperature, max_output_tokens and stream)
	// must not be copied into the count request. Keeping this projection
	// explicit prevents a compatible gateway from interpreting a count call as
	// a generation request.
	payload := inputTokenCountRequest{
		Model: converted.Model, Input: converted.Input, Instructions: converted.Instructions,
		Tools: converted.Tools, ToolChoice: converted.ToolChoice, Text: converted.Text, Reasoning: converted.Reasoning,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return provider.TokenCount{}, fmt.Errorf("编码 OpenAI token count 请求失败: %w", err)
	}
	endpoint, err := inputTokensEndpoint(p.BaseURL)
	if err != nil {
		return provider.TokenCount{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return provider.TokenCount{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	setAuthHeaders(httpRequest, p.APIKey)
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return provider.TokenCount{}, fmt.Errorf("请求 OpenAI token count 失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1*1024*1024))
	if err != nil {
		return provider.TokenCount{}, fmt.Errorf("读取 OpenAI token count 失败: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.TokenCount{}, fmt.Errorf("OpenAI token count 返回 HTTP %d: %s", response.StatusCode, safeOpenAIErrorBody(responseBody, p.APIKey))
	}
	var envelope struct {
		Object      string `json:"object"`
		InputTokens *int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return provider.TokenCount{}, fmt.Errorf("解析 OpenAI token count 失败: %w", err)
	}
	if envelope.InputTokens == nil || *envelope.InputTokens < 0 || uint64(*envelope.InputTokens) > uint64(^uint(0)>>1) {
		return provider.TokenCount{}, fmt.Errorf("OpenAI token count 响应缺少有效 input_tokens")
	}
	if object := strings.TrimSpace(envelope.Object); object != "" && object != "response.input_tokens" {
		return provider.TokenCount{}, fmt.Errorf("OpenAI token count 响应 object 无效")
	}
	return provider.TokenCount{
		Tokens: int(*envelope.InputTokens), Name: provider.OpenAIInputTokenCounterName,
		Version: provider.OpenAIInputTokenCounterVersion, Source: "provider", Quality: "provider", Exact: true,
	}, nil
}

// inputTokenCountRequest is deliberately narrower than ResponsesRequest:
// OpenAI's count endpoint accepts input/context fields, not generation knobs.
type inputTokenCountRequest struct {
	Model        string               `json:"model,omitempty"`
	Input        []map[string]any     `json:"input,omitempty"`
	Instructions string               `json:"instructions,omitempty"`
	Tools        []ResponseTool       `json:"tools,omitempty"`
	ToolChoice   any                  `json:"tool_choice,omitempty"`
	Text         *ResponsesTextConfig `json:"text,omitempty"`
	Reasoning    *ResponsesReasoning  `json:"reasoning,omitempty"`
}

func modelCapabilityProfile(p provider.Provider, m provider.Model) provider.ModelCapabilityProfile {
	if m.Capabilities != nil {
		return provider.NormalizeCapabilityProfile(*m.Capabilities)
	}
	return provider.DefaultCapabilities(p, m)
}

// TestConnection 通过模型目录接口检查凭据和地址；目录为空的兼容服务仍视为连接成功。
func (a *Adapter) TestConnection(ctx context.Context, p provider.Provider) (provider.ProbeResult, error) {
	models, err := a.DiscoverModels(ctx, p)
	if err != nil {
		return provider.ProbeResult{}, err
	}
	return provider.ProbeResult{Message: fmt.Sprintf("连接正常，发现 %d 个模型", len(models)), Models: models}, nil
}

// DiscoverModels 读取 OpenAI 风格的 GET /models 响应。
func (a *Adapter) DiscoverModels(ctx context.Context, p provider.Provider) ([]provider.Model, error) {
	endpoint, err := modelsEndpoint(p.BaseURL)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	setAuthHeaders(request, p.APIKey)
	response, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求模型目录失败: %w", err)
	}
	defer response.Body.Close()
	body, err := readBody(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, upstreamError(response.StatusCode, body)
	}
	var envelope struct {
		Data []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			DisplayName     string `json:"display_name"`
			ContextWindow   int    `json:"context_window"`
			MaxOutputTokens int    `json:"max_output_tokens"`
			ContextLength   int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("解析模型目录失败: %w", err)
	}
	result := make([]provider.Model, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = strings.TrimSpace(item.Name)
		}
		if id == "" {
			continue
		}
		contextWindow := item.ContextWindow
		if contextWindow == 0 {
			contextWindow = item.ContextLength
		}
		result = append(result, provider.Model{
			ID:              id,
			DisplayName:     firstNonEmpty(item.DisplayName, id),
			Enabled:         true,
			Source:          "discovered",
			ContextWindow:   contextWindow,
			MaxOutputTokens: item.MaxOutputTokens,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// Model 实现 ADK model.LLM，并在这里统一处理 Chat Completions 与 Responses。
type Model struct {
	name    string
	apiKey  string
	baseURL string
	format  WireFormat
	client  *http.Client
}

// NewModel 创建一个支持两种 OpenAI 兼容线路的 ADK 模型。
func NewModel(_ context.Context, modelName string, cfg *ClientConfig) (adkmodel.LLM, error) {
	if strings.TrimSpace(modelName) == "" {
		return nil, errors.New("模型 ID 不能为空")
	}
	if cfg == nil {
		cfg = &ClientConfig{}
	}
	if _, err := parseBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	format := cfg.Format
	if format == "" {
		format = WireFormatAuto
	}
	if format != WireFormatAuto && format != WireFormatChat && format != WireFormatResponses {
		return nil, fmt.Errorf("不支持的 OpenAI 线路格式 %q", format)
	}
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 120 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &Model{
		name:    modelName,
		apiKey:  cfg.APIKey,
		baseURL: strings.TrimSpace(cfg.BaseURL),
		format:  format,
		client:  client,
	}, nil
}

func (m *Model) Name() string { return m.name }

// GenerateContent 将 ADK 请求转换为上游 JSON，再把响应转换回 ADK 事件。
func (m *Model) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if req == nil {
		return errorSequence(errors.New("LLM 请求不能为空"))
	}
	format := m.effectiveFormat()
	var payload any
	var endpoint string
	var err error
	switch format {
	case WireFormatResponses:
		converted, convertErr := ConvertResponsesRequest(req)
		if convertErr != nil {
			return errorSequence(convertErr)
		}
		converted.Stream = stream
		if converted.Model == "" {
			converted.Model = m.name
		}
		payload = converted
		endpoint, err = completionEndpoint(m.baseURL, format)
	case WireFormatChat:
		converted, convertErr := ConvertChatRequest(req)
		if convertErr != nil {
			return errorSequence(convertErr)
		}
		converted.Stream = stream
		if converted.Model == "" {
			converted.Model = m.name
		}
		payload = converted
		endpoint, err = completionEndpoint(m.baseURL, format)
	default:
		return errorSequence(fmt.Errorf("未解析的 OpenAI 线路格式 %q", format))
	}
	if err != nil {
		return errorSequence(err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errorSequence(fmt.Errorf("编码上游请求失败: %w", err))
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			yield(nil, err)
			return
		}
		request.Header.Set("Content-Type", "application/json")
		setAuthHeaders(request, m.apiKey)
		response, err := m.client.Do(request)
		if err != nil {
			yield(nil, fmt.Errorf("调用 OpenAI 兼容模型失败: %w", err))
			return
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			responseBody, readErr := readBody(response.Body)
			if readErr != nil {
				yield(nil, readErr)
				return
			}
			yield(nil, upstreamError(response.StatusCode, responseBody))
			return
		}
		if stream {
			if format == WireFormatResponses {
				readResponsesStream(response.Body, yield)
			} else {
				readChatStream(response.Body, yield)
			}
			return
		}
		responseBody, err := readBody(response.Body)
		if err != nil {
			yield(nil, err)
			return
		}
		var converted *adkmodel.LLMResponse
		if format == WireFormatResponses {
			converted, err = parseResponsesResponse(responseBody)
		} else {
			converted, err = parseChatResponse(responseBody)
		}
		if err != nil {
			yield(nil, err)
			return
		}
		yield(converted, nil)
	}
}

func (m *Model) effectiveFormat() WireFormat {
	if m.format != WireFormatAuto {
		return m.format
	}
	parsed, err := url.Parse(m.baseURL)
	if err == nil && strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/responses") {
		return WireFormatResponses
	}
	// Chat Completions 的部署数量更广；未指定时默认走 chat，用户可显式切换 responses。
	return WireFormatChat
}

// ChatRequest 是可被大多数 OpenAI 兼容网关接受的 Chat Completions 请求。
type ChatRequest struct {
	Model            string              `json:"model"`
	Messages         []ChatMessage       `json:"messages"`
	Temperature      *float32            `json:"temperature,omitempty"`
	TopP             *float32            `json:"top_p,omitempty"`
	MaxTokens        *int32              `json:"max_tokens,omitempty"`
	PresencePenalty  *float32            `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float32            `json:"frequency_penalty,omitempty"`
	Stop             []string            `json:"stop,omitempty"`
	Tools            []ChatTool          `json:"tools,omitempty"`
	ToolChoice       any                 `json:"tool_choice,omitempty"`
	ResponseFormat   *ChatResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort  string              `json:"reasoning_effort,omitempty"`
	Stream           bool                `json:"stream,omitempty"`
	StreamOptions    map[string]any      `json:"stream_options,omitempty"`
}

type ChatResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *ChatJSONSchema `json:"json_schema,omitempty"`
}

type ChatJSONSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict,omitempty"`
	Schema map[string]any `json:"schema"`
}

// ChatMessage 是 Chat Completions 的一条消息。
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// ChatContentPart 表示 Chat Completions 的文本、图片或文件输入。
type ChatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
	File     *ChatFile     `json:"file,omitempty"`
}

type ChatImageURL struct {
	URL string `json:"url"`
}

// ChatFile 是 OpenAI Chat Completions 的标准文件内容对象。
type ChatFile struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

// ChatTool 和 ChatToolCall 分别表示工具声明和历史中的工具调用。
type ChatTool struct {
	Type     string           `json:"type"`
	Function ChatToolFunction `json:"function"`
}

type ChatToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type ChatToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function ChatToolCallFunction `json:"function"`
}

type ChatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ResponsesRequest 是 Responses API 的通用 JSON 请求。
type ResponsesRequest struct {
	Model           string               `json:"model"`
	Input           []map[string]any     `json:"input,omitempty"`
	Instructions    string               `json:"instructions,omitempty"`
	Temperature     *float32             `json:"temperature,omitempty"`
	TopP            *float32             `json:"top_p,omitempty"`
	MaxOutputTokens *int32               `json:"max_output_tokens,omitempty"`
	Tools           []ResponseTool       `json:"tools,omitempty"`
	ToolChoice      any                  `json:"tool_choice,omitempty"`
	Text            *ResponsesTextConfig `json:"text,omitempty"`
	Reasoning       *ResponsesReasoning  `json:"reasoning,omitempty"`
	Stream          bool                 `json:"stream,omitempty"`
}

type ResponsesTextConfig struct {
	Format *ResponsesTextFormat `json:"format,omitempty"`
}

type ResponsesTextFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name,omitempty"`
	Strict bool           `json:"strict,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
}

const maxResponseJSONSchemaBytes = 64 * 1024

type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type ResponseTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// ConvertChatRequest 将 ADK 的内容、工具和生成参数转换成 Chat Completions 格式。
func ConvertChatRequest(req *adkmodel.LLMRequest) (ChatRequest, error) {
	if req == nil {
		return ChatRequest{}, errors.New("LLM 请求不能为空")
	}
	result := ChatRequest{Model: req.Model}
	if req.Config != nil {
		result.Temperature = req.Config.Temperature
		result.TopP = req.Config.TopP
		result.MaxTokens = positiveInt32(req.Config.MaxOutputTokens)
		result.PresencePenalty = req.Config.PresencePenalty
		result.FrequencyPenalty = req.Config.FrequencyPenalty
		result.Stop = append([]string(nil), req.Config.StopSequences...)
		tools, err := convertChatTools(req.Config.Tools)
		if err != nil {
			return ChatRequest{}, err
		}
		result.Tools = tools
		result.ToolChoice = convertToolChoice(req.Config.ToolConfig)
		if req.Config.ResponseJsonSchema != nil {
			schema, schemaErr := responseJSONSchema(req.Config.ResponseJsonSchema)
			if schemaErr != nil {
				return ChatRequest{}, schemaErr
			}
			result.ResponseFormat = &ChatResponseFormat{Type: "json_schema", JSONSchema: &ChatJSONSchema{Name: "abot_response", Strict: true, Schema: schema}}
		} else if req.Config.ResponseMIMEType == "application/json" {
			result.ResponseFormat = &ChatResponseFormat{Type: "json_object"}
		}
		if req.Config.ThinkingConfig != nil {
			result.ReasoningEffort = thinkingEffort(req.Config.ThinkingConfig)
		}
		if system := contentText(req.Config.SystemInstruction); system != "" {
			result.Messages = append(result.Messages, ChatMessage{Role: "system", Content: system})
		}
	}
	for _, content := range req.Contents {
		if err := appendChatContent(&result.Messages, content); err != nil {
			return ChatRequest{}, err
		}
	}
	return result, nil
}

// ConvertResponsesRequest 将 ADK 的内容转换成 Responses API 的 input item。
func ConvertResponsesRequest(req *adkmodel.LLMRequest) (ResponsesRequest, error) {
	if req == nil {
		return ResponsesRequest{}, errors.New("LLM 请求不能为空")
	}
	result := ResponsesRequest{Model: req.Model}
	if req.Config != nil {
		result.Temperature = req.Config.Temperature
		result.TopP = req.Config.TopP
		result.MaxOutputTokens = positiveInt32(req.Config.MaxOutputTokens)
		tools, err := convertResponseTools(req.Config.Tools)
		if err != nil {
			return ResponsesRequest{}, err
		}
		result.Tools = tools
		result.ToolChoice = convertToolChoice(req.Config.ToolConfig)
		if req.Config.ResponseJsonSchema != nil {
			schema, schemaErr := responseJSONSchema(req.Config.ResponseJsonSchema)
			if schemaErr != nil {
				return ResponsesRequest{}, schemaErr
			}
			result.Text = &ResponsesTextConfig{Format: &ResponsesTextFormat{Type: "json_schema", Name: "abot_response", Strict: true, Schema: schema}}
		} else if req.Config.ResponseMIMEType == "application/json" {
			result.Text = &ResponsesTextConfig{Format: &ResponsesTextFormat{Type: "json_object"}}
		}
		if req.Config.ThinkingConfig != nil {
			reasoning := &ResponsesReasoning{Effort: thinkingEffort(req.Config.ThinkingConfig)}
			if req.Config.ThinkingConfig.IncludeThoughts {
				reasoning.Summary = "auto"
			}
			if reasoning.Effort != "" || reasoning.Summary != "" {
				result.Reasoning = reasoning
			}
		}
		result.Instructions = contentText(req.Config.SystemInstruction)
	}
	for _, content := range req.Contents {
		if err := appendResponseContent(&result.Input, content); err != nil {
			return ResponsesRequest{}, err
		}
	}
	return result, nil
}

func positiveInt32(value int32) *int32 {
	if value <= 0 {
		return nil
	}
	return &value
}

func responseJSONSchema(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("编码 OpenAI JSON Schema 失败: %w", err)
	}
	if len(encoded) > maxResponseJSONSchemaBytes {
		return nil, fmt.Errorf("OpenAI JSON Schema 超过 %d 字节上限", maxResponseJSONSchemaBytes)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil || len(schema) == 0 {
		return nil, fmt.Errorf("OpenAI JSON Schema 必须是非空对象")
	}
	return schema, nil
}

func thinkingEffort(config *genai.ThinkingConfig) string {
	if config == nil {
		return ""
	}
	switch config.ThinkingLevel {
	case genai.ThinkingLevelMinimal:
		return "minimal"
	case genai.ThinkingLevelLow:
		return "low"
	case genai.ThinkingLevelMedium:
		return "medium"
	case genai.ThinkingLevelHigh:
		return "high"
	default:
		return ""
	}
}

func appendChatContent(messages *[]ChatMessage, content *genai.Content) error {
	if content == nil {
		return nil
	}
	role := normalizeRole(content.Role)
	var textParts []ChatContentPart
	var toolCalls []ChatToolCall
	var responses []*genai.FunctionResponse
	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		switch {
		case part.Text != "":
			textParts = append(textParts, ChatContentPart{Type: "text", Text: part.Text})
		case part.InlineData != nil:
			if isImageMIME(part.InlineData.MIMEType) {
				textParts = append(textParts, ChatContentPart{Type: "image_url", ImageURL: &ChatImageURL{URL: dataURL(part.InlineData.MIMEType, part.InlineData.Data)}})
				break
			}
			textParts = append(textParts, ChatContentPart{Type: "file", File: &ChatFile{
				Filename: part.InlineData.DisplayName,
				FileData: dataURL(part.InlineData.MIMEType, part.InlineData.Data),
			}})
		case part.FileData != nil:
			if isImageMIME(part.FileData.MIMEType) {
				textParts = append(textParts, ChatContentPart{Type: "image_url", ImageURL: &ChatImageURL{URL: part.FileData.FileURI}})
				break
			}
			file, err := chatFileFromURI(part.FileData)
			if err != nil {
				return err
			}
			textParts = append(textParts, ChatContentPart{Type: "file", File: file})
		case part.FunctionCall != nil:
			args, err := jsonString(part.FunctionCall.Args)
			if err != nil {
				return fmt.Errorf("编码工具调用 %q 参数失败: %w", part.FunctionCall.Name, err)
			}
			toolCalls = append(toolCalls, ChatToolCall{
				ID:       firstNonEmpty(part.FunctionCall.ID, "call_"+part.FunctionCall.Name),
				Type:     "function",
				Function: ChatToolCallFunction{Name: part.FunctionCall.Name, Arguments: args},
			})
		case part.FunctionResponse != nil:
			responses = append(responses, part.FunctionResponse)
		default:
			return fmt.Errorf("openai chat 不支持 ADK 内容分片 %T", part)
		}
	}
	if len(textParts) > 0 || len(toolCalls) > 0 {
		message := ChatMessage{Role: role, ToolCalls: toolCalls}
		message.Content = chatContentValue(textParts)
		if len(toolCalls) > 0 {
			message.Role = "assistant"
		}
		*messages = append(*messages, message)
	}
	for _, response := range responses {
		output, err := jsonString(response.Response)
		if err != nil {
			return fmt.Errorf("编码工具 %q 返回值失败: %w", response.Name, err)
		}
		callID := strings.TrimSpace(response.ID)
		if callID == "" {
			// 某些 ADK 调用链只保留函数名；优先在本条内容和历史中找最近一次调用。
			for index := len(toolCalls) - 1; index >= 0 && callID == ""; index-- {
				if toolCalls[index].Function.Name == response.Name {
					callID = toolCalls[index].ID
				}
			}
			for index := len(*messages) - 1; index >= 0 && callID == ""; index-- {
				for callIndex := len((*messages)[index].ToolCalls) - 1; callIndex >= 0; callIndex-- {
					call := (*messages)[index].ToolCalls[callIndex]
					if call.Function.Name == response.Name {
						callID = call.ID
						break
					}
				}
			}
		}
		*messages = append(*messages, ChatMessage{
			Role:       "tool",
			ToolCallID: firstNonEmpty(callID, "call_"+response.Name),
			Content:    output,
			Name:       response.Name,
		})
	}
	return nil
}

func appendResponseContent(items *[]map[string]any, content *genai.Content) error {
	if content == nil {
		return nil
	}
	role := normalizeRole(content.Role)
	var messageContent []map[string]any
	var functionCalls []map[string]any
	var functionResponses []*genai.FunctionResponse
	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		switch {
		case part.Text != "":
			typeName := "input_text"
			if role == "assistant" {
				typeName = "output_text"
			}
			messageContent = append(messageContent, map[string]any{"type": typeName, "text": part.Text})
		case part.InlineData != nil:
			if isImageMIME(part.InlineData.MIMEType) {
				messageContent = append(messageContent, map[string]any{
					"type":      "input_image",
					"image_url": dataURL(part.InlineData.MIMEType, part.InlineData.Data),
				})
				break
			}
			file := map[string]any{
				"type":      "input_file",
				"file_data": dataURL(part.InlineData.MIMEType, part.InlineData.Data),
			}
			if name := strings.TrimSpace(part.InlineData.DisplayName); name != "" {
				file["filename"] = name
			}
			messageContent = append(messageContent, file)
		case part.FileData != nil:
			if isImageMIME(part.FileData.MIMEType) {
				messageContent = append(messageContent, map[string]any{
					"type":      "input_image",
					"image_url": part.FileData.FileURI,
				})
				break
			}
			file, err := responseFileFromURI(part.FileData)
			if err != nil {
				return err
			}
			messageContent = append(messageContent, file)
		case part.FunctionCall != nil:
			args, err := jsonString(part.FunctionCall.Args)
			if err != nil {
				return fmt.Errorf("编码 Responses 工具调用参数失败: %w", err)
			}
			functionCalls = append(functionCalls, map[string]any{
				"type":      "function_call",
				"call_id":   firstNonEmpty(part.FunctionCall.ID, "call_"+part.FunctionCall.Name),
				"name":      part.FunctionCall.Name,
				"arguments": args,
			})
		case part.FunctionResponse != nil:
			functionResponses = append(functionResponses, part.FunctionResponse)
		default:
			return fmt.Errorf("openai responses 不支持 ADK 内容分片 %T", part)
		}
	}
	if len(messageContent) > 0 {
		message := map[string]any{"role": role, "content": messageContent}
		if role == "assistant" {
			message["type"] = "message"
		}
		*items = append(*items, message)
	}
	*items = append(*items, functionCalls...)
	for _, response := range functionResponses {
		output, err := jsonString(response.Response)
		if err != nil {
			return fmt.Errorf("编码 Responses 工具返回值失败: %w", err)
		}
		callID := strings.TrimSpace(response.ID)
		if callID == "" {
			callID = findResponseCallID(*items, response.Name)
		}
		*items = append(*items, map[string]any{
			"type":    "function_call_output",
			"call_id": firstNonEmpty(callID, "call_"+response.Name),
			"output":  output,
		})
	}
	return nil
}

// isImageMIME 判断媒体是否可以直接映射到 OpenAI 的图片输入字段。
func isImageMIME(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "image/")
}

// chatFileFromURI 将 ADK 的 URI 文件转换成 Chat Completions 支持的 file_id 或 data URL。
func chatFileFromURI(fileData *genai.FileData) (*ChatFile, error) {
	if fileData == nil {
		return nil, errors.New("OpenAI Chat 文件内容不能为空")
	}
	file := &ChatFile{Filename: strings.TrimSpace(fileData.DisplayName)}
	uri := strings.TrimSpace(fileData.FileURI)
	switch {
	case strings.HasPrefix(strings.ToLower(uri), "data:"):
		file.FileData = uri
	case strings.HasPrefix(uri, "file-"):
		file.FileID = uri
	default:
		return nil, fmt.Errorf("openai chat 文件 URI %q 不是 data URL 或 file_id", uri)
	}
	return file, nil
}

// responseFileFromURI 将 URI 文件转换成 Responses 的 input_file 内容分片。
func responseFileFromURI(fileData *genai.FileData) (map[string]any, error) {
	if fileData == nil {
		return nil, errors.New("OpenAI Responses 文件内容不能为空")
	}
	item := map[string]any{"type": "input_file"}
	if name := strings.TrimSpace(fileData.DisplayName); name != "" {
		item["filename"] = name
	}
	uri := strings.TrimSpace(fileData.FileURI)
	switch {
	case strings.HasPrefix(strings.ToLower(uri), "data:"):
		item["file_data"] = uri
	case strings.HasPrefix(strings.ToLower(uri), "http://"), strings.HasPrefix(strings.ToLower(uri), "https://"):
		item["file_url"] = uri
	case strings.HasPrefix(uri, "file-"):
		item["file_id"] = uri
	default:
		return nil, fmt.Errorf("openai responses 文件 URI %q 不是 data URL、HTTP URL 或 file_id", uri)
	}
	return item, nil
}

// findResponseCallID 从 Responses 历史输入中找到最近的同名函数调用。
func findResponseCallID(items []map[string]any, name string) string {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item["type"] != "function_call" || item["name"] != name {
			continue
		}
		if callID, ok := item["call_id"].(string); ok && strings.TrimSpace(callID) != "" {
			return callID
		}
	}
	return ""
}

func chatContentValue(parts []ChatContentPart) any {
	if len(parts) == 1 && parts[0].Type == "text" {
		return parts[0].Text
	}
	return parts
}

func convertChatTools(tools []*genai.Tool) ([]ChatTool, error) {
	var result []ChatTool
	for index, item := range tools {
		if item == nil || len(item.FunctionDeclarations) == 0 {
			return nil, fmt.Errorf("第 %d 个工具不是函数工具", index+1)
		}
		for _, declaration := range item.FunctionDeclarations {
			parameters, err := declarationParameters(declaration)
			if err != nil {
				return nil, err
			}
			result = append(result, ChatTool{
				Type: "function",
				Function: ChatToolFunction{
					Name:        declaration.Name,
					Description: declaration.Description,
					Parameters:  parameters,
				},
			})
		}
	}
	return result, nil
}

func convertResponseTools(tools []*genai.Tool) ([]ResponseTool, error) {
	var result []ResponseTool
	for index, item := range tools {
		if item == nil || len(item.FunctionDeclarations) == 0 {
			return nil, fmt.Errorf("第 %d 个工具不是函数工具", index+1)
		}
		for _, declaration := range item.FunctionDeclarations {
			parameters, err := declarationParameters(declaration)
			if err != nil {
				return nil, err
			}
			result = append(result, ResponseTool{
				Type:        "function",
				Name:        declaration.Name,
				Description: declaration.Description,
				Parameters:  parameters,
			})
		}
	}
	return result, nil
}

func declarationParameters(declaration *genai.FunctionDeclaration) (map[string]any, error) {
	if declaration == nil || strings.TrimSpace(declaration.Name) == "" {
		return nil, errors.New("函数工具缺少名称")
	}
	var raw any
	if declaration.Parameters != nil {
		raw = declaration.Parameters
	} else if declaration.ParametersJsonSchema != nil {
		raw = declaration.ParametersJsonSchema
	}
	if raw == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("编码函数 %q 参数 schema 失败: %w", declaration.Name, err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("解析函数 %q 参数 schema 失败: %w", declaration.Name, err)
	}
	normalizeSchema(result)
	return result, nil
}

func normalizeSchema(value any) {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if key == "type" {
				if typeName, ok := child.(string); ok {
					item[key] = strings.ToLower(typeName)
				}
			}
			if key == "propertyOrdering" {
				delete(item, key)
				continue
			}
			normalizeSchema(child)
		}
	case []any:
		for _, child := range item {
			normalizeSchema(child)
		}
	}
}

func convertToolChoice(config *genai.ToolConfig) any {
	if config == nil || config.FunctionCallingConfig == nil {
		return nil
	}
	choice := config.FunctionCallingConfig
	switch choice.Mode {
	case "", genai.FunctionCallingConfigModeUnspecified, genai.FunctionCallingConfigModeAuto:
		if len(choice.AllowedFunctionNames) == 0 {
			return "auto"
		}
		if len(choice.AllowedFunctionNames) == 1 {
			return map[string]any{"type": "function", "function": map[string]any{"name": choice.AllowedFunctionNames[0]}}
		}
		return "auto"
	case genai.FunctionCallingConfigModeNone:
		return "none"
	case genai.FunctionCallingConfigModeAny:
		return "required"
	default:
		return "auto"
	}
}

func parseChatResponse(body []byte) (*adkmodel.LLMResponse, error) {
	var response chatCompletionResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("解析 Chat Completions 响应失败: %w", err)
	}
	if len(response.Choices) == 0 {
		return nil, errors.New("Chat Completions 响应没有 choices")
	}
	choice := response.Choices[0]
	return chatLLMResponse(response.ID, response.Model, choice.Message.Content, choice.Message.ToolCalls, choice.FinishReason, response.Usage, false)
}

type chatCompletionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      chatResponseMessage `json:"message"`
		FinishReason string              `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage,omitempty"`
}

type chatResponseMessage struct {
	Content   json.RawMessage `json:"content"`
	ToolCalls []ChatToolCall  `json:"tool_calls,omitempty"`
}

type chatUsage struct {
	PromptTokens           int32                     `json:"prompt_tokens"`
	CompletionTokens       int32                     `json:"completion_tokens"`
	TotalTokens            int32                     `json:"total_tokens"`
	ReasoningTokens        int32                     `json:"reasoning_tokens,omitempty"`
	CompletionTokenDetails *openAIOutputTokenDetails `json:"completion_tokens_details,omitempty"`
}

func (u *chatUsage) reasoningTokens() int32 {
	if u == nil {
		return 0
	}
	if u.ReasoningTokens > 0 {
		return u.ReasoningTokens
	}
	if u.CompletionTokenDetails != nil {
		return u.CompletionTokenDetails.ReasoningTokens
	}
	return 0
}

func chatLLMResponse(id, modelName string, content json.RawMessage, calls []ChatToolCall, finish string, usage *chatUsage, partial bool) (*adkmodel.LLMResponse, error) {
	parts := make([]*genai.Part, 0)
	if text := rawText(content); text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}
	for _, call := range calls {
		args := map[string]any{}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return nil, fmt.Errorf("解析 Chat 工具 %q 参数失败: %w", call.Function.Name, err)
			}
		}
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID: call.ID, Name: call.Function.Name, Args: args,
		}})
	}
	result := &adkmodel.LLMResponse{
		Partial:        partial,
		Content:        contentFromParts(parts),
		FinishReason:   finishReason(finish),
		CustomMetadata: openAIMetadata(id, modelName, WireFormatChat),
	}
	if usage != nil {
		result.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     usage.PromptTokens,
			CandidatesTokenCount: usage.CompletionTokens,
			TotalTokenCount:      usage.TotalTokens,
			ThoughtsTokenCount:   usage.reasoningTokens(),
		}
	}
	if !partial {
		result.TurnComplete = true
	}
	return result, nil
}

func readChatStream(body io.Reader, yield func(*adkmodel.LLMResponse, error) bool) {
	state := &chatStreamState{calls: make(map[int]*streamToolCall)}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := bytes.TrimSpace([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		if len(data) == 0 {
			continue
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			yield(nil, fmt.Errorf("解析 Chat 流事件失败: %w", err))
			return
		}
		state.id = firstNonEmpty(chunk.ID, state.id)
		state.model = firstNonEmpty(chunk.Model, state.model)
		if chunk.Usage != nil {
			state.usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				state.finish = *choice.FinishReason
			}
			text := rawText(choice.Delta.Content)
			if text != "" {
				state.text.WriteString(text)
				partial := &adkmodel.LLMResponse{
					Partial:        true,
					Content:        genai.NewContentFromText(text, genai.RoleModel),
					CustomMetadata: openAIMetadata(state.id, state.model, WireFormatChat),
				}
				if !yield(partial, nil) {
					return
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := state.calls[delta.Index]
				if call == nil {
					call = &streamToolCall{}
					state.calls[delta.Index] = call
				}
				call.id = firstNonEmpty(delta.ID, call.id)
				call.name = firstNonEmpty(delta.Function.Name, call.name)
				call.arguments.WriteString(delta.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		yield(nil, fmt.Errorf("读取 Chat 流失败: %w", err))
		return
	}
	final, err := state.finalResponse()
	if err != nil {
		yield(nil, err)
		return
	}
	yield(final, nil)
}

type chatStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   json.RawMessage     `json:"content"`
			ToolCalls []chatToolCallDelta `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage,omitempty"`
}

type chatToolCallDelta struct {
	Index    int                  `json:"index"`
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"`
	Function ChatToolCallFunction `json:"function"`
}

type streamToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

type chatStreamState struct {
	id     string
	model  string
	text   strings.Builder
	calls  map[int]*streamToolCall
	usage  *chatUsage
	finish string
}

func (s *chatStreamState) finalResponse() (*adkmodel.LLMResponse, error) {
	indexes := make([]int, 0, len(s.calls))
	for index := range s.calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	calls := make([]ChatToolCall, 0, len(indexes))
	for _, index := range indexes {
		call := s.calls[index]
		arguments := call.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		var check map[string]any
		if err := json.Unmarshal([]byte(arguments), &check); err != nil {
			return nil, fmt.Errorf("解析 Chat 工具 %q 参数失败: %w", call.name, err)
		}
		calls = append(calls, ChatToolCall{ID: call.id, Type: "function", Function: ChatToolCallFunction{Name: call.name, Arguments: arguments}})
	}
	content := json.RawMessage(nil)
	if s.text.Len() > 0 {
		encoded, _ := json.Marshal(s.text.String())
		content = encoded
	}
	result, err := chatLLMResponse(s.id, s.model, content, calls, s.finish, s.usage, false)
	if err != nil {
		return nil, err
	}
	result.TurnComplete = true
	return result, nil
}

func parseResponsesResponse(body []byte) (*adkmodel.LLMResponse, error) {
	var response responsesCompletion
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("解析 Responses 响应失败: %w", err)
	}
	return response.toLLMResponse(false)
}

type responsesCompletion struct {
	ID               string                `json:"id"`
	Model            string                `json:"model"`
	Status           string                `json:"status"`
	Output           []responsesOutputItem `json:"output"`
	OutputText       string                `json:"output_text,omitempty"`
	Usage            *responsesUsage       `json:"usage,omitempty"`
	IncompleteDetail *responsesIncomplete  `json:"incomplete_details,omitempty"`
	Error            *responsesError       `json:"error,omitempty"`
}

type responsesOutputItem struct {
	Type      string                   `json:"type"`
	Role      string                   `json:"role,omitempty"`
	ID        string                   `json:"id,omitempty"`
	CallID    string                   `json:"call_id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Arguments string                   `json:"arguments,omitempty"`
	Content   []responsesOutputContent `json:"content,omitempty"`
}

type responsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesUsage struct {
	InputTokens        int32                     `json:"input_tokens"`
	OutputTokens       int32                     `json:"output_tokens"`
	TotalTokens        int32                     `json:"total_tokens"`
	ReasoningTokens    int32                     `json:"reasoning_tokens,omitempty"`
	OutputTokenDetails *openAIOutputTokenDetails `json:"output_tokens_details,omitempty"`
}

func (u *responsesUsage) reasoningTokens() int32 {
	if u == nil {
		return 0
	}
	if u.ReasoningTokens > 0 {
		return u.ReasoningTokens
	}
	if u.OutputTokenDetails != nil {
		return u.OutputTokenDetails.ReasoningTokens
	}
	return 0
}

type openAIOutputTokenDetails struct {
	ReasoningTokens int32 `json:"reasoning_tokens,omitempty"`
}

type responsesIncomplete struct {
	Reason string `json:"reason"`
}

type responsesError struct {
	Message string `json:"message"`
}

func (r *responsesCompletion) toLLMResponse(partial bool) (*adkmodel.LLMResponse, error) {
	parts := make([]*genai.Part, 0)
	text := r.OutputText
	if text == "" {
		for _, item := range r.Output {
			switch item.Type {
			case "message", "output_text":
				for _, content := range item.Content {
					if content.Text != "" {
						text += content.Text
					}
				}
			}
		}
	}
	for _, item := range r.Output {
		if item.Type != "function_call" {
			continue
		}
		args := map[string]any{}
		arguments := item.Arguments
		if arguments == "" {
			arguments = "{}"
		}
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return nil, fmt.Errorf("解析 Responses 工具 %q 参数失败: %w", item.Name, err)
		}
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID: firstNonEmpty(item.CallID, item.ID), Name: item.Name, Args: args,
		}})
	}
	if text != "" {
		parts = append([]*genai.Part{genai.NewPartFromText(text)}, parts...)
	}
	result := &adkmodel.LLMResponse{
		Partial:        partial,
		Content:        contentFromParts(parts),
		FinishReason:   responsesFinishReason(r.Status, r.IncompleteDetail),
		CustomMetadata: openAIMetadata(r.ID, r.Model, WireFormatResponses),
	}
	if r.Error != nil {
		result.ErrorCode = "upstream_error"
		result.ErrorMessage = r.Error.Message
	}
	if r.Usage != nil {
		result.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     r.Usage.InputTokens,
			CandidatesTokenCount: r.Usage.OutputTokens,
			TotalTokenCount:      r.Usage.TotalTokens,
			ThoughtsTokenCount:   r.Usage.reasoningTokens(),
		}
	}
	if !partial {
		result.TurnComplete = true
	}
	return result, nil
}

func readResponsesStream(body io.Reader, yield func(*adkmodel.LLMResponse, error) bool) {
	state := &responsesStreamState{calls: make(map[string]*streamResponseCall)}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" || data == "" {
			continue
		}
		var event responsesStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			yield(nil, fmt.Errorf("解析 Responses 流事件失败: %w", err))
			return
		}
		if err := state.apply(event, yield); err != nil {
			if errors.Is(err, errStreamStopped) {
				// 下游主动停止消费时直接结束，不把停止动作误报成上游失败。
				return
			}
			yield(nil, err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		yield(nil, fmt.Errorf("读取 Responses 流失败: %w", err))
		return
	}
	result, err := state.finalResponse()
	if err != nil {
		yield(nil, err)
		return
	}
	yield(result, nil)
}

type responsesStreamEvent struct {
	Type      string          `json:"type"`
	Delta     string          `json:"delta,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
	Item      json.RawMessage `json:"item,omitempty"`
	ItemID    string          `json:"item_id,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
}

type streamResponseCall struct {
	id        string
	callID    string
	name      string
	arguments strings.Builder
}

type responsesStreamState struct {
	id         string
	model      string
	text       strings.Builder
	calls      map[string]*streamResponseCall
	full       *responsesCompletion
	finish     string
	incomplete *responsesIncomplete
}

func (s *responsesStreamState) apply(event responsesStreamEvent, yield func(*adkmodel.LLMResponse, error) bool) error {
	switch event.Type {
	case "response.created", "response.in_progress":
		var response responsesCompletion
		if len(event.Response) > 0 && json.Unmarshal(event.Response, &response) == nil {
			s.id = firstNonEmpty(response.ID, s.id)
			s.model = firstNonEmpty(response.Model, s.model)
		}
	case "response.output_text.delta":
		if event.Delta != "" {
			s.text.WriteString(event.Delta)
			returnYield := yield(&adkmodel.LLMResponse{
				Partial:        true,
				Content:        genai.NewContentFromText(event.Delta, genai.RoleModel),
				CustomMetadata: openAIMetadata(s.id, s.model, WireFormatResponses),
			}, nil)
			if !returnYield {
				return errStreamStopped
			}
		}
	case "response.output_item.added":
		var item responsesOutputItem
		if err := json.Unmarshal(event.Item, &item); err == nil && item.Type == "function_call" {
			key := firstNonEmpty(item.ID, item.CallID, event.ItemID)
			if key != "" {
				s.calls[key] = &streamResponseCall{id: item.ID, callID: item.CallID, name: item.Name}
			}
		}
	case "response.function_call_arguments.delta":
		call := s.calls[event.ItemID]
		if call == nil {
			call = &streamResponseCall{}
			s.calls[event.ItemID] = call
		}
		call.arguments.WriteString(event.Delta)
	case "response.function_call_arguments.done":
		call := s.calls[event.ItemID]
		if call == nil {
			call = &streamResponseCall{}
			s.calls[event.ItemID] = call
		}
		call.arguments.Reset()
		call.arguments.WriteString(event.Arguments)
	case "response.completed":
		var response responsesCompletion
		if len(event.Response) > 0 {
			if err := json.Unmarshal(event.Response, &response); err != nil {
				return fmt.Errorf("解析 Responses 完成事件失败: %w", err)
			}
			s.full = &response
			s.id = firstNonEmpty(response.ID, s.id)
			s.model = firstNonEmpty(response.Model, s.model)
		}
	case "response.incomplete":
		s.finish = "incomplete"
		var response responsesCompletion
		if len(event.Response) > 0 && json.Unmarshal(event.Response, &response) == nil {
			s.id = firstNonEmpty(response.ID, s.id)
			s.model = firstNonEmpty(response.Model, s.model)
			s.incomplete = response.IncompleteDetail
		}
	case "response.failed":
		var response responsesCompletion
		if len(event.Response) > 0 {
			_ = json.Unmarshal(event.Response, &response)
		}
		if response.Error != nil {
			return fmt.Errorf("Responses 上游失败: %s", response.Error.Message)
		}
		return errors.New("Responses 上游失败")
	}
	return nil
}

func (s *responsesStreamState) finalResponse() (*adkmodel.LLMResponse, error) {
	if s.full != nil {
		return s.full.toLLMResponse(false)
	}
	response := &responsesCompletion{ID: s.id, Model: s.model, Status: "completed", OutputText: s.text.String()}
	for _, call := range s.calls {
		arguments := call.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		var check map[string]any
		if err := json.Unmarshal([]byte(arguments), &check); err != nil {
			return nil, fmt.Errorf("解析 Responses 工具 %q 参数失败: %w", call.name, err)
		}
		response.Output = append(response.Output, responsesOutputItem{Type: "function_call", ID: call.id, CallID: call.callID, Name: call.name, Arguments: arguments})
	}
	if s.finish == "incomplete" {
		response.Status = "incomplete"
		response.IncompleteDetail = s.incomplete
	}
	return response.toLLMResponse(false)
}

var errStreamStopped = errors.New("流读取已停止")

func contentFromParts(parts []*genai.Part) *genai.Content {
	if len(parts) == 0 {
		return nil
	}
	return &genai.Content{Role: genai.RoleModel, Parts: parts}
}

func contentText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var result strings.Builder
	for _, part := range content.Parts {
		if part != nil && part.Text != "" {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var result strings.Builder
		for _, part := range parts {
			if part.Type == "text" || part.Type == "output_text" || part.Type == "input_text" || part.Type == "" {
				result.WriteString(part.Text)
			}
		}
		return result.String()
	}
	return ""
}

func normalizeRole(role string) string {
	if strings.EqualFold(role, "model") || strings.EqualFold(role, "assistant") {
		return "assistant"
	}
	if strings.EqualFold(role, "system") {
		return "system"
	}
	return "user"
}

func jsonString(value any) (string, error) {
	if value == nil {
		return "{}", nil
	}
	data, err := json.Marshal(value)
	return string(data), err
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func finishReason(reason string) genai.FinishReason {
	switch strings.ToLower(reason) {
	case "stop", "tool_calls", "completed", "":
		return genai.FinishReasonStop
	case "length", "max_tokens", "max_output_tokens":
		return genai.FinishReasonMaxTokens
	case "content_filter", "safety":
		return genai.FinishReasonSafety
	default:
		return genai.FinishReasonOther
	}
}

func responsesFinishReason(status string, incomplete *responsesIncomplete) genai.FinishReason {
	if incomplete != nil {
		return finishReason(incomplete.Reason)
	}
	if status == "incomplete" {
		return genai.FinishReasonMaxTokens
	}
	return genai.FinishReasonStop
}

func openAIMetadata(id, modelName string, format WireFormat) map[string]any {
	return map[string]any{
		"openai_response_id": id,
		"openai_model":       modelName,
		"openai_wire_format": string(format),
	}
}

func setAuthHeaders(request *http.Request, apiKey string) {
	if strings.TrimSpace(apiKey) != "" {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}
	request.Header.Set("User-Agent", "abot-provider-openai/1")
}

func parseBaseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("OpenAI 兼容供应商 API 地址不能为空")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("OpenAI API 地址必须是带 http/https 和主机的 URL")
	}
	return u, nil
}

func completionEndpoint(base string, format WireFormat) (string, error) {
	u, err := parseBaseURL(base)
	if err != nil {
		return "", err
	}
	pathName := strings.TrimRight(u.Path, "/")
	pathName = strings.TrimSuffix(pathName, "/chat/completions")
	pathName = strings.TrimSuffix(pathName, "/responses")
	if pathName == "" && strings.Contains(strings.ToLower(u.Host), "api.openai.com") {
		pathName = "/v1"
	}
	suffix := "/chat/completions"
	if format == WireFormatResponses {
		suffix = "/responses"
	}
	u.Path = strings.TrimRight(pathName, "/") + suffix
	u.RawQuery = ""
	return u.String(), nil
}

// inputTokensEndpoint resolves the Responses input-token count route from the
// same base URL forms accepted by completionEndpoint. A provider may expose a
// custom /v1 prefix or pass a URL that already ends in /responses; neither
// case should result in a duplicated suffix.
func inputTokensEndpoint(base string) (string, error) {
	u, err := parseBaseURL(base)
	if err != nil {
		return "", err
	}
	pathName := strings.TrimRight(u.Path, "/")
	pathName = strings.TrimSuffix(pathName, "/responses/input_tokens")
	pathName = strings.TrimSuffix(pathName, "/responses")
	pathName = strings.TrimSuffix(pathName, "/chat/completions")
	if pathName == "" && strings.Contains(strings.ToLower(u.Host), "api.openai.com") {
		pathName = "/v1"
	}
	u.Path = strings.TrimRight(pathName, "/") + "/responses/input_tokens"
	u.RawQuery = ""
	return u.String(), nil
}

func modelsEndpoint(base string) (string, error) {
	u, err := parseBaseURL(base)
	if err != nil {
		return "", err
	}
	pathName := strings.TrimRight(u.Path, "/")
	pathName = strings.TrimSuffix(pathName, "/chat/completions")
	pathName = strings.TrimSuffix(pathName, "/responses")
	if pathName == "" && strings.Contains(strings.ToLower(u.Host), "api.openai.com") {
		pathName = "/v1"
	}
	u.Path = strings.TrimRight(pathName, "/") + "/models"
	return u.String(), nil
}

func readBody(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}
	return data, nil
}

func upstreamError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	if len(message) > 1000 {
		message = message[:1000]
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return fmt.Errorf("上游 API 返回 HTTP %d: %s", status, message)
}

func safeOpenAIErrorBody(body []byte, secret string) string {
	message := strings.TrimSpace(string(body))
	if len(message) > 1000 {
		message = message[:1000]
	}
	if message == "" {
		message = "上游错误"
	}
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "REDACTED")
	}
	return message
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func errorSequence(err error) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, err)
	}
}
