package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// ToolSource identifies who owns a tool contract. External sources are kept
// as reserved values so they can be added without changing the snapshot shape.
type ToolSource string

const (
	ToolSourceBuiltin   ToolSource = "builtin"
	ToolSourceWorkspace ToolSource = "workspace"
	ToolSourceMemory    ToolSource = "memory"
	ToolSourceSkill     ToolSource = "skill"
	ToolSourceMCP       ToolSource = "mcp"
	ToolSourcePlugin    ToolSource = "plugin"
	ToolSourceRemote    ToolSource = "remote"
)

type ToolCapability string

const (
	ToolCapabilityReadWorkspace       ToolCapability = "read_workspace"
	ToolCapabilityWriteWorkspace      ToolCapability = "write_workspace"
	ToolCapabilityExecuteProcess      ToolCapability = "execute_process"
	ToolCapabilityInteractiveTerminal ToolCapability = "interactive_terminal"
	ToolCapabilityReadMemory          ToolCapability = "read_memory"
	ToolCapabilityWriteMemory         ToolCapability = "write_memory"
	ToolCapabilityNetwork             ToolCapability = "network"
	ToolCapabilityExternalSideEffect  ToolCapability = "external_side_effect"
	ToolCapabilityDestructive         ToolCapability = "destructive"
	ToolCapabilityLongRunning         ToolCapability = "long_running"
	ToolCapabilityStreamingResult     ToolCapability = "streaming_result"
)

type ToolRiskLevel string

const (
	ToolRiskLow      ToolRiskLevel = "low"
	ToolRiskMedium   ToolRiskLevel = "medium"
	ToolRiskHigh     ToolRiskLevel = "high"
	ToolRiskCritical ToolRiskLevel = "critical"
)

type ToolPolicy string

const (
	ToolPolicyAllow ToolPolicy = "allow"
	ToolPolicyAsk   ToolPolicy = "ask"
	ToolPolicyDeny  ToolPolicy = "deny"
)

type ToolStatus string

const (
	ToolStatusReady       ToolStatus = "ready"
	ToolStatusDegraded    ToolStatus = "degraded"
	ToolStatusUnavailable ToolStatus = "unavailable"
	ToolStatusDisabled    ToolStatus = "disabled"
)

type ToolRiskProfile struct {
	Level          ToolRiskLevel `json:"level"`
	DefaultPolicy  ToolPolicy    `json:"default_policy"`
	Reversible     bool          `json:"reversible"`
	Idempotent     bool          `json:"idempotent"`
	DataExposure   string        `json:"data_exposure,omitempty"`
	SideEffectKind string        `json:"side_effect_kind,omitempty"`
}

type ToolLimits struct {
	TimeoutSeconds        int `json:"timeout_seconds,omitempty"`
	MaxInputBytes         int `json:"max_input_bytes,omitempty"`
	MaxInlineResultBytes  int `json:"max_inline_result_bytes,omitempty"`
	MaxArtifactBytes      int `json:"max_artifact_bytes,omitempty"`
	MaxCallsPerInvocation int `json:"max_calls_per_invocation,omitempty"`
	MaxConcurrency        int `json:"max_concurrency,omitempty"`
	MaxRetries            int `json:"max_retries,omitempty"`
}

type ToolExecutionProfile struct {
	LongRunning      bool     `json:"long_running"`
	StreamingResult  bool     `json:"streaming_result"`
	RequiresApproval bool     `json:"requires_approval"`
	ConcurrencyClass string   `json:"concurrency_class,omitempty"`
	Phases           []string `json:"phases,omitempty"`
}

