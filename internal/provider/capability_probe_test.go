package provider

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestRunCapabilityProbeCollectsMetadataOnlyEvidence(t *testing.T) {
	model := &probeTestLLM{}
	profile := DefaultCapabilities(Provider{ID: "probe", Protocol: ProtocolOpenAICompatible}, Model{ID: "demo"})
	profile.Route = "chat"
	profile.ToolCalling = Support{State: SupportUnknown, Source: "catalog"}
	profile.Streaming = Support{State: SupportUnknown, Source: "catalog"}
	profile.StructuredOutput = Support{State: SupportUnknown, Source: "catalog"}

	result, err := RunCapabilityProbe(context.Background(), model, profile, CapabilityProbeOptions{MaxOutputTokens: 7})
	if err != nil {
		t.Fatalf("能力探测失败: %v", err)
	}
	if result.Profile.ToolCalling.State != SupportSupported || result.Profile.Streaming.State != SupportSupported || result.Profile.StructuredOutput.State != SupportSupported {
		t.Fatalf("探测后的能力状态不正确: %#v", result.Profile)
	}
	if result.Profile.UsageDetails == nil || len(result.Profile.UsageDetails) != 3 {
		t.Fatalf("usage 能力没有去重记录: %#v", result.Profile.UsageDetails)
	}
	if len(result.Observations) != 5 {
		t.Fatalf("观察数量 = %d，期望基础文本、usage、stream、tool、json 共 5 项", len(result.Observations))
	}
	if result.Profile.SourceRevision == profile.SourceRevision || !strings.Contains(result.Profile.SourceRevision, ":probe:") {
		t.Fatalf("探测没有生成新的 source revision: %q", result.Profile.SourceRevision)
	}
	for _, observation := range result.Observations {
		if observation.Source != "probe" || observation.Route != "chat" || observation.ObservedAt.IsZero() {
			t.Fatalf("观察缺少稳定的来源元数据: %#v", observation)
		}
		if strings.Contains(observation.Reason, "Reply with OK") || strings.Contains(observation.Reason, "abot_capability_probe_noop") {
			t.Fatalf("观察不应包含探测正文或工具名称: %#v", observation)
		}
	}
	model.mu.Lock()
	calls := append([]probeCall(nil), model.calls...)
	model.mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("LLM 调用次数 = %d，期望基础、stream、tool、json 四次", len(calls))
	}
	for _, call := range calls {
		if call.maxOutput != 7 {
			t.Fatalf("探测请求 MaxOutputTokens = %d，期望 7", call.maxOutput)
		}
	}
	if calls[2].toolName != "abot_capability_probe_noop" {
		t.Fatalf("工具探测没有使用固定 no-op 声明: %#v", calls[2])
	}
}

func TestRunCapabilityProbeKeepsTransientUnknownAndUserOverride(t *testing.T) {
	model := &probeTestLLM{
		streamErr: errors.New("upstream timeout"),
		toolErr:   errors.New("unsupported function tools"),
		jsonErr:   errors.New("temporary network failure"),
	}
	profile := DefaultCapabilities(Provider{ID: "probe", Protocol: ProtocolOpenAICompatible}, Model{ID: "demo"})
	profile.StructuredOutput = Support{State: SupportDegraded, Source: "user_override", Confidence: 1}

	result, err := RunCapabilityProbe(context.Background(), model, profile, CapabilityProbeOptions{})
	if err != nil {
		t.Fatalf("可选能力探测不应因单项失败而失败: %v", err)
	}
	if result.Profile.Streaming.State != SupportSupported || result.Profile.Streaming.Source != "openai_adapter" {
		t.Fatalf("瞬时 stream 错误不应覆盖已有静态证据: %#v", result.Profile.Streaming)
	}
	if result.Profile.ToolCalling.State != SupportUnsupported || result.Profile.ToolCalling.Source != "probe" {
		t.Fatalf("明确不支持应收紧 tool calling: %#v", result.Profile.ToolCalling)
	}
	if result.Profile.StructuredOutput.State != SupportDegraded || result.Profile.StructuredOutput.Source != "user_override" {
		t.Fatalf("user override 不应被探测覆盖: %#v", result.Profile.StructuredOutput)
	}
	if observation := findProbeObservation(result, "streaming"); observation.State != SupportUnknown {
		t.Fatalf("瞬时 stream 错误应记录 unknown 证据: %#v", observation)
	}
	if observation := findProbeObservation(result, "tool_calling"); observation.State != SupportUnsupported {
		t.Fatalf("明确 tool 错误应记录 unsupported 证据: %#v", observation)
	}
	if observation := findProbeObservation(result, "structured_output"); observation.State != SupportUnknown {
		t.Fatalf("瞬时 JSON 错误应记录 unknown 证据: %#v", observation)
	}
}

