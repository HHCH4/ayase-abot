package provider

import (
	"errors"
	"testing"
)

func capabilityGateProfile(providerID, modelID, route string) ModelCapabilityProfile {
	profile := DefaultCapabilities(Provider{ID: providerID, Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormat(route)}, Model{ID: modelID, ContextWindow: 8192, MaxOutputTokens: 1024})
	profile.Route = route
	profile.ToolCalling = Support{State: SupportSupported, Source: "catalog"}
	profile.ParallelToolCalls = Support{State: SupportSupported, Source: "catalog"}
	profile.ToolChoiceModes = []string{"auto", "none", "any"}
	profile.Streaming = Support{State: SupportSupported, Source: "catalog"}
	profile.StructuredOutput = Support{State: SupportSupported, Source: "probe"}
	profile.StructuredOutputSchema = Support{State: SupportSupported, Source: "probe"}
	profile.JSONSchemaDialect = "openai-" + route + "-json-schema"
	profile.MaxTools = CapabilityValue[int]{Value: 32, Known: true, Source: "catalog"}
	profile.MaxSchemaBytes = CapabilityValue[int]{Value: 64 * 1024, Known: true, Source: "catalog"}
	return profile
}

func TestEvaluateCapabilityReleaseGateRejectsProviderRegressions(t *testing.T) {
	base := capabilityGateProfile("openai", "model", "chat")
	candidate := base
	candidate.Streaming = Support{State: SupportDegraded, Source: "probe"}
	candidate.ContextWindow.Value = 4096
	candidate.ToolChoiceModes = []string{"auto", "any"}
	candidate.StructuredOutputSchema.State = SupportDegraded
	candidate.JSONSchemaDialect = "openai-chat-json-schema-v2"
	gate, err := EvaluateCapabilityReleaseGate([]ModelCapabilityProfile{candidate}, []ModelCapabilityProfile{base}, CapabilityGatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if gate.Passed || gate.ComparedCandidates != 1 || gate.Downgrades < 4 || len(gate.Regressions) < 4 {
		t.Fatalf("能力退化应阻断 gate: %#v", gate)
	}
	for index := 1; index < len(gate.Regressions); index++ {
		previous, current := gate.Regressions[index-1], gate.Regressions[index]
		if previous.Key > current.Key || (previous.Key == current.Key && previous.Feature > current.Feature) {
			t.Fatalf("regression 排序不稳定: %#v", gate.Regressions)
		}
	}
	if len(gate.Reasons) == 0 || gate.Reasons[0] != "capability_downgrade:5" {
		// The exact count is intentionally asserted against the current
		// metadata contract: streaming, schema state, dialect, context limit,
		// and tool-choice removal each count once.
		t.Fatalf("能力门禁原因未稳定排序或计数错误: %#v", gate.Reasons)
	}
}

func TestEvaluateCapabilityReleaseGateHandlesRouteAndBaselinePolicy(t *testing.T) {
	base := capabilityGateProfile("openai", "model", "chat")
	responses := capabilityGateProfile("openai", "model", "responses")
	legacy := capabilityGateProfile("openai", "model", "legacy")
	gate, err := EvaluateCapabilityReleaseGate([]ModelCapabilityProfile{responses}, []ModelCapabilityProfile{legacy, base}, CapabilityGatePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if gate.Passed || len(gate.Reasons) != 1 || gate.Reasons[0] != "route_changed:openai\x00model\x00openai-compatible\x00responses" {
		t.Fatalf("route 变化应有明确阻断原因: %#v", gate)
	}
	allowRoute := false
	gate, err = EvaluateCapabilityReleaseGate([]ModelCapabilityProfile{responses}, []ModelCapabilityProfile{legacy, base}, CapabilityGatePolicy{FailOnRouteChange: &allowRoute})
	if err != nil || !gate.Passed {
		t.Fatalf("显式允许 route 变化时不应阻断: gate=%#v err=%v", gate, err)
	}
	missing, err := EvaluateCapabilityReleaseGate([]ModelCapabilityProfile{base}, nil, CapabilityGatePolicy{RequireBaseline: true})
	if err != nil || missing.Passed || len(missing.Reasons) != 2 || missing.Reasons[0] != "baseline_missing:openai\x00model\x00openai-compatible\x00chat" || missing.Reasons[1] != "baseline_required" {
		t.Fatalf("缺失 baseline 应稳定失败: %#v err=%v", missing, err)
	}
}

func TestEvaluateCapabilityReleaseGateValidatesInputsBeforeComparison(t *testing.T) {
	base := capabilityGateProfile("openai", "model", "chat")
	if _, err := EvaluateCapabilityReleaseGate([]ModelCapabilityProfile{base, base}, []ModelCapabilityProfile{base}, CapabilityGatePolicy{}); !errors.Is(err, ErrInvalidCapabilityGate) {
		t.Fatalf("candidate 重复 profile 应被拒绝: %v", err)
	}
	if _, err := EvaluateCapabilityReleaseGate([]ModelCapabilityProfile{base}, []ModelCapabilityProfile{base, base}, CapabilityGatePolicy{}); !errors.Is(err, ErrInvalidCapabilityGate) {
		t.Fatalf("baseline 重复 profile 应被拒绝: %v", err)
	}
	if _, err := EvaluateCapabilityReleaseGate([]ModelCapabilityProfile{base}, nil, CapabilityGatePolicy{MaxDowngrades: -1}); !errors.Is(err, ErrInvalidCapabilityGate) {
		t.Fatalf("无效 gate policy 应被拒绝: %v", err)
	}
	profiles := make([]ModelCapabilityProfile, MaxCapabilityGateProfiles+1)
	for index := range profiles {
		profiles[index] = capabilityGateProfile("openai", "model-"+string(rune('a'+index)), "chat")
	}
	if _, err := EvaluateCapabilityReleaseGate(profiles, nil, CapabilityGatePolicy{}); !errors.Is(err, ErrInvalidCapabilityGate) {
		t.Fatalf("超过 gate profile 上限应被拒绝: %v", err)
	}
}

func TestEvaluateCapabilityReleaseGateCanReportWithoutFailingOnDowngrade(t *testing.T) {
	base := capabilityGateProfile("openai", "model", "chat")
	candidate := base
	candidate.Images = Support{State: SupportUnsupported, Source: "probe"}
	fail := false
	gate, err := EvaluateCapabilityReleaseGate([]ModelCapabilityProfile{candidate}, []ModelCapabilityProfile{base}, CapabilityGatePolicy{FailOnDowngrade: &fail, MaxDowngrades: 10})
	if err != nil || !gate.Passed || gate.Downgrades != 1 || len(gate.Regressions) != 1 {
		t.Fatalf("显式报告模式应保留退化但不阻断: gate=%#v err=%v", gate, err)
	}
}
