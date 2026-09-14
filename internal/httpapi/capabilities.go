package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"Abot/internal/provider"
)

// capabilityOverridePayload mirrors provider.CapabilityOverrides while keeping
// the HTTP boundary explicit. A pointer distinguishes “leave unchanged” from
// a requested conservative state.
type capabilityOverridePayload struct {
	ToolCalling            *provider.SupportState `json:"tool_calling,omitempty"`
	ParallelToolCalls      *provider.SupportState `json:"parallel_tool_calls,omitempty"`
	StructuredOutput       *provider.SupportState `json:"structured_output,omitempty"`
	StructuredOutputSchema *provider.SupportState `json:"structured_output_schema,omitempty"`
	Streaming              *provider.SupportState `json:"streaming,omitempty"`
	Images                 *provider.SupportState `json:"images,omitempty"`
	InputFiles             *provider.SupportState `json:"input_files,omitempty"`
	Audio                  *provider.SupportState `json:"audio,omitempty"`
	ReasoningEffort        *provider.SupportState `json:"reasoning_effort,omitempty"`
	ReasoningSummary       *provider.SupportState `json:"reasoning_summary,omitempty"`
	PromptCaching          *provider.SupportState `json:"prompt_caching,omitempty"`
	NativeCompaction       *provider.SupportState `json:"native_compaction,omitempty"`
}

// capabilityProbePayload exposes only bounded, boolean probe switches. The
// server intentionally keeps the timeout internal so a client cannot turn a
// management probe into an unbounded upstream request.
type capabilityProbePayload struct {
	IncludeStreaming        *bool `json:"include_streaming"`
	IncludeToolCalling      *bool `json:"include_tool_calling"`
	IncludeStructuredJSON   *bool `json:"include_structured_json"`
	IncludeTokenCount       *bool `json:"include_token_count"`
	IncludeStructuredSchema *bool `json:"include_structured_schema"`
	IncludeReasoning        *bool `json:"include_reasoning"`
	IncludeImages           *bool `json:"include_images"`
	IncludeInputFiles       *bool `json:"include_input_files"`
	MaxOutputTokens         int   `json:"max_output_tokens"`
}

func (p capabilityProbePayload) domain() provider.CapabilityProbeOptions {
	options := provider.CapabilityProbeOptions{
		IncludeStreaming:          true,
		IncludeToolCalling:        true,
		IncludeStructuredJSON:     true,
		MaxOutputTokens:           p.MaxOutputTokens,
		ExplicitOptionalSelection: true,
	}
	if p.IncludeStreaming != nil {
		options.IncludeStreaming = *p.IncludeStreaming
	}
	if p.IncludeToolCalling != nil {
		options.IncludeToolCalling = *p.IncludeToolCalling
	}
	if p.IncludeStructuredJSON != nil {
		options.IncludeStructuredJSON = *p.IncludeStructuredJSON
	}
	if p.IncludeTokenCount != nil {
		options.IncludeTokenCount = *p.IncludeTokenCount
	}
	if p.IncludeStructuredSchema != nil {
		options.IncludeStructuredSchema = *p.IncludeStructuredSchema
	}
	if p.IncludeReasoning != nil {
		options.IncludeReasoning = *p.IncludeReasoning
	}
	if p.IncludeImages != nil {
		options.IncludeImages = *p.IncludeImages
	}
	if p.IncludeInputFiles != nil {
		options.IncludeInputFiles = *p.IncludeInputFiles
	}
	return options
}

func (p capabilityOverridePayload) domain() provider.CapabilityOverrides {
	return provider.CapabilityOverrides{
		ToolCalling: p.ToolCalling, ParallelToolCalls: p.ParallelToolCalls,
		StructuredOutput: p.StructuredOutput, StructuredOutputSchema: p.StructuredOutputSchema, Streaming: p.Streaming,
		Images: p.Images, InputFiles: p.InputFiles, Audio: p.Audio,
		ReasoningEffort: p.ReasoningEffort, ReasoningSummary: p.ReasoningSummary,
		PromptCaching: p.PromptCaching, NativeCompaction: p.NativeCompaction,
	}
}

func (s *Server) updateProviderModelCapabilityOverrides(writer http.ResponseWriter, request *http.Request) {
	var payload capabilityOverridePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, err)
		return
	}
	model, err := s.providers.ApplyCapabilityOverrides(request.Context(), request.PathValue("id"), strings.TrimSpace(request.PathValue("model")), payload.domain())
	if err != nil {
		writeError(writer, err)
		return
	}
	// Return only the effective profile. Provider API keys and other provider
	// configuration never cross this boundary.
	if model.Capabilities == nil {
		writeError(writer, provider.ErrInvalidRequest)
		return
	}
	writeJSON(writer, http.StatusOK, model.Capabilities)
}

func (s *Server) probeProviderModelCapabilities(writer http.ResponseWriter, request *http.Request) {
	var payload capabilityProbePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	result, err := s.providers.ProbeCapabilities(request.Context(), request.PathValue("id"), strings.TrimSpace(request.PathValue("model")), payload.domain())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) getProviderModelCapabilityObservations(writer http.ResponseWriter, request *http.Request) {
	limit := 0
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			writeError(writer, fmt.Errorf("limit 无效"))
			return
		}
		limit = value
	}
	items, err := s.providers.ListCapabilityObservations(request.Context(), request.PathValue("id"), request.PathValue("model"), limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"observations": items})
}