type ToolAvailability struct {
	Status    ToolStatus `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	CheckedAt time.Time  `json:"checked_at,omitempty"`
}

// ToolDescriptor is the stable, provider-neutral contract exposed to the
// Registry, policy and Context Engine. It contains no credentials or bound
// workspace paths.
type ToolDescriptor struct {
	ID           string               `json:"id"`
	ModelName    string               `json:"model_name"`
	Version      string               `json:"version"`
	Source       ToolSource           `json:"source"`
	Description  string               `json:"description,omitempty"`
	InputSchema  json.RawMessage      `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage      `json:"output_schema,omitempty"`
	SchemaDigest string               `json:"schema_digest"`
	Capabilities []ToolCapability     `json:"capabilities,omitempty"`
	Risk         ToolRiskProfile      `json:"risk"`
	Limits       ToolLimits           `json:"limits"`
	Execution    ToolExecutionProfile `json:"execution"`
	Availability ToolAvailability     `json:"availability"`
	Public       bool                 `json:"public"`
}

type ToolRef struct {
	ID           string `json:"id"`
	ModelName    string `json:"model_name"`
	Version      string `json:"version"`
	SchemaDigest string `json:"schema_digest"`
}

type ToolSelectionExclusion struct {
	ID        string `json:"id"`
	ModelName string `json:"model_name,omitempty"`
	Reason    string `json:"reason"`
}

// ToolSetSnapshot is immutable per Invocation. Re-execution must resolve the
// same descriptor versions; a different digest is a migration/error, not an
// opportunity to silently substitute a same-named tool.
type ToolSetSnapshot struct {
	ID              string                   `json:"id"`
	InvocationID    string                   `json:"invocation_id"`
	CatalogRevision string                   `json:"catalog_revision"`
	PolicyRevision  string                   `json:"policy_revision,omitempty"`
	ModelProfile    string                   `json:"model_profile,omitempty"`
	Tools           []ToolRef                `json:"tools"`
	Excluded        []ToolSelectionExclusion `json:"excluded,omitempty"`
	Digest          string                   `json:"digest"`
	CreatedAt       time.Time                `json:"created_at"`
}

var (
	ErrInvalidToolDescriptor   = errors.New("工具描述无效")
	ErrToolConflict            = errors.New("工具注册冲突")
	ErrToolNotFound            = errors.New("工具不存在")
	ErrRequiredToolUnavailable = errors.New("必需工具不可用")
	ErrToolSnapshotConflict    = errors.New("ToolSet Snapshot 已固定，不能静默改变")
)

var (
	toolIDPattern        = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,127}$`)
	toolModelNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]{0,127}$`)
	semverPattern        = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
)

var knownToolCapabilities = map[ToolCapability]struct{}{
	ToolCapabilityReadWorkspace: {}, ToolCapabilityWriteWorkspace: {}, ToolCapabilityExecuteProcess: {},
	ToolCapabilityInteractiveTerminal: {}, ToolCapabilityReadMemory: {}, ToolCapabilityWriteMemory: {},
	ToolCapabilityNetwork: {}, ToolCapabilityExternalSideEffect: {}, ToolCapabilityDestructive: {},
	ToolCapabilityLongRunning: {}, ToolCapabilityStreamingResult: {},
}

