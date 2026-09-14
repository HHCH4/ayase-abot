package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
)

// Protocol 是供应商所使用的上游 API 协议。
type Protocol string

const (
	ProtocolOpenAICompatible Protocol = "openai-compatible"
	// ProtocolOpenAICompletions 兼容设计文档中的旧名称，保存时统一归一到 OpenAICompatible。
	ProtocolOpenAICompletions Protocol = "openai-completions"
	ProtocolGemini            Protocol = "gemini"
)

// TokenCountProtocol identifies an explicitly configured provider-side token
// counting route. It is independent from the generation protocol because a
// gateway may expose a counting API with a different wire shape. An empty
// value keeps the adapter's native/default counter; Anthropic is opt-in and
// never inferred from a compatible generation endpoint.
type TokenCountProtocol string

const (
	TokenCountProtocolAnthropic TokenCountProtocol = "anthropic"
)

// OpenAIFormat 指定 OpenAI 兼容服务的请求线路。
type OpenAIFormat string

const (
	OpenAIFormatAuto      OpenAIFormat = "auto"
	OpenAIFormatChat      OpenAIFormat = "chat"
	OpenAIFormatResponses OpenAIFormat = "responses"
)

// Status 表示最近一次连接检查的结果。
type Status string

const (
	StatusConfigured Status = "configured"
	StatusReady      Status = "ready"
	StatusError      Status = "error"
)

var (
	ErrNotFound                   = errors.New("供应商不存在")
	ErrNoDefault                  = errors.New("尚未设置默认模型")
	ErrNoModel                    = errors.New("没有可用模型")
	ErrInvalidRequest             = errors.New("供应商配置无效")
	ErrCapabilityProbeUnavailable = errors.New("当前协议未提供模型能力探测")
	ErrTokenCounterUnavailable    = errors.New("当前协议未提供 token count 能力")
)

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Model 是供应商模型目录中的一项。
type Model struct {
	ID              string                  `json:"id"`
	DisplayName     string                  `json:"display_name"`
	Enabled         bool                    `json:"enabled"`
	Source          string                  `json:"source"`
	ContextWindow   int                     `json:"context_window,omitempty"`
	MaxOutputTokens int                     `json:"max_output_tokens,omitempty"`
	Capabilities    *ModelCapabilityProfile `json:"capabilities,omitempty"`
	CreatedAt       time.Time               `json:"created_at,omitempty"`
	UpdatedAt       time.Time               `json:"updated_at,omitempty"`
}

// Provider 是内部完整配置，APIKey 只在进程内使用，禁止直接序列化给前端。
type Provider struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	BaseURL            string             `json:"base_url"`
	Protocol           Protocol           `json:"protocol"`
	OpenAIFormat       OpenAIFormat       `json:"openai_format"`
	TokenCountProtocol TokenCountProtocol `json:"token_count_protocol,omitempty"`
	APIKey             string             `json:"-"`
	Models             []Model            `json:"models"`
	Status             Status             `json:"status"`
	StatusMessage      string             `json:"status_message,omitempty"`
	LastCheckedAt      *time.Time         `json:"last_checked_at,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

// DefaultRef 是全局默认供应商和模型的引用。
type DefaultRef struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

// SaveRequest 是供应商保存用例。APIKey 为 nil 表示编辑时保留旧密钥。
type SaveRequest struct {
	Provider Provider
	APIKey   *string
}

// Repository 是供应商领域需要的持久化能力，具体 SQLite 实现在 storage/sqlite。
type Repository interface {
	List(context.Context) ([]Provider, error)
	Get(context.Context, string) (Provider, error)
	Save(context.Context, Provider) error
	Delete(context.Context, string) error
	GetDefault(context.Context) (DefaultRef, error)
	SetDefault(context.Context, DefaultRef) error
}

// ProbeResult 是连接检查或模型探测的结果。
type ProbeResult struct {
	Message string  `json:"message"`
	Models  []Model `json:"models,omitempty"`
}

// CapabilityProbeOptions bounds an explicit model capability probe. Probes
// never use a production Conversation or execute host tools.
type CapabilityProbeOptions struct {
	Timeout               time.Duration `json:"-"`
	IncludeStreaming      bool          `json:"include_streaming"`
	IncludeToolCalling    bool          `json:"include_tool_calling"`
	IncludeStructuredJSON bool          `json:"include_structured_json"`
	// IncludeTokenCount is opt-in because it sends an additional request to a
	// provider-side counting endpoint. A successful result is exact only for
	// the complete bounded probe request; failures must keep the heuristic
	// fallback rather than being interpreted as a generation failure.
	IncludeTokenCount bool `json:"include_token_count"`
	// These probes are opt-in because they send a small multimodal payload or
	// request reasoning metadata and may consume provider quota. They never
	// execute tools and only persist feature state/reason metadata.
	IncludeStructuredSchema bool `json:"include_structured_schema"`
	IncludeReasoning        bool `json:"include_reasoning"`
	IncludeImages           bool `json:"include_images"`
	IncludeInputFiles       bool `json:"include_input_files"`
	MaxOutputTokens         int  `json:"max_output_tokens"`
	// ExplicitOptionalSelection lets an API caller intentionally disable all
	// optional probes. Without it, the zero value keeps the convenient
	// provider-neutral default of running all bounded probes.
	ExplicitOptionalSelection bool `json:"-"`
}

// CapabilityObservation records one piece of probe evidence. Input prompts,
// tool arguments and provider responses are intentionally not stored.
type CapabilityObservation struct {
	Feature    string       `json:"feature"`
	State      SupportState `json:"state"`
	Source     string       `json:"source"`
	Confidence float64      `json:"confidence,omitempty"`
	Route      string       `json:"route,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
}

