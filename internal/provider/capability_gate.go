package provider

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const MaxCapabilityGateProfiles = 100

var ErrInvalidCapabilityGate = errors.New("模型能力发布门禁无效")

// CapabilityGatePolicy controls cross-version comparison of provider model
// capability profiles. A nil boolean uses the conservative default true.
// The policy only consumes metadata profiles; it never calls a provider.
type CapabilityGatePolicy struct {
	RequireBaseline   bool  `json:"require_baseline"`
	FailOnDowngrade   *bool `json:"fail_on_downgrade,omitempty"`
	FailOnRouteChange *bool `json:"fail_on_route_change,omitempty"`
	MaxDowngrades     int   `json:"max_downgrades"`
}

func (p CapabilityGatePolicy) normalized() (CapabilityGatePolicy, error) {
	if p.MaxDowngrades < 0 {
		return CapabilityGatePolicy{}, fmt.Errorf("%w: max_downgrades 不能为负数", ErrInvalidCapabilityGate)
	}
	if p.FailOnDowngrade == nil {
		value := true
		p.FailOnDowngrade = &value
	}
	if p.FailOnRouteChange == nil {
		value := true
		p.FailOnRouteChange = &value
	}
	return p, nil
}

// CapabilityRegression describes one profile change that may affect request
// safety or model behavior. Before/After are strings so support-state and
// numeric-limit changes share the same stable JSON shape.
type CapabilityRegression struct {
	Key     string `json:"key"`
	Feature string `json:"feature"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Reason  string `json:"reason"`
}

type CapabilityGateResult struct {
	Passed             bool                   `json:"passed"`
	TotalCandidates    int                    `json:"total_candidates"`
	ComparedCandidates int                    `json:"compared_candidates"`
	Downgrades         int                    `json:"downgrades"`
	Regressions        []CapabilityRegression `json:"regressions,omitempty"`
	Reasons            []string               `json:"reasons,omitempty"`
}

// EvaluateCapabilityReleaseGate compares candidate profiles with the same
// provider/model/route in a baseline. It validates every input before
// comparing, rejects duplicate keys, and sorts regressions/reasons so a CI
// result is reproducible. Loss of a known numeric limit is treated as a
// downgrade because silently losing a safety bound is unsafe.
func EvaluateCapabilityReleaseGate(candidates, baseline []ModelCapabilityProfile, policy CapabilityGatePolicy) (CapabilityGateResult, error) {
	if len(candidates) == 0 || len(candidates) > MaxCapabilityGateProfiles || len(baseline) > MaxCapabilityGateProfiles {
		return CapabilityGateResult{}, fmt.Errorf("%w: profile 数量必须为 candidate 1–%d、baseline 0–%d", ErrInvalidCapabilityGate, MaxCapabilityGateProfiles, MaxCapabilityGateProfiles)
	}
	policy, err := policy.normalized()
	if err != nil {
		return CapabilityGateResult{}, err
	}
	candidateByKey, err := validateCapabilityGateProfiles(candidates, "candidate")
	if err != nil {
		return CapabilityGateResult{}, err
	}
	baselineByKey, err := validateCapabilityGateProfiles(baseline, "baseline")
	if err != nil {
		return CapabilityGateResult{}, err
	}
	result := CapabilityGateResult{Passed: true, TotalCandidates: len(candidates), Regressions: make([]CapabilityRegression, 0)}
	if policy.RequireBaseline && len(baseline) == 0 {
		result.Passed = false
		result.Reasons = append(result.Reasons, "baseline_required")
	}

	keys := make([]string, 0, len(candidateByKey))
	for key := range candidateByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		candidate := candidateByKey[key]
		base, ok := baselineByKey[key]
		if !ok {
			if previousKey := sameTargetBaselineKey(baselineByKey, candidate); previousKey != "" {
				result.Reasons = append(result.Reasons, "route_changed:"+key)
				if *policy.FailOnRouteChange {
					result.Passed = false
				}
			} else if policy.RequireBaseline {
				result.Passed = false
				result.Reasons = append(result.Reasons, "baseline_missing:"+key)
			}
			continue
		}
		result.ComparedCandidates++
		compareCapabilityProfile(&result, base, candidate)
	}
	if result.Downgrades > policy.MaxDowngrades {
		result.Passed = false
		result.Reasons = append(result.Reasons, fmt.Sprintf("downgrades:%d>%d", result.Downgrades, policy.MaxDowngrades))
	}
	if *policy.FailOnDowngrade && result.Downgrades > 0 {
		result.Passed = false
		result.Reasons = append(result.Reasons, fmt.Sprintf("capability_downgrade:%d", result.Downgrades))
	}
	sort.SliceStable(result.Regressions, func(i, j int) bool {
		if result.Regressions[i].Key == result.Regressions[j].Key {
			return result.Regressions[i].Feature < result.Regressions[j].Feature
		}
		return result.Regressions[i].Key < result.Regressions[j].Key
	})
	sort.Strings(result.Reasons)
	return result, nil
}

func validateCapabilityGateProfiles(profiles []ModelCapabilityProfile, label string) (map[string]ModelCapabilityProfile, error) {
	result := make(map[string]ModelCapabilityProfile, len(profiles))
	for index, item := range profiles {
		item = normalizeCapabilityProfile(item)
		if err := item.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %s[%d]: %v", ErrInvalidCapabilityGate, label, index, err)
		}
		key := capabilityProfileKey(item)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("%w: %s profile 重复 %s", ErrInvalidCapabilityGate, label, key)
		}
		result[key] = item
	}
	return result, nil
}

func capabilityProfileKey(profile ModelCapabilityProfile) string {
	return strings.Join([]string{strings.TrimSpace(profile.ProviderID), strings.TrimSpace(profile.ModelID), string(normalizeProtocol(profile.Protocol)), strings.TrimSpace(profile.Route)}, "\x00")
}

func capabilityTargetKey(profile ModelCapabilityProfile) string {
	return strings.Join([]string{strings.TrimSpace(profile.ProviderID), strings.TrimSpace(profile.ModelID), string(normalizeProtocol(profile.Protocol))}, "\x00")
}

func sameTargetBaselineKey(baseline map[string]ModelCapabilityProfile, candidate ModelCapabilityProfile) string {
	target := capabilityTargetKey(candidate)
	matches := make([]string, 0, len(baseline))
	for key, item := range baseline {
		if capabilityTargetKey(item) == target {
			matches = append(matches, key)
		}
	}
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[0]
}

func compareCapabilityProfile(result *CapabilityGateResult, baseline, candidate ModelCapabilityProfile) {
	key := capabilityProfileKey(candidate)
	compareSupport := func(feature string, before, after SupportState) {
		if supportRank(after) >= supportRank(before) {
			return
		}
		result.Downgrades++
		result.Regressions = append(result.Regressions, CapabilityRegression{Key: key, Feature: feature, Before: string(before), After: string(after), Reason: "support_downgrade"})
	}
	for _, item := range []struct {
		feature string
		before  SupportState
		after   SupportState
	}{
		{"tool_calling", baseline.ToolCalling.State, candidate.ToolCalling.State},
		{"parallel_tool_calls", baseline.ParallelToolCalls.State, candidate.ParallelToolCalls.State},
		{"structured_output", baseline.StructuredOutput.State, candidate.StructuredOutput.State},
		{"structured_output_schema", baseline.StructuredOutputSchema.State, candidate.StructuredOutputSchema.State},
		{"streaming", baseline.Streaming.State, candidate.Streaming.State},
		{"images", baseline.Images.State, candidate.Images.State},
		{"input_files", baseline.InputFiles.State, candidate.InputFiles.State},
		{"audio", baseline.Audio.State, candidate.Audio.State},
		{"reasoning_effort", baseline.ReasoningEffort.State, candidate.ReasoningEffort.State},
		{"reasoning_summary", baseline.ReasoningSummary.State, candidate.ReasoningSummary.State},
		{"prompt_caching", baseline.PromptCaching.State, candidate.PromptCaching.State},
		{"native_compaction", baseline.NativeCompaction.State, candidate.NativeCompaction.State},
	} {
		compareSupport(item.feature, item.before, item.after)
	}
	compareLimit := func(feature string, before, after CapabilityValue[int]) {
		if !before.Known {
			return
		}
		if !after.Known {
			result.Downgrades++
			result.Regressions = append(result.Regressions, CapabilityRegression{Key: key, Feature: feature, Before: fmt.Sprint(before.Value), After: "unknown", Reason: "known_limit_lost"})
			return
		}
		if after.Value < before.Value {
			result.Downgrades++
			result.Regressions = append(result.Regressions, CapabilityRegression{Key: key, Feature: feature, Before: fmt.Sprint(before.Value), After: fmt.Sprint(after.Value), Reason: "limit_decreased"})
		}
	}
	compareLimit("context_window", baseline.ContextWindow, candidate.ContextWindow)
	compareLimit("max_output_tokens", baseline.MaxOutputTokens, candidate.MaxOutputTokens)
	compareLimit("max_tools", baseline.MaxTools, candidate.MaxTools)
	compareLimit("max_schema_bytes", baseline.MaxSchemaBytes, candidate.MaxSchemaBytes)
	if supportRank(candidate.StructuredOutputSchema.State) >= supportRank(SupportDegraded) && supportRank(baseline.StructuredOutputSchema.State) >= supportRank(SupportDegraded) && strings.TrimSpace(baseline.JSONSchemaDialect) != strings.TrimSpace(candidate.JSONSchemaDialect) {
		result.Downgrades++
		result.Regressions = append(result.Regressions, CapabilityRegression{Key: key, Feature: "json_schema_dialect", Before: baseline.JSONSchemaDialect, After: candidate.JSONSchemaDialect, Reason: "dialect_changed"})
	}
	baseModes := make(map[string]struct{}, len(baseline.ToolChoiceModes))
	for _, mode := range baseline.ToolChoiceModes {
		baseModes[strings.ToLower(strings.TrimSpace(mode))] = struct{}{}
	}
	candidateModes := make(map[string]struct{}, len(candidate.ToolChoiceModes))
	for _, mode := range candidate.ToolChoiceModes {
		candidateModes[strings.ToLower(strings.TrimSpace(mode))] = struct{}{}
	}
	missingModes := make([]string, 0)
	for mode := range baseModes {
		if mode != "" {
			if _, ok := candidateModes[mode]; !ok {
				missingModes = append(missingModes, mode)
			}
		}
	}
	if len(missingModes) > 0 {
		sort.Strings(missingModes)
		result.Downgrades++
		result.Regressions = append(result.Regressions, CapabilityRegression{Key: key, Feature: "tool_choice_modes", Before: strings.Join(missingModes, ","), After: "missing", Reason: "tool_choice_mode_removed"})
	}
}