func (d ToolDescriptor) Validate() error {
	if !toolIDPattern.MatchString(strings.TrimSpace(d.ID)) {
		return fmt.Errorf("%w: id 必须匹配稳定工具 ID 规则", ErrInvalidToolDescriptor)
	}
	if !toolModelNamePattern.MatchString(strings.TrimSpace(d.ModelName)) {
		return fmt.Errorf("%w: model_name 无效", ErrInvalidToolDescriptor)
	}
	if !semverPattern.MatchString(strings.TrimSpace(d.Version)) {
		return fmt.Errorf("%w: version 必须是 semver", ErrInvalidToolDescriptor)
	}
	switch d.Source {
	case ToolSourceBuiltin, ToolSourceWorkspace, ToolSourceMemory, ToolSourceSkill, ToolSourceMCP, ToolSourcePlugin, ToolSourceRemote:
	default:
		return fmt.Errorf("%w: source %q 不受支持", ErrInvalidToolDescriptor, d.Source)
	}
	if len(d.Description) > 16*1024 {
		return fmt.Errorf("%w: description 过长", ErrInvalidToolDescriptor)
	}
	if d.Risk.Level != "" {
		switch d.Risk.Level {
		case ToolRiskLow, ToolRiskMedium, ToolRiskHigh, ToolRiskCritical:
		default:
			return fmt.Errorf("%w: risk level 不受支持", ErrInvalidToolDescriptor)
		}
	}
	if d.Risk.DefaultPolicy != "" && d.Risk.DefaultPolicy != ToolPolicyAllow && d.Risk.DefaultPolicy != ToolPolicyAsk && d.Risk.DefaultPolicy != ToolPolicyDeny {
		return fmt.Errorf("%w: default policy 不受支持", ErrInvalidToolDescriptor)
	}
	seenCaps := make(map[ToolCapability]struct{}, len(d.Capabilities))
	for _, capability := range d.Capabilities {
		if _, ok := knownToolCapabilities[capability]; !ok {
			return fmt.Errorf("%w: capability %q 不受支持", ErrInvalidToolDescriptor, capability)
		}
		if _, duplicate := seenCaps[capability]; duplicate {
			return fmt.Errorf("%w: capability %q 重复", ErrInvalidToolDescriptor, capability)
		}
		seenCaps[capability] = struct{}{}
	}
	for _, value := range []struct {
		name  string
		value int
	}{{"timeout_seconds", d.Limits.TimeoutSeconds}, {"max_input_bytes", d.Limits.MaxInputBytes}, {"max_inline_result_bytes", d.Limits.MaxInlineResultBytes}, {"max_artifact_bytes", d.Limits.MaxArtifactBytes}, {"max_calls_per_invocation", d.Limits.MaxCallsPerInvocation}, {"max_concurrency", d.Limits.MaxConcurrency}, {"max_retries", d.Limits.MaxRetries}} {
		if value.value < 0 {
			return fmt.Errorf("%w: %s 不能为负数", ErrInvalidToolDescriptor, value.name)
		}
	}
	if len(d.Execution.Phases) > 0 {
		seen := make(map[string]struct{}, len(d.Execution.Phases))
		for _, phase := range d.Execution.Phases {
			phase = strings.TrimSpace(phase)
			if phase == "" {
				return fmt.Errorf("%w: execution phase 不能为空", ErrInvalidToolDescriptor)
			}
			if _, ok := seen[phase]; ok {
				return fmt.Errorf("%w: execution phase %q 重复", ErrInvalidToolDescriptor, phase)
			}
			seen[phase] = struct{}{}
		}
	}
	if d.Availability.Status != "" {
		switch d.Availability.Status {
		case ToolStatusReady, ToolStatusDegraded, ToolStatusUnavailable, ToolStatusDisabled:
		default:
			return fmt.Errorf("%w: availability status 不受支持", ErrInvalidToolDescriptor)
		}
	}
	for _, schema := range []struct {
		name string
		data json.RawMessage
	}{{"input_schema", d.InputSchema}, {"output_schema", d.OutputSchema}} {
		if len(schema.data) == 0 {
			continue
		}
		if len(schema.data) > 256*1024 {
			return fmt.Errorf("%w: %s 过大", ErrInvalidToolDescriptor, schema.name)
		}
		var value any
		if err := json.Unmarshal(schema.data, &value); err != nil {
			return fmt.Errorf("%w: %s 不是合法 JSON: %v", ErrInvalidToolDescriptor, schema.name, err)
		}
		if value == nil {
			return fmt.Errorf("%w: %s 不能为 null", ErrInvalidToolDescriptor, schema.name)
		}
	}
	if strings.TrimSpace(d.SchemaDigest) == "" {
		return fmt.Errorf("%w: 缺少 schema_digest", ErrInvalidToolDescriptor)
	}
	return nil
}

