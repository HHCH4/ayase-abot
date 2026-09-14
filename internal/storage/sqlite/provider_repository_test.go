package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Abot/internal/provider"
)

func TestProviderRepositoryPersistsConfigModelsAndDefaults(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	repo := store.ProviderRepository()
	item := provider.Provider{
		ID: "demo", Name: "演示供应商", BaseURL: "http://127.0.0.1:9999/v1",
		Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatResponses, TokenCountProtocol: provider.TokenCountProtocolAnthropic,
		APIKey: "secret-key", Status: provider.StatusConfigured,
		Models: []provider.Model{{ID: "demo-model", DisplayName: "演示模型", Enabled: true, Source: "manual", ContextWindow: 8192}},
	}
	if err := repo.Save(context.Background(), item); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	if err := repo.SetDefault(context.Background(), provider.DefaultRef{ProviderID: "demo", ModelID: "demo-model"}); err != nil {
		t.Fatalf("保存默认模型失败: %v", err)
	}

	got, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatalf("读取供应商失败: %v", err)
	}
	if got.APIKey != "secret-key" || got.OpenAIFormat != provider.OpenAIFormatResponses || got.TokenCountProtocol != provider.TokenCountProtocolAnthropic || len(got.Models) != 1 {
		t.Fatalf("读取后的配置不完整: %#v", got)
	}
	if got.Models[0].ContextWindow != 8192 {
		t.Fatalf("模型上下文窗口 = %d，期望 8192", got.Models[0].ContextWindow)
	}

	defaults, err := repo.GetDefault(context.Background())
	if err != nil {
		t.Fatalf("读取默认模型失败: %v", err)
	}
	if defaults.ProviderID != "demo" || defaults.ModelID != "demo-model" {
		t.Fatalf("默认模型引用不正确: %#v", defaults)
	}

	if err := repo.Delete(context.Background(), "demo"); err != nil {
		t.Fatalf("删除供应商失败: %v", err)
	}
	defaults, err = repo.GetDefault(context.Background())
	if err != nil {
		t.Fatalf("删除后读取默认模型失败: %v", err)
	}
	if defaults != (provider.DefaultRef{}) {
		t.Fatalf("删除供应商后默认引用仍存在: %#v", defaults)
	}
}

