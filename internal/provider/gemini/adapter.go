package gemini

import (
	"bytes"
	"context"
	"encoding/json"
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
	adkgemini "google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"
)

// Adapter 是 Gemini 原生协议适配器。模型调用交给 ADK 官方实现，管理探测在此完成。
type Adapter struct {
	client *http.Client
}

// NewAdapter 创建 Gemini 适配器。
func NewAdapter(client *http.Client) *Adapter {
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return &Adapter{client: client}
}

// BuildModel 通过 ADK 官方 Gemini 模型接入统一 LLM 接口。
func (a *Adapter) BuildModel(ctx context.Context, p provider.Provider, modelID string) (adkmodel.LLM, error) {
	config := &genai.ClientConfig{APIKey: p.APIKey, HTTPClient: a.client}
	if p.BaseURL != "" {
		config.HTTPOptions.BaseURL = strings.TrimRight(p.BaseURL, "/")
	}
	model, err := adkgemini.NewModel(ctx, modelID, config)
	if err != nil {
		return nil, err
	}
	// Gemini Developer API 不接受 Blob.displayName；文件名只在 OpenAI 转换层使用。
	return &modelWithoutDisplayNames{delegate: model}, nil
}

// ProbeCapabilities runs metadata-only probes through the same ADK Gemini
// model wrapper used for chat. A returned function call is observed but never
// dispatched to a host tool.
func (a *Adapter) ProbeCapabilities(ctx context.Context, p provider.Provider, m provider.Model, options provider.CapabilityProbeOptions) (provider.CapabilityProbeResult, error) {
	llm, err := a.BuildModel(ctx, p, m.ID)
	if err != nil {
		return provider.CapabilityProbeResult{}, err
	}
	profile := modelCapabilityProfile(p, m)
	profile.Route = "gemini"
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

// CountTokens uses Gemini's official countTokens endpoint for the exact
// materialized request. It is deliberately separate from GenerateContent:
// no model response is generated and no tool can be executed. If an upstream
// deployment does not expose this endpoint, callers can safely fall back to
// the bounded Runtime heuristic.
func (a *Adapter) CountTokens(ctx context.Context, p provider.Provider, m provider.Model, request *adkmodel.LLMRequest) (provider.TokenCount, error) {
	if p.TokenCountProtocol == provider.TokenCountProtocolAnthropic {
		return provider.CountAnthropicTokens(ctx, p, m, request, a.client)
	}
	if request == nil {
		return provider.TokenCount{}, fmt.Errorf("Gemini token count 请求不能为空")
	}
	modelID := strings.TrimSpace(m.ID)
	if modelID == "" {
		modelID = strings.TrimSpace(request.Model)
	}
	if modelID == "" {
		return provider.TokenCount{}, fmt.Errorf("Gemini token count 缺少模型 ID")
	}
	endpoint, err := countTokensEndpoint(p.BaseURL, modelID)
	if err != nil {
		return provider.TokenCount{}, err
	}
	contents := cloneContentsWithoutDisplayNames(request.Contents)
	if contents == nil {
		contents = []*genai.Content{}
	}
	bodyValue := struct {
		Contents          []*genai.Content `json:"contents"`
		SystemInstruction *genai.Content   `json:"systemInstruction,omitempty"`
		Tools             []*genai.Tool    `json:"tools,omitempty"`
	}{Contents: contents}
	if request.Config != nil {
		bodyValue.SystemInstruction = cloneContentWithoutDisplayNames(request.Config.SystemInstruction)
		bodyValue.Tools = request.Config.Tools
	}
	body, err := json.Marshal(bodyValue)
	if err != nil {
		return provider.TokenCount{}, fmt.Errorf("编码 Gemini token count 请求失败: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return provider.TokenCount{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "abot-provider-gemini/1")
	if strings.TrimSpace(p.APIKey) != "" {
		httpRequest.Header.Set("x-goog-api-key", strings.TrimSpace(p.APIKey))
	}
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return provider.TokenCount{}, fmt.Errorf("请求 Gemini token count 失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1*1024*1024))
	if err != nil {
		return provider.TokenCount{}, fmt.Errorf("读取 Gemini token count 失败: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.TokenCount{}, fmt.Errorf("Gemini token count 返回 HTTP %d: %s", response.StatusCode, safeErrorBody(responseBody, p.APIKey))
	}
	var envelope struct {
		TotalTokens      *int32 `json:"totalTokens"`
		TotalTokensSnake *int32 `json:"total_tokens"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return provider.TokenCount{}, fmt.Errorf("解析 Gemini token count 失败: %w", err)
	}
	value := envelope.TotalTokens
	if value == nil {
		value = envelope.TotalTokensSnake
	}
	if value == nil || *value < 0 {
		return provider.TokenCount{}, fmt.Errorf("Gemini token count 响应缺少有效 totalTokens")
	}
	return provider.TokenCount{Tokens: int(*value), Name: provider.GeminiTokenCounterName, Version: provider.GeminiTokenCounterVersion, Source: "provider", Quality: "provider", Exact: true}, nil
}

func modelCapabilityProfile(p provider.Provider, m provider.Model) provider.ModelCapabilityProfile {
	if m.Capabilities != nil {
		return provider.NormalizeCapabilityProfile(*m.Capabilities)
	}
	return provider.DefaultCapabilities(p, m)
}

// modelWithoutDisplayNames 在调用 Gemini 前复制并清除文件名元数据，避免官方 SDK 拒绝请求。
type modelWithoutDisplayNames struct {
	delegate adkmodel.LLM
}

func (m *modelWithoutDisplayNames) Name() string { return m.delegate.Name() }

func (m *modelWithoutDisplayNames) GenerateContent(ctx context.Context, request *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return m.delegate.GenerateContent(ctx, cloneRequestWithoutDisplayNames(request), stream)
}

func cloneRequestWithoutDisplayNames(request *adkmodel.LLMRequest) *adkmodel.LLMRequest {
	if request == nil {
		return nil
	}
	clone := *request
	clone.Contents = cloneContentsWithoutDisplayNames(request.Contents)
	if request.Config != nil && request.Config.SystemInstruction != nil {
		config := *request.Config
		config.SystemInstruction = cloneContentWithoutDisplayNames(request.Config.SystemInstruction)
		clone.Config = &config
	}
	return &clone
}

func cloneContentsWithoutDisplayNames(contents []*genai.Content) []*genai.Content {
	if contents == nil {
		return nil
	}
	result := make([]*genai.Content, len(contents))
	for index, content := range contents {
		result[index] = cloneContentWithoutDisplayNames(content)
	}
	return result
}

func cloneContentWithoutDisplayNames(content *genai.Content) *genai.Content {
	if content == nil {
		return nil
	}
	clone := *content
	if content.Parts == nil {
		return &clone
	}
	clone.Parts = make([]*genai.Part, len(content.Parts))
	for index, part := range content.Parts {
		if part == nil {
			continue
		}
		partClone := *part
		if part.InlineData != nil {
			blob := *part.InlineData
			blob.DisplayName = ""
			partClone.InlineData = &blob
		}
		if part.FileData != nil {
			file := *part.FileData
			file.DisplayName = ""
			partClone.FileData = &file
		}
		clone.Parts[index] = &partClone
	}
	return &clone
}

// TestConnection 以 GET /models 作为轻量健康检查。
func (a *Adapter) TestConnection(ctx context.Context, p provider.Provider) (provider.ProbeResult, error) {
	models, err := a.DiscoverModels(ctx, p)
	if err != nil {
		return provider.ProbeResult{}, err
	}
	return provider.ProbeResult{Message: fmt.Sprintf("连接正常，发现 %d 个模型", len(models)), Models: models}, nil
}

// DiscoverModels 读取 Gemini API 的模型目录，并去掉响应中常见的 models/ 前缀。
func (a *Adapter) DiscoverModels(ctx context.Context, p provider.Provider) ([]provider.Model, error) {
	endpoint, err := modelsEndpoint(p.BaseURL, p.APIKey)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		request.Header.Set("x-goog-api-key", p.APIKey)
	}
	request.Header.Set("User-Agent", "abot-provider-gemini/1")
	response, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 Gemini 模型目录失败: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("读取 Gemini 模型目录失败: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Gemini API 返回 HTTP %d: %s", response.StatusCode, trimBody(body))
	}
	var envelope struct {
		Models []struct {
			Name             string `json:"name"`
			DisplayName      string `json:"displayName"`
			InputTokenLimit  int    `json:"inputTokenLimit"`
			OutputTokenLimit int    `json:"outputTokenLimit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("解析 Gemini 模型目录失败: %w", err)
	}
	result := make([]provider.Model, 0, len(envelope.Models))
	for _, item := range envelope.Models {
		id := strings.TrimPrefix(strings.TrimSpace(item.Name), "models/")
		if id == "" {
			continue
		}
		result = append(result, provider.Model{
			ID:              id,
			DisplayName:     firstNonEmpty(item.DisplayName, id),
			Enabled:         true,
			Source:          "discovered",
			ContextWindow:   item.InputTokenLimit,
			MaxOutputTokens: item.OutputTokenLimit,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func modelsEndpoint(baseURL, apiKey string) (string, error) {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("Gemini API 地址必须是带协议和主机的 URL")
	}
	pathName := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(pathName, "/models") {
		if !strings.HasSuffix(pathName, "/v1beta") && !strings.HasSuffix(pathName, "/v1") {
			pathName += "/v1beta"
		}
		pathName += "/models"
	}
	u.Path = pathName
	query := u.Query()
	if apiKey != "" {
		query.Set("key", apiKey)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func countTokensEndpoint(baseURL, modelID string) (string, error) {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("Gemini API 地址必须是带协议和主机的 URL")
	}
	pathName := strings.TrimRight(u.Path, "/")
	// Accept the same base URL shapes as GenerateContent and model discovery:
	// root, /v1beta, /v1, or /v1beta/models. A caller must not be able to
	// smuggle an action suffix into the model path.
	if strings.HasSuffix(pathName, "/models") {
		pathName += "/" + url.PathEscape(modelID)
	} else if strings.HasSuffix(pathName, "/v1beta") || strings.HasSuffix(pathName, "/v1") {
		pathName += "/models/" + url.PathEscape(modelID)
	} else {
		pathName += "/v1beta/models/" + url.PathEscape(modelID)
	}
	u.Path = pathName + ":countTokens"
	query := u.Query()
	// Authentication is sent through x-goog-api-key below; never copy a
	// caller-provided key query parameter into a persisted or logged URL, but
	// preserve unrelated gateway parameters from the configured base URL.
	query.Del("key")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func safeErrorBody(body []byte, secret string) string {
	message := trimBody(body)
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	return message
}

func trimBody(body []byte) string {
	message := strings.TrimSpace(string(body))
	if len(message) > 1000 {
		return message[:1000]
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