func normalizeToolDescriptor(d ToolDescriptor) (ToolDescriptor, error) {
	d.ID = strings.TrimSpace(d.ID)
	d.ModelName = strings.TrimSpace(d.ModelName)
	d.Version = strings.TrimSpace(d.Version)
	if d.Version == "" {
		d.Version = "1.0.0"
	}
	d.Source = ToolSource(strings.ToLower(strings.TrimSpace(string(d.Source))))
	if d.Risk.Level == "" {
		d.Risk.Level = ToolRiskLow
	}
	if d.Risk.DefaultPolicy == "" {
		d.Risk.DefaultPolicy = ToolPolicyAllow
	}
	if d.Availability.Status == "" {
		d.Availability.Status = ToolStatusReady
	}
	d.Description = strings.TrimSpace(d.Description)
	d.Capabilities = uniqueToolCapabilities(d.Capabilities)
	d.Execution.Phases = normalizeStrings(d.Execution.Phases)
	var err error
	d.InputSchema, err = canonicalJSON(d.InputSchema)
	if err != nil {
		return ToolDescriptor{}, fmt.Errorf("%w: input schema: %v", ErrInvalidToolDescriptor, err)
	}
	d.OutputSchema, err = canonicalJSON(d.OutputSchema)
	if err != nil {
		return ToolDescriptor{}, fmt.Errorf("%w: output schema: %v", ErrInvalidToolDescriptor, err)
	}
	d.SchemaDigest = schemaDigest(d.InputSchema, d.OutputSchema)
	if err := d.Validate(); err != nil {
		return ToolDescriptor{}, err
	}
	return d, nil
}

func canonicalJSON(data json.RawMessage) (json.RawMessage, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func schemaDigest(input, output json.RawMessage) string {
	encoded, _ := json.Marshal(struct {
		Input  json.RawMessage `json:"input,omitempty"`
		Output json.RawMessage `json:"output,omitempty"`
	}{input, output})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (s ToolSetSnapshot) Validate() error {
	if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.InvocationID) == "" {
		return fmt.Errorf("%w: snapshot 缺少 id/invocation_id", ErrInvalidToolDescriptor)
	}
	if strings.TrimSpace(s.CatalogRevision) == "" || strings.TrimSpace(s.Digest) == "" {
		return fmt.Errorf("%w: snapshot 缺少 catalog_revision/digest", ErrInvalidToolDescriptor)
	}
	seen := make(map[string]struct{}, len(s.Tools))
	for _, ref := range s.Tools {
		if !toolIDPattern.MatchString(ref.ID) || !toolModelNamePattern.MatchString(ref.ModelName) || !semverPattern.MatchString(ref.Version) || strings.TrimSpace(ref.SchemaDigest) == "" {
			return fmt.Errorf("%w: snapshot tool ref 无效", ErrInvalidToolDescriptor)
		}
		key := ref.ID + "@" + ref.Version
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%w: snapshot tool ref 重复", ErrInvalidToolDescriptor)
		}
		seen[key] = struct{}{}
	}
	return nil
}

type registeredTool struct {
	descriptor ToolDescriptor
	tool       adktool.Tool
}

// ToolRegistry is an in-process catalog. Runtime snapshots are the durable
// boundary; registry updates affect only future invocations.
type ToolRegistry struct {
	mu      sync.RWMutex
	entries map[string]registeredTool
	byName  map[string]string
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{entries: make(map[string]registeredTool), byName: make(map[string]string)}
}

func (r *ToolRegistry) Register(descriptor ToolDescriptor, implementation adktool.Tool) (ToolDescriptor, error) {
	if r == nil {
		return ToolDescriptor{}, errors.New("工具 Registry 不能为空")
	}
	if implementation == nil {
		return ToolDescriptor{}, fmt.Errorf("%w: 工具实现不能为空", ErrInvalidToolDescriptor)
	}
	normalized, err := normalizeToolDescriptor(descriptor)
	if err != nil {
		return ToolDescriptor{}, err
	}
	key := normalized.ID + "@" + normalized.Version
	r.mu.Lock()
	defer r.mu.Unlock()
	if existingKey, ok := r.byName[normalized.ModelName]; ok && existingKey != key {
		return ToolDescriptor{}, fmt.Errorf("%w: model_name %q 已由 %s 占用", ErrToolConflict, normalized.ModelName, existingKey)
	}
	if existing, ok := r.entries[key]; ok {
		if existing.descriptor.SchemaDigest != normalized.SchemaDigest {
			return ToolDescriptor{}, fmt.Errorf("%w: %s/version %s 的 schema digest 变化", ErrToolConflict, normalized.ID, normalized.Version)
		}
		if existing.descriptor.Source == ToolSourceBuiltin && normalized.Source != ToolSourceBuiltin {
			return ToolDescriptor{}, fmt.Errorf("%w: 外部来源不能覆盖 builtin 工具 %q", ErrToolConflict, normalized.ID)
		}
	}
	r.entries[key] = registeredTool{descriptor: normalized, tool: implementation}
	r.byName[normalized.ModelName] = key
	return normalized, nil
}