func TestProviderRepositoryPersistsCapabilityObservationHistory(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()
	repo := store.ProviderRepository()
	if err := repo.Save(context.Background(), provider.Provider{ID: "probe", Name: "探测", BaseURL: "http://127.0.0.1:9999/v1", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	observationRepo, ok := repo.(provider.CapabilityObservationRepository)
	if !ok {
		t.Fatal("SQLite provider repository 未实现 observation 历史")
	}
	base := time.Date(2026, time.September, 12, 4, 0, 0, 0, time.UTC)
	items := make([]provider.CapabilityObservationRecord, 0, 3)
	for index, state := range []provider.SupportState{provider.SupportUnknown, provider.SupportSupported, provider.SupportDegraded} {
		observation := provider.CapabilityObservationRecord{
			ID:         "capobs-test-" + string(rune('a'+index)),
			ProviderID: "probe", ModelID: "model", Protocol: provider.ProtocolOpenAICompatible, Route: "chat",
			Feature: "streaming", State: state, Source: "probe", Confidence: 0.5, Reason: "bounded reason",
			ObservedAt: base.Add(time.Duration(index) * time.Minute), CreatedAt: base.Add(time.Duration(index) * time.Minute),
		}
		items = append(items, observation)
	}
	if err := observationRepo.SaveCapabilityObservations(context.Background(), items); err != nil {
		t.Fatalf("保存 observation 历史失败: %v", err)
	}
	if err := observationRepo.SaveCapabilityObservations(context.Background(), items[:1]); err != nil {
		t.Fatalf("重复保存 observation 不应失败: %v", err)
	}
	got, err := observationRepo.ListCapabilityObservations(context.Background(), "probe", "model", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].State != provider.SupportDegraded || got[1].State != provider.SupportSupported {
		t.Fatalf("observation 历史应按最新时间倒序且受 limit 限制: %#v", got)
	}
	if err := repo.Delete(context.Background(), "probe"); err != nil {
		t.Fatal(err)
	}
	got, err = observationRepo.ListCapabilityObservations(context.Background(), "probe", "model", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("删除 provider 后 observation 不应残留: got=%#v err=%v", got, err)
	}
}

func TestProviderRepositoryPersistsTokenizerCalibration(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.ProviderRepository()
	profile := provider.DefaultCapabilities(provider.Provider{ID: "calibration", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatChat}, provider.Model{ID: "model"})
	profile.Route = string(provider.OpenAIFormatChat)
	profile.Tokenizer.Calibration = provider.TokenCalibration{
		Samples: 3, EstimatedTokens: 30, ActualTokens: 36, ErrorTokens: 6, AbsoluteErrorTokens: 6,
		SafetyMultiplier: 1.32, Source: "runtime_usage", UpdatedAt: time.Now().UTC(),
	}
	profile.Tokenizer.Quality = "calibrated"
	item := provider.Provider{ID: "calibration", Name: "校准", BaseURL: "http://127.0.0.1:9999/v1", Protocol: provider.ProtocolOpenAICompatible, OpenAIFormat: provider.OpenAIFormatChat, Models: []provider.Model{{ID: "model", Enabled: true, Capabilities: &profile}}}
	if err := repo.Save(context.Background(), item); err != nil {
		t.Fatalf("保存 tokenizer 校准 profile 失败: %v", err)
	}
	got, err := repo.Get(context.Background(), item.ID)
	if err != nil || len(got.Models) != 1 || got.Models[0].Capabilities == nil {
		t.Fatalf("读取 tokenizer 校准 profile 失败: %#v err=%v", got, err)
	}
	calibration := got.Models[0].Capabilities.Tokenizer.Calibration
	if calibration.Samples != 3 || calibration.EstimatedTokens != 30 || calibration.ActualTokens != 36 || calibration.SafetyMultiplier != 1.32 || got.Models[0].Capabilities.Tokenizer.Quality != "calibrated" {
		t.Fatalf("SQLite 未保留 tokenizer 校准元数据: %#v", got.Models[0].Capabilities.Tokenizer)
	}
}

func TestProviderRepositoryPrunesCapabilityObservationHistory(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.ProviderRepository()
	if err := repo.Save(context.Background(), provider.Provider{ID: "probe", Name: "探测", BaseURL: "http://127.0.0.1:9999/v1", Protocol: provider.ProtocolOpenAICompatible, Models: []provider.Model{{ID: "model", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	observationRepo, ok := repo.(provider.CapabilityObservationRepository)
	if !ok {
		t.Fatal("SQLite provider repository 未实现 observation 历史")
	}
	base := time.Date(2026, time.September, 12, 5, 0, 0, 0, time.UTC)
	items := make([]provider.CapabilityObservationRecord, 0, provider.MaxCapabilityObservationHistory+1)
	for index := 0; index <= provider.MaxCapabilityObservationHistory; index++ {
		when := base.Add(time.Duration(index) * time.Second)
		items = append(items, provider.CapabilityObservationRecord{
			ID: "capobs-prune-" + fmt.Sprint(index), ProviderID: "probe", ModelID: "model", Protocol: provider.ProtocolOpenAICompatible,
			Feature: "streaming", State: provider.SupportUnknown, Source: "probe", ObservedAt: when, CreatedAt: when,
		})
	}
	if err := observationRepo.SaveCapabilityObservations(context.Background(), items); err != nil {
		t.Fatalf("保存 observation 历史失败: %v", err)
	}
	got, err := observationRepo.ListCapabilityObservations(context.Background(), "probe", "model", provider.MaxCapabilityObservationHistory)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != provider.MaxCapabilityObservationHistory || got[0].ID != "capobs-prune-"+fmt.Sprint(provider.MaxCapabilityObservationHistory) {
		t.Fatalf("observation 历史应保留最新 %d 条: len=%d first=%#v", provider.MaxCapabilityObservationHistory, len(got), got[:minObservationTest(len(got), 1)])
	}
}

func minObservationTest(value, limit int) int {
	if value < limit {
		return value
	}
	return limit
}
