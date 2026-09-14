package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	defaultCapabilityProbeTimeout = 15 * time.Second
	maxCapabilityProbeTimeout     = 30 * time.Second
	defaultCapabilityProbeOutput  = 32
	maxCapabilityProbeOutput      = 128
)

var (
	probeBearerPattern      = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	probeSecretFieldPattern = regexp.MustCompile(`(?i)["']?(api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret|authorization|private[_-]?key)["']?\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;}]+)`)
)

// RunCapabilityProbe executes a bounded, side-effect-free probe against an
// already constructed ADK model. It never registers a tool with ADK: the
// function declaration is sent directly to the model and any returned call is
// only observed, never executed. Existing user overrides remain authoritative
// when a probe appears to find a more permissive capability.
func RunCapabilityProbe(ctx context.Context, llm adkmodel.LLM, profile ModelCapabilityProfile, options CapabilityProbeOptions) (CapabilityProbeResult, error) {
	if llm == nil {
		return CapabilityProbeResult{}, errors.New("模型能力探测需要非空模型")
	}
	if err := profile.Validate(); err != nil {
		return CapabilityProbeResult{}, err
	}
	options = normalizeCapabilityProbeOptions(options)
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	started := time.Now().UTC()
	result := CapabilityProbeResult{Profile: profile, StartedAt: started}
	route := strings.TrimSpace(profile.Route)

	observe := func(feature string, state SupportState, confidence float64, reason string) {
		now := time.Now().UTC()
		result.Observations = append(result.Observations, CapabilityObservation{
			Feature: feature, State: state, Source: "probe", Confidence: confidence,
			Route: route, Reason: reason, ObservedAt: now,
		})
		applyProbeSupport(&result.Profile, feature, state, confidence, reason, now)
	}

	baseRequest := probeTextRequest(profile.ModelID, options.MaxOutputTokens)
	baseResponses, err := collectProbeResponses(probeCtx, llm, baseRequest, false)
	if err != nil {
		result.CompletedAt = time.Now().UTC()
		result.Message = "基础文本探测失败"
		return result, fmt.Errorf("模型能力基础探测失败: %s", safeProbeError(err))
	}
	observe("text_generation", SupportSupported, 0.95, "non-streaming text response received")
	if responseUsage(baseResponses) {
		result.Profile.UsageDetails = appendUnique(result.Profile.UsageDetails, "prompt_tokens", "output_tokens", "total_tokens")
		observe("usage", SupportSupported, 0.8, "usage metadata returned")
	}

	if options.IncludeStreaming {
		responses, streamErr := collectProbeResponses(probeCtx, llm, baseRequest, true)
		switch {
		case streamErr == nil && len(responses) > 0:
			observe("streaming", SupportSupported, 0.95, "stream response received")
		case streamErr != nil && capabilityUnsupportedError(streamErr):
			observe("streaming", SupportUnsupported, 0.9, "provider explicitly rejected streaming")
		case streamErr != nil:
			observe("streaming", SupportUnknown, 0, "probe inconclusive: "+safeProbeError(streamErr))
		default:
			observe("streaming", SupportUnknown, 0, "probe returned no stream response")
		}
	}

	if options.IncludeToolCalling {
		request := probeToolRequest(profile.ModelID, options.MaxOutputTokens)
		responses, toolErr := collectProbeResponses(probeCtx, llm, request, false)
		switch {
		case toolErr == nil && responseContainsFunctionCall(responses):
			observe("tool_calling", SupportSupported, 0.95, "model returned a function call for a no-op declaration")
		case toolErr != nil && capabilityUnsupportedError(toolErr):
			observe("tool_calling", SupportUnsupported, 0.9, "provider explicitly rejected function tools")
		case toolErr != nil:
			observe("tool_calling", SupportUnknown, 0, "probe inconclusive: "+safeProbeError(toolErr))
		default:
			observe("tool_calling", SupportUnknown, 0, "model did not return a function call")
		}
	}

	if options.IncludeStructuredJSON {
		request := probeJSONRequest(profile.ModelID, options.MaxOutputTokens)
		responses, jsonErr := collectProbeResponses(probeCtx, llm, request, false)
		text := firstProbeText(responses)
		switch {
		case jsonErr != nil && capabilityUnsupportedError(jsonErr):
			observe("structured_output", SupportUnsupported, 0.9, "provider explicitly rejected JSON response mode")
		case jsonErr != nil:
			observe("structured_output", SupportUnknown, 0, "probe inconclusive: "+safeProbeError(jsonErr))
		case validJSON(text):
			observe("structured_output", SupportSupported, 0.95, "response was valid JSON")
		case strings.TrimSpace(text) != "":
			observe("structured_output", SupportDegraded, 0.6, "response mode succeeded but returned invalid JSON")
		default:
			observe("structured_output", SupportUnknown, 0, "probe returned no text")
		}
	}

	if options.IncludeStructuredSchema {
		request := probeStructuredSchemaRequest(profile.ModelID, options.MaxOutputTokens)
		responses, schemaErr := collectProbeResponses(probeCtx, llm, request, false)
		text := firstProbeText(responses)
		switch {
		case schemaErr != nil && capabilityUnsupportedError(schemaErr):
			observe("structured_output_schema", SupportUnsupported, 0.9, "provider explicitly rejected the bounded JSON Schema subset")
		case schemaErr != nil:
			observe("structured_output_schema", SupportUnknown, 0, "probe inconclusive: "+safeProbeError(schemaErr))
		case validStructuredSchemaResponse(text):
			observe("structured_output_schema", SupportSupported, 0.9, "bounded JSON Schema response was valid")
		case strings.TrimSpace(text) != "":
			observe("structured_output_schema", SupportDegraded, 0.5, "schema request succeeded but response did not match the bounded shape")
		default:
			observe("structured_output_schema", SupportUnknown, 0, "probe returned no text")
		}
	}

	if options.IncludeReasoning {
		request := probeReasoningRequest(profile.ModelID, options.MaxOutputTokens)
		responses, reasoningErr := collectProbeResponses(probeCtx, llm, request, false)
		switch {
		case reasoningErr != nil && capabilityUnsupportedError(reasoningErr):
			observe("reasoning_effort", SupportUnsupported, 0.9, "provider explicitly rejected reasoning configuration")
		case reasoningErr != nil:
			observe("reasoning_effort", SupportUnknown, 0, "probe inconclusive: "+safeProbeError(reasoningErr))
		case responseContainsThoughts(responses):
			observe("reasoning_effort", SupportSupported, 0.8, "reasoning metadata or thought part was returned")
		default:
			observe("reasoning_effort", SupportUnknown, 0, "reasoning request succeeded without observable thought metadata")
		}
	}

	if options.IncludeImages {
		request := probeImageRequest(profile.ModelID, options.MaxOutputTokens)
		responses, imageErr := collectProbeResponses(probeCtx, llm, request, false)
		switch {
		case imageErr != nil && capabilityUnsupportedError(imageErr):
			observe("images", SupportUnsupported, 0.9, "provider explicitly rejected image input")
		case imageErr != nil:
			observe("images", SupportUnknown, 0, "probe inconclusive: "+safeProbeError(imageErr))
		case len(responses) > 0:
			observe("images", SupportSupported, 0.8, "small image input was accepted")
		default:
			observe("images", SupportUnknown, 0, "probe returned no image response")
		}
	}

	if options.IncludeInputFiles {
		request := probeInputFileRequest(profile.ModelID, options.MaxOutputTokens)
		responses, fileErr := collectProbeResponses(probeCtx, llm, request, false)
		switch {
		case fileErr != nil && capabilityUnsupportedError(fileErr):
			observe("input_files", SupportUnsupported, 0.9, "provider explicitly rejected file input")
		case fileErr != nil:
			observe("input_files", SupportUnknown, 0, "probe inconclusive: "+safeProbeError(fileErr))
		case len(responses) > 0:
			observe("input_files", SupportSupported, 0.75, "small text/PDF file input was accepted")
		default:
			observe("input_files", SupportUnknown, 0, "probe returned no file response")
		}
	}

	result.CompletedAt = time.Now().UTC()
	result.Profile.SourceRevision = probeSourceRevision(result.Profile.SourceRevision, result.CompletedAt)
	result.Profile.UpdatedAt = result.CompletedAt
	result.Message = fmt.Sprintf("能力探测完成，记录 %d 项 metadata-only 证据", len(result.Observations))
	return result, nil
}