func TestRunCapabilityProbeBaseFailureDoesNotClaimCapabilities(t *testing.T) {
	model := &probeTestLLM{baseErr: errors.New("connection reset")}
	profile := DefaultCapabilities(Provider{ID: "probe", Protocol: ProtocolGemini}, Model{ID: "demo"})
	result, err := RunCapabilityProbe(context.Background(), model, profile, CapabilityProbeOptions{})
	if err == nil || !strings.Contains(err.Error(), "基础探测") {
		t.Fatalf("基础探测失败应返回可诊断错误，得到 %v", err)
	}
	if result.Profile.SourceRevision != profile.SourceRevision || len(result.Observations) != 0 {
		t.Fatalf("基础失败不应写入能力证据: %#v", result)
	}
}

func TestRunCapabilityProbeBoundsOptions(t *testing.T) {
	got := normalizeCapabilityProbeOptions(CapabilityProbeOptions{Timeout: 2 * time.Minute, MaxOutputTokens: 1000})
	if got.Timeout != maxCapabilityProbeTimeout || got.MaxOutputTokens != maxCapabilityProbeOutput {
		t.Fatalf("探测边界未收紧: %#v", got)
	}
	if !got.IncludeStreaming || !got.IncludeToolCalling || !got.IncludeStructuredJSON {
		t.Fatalf("默认探测应包含可选能力: %#v", got)
	}
	explicit := normalizeCapabilityProbeOptions(CapabilityProbeOptions{ExplicitOptionalSelection: true})
	if explicit.IncludeStreaming || explicit.IncludeToolCalling || explicit.IncludeStructuredJSON {
		t.Fatalf("显式禁用时不应重新启用可选探测: %#v", explicit)
	}
}

func TestTokenCountProbeOutcomeKeepsTransientErrorsUnknown(t *testing.T) {
	state, confidence, reason := TokenCountProbeOutcome(nil)
	if state != SupportSupported || confidence <= 0 || !strings.Contains(reason, "accepted") {
		t.Fatalf("成功 token-count probe 结果不正确: state=%s confidence=%v reason=%q", state, confidence, reason)
	}
	state, confidence, reason = TokenCountProbeOutcome(errors.New("OpenAI token count 返回 HTTP 404: route not found"))
	if state != SupportUnsupported || confidence <= 0 || !strings.Contains(reason, "rejected") {
		t.Fatalf("缺失 endpoint 应为 unsupported: state=%s confidence=%v reason=%q", state, confidence, reason)
	}
	state, confidence, reason = TokenCountProbeOutcome(errors.New("请求 token count 失败: upstream timeout"))
	if state != SupportUnknown || confidence != 0 || !strings.Contains(reason, "inconclusive") {
		t.Fatalf("瞬时错误应为 unknown: state=%s confidence=%v reason=%q", state, confidence, reason)
	}
}

