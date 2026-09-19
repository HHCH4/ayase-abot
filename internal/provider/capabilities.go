package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

type SupportState string

const (
	SupportSupported   SupportState = "supported"
	SupportUnsupported SupportState = "unsupported"
	SupportUnknown     SupportState = "unknown"
	SupportDegraded    SupportState = "degraded"
)

type Support struct {
	State      SupportState `json:"state"`
	Source     string       `json:"source,omitempty"`
	Confidence float64      `json:"confidence,omitempty"`
	ObservedAt time.Time    `json:"observed_at,omitempty"`
	Reason     string       `json:"reason,omitempty"`
}

// CapabilityOverrides contains only the user-controlled support switches.
// Numeric catalog limits are intentionally not part of this type: changing a
// context/output limit is a model-directory edit and must not be smuggled in
// as a capability assertion. An override may make a feature more
// conservative, but it can never turn an unknown/unsupported feature into
// supported.
type CapabilityOverrides struct {
	ToolCalling            *SupportState `json:"tool_calling,omitempty"`
	ParallelToolCalls      *SupportState `json:"parallel_tool_calls,omitempty"`
	StructuredOutput       *SupportState `json:"structured_output,omitempty"`
	StructuredOutputSchema *SupportState `json:"structured_output_schema,omitempty"`
	Streaming              *SupportState `json:"streaming,omitempty"`
	Images                 *SupportState `json:"images,omitempty"`
	InputFiles             *SupportState `json:"input_files,omitempty"`
	Audio                  *SupportState `json:"audio,omitempty"`
	ReasoningEffort        *SupportState `json:"reasoning_effort,omitempty"`
	ReasoningSummary       *SupportState `json:"reasoning_summary,omitempty"`
	PromptCaching          *SupportState `json:"prompt_caching,omitempty"`
	NativeCompaction       *SupportState `json:"native_compaction,omitempty"`
}

var ErrInvalidCapabilityOverride = errors.New("模型能力 override 无效")