func normalizeCapabilityProbeOptions(options CapabilityProbeOptions) CapabilityProbeOptions {
	if options.Timeout <= 0 {
		options.Timeout = defaultCapabilityProbeTimeout
	}
	if options.Timeout > maxCapabilityProbeTimeout {
		options.Timeout = maxCapabilityProbeTimeout
	}
	if options.MaxOutputTokens <= 0 {
		options.MaxOutputTokens = defaultCapabilityProbeOutput
	}
	if options.MaxOutputTokens > maxCapabilityProbeOutput {
		options.MaxOutputTokens = maxCapabilityProbeOutput
	}
	if !options.ExplicitOptionalSelection && !options.IncludeStreaming && !options.IncludeToolCalling && !options.IncludeStructuredJSON {
		options.IncludeStreaming = true
		options.IncludeToolCalling = true
		options.IncludeStructuredJSON = true
	}
	return options
}

// TokenCountProbeOutcome classifies an optional provider-side token-count
// probe. Endpoint absence is an explicit unsupported result; transport,
// timeout, rate-limit and other transient errors remain unknown so they do
// not erase a previously trusted tokenizer or force a generation failure.
func TokenCountProbeOutcome(err error) (SupportState, float64, string) {
	if err == nil {
		return SupportSupported, 0.95, "provider accepted the bounded token-count probe"
	}
	if errors.Is(err, ErrTokenCounterUnavailable) {
		return SupportUnsupported, 0.9, "provider protocol does not expose a token-count endpoint"
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	for _, marker := range []string{"http 404", "http 405", "http 501", "not found", "method not allowed", "not implemented", "unsupported", "not support", "unknown endpoint", "no route"} {
		if strings.Contains(message, marker) {
			return SupportUnsupported, 0.9, "provider explicitly rejected the token-count endpoint"
		}
	}
	return SupportUnknown, 0, "token-count probe inconclusive: " + safeProbeError(err)
}

// TokenizerProfileFromTokenCount converts successful provider metadata into
// the persisted tokenizer description used by Context Manifest. It does not
// persist the counted prompt or the returned integer; the integer is only
// used for the current bounded probe's capability evidence.
func TokenizerProfileFromTokenCount(count TokenCount) TokenizerProfile {
	profile := DefaultTokenizerProfile()
	if value := strings.TrimSpace(count.Name); value != "" {
		profile.Name = value
	}
	if value := strings.TrimSpace(count.Version); value != "" {
		profile.Version = value
	}
	if value := strings.TrimSpace(count.Source); value != "" {
		profile.Source = value
	} else {
		profile.Source = "provider"
	}
	quality := strings.TrimSpace(count.Quality)
	if quality == "" {
		quality = "provider"
	}
	if _, ok := validTokenizerQualities[quality]; !ok {
		quality = "provider"
	}
	profile.Quality = quality
	profile.Known = count.Exact
	if count.Exact {
		profile.Confidence = 0.95
	}
	profile.Calibration = TokenCalibration{SafetyMultiplier: 1, Source: profile.Source}
	return profile
}

// ApplyTokenCountProbe appends a metadata-only token-count observation and,
// on a validated exact result, updates the result tokenizer profile. It is
// shared by built-in adapters so OpenAI-compatible gateways and native
// providers have identical downgrade semantics.
func ApplyTokenCountProbe(result *CapabilityProbeResult, route string, count TokenCount, err error) {
	if result == nil {
		return
	}
	if err == nil && (!count.Exact || count.Tokens < 0 || strings.TrimSpace(count.Name) == "" || strings.TrimSpace(count.Version) == "") {
		err = fmt.Errorf("%w: provider token-count probe returned incomplete metadata", ErrInvalidRequest)
	}
	state, confidence, reason := TokenCountProbeOutcome(err)
	now := time.Now().UTC()
	result.Observations = append(result.Observations, CapabilityObservation{
		Feature: "token_count", State: state, Source: "probe", Confidence: confidence,
		Route: strings.TrimSpace(route), Reason: reason, ObservedAt: now,
	})
	if err == nil && state == SupportSupported {
		result.Profile.Tokenizer = TokenizerProfileFromTokenCount(count)
	}
	result.CompletedAt = now
	result.Profile.SourceRevision = probeSourceRevision(result.Profile.SourceRevision, now)
	result.Profile.UpdatedAt = now
	result.Message = fmt.Sprintf("能力探测完成，记录 %d 项 metadata-only 证据", len(result.Observations))
}

func probeTextRequest(modelID string, maxOutput int) *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{Model: strings.TrimSpace(modelID), Config: &genai.GenerateContentConfig{MaxOutputTokens: int32(maxOutput)}, Contents: []*genai.Content{genai.NewContentFromText("Reply with OK.", genai.RoleUser)}}
}

