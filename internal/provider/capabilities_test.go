package provider

import (
	"errors"
	"testing"
)

func TestDefaultCapabilitiesAreProtocolConservative(t *testing.T) {
	openAI := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	if openAI.ToolCalling.State != SupportSupported || openAI.Streaming.State != SupportSupported {
		t.Fatalf("OpenAI adapter capability=%#v", openAI)
	}
	if openAI.Images.State != SupportUnknown || openAI.StructuredOutput.State != SupportUnknown || openAI.StructuredOutputSchema.State != SupportUnknown {
		t.Fatalf("unknown protocol details must remain unknown: %#v", openAI)
	}
	if openAI.Tokenizer.Quality != DefaultTokenizerQuality || openAI.Tokenizer.Calibration.SafetyMultiplier != 1 {
		t.Fatalf("默认 tokenizer 必须明确标记为未校准 heuristic: %#v", openAI.Tokenizer)
	}
	gemini := DefaultCapabilities(Provider{ID: "gemini", Protocol: ProtocolGemini}, Model{ID: "model"})
	if gemini.Route != "gemini" || gemini.ToolCalling.State != SupportSupported || gemini.Images.State != SupportSupported {
		t.Fatalf("Gemini capability=%#v", gemini)
	}
	auto := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	if auto.Route != string(OpenAIFormatAuto) {
		t.Fatalf("OpenAI 未指定线路时应使用 auto route: %#v", auto)
	}
	responses := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatResponses, BaseURL: "https://api.openai.com/v1"}, Model{ID: "model"})
	if !responses.Tokenizer.Known || responses.Tokenizer.Name != OpenAIInputTokenCounterName || responses.Tokenizer.Version != OpenAIInputTokenCounterVersion {
		t.Fatalf("Responses 线路应声明官方 input token counter: %#v", responses.Tokenizer)
	}
	autoResponses := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.openai.com/v1/responses"}, Model{ID: "model"})
	if !autoResponses.Tokenizer.Known || autoResponses.Tokenizer.Name != OpenAIInputTokenCounterName {
		t.Fatalf("官方 Responses URL 应识别 input token counter: %#v", autoResponses.Tokenizer)
	}
	chat := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat, BaseURL: "https://api.openai.com/v1/chat/completions"}, Model{ID: "model"})
	if !chat.Tokenizer.Known || chat.Tokenizer.Name != OpenAIInputTokenCounterName || chat.Tokenizer.Version != OpenAIInputTokenCounterVersion {
		t.Fatalf("官方 Chat 线路应复用 input token counter: %#v", chat.Tokenizer)
	}
	completions := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompletions, OpenAIFormat: OpenAIFormatChat, BaseURL: "https://api.openai.com/v1"}, Model{ID: "model"})
	if !completions.Tokenizer.Known {
		t.Fatalf("旧 OpenAI completions 协议别名也应识别官方 input token counter: %#v", completions.Tokenizer)
	}
	nonCanonicalPort := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat, BaseURL: "https://api.openai.com:8443/v1"}, Model{ID: "model"})
	if nonCanonicalPort.Tokenizer.Known {
		t.Fatalf("非标准端口可能是代理，不应声明官方 tokenizer: %#v", nonCanonicalPort.Tokenizer)
	}
	compatibleResponses := DefaultCapabilities(Provider{ID: "gateway", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatResponses, BaseURL: "https://gateway.example/v1"}, Model{ID: "model"})
	if compatibleResponses.Tokenizer.Known || compatibleResponses.Tokenizer.Quality != DefaultTokenizerQuality {
		t.Fatalf("兼容网关不应未经探测声明官方 tokenizer: %#v", compatibleResponses.Tokenizer)
	}
}

