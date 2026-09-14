package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	// Anthropic's count endpoint is a metadata-only request. Keep the body
	// bounded even when a caller constructs an LLMRequest outside Kernel.
	maxAnthropicTokenCountBody     = 2 * 1024 * 1024
	maxAnthropicTokenCountMessages = 10000
	maxAnthropicTokenCountTools    = 512
	maxAnthropicTokenCountParts    = 10000
)

// CountAnthropicTokens calls POST /v1/messages/count_tokens. It is shared by
// generation adapters that explicitly opt into an Anthropic-shaped counting
// route. The generation request still uses its configured adapter/protocol;
// this function only projects the materialized ADK request for counting.
func CountAnthropicTokens(ctx context.Context, p Provider, m Model, request *adkmodel.LLMRequest, client *http.Client) (TokenCount, error) {
	if request == nil {
		return TokenCount{}, errors.New("Anthropic token count 请求不能为空")
	}
	modelID := strings.TrimSpace(m.ID)
	if modelID == "" {
		modelID = strings.TrimSpace(request.Model)
	}
	if modelID == "" {
		return TokenCount{}, errors.New("Anthropic token count 缺少模型 ID")
	}
	payload, err := anthropicTokenCountRequestFromLLM(modelID, request)
	if err != nil {
		return TokenCount{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return TokenCount{}, fmt.Errorf("编码 Anthropic token count 请求失败: %w", err)
	}
	if len(body) > maxAnthropicTokenCountBody {
		return TokenCount{}, fmt.Errorf("Anthropic token count 请求超过 %d 字节上限", maxAnthropicTokenCountBody)
	}
	endpoint, err := anthropicCountTokensEndpoint(p.BaseURL)
	if err != nil {
		return TokenCount{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		client = http.DefaultClient
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return TokenCount{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("anthropic-version", "2023-06-01")
	// The endpoint is documented as a beta API. Sending the beta marker is
	// harmless for compatible gateways and makes the intended capability
	// explicit to Anthropic's API.
	httpRequest.Header.Set("anthropic-beta", "token-counting-2024-11-01")
	if key := strings.TrimSpace(p.APIKey); key != "" {
		httpRequest.Header.Set("x-api-key", key)
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return TokenCount{}, fmt.Errorf("请求 Anthropic token count 失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1*1024*1024))
	if err != nil {
		return TokenCount{}, fmt.Errorf("读取 Anthropic token count 失败: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return TokenCount{}, fmt.Errorf("Anthropic token count 返回 HTTP %d: %s", response.StatusCode, safeAnthropicTokenCountError(responseBody, p.APIKey))
	}
	var envelope struct {
		InputTokens *int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return TokenCount{}, fmt.Errorf("解析 Anthropic token count 失败: %w", err)
	}
	if envelope.InputTokens == nil || *envelope.InputTokens < 0 || uint64(*envelope.InputTokens) > uint64(^uint(0)>>1) {
		return TokenCount{}, errors.New("Anthropic token count 响应缺少有效 input_tokens")
	}
	return TokenCount{
		Tokens: int(*envelope.InputTokens), Name: AnthropicTokenCounterName,
		Version: AnthropicTokenCounterVersion, Source: "provider", Quality: "provider", Exact: true,
	}, nil
}

type anthropicTokenCountRequest struct {
	Model    string                    `json:"model"`
	Messages []anthropicMessage        `json:"messages"`
	System   []anthropicContentBlock   `json:"system,omitempty"`
	Tools    []anthropicToolDefinition `json:"tools,omitempty"`
	Thinking map[string]any            `json:"thinking,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	Source    *anthropicMediaSource `json:"source,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     map[string]any        `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   any                   `json:"content,omitempty"`
}

type anthropicMediaSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

func anthropicTokenCountRequestFromLLM(modelID string, request *adkmodel.LLMRequest) (anthropicTokenCountRequest, error) {
	if len(request.Contents) > maxAnthropicTokenCountMessages {
		return anthropicTokenCountRequest{}, fmt.Errorf("Anthropic token count messages 超过 %d 条上限", maxAnthropicTokenCountMessages)
	}
	result := anthropicTokenCountRequest{Model: modelID, Messages: make([]anthropicMessage, 0, len(request.Contents))}
	if request.Config != nil {
		if request.Config.SystemInstruction != nil {
			blocks, err := anthropicContentBlocks(request.Config.SystemInstruction.Parts, &partConversionState{})
			if err != nil {
				return anthropicTokenCountRequest{}, fmt.Errorf("转换 Anthropic system instruction 失败: %w", err)
			}
			result.System = blocks
		}
		if len(request.Config.Tools) > maxAnthropicTokenCountTools {
			return anthropicTokenCountRequest{}, fmt.Errorf("Anthropic token count tools 超过 %d 项上限", maxAnthropicTokenCountTools)
		}
		for _, tool := range request.Config.Tools {
			if tool == nil {
				continue
			}
			for _, declaration := range tool.FunctionDeclarations {
				if declaration == nil || strings.TrimSpace(declaration.Name) == "" {
					return anthropicTokenCountRequest{}, errors.New("Anthropic token count 工具缺少名称")
				}
				schema, err := anthropicToolSchema(declaration)
				if err != nil {
					return anthropicTokenCountRequest{}, err
				}
				result.Tools = append(result.Tools, anthropicToolDefinition{Name: declaration.Name, Description: declaration.Description, InputSchema: schema})
				if len(result.Tools) > maxAnthropicTokenCountTools {
					return anthropicTokenCountRequest{}, fmt.Errorf("Anthropic token count tools 超过 %d 项上限", maxAnthropicTokenCountTools)
				}
			}
		}
		if thinking := anthropicThinking(request.Config.ThinkingConfig); thinking != nil {
			result.Thinking = thinking
		}
	}
	state := partConversionState{}
	for index, content := range request.Contents {
		if content == nil {
			return anthropicTokenCountRequest{}, fmt.Errorf("Anthropic token count 第 %d 条 message 为空", index+1)
		}
		role := anthropicRole(content.Role)
		blocks, err := anthropicContentBlocks(content.Parts, &state)
		if err != nil {
			return anthropicTokenCountRequest{}, fmt.Errorf("转换 Anthropic 第 %d 条 message 失败: %w", index+1, err)
		}
		if len(blocks) == 0 {
			blocks = []anthropicContentBlock{{Type: "text", Text: ""}}
		}
		result.Messages = append(result.Messages, anthropicMessage{Role: role, Content: blocks})
	}
	return result, nil
}

type partConversionState struct{ functionCall int }

func anthropicRole(role string) string {
	if strings.EqualFold(strings.TrimSpace(role), "model") || strings.EqualFold(strings.TrimSpace(role), "assistant") {
		return "assistant"
	}
	return "user"
}

func anthropicContentBlocks(parts []*genai.Part, state *partConversionState) ([]anthropicContentBlock, error) {
	if len(parts) > maxAnthropicTokenCountParts {
		return nil, fmt.Errorf("content parts 超过 %d 项上限", maxAnthropicTokenCountParts)
	}
	if state == nil {
		state = &partConversionState{}
	}
	result := make([]anthropicContentBlock, 0, len(parts))
	for _, part := range parts {
		if part == nil {
			return nil, errors.New("content part 为空")
		}
		switch {
		case part.Text != "":
			result = append(result, anthropicContentBlock{Type: "text", Text: part.Text})
		case part.InlineData != nil:
			block, err := anthropicInlineMedia(part.InlineData)
			if err != nil {
				return nil, err
			}
			result = append(result, block)
		case part.FileData != nil:
			block, err := anthropicRemoteMedia(part.FileData)
			if err != nil {
				return nil, err
			}
			result = append(result, block)
		case part.FunctionCall != nil:
			id := strings.TrimSpace(part.FunctionCall.ID)
			if id == "" {
				return nil, errors.New("function call 缺少稳定 ID，不能精确计数")
			}
			if strings.TrimSpace(part.FunctionCall.Name) == "" {
				return nil, errors.New("function call 缺少名称")
			}
			input := part.FunctionCall.Args
			if input == nil {
				input = map[string]any{}
			}
			result = append(result, anthropicContentBlock{Type: "tool_use", ID: id, Name: part.FunctionCall.Name, Input: input})
			state.functionCall++
		case part.FunctionResponse != nil:
			id := strings.TrimSpace(part.FunctionResponse.ID)
			if id == "" {
				return nil, errors.New("function response 缺少对应 tool_use_id，不能精确计数")
			}
			content, err := anthropicFunctionResponseContent(part.FunctionResponse)
			if err != nil {
				return nil, err
			}
			result = append(result, anthropicContentBlock{Type: "tool_result", ToolUseID: id, Content: content})
		default:
			// Unknown ADK parts are not silently represented as text. Returning an
			// error keeps the caller on the bounded heuristic and avoids an exact
			// claim for a shape the provider did not count.
			return nil, errors.New("存在 Anthropic token count 不支持的 content part")
		}
	}
	return result, nil
}

func anthropicInlineMedia(blob *genai.Blob) (anthropicContentBlock, error) {
	if blob == nil || strings.TrimSpace(blob.MIMEType) == "" {
		return anthropicContentBlock{}, errors.New("inline media 缺少 MIME 类型")
	}
	mimeType := strings.ToLower(strings.TrimSpace(blob.MIMEType))
	if mimeType == "application/pdf" {
		return anthropicContentBlock{Type: "document", Source: &anthropicMediaSource{Type: "base64", MediaType: mimeType, Data: base64.StdEncoding.EncodeToString(blob.Data)}}, nil
	}
	if strings.HasPrefix(mimeType, "image/") {
		return anthropicContentBlock{Type: "image", Source: &anthropicMediaSource{Type: "base64", MediaType: mimeType, Data: base64.StdEncoding.EncodeToString(blob.Data)}}, nil
	}
	return anthropicContentBlock{}, fmt.Errorf("Anthropic token count 不支持 MIME 类型 %q", mimeType)
}

func anthropicRemoteMedia(data *genai.FileData) (anthropicContentBlock, error) {
	if data == nil || strings.TrimSpace(data.FileURI) == "" || strings.TrimSpace(data.MIMEType) == "" {
		return anthropicContentBlock{}, errors.New("remote media 缺少 URI 或 MIME 类型")
	}
	mimeType := strings.ToLower(strings.TrimSpace(data.MIMEType))
	if mimeType == "application/pdf" || strings.HasPrefix(mimeType, "image/") {
		return anthropicContentBlock{}, errors.New("Anthropic token count 不支持 URL/file media，请使用 base64 inline data")
	}
	return anthropicContentBlock{}, fmt.Errorf("Anthropic token count 不支持 MIME 类型 %q", mimeType)
}

func anthropicFunctionResponseContent(response *genai.FunctionResponse) (any, error) {
	if response == nil {
		return nil, errors.New("function response 为空")
	}
	if len(response.Parts) > 0 {
		blocks, err := anthropicFunctionResponseParts(response.Parts)
		if err != nil {
			return nil, err
		}
		return blocks, nil
	}
	if response.Response == nil {
		return "", nil
	}
	encoded, err := json.Marshal(response.Response)
	if err != nil {
		return nil, fmt.Errorf("编码 function response 失败: %w", err)
	}
	return string(encoded), nil
}

func anthropicFunctionResponseParts(parts []*genai.FunctionResponsePart) ([]anthropicContentBlock, error) {
	result := make([]anthropicContentBlock, 0, len(parts))
	for _, part := range parts {
		if part == nil {
			return nil, errors.New("function response part 为空")
		}
		if part.InlineData != nil {
			block, err := anthropicInlineMedia(&genai.Blob{Data: part.InlineData.Data, MIMEType: part.InlineData.MIMEType})
			if err != nil {
				return nil, err
			}
			result = append(result, block)
			continue
		}
		if part.FileData != nil {
			block, err := anthropicRemoteMedia(&genai.FileData{FileURI: part.FileData.FileURI, MIMEType: part.FileData.MIMEType})
			if err != nil {
				return nil, err
			}
			result = append(result, block)
			continue
		}
		return nil, errors.New("function response part 不支持")
	}
	return result, nil
}

func anthropicToolSchema(declaration *genai.FunctionDeclaration) (any, error) {
	if declaration.ParametersJsonSchema != nil {
		return declaration.ParametersJsonSchema, nil
	}
	if declaration.Parameters == nil {
		return map[string]any{"type": "object"}, nil
	}
	encoded, err := json.Marshal(declaration.Parameters)
	if err != nil {
		return nil, fmt.Errorf("编码 Anthropic 工具 schema 失败: %w", err)
	}
	var schema any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return nil, fmt.Errorf("解析 Anthropic 工具 schema 失败: %w", err)
	}
	return schema, nil
}

func anthropicThinking(config *genai.ThinkingConfig) map[string]any {
	if config == nil {
		return nil
	}
	if config.ThinkingBudget != nil && *config.ThinkingBudget > 0 {
		return map[string]any{"type": "enabled", "budget_tokens": *config.ThinkingBudget}
	}
	if config.IncludeThoughts {
		return map[string]any{"type": "adaptive"}
	}
	return nil
}

func anthropicCountTokensEndpoint(baseURL string) (string, error) {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = "https://api.anthropic.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("Anthropic token count API 地址无效")
	}
	pathName := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(pathName, "/messages/count_tokens"):
		// Keep an explicitly complete endpoint intact.
	case strings.HasSuffix(pathName, "/messages"):
		pathName += "/count_tokens"
	case strings.HasSuffix(pathName, "/v1"):
		pathName += "/messages/count_tokens"
	default:
		pathName += "/v1/messages/count_tokens"
	}
	u.Path = pathName
	return u.String(), nil
}

func safeAnthropicTokenCountError(body []byte, secret string) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "上游未返回错误正文"
	}
	if len(text) > 512 {
		text = text[:512]
	}
	if secret = strings.TrimSpace(secret); secret != "" {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	for _, key := range []string{"api_key", "apikey", "access_token", "authorization", "x-api-key"} {
		text = strings.ReplaceAll(text, key, "[REDACTED_FIELD]")
	}
	return text
}
