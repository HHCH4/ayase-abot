package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultCapabilityObservationLimit = 50
	maxCapabilityObservationLimit     = 200
	maxCapabilityObservationReason    = 240
	// MaxCapabilityObservationHistory bounds durable evidence per model.
	MaxCapabilityObservationHistory = 100
)

var ErrCapabilityObservationUnavailable = errors.New("当前存储未提供模型能力 observation 历史")

// CapabilityObservationRecord is the durable, metadata-only form of one
// probe observation. It deliberately has no request, response, tool args or
// secret fields; the reason is bounded and sanitized before persistence.
type CapabilityObservationRecord struct {
	ID         string       `json:"id"`
	ProviderID string       `json:"provider_id"`
	ModelID    string       `json:"model_id"`
	Protocol   Protocol     `json:"protocol"`
	Route      string       `json:"route,omitempty"`
	Feature    string       `json:"feature"`
	State      SupportState `json:"state"`
	Source     string       `json:"source"`
	Confidence float64      `json:"confidence,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
	CreatedAt  time.Time    `json:"created_at"`
}

func (r CapabilityObservationRecord) Validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.ProviderID) == "" || strings.TrimSpace(r.ModelID) == "" {
		return fmt.Errorf("能力 observation 缺少 id/provider_id/model_id")
	}
	if strings.TrimSpace(r.Feature) == "" || strings.TrimSpace(r.Source) == "" {
		return fmt.Errorf("能力 observation 缺少 feature/source")
	}
	switch r.State {
	case SupportSupported, SupportUnsupported, SupportUnknown, SupportDegraded:
	default:
		return fmt.Errorf("能力 observation state 无效: %q", r.State)
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf("能力 observation confidence 超出范围")
	}
	if r.ObservedAt.IsZero() || r.CreatedAt.IsZero() {
		return fmt.Errorf("能力 observation 缺少时间")
	}
	if len(r.Reason) > maxCapabilityObservationReason {
		return fmt.Errorf("能力 observation reason 超出长度限制")
	}
	return nil
}

// CapabilityObservationRepository is optional so embedders using an in-memory
// or legacy provider repository can keep the existing Registry contract.
type CapabilityObservationRepository interface {
	SaveCapabilityObservations(context.Context, []CapabilityObservationRecord) error
	ListCapabilityObservations(context.Context, string, string, int) ([]CapabilityObservationRecord, error)
}

// capabilityObservationRecords converts an adapter result into bounded,
// deterministic records. IDs include the observation timestamp and reason so
// repeated probes are idempotent without collapsing distinct evidence.
func capabilityObservationRecords(p Provider, m Model, profile ModelCapabilityProfile, result CapabilityProbeResult, completedAt time.Time) ([]CapabilityObservationRecord, error) {
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	records := make([]CapabilityObservationRecord, 0, len(result.Observations))
	for _, observation := range result.Observations {
		feature := strings.TrimSpace(observation.Feature)
		if feature == "" {
			return nil, fmt.Errorf("能力 observation 缺少 feature")
		}
		observedAt := observation.ObservedAt
		if observedAt.IsZero() {
			observedAt = completedAt
		}
		reason := sanitizeCapabilityObservationReason(observation.Reason)
		if len(reason) > maxCapabilityObservationReason {
			reason = reason[:maxCapabilityObservationReason]
		}
		route := strings.TrimSpace(observation.Route)
		if route == "" {
			route = strings.TrimSpace(profile.Route)
		}
		source := strings.TrimSpace(observation.Source)
		if source == "" {
			source = "probe"
		}
		record := CapabilityObservationRecord{
			ProviderID: p.ID, ModelID: m.ID, Protocol: normalizeProtocol(p.Protocol), Route: route,
			Feature: feature, State: observation.State, Source: source, Confidence: observation.Confidence,
			Reason: reason, ObservedAt: observedAt.UTC(), CreatedAt: completedAt.UTC(),
		}
		record.ID = capabilityObservationID(record)
		if err := record.Validate(); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func sanitizeCapabilityObservationReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	// safeProbeError applies the same Bearer/secret-field redaction as the
	// synchronous probe result. Wrapping the reason as an error keeps this
	// helper independent of any provider-specific response type.
	return safeProbeError(errors.New(reason))
}

func capabilityObservationID(record CapabilityObservationRecord) string {
	basis := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%.6f\x00%s\x00%s", record.ProviderID, record.ModelID, record.Protocol, record.Route, record.Feature, record.State, record.Confidence, record.Reason, record.ObservedAt.UTC().Format(time.RFC3339Nano))
	sum := sha256.Sum256([]byte(basis))
	return "capobs:" + hex.EncodeToString(sum[:])[:40]
}

func normalizeCapabilityObservationLimit(limit int) int {
	if limit <= 0 {
		return defaultCapabilityObservationLimit
	}
	if limit > maxCapabilityObservationLimit {
		return maxCapabilityObservationLimit
	}
	return limit
}

// NormalizeCapabilityObservationLimit exposes the same bound to storage
// implementations without making the internal default part of their policy.
func NormalizeCapabilityObservationLimit(limit int) int {
	return normalizeCapabilityObservationLimit(limit)
}