func TestBuildCapabilityProfilesIsDeterministicAndFailClosed(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "alpha", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat}, Model{ID: "explicit"})
	profile.ToolCalling = Support{State: SupportDegraded, Source: "probe"}
	providers := []Provider{
		{ID: "zeta", APIKey: "do-not-export", Protocol: ProtocolGemini, Models: []Model{{ID: "z-model", Enabled: true}}},
		{ID: "alpha", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat, Models: []Model{
			{ID: "z-model", Enabled: true},
			{ID: "explicit", Enabled: true, Capabilities: &profile},
		}},
	}
	profiles, err := BuildCapabilityProfiles(providers)
	if err != nil {
		t.Fatalf("导出能力 profile 失败: %v", err)
	}
	if len(profiles) != 3 {
		t.Fatalf("能力 profile 数量=%d，期望 3", len(profiles))
	}
	if profiles[0].ProviderID != "alpha" || profiles[0].ModelID != "explicit" || profiles[1].ProviderID != "alpha" || profiles[1].ModelID != "z-model" || profiles[2].ProviderID != "zeta" {
		t.Fatalf("能力 profile 排序不稳定: %#v", profiles)
	}
	if profiles[0].ToolCalling.State != SupportDegraded || profiles[1].ToolCalling.State != SupportSupported {
		t.Fatalf("应优先使用持久化 profile、否则使用协议默认: %#v", profiles)
	}
	profiles[0].ToolCalling.State = SupportUnsupported
	if profile.ToolCalling.State != SupportDegraded {
		t.Fatal("导出结果不应修改持久化 profile")
	}

	bad := profile
	bad.ProviderID = "other-provider"
	_, err = BuildCapabilityProfiles([]Provider{{
		ID: "alpha", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "explicit", Capabilities: &bad}},
	}})
	if !errors.Is(err, ErrInvalidCapabilityProfile) {
		t.Fatalf("能力 profile 身份错配应 fail-closed: %v", err)
	}
}

func TestEffectiveTokenizerNormalizesLegacyProfileAndCapsMultiplier(t *testing.T) {
	profile := ModelCapabilityProfile{}
	tokenizer := EffectiveTokenizer(&profile)
	if tokenizer.Name != DefaultTokenizerName || tokenizer.Version != DefaultTokenizerVersion || tokenizer.Quality != DefaultTokenizerQuality || tokenizer.Calibration.SafetyMultiplier != 1 {
		t.Fatalf("旧 profile 的 tokenizer 归一化不正确: %#v", tokenizer)
	}
	profile.Tokenizer.Calibration.Samples = MinTokenizerCalibrationSamples
	profile.Tokenizer.Calibration.SafetyMultiplier = MaxTokenizerSafetyMultiplier + 1
	profile.Tokenizer.Quality = DefaultTokenizerQuality
	tokenizer = EffectiveTokenizer(&profile)
	if tokenizer.Calibration.SafetyMultiplier != MaxTokenizerSafetyMultiplier || tokenizer.Quality != "calibrated" {
		t.Fatalf("tokenizer multiplier/quality 未按边界归一化: %#v", tokenizer)
	}
}

