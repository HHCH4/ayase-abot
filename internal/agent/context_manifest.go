package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"Abot/internal/provider"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var ErrContextBudgetExceeded = errors.New("context budget exhausted")

// ContextBudgetError is returned before a provider call when the immutable
// input plus output/safety reserves cannot fit the known model window.
type ContextBudgetError struct {
	Model         string
	ContextWindow int
	Estimated     int
	OutputReserve int
	SafetyReserve int
}

// TrimContextHistory removes only old, non-structural history entries when a
// known hard window is exceeded. Function-call/response pairs and the newest
// user message are protected; if the fixed portion still cannot fit, callers
// must fail before the provider call instead of guessing.
func TrimContextHistory(request *adkmodel.LLMRequest, manifest ContextManifest) []ContextExcludedItem {
	if request == nil || manifest.ContextWindow <= 0 {
		return nil
	}
	limit := manifest.ContextWindow - manifest.OutputReserve - manifest.SafetyReserve
	if limit <= 0 {
		return nil
	}
	contents := request.Contents
	lastUser := -1
	for index, content := range contents {
		if content != nil && strings.EqualFold(strings.TrimSpace(content.Role), genai.RoleUser) {
			lastUser = index
		}
	}
	current := manifest.EstimatedInput
	removed := make([]ContextExcludedItem, 0)
	for index, content := range contents {
		if current <= limit {
			break
		}
		if content == nil || index == lastUser || contentHasFunctionPart(content) {
			continue
		}
		current -= ScaleTokenEstimate(EstimateContentTokens(content), manifest.EstimateMultiplier)
		removed = append(removed, ContextExcludedItem{ID: fmt.Sprintf("history:%d", index), Kind: "session_history", Reason: "context_trimmed"})
		contents[index] = nil
	}
	if len(removed) == 0 {
		return nil
	}
	compacted := make([]*genai.Content, 0, len(contents)-len(removed))
	for _, content := range contents {
		if content != nil {
			compacted = append(compacted, content)
		}
	}
	request.Contents = compacted
	return removed
}

func contentHasFunctionPart(content *genai.Content) bool {
	if content == nil {
		return false
	}
	for _, part := range content.Parts {
		if part != nil && (part.FunctionCall != nil || part.FunctionResponse != nil) {
			return true
		}
	}
	return false
}

func (e *ContextBudgetError) Error() string {
	if e == nil {
		return ErrContextBudgetExceeded.Error()
	}
	return fmt.Sprintf("%v: model=%s estimated=%d output_reserve=%d safety_reserve=%d context_window=%d", ErrContextBudgetExceeded, e.Model, e.Estimated, e.OutputReserve, e.SafetyReserve, e.ContextWindow)
}

func (e *ContextBudgetError) Unwrap() error { return ErrContextBudgetExceeded }

// DynamicContextBudget implements W-O-S-F. A negative result means the fixed
// P0 context already exceeds the hard window and must not be silently trimmed.
func DynamicContextBudget(contextWindow, outputReserve, safetyReserve, fixedInput int) int {
	if contextWindow <= 0 {
		return -1
	}
	return contextWindow - outputReserve - safetyReserve - fixedInput
}

// ContextManifest is a metadata-only description of one model request. It
// deliberately stores digests and references, not the potentially sensitive
// prompt body. Runtime persists it so a user can explain where the context
// budget went after a turn or a restart.
type ContextManifest struct {
	ID                 string                `json:"id"`
	InvocationID       string                `json:"invocation_id"`
	ModelCallID        string                `json:"model_call_id"`
	BuilderVersion     string                `json:"builder_version"`
	EstimatorVersion   string                `json:"estimator_version"`
	EstimatorName      string                `json:"estimator_name,omitempty"`
	EstimateQuality    string                `json:"estimate_quality,omitempty"`
	EstimateMultiplier float64               `json:"estimate_multiplier,omitempty"`
	Model              string                `json:"model"`
	ToolSetSnapshotID  string                `json:"toolset_snapshot_id,omitempty"`
	ToolSetDigest      string                `json:"toolset_digest,omitempty"`
	ContextWindow      int                   `json:"context_window,omitempty"`
	OutputReserve      int                   `json:"output_reserve,omitempty"`
	SafetyReserve      int                   `json:"safety_reserve,omitempty"`
	EstimatedInput     int                   `json:"estimated_input"`
	ActualInput        *int                  `json:"actual_input,omitempty"`
	Included           []ContextManifestItem `json:"included,omitempty"`
	Excluded           []ContextExcludedItem `json:"excluded,omitempty"`
	Warnings           []string              `json:"warnings,omitempty"`
	Digest             string                `json:"digest"`
	CreatedAt          time.Time             `json:"created_at"`
}

