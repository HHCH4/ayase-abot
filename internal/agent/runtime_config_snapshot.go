package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"Abot/internal/provider"
)

// RuntimeConfigSnapshotVersion identifies the bounded, metadata-only
// configuration projection persisted with a durable Invocation. Bump it
// whenever changing a field can alter the meaning of a resumed turn.
const RuntimeConfigSnapshotVersion = 1

const (
	// Config snapshots are written to the Invocation row, so keep the bound
	// deliberately small. The snapshot never contains instructions, prompts,
	// API keys, URLs, tool arguments or model output.
	MaxRuntimeConfigSnapshotBytes = 32 << 10
	maxRuntimeSnapshotIDLength    = 512
	maxRuntimeSnapshotPathLength  = 4096
	maxRuntimeSnapshotDigestLen   = len("sha256:") + sha256.Size*2
)

var (
	ErrInvalidRuntimeConfigSnapshot  = errors.New("运行时配置快照无效")
	ErrRuntimeConfigSnapshotMismatch = errors.New("运行时配置快照与当前配置不匹配")
)

// RuntimeConfigSnapshot is the immutable, non-secret projection of the
// effective Kernel configuration for one Invocation. It is intentionally
// separate from RuntimeOptions because the latter contains user-authored
// instruction/prompt text and must never be serialized here.
type RuntimeConfigSnapshot struct {
	Version                int                    `json:"version"`
	AgentDefinitionVersion string                 `json:"agent_definition_version"`
	AppName                string                 `json:"app_name"`
	ProviderID             string                 `json:"provider_id"`
	ModelID                string                 `json:"model_id"`
	ProviderProtocol       string                 `json:"provider_protocol"`
	ProviderOpenAIFormat   string                 `json:"provider_openai_format,omitempty"`
	TokenCountProtocol     string                 `json:"token_count_protocol,omitempty"`
	ModelContextWindow     int                    `json:"model_context_window,omitempty"`
	ModelMaxOutputTokens   int                    `json:"model_max_output_tokens,omitempty"`
	PersonaID              string                 `json:"persona_id"`
	InstructionDigest      string                 `json:"instruction_digest"`
	PromptPrefixDigest     string                 `json:"prompt_prefix_digest,omitempty"`
	WorkspaceID            string                 `json:"workspace_id,omitempty"`
	TargetPath             string                 `json:"target_path,omitempty"`
	ToolCatalogRevision    string                 `json:"tool_catalog_revision,omitempty"`
	Options                RuntimeOptionsSnapshot `json:"options"`
}

// RuntimeOptionsSnapshot contains only scalar policy and budget values. Text
// fields from RuntimeOptions are represented by the digests above.
type RuntimeOptionsSnapshot struct {
	AIEnabled                     bool    `json:"ai_enabled"`
	AITemperature                 float64 `json:"ai_temperature"`
	AITopP                        float64 `json:"ai_top_p"`
	AIMaxOutputTokens             int     `json:"ai_max_output_tokens"`
	AIRequestRetries              int     `json:"ai_request_retries"`
	CompactionEnabled             bool    `json:"compaction_enabled"`
	CompactionRatio               float64 `json:"compaction_ratio"`
	CompactionSafetyTokens        int     `json:"compaction_safety_tokens"`
	CompactionRetentionEvents     int     `json:"compaction_retention_events"`
	CompactionInterval            int     `json:"compaction_interval"`
	CompactionOverlap             int     `json:"compaction_overlap"`
	CompactionUnknownWindowTokens int     `json:"compaction_unknown_window_tokens"`
	AgentMaxToolCalls             int     `json:"agent_max_tool_calls"`
	ToolSchemaBudgetTokens        int     `json:"tool_schema_budget_tokens"`
	WorkspaceEnabled              bool    `json:"workspace_enabled"`
	WorkspaceReadEnabled          bool    `json:"workspace_read_enabled"`
	WorkspaceWriteEnabled         bool    `json:"workspace_write_enabled"`
	WorkspaceExecEnabled          bool    `json:"workspace_exec_enabled"`
	WorkspaceGitEnabled           bool    `json:"workspace_git_enabled"`
	WorkspaceCommandTimeoutSecs   int     `json:"workspace_command_timeout_secs"`
	MessageStreamingEnabled       bool    `json:"message_streaming_enabled"`
	MemoryEnabled                 bool    `json:"memory_enabled"`
	MemoryAutoRetrieve            bool    `json:"memory_auto_retrieve"`
	MemoryMaxResults              int     `json:"memory_max_results"`
	ModalFallbackEnabled          bool    `json:"modal_fallback_enabled"`
	ModalFallbackProviderID       string  `json:"modal_fallback_provider_id,omitempty"`
	ModalFallbackVisionModel      string  `json:"modal_fallback_vision_model,omitempty"`
	ModalFallbackAudioModel       string  `json:"modal_fallback_audio_model,omitempty"`
}