func probeToolRequest(modelID string, maxOutput int) *adkmodel.LLMRequest {
	request := probeTextRequest(modelID, maxOutput)
	request.Contents = []*genai.Content{genai.NewContentFromText("Call the declared no-op function exactly once; do not describe it.", genai.RoleUser)}
	request.Config.Tools = []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "abot_capability_probe_noop", Description: "No-op capability probe. Never execute.", Parameters: &genai.Schema{Type: genai.TypeObject}}}}}
	request.Config.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"abot_capability_probe_noop"}}}
	return request
}

func probeJSONRequest(modelID string, maxOutput int) *adkmodel.LLMRequest {
	request := probeTextRequest(modelID, maxOutput)
	request.Contents = []*genai.Content{genai.NewContentFromText("Return a single valid JSON object: {}", genai.RoleUser)}
	request.Config.ResponseMIMEType = "application/json"
	return request
}

func probeStructuredSchemaRequest(modelID string, maxOutput int) *adkmodel.LLMRequest {
	request := probeTextRequest(modelID, maxOutput)
	request.Contents = []*genai.Content{genai.NewContentFromText("Return JSON matching the requested schema with ok=true.", genai.RoleUser)}
	request.Config.ResponseMIMEType = "application/json"
	request.Config.ResponseJsonSchema = map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
		"required":             []string{"ok"},
		"additionalProperties": false,
	}
	return request
}