func TestNegotiateRejectsRequiredUnknownAndDowngradesOptional(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible}, Model{ID: "model", ContextWindow: 8192, MaxOutputTokens: 512})
	result, err := Negotiate(profile, ModelRequirements{RequiresTools: true, RequiresImages: true, RequiresParallelTools: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Compatible || result.FailureCode == "" {
		t.Fatalf("required unknown image capability should fail: %#v", result)
	}
	if result.RequestPlan.Streaming != true || result.RequestPlan.ParallelToolLimit != 1 {
		t.Fatalf("supported streaming and unknown parallel downgrade expected: %#v", result.RequestPlan)
	}

	optional, err := Negotiate(profile, ModelRequirements{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if optional.RequestPlan.Streaming != true || optional.RequestPlan.ParallelToolLimit != 1 {
		t.Fatalf("optional streaming should stay enabled when supported: %#v", optional.RequestPlan)
	}
}

func TestNegotiateAudioRequirementAndRequestPlan(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "audio", Protocol: ProtocolOpenAICompatible}, Model{ID: "model", ContextWindow: 8192})
	unknown, err := Negotiate(profile, ModelRequirements{RequiresAudio: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Compatible || unknown.FailureCode != "model_incompatible_audio" {
		t.Fatalf("未知 audio 能力必须 fail-closed: %#v", unknown)
	}

	profile.Audio = Support{State: SupportSupported, Source: "probe"}
	supported, err := Negotiate(profile, ModelRequirements{RequiresAudio: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !supported.Compatible || !supported.RequestPlan.Audio {
		t.Fatalf("audio requirement 未进入 RequestPlan: %#v", supported)
	}
	if err := supported.RequestPlan.Validate(profile); err != nil {
		t.Fatalf("支持 audio 的 RequestPlan 不应无效: %v", err)
	}

	profile.Audio = Support{State: SupportUnknown}
	if err := (RequestPlan{Audio: true}).Validate(profile); err == nil {
		t.Fatal("RequestPlan 不应在 audio unknown 时手工启用 audio")
	}
}

func TestNegotiateRejectsContextUnknownForMinimum(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	result, err := Negotiate(profile, ModelRequirements{MinimumContextWindow: 4096}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Compatible || result.FailureCode != "model_context_unknown" {
		t.Fatalf("unknown context should be conservative: %#v", result)
	}
}

func TestCapabilityProfileValidationAndStableSnapshotID(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if profile.SnapshotID() == "" || profile.SnapshotID() != profile.SnapshotID() {
		t.Fatal("snapshot ID should be deterministic")
	}
	profile.ToolCalling.State = "bogus"
	if !errors.Is(profile.Validate(), ErrInvalidCapabilityProfile) {
		t.Fatal("invalid support state should be rejected")
	}
}

func TestCapabilityOverridesOnlyTightenSupport(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	degraded := SupportDegraded
	updated, err := ApplyCapabilityOverrides(profile, CapabilityOverrides{ToolCalling: &degraded})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ToolCalling.State != SupportDegraded || updated.ToolCalling.Source != "user_override" || updated.ToolCalling.Confidence != 1 {
		t.Fatalf("conservative override 未记录 provenance: %#v", updated.ToolCalling)
	}
	if updated.SnapshotID() == profile.SnapshotID() {
		t.Fatal("能力 override 应改变 profile snapshot")
	}

	supported := SupportSupported
	if _, err := ApplyCapabilityOverrides(profile, CapabilityOverrides{Images: &supported}); !errors.Is(err, ErrInvalidCapabilityOverride) {
		t.Fatalf("不应允许通过 override 声明 supported: %v", err)
	}
	unknown := SupportUnknown
	unsupported := SupportUnsupported
	unknownProfile := profile
	unknownProfile.Images = Support{State: SupportUnknown}
	if _, err := ApplyCapabilityOverrides(unknownProfile, CapabilityOverrides{Images: &supported}); !errors.Is(err, ErrInvalidCapabilityOverride) {
		t.Fatalf("unknown 不应被伪装为 supported: %v", err)
	}
	if _, err := ApplyCapabilityOverrides(unknownProfile, CapabilityOverrides{Images: &unsupported}); err != nil {
		t.Fatalf("unknown -> unsupported 应允许: %v", err)
	}
	degradedProfile := profile
	degradedProfile.Streaming = Support{State: SupportDegraded}
	if _, err := ApplyCapabilityOverrides(degradedProfile, CapabilityOverrides{Streaming: &unknown}); err != nil {
		t.Fatalf("degraded -> unknown 应允许: %v", err)
	}
	if _, err := ApplyCapabilityOverrides(degradedProfile, CapabilityOverrides{Streaming: &supported}); !errors.Is(err, ErrInvalidCapabilityOverride) {
		t.Fatalf("degraded 不应升级为 supported: %v", err)
	}
	if _, err := ApplyCapabilityOverrides(profile, CapabilityOverrides{StructuredOutputSchema: &supported}); !errors.Is(err, ErrInvalidCapabilityOverride) {
		t.Fatalf("schema override 不应伪造 supported: %v", err)
	}
}

func TestNegotiationProjectsSupportedReasoningIntoRequestPlan(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "reasoning", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	profile.ReasoningEffort = Support{State: SupportSupported, Source: "probe"}
	profile.ReasoningSummary = Support{State: SupportDegraded, Source: "probe"}
	result, err := Negotiate(profile, ModelRequirements{
		RequestedReasoningEffort: "high", RequiresReasoningSummary: true,
	}, false)
	if err != nil {
		t.Fatalf("协商推理能力失败: %v", err)
	}
	if result.RequestPlan.ReasoningEffort != "high" || !result.RequestPlan.ReasoningSummary {
		t.Fatalf("推理能力未进入请求计划: %#v", result.RequestPlan)
	}
}

func TestNegotiationOmitsUnknownReasoningWithoutPretendingSupport(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "reasoning", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	result, err := Negotiate(profile, ModelRequirements{RequestedReasoningEffort: "high", RequiresReasoningSummary: true}, false)
	if err != nil {
		t.Fatalf("unknown 推理能力不应让普通请求失败: %v", err)
	}
	if result.RequestPlan.ReasoningEffort != "" || result.RequestPlan.ReasoningSummary {
		t.Fatalf("unknown 推理能力不能被透传: %#v", result.RequestPlan)
	}
	if len(result.DisabledFeatures) != 2 {
		t.Fatalf("应记录两个被省略的推理字段: %#v", result.DisabledFeatures)
	}
}

func TestNegotiationRequiresStructuredSchemaEvidenceAndProjectsDialect(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "schema", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	unknown, err := Negotiate(profile, ModelRequirements{RequiresStructuredSchema: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Compatible || unknown.FailureCode != "model_incompatible_structured_output_schema" || unknown.RequestPlan.StructuredSchema {
		t.Fatalf("缺少 schema 证据时必须失败关闭: %#v", unknown)
	}
	profile.StructuredOutputSchema = Support{State: SupportSupported, Source: "probe", Confidence: 0.9}
	profile.JSONSchemaDialect = "openai-chat-json-schema"
	supported, err := Negotiate(profile, ModelRequirements{RequiresStructuredSchema: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !supported.Compatible || !supported.RequestPlan.StructuredSchema || supported.RequestPlan.JSONSchemaDialect != profile.JSONSchemaDialect {
		t.Fatalf("schema 证据应进入请求计划并保留 dialect: %#v", supported)
	}
	legacy := profile
	legacy.StructuredOutputSchema = Support{}
	legacy.Audio = Support{}
	if result, err := Negotiate(legacy, ModelRequirements{}, false); err != nil || result.Profile.StructuredOutputSchema.State != SupportUnknown {
		t.Fatalf("旧 profile 的 schema 字段应安全归一化: result=%#v err=%v", result, err)
	}
}

func TestRequestPlanValidationRejectsUnknownFieldsAndMismatchedSchemaDialect(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "plan", Protocol: ProtocolOpenAICompatible}, Model{ID: "model", ContextWindow: 4096, MaxOutputTokens: 256})
	if err := (RequestPlan{Images: true}).Validate(profile); !errors.Is(err, ErrInvalidRequestPlan) {
		t.Fatalf("unknown image field 不应被透传: %v", err)
	}
	profile.StructuredOutput = Support{State: SupportSupported, Source: "probe"}
	profile.StructuredOutputSchema = Support{State: SupportSupported, Source: "probe"}
	profile.JSONSchemaDialect = "openai-chat-json-schema"
	if err := (RequestPlan{StructuredOutput: true, StructuredSchema: true, JSONSchemaDialect: "gemini-json-schema-subset"}).Validate(profile); !errors.Is(err, ErrInvalidRequestPlan) {
		t.Fatalf("schema dialect 不匹配应阻止请求计划: %v", err)
	}
	if err := (RequestPlan{StructuredOutput: true, StructuredSchema: true, JSONSchemaDialect: profile.JSONSchemaDialect}).Validate(profile); err != nil {
		t.Fatalf("有效 schema request plan 不应失败: %v", err)
	}
	if err := (RequestPlan{MaxOutputTokens: 257}).Validate(profile); !errors.Is(err, ErrInvalidRequestPlan) {
		t.Fatalf("超过 provider output 上限应阻止请求计划: %v", err)
	}
}

func TestRequestPlanValidatesToolChoiceModesAndStaleSchemaDialect(t *testing.T) {
	profile := DefaultCapabilities(Provider{ID: "openai", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	profile.ToolCalling = Support{State: SupportSupported}
	profile.ToolChoiceModes = []string{"auto", "none", "any"}
	if err := (RequestPlan{ToolChoice: "none"}).Validate(profile); err != nil {
		t.Fatalf("支持的 tool_choice 不应被拒绝: %v", err)
	}
	if err := (RequestPlan{ToolChoice: "required"}).Validate(profile); err != nil {
		t.Fatalf("any/required 别名应被接受: %v", err)
	}
	if err := (RequestPlan{ToolChoice: "forced"}).Validate(profile); !errors.Is(err, ErrInvalidRequestPlan) {
		t.Fatalf("未知 tool_choice 应被拒绝: %v", err)
	}
	profile.ToolCalling = Support{State: SupportUnknown}
	if err := (RequestPlan{ToolChoice: "auto"}).Validate(profile); !errors.Is(err, ErrInvalidRequestPlan) {
		t.Fatalf("unknown tool calling 不应允许透传 tool_choice: %v", err)
	}
	if err := (RequestPlan{JSONSchemaDialect: "openai-chat-json-schema"}).Validate(profile); !errors.Is(err, ErrInvalidRequestPlan) {
		t.Fatalf("未启用 schema 时的残留 dialect 应被拒绝: %v", err)
	}
}
