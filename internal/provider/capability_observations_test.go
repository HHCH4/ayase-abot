package provider

import (
	"strings"
	"testing"
	"time"
)

func TestCapabilityObservationRecordsAreBoundedAndSanitized(t *testing.T) {
	completed := time.Date(2026, time.September, 12, 4, 0, 0, 0, time.UTC)
	p := Provider{ID: "probe", Protocol: ProtocolOpenAICompatible}
	m := Model{ID: "model"}
	profile := DefaultCapabilities(p, m)
	result := CapabilityProbeResult{
		Profile: profile,
		Observations: []CapabilityObservation{
			{Feature: "streaming", State: SupportSupported, Source: "probe", Confidence: 0.95, Reason: `HTTP 401: {"api_key":"secret","authorization":"Bearer token"}`, ObservedAt: completed},
		},
	}
	records, err := capabilityObservationRecords(p, m, profile, result, completed)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID == "" || records[0].ProviderID != "probe" || records[0].ModelID != "model" {
		t.Fatalf("observation record identity 不正确: %#v", records)
	}
	if strings.Contains(records[0].Reason, "secret") || strings.Contains(records[0].Reason, "Bearer token") || len(records[0].Reason) > maxCapabilityObservationReason {
		t.Fatalf("observation reason 泄露或超限: %q", records[0].Reason)
	}
	if err := records[0].Validate(); err != nil {
		t.Fatalf("生成的 observation 不应验证失败: %v", err)
	}

	second, err := capabilityObservationRecords(p, m, profile, result, completed)
	if err != nil || len(second) != 1 || second[0].ID != records[0].ID {
		t.Fatalf("相同 evidence 应生成幂等 ID: first=%#v second=%#v err=%v", records, second, err)
	}
}

func TestNormalizeCapabilityObservationLimit(t *testing.T) {
	if got := NormalizeCapabilityObservationLimit(0); got != defaultCapabilityObservationLimit {
		t.Fatalf("默认 observation limit=%d", got)
	}
	if got := NormalizeCapabilityObservationLimit(maxCapabilityObservationLimit + 1); got != maxCapabilityObservationLimit {
		t.Fatalf("过大 observation limit=%d", got)
	}
}