func probeReasoningRequest(modelID string, maxOutput int) *adkmodel.LLMRequest {
	request := probeTextRequest(modelID, maxOutput)
	request.Contents = []*genai.Content{genai.NewContentFromText("Answer with OK and use the lowest available reasoning effort.", genai.RoleUser)}
	request.Config.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow, IncludeThoughts: true}
	return request
}

func probeImageRequest(modelID string, maxOutput int) *adkmodel.LLMRequest {
	request := probeTextRequest(modelID, maxOutput)
	imageData, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	request.Contents = []*genai.Content{genai.NewContentFromParts([]*genai.Part{
		{Text: "Describe the supplied one-pixel image in one word."},
		{InlineData: &genai.Blob{MIMEType: "image/png", Data: imageData}},
	}, genai.RoleUser)}
	return request
}

func probeInputFileRequest(modelID string, maxOutput int) *adkmodel.LLMRequest {
	request := probeTextRequest(modelID, maxOutput)
	request.Contents = []*genai.Content{genai.NewContentFromParts([]*genai.Part{
		{Text: "Summarize the supplied file in one word."},
		{InlineData: &genai.Blob{MIMEType: "application/pdf", DisplayName: "probe.pdf", Data: []byte("%PDF-1.4\n%%EOF\n")}},
	}, genai.RoleUser)}
	return request
}

func collectProbeResponses(ctx context.Context, llm adkmodel.LLM, request *adkmodel.LLMRequest, stream bool) ([]*adkmodel.LLMResponse, error) {
	responses := make([]*adkmodel.LLMResponse, 0, 2)
	for response, err := range llm.GenerateContent(ctx, request, stream) {
		if err != nil {
			return responses, err
		}
		if response != nil {
			responses = append(responses, response)
		}
	}
	if len(responses) == 0 {
		return responses, errors.New("provider returned no response")
	}
	return responses, nil
}

func responseContainsFunctionCall(responses []*adkmodel.LLMResponse) bool {
	for _, response := range responses {
		if response == nil || response.Content == nil {
			continue
		}
		for _, part := range response.Content.Parts {
			if part != nil && part.FunctionCall != nil {
				return true
			}
		}
	}
	return false
}

func responseUsage(responses []*adkmodel.LLMResponse) bool {
	for _, response := range responses {
		if response != nil && response.UsageMetadata != nil {
			return true
		}
	}
	return false
}

func firstProbeText(responses []*adkmodel.LLMResponse) string {
	var builder strings.Builder
	for _, response := range responses {
		if response == nil || response.Content == nil {
			continue
		}
		for _, part := range response.Content.Parts {
			if part != nil && part.Text != "" {
				builder.WriteString(part.Text)
			}
		}
	}
	return strings.TrimSpace(builder.String())
}

func validStructuredSchemaResponse(value string) bool {
	var payload struct {
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &payload); err != nil {
		return false
	}
	return payload.OK != nil && *payload.OK
}