type ContextManifestItem struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Priority        string `json:"priority"`
	SourceRef       string `json:"source_ref,omitempty"`
	Trust           string `json:"trust,omitempty"`
	ContentDigest   string `json:"content_digest,omitempty"`
	TokenEstimate   int    `json:"token_estimate"`
	SelectionReason string `json:"selection_reason,omitempty"`
	Freshness       string `json:"freshness,omitempty"`
}

type ContextExcludedItem struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// ContextManifestObserver is called from the ADK BeforeModel boundary after
// compaction has materialized the request. Implementations must only persist
// metadata or publish an event; they must not execute tools or side effects.
type ContextManifestObserver func(context.Context, ContextManifest)

const (
	ContextBuilderVersion   = "context-builder-v1"
	ContextEstimatorVersion = "heuristic-v1"
)

// EstimateTextTokens is a deterministic, intentionally conservative heuristic.
// It is not presented as provider billing truth; actual usage remains optional
// and is captured separately when the provider reports it.
func EstimateTextTokens(value string) int {
	if value == "" {
		return 0
	}
	if !utf8.ValidString(value) {
		return (len(value) + 3) / 4
	}
	runes := utf8.RuneCountInString(value)
	// CJK and emoji generally consume more than four bytes per token; keeping
	// this bounded avoids turning a malformed or enormous result into overflow.
	wide := 0
	for _, r := range value {
		if r >= 0x2e80 || r == '\ufe0f' {
			wide++
		}
	}
	estimate := (runes + 3) / 4
	if wide > runes/3 {
		estimate = (runes + 1) / 2
	}
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}

func EstimateContentTokens(content *genai.Content) int {
	if content == nil {
		return 0
	}
	total := 0
	maxInt := int(^uint(0) >> 1)
	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		partEstimate := 0
		if part.Text != "" {
			partEstimate = addTokenEstimate(partEstimate, EstimateTextTokens(part.Text))
		}
		if part.InlineData != nil {
			partEstimate = addTokenEstimate(partEstimate, estimateInlineDataTokens(part.InlineData))
		}
		if part.FileData != nil {
			partEstimate = addTokenEstimate(partEstimate, estimateFileDataTokens(part.FileData))
		}
		// Function parts remain structural placeholders. Their arguments/results
		// are separately represented in the tool ledger and must not make a
		// malformed JSON payload consume an unbounded context budget here.
		if part.FunctionCall != nil || part.FunctionResponse != nil {
			partEstimate = addTokenEstimate(partEstimate, 256)
		}
		if partEstimate == 0 {
			partEstimate = 256
		}
		if partEstimate > maxInt-total {
			return maxInt
		}
		total += partEstimate
	}
	return total
}

const (
	imageModalityBaseTokens     = 768
	imageModalityChunkBytes     = 16 * 1024
	imageModalityChunkTokens    = 256
	documentModalityBaseTokens  = 512
	documentModalityChunkBytes  = 1024
	documentModalityChunkTokens = 256
	unknownModalityBaseTokens   = 256
	unknownModalityChunkBytes   = 4 * 1024
	unknownModalityChunkTokens  = 128
	imageModalityMaxTokens      = 8192
	documentModalityMaxTokens   = 8192
	unknownModalityMaxTokens    = 4096
	remoteModalityTokens        = 1024
)

func addTokenEstimate(current, next int) int {
	if current < 0 {
		current = 0
	}
	if next <= 0 {
		return current
	}
	maxInt := int(^uint(0) >> 1)
	if current > maxInt-next {
		return maxInt
	}
	return current + next
}

func estimateInlineDataTokens(blob *genai.Blob) int {
	if blob == nil {
		return 0
	}
	mime := strings.ToLower(strings.TrimSpace(blob.MIMEType))
	switch {
	case strings.HasPrefix(mime, "image/"):
		return estimateModalityBytes(imageModalityBaseTokens, len(blob.Data), imageModalityChunkBytes, imageModalityChunkTokens, imageModalityMaxTokens)
	case strings.HasPrefix(mime, "text/"), mime == "application/json", mime == "application/xml", mime == "application/pdf", strings.HasSuffix(mime, "+json"), strings.HasSuffix(mime, "+xml"):
		return estimateModalityBytes(documentModalityBaseTokens, len(blob.Data), documentModalityChunkBytes, documentModalityChunkTokens, documentModalityMaxTokens)
	default:
		return estimateModalityBytes(unknownModalityBaseTokens, len(blob.Data), unknownModalityChunkBytes, unknownModalityChunkTokens, unknownModalityMaxTokens)
	}
}

