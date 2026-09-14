package httpapi

import (
	"fmt"
	"net/http"

	"Abot/internal/provider"
)

// capabilityGatePayload keeps the release gate boundary explicit. Profiles
// contain metadata only; provider credentials and request content are never
// accepted by this endpoint.
type capabilityGatePayload struct {
	CandidateProfiles []provider.ModelCapabilityProfile `json:"candidate_profiles"`
	BaselineProfiles  []provider.ModelCapabilityProfile `json:"baseline_profiles,omitempty"`
	Policy            provider.CapabilityGatePolicy     `json:"policy"`
}

// evaluateProviderCapabilityGate runs the bounded, deterministic provider
// capability release gate. It performs no upstream calls and does not mutate
// the provider registry.
func (s *Server) evaluateProviderCapabilityGate(writer http.ResponseWriter, request *http.Request) {
	var payload capabilityGatePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	result, err := provider.EvaluateCapabilityReleaseGate(payload.CandidateProfiles, payload.BaselineProfiles, payload.Policy)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}