func responseContainsThoughts(responses []*adkmodel.LLMResponse) bool {
	for _, response := range responses {
		if response == nil {
			continue
		}
		if response.UsageMetadata != nil && response.UsageMetadata.ThoughtsTokenCount > 0 {
			return true
		}
		if response.Content == nil {
			continue
		}
		for _, part := range response.Content.Parts {
			if part != nil && part.Thought {
				return true
			}
		}
	}
	return false
}

func validJSON(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	var decoded any
	return json.Unmarshal([]byte(value), &decoded) == nil
}

func capabilityUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"http 408", "http 429", "http 500", "http 502", "http 503", "http 504", "timeout", "timed out", "rate limit", "temporarily", "connection reset", "network"} {
		if strings.Contains(message, marker) {
			return false
		}
	}
	for _, marker := range []string{"unsupported", "not support", "unknown field", "unrecognized field", "invalid tool", "tool_choice", "response_format", "response mimetype", "responsejsonschema", "response_json_schema", "thinkingconfig", "input_image", "input_file", "parallel_tool_calls"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func safeProbeError(err error) string {
	if err == nil {
		return "unknown"
	}
	message := strings.TrimSpace(err.Error())
	message = probeBearerPattern.ReplaceAllString(message, "Bearer [redacted]")
	message = probeSecretFieldPattern.ReplaceAllString(message, "$1=[redacted]")
	if len(message) > 240 {
		message = message[:240]
		for len(message) > 0 && !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func applyProbeSupport(profile *ModelCapabilityProfile, feature string, state SupportState, confidence float64, reason string, observedAt time.Time) {
	if profile == nil || state == SupportUnknown {
		return
	}
	if feature == "structured_output_schema" {
		// JSON Schema support is tracked separately from the broader
		// structured_output switch. The probe only covers a bounded subset and
		// never claims that every provider schema keyword is accepted.
		target := &profile.StructuredOutputSchema
		if target.Source == "user_override" {
			return
		}
		*target = Support{State: state, Source: "probe", Confidence: confidence, ObservedAt: observedAt, Reason: reason}
		if state == SupportSupported || state == SupportDegraded {
			profile.JSONSchemaDialect = probeSchemaDialect(profile.Route)
			if profile.StructuredOutput.State == SupportUnknown && profile.StructuredOutput.Source != "user_override" {
				profile.StructuredOutput = *target
			}
		} else if state == SupportUnsupported {
			// A fresh explicit rejection must not leave a dialect from an older
			// route/probe that would make a later request look supported.
			profile.JSONSchemaDialect = ""
		}
		return
	}
	target := supportField(profile, feature)
	if target == nil {
		return
	}
	// A conservative user override is authoritative for the effective profile.
	// Keep both its state and provenance intact; a later probe must not make a
	// manual safety decision look like provider evidence.
	if target.Source == "user_override" {
		return
	}
	*target = Support{State: state, Source: "probe", Confidence: confidence, ObservedAt: observedAt, Reason: reason}
}

func probeSchemaDialect(route string) string {
	switch strings.ToLower(strings.TrimSpace(route)) {
	case "chat":
		return "openai-chat-json-schema"
	case "responses":
		return "openai-responses-json-schema"
	case "gemini":
		return "gemini-json-schema-subset"
	default:
		return "json-schema-subset"
	}
}

func supportField(profile *ModelCapabilityProfile, feature string) *Support {
	switch feature {
	case "tool_calling":
		return &profile.ToolCalling
	case "parallel_tool_calls":
		return &profile.ParallelToolCalls
	case "structured_output":
		return &profile.StructuredOutput
	case "structured_output_schema":
		return &profile.StructuredOutputSchema
	case "streaming":
		return &profile.Streaming
	case "images":
		return &profile.Images
	case "input_files":
		return &profile.InputFiles
	case "audio":
		return &profile.Audio
	case "reasoning_effort":
		return &profile.ReasoningEffort
	case "reasoning_summary":
		return &profile.ReasoningSummary
	case "prompt_caching":
		return &profile.PromptCaching
	case "native_compaction":
		return &profile.NativeCompaction
	default:
		return nil
	}
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

func probeSourceRevision(previous string, completedAt time.Time) string {
	previous = strings.TrimSpace(previous)
	if previous == "" {
		previous = "catalog"
	}
	if index := strings.Index(previous, ":probe:"); index >= 0 {
		previous = previous[:index]
	}
	return previous + ":probe:" + completedAt.UTC().Format("20060102T150405.000000000Z")
}
