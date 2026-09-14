package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// RuntimeCapabilityFailureThreshold is the number of distinct, recent
	// stable rejections required before an observed feature is downgraded to
	// unsupported. Until then the effective state is unknown, so a required
	// feature fails closed instead of being sent repeatedly.
	RuntimeCapabilityFailureThreshold = 3
	RuntimeCapabilityEvidenceWindow   = 24 * time.Hour
)

// RecordRuntimeCapabilityObservations stores metadata-only evidence from a
// real model call without mutating the active profile. Runtime uses this
// method while an Invocation is running so its immutable capability snapshot
// cannot be invalidated by the very evidence it is collecting.
func (r *Registry) RecordRuntimeCapabilityObservations(ctx context.Context, providerID, modelID string, observations []CapabilityObservation) error {
	_, _, _, records, err := r.prepareRuntimeEvidence(ctx, providerID, modelID, observations)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	if observationRepo, ok := r.repo.(CapabilityObservationRepository); ok {
		if err := observationRepo.SaveCapabilityObservations(ctx, records); err != nil {
			return fmt.Errorf("保存模型运行证据失败: %w", err)
		}
	}
	return nil
}

// RecordRuntimeCapabilityEvidence stores metadata-only evidence and
// immediately reconciles the active profile. Callers handling an active
// Invocation should use RecordRuntimeCapabilityObservations and defer this
// method until the Invocation reaches a terminal state.
func (r *Registry) RecordRuntimeCapabilityEvidence(ctx context.Context, providerID, modelID string, observations []CapabilityObservation) error {
	if r == nil {
		return fmt.Errorf("模型运行证据需要非空 Registry")
	}
	if err := r.RecordRuntimeCapabilityObservations(ctx, providerID, modelID, observations); err != nil {
		return err
	}
	return r.ReconcileRuntimeCapabilityEvidence(ctx, providerID, modelID)
}

// ReconcileRuntimeCapabilityEvidence applies recent, durable runtime
// observations to the catalog profile. It is intended after an Invocation is
// terminal, preserving the snapshot used during that Invocation.
func (r *Registry) ReconcileRuntimeCapabilityEvidence(ctx context.Context, providerID, modelID string) error {
	if r == nil {
		return fmt.Errorf("模型运行证据需要非空 Registry")
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	observationRepo, ok := r.repo.(CapabilityObservationRepository)
	if !ok {
		return ErrCapabilityObservationUnavailable
	}
	item, err := r.Get(providerID)
	if err != nil {
		return err
	}
	index := -1
	for i := range item.Models {
		if item.Models[i].ID == modelID {
			index = i
			break
		}
	}
	if index < 0 {
		return ErrNoModel
	}
	model := item.Models[index]
	profile := model.Capabilities
	if profile == nil {
		value := DefaultCapabilities(item, model)
		profile = &value
	}
	original := *profile
	records, err := observationRepo.ListCapabilityObservations(ctx, providerID, modelID, MaxCapabilityObservationHistory)
	if err != nil {
		return fmt.Errorf("读取模型运行证据失败: %w", err)
	}
	now := time.Now().UTC()
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].ObservedAt.Before(records[j].ObservedAt)
	})
	for _, record := range records {
		if record.Source != "runtime" || record.ObservedAt.Before(now.Add(-RuntimeCapabilityEvidenceWindow)) || record.ObservedAt.After(now.Add(time.Minute)) {
			continue
		}
		applyRuntimeObservation(profile, CapabilityObservation{Feature: record.Feature, State: record.State, Source: record.Source, Confidence: record.Confidence, Route: record.Route, Reason: record.Reason, ObservedAt: record.ObservedAt}, records, nil, now)
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile.SnapshotID() == original.SnapshotID() {
		return nil
	}
	baseRevision := profile.SourceRevision
	if baseRevision == "" {
		baseRevision = "catalog"
	}
	if marker := strings.Index(baseRevision, ":runtime-evidence:"); marker >= 0 {
		baseRevision = baseRevision[:marker]
	}
	profile.SourceRevision = baseRevision + ":runtime-evidence:" + now.Format("20060102T150405.000000000Z")
	profile.UpdatedAt = now
	model.Capabilities = profile
	item.Models[index] = model
	if _, err := r.Save(ctx, SaveRequest{Provider: item}); err != nil {
		return fmt.Errorf("保存模型运行能力状态失败: %w", err)
	}
	return nil
}