// BuildRuntimeConfigSnapshot creates a deterministic projection from the
// already normalized Kernel runtime and resolved provider/model. It does not
// copy any secret-bearing Provider fields (APIKey/BaseURL) or user text.
func BuildRuntimeConfigSnapshot(appName string, runtime RuntimeOptions, resolved provider.ResolvedModel, workspaceID, targetPath, toolCatalogRevision string) (RuntimeConfigSnapshot, string, error) {
	snapshot := RuntimeConfigSnapshot{
		Version:                RuntimeConfigSnapshotVersion,
		AgentDefinitionVersion: "kernel-runtime-v2",
		AppName:                strings.TrimSpace(appName),
		ProviderID:             strings.TrimSpace(resolved.Provider.ID),
		ModelID:                strings.TrimSpace(resolved.Model.ID),
		ProviderProtocol:       strings.TrimSpace(string(resolved.Provider.Protocol)),
		ProviderOpenAIFormat:   strings.TrimSpace(string(resolved.Provider.OpenAIFormat)),
		TokenCountProtocol:     strings.TrimSpace(string(resolved.Provider.TokenCountProtocol)),
		ModelContextWindow:     resolved.Model.ContextWindow,
		ModelMaxOutputTokens:   resolved.Model.MaxOutputTokens,
		PersonaID:              effectivePersonaID(runtime),
		InstructionDigest:      runtimeSnapshotTextDigest(runtime.Instruction),
		PromptPrefixDigest:     runtimeSnapshotOptionalTextDigest(runtime.MessagePromptPrefix),
		WorkspaceID:            strings.TrimSpace(workspaceID),
		TargetPath:             strings.TrimSpace(targetPath),
		ToolCatalogRevision:    strings.TrimSpace(toolCatalogRevision),
		Options:                runtimeOptionsSnapshot(runtime),
	}
	normalized, err := snapshot.Normalize()
	if err != nil {
		return RuntimeConfigSnapshot{}, "", err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: 无法编码", ErrInvalidRuntimeConfigSnapshot)
	}
	if len(encoded) > MaxRuntimeConfigSnapshotBytes {
		return RuntimeConfigSnapshot{}, "", fmt.Errorf("%w: 快照超过 %d 字节", ErrInvalidRuntimeConfigSnapshot, MaxRuntimeConfigSnapshotBytes)
	}
	return normalized, string(encoded), nil
}

func runtimeOptionsSnapshot(runtime RuntimeOptions) RuntimeOptionsSnapshot {
	return RuntimeOptionsSnapshot{
		AIEnabled:                     runtime.AIEnabled,
		AITemperature:                 runtime.AITemperature,
		AITopP:                        runtime.AITopP,
		AIMaxOutputTokens:             runtime.AIMaxOutputTokens,
		AIRequestRetries:              runtime.AIRequestRetries,
		CompactionEnabled:             runtime.CompactionEnabled,
		CompactionRatio:               runtime.CompactionRatio,
		CompactionSafetyTokens:        runtime.CompactionSafetyTokens,
		CompactionRetentionEvents:     runtime.CompactionRetentionEvents,
		CompactionInterval:            runtime.CompactionInterval,
		CompactionOverlap:             runtime.CompactionOverlap,
		CompactionUnknownWindowTokens: runtime.CompactionUnknownWindowTokens,
		AgentMaxToolCalls:             runtime.AgentMaxToolCalls,
		ToolSchemaBudgetTokens:        runtime.ToolSchemaBudgetTokens,
		WorkspaceEnabled:              runtime.WorkspaceEnabled,
		WorkspaceReadEnabled:          runtime.WorkspaceReadEnabled,
		WorkspaceWriteEnabled:         runtime.WorkspaceWriteEnabled,
		WorkspaceExecEnabled:          runtime.WorkspaceExecEnabled,
		WorkspaceGitEnabled:           runtime.WorkspaceGitEnabled,
		WorkspaceCommandTimeoutSecs:   runtime.WorkspaceCommandTimeoutSecs,
		MessageStreamingEnabled:       runtime.MessageStreamingEnabled,
		MemoryEnabled:                 runtime.MemoryEnabled,
		MemoryAutoRetrieve:            runtime.MemoryAutoRetrieve,
		MemoryMaxResults:              runtime.MemoryMaxResults,
		ModalFallbackEnabled:          runtime.ModalFallbackEnabled,
		ModalFallbackProviderID:       strings.TrimSpace(runtime.ModalFallbackProviderID),
		ModalFallbackVisionModel:      strings.TrimSpace(runtime.ModalFallbackVisionModel),
		ModalFallbackAudioModel:       strings.TrimSpace(runtime.ModalFallbackAudioModel),
	}
}

func runtimeSnapshotTextDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func runtimeSnapshotOptionalTextDigest(value string) string {
	if value == "" {
		return ""
	}
	return runtimeSnapshotTextDigest(value)
}

// Normalize trims and validates a snapshot without interpreting it as an
// execution instruction. It also supplies the only supported version default
// for callers constructing a value in Go; wire JSON must still contain a
// version because ParseRuntimeConfigSnapshot rejects the zero value.
func (s RuntimeConfigSnapshot) Normalize() (RuntimeConfigSnapshot, error) {
	s.AgentDefinitionVersion = strings.TrimSpace(s.AgentDefinitionVersion)
	s.AppName = strings.TrimSpace(s.AppName)
	s.ProviderID = strings.TrimSpace(s.ProviderID)
	s.ModelID = strings.TrimSpace(s.ModelID)
	s.ProviderProtocol = strings.TrimSpace(s.ProviderProtocol)
	s.ProviderOpenAIFormat = strings.TrimSpace(s.ProviderOpenAIFormat)
	s.TokenCountProtocol = strings.TrimSpace(s.TokenCountProtocol)
	s.PersonaID = strings.TrimSpace(s.PersonaID)
	s.InstructionDigest = strings.TrimSpace(s.InstructionDigest)
	s.PromptPrefixDigest = strings.TrimSpace(s.PromptPrefixDigest)
	s.WorkspaceID = strings.TrimSpace(s.WorkspaceID)
	s.TargetPath = strings.TrimSpace(s.TargetPath)
	s.ToolCatalogRevision = strings.TrimSpace(s.ToolCatalogRevision)
	if s.Version != RuntimeConfigSnapshotVersion || s.AgentDefinitionVersion == "" || s.AppName == "" || s.ProviderID == "" || s.ModelID == "" || s.ProviderProtocol == "" || s.PersonaID == "" || !validRuntimeSnapshotDigest(s.InstructionDigest) {
		return RuntimeConfigSnapshot{}, fmt.Errorf("%w: 缺少必需字段或版本不受支持", ErrInvalidRuntimeConfigSnapshot)
	}
	if s.PromptPrefixDigest != "" && !validRuntimeSnapshotDigest(s.PromptPrefixDigest) {
		return RuntimeConfigSnapshot{}, fmt.Errorf("%w: prompt prefix digest 无效", ErrInvalidRuntimeConfigSnapshot)
	}
	for name, value := range map[string]string{
		"agent_definition_version": s.AgentDefinitionVersion,
		"app_name":                 s.AppName,
		"provider_id":              s.ProviderID,
		"model_id":                 s.ModelID,
		"provider_protocol":        s.ProviderProtocol,
		"provider_openai_format":   s.ProviderOpenAIFormat,
		"token_count_protocol":     s.TokenCountProtocol,
		"persona_id":               s.PersonaID,
		"workspace_id":             s.WorkspaceID,
		"tool_catalog_revision":    s.ToolCatalogRevision,
	} {
		if len(value) > maxRuntimeSnapshotIDLength {
			return RuntimeConfigSnapshot{}, fmt.Errorf("%w: %s 超出长度限制", ErrInvalidRuntimeConfigSnapshot, name)
		}
	}
	if len(s.TargetPath) > maxRuntimeSnapshotPathLength {
		return RuntimeConfigSnapshot{}, fmt.Errorf("%w: target_path 超出长度限制", ErrInvalidRuntimeConfigSnapshot)
	}
	if s.ModelContextWindow < 0 || s.ModelMaxOutputTokens < 0 {
		return RuntimeConfigSnapshot{}, fmt.Errorf("%w: model budget 不能为负数", ErrInvalidRuntimeConfigSnapshot)
	}
	if err := validateRuntimeOptionsSnapshot(s.Options); err != nil {
		return RuntimeConfigSnapshot{}, err
	}
	return s, nil
}