func TestApplyTokenCountProbeStoresOnlyTokenizerMetadata(t *testing.T) {
	result := CapabilityProbeResult{Profile: DefaultCapabilities(Provider{ID: "gateway", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})}
	ApplyTokenCountProbe(&result, "responses", TokenCount{
		Tokens: 23, Name: "gateway-counter", Version: "v1", Source: "provider", Quality: "provider", Exact: true,
	}, nil)
	if !result.Profile.Tokenizer.Known || result.Profile.Tokenizer.Name != "gateway-counter" || result.Profile.Tokenizer.Confidence <= 0 {
		t.Fatalf("成功 probe 未写入 tokenizer metadata: %#v", result.Profile.Tokenizer)
	}
	if len(result.Observations) != 1 || result.Observations[0].Feature != "token_count" || result.Observations[0].State != SupportSupported {
		t.Fatalf("token-count observation 不正确: %#v", result.Observations)
	}
	if strings.Contains(result.Observations[0].Reason, "23") || strings.Contains(result.Observations[0].Reason, "Count this") {
		t.Fatalf("observation 不应保存请求/计数正文: %#v", result.Observations[0])
	}

	unknown := CapabilityProbeResult{Profile: DefaultCapabilities(Provider{ID: "gateway", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})}
	ApplyTokenCountProbe(&unknown, "responses", TokenCount{}, errors.New("upstream timeout"))
	if unknown.Profile.Tokenizer.Known || unknown.Observations[0].State != SupportUnknown {
		t.Fatalf("瞬时失败不应伪造 tokenizer: %#v", unknown)
	}
}

func TestRunCapabilityProbeOptionalSchemaReasoningAndMultimodal(t *testing.T) {
	model := &probeTestLLM{}
	profile := DefaultCapabilities(Provider{ID: "probe", Protocol: ProtocolOpenAICompatible}, Model{ID: "demo"})
	profile.Route = "chat"
	profile.Images = Support{State: SupportUnknown, Source: "catalog"}
	profile.InputFiles = Support{State: SupportUnknown, Source: "catalog"}
	profile.ReasoningEffort = Support{State: SupportUnknown, Source: "catalog"}

	result, err := RunCapabilityProbe(context.Background(), model, profile, CapabilityProbeOptions{
		ExplicitOptionalSelection: true,
		IncludeStructuredSchema:   true,
		IncludeReasoning:          true,
		IncludeImages:             true,
		IncludeInputFiles:         true,
		MaxOutputTokens:           9,
	})
	if err != nil {
		t.Fatalf("可选细分能力探测失败: %v", err)
	}
	if result.Profile.Images.State != SupportSupported || result.Profile.InputFiles.State != SupportSupported || result.Profile.ReasoningEffort.State != SupportSupported {
		t.Fatalf("多模态/推理能力状态不正确: %#v", result.Profile)
	}
	if result.Profile.StructuredOutputSchema.State != SupportSupported {
		t.Fatalf("JSON Schema 能力状态不正确: %#v", result.Profile.StructuredOutputSchema)
	}
	if observation := findProbeObservation(result, "structured_output_schema"); observation.State != SupportSupported {
		t.Fatalf("JSON Schema 证据不正确: %#v", observation)
	}
	if result.Profile.JSONSchemaDialect != "openai-chat-json-schema" {
		t.Fatalf("JSON Schema dialect 未绑定当前线路: %q", result.Profile.JSONSchemaDialect)
	}
	if len(result.Observations) != 6 {
		t.Fatalf("观察数量=%d，期望基础文本、usage、schema、reasoning、images、files 共 6 项", len(result.Observations))
	}
	model.mu.Lock()
	calls := append([]probeCall(nil), model.calls...)
	model.mu.Unlock()
	if len(calls) != 5 {
		t.Fatalf("LLM 调用次数=%d，期望基础、schema、reasoning、images、files 五次", len(calls))
	}
	seen := map[string]bool{}
	for _, call := range calls {
		if call.maxOutput != 9 {
			t.Fatalf("可选探测 MaxOutputTokens=%d，期望 9", call.maxOutput)
		}
		if call.schemaMode {
			seen["schema"] = true
		}
		if call.reasoning {
			seen["reasoning"] = true
		}
		if call.image {
			seen["image"] = true
		}
		if call.file {
			seen["file"] = true
		}
	}
	for _, key := range []string{"schema", "reasoning", "image", "file"} {
		if !seen[key] {
			t.Fatalf("没有观察到 %s 探针请求: %#v", key, calls)
		}
	}
}