// FileData identifies provider-hosted content whose byte size is unavailable
// locally. Keep a conservative fixed charge instead of pretending the URI is
// the content; an official provider count can replace this estimate later.
func estimateFileDataTokens(file *genai.FileData) int {
	if file == nil {
		return 0
	}
	return remoteModalityTokens
}

func estimateModalityBytes(base, size, chunkBytes, chunkTokens, limit int) int {
	if base < 0 {
		base = 0
	}
	if limit < base {
		return base
	}
	if size <= 0 || chunkBytes <= 0 || chunkTokens <= 0 {
		return base
	}
	chunks := size / chunkBytes
	if size%chunkBytes != 0 {
		chunks++
	}
	remaining := limit - base
	if chunks > remaining/chunkTokens {
		return limit
	}
	return base + chunks*chunkTokens
}

// ScaleTokenEstimate applies a conservative, bounded multiplier. A zero or
// sub-unit multiplier is treated as the uncalibrated estimate; callers never
// get an artificially smaller budget from usage feedback.
func ScaleTokenEstimate(value int, multiplier float64) int {
	if value <= 0 {
		return 0
	}
	if multiplier < 1 {
		multiplier = 1
	}
	if multiplier > provider.MaxTokenizerSafetyMultiplier {
		multiplier = provider.MaxTokenizerSafetyMultiplier
	}
	maxInt := int(^uint(0) >> 1)
	if multiplier <= 1 {
		return value
	}
	// Avoid converting +Inf or a value larger than int's range back to int;
	// malformed attachment/schema metadata must saturate, never wrap negative.
	if float64(value) >= float64(maxInt)/multiplier {
		return maxInt
	}
	scaled := math.Ceil(float64(value) * multiplier)
	if scaled >= float64(maxInt) {
		return maxInt
	}
	return int(scaled)
}

// EstimateContentTokensForTokenizer keeps the legacy heuristic API intact
// while allowing a model's explicitly calibrated safety multiplier to affect
// the next request. Modality bytes are only a bounded heuristic; provider
// count APIs still take precedence when available.
func EstimateContentTokensForTokenizer(content *genai.Content, tokenizer provider.TokenizerProfile) int {
	return ScaleTokenEstimate(EstimateContentTokens(content), tokenizer.Calibration.SafetyMultiplier)
}

func EstimateToolSchemaTokens(tools []*genai.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	data, err := json.Marshal(tools)
	if err != nil {
		maxInt := int(^uint(0) >> 1)
		if len(tools) > maxInt/256 {
			return maxInt
		}
		return len(tools) * 256
	}
	return EstimateTextTokens(string(data))
}

func (m ContextManifest) WithDigest() ContextManifest {
	copyManifest := m
	copyManifest.Digest = ""
	// ID/model-call timestamp and actual provider usage are observation
	// metadata, not context selection inputs. Excluding them makes the digest
	// stable for the same revisions and model profile across recovery runs.
	basis := struct {
		InvocationID       string                `json:"invocation_id"`
		BuilderVersion     string                `json:"builder_version"`
		EstimatorVersion   string                `json:"estimator_version"`
		EstimatorName      string                `json:"estimator_name,omitempty"`
		EstimateQuality    string                `json:"estimate_quality,omitempty"`
		EstimateMultiplier float64               `json:"estimate_multiplier,omitempty"`
		Model              string                `json:"model"`
		ToolSetSnapshotID  string                `json:"toolset_snapshot_id,omitempty"`
		ToolSetDigest      string                `json:"toolset_digest,omitempty"`
		ContextWindow      int                   `json:"context_window"`
		OutputReserve      int                   `json:"output_reserve"`
		SafetyReserve      int                   `json:"safety_reserve"`
		EstimatedInput     int                   `json:"estimated_input"`
		Included           []ContextManifestItem `json:"included,omitempty"`
		Excluded           []ContextExcludedItem `json:"excluded,omitempty"`
		Warnings           []string              `json:"warnings,omitempty"`
	}{copyManifest.InvocationID, copyManifest.BuilderVersion, copyManifest.EstimatorVersion, copyManifest.EstimatorName, copyManifest.EstimateQuality, copyManifest.EstimateMultiplier, copyManifest.Model, copyManifest.ToolSetSnapshotID, copyManifest.ToolSetDigest, copyManifest.ContextWindow, copyManifest.OutputReserve, copyManifest.SafetyReserve, copyManifest.EstimatedInput, copyManifest.Included, copyManifest.Excluded, copyManifest.Warnings}
	encoded, err := json.Marshal(basis)
	if err == nil {
		sum := sha256.Sum256(encoded)
		copyManifest.Digest = hex.EncodeToString(sum[:])
	}
	return copyManifest
}