func (r *Registry) prepareRuntimeEvidence(ctx context.Context, providerID, modelID string, observations []CapabilityObservation) (Provider, Model, []CapabilityObservation, []CapabilityObservationRecord, error) {
	if r == nil {
		return Provider{}, Model{}, nil, nil, fmt.Errorf("模型运行证据需要非空 Registry")
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" || len(observations) == 0 {
		return Provider{}, Model{}, nil, nil, nil
	}
	item, err := r.Get(providerID)
	if err != nil {
		return Provider{}, Model{}, nil, nil, err
	}
	index := -1
	for i := range item.Models {
		if item.Models[i].ID == modelID {
			index = i
			break
		}
	}
	if index < 0 {
		return Provider{}, Model{}, nil, nil, ErrNoModel
	}
	model := item.Models[index]
	profile := model.Capabilities
	if profile == nil {
		value := DefaultCapabilities(item, model)
		profile = &value
	}
	defaultRoute := strings.TrimSpace(profile.Route)
	now := time.Now().UTC()
	normalized := make([]CapabilityObservation, 0, len(observations))
	for _, observation := range observations {
		feature := strings.TrimSpace(observation.Feature)
		if feature == "" {
			continue
		}
		if observation.State != SupportSupported && observation.State != SupportUnsupported && observation.State != SupportUnknown && observation.State != SupportDegraded {
			continue
		}
		observedAt := observation.ObservedAt
		if observedAt.IsZero() {
			observedAt = now
		}
		route := strings.TrimSpace(observation.Route)
		if route == "" {
			route = defaultRoute
		}
		confidence := observation.Confidence
		if confidence < 0 {
			confidence = 0
		}
		if confidence > 1 {
			confidence = 1
		}
		normalized = append(normalized, CapabilityObservation{Feature: feature, State: observation.State, Source: "runtime", Confidence: confidence, Route: route, Reason: sanitizeCapabilityObservationReason(observation.Reason), ObservedAt: observedAt.UTC()})
	}
	if len(normalized) == 0 {
		return item, model, nil, nil, nil
	}
	result := CapabilityProbeResult{Profile: *profile, Observations: normalized, Message: "runtime evidence"}
	records, err := capabilityObservationRecords(item, model, *profile, result, now)
	return item, model, normalized, records, err
}

func applyRuntimeObservation(profile *ModelCapabilityProfile, observation CapabilityObservation, prior, current []CapabilityObservationRecord, now time.Time) {
	if profile == nil {
		return
	}
	target := supportField(profile, observation.Feature)
	if target == nil || target.Source == "user_override" {
		return
	}
	if (profile.Route == "" && observation.Route != "") || (profile.Route != "" && observation.Route != "" && profile.Route != observation.Route) {
		// Route-specific evidence is still durable, but must not change the
		// profile used by another active wire format.
		return
	}
	switch observation.State {
	case SupportUnknown:
		return
	case SupportUnsupported:
		count := recentRuntimeEvidenceCount(prior, current, observation, now, SupportUnsupported)
		if count < RuntimeCapabilityFailureThreshold {
			// Fail closed while evidence is accumulating. This is deliberately
			// unknown rather than unsupported because the threshold is not met.
			target.State = SupportUnknown
			target.Source = "runtime_threshold_pending"
			target.Confidence = 0
			target.ObservedAt = observation.ObservedAt
			target.Reason = fmt.Sprintf("runtime rejection threshold pending (%d/%d)", count, RuntimeCapabilityFailureThreshold)
			return
		}
		target.State = SupportUnsupported
		target.Source = "runtime_threshold"
		target.Confidence = 0.9
		target.ObservedAt = observation.ObservedAt
		target.Reason = fmt.Sprintf("%d stable runtime rejections within %s", count, RuntimeCapabilityEvidenceWindow)
		if observation.Feature == "structured_output_schema" {
			profile.JSONSchemaDialect = ""
		}
	case SupportSupported, SupportDegraded:
		// A successful real call is direct evidence for the current route. Do
		// not upgrade a user override, handled above; otherwise preserve the
		// observed degraded state and provenance.
		target.State = observation.State
		target.Source = "runtime"
		target.Confidence = observation.Confidence
		target.ObservedAt = observation.ObservedAt
		target.Reason = observation.Reason
		if observation.Feature == "structured_output_schema" {
			profile.JSONSchemaDialect = probeSchemaDialect(observation.Route)
			if profile.StructuredOutput.State == SupportUnknown && profile.StructuredOutput.Source != "user_override" {
				profile.StructuredOutput = *target
			}
		}
	}
}

func recentRuntimeEvidenceCount(prior, current []CapabilityObservationRecord, target CapabilityObservation, now time.Time, state SupportState) int {
	seen := make(map[string]struct{}, len(prior)+len(current))
	count := 0
	match := func(item CapabilityObservationRecord) {
		if item.Source != "runtime" || item.Feature != target.Feature || item.State != state || item.Route != target.Route {
			return
		}
		observedAt := item.ObservedAt.UTC()
		if observedAt.IsZero() || observedAt.Before(now.Add(-RuntimeCapabilityEvidenceWindow)) || observedAt.After(now.Add(time.Minute)) {
			return
		}
		if _, exists := seen[item.ID]; exists {
			return
		}
		seen[item.ID] = struct{}{}
		count++
	}
	for _, item := range prior {
		match(item)
	}
	for _, item := range current {
		match(item)
	}
	// The current record is generated from the observation and may have a
	// timestamp just outside the injected `now` in deterministic tests. Count
	// it by feature/state/route as a final fallback, still deduping by ID.
	if count == 0 && target.State == state {
		count = 1
	}
	return count
}