func validRuntimeSnapshotDigest(value string) bool {
	if len(value) != maxRuntimeSnapshotDigestLen || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validateRuntimeOptionsSnapshot(options RuntimeOptionsSnapshot) error {
	for name, value := range map[string]float64{"ai_temperature": options.AITemperature, "ai_top_p": options.AITopP, "compaction_ratio": options.CompactionRatio} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%w: %s 不是有限数值", ErrInvalidRuntimeConfigSnapshot, name)
		}
	}
	for name, value := range map[string]int{
		"ai_max_output_tokens":             options.AIMaxOutputTokens,
		"ai_request_retries":               options.AIRequestRetries,
		"compaction_safety_tokens":         options.CompactionSafetyTokens,
		"compaction_retention_events":      options.CompactionRetentionEvents,
		"compaction_interval":              options.CompactionInterval,
		"compaction_overlap":               options.CompactionOverlap,
		"compaction_unknown_window_tokens": options.CompactionUnknownWindowTokens,
		"agent_max_tool_calls":             options.AgentMaxToolCalls,
		"tool_schema_budget_tokens":        options.ToolSchemaBudgetTokens,
		"workspace_command_timeout_secs":   options.WorkspaceCommandTimeoutSecs,
		"memory_max_results":               options.MemoryMaxResults,
	} {
		if value < 0 {
			return fmt.Errorf("%w: %s 不能为负数", ErrInvalidRuntimeConfigSnapshot, name)
		}
	}
	if options.CompactionInterval == 0 && options.CompactionOverlap != 0 {
		return fmt.Errorf("%w: compaction overlap 不能脱离 interval", ErrInvalidRuntimeConfigSnapshot)
	}
	if options.CompactionOverlap > options.CompactionInterval {
		return fmt.Errorf("%w: compaction overlap 不能大于 interval", ErrInvalidRuntimeConfigSnapshot)
	}
	for name, value := range map[string]string{
		"modal_fallback_provider_id":  options.ModalFallbackProviderID,
		"modal_fallback_vision_model": options.ModalFallbackVisionModel,
		"modal_fallback_audio_model":  options.ModalFallbackAudioModel,
	} {
		if len(value) > 128 {
			return fmt.Errorf("%w: %s 超出长度限制", ErrInvalidRuntimeConfigSnapshot, name)
		}
	}
	return nil
}

// ParseRuntimeConfigSnapshot accepts only one bounded JSON document and
// rejects unknown fields/trailing documents so future callers cannot smuggle
// execution directives into the persisted contract.
func ParseRuntimeConfigSnapshot(encoded string) (RuntimeConfigSnapshot, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" || len(encoded) > MaxRuntimeConfigSnapshotBytes {
		return RuntimeConfigSnapshot{}, fmt.Errorf("%w: JSON 为空或超过 %d 字节", ErrInvalidRuntimeConfigSnapshot, MaxRuntimeConfigSnapshotBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	decoder.DisallowUnknownFields()
	var snapshot RuntimeConfigSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return RuntimeConfigSnapshot{}, fmt.Errorf("%w: JSON 无效", ErrInvalidRuntimeConfigSnapshot)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return RuntimeConfigSnapshot{}, fmt.Errorf("%w: 不允许尾部 JSON 文档", ErrInvalidRuntimeConfigSnapshot)
	}
	normalized, err := snapshot.Normalize()
	if err != nil {
		return RuntimeConfigSnapshot{}, err
	}
	return normalized, nil
}

// RuntimeConfigSnapshotDigest returns a stable metadata-only digest suitable
// for API/events. Invalid input returns an empty string rather than hashing an
// untrusted or oversized payload.
func RuntimeConfigSnapshotDigest(encoded string) string {
	snapshot, err := ParseRuntimeConfigSnapshot(encoded)
	if err != nil {
		return ""
	}
	canonical, err := json.Marshal(snapshot)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// ValidateRuntimeConfigSnapshot compares canonical projections and emits only
// their digests in an error. The original snapshot is never copied into an
// error or event.
func ValidateRuntimeConfigSnapshot(stored string, current RuntimeConfigSnapshot) error {
	parsed, err := ParseRuntimeConfigSnapshot(stored)
	if err != nil {
		return err
	}
	normalized, err := current.Normalize()
	if err != nil {
		return err
	}
	left, marshalErr := json.Marshal(parsed)
	if marshalErr != nil {
		return fmt.Errorf("%w: 无法规范化已保存快照", ErrInvalidRuntimeConfigSnapshot)
	}
	right, marshalErr := json.Marshal(normalized)
	if marshalErr != nil {
		return fmt.Errorf("%w: 无法规范化当前快照", ErrInvalidRuntimeConfigSnapshot)
	}
	if bytes.Equal(left, right) {
		return nil
	}
	return fmt.Errorf("%w: stored=%s current=%s", ErrRuntimeConfigSnapshotMismatch, RuntimeConfigSnapshotDigest(string(left)), RuntimeConfigSnapshotDigest(string(right)))
}