func TestRunCapabilityProbeAudioUsesBoundedWAVInput(t *testing.T) {
	model := &probeTestLLM{}
	profile := DefaultCapabilities(Provider{ID: "probe", Protocol: ProtocolOpenAICompatible}, Model{ID: "audio-model"})
	profile.Audio = Support{State: SupportUnknown, Source: "catalog"}
	result, err := RunCapabilityProbe(context.Background(), model, profile, CapabilityProbeOptions{
		ExplicitOptionalSelection: true,
		IncludeAudio:              true,
	})
	if err != nil {
		t.Fatalf("audio 能力探测失败: %v", err)
	}
	if result.Profile.Audio.State != SupportSupported || findProbeObservation(result, "audio").State != SupportSupported {
		t.Fatalf("audio 能力证据错误: profile=%#v observations=%#v", result.Profile.Audio, result.Observations)
	}
	model.mu.Lock()
	calls := append([]probeCall(nil), model.calls...)
	model.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("audio probe 调用次数=%d，期望基础文本+audio 两次", len(calls))
	}
	audioRequest := probeAudioRequest(profile.ModelID, 16)
	part := audioRequest.Contents[0].Parts[1]
	if part.InlineData == nil || part.InlineData.MIMEType != "audio/wav" || len(part.InlineData.Data) < 12 || string(part.InlineData.Data[:4]) != "RIFF" {
		t.Fatalf("audio probe 载荷不是受限 WAV: %#v", part.InlineData)
	}
}

func TestRunCapabilityProbeMapsProviderSubsetRejectionsWithoutDowngradingTransientErrors(t *testing.T) {
	model := &probeTestLLM{
		schemaErr: errors.New("HTTP 400: unsupported response_format json_schema"),
		imageErr:  errors.New("provider does not support image input"),
		fileErr:   errors.New("HTTP 429: rate limit exceeded"),
	}
	profile := DefaultCapabilities(Provider{ID: "probe", Protocol: ProtocolOpenAICompatible}, Model{ID: "demo"})
	profile.Route = "responses"
	result, err := RunCapabilityProbe(context.Background(), model, profile, CapabilityProbeOptions{
		ExplicitOptionalSelection: true,
		IncludeStructuredSchema:   true,
		IncludeImages:             true,
		IncludeInputFiles:         true,
	})
	if err != nil {
		t.Fatalf("provider 子集拒绝不应使整组 probe 失败: %v", err)
	}
	if result.Profile.StructuredOutputSchema.State != SupportUnsupported || result.Profile.Images.State != SupportUnsupported {
		t.Fatalf("明确 provider 子集拒绝应标记 unsupported: %#v", result.Profile)
	}
	if result.Profile.InputFiles.State != SupportUnknown {
		t.Fatalf("限流属于瞬时错误，文件能力应保持 unknown: %#v", result.Profile.InputFiles)
	}
	if result.Profile.JSONSchemaDialect != "" {
		t.Fatalf("schema 拒绝后不应保留旧 dialect: %q", result.Profile.JSONSchemaDialect)
	}
	if findProbeObservation(result, "structured_output_schema").State != SupportUnsupported || findProbeObservation(result, "images").State != SupportUnsupported || findProbeObservation(result, "input_files").State != SupportUnknown {
		t.Fatalf("provider 子集 observation 状态不正确: %#v", result.Observations)
	}
}

func TestSafeProbeErrorRedactsCredentials(t *testing.T) {
	message := safeProbeError(errors.New(`HTTP 401: {"api_key":"secret-value","authorization":"Bearer top-secret"}`))
	if strings.Contains(message, "secret-value") || strings.Contains(message, "top-secret") || strings.Contains(message, "Bearer top-secret") {
		t.Fatalf("探测错误泄露凭据: %q", message)
	}
}

func TestCapabilityUnsupportedErrorKeepsTransientFailuresUnknown(t *testing.T) {
	if capabilityUnsupportedError(errors.New(`HTTP 429: temporarily unsupported due to rate limit`)) {
		t.Fatal("429/rate-limit 失败不应被视为永久 unsupported")
	}
	if capabilityUnsupportedError(errors.New(`HTTP 400: field response_format is unsupported`)) == false {
		t.Fatal("明确的 400 字段拒绝应视为 unsupported")
	}
	for _, message := range []string{
		"HTTP 400: responseJsonSchema is not supported",
		"HTTP 400: input_image is not supported",
		"HTTP 400: input_file is not supported",
		"HTTP 400: parallel_tool_calls is not supported",
	} {
		if !capabilityUnsupportedError(errors.New(message)) {
			t.Fatalf("provider-specific field rejection should be unsupported: %s", message)
		}
	}
}