// RegisterRuntimeTool derives a descriptor from an ADK tool. A functiontool
// declaration is used when available; raw tools still receive an explicit
// empty-schema descriptor and can be overridden with Register for richer
// policy metadata.
func (r *ToolRegistry) RegisterRuntimeTool(implementation adktool.Tool, source ToolSource) (ToolDescriptor, error) {
	if implementation == nil {
		return ToolDescriptor{}, fmt.Errorf("%w: 工具实现不能为空", ErrInvalidToolDescriptor)
	}
	name := strings.TrimSpace(implementation.Name())
	if name == "" {
		return ToolDescriptor{}, fmt.Errorf("%w: 工具名称不能为空", ErrInvalidToolDescriptor)
	}
	descriptor := ToolDescriptor{
		ID: "", ModelName: name, Version: "1.0.0", Source: source,
		Description: implementation.Description(), Public: true,
		Risk:      ToolRiskProfile{Level: ToolRiskLow, DefaultPolicy: ToolPolicyAllow, Reversible: true, Idempotent: true},
		Execution: ToolExecutionProfile{LongRunning: implementation.IsLongRunning()},
	}
	if implementation.IsLongRunning() {
		descriptor.Capabilities = append(descriptor.Capabilities, ToolCapabilityLongRunning)
	}
	if declaration, ok := implementation.(interface {
		Declaration() *genai.FunctionDeclaration
	}); ok {
		if value := declaration.Declaration(); value != nil {
			if strings.TrimSpace(value.Description) != "" {
				descriptor.Description = value.Description
			}
			if value.Parameters != nil {
				descriptor.InputSchema, _ = json.Marshal(value.Parameters)
			} else if value.ParametersJsonSchema != nil {
				descriptor.InputSchema, _ = json.Marshal(value.ParametersJsonSchema)
			}
			if value.Response != nil {
				descriptor.OutputSchema, _ = json.Marshal(value.Response)
			} else if value.ResponseJsonSchema != nil {
				descriptor.OutputSchema, _ = json.Marshal(value.ResponseJsonSchema)
			}
		}
	}
	descriptor.ID = stableRuntimeToolID(source, name)
	descriptor.Capabilities = append(descriptor.Capabilities, inferToolCapabilities(source, name)...)
	return r.Register(descriptor, implementation)
}

func (r *ToolRegistry) RegisterRuntimeTools(implementations []adktool.Tool, source ToolSource) error {
	for _, implementation := range implementations {
		if _, err := r.RegisterRuntimeTool(implementation, source); err != nil {
			return err
		}
	}
	return nil
}