type CapabilityProbeResult struct {
	Profile      ModelCapabilityProfile  `json:"profile"`
	Observations []CapabilityObservation `json:"observations,omitempty"`
	Message      string                  `json:"message"`
	StartedAt    time.Time               `json:"started_at"`
	CompletedAt  time.Time               `json:"completed_at"`
}

// TokenCount is the provider-side count for one materialized model request.
// It is metadata only: the request body is never persisted by this layer.
// Exact is true only when the provider/count endpoint accepted the complete
// request shape; callers must fall back to the bounded heuristic otherwise.
type TokenCount struct {
	Tokens  int    `json:"tokens"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Source  string `json:"source"`
	Quality string `json:"quality"`
	Exact   bool   `json:"exact"`
}

// TokenCounter is an optional adapter capability. It deliberately lives next
// to Adapter rather than extending that interface so third-party adapters
// remain source-compatible. Implementations must not execute model tools.
type TokenCounter interface {
	CountTokens(context.Context, Provider, Model, *adkmodel.LLMRequest) (TokenCount, error)
}

// CapabilityProber is optional so existing third-party adapters remain
// source-compatible. Built-in adapters implement it with the bounded runner
// in capability_probe.go.
type CapabilityProber interface {
	ProbeCapabilities(context.Context, Provider, Model, CapabilityProbeOptions) (CapabilityProbeResult, error)
}

// Adapter 把某一种供应商协议转换为 ADK 模型，同时提供管理面探测能力。
type Adapter interface {
	BuildModel(context.Context, Provider, string) (adkmodel.LLM, error)
	TestConnection(context.Context, Provider) (ProbeResult, error)
	DiscoverModels(context.Context, Provider) ([]Model, error)
}

// ResolvedModel 是 Agent 内核解析后的运行时模型。
type ResolvedModel struct {
	Provider Provider
	Model    Model
	LLM      adkmodel.LLM
}

// Validate 检查供应商配置，避免把明显错误留到聊天时才暴露。
func (p Provider) Validate() error {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	if !providerIDPattern.MatchString(p.ID) {
		return fmt.Errorf("%w: Provider ID 必须匹配 [a-z][a-z0-9_-]{0,63}", ErrInvalidRequest)
	}
	if p.Name == "" {
		return fmt.Errorf("%w: 显示名称不能为空", ErrInvalidRequest)
	}
	switch p.Protocol {
	case ProtocolOpenAICompatible, ProtocolOpenAICompletions:
		if p.BaseURL == "" {
			return fmt.Errorf("%w: OpenAI 兼容协议需要 API 地址", ErrInvalidRequest)
		}
		if err := validateHTTPURL(p.BaseURL); err != nil {
			return fmt.Errorf("%w: API 地址无效: %v", ErrInvalidRequest, err)
		}
		if p.OpenAIFormat == "" {
			p.OpenAIFormat = OpenAIFormatAuto
		}
		if p.OpenAIFormat != OpenAIFormatAuto && p.OpenAIFormat != OpenAIFormatChat && p.OpenAIFormat != OpenAIFormatResponses {
			return fmt.Errorf("%w: OpenAI 格式必须是 auto、chat 或 responses", ErrInvalidRequest)
		}
	case ProtocolGemini:
		if p.BaseURL != "" {
			if err := validateHTTPURL(p.BaseURL); err != nil {
				return fmt.Errorf("%w: Gemini API 地址无效: %v", ErrInvalidRequest, err)
			}
		}
	default:
		return fmt.Errorf("%w: 不支持的协议 %q", ErrInvalidRequest, p.Protocol)
	}
	switch TokenCountProtocol(strings.TrimSpace(string(p.TokenCountProtocol))) {
	case "", TokenCountProtocolAnthropic:
		// Empty keeps the generation adapter's native/default counter. The
		// Anthropic route is deliberately explicit because it may target a
		// gateway whose generation protocol is OpenAI-compatible or Gemini.
	default:
		return fmt.Errorf("%w: 不支持的 token count 协议 %q", ErrInvalidRequest, p.TokenCountProtocol)
	}
	seen := make(map[string]struct{}, len(p.Models))
	for i := range p.Models {
		p.Models[i].ID = strings.TrimSpace(p.Models[i].ID)
		if p.Models[i].ID == "" {
			return fmt.Errorf("%w: 第 %d 个模型 ID 不能为空", ErrInvalidRequest, i+1)
		}
		if _, ok := seen[p.Models[i].ID]; ok {
			return fmt.Errorf("%w: 模型 %q 重复", ErrInvalidRequest, p.Models[i].ID)
		}
		seen[p.Models[i].ID] = struct{}{}
		if p.Models[i].ContextWindow < 0 || p.Models[i].MaxOutputTokens < 0 {
			return fmt.Errorf("%w: 模型 %q 的 token 配置不能为负数", ErrInvalidRequest, p.Models[i].ID)
		}
	}
	return nil
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("必须是带 http/https 协议和主机的 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("只支持 http 或 https")
	}
	return nil
}

func cloneProvider(p Provider) Provider {
	p.Models = append([]Model(nil), p.Models...)
	for index := range p.Models {
		if p.Models[index].Capabilities != nil {
			profile := *p.Models[index].Capabilities
			profile.ToolChoiceModes = append([]string(nil), profile.ToolChoiceModes...)
			profile.UsageDetails = append([]string(nil), profile.UsageDetails...)
			p.Models[index].Capabilities = &profile
		}
	}
	return p
}

func cloneProviders(items []Provider) []Provider {
	result := make([]Provider, len(items))
	for i, item := range items {
		result[i] = cloneProvider(item)
	}
	return result
}