func TestProbeSourceRevisionDoesNotGrowOnRepeatedRuns(t *testing.T) {
	first := probeSourceRevision("catalog", time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	second := probeSourceRevision(first, time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC))
	if strings.Count(second, ":probe:") != 1 || !strings.HasSuffix(second, "20260102T000000.000000000Z") {
		t.Fatalf("重复探测 source revision 不应无限增长: %q", second)
	}
}

func findProbeObservation(result CapabilityProbeResult, feature string) CapabilityObservation {
	for _, observation := range result.Observations {
		if observation.Feature == feature {
			return observation
		}
	}
	return CapabilityObservation{}
}

type probeCall struct {
	stream     bool
	toolName   string
	jsonMode   bool
	schemaMode bool
	reasoning  bool
	image      bool
	file       bool
	maxOutput  int
}

type probeTestLLM struct {
	mu           sync.Mutex
	calls        []probeCall
	started      chan struct{}
	release      chan struct{}
	startOnce    sync.Once
	baseErr      error
	streamErr    error
	toolErr      error
	jsonErr      error
	schemaErr    error
	reasoningErr error
	imageErr     error
	fileErr      error
}

func (m *probeTestLLM) Name() string { return "probe-test" }

func (m *probeTestLLM) GenerateContent(ctx context.Context, request *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		call := probeCall{stream: stream}
		if request != nil && request.Config != nil {
			call.maxOutput = int(request.Config.MaxOutputTokens)
			call.jsonMode = request.Config.ResponseMIMEType == "application/json"
			call.schemaMode = request.Config.ResponseJsonSchema != nil
			call.reasoning = request.Config.ThinkingConfig != nil
			for _, tool := range request.Config.Tools {
				for _, declaration := range tool.FunctionDeclarations {
					if declaration != nil {
						call.toolName = declaration.Name
					}
				}
			}
		}
		if request != nil {
			for _, content := range request.Contents {
				if content == nil {
					continue
				}
				for _, part := range content.Parts {
					if part == nil || part.InlineData == nil {
						continue
					}
					if strings.HasPrefix(strings.ToLower(part.InlineData.MIMEType), "image/") {
						call.image = true
					} else if part.InlineData.MIMEType != "" {
						call.file = true
					}
				}
			}
		}
		m.mu.Lock()
		m.calls = append(m.calls, call)
		m.mu.Unlock()
		if !stream && call.toolName == "" && !call.jsonMode && m.started != nil {
			m.startOnce.Do(func() { close(m.started) })
			<-m.release
		}

		if stream && m.streamErr != nil {
			yield(nil, m.streamErr)
			return
		}
		if call.toolName != "" && m.toolErr != nil {
			yield(nil, m.toolErr)
			return
		}
		if call.jsonMode && m.jsonErr != nil {
			yield(nil, m.jsonErr)
			return
		}
		if call.schemaMode && m.schemaErr != nil {
			yield(nil, m.schemaErr)
			return
		}
		if call.reasoning && m.reasoningErr != nil {
			yield(nil, m.reasoningErr)
			return
		}
		if call.image && m.imageErr != nil {
			yield(nil, m.imageErr)
			return
		}
		if call.file && m.fileErr != nil {
			yield(nil, m.fileErr)
			return
		}
		if !stream && call.toolName == "" && !call.jsonMode && m.baseErr != nil {
			yield(nil, m.baseErr)
			return
		}
		if call.toolName != "" {
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{Name: call.toolName, ID: "probe-call"}}}, genai.RoleModel)}, nil)
			return
		}
		text := "OK"
		if call.schemaMode {
			text = `{"ok":true}`
		} else if call.jsonMode {
			text = "{}"
		}
		if call.reasoning {
			yield(&adkmodel.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{{Thought: true}, genai.NewPartFromText(text)}, genai.RoleModel), UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2, ThoughtsTokenCount: 1}}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel), UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2}}, nil)
	}
}