func stableRuntimeToolID(source ToolSource, name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var builder strings.Builder
	builder.WriteString(strings.ToLower(strings.TrimSpace(string(source))))
	builder.WriteByte('.')
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == ':' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func inferToolCapabilities(source ToolSource, name string) []ToolCapability {
	name = strings.ToLower(name)
	result := make([]ToolCapability, 0, 3)
	if strings.Contains(name, "search") {
		result = append(result, ToolCapabilityNetwork)
	}
	if source == ToolSourceWorkspace {
		switch {
		case strings.Contains(name, "request_write"), strings.Contains(name, "patch"), strings.Contains(name, "delete"), strings.Contains(name, "mkdir"):
			result = append(result, ToolCapabilityWriteWorkspace, ToolCapabilityExternalSideEffect)
		case strings.Contains(name, "command"):
			result = append(result, ToolCapabilityExecuteProcess, ToolCapabilityExternalSideEffect)
		default:
			result = append(result, ToolCapabilityReadWorkspace)
		}
	}
	if source == ToolSourceMemory {
		if strings.Contains(name, "save") || strings.Contains(name, "write") {
			result = append(result, ToolCapabilityWriteMemory)
		} else {
			result = append(result, ToolCapabilityReadMemory)
		}
	}
	if strings.Contains(name, "request_") || strings.Contains(name, "delete") || strings.Contains(name, "command") {
		result = append(result, ToolCapabilityExternalSideEffect)
	}
	return uniqueToolCapabilities(result)
}

func (r *ToolRegistry) List() []ToolDescriptor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	items := make([]ToolDescriptor, 0, len(r.entries))
	for _, item := range r.entries {
		items = append(items, cloneToolDescriptor(item.descriptor))
	}
	r.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (r *ToolRegistry) Get(id, version string) (ToolDescriptor, adktool.Tool, error) {
	if r == nil {
		return ToolDescriptor{}, nil, ErrToolNotFound
	}
	key := strings.TrimSpace(id) + "@" + strings.TrimSpace(version)
	r.mu.RLock()
	entry, ok := r.entries[key]
	r.mu.RUnlock()
	if !ok {
		return ToolDescriptor{}, nil, ErrToolNotFound
	}
	return cloneToolDescriptor(entry.descriptor), entry.tool, nil
}

// Find returns public descriptor metadata for all versions of a stable tool
// ID. Implementations are never returned, so this method is safe for catalog
// and WebUI queries.
func (r *ToolRegistry) Find(id string) []ToolDescriptor {
	if r == nil {
		return nil
	}
	id = strings.TrimSpace(id)
	items := r.List()
	result := make([]ToolDescriptor, 0)
	for _, item := range items {
		if item.ID == id {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	return result
}

func (r *ToolRegistry) CatalogRevision() string {
	items := r.List()
	encoded, _ := json.Marshal(items)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

type ToolSelectionRequest struct {
	InvocationID    string
	Phase           string
	PolicyRevision  string
	ModelProfile    string
	AllowedTools    []string
	DeniedTools     []string
	RequiredTools   []string
	MaxSchemaTokens int
	Scores          map[string]float64
	Policy          func(ToolDescriptor) (ToolPolicy, string)
	Compatible      func(ToolDescriptor) (bool, string)
}

type ToolSelection struct {
	Tools           []adktool.Tool
	Descriptors     []ToolDescriptor
	Included        []ToolRef
	Excluded        []ToolSelectionExclusion
	MissingRequired []string
	SchemaTokens    int
	Snapshot        ToolSetSnapshot
}

// Select applies availability, policy, compatibility, phase and schema budget
// in that order. Required tools are considered first, then candidates are
// sorted by explicit score, source priority and stable ID.
func (r *ToolRegistry) Select(request ToolSelectionRequest) (ToolSelection, error) {
	if r == nil {
		return ToolSelection{}, errors.New("工具 Registry 不能为空")
	}
	if request.MaxSchemaTokens < 0 {
		return ToolSelection{}, fmt.Errorf("%w: schema budget 不能为负数", ErrInvalidToolDescriptor)
	}
	allowed := stringSet(request.AllowedTools)
	denied := stringSet(request.DeniedTools)
	required := stringSet(request.RequiredTools)
	items := r.List()
	r.mu.RLock()
	entries := make(map[string]registeredTool, len(r.entries))
	for key, entry := range r.entries {
		entries[key] = entry
	}
	r.mu.RUnlock()
	type candidate struct {
		entry    registeredTool
		required bool
		score    float64
	}
	candidates := make([]candidate, 0, len(items))
	selection := ToolSelection{Tools: make([]adktool.Tool, 0, len(items)), Descriptors: make([]ToolDescriptor, 0, len(items)), Included: make([]ToolRef, 0, len(items)), Excluded: make([]ToolSelectionExclusion, 0)}
	for _, descriptor := range items {
		key := descriptor.ID + "@" + descriptor.Version
		entry := entries[key]
		isRequired := containsTool(required, descriptor)
		reason := ""
		if descriptor.Availability.Status != ToolStatusReady {
			reason = "source_unhealthy"
		}
		if reason == "" && len(allowed) > 0 && !containsTool(allowed, descriptor) {
			reason = "policy_denied"
		}
		if reason == "" && containsTool(denied, descriptor) {
			reason = "policy_denied"
		}
		if reason == "" && len(descriptor.Execution.Phases) > 0 && strings.TrimSpace(request.Phase) != "" && !stringSliceContains(descriptor.Execution.Phases, strings.TrimSpace(request.Phase)) {
			reason = "phase_irrelevant"
		}
		if reason == "" && request.Policy != nil {
			decision, policyReason := request.Policy(descriptor)
			if decision == ToolPolicyDeny {
				reason = firstNonEmptyToolReason(policyReason, "policy_denied")
			}
		}
		if reason == "" && request.Compatible != nil {
			compatible, compatibilityReason := request.Compatible(descriptor)
			if !compatible {
				reason = firstNonEmptyToolReason(compatibilityReason, "model_unsupported")
			}
		}
		if reason != "" {
			selection.Excluded = append(selection.Excluded, ToolSelectionExclusion{ID: descriptor.ID, ModelName: descriptor.ModelName, Reason: reason})
			if isRequired {
				selection.MissingRequired = append(selection.MissingRequired, descriptor.ID)
			}
			continue
		}
		score := 0.0
		if request.Scores != nil {
			score = request.Scores[descriptor.ID]
			if score == 0 {
				score = request.Scores[descriptor.ModelName]
			}
		}
		candidate := candidate{entry: entry, required: isRequired, score: score}
		if isRequired {
			// Required candidates are sorted before all optional candidates below.
			candidate.score += 1e9
		}
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].required != candidates[j].required {
			return candidates[i].required
		}
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].entry.descriptor.Source != candidates[j].entry.descriptor.Source {
			return sourcePriority(candidates[i].entry.descriptor.Source) < sourcePriority(candidates[j].entry.descriptor.Source)
		}
		return candidates[i].entry.descriptor.ID < candidates[j].entry.descriptor.ID
	})
	for _, item := range candidates {
		descriptor := item.entry.descriptor
		tokens := toolDescriptorTokenEstimate(descriptor)
		if request.MaxSchemaTokens > 0 && selection.SchemaTokens+tokens > request.MaxSchemaTokens {
			reason := "over_schema_budget"
			if item.required {
				reason = "required_over_schema_budget"
				selection.MissingRequired = append(selection.MissingRequired, descriptor.ID)
			}
			selection.Excluded = append(selection.Excluded, ToolSelectionExclusion{ID: descriptor.ID, ModelName: descriptor.ModelName, Reason: reason})
			continue
		}
		selection.SchemaTokens += tokens
		selection.Tools = append(selection.Tools, item.entry.tool)
		selection.Descriptors = append(selection.Descriptors, cloneToolDescriptor(descriptor))
		selection.Included = append(selection.Included, ToolRef{ID: descriptor.ID, ModelName: descriptor.ModelName, Version: descriptor.Version, SchemaDigest: descriptor.SchemaDigest})
	}
	sort.Strings(selection.MissingRequired)
	selection.Snapshot = newToolSetSnapshot(request, r.CatalogRevision(), selection.Included, selection.Excluded)
	if err := selection.Snapshot.Validate(); err != nil {
		return ToolSelection{}, err
	}
	if len(selection.MissingRequired) > 0 {
		return selection, fmt.Errorf("%w: %s", ErrRequiredToolUnavailable, strings.Join(selection.MissingRequired, ", "))
	}
	return selection, nil
}

// Search implements the deferred-discovery shape without exposing full
// schemas. The caller must explicitly select a result in a later invocation
// turn before it is registered into a ToolSet Snapshot.
func (r *ToolRegistry) Search(query string, capabilities []ToolCapability, source ToolSource, risk ToolRiskLevel, limit int) []ToolDescriptor {
	query = strings.ToLower(strings.TrimSpace(query))
	capSet := make(map[ToolCapability]struct{}, len(capabilities))
	for _, capability := range capabilities {
		capSet[capability] = struct{}{}
	}
	items := r.List()
	result := make([]ToolDescriptor, 0)
	for _, item := range items {
		if source != "" && item.Source != source || risk != "" && item.Risk.Level != risk {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.ModelName+" "+item.Description), query) {
			continue
		}
		matchesCapability := true
		for capability := range capSet {
			if !stringSliceContainsToolCapability(item.Capabilities, capability) {
				matchesCapability = false
				break
			}
		}
		if !matchesCapability {
			continue
		}
		// Directory/search callers do not receive schemas or descriptions that
		// could contain untrusted instructions beyond the bounded summary.
		item.InputSchema = nil
		item.OutputSchema = nil
		result = append(result, item)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

func newToolSetSnapshot(request ToolSelectionRequest, catalogRevision string, included []ToolRef, excluded []ToolSelectionExclusion) ToolSetSnapshot {
	refs := append([]ToolRef(nil), included...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	copyExcluded := append([]ToolSelectionExclusion(nil), excluded...)
	sort.Slice(copyExcluded, func(i, j int) bool {
		if copyExcluded[i].ID != copyExcluded[j].ID {
			return copyExcluded[i].ID < copyExcluded[j].ID
		}
		return copyExcluded[i].Reason < copyExcluded[j].Reason
	})
	basis, _ := json.Marshal(struct {
		CatalogRevision string                   `json:"catalog_revision"`
		PolicyRevision  string                   `json:"policy_revision,omitempty"`
		ModelProfile    string                   `json:"model_profile,omitempty"`
		Tools           []ToolRef                `json:"tools"`
		Excluded        []ToolSelectionExclusion `json:"excluded,omitempty"`
	}{catalogRevision, request.PolicyRevision, request.ModelProfile, refs, copyExcluded})
	sum := sha256.Sum256(basis)
	digest := hex.EncodeToString(sum[:])
	// The snapshot row is keyed per invocation (InvocationID carries a unique
	// index), so the identifier must include the invocation. Deriving it from
	// the tool-set digest alone made every invocation after the first collide on
	// the primary key whenever the tool set was unchanged.
	invocationID := strings.TrimSpace(request.InvocationID)
	return ToolSetSnapshot{ID: "toolset:" + digest[:24] + ":" + invocationID, InvocationID: invocationID, CatalogRevision: catalogRevision, PolicyRevision: strings.TrimSpace(request.PolicyRevision), ModelProfile: strings.TrimSpace(request.ModelProfile), Tools: refs, Excluded: copyExcluded, Digest: digest, CreatedAt: time.Now().UTC()}
}

func toolDescriptorTokenEstimate(descriptor ToolDescriptor) int {
	encoded, _ := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Input       json.RawMessage `json:"input,omitempty"`
		Output      json.RawMessage `json:"output,omitempty"`
	}{descriptor.ModelName, descriptor.Description, descriptor.InputSchema, descriptor.OutputSchema})
	return EstimateTextTokens(string(encoded))
}

func cloneToolDescriptor(item ToolDescriptor) ToolDescriptor {
	item.InputSchema = append(json.RawMessage(nil), item.InputSchema...)
	item.OutputSchema = append(json.RawMessage(nil), item.OutputSchema...)
	item.Capabilities = append([]ToolCapability(nil), item.Capabilities...)
	item.Execution.Phases = append([]string(nil), item.Execution.Phases...)
	return item
}

func uniqueToolCapabilities(values []ToolCapability) []ToolCapability {
	seen := make(map[ToolCapability]struct{}, len(values))
	result := make([]ToolCapability, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func normalizeStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func containsTool(values map[string]struct{}, descriptor ToolDescriptor) bool {
	_, byID := values[descriptor.ID]
	_, byName := values[descriptor.ModelName]
	return byID || byName
}

func stringSliceContains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func stringSliceContainsToolCapability(values []ToolCapability, value ToolCapability) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func sourcePriority(source ToolSource) int {
	switch source {
	case ToolSourceBuiltin:
		return 0
	case ToolSourceWorkspace:
		return 1
	case ToolSourceMemory:
		return 2
	default:
		return 3
	}
}

func firstNonEmptyToolReason(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}