type CapabilityValue[T any] struct {
	Value      T         `json:"value"`
	Known      bool      `json:"known"`
	Source     string    `json:"source,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// TokenCalibration is bounded, metadata-only feedback for the local context
// estimator. It is deliberately kept separate from provider billing usage:
// actual usage is still recorded on the model-call/manifest, while this
// aggregate only controls a conservative safety multiplier after enough
// samples have accumulated.
type TokenCalibration struct {
	Samples             int       `json:"samples"`
	EstimatedTokens     int       `json:"estimated_tokens"`
	ActualTokens        int       `json:"actual_tokens"`
	ErrorTokens         int64     `json:"error_tokens"`
	AbsoluteErrorTokens int64     `json:"absolute_error_tokens"`
	SafetyMultiplier    float64   `json:"safety_multiplier"`
	Source              string    `json:"source,omitempty"`
	UpdatedAt           time.Time `json:"updated_at,omitempty"`
}

// TokenizerProfile describes how Context Manifest token estimates were
// produced. A provider/official tokenizer may eventually set Known=true; the
// built-in default remains an explicitly labelled heuristic. Calibration is
// only allowed to increase the estimate and is capped by the helper below.
type TokenizerProfile struct {
	Name        string           `json:"name"`
	Version     string           `json:"version"`
	Source      string           `json:"source"`
	Quality     string           `json:"quality"`
	Known       bool             `json:"known"`
	Confidence  float64          `json:"confidence"`
	Calibration TokenCalibration `json:"calibration"`
}

const (
	DefaultTokenizerName           = "abot-heuristic"
	DefaultTokenizerVersion        = "heuristic-v1"
	DefaultTokenizerSource         = "heuristic"
	DefaultTokenizerQuality        = "heuristic"
	OpenAIInputTokenCounterName    = "openai-input-tokens"
	OpenAIInputTokenCounterVersion = "openai-input-tokens-v1"
	GeminiTokenCounterName         = "gemini-countTokens"
	GeminiTokenCounterVersion      = "gemini-countTokens-v1"
	AnthropicTokenCounterName      = "anthropic-input-tokens"
	AnthropicTokenCounterVersion   = "anthropic-input-tokens-v1"
	MinTokenizerCalibrationSamples = 3
	MaxTokenizerCalibrationSamples = 200
	MaxTokenizerSafetyMultiplier   = 2.0
)

var validTokenizerQualities = map[string]struct{}{
	"exact": {}, "provider": {}, "local": {}, "heuristic": {}, "calibrated": {}, "unknown": {},
}

// DefaultTokenizerProfile is intentionally conservative: no provider-specific
// tokenizer is claimed until an adapter supplies one or usage calibration has
// met the explicit sample threshold.
func DefaultTokenizerProfile() TokenizerProfile {
	return TokenizerProfile{
		Name: DefaultTokenizerName, Version: DefaultTokenizerVersion,
		Source: DefaultTokenizerSource, Quality: DefaultTokenizerQuality,
		Confidence:  0,
		Calibration: TokenCalibration{SafetyMultiplier: 1, Source: "heuristic"},
	}
}

// GeminiTokenCounterProfile describes the official Gemini countTokens API.
// The API counts the complete request shape at the provider boundary, so it is
// safe to use for a manifest when the endpoint succeeds. A failed request
// still falls back to the explicitly labelled heuristic at runtime.
func GeminiTokenCounterProfile() TokenizerProfile {
	return TokenizerProfile{
		Name: GeminiTokenCounterName, Version: GeminiTokenCounterVersion,
		Source: "provider", Quality: "provider", Known: true, Confidence: 0.8,
		Calibration: TokenCalibration{SafetyMultiplier: 1, Source: "provider"},
	}
}

// OpenAIInputTokenCounterProfile describes the official OpenAI input-token
// count endpoint. The endpoint accepts the materialized Responses input shape
// for both official Chat Completions and Responses model routes. Arbitrary
// OpenAI-compatible gateways do not inherit this guarantee; callers must keep
// the heuristic fallback unless their own route has explicit evidence.
func OpenAIInputTokenCounterProfile() TokenizerProfile {
	return TokenizerProfile{
		Name: OpenAIInputTokenCounterName, Version: OpenAIInputTokenCounterVersion,
		Source: "provider", Quality: "provider", Known: true, Confidence: 0.8,
		Calibration: TokenCalibration{SafetyMultiplier: 1, Source: "provider"},
	}
}

// AnthropicTokenCounterProfile describes the official Messages
// /v1/messages/count_tokens endpoint. It is only selected when the provider
// explicitly opts into the Anthropic-shaped count route; generation protocol
// compatibility does not imply this capability.
func AnthropicTokenCounterProfile() TokenizerProfile {
	return TokenizerProfile{
		Name: AnthropicTokenCounterName, Version: AnthropicTokenCounterVersion,
		Source: "provider", Quality: "provider", Known: true, Confidence: 0.95,
		Calibration: TokenCalibration{SafetyMultiplier: 1, Source: "provider"},
	}
}

// EffectiveTokenizer returns a normalized copy suitable for request
// estimation. Missing fields from older persisted profiles are filled with
// the conservative default; malformed values are clamped by callers only
// after profile validation has succeeded.
func EffectiveTokenizer(profile *ModelCapabilityProfile) TokenizerProfile {
	result := DefaultTokenizerProfile()
	if profile == nil {
		return result
	}
	result = profile.Tokenizer
	if result.Name == "" {
		result.Name = DefaultTokenizerName
	}
	if result.Version == "" {
		result.Version = DefaultTokenizerVersion
	}
	if result.Source == "" {
		result.Source = DefaultTokenizerSource
	}
	if result.Quality == "" {
		result.Quality = DefaultTokenizerQuality
	}
	if result.Calibration.SafetyMultiplier < 1 {
		result.Calibration.SafetyMultiplier = 1
	}
	if result.Calibration.SafetyMultiplier > MaxTokenizerSafetyMultiplier {
		result.Calibration.SafetyMultiplier = MaxTokenizerSafetyMultiplier
	}
	if result.Calibration.Samples >= MinTokenizerCalibrationSamples && result.Calibration.SafetyMultiplier > 1 && result.Quality == DefaultTokenizerQuality {
		result.Quality = "calibrated"
	}
	return result
}

// ModelCapabilityProfile is provider-neutral evidence about one model route.
// Unknown is deliberately represented explicitly; callers must not interpret
// a missing probe as support.
type ModelCapabilityProfile struct {
	ProviderID             string               `json:"provider_id"`
	ModelID                string               `json:"model_id"`
	Protocol               Protocol             `json:"protocol"`
	Route                  string               `json:"route,omitempty"`
	ContextWindow          CapabilityValue[int] `json:"context_window"`
	MaxOutputTokens        CapabilityValue[int] `json:"max_output_tokens"`
	ToolCalling            Support              `json:"tool_calling"`
	ParallelToolCalls      Support              `json:"parallel_tool_calls"`
	ToolChoiceModes        []string             `json:"tool_choice_modes,omitempty"`
	StructuredOutput       Support              `json:"structured_output"`
	StructuredOutputSchema Support              `json:"structured_output_schema"`
	JSONSchemaDialect      string               `json:"json_schema_dialect,omitempty"`
	Streaming              Support              `json:"streaming"`
	Images                 Support              `json:"images"`
	InputFiles             Support              `json:"input_files"`
	Audio                  Support              `json:"audio"`
	ReasoningEffort        Support              `json:"reasoning_effort"`
	ReasoningSummary       Support              `json:"reasoning_summary"`
	UsageDetails           []string             `json:"usage_details,omitempty"`
	PromptCaching          Support              `json:"prompt_caching"`
	NativeCompaction       Support              `json:"native_compaction"`
	MaxTools               CapabilityValue[int] `json:"max_tools"`
	MaxSchemaBytes         CapabilityValue[int] `json:"max_schema_bytes"`
	Tokenizer              TokenizerProfile     `json:"tokenizer"`
	SourceRevision         string               `json:"source_revision,omitempty"`
	UpdatedAt              time.Time            `json:"updated_at"`
}

type ModelRequirements struct {
	RequiresTools            bool
	RequiresStructuredOut    bool
	RequiresStructuredSchema bool
	RequiresImages           bool
	RequiresAudio            bool
	RequiresFiles            bool
	RequiresStreaming        bool
	RequiresParallelTools    bool
	// RequestedReasoningEffort is optional. It is projected only when the
	// effective profile explicitly supports reasoning.
	RequestedReasoningEffort string
	RequiresReasoningSummary bool
	MinimumContextWindow     int
	MinimumOutputTokens      int
}

type RequestPlan struct {
	Streaming         bool   `json:"streaming"`
	ToolChoice        string `json:"tool_choice,omitempty"`
	ParallelToolLimit int    `json:"parallel_tool_limit,omitempty"`
	StructuredOutput  bool   `json:"structured_output"`
	StructuredSchema  bool   `json:"structured_schema"`
	JSONSchemaDialect string `json:"json_schema_dialect,omitempty"`
	Images            bool   `json:"images"`
	Audio             bool   `json:"audio"`
	InputFiles        bool   `json:"input_files"`
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
	ReasoningSummary  bool   `json:"reasoning_summary"`
	MaxOutputTokens   int    `json:"max_output_tokens,omitempty"`
	ContextWindow     int    `json:"context_window,omitempty"`
	CaptureUsage      bool   `json:"capture_usage"`
}

type DisabledFeature struct {
	Feature string `json:"feature"`
	Reason  string `json:"reason"`
}

type NegotiationResult struct {
	ProfileSnapshotID string                 `json:"profile_snapshot_id"`
	Profile           ModelCapabilityProfile `json:"profile"`
	AllowedFeatures   []string               `json:"allowed_features,omitempty"`
	DisabledFeatures  []DisabledFeature      `json:"disabled_features,omitempty"`
	RequestPlan       RequestPlan            `json:"request_plan"`
	Warnings          []string               `json:"warnings,omitempty"`
	Compatible        bool                   `json:"compatible"`
	FailureCode       string                 `json:"failure_code,omitempty"`
}

type CapabilitySnapshot struct {
	ID           string            `json:"id"`
	InvocationID string            `json:"invocation_id"`
	Result       NegotiationResult `json:"result"`
	CreatedAt    time.Time         `json:"created_at"`
}

var (
	ErrInvalidCapabilityProfile   = errors.New("模型能力 Profile 无效")
	ErrCapabilitySnapshotConflict = errors.New("模型能力 Snapshot 已固定，不能静默改变")
	ErrInvalidRequestPlan         = errors.New("模型 RequestPlan 无效")
)

// Validate checks the final provider-neutral request shape against the
// effective capability profile. Adapters may still reject a provider-specific
// keyword, but an unknown/unsupported feature must never be sent merely
// because a caller assembled a RequestPlan by hand.
func (p RequestPlan) Validate(profile ModelCapabilityProfile) error {
	profile = normalizeCapabilityProfile(profile)
	if err := profile.Validate(); err != nil {
		return err
	}
	check := func(feature string, enabled bool, support Support) error {
		if !enabled {
			return nil
		}
		if support.State != SupportSupported && support.State != SupportDegraded {
			return fmt.Errorf("%w: %s 状态为 %s", ErrInvalidRequestPlan, feature, support.State)
		}
		return nil
	}
	if err := check("streaming", p.Streaming, profile.Streaming); err != nil {
		return err
	}
	if err := check("structured_output", p.StructuredOutput, profile.StructuredOutput); err != nil {
		return err
	}
	if err := check("structured_output_schema", p.StructuredSchema, profile.StructuredOutputSchema); err != nil {
		return err
	}
	if choice := strings.TrimSpace(p.ToolChoice); choice != "" {
		if err := check("tool_calling", true, profile.ToolCalling); err != nil {
			return err
		}
		if len(profile.ToolChoiceModes) > 0 && !toolChoiceModeSupported(choice, profile.ToolChoiceModes) {
			return fmt.Errorf("%w: tool_choice %q 不在模型支持模式内", ErrInvalidRequestPlan, choice)
		}
	}
	if p.StructuredSchema {
		if !p.StructuredOutput {
			return fmt.Errorf("%w: structured schema 必须同时启用 structured output", ErrInvalidRequestPlan)
		}
		if strings.TrimSpace(p.JSONSchemaDialect) == "" || strings.TrimSpace(profile.JSONSchemaDialect) == "" || strings.TrimSpace(p.JSONSchemaDialect) != strings.TrimSpace(profile.JSONSchemaDialect) {
			return fmt.Errorf("%w: structured schema 缺少匹配的 JSON Schema dialect", ErrInvalidRequestPlan)
		}
	} else if strings.TrimSpace(p.JSONSchemaDialect) != "" {
		return fmt.Errorf("%w: 未启用 structured schema 时不能携带 JSON Schema dialect", ErrInvalidRequestPlan)
	}
	if err := check("images", p.Images, profile.Images); err != nil {
		return err
	}
	if err := check("audio", p.Audio, profile.Audio); err != nil {
		return err
	}
	if err := check("input_files", p.InputFiles, profile.InputFiles); err != nil {
		return err
	}
	if strings.TrimSpace(p.ReasoningEffort) != "" {
		if err := check("reasoning_effort", true, profile.ReasoningEffort); err != nil {
			return err
		}
	}
	if err := check("reasoning_summary", p.ReasoningSummary, profile.ReasoningSummary); err != nil {
		return err
	}
	if p.ParallelToolLimit < 0 {
		return fmt.Errorf("%w: parallel_tool_limit 不能为负数", ErrInvalidRequestPlan)
	}
	if p.ParallelToolLimit > 1 {
		if err := check("parallel_tool_calls", true, profile.ParallelToolCalls); err != nil {
			return err
		}
	}
	if p.MaxOutputTokens < 0 || (profile.MaxOutputTokens.Known && p.MaxOutputTokens > profile.MaxOutputTokens.Value) {
		return fmt.Errorf("%w: max_output_tokens 超出模型上限", ErrInvalidRequestPlan)
	}
	if p.ContextWindow < 0 || (profile.ContextWindow.Known && p.ContextWindow > profile.ContextWindow.Value) {
		return fmt.Errorf("%w: context_window 超出模型上限", ErrInvalidRequestPlan)
	}
	return nil
}

func toolChoiceModeSupported(choice string, modes []string) bool {
	choice = strings.ToLower(strings.TrimSpace(choice))
	for _, mode := range modes {
		mode = strings.ToLower(strings.TrimSpace(mode))
		if mode == choice || (choice == "required" && mode == "any") || (choice == "any" && mode == "required") {
			return true
		}
	}
	return false
}

func (s CapabilitySnapshot) Validate() error {
	if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.InvocationID) == "" {
		return fmt.Errorf("%w: capability snapshot 缺少 id/invocation_id", ErrInvalidCapabilityProfile)
	}
	if strings.TrimSpace(s.Result.ProfileSnapshotID) == "" {
		return fmt.Errorf("%w: capability snapshot 缺少 profile snapshot", ErrInvalidCapabilityProfile)
	}
	if err := s.Result.Profile.Validate(); err != nil {
		return err
	}
	return nil
}

func (p ModelCapabilityProfile) Validate() error {
	// Profiles persisted before structured schema evidence existed have no
	// value for the new field. Treat that omission conservatively as unknown
	// instead of making old catalog rows unreadable.
	if p.StructuredOutputSchema.State == "" {
		p.StructuredOutputSchema = Support{State: SupportUnknown, Source: "legacy_default"}
	}
	// Audio was added after the original capability profile schema. Missing
	// audio evidence is likewise unknown, never an implicit supported claim.
	if p.Audio.State == "" {
		p.Audio = Support{State: SupportUnknown, Source: "legacy_default"}
	}
	if strings.TrimSpace(p.ProviderID) == "" || strings.TrimSpace(p.ModelID) == "" {
		return fmt.Errorf("%w: provider_id/model_id 不能为空", ErrInvalidCapabilityProfile)
	}
	for name, value := range map[string]Support{
		"tool_calling": p.ToolCalling, "parallel_tool_calls": p.ParallelToolCalls, "structured_output": p.StructuredOutput, "structured_output_schema": p.StructuredOutputSchema,
		"streaming": p.Streaming, "images": p.Images, "input_files": p.InputFiles, "audio": p.Audio,
		"reasoning_effort": p.ReasoningEffort, "reasoning_summary": p.ReasoningSummary, "prompt_caching": p.PromptCaching, "native_compaction": p.NativeCompaction,
	} {
		if value.State != SupportSupported && value.State != SupportUnsupported && value.State != SupportUnknown && value.State != SupportDegraded {
			return fmt.Errorf("%w: %s state 无效", ErrInvalidCapabilityProfile, name)
		}
		if value.Confidence < 0 || value.Confidence > 1 {
			return fmt.Errorf("%w: %s confidence 超出范围", ErrInvalidCapabilityProfile, name)
		}
	}
	for name, value := range map[string]CapabilityValue[int]{"context_window": p.ContextWindow, "max_output_tokens": p.MaxOutputTokens, "max_tools": p.MaxTools, "max_schema_bytes": p.MaxSchemaBytes} {
		if value.Value < 0 {
			return fmt.Errorf("%w: %s 不能为负数", ErrInvalidCapabilityProfile, name)
		}
		if value.Confidence < 0 || value.Confidence > 1 {
			return fmt.Errorf("%w: %s confidence 超出范围", ErrInvalidCapabilityProfile, name)
		}
	}
	if p.Tokenizer.Confidence < 0 || p.Tokenizer.Confidence > 1 {
		return fmt.Errorf("%w: tokenizer confidence 超出范围", ErrInvalidCapabilityProfile)
	}
	if quality := strings.TrimSpace(p.Tokenizer.Quality); quality != "" {
		if _, ok := validTokenizerQualities[quality]; !ok {
			return fmt.Errorf("%w: tokenizer quality %q 无效", ErrInvalidCapabilityProfile, quality)
		}
	}
	calibration := p.Tokenizer.Calibration
	if calibration.Samples < 0 || calibration.EstimatedTokens < 0 || calibration.ActualTokens < 0 || calibration.AbsoluteErrorTokens < 0 {
		return fmt.Errorf("%w: tokenizer calibration 数值无效", ErrInvalidCapabilityProfile)
	}
	if calibration.Samples > MaxTokenizerCalibrationSamples {
		return fmt.Errorf("%w: tokenizer calibration samples 超出上限", ErrInvalidCapabilityProfile)
	}
	if calibration.SafetyMultiplier < 0 || calibration.SafetyMultiplier > MaxTokenizerSafetyMultiplier {
		return fmt.Errorf("%w: tokenizer safety multiplier 超出范围", ErrInvalidCapabilityProfile)
	}
	return nil
}

// validate 按调用场景校验能力选择；普通 override 保持收紧策略，管理界面的手动选择允许管理员明确声明能力。
func (o CapabilityOverrides) validate(allowSupported bool) error {
	for name, value := range map[string]*SupportState{
		"tool_calling": o.ToolCalling, "parallel_tool_calls": o.ParallelToolCalls,
		"structured_output": o.StructuredOutput, "structured_output_schema": o.StructuredOutputSchema, "streaming": o.Streaming,
		"images": o.Images, "input_files": o.InputFiles, "audio": o.Audio,
		"reasoning_effort": o.ReasoningEffort, "reasoning_summary": o.ReasoningSummary,
		"prompt_caching": o.PromptCaching, "native_compaction": o.NativeCompaction,
	} {
		if value == nil {
			continue
		}
		switch *value {
		case SupportUnsupported, SupportUnknown, SupportDegraded:
			// 这些状态不会产生超出模型能力的请求，普通 override 和手动选择都允许。
		case SupportSupported:
			if !allowSupported {
				return fmt.Errorf("%w: %s 不能通过保守 override 声明 supported", ErrInvalidCapabilityOverride, name)
			}
		default:
			return fmt.Errorf("%w: %s state 无效", ErrInvalidCapabilityOverride, name)
		}
	}
	return nil
}

// Validate 保留运行时保守 override 的旧契约，避免普通运行路径伪造供应商证据。
func (o CapabilityOverrides) Validate() error {
	return o.validate(false)
}

// validateManual 供已授权的管理界面使用，允许管理员在探测结果 unknown 或错误时明确选择能力。
func (o CapabilityOverrides) validateManual() error {
	return o.validate(true)
}

// ApplyCapabilityOverrides returns a new effective profile and leaves the
// catalog profile untouched. Overrides can only move a support state toward a
// more conservative value (supported -> degraded/unknown/unsupported,
// degraded -> unknown/unsupported, unknown -> unsupported). Every changed
// field records user_override provenance so the UI and Invocation snapshot do
// not mistake it for an adapter or probe fact.
func ApplyCapabilityOverrides(profile ModelCapabilityProfile, overrides CapabilityOverrides) (ModelCapabilityProfile, error) {
	return applyCapabilityOverrides(profile, overrides, false)
}

// ApplyManualCapabilityOverrides 应用管理界面的显式能力选择。
// 这里的 user_override 是管理员配置，不冒充探测证据；后续请求会按该选择参与能力协商。
func ApplyManualCapabilityOverrides(profile ModelCapabilityProfile, overrides CapabilityOverrides) (ModelCapabilityProfile, error) {
	return applyCapabilityOverrides(profile, overrides, true)
}

// applyCapabilityOverrides 根据调用方是否为管理界面选择能力，决定是否允许把状态提升为 supported。
func applyCapabilityOverrides(profile ModelCapabilityProfile, overrides CapabilityOverrides, manual bool) (ModelCapabilityProfile, error) {
	profile = normalizeCapabilityProfile(profile)
	if err := profile.Validate(); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if manual {
		if err := overrides.validateManual(); err != nil {
			return ModelCapabilityProfile{}, err
		}
	} else if err := overrides.Validate(); err != nil {
		return ModelCapabilityProfile{}, err
	}
	result := profile
	now := time.Now().UTC()
	changed := false
	apply := func(name string, current *Support, requested *SupportState) error {
		if requested == nil {
			return nil
		}
		if !manual && supportRank(*requested) > supportRank(current.State) {
			return fmt.Errorf("%w: %s 只能收紧当前状态 %q", ErrInvalidCapabilityOverride, name, current.State)
		}
		current.State = *requested
		current.Source = "user_override"
		current.Confidence = 1
		current.ObservedAt = now
		if manual {
			current.Reason = "manual capability selection"
		} else {
			current.Reason = "manual conservative override"
		}
		changed = true
		return nil
	}
	if err := apply("tool_calling", &result.ToolCalling, overrides.ToolCalling); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("parallel_tool_calls", &result.ParallelToolCalls, overrides.ParallelToolCalls); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("structured_output", &result.StructuredOutput, overrides.StructuredOutput); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("structured_output_schema", &result.StructuredOutputSchema, overrides.StructuredOutputSchema); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("streaming", &result.Streaming, overrides.Streaming); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("images", &result.Images, overrides.Images); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("input_files", &result.InputFiles, overrides.InputFiles); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("audio", &result.Audio, overrides.Audio); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("reasoning_effort", &result.ReasoningEffort, overrides.ReasoningEffort); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("reasoning_summary", &result.ReasoningSummary, overrides.ReasoningSummary); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("prompt_caching", &result.PromptCaching, overrides.PromptCaching); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if err := apply("native_compaction", &result.NativeCompaction, overrides.NativeCompaction); err != nil {
		return ModelCapabilityProfile{}, err
	}
	if !changed {
		return result, nil
	}
	if result.SourceRevision == "" {
		result.SourceRevision = "catalog"
	}
	if !strings.HasSuffix(result.SourceRevision, ":user_override") {
		result.SourceRevision += ":user_override"
	}
	result.UpdatedAt = now
	return result, nil
}

func supportRank(value SupportState) int {
	switch value {
	case SupportSupported:
		return 3
	case SupportDegraded:
		return 2
	case SupportUnknown:
		return 1
	case SupportUnsupported:
		return 0
	default:
		return -1
	}
}

// DefaultCapabilities creates conservative protocol evidence from adapter
// guarantees and discovered model limits. Unknown remains unknown where the
// protocol does not guarantee a feature for every compatible deployment.
func DefaultCapabilities(p Provider, m Model) ModelCapabilityProfile {
	route := strings.TrimSpace(string(p.OpenAIFormat))
	switch normalizeProtocol(p.Protocol) {
	case ProtocolGemini:
		route = "gemini"
	case ProtocolOpenAICompatible:
		if route == "" {
			route = string(OpenAIFormatAuto)
		}
	}
	profile := ModelCapabilityProfile{ProviderID: p.ID, ModelID: m.ID, Protocol: p.Protocol, Route: route, SourceRevision: p.UpdatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: time.Now().UTC(),
		ContextWindow:   CapabilityValue[int]{Value: m.ContextWindow, Known: m.ContextWindow > 0, Source: "catalog", Confidence: boolConfidence(m.ContextWindow > 0)},
		MaxOutputTokens: CapabilityValue[int]{Value: m.MaxOutputTokens, Known: m.MaxOutputTokens > 0, Source: "catalog", Confidence: boolConfidence(m.MaxOutputTokens > 0)},
		MaxTools:        CapabilityValue[int]{Source: "protocol"}, MaxSchemaBytes: CapabilityValue[int]{Source: "protocol"},
		Tokenizer:   DefaultTokenizerProfile(),
		ToolCalling: Support{State: SupportUnknown, Source: "conservative_default"}, ParallelToolCalls: Support{State: SupportUnknown, Source: "conservative_default"},
		StructuredOutput: Support{State: SupportUnknown, Source: "conservative_default"}, StructuredOutputSchema: Support{State: SupportUnknown, Source: "conservative_default"}, Streaming: Support{State: SupportUnknown, Source: "conservative_default"},
		Images: Support{State: SupportUnknown, Source: "conservative_default"}, InputFiles: Support{State: SupportUnknown, Source: "conservative_default"}, Audio: Support{State: SupportUnknown, Source: "conservative_default"},
		ReasoningEffort: Support{State: SupportUnknown, Source: "conservative_default"}, ReasoningSummary: Support{State: SupportUnknown, Source: "conservative_default"},
		PromptCaching: Support{State: SupportUnknown, Source: "conservative_default"}, NativeCompaction: Support{State: SupportUnknown, Source: "conservative_default"},
	}
	switch p.Protocol {
	case ProtocolGemini:
		profile.Tokenizer = GeminiTokenCounterProfile()
		profile.ToolCalling = Support{State: SupportSupported, Source: "gemini_adapter", Confidence: 1}
		profile.ParallelToolCalls = Support{State: SupportSupported, Source: "gemini_adapter", Confidence: 0.8}
		profile.Streaming = Support{State: SupportSupported, Source: "gemini_adapter", Confidence: 1}
		profile.Images = Support{State: SupportSupported, Source: "gemini_adapter", Confidence: 0.8}
		profile.InputFiles = Support{State: SupportSupported, Source: "gemini_adapter", Confidence: 0.7}
		profile.ToolChoiceModes = []string{"auto", "none", "any"}
	case ProtocolOpenAICompatible, ProtocolOpenAICompletions:
		profile.ToolCalling = Support{State: SupportSupported, Source: "openai_adapter", Confidence: 0.8}
		profile.Streaming = Support{State: SupportSupported, Source: "openai_adapter", Confidence: 0.8}
		profile.ToolChoiceModes = []string{"auto", "none", "any"}
		if isOfficialOpenAIAPIBaseURL(p.BaseURL) {
			profile.Tokenizer = OpenAIInputTokenCounterProfile()
		}
	}
	if p.TokenCountProtocol == TokenCountProtocolAnthropic {
		// Configuration only selects a route. Do not claim exact tokenization
		// until an explicit probe succeeds for this provider/model endpoint.
		profile.Tokenizer = DefaultTokenizerProfile()
	}
	return profile
}

// BuildCapabilityProfiles returns a deterministic, metadata-only projection of
// the configured model catalog. Persisted probe evidence is preferred; models
// without a profile receive the conservative protocol default. Provider API
// keys, URLs and model display metadata are deliberately not copied into the
// projection so it can be exported to a release gate safely.
//
// The returned slice is sorted by provider/model/protocol/route and every
// profile is validated before any result is returned. A malformed catalog is
// therefore fail-closed instead of producing a partial candidate document.
func BuildCapabilityProfiles(providers []Provider) ([]ModelCapabilityProfile, error) {
	ordered := append([]Provider(nil), providers...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return strings.TrimSpace(ordered[i].ID) < strings.TrimSpace(ordered[j].ID)
	})
	result := make([]ModelCapabilityProfile, 0)
	for _, item := range ordered {
		models := append([]Model(nil), item.Models...)
		sort.SliceStable(models, func(i, j int) bool {
			return strings.TrimSpace(models[i].ID) < strings.TrimSpace(models[j].ID)
		})
		for _, model := range models {
			profile := DefaultCapabilities(item, model)
			if model.Capabilities != nil {
				profile = NormalizeCapabilityProfile(*model.Capabilities)
			}
			// Older rows may predate identity fields. Fill only omitted identity;
			// an explicit mismatch is rejected rather than exported under the
			// wrong provider/model key.
			if strings.TrimSpace(profile.ProviderID) == "" {
				profile.ProviderID = strings.TrimSpace(item.ID)
			}
			if strings.TrimSpace(profile.ModelID) == "" {
				profile.ModelID = strings.TrimSpace(model.ID)
			}
			if profile.Protocol == "" {
				profile.Protocol = item.Protocol
			}
			if strings.TrimSpace(profile.Route) == "" {
				profile.Route = DefaultCapabilities(item, model).Route
			}
			if profile.ProviderID != strings.TrimSpace(item.ID) || profile.ModelID != strings.TrimSpace(model.ID) || normalizeProtocol(profile.Protocol) != normalizeProtocol(item.Protocol) {
				return nil, fmt.Errorf("%w: %s/%s 能力 profile 身份不匹配", ErrInvalidCapabilityProfile, item.ID, model.ID)
			}
			if err := profile.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %s/%s 能力 profile 无法导出: %v", ErrInvalidCapabilityProfile, item.ID, model.ID, err)
			}
			result = append(result, profile)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left := strings.Join([]string{result[i].ProviderID, result[i].ModelID, string(normalizeProtocol(result[i].Protocol)), result[i].Route}, "\x00")
		right := strings.Join([]string{result[j].ProviderID, result[j].ModelID, string(normalizeProtocol(result[j].Protocol)), result[j].Route}, "\x00")
		return left < right
	})
	return result, nil
}

func isOfficialOpenAIAPIBaseURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" {
		return false
	}
	// A non-standard port can be a reverse proxy even when the hostname is
	// api.openai.com, so only the canonical HTTPS origin inherits the official
	// count endpoint guarantee.
	return strings.EqualFold(u.Hostname(), "api.openai.com") && (u.Port() == "" || u.Port() == "443")
}

func isOpenAIResponsesRoute(p Provider) bool {
	if p.OpenAIFormat == OpenAIFormatResponses {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(p.BaseURL))
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/responses")
}

func boolConfidence(known bool) float64 {
	if known {
		return 0.8
	}
	return 0
}

func (p ModelCapabilityProfile) SnapshotID() string {
	copyProfile := p
	copyProfile.UpdatedAt = time.Time{}
	encoded, _ := json.Marshal(copyProfile)
	sum := sha256.Sum256(encoded)
	return "capability:" + hex.EncodeToString(sum[:])[:24]
}

// Negotiate applies conservative requirements and records every allowed
// downgrade. It never turns unknown into supported for a required feature.
func Negotiate(profile ModelCapabilityProfile, requirements ModelRequirements, requestedStreaming bool) (NegotiationResult, error) {
	profile = normalizeCapabilityProfile(profile)
	if err := profile.Validate(); err != nil {
		return NegotiationResult{}, err
	}
	result := NegotiationResult{Profile: profile, ProfileSnapshotID: profile.SnapshotID(), Compatible: true, RequestPlan: RequestPlan{Streaming: requestedStreaming, CaptureUsage: true}}
	checkRequired := func(feature string, support Support, required bool) {
		if !required {
			return
		}
		if support.State == SupportUnsupported || support.State == SupportUnknown {
			result.Compatible = false
			if result.FailureCode == "" {
				result.FailureCode = "model_incompatible_" + feature
			}
			result.DisabledFeatures = append(result.DisabledFeatures, DisabledFeature{Feature: feature, Reason: string(support.State)})
		}
	}
	checkRequired("tool_calling", profile.ToolCalling, requirements.RequiresTools)
	checkRequired("structured_output", profile.StructuredOutput, requirements.RequiresStructuredOut)
	checkRequired("structured_output_schema", profile.StructuredOutputSchema, requirements.RequiresStructuredSchema)
	checkRequired("images", profile.Images, requirements.RequiresImages)
	checkRequired("audio", profile.Audio, requirements.RequiresAudio)
	checkRequired("input_files", profile.InputFiles, requirements.RequiresFiles)
	checkRequired("streaming", profile.Streaming, requirements.RequiresStreaming)
	checkRequired("parallel_tool_calls", profile.ParallelToolCalls, requirements.RequiresParallelTools)
	if profile.ContextWindow.Known && requirements.MinimumContextWindow > 0 && profile.ContextWindow.Value < requirements.MinimumContextWindow {
		result.Compatible = false
		result.FailureCode = "model_context_too_small"
	} else if !profile.ContextWindow.Known && requirements.MinimumContextWindow > 0 {
		result.Compatible = false
		result.FailureCode = "model_context_unknown"
	}
	if profile.MaxOutputTokens.Known && requirements.MinimumOutputTokens > 0 && profile.MaxOutputTokens.Value < requirements.MinimumOutputTokens {
		result.Compatible = false
		result.FailureCode = "model_output_too_small"
	}
	if requestedStreaming {
		switch profile.Streaming.State {
		case SupportSupported, SupportDegraded:
			result.AllowedFeatures = append(result.AllowedFeatures, "streaming")
		case SupportUnsupported:
			result.RequestPlan.Streaming = false
			result.DisabledFeatures = append(result.DisabledFeatures, DisabledFeature{Feature: "streaming", Reason: "unsupported"})
			result.Warnings = append(result.Warnings, "streaming_disabled")
		case SupportUnknown:
			result.RequestPlan.Streaming = false
			result.DisabledFeatures = append(result.DisabledFeatures, DisabledFeature{Feature: "streaming", Reason: "unknown"})
			result.Warnings = append(result.Warnings, "streaming_conservative_serialization")
		}
	}
	if requirements.RequiresParallelTools {
		if profile.ParallelToolCalls.State == SupportSupported || profile.ParallelToolCalls.State == SupportDegraded {
			result.RequestPlan.ParallelToolLimit = 4
		} else {
			result.RequestPlan.ParallelToolLimit = 1
			result.Warnings = append(result.Warnings, "parallel_tools_downgraded_to_serial")
		}
	} else if profile.ParallelToolCalls.State == SupportSupported {
		result.RequestPlan.ParallelToolLimit = 4
	} else {
		result.RequestPlan.ParallelToolLimit = 1
	}
	if profile.ContextWindow.Known {
		result.RequestPlan.ContextWindow = profile.ContextWindow.Value
	}
	if profile.MaxOutputTokens.Known {
		result.RequestPlan.MaxOutputTokens = profile.MaxOutputTokens.Value
	}
	if profile.ToolCalling.State == SupportSupported || profile.ToolCalling.State == SupportDegraded {
		result.RequestPlan.ToolChoice = "auto"
		if requirements.RequiresTools {
			result.AllowedFeatures = append(result.AllowedFeatures, "tool_calling")
		}
	}
	if profile.StructuredOutput.State == SupportSupported || profile.StructuredOutput.State == SupportDegraded {
		result.RequestPlan.StructuredOutput = requirements.RequiresStructuredOut
	}
	if requirements.RequiresStructuredSchema {
		switch profile.StructuredOutputSchema.State {
		case SupportSupported:
			result.RequestPlan.StructuredOutput = true
			result.RequestPlan.StructuredSchema = true
			result.RequestPlan.JSONSchemaDialect = profile.JSONSchemaDialect
			result.AllowedFeatures = append(result.AllowedFeatures, "structured_output_schema")
		case SupportDegraded:
			result.RequestPlan.StructuredOutput = true
			result.RequestPlan.StructuredSchema = true
			result.RequestPlan.JSONSchemaDialect = profile.JSONSchemaDialect
			result.Warnings = append(result.Warnings, "structured_output_schema_degraded")
			result.AllowedFeatures = append(result.AllowedFeatures, "structured_output_schema")
		case SupportUnsupported, SupportUnknown:
			result.DisabledFeatures = append(result.DisabledFeatures, DisabledFeature{Feature: "structured_output_schema", Reason: string(profile.StructuredOutputSchema.State)})
		}
	}
	if profile.Images.State == SupportSupported || profile.Images.State == SupportDegraded {
		result.RequestPlan.Images = requirements.RequiresImages
	}
	if profile.Audio.State == SupportSupported || profile.Audio.State == SupportDegraded {
		result.RequestPlan.Audio = requirements.RequiresAudio
	}
	if profile.InputFiles.State == SupportSupported || profile.InputFiles.State == SupportDegraded {
		result.RequestPlan.InputFiles = requirements.RequiresFiles
	}
	if effort := strings.TrimSpace(requirements.RequestedReasoningEffort); effort != "" {
		switch profile.ReasoningEffort.State {
		case SupportSupported, SupportDegraded:
			result.RequestPlan.ReasoningEffort = effort
			result.AllowedFeatures = append(result.AllowedFeatures, "reasoning_effort")
		case SupportUnsupported, SupportUnknown:
			result.Warnings = append(result.Warnings, "reasoning_effort_omitted")
			result.DisabledFeatures = append(result.DisabledFeatures, DisabledFeature{Feature: "reasoning_effort", Reason: string(profile.ReasoningEffort.State)})
		}
	}
	if requirements.RequiresReasoningSummary {
		switch profile.ReasoningSummary.State {
		case SupportSupported, SupportDegraded:
			result.RequestPlan.ReasoningSummary = true
			result.AllowedFeatures = append(result.AllowedFeatures, "reasoning_summary")
		case SupportUnsupported, SupportUnknown:
			result.Warnings = append(result.Warnings, "reasoning_summary_omitted")
			result.DisabledFeatures = append(result.DisabledFeatures, DisabledFeature{Feature: "reasoning_summary", Reason: string(profile.ReasoningSummary.State)})
		}
	}
	if err := result.RequestPlan.Validate(profile); err != nil {
		return NegotiationResult{}, err
	}
	return result, nil
}

func normalizeCapabilityProfile(profile ModelCapabilityProfile) ModelCapabilityProfile {
	if profile.StructuredOutputSchema.State == "" {
		profile.StructuredOutputSchema = Support{State: SupportUnknown, Source: "legacy_default"}
	}
	if profile.Audio.State == "" {
		profile.Audio = Support{State: SupportUnknown, Source: "legacy_default"}
	}
	// A successful bounded schema request is also direct evidence that the
	// provider can produce structured JSON. Promote only an unknown base state;
	// explicit user or provider restrictions remain authoritative.
	if (profile.StructuredOutputSchema.State == SupportSupported || profile.StructuredOutputSchema.State == SupportDegraded) &&
		profile.StructuredOutput.State == SupportUnknown && profile.StructuredOutput.Source != "user_override" {
		profile.StructuredOutput = profile.StructuredOutputSchema
	}
	return profile
}

// NormalizeCapabilityProfile fills fields introduced after older catalog
// rows were persisted. It never upgrades a capability; missing schema
// evidence remains unknown and only a bounded schema success can promote the
// broader structured-output state.
func NormalizeCapabilityProfile(profile ModelCapabilityProfile) ModelCapabilityProfile {
	return normalizeCapabilityProfile(profile)
}