func (m ContextManifest) Validate() error {
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.InvocationID) == "" || strings.TrimSpace(m.ModelCallID) == "" {
		return fmt.Errorf("context manifest 缺少 ID")
	}
	if m.EstimatedInput < 0 || m.ContextWindow < 0 || m.OutputReserve < 0 || m.SafetyReserve < 0 {
		return fmt.Errorf("context manifest token 数不能为负数")
	}
	if m.EstimateMultiplier < 0 || m.EstimateMultiplier > provider.MaxTokenizerSafetyMultiplier {
		return fmt.Errorf("context manifest estimate multiplier 超出范围")
	}
	if strings.TrimSpace(m.Digest) == "" {
		return fmt.Errorf("context manifest 缺少 digest")
	}
	return nil
}

func buildContextManifest(request *adkmodel.LLMRequest, invocationID, modelCallID string, model provider.Model, runtime RuntimeOptions, instructions []ProjectInstruction, contract *TaskContractProjection, snapshots ...*RuntimeSnapshotProjection) ContextManifest {
	var snapshot *RuntimeSnapshotProjection
	if len(snapshots) > 0 {
		snapshot = snapshots[0]
	}
	return buildContextManifestWithWorkingSet(request, invocationID, modelCallID, model, runtime, instructions, contract, snapshot, nil)
}

