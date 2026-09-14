package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"Abot/internal/provider"
)

func httpCapabilityGateProfile(route string) provider.ModelCapabilityProfile {
	profile := provider.DefaultCapabilities(
		provider.Provider{ID: "gate-provider", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormat(route)},
		provider.Model{ID: "gate-model", ContextWindow: 8192, MaxOutputTokens: 1024},
	)
	profile.Route = route
	profile.Streaming = provider.Support{State: provider.SupportSupported, Source: "catalog"}
	profile.StructuredOutput = provider.Support{State: provider.SupportSupported, Source: "probe"}
	profile.StructuredOutputSchema = provider.Support{State: provider.SupportSupported, Source: "probe"}
	profile.JSONSchemaDialect = "openai-" + route
	return profile
}

func TestProviderCapabilityGateAPIIsBoundedDeterministicAndMetadataOnly(t *testing.T) {
	base := httpCapabilityGateProfile("chat")
	candidate := base
	candidate.Streaming = provider.Support{State: provider.SupportDegraded, Source: "probe"}
	requestBody := map[string]any{
		"candidate_profiles": []provider.ModelCapabilityProfile{candidate},
		"baseline_profiles":  []provider.ModelCapabilityProfile{base},
		"policy":             provider.CapabilityGatePolicy{},
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	response := callHTTP(NewServer(nil, nil).Handler(), http.MethodPost, "/api/v1/providers/capability-gate", string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("能力发布门禁 API 状态码=%d body=%s", response.Code, response.Body.String())
	}
	var result provider.CapabilityGateResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析能力发布门禁响应失败: %v", err)
	}
	if result.Passed || result.TotalCandidates != 1 || result.ComparedCandidates != 1 || result.Downgrades != 1 {
		t.Fatalf("能力发布门禁结果不正确: %#v", result)
	}
	if len(result.Regressions) != 1 || result.Regressions[0].Feature != "streaming" {
		t.Fatalf("能力发布门禁退化明细不正确: %#v", result.Regressions)
	}
	if len(result.Reasons) == 0 || result.Reasons[0] != "capability_downgrade:1" {
		t.Fatalf("能力发布门禁原因不稳定: %#v", result.Reasons)
	}

	invalid := callHTTP(NewServer(nil, nil).Handler(), http.MethodPost, "/api/v1/providers/capability-gate", `{"candidate_profiles":[],"unexpected":true}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("能力发布门禁应拒绝未知字段/空 candidate，状态码=%d body=%s", invalid.Code, invalid.Body.String())
	}

	tooMany := make([]provider.ModelCapabilityProfile, provider.MaxCapabilityGateProfiles+1)
	for index := range tooMany {
		tooMany[index] = httpCapabilityGateProfile("chat")
		tooMany[index].ModelID = "gate-model-" + string(rune('a'+index))
	}
	requestBody["candidate_profiles"] = tooMany
	body, err = json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	bounded := callHTTP(NewServer(nil, nil).Handler(), http.MethodPost, "/api/v1/providers/capability-gate", string(body))
	if bounded.Code != http.StatusBadRequest {
		t.Fatalf("能力发布门禁应拒绝超限 profile，状态码=%d body=%s", bounded.Code, bounded.Body.String())
	}
}