func buildContextManifestWithWorkingSet(request *adkmodel.LLMRequest, invocationID, modelCallID string, model provider.Model, runtime RuntimeOptions, instructions []ProjectInstruction, contract *TaskContractProjection, snapshot *RuntimeSnapshotProjection, workingSet []WorkingSetItem, toolSets ...*ToolSetSnapshot) ContextManifest {
	var toolSet *ToolSetSnapshot
	if len(toolSets) > 0 {
		toolSet = toolSets[0]
	}
	tokenizer := provider.EffectiveTokenizer(model.Capabilities)
	manifest := ContextManifest{
		ID: "manifest:" + modelCallID, InvocationID: strings.TrimSpace(invocationID), ModelCallID: modelCallID,
		BuilderVersion: ContextBuilderVersion, EstimatorVersion: tokenizer.Version, EstimatorName: tokenizer.Name, EstimateQuality: tokenizer.Quality, EstimateMultiplier: tokenizer.Calibration.SafetyMultiplier, Model: model.ID,
		ContextWindow: model.ContextWindow, OutputReserve: runtime.AIMaxOutputTokens, SafetyReserve: runtime.CompactionSafetyTokens,
		CreatedAt: time.Now().UTC(), Included: make([]ContextManifestItem, 0), Excluded: make([]ContextExcludedItem, 0),
	}
	if toolSet != nil {
		manifest.ToolSetSnapshotID = toolSet.ID
		manifest.ToolSetDigest = toolSet.Digest
	}
	if manifest.OutputReserve <= 0 {
		manifest.OutputReserve = model.MaxOutputTokens
	}
	add := func(item ContextManifestItem) {
		if item.TokenEstimate < 0 {
			item.TokenEstimate = 0
		}
		maxInt := int(^uint(0) >> 1)
		if item.TokenEstimate > maxInt-manifest.EstimatedInput {
			manifest.EstimatedInput = maxInt
		} else {
			manifest.EstimatedInput += item.TokenEstimate
		}
		manifest.Included = append(manifest.Included, item)
	}
	if request != nil && request.Config != nil && request.Config.SystemInstruction != nil {
		text := TextFromContent(request.Config.SystemInstruction)
		add(ContextManifestItem{ID: "system:instruction", Kind: "runtime_instruction", Priority: "P0", SourceRef: "kernel", Trust: "runtime", ContentDigest: digestContextText(text), TokenEstimate: EstimateContentTokensForTokenizer(request.Config.SystemInstruction, tokenizer), SelectionReason: "always_required", Freshness: "current"})
	}
	if request != nil {
		for index, content := range request.Contents {
			if content == nil {
				continue
			}
			encoded, _ := json.Marshal(content)
			role := strings.TrimSpace(content.Role)
			if role == "" {
				role = "unknown"
			}
			add(ContextManifestItem{ID: fmt.Sprintf("history:%d", index), Kind: "session_history", Priority: "P1", SourceRef: "adk_session:" + role, Trust: "session", ContentDigest: digestContextText(string(encoded)), TokenEstimate: EstimateContentTokensForTokenizer(content, tokenizer), SelectionReason: "adk_materialized", Freshness: "current"})
		}
		if request.Config != nil {
			if toolTokens := EstimateToolSchemaTokens(request.Config.Tools); toolTokens > 0 {
				encoded, _ := json.Marshal(request.Config.Tools)
				add(ContextManifestItem{ID: "tools:schema", Kind: "tool_schema", Priority: "P0", SourceRef: "runner", Trust: "runtime", ContentDigest: digestContextText(string(encoded)), TokenEstimate: ScaleTokenEstimate(toolTokens, tokenizer.Calibration.SafetyMultiplier), SelectionReason: "available_tools", Freshness: "current"})
			}
		}
	}
	if contract != nil {
		encoded, _ := json.Marshal(contract)
		add(ContextManifestItem{ID: "contract:v" + fmt.Sprint(contract.Version), Kind: "task_contract", Priority: "P0", SourceRef: "contract:" + fmt.Sprint(contract.Version), Trust: "runtime", ContentDigest: digestContextText(string(encoded)), TokenEstimate: ScaleTokenEstimate(EstimateTextTokens(string(encoded)), tokenizer.Calibration.SafetyMultiplier), SelectionReason: "always_required", Freshness: "current"})
	}
	if snapshot != nil {
		encoded, _ := json.Marshal(snapshot)
		add(ContextManifestItem{ID: "runtime_snapshot:v" + fmt.Sprint(snapshot.Revision), Kind: "runtime_snapshot", Priority: "P0", SourceRef: "runtime_snapshot:" + fmt.Sprint(snapshot.Revision), Trust: "runtime", ContentDigest: digestContextText(string(encoded)), TokenEstimate: ScaleTokenEstimate(EstimateTextTokens(string(encoded)), tokenizer.Calibration.SafetyMultiplier), SelectionReason: "always_required", Freshness: "current"})
	}
	for _, instruction := range instructions {
		add(ContextManifestItem{ID: "instruction:" + instruction.Path, Kind: "project_instruction", Priority: "P0", SourceRef: instruction.Path, Trust: "untrusted_project_data", ContentDigest: instruction.ContentDigest, TokenEstimate: ScaleTokenEstimate(EstimateTextTokens(instruction.Content), tokenizer.Calibration.SafetyMultiplier), SelectionReason: "scoped_workspace", Freshness: "current"})
	}
	for _, item := range workingSet {
		freshness := item.Freshness
		if freshness == "" {
			freshness = WorkingSetFresh
		}
		if freshness == WorkingSetStale || freshness == WorkingSetUnknown {
			manifest.Excluded = append(manifest.Excluded, ContextExcludedItem{ID: item.ID, Kind: item.Kind, Reason: freshness})
			continue
		}
		add(ContextManifestItem{ID: "working_set:" + item.ID, Kind: item.Kind, Priority: fmt.Sprintf("P%d", item.Priority), SourceRef: item.SourceRef, Trust: item.Trust, ContentDigest: item.ContentDigest, TokenEstimate: ScaleTokenEstimate(item.TokenEstimate, tokenizer.Calibration.SafetyMultiplier), SelectionReason: "working_set", Freshness: freshness})
	}
	if manifest.ContextWindow > 0 && budgetExceedsWindow(manifest.EstimatedInput, manifest.OutputReserve, manifest.SafetyReserve, manifest.ContextWindow) {
		manifest.Warnings = append(manifest.Warnings, "estimated_context_exceeds_hard_window")
		manifest.Excluded = append(manifest.Excluded, ContextExcludedItem{ID: "budget:overflow", Kind: "budget", Reason: "over_budget"})
	}
	return manifest.WithDigest()
}

func budgetExceedsWindow(input, outputReserve, safetyReserve, window int) bool {
	if window <= 0 || input > window {
		return window > 0 && input > window
	}
	remaining := window - input
	if outputReserve > remaining {
		return true
	}
	return safetyReserve > remaining-outputReserve
}

func digestContextText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
