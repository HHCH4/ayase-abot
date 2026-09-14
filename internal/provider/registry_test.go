package provider

import (
	"context"
	"errors"
	"iter"
	"sort"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
)

func TestRegistrySaveResolveAndHotUpdate(t *testing.T) {
	repo := newTestRepository()
	adapter := &testAdapter{}
	registry, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{
		ProtocolOpenAICompatible: adapter,
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}

	saved, err := registry.Save(context.Background(), SaveRequest{
		Provider: Provider{
			ID:       " demo-provider ",
			Name:     "演示供应商",
			BaseURL:  " http://127.0.0.1:9999/v1 ",
			Protocol: ProtocolOpenAICompatible,
			Models: []Model{{
				ID: " demo-model ", Source: "", DisplayName: "", Enabled: true, ContextWindow: 4096,
			}},
		},
		APIKey: stringPointer("first-key"),
	})
	if err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	if saved.ID != "demo-provider" || saved.BaseURL != "http://127.0.0.1:9999/v1" || saved.OpenAIFormat != OpenAIFormatAuto {
		t.Fatalf("保存后配置没有标准化: %#v", saved)
	}
	if saved.Models[0].ID != "demo-model" || saved.Models[0].DisplayName != "demo-model" || saved.Models[0].Source != "manual" {
		t.Fatalf("保存后模型没有标准化: %#v", saved.Models)
	}

	// API 编辑时不传 api_key，旧密钥必须继续可用。
	saved, err = registry.Save(context.Background(), SaveRequest{
		Provider: Provider{
			ID: "demo-provider", Name: "演示供应商（更新）", BaseURL: "http://127.0.0.1:9999/v1",
			Protocol: ProtocolOpenAICompatible, Models: []Model{{ID: "second-model", Enabled: true}},
		},
	})
	if err != nil {
		t.Fatalf("更新供应商失败: %v", err)
	}
	if saved.APIKey != "first-key" {
		t.Fatalf("编辑未传 api_key 时密钥 = %q，期望保留旧密钥", saved.APIKey)
	}

	if err := registry.SetDefault(context.Background(), DefaultRef{ProviderID: "demo-provider", ModelID: "second-model"}); err != nil {
		t.Fatalf("设置默认模型失败: %v", err)
	}
	resolved, err := registry.Resolve(context.Background(), "", "")
	if err != nil {
		t.Fatalf("解析默认模型失败: %v", err)
	}
	if resolved.Model.ID != "second-model" || resolved.LLM.Name() != "second-model" {
		t.Fatalf("默认模型解析结果不正确: %#v", resolved)
	}
	if adapter.builds != 1 {
		t.Fatalf("首次解析 Build 次数 = %d，期望 1", adapter.builds)
	}
	if _, err := registry.Resolve(context.Background(), "demo-provider", "second-model"); err != nil {
		t.Fatalf("缓存模型解析失败: %v", err)
	}
	if adapter.builds != 1 {
		t.Fatalf("缓存命中后 Build 次数 = %d，期望仍为 1", adapter.builds)
	}

	// 保存新配置会清空运行时模型缓存，确保热更新后不会继续使用旧实例。
	if _, err := registry.Save(context.Background(), SaveRequest{
		Provider: Provider{ID: "demo-provider", Name: "演示供应商（再次更新）", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, Models: []Model{{ID: "second-model", Enabled: true}}},
	}); err != nil {
		t.Fatalf("热更新供应商失败: %v", err)
	}
	if _, err := registry.Resolve(context.Background(), "demo-provider", "second-model"); err != nil {
		t.Fatalf("热更新后模型解析失败: %v", err)
	}
	if adapter.builds != 2 {
		t.Fatalf("热更新后 Build 次数 = %d，期望 2", adapter.builds)
	}
}

func TestRegistryProbeFailureRefreshesErrorStatus(t *testing.T) {
	repo := newTestRepository()
	adapter := &testAdapter{probeErr: errors.New("上游不可用")}
	registry, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{
		ProtocolOpenAICompatible: adapter,
	})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "demo", Name: "演示", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
	}}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	if _, err := registry.TestConnection(context.Background(), "demo"); err == nil {
		t.Fatal("连接失败时应返回错误")
	}
	item, err := registry.Get("demo")
	if err != nil {
		t.Fatalf("读取探测状态失败: %v", err)
	}
	if item.Status != StatusError || item.StatusMessage != "上游不可用" {
		t.Fatalf("探测失败状态 = %#v", item)
	}
}

func TestRegistryPersistsConservativeCapabilityOverride(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "caps", Name: "能力", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	degraded := SupportDegraded
	model, err := registry.ApplyCapabilityOverrides(context.Background(), "caps", "model", CapabilityOverrides{ToolCalling: &degraded})
	if err != nil {
		t.Fatalf("保存能力 override 失败: %v", err)
	}
	if model.Capabilities == nil || model.Capabilities.ToolCalling.State != SupportDegraded {
		t.Fatalf("override 结果不正确: %#v", model.Capabilities)
	}
	stored, err := registry.Get("caps")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Models[0].Capabilities == nil || stored.Models[0].Capabilities.ToolCalling.Source != "user_override" {
		t.Fatalf("override 未持久化: %#v", stored.Models[0].Capabilities)
	}
}

func TestRegistryProbeCapabilitiesPersistsProfile(t *testing.T) {
	repo := newTestRepository()
	adapter := &capabilityProbeAdapter{model: &probeTestLLM{}}
	registry, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "probe", Name: "探测", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	result, err := registry.ProbeCapabilities(context.Background(), "probe", "model", CapabilityProbeOptions{IncludeStreaming: false, IncludeToolCalling: false, IncludeStructuredJSON: false})
	if err != nil {
		t.Fatalf("运行模型能力探测失败: %v", err)
	}
	if result.Profile.ModelID != "model" || result.Profile.ProviderID != "probe" {
		t.Fatalf("探测结果身份不正确: %#v", result.Profile)
	}
	stored, err := registry.Get("probe")
	if err != nil {
		t.Fatalf("读取探测后的供应商失败: %v", err)
	}
	if stored.Models[0].Capabilities == nil || stored.Models[0].Capabilities.SourceRevision == "" {
		t.Fatalf("探测 profile 未持久化: %#v", stored.Models)
	}
	if stored.Status != StatusReady || stored.LastCheckedAt == nil {
		t.Fatalf("探测成功后的供应商状态不正确: %#v", stored)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "probe", Name: "探测（编辑）", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("编辑供应商失败: %v", err)
	}
	stored, err = registry.Get("probe")
	if err != nil || stored.Models[0].Capabilities == nil {
		t.Fatalf("普通供应商编辑不应丢失能力 profile: %#v err=%v", stored.Models, err)
	}
}

func TestRegistrySaveInvalidatesCapabilityProfileWhenRouteChanges(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	profile := DefaultCapabilities(Provider{ID: "route", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat}, Model{ID: "model"})
	profile.Route = string(OpenAIFormatChat)
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "route", Name: "线路", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat,
		Models: []Model{{ID: "model", Enabled: true, Capabilities: &profile}},
	}}); err != nil {
		t.Fatalf("保存 Chat profile 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "route", Name: "线路（Responses）", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatResponses,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("切换 Responses 线路失败: %v", err)
	}
	stored, err := registry.Get("route")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Models[0].Capabilities != nil {
		t.Fatalf("线路变化时不应沿用旧能力 profile: %#v", stored.Models[0].Capabilities)
	}
}

func TestRegistrySaveInvalidatesCapabilityProfileWhenTokenCountRouteChanges(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	profile := DefaultCapabilities(Provider{ID: "route-count", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat}, Model{ID: "model"})
	profile.Route = string(OpenAIFormatChat)
	profile.Tokenizer = OpenAIInputTokenCounterProfile()
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "route-count", Name: "计数线路", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat,
		Models: []Model{{ID: "model", Enabled: true, Capabilities: &profile}},
	}}); err != nil {
		t.Fatalf("保存初始计数 profile 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "route-count", Name: "计数线路（Anthropic）", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat,
		TokenCountProtocol: TokenCountProtocolAnthropic, Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("切换 Anthropic 计数线路失败: %v", err)
	}
	stored, err := registry.Get("route-count")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Models[0].Capabilities != nil {
		t.Fatalf("计数线路变化时不应沿用旧 tokenizer profile: %#v", stored.Models[0].Capabilities)
	}
}

func TestRegistryRejectsUnknownTokenCountProtocol(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	_, err = registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "invalid-count", Name: "非法计数线路", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		TokenCountProtocol: TokenCountProtocol("unknown"), Models: []Model{{ID: "model", Enabled: true}},
	}})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("未知 token count 协议应返回 ErrInvalidRequest: %v", err)
	}
}

func TestSameProbeTargetIncludesTokenCountProtocol(t *testing.T) {
	base := Provider{ID: "probe", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat, TokenCountProtocol: TokenCountProtocolAnthropic, APIKey: "key"}
	model := Model{ID: "model", Enabled: true}
	changed := base
	changed.TokenCountProtocol = ""
	if sameProbeTarget(base, model, changed, model) {
		t.Fatal("token count 协议变化必须使能力 probe target 失效")
	}
}

func TestRegistryProbeCapabilitiesRequiresAdapterSupport(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{ProtocolOpenAICompatible: &testAdapter{}})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "probe", Name: "探测", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	if _, err := registry.ProbeCapabilities(context.Background(), "probe", "model", CapabilityProbeOptions{}); !errors.Is(err, ErrCapabilityProbeUnavailable) {
		t.Fatalf("不支持探测的适配器应返回稳定错误，得到 %v", err)
	}
}

func TestRegistryProbeCapabilitiesMergesConcurrentConservativeOverride(t *testing.T) {
	repo := newTestRepository()
	model := &probeTestLLM{started: make(chan struct{}), release: make(chan struct{})}
	registry, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{ProtocolOpenAICompatible: &capabilityProbeAdapter{model: model}})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "probe", Name: "探测", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	probeDone := make(chan error, 1)
	go func() {
		_, probeErr := registry.ProbeCapabilities(context.Background(), "probe", "model", CapabilityProbeOptions{IncludeStreaming: false, IncludeToolCalling: false, IncludeStructuredJSON: false})
		probeDone <- probeErr
	}()
	<-model.started
	unsupported := SupportUnsupported
	if _, err := registry.ApplyCapabilityOverrides(context.Background(), "probe", "model", CapabilityOverrides{StructuredOutput: &unsupported}); err != nil {
		t.Fatalf("并发保存 conservative override 失败: %v", err)
	}
	close(model.release)
	if err := <-probeDone; err != nil {
		t.Fatalf("合并并发 probe 失败: %v", err)
	}
	stored, err := registry.Get("probe")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Models[0].Capabilities == nil || stored.Models[0].Capabilities.StructuredOutput.Source != "user_override" || stored.Models[0].Capabilities.StructuredOutput.State != SupportUnsupported {
		t.Fatalf("并发 probe 不应覆盖 override: %#v", stored.Models[0].Capabilities)
	}
}

func TestRegistryProbeCapabilitiesMergesTokenCountTokenizerAcrossConcurrentOverride(t *testing.T) {
	repo := newTestRepository()
	model := &probeTestLLM{started: make(chan struct{}), release: make(chan struct{})}
	registry, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{ProtocolOpenAICompatible: &capabilityProbeAdapter{model: model, tokenCount: true}})
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "probe-tokenizer", Name: "探测 tokenizer", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	probeDone := make(chan error, 1)
	go func() {
		_, probeErr := registry.ProbeCapabilities(context.Background(), "probe-tokenizer", "model", CapabilityProbeOptions{ExplicitOptionalSelection: true, IncludeTokenCount: true})
		probeDone <- probeErr
	}()
	<-model.started
	unsupported := SupportUnsupported
	if _, err := registry.ApplyCapabilityOverrides(context.Background(), "probe-tokenizer", "model", CapabilityOverrides{StructuredOutput: &unsupported}); err != nil {
		t.Fatalf("并发保存 conservative override 失败: %v", err)
	}
	close(model.release)
	if err := <-probeDone; err != nil {
		t.Fatalf("合并 token-count probe 失败: %v", err)
	}
	stored, err := registry.Get("probe-tokenizer")
	if err != nil {
		t.Fatal(err)
	}
	profile := stored.Models[0].Capabilities
	if profile == nil || !profile.Tokenizer.Known || profile.Tokenizer.Name != "gateway-counter" || profile.StructuredOutput.Source != "user_override" {
		t.Fatalf("并发 merge 不应丢失 tokenizer 或 override: %#v", profile)
	}
}

func TestRegistryContextUsageCalibrationRequiresSamplesAndOnlyAddsSafety(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "calibration", Name: "校准", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	for i := 0; i < MinTokenizerCalibrationSamples-1; i++ {
		if err := registry.RecordContextUsageCalibration(context.Background(), "calibration", "model", 10, 20); err != nil {
			t.Fatalf("记录第 %d 个 token 样本失败: %v", i+1, err)
		}
	}
	profile := mustRegistryProfile(t, registry, "calibration", "model")
	if profile.Tokenizer.Calibration.Samples != MinTokenizerCalibrationSamples-1 || profile.Tokenizer.Calibration.SafetyMultiplier != 1 {
		t.Fatalf("未达样本门槛时不应调参: %#v", profile.Tokenizer)
	}
	if err := registry.RecordContextUsageCalibration(context.Background(), "calibration", "model", 10, 20); err != nil {
		t.Fatalf("记录达到门槛的 token 样本失败: %v", err)
	}
	profile = mustRegistryProfile(t, registry, "calibration", "model")
	if profile.Tokenizer.Calibration.Samples != MinTokenizerCalibrationSamples || profile.Tokenizer.Calibration.SafetyMultiplier <= 1 || profile.Tokenizer.Quality != "calibrated" {
		t.Fatalf("达到门槛后应启用有界保守校准: %#v", profile.Tokenizer)
	}
	previous := profile.Tokenizer.Calibration.SafetyMultiplier
	if err := registry.RecordContextUsageCalibration(context.Background(), "calibration", "model", 100, 1); err != nil {
		t.Fatalf("记录低于估算的样本失败: %v", err)
	}
	profile = mustRegistryProfile(t, registry, "calibration", "model")
	if profile.Tokenizer.Calibration.SafetyMultiplier != previous {
		t.Fatalf("实际 token 较低时不应缩小安全倍数: before=%v after=%v", previous, profile.Tokenizer.Calibration.SafetyMultiplier)
	}
}

func TestRegistryCountTokensUsesOptionalAdapterAndValidatesMetadata(t *testing.T) {
	repo := newTestRepository()
	adapter := &tokenCountAdapter{count: func(_ context.Context, p Provider, m Model, request *adkmodel.LLMRequest) (TokenCount, error) {
		if p.ID != "count" || m.ID != "model" || request == nil {
			t.Fatalf("token counter 收到错误目标: p=%#v m=%#v request=%#v", p, m, request)
		}
		return TokenCount{Tokens: 42, Name: "provider-count", Version: "v1", Source: "provider", Quality: "provider", Exact: true}, nil
	}}
	registry, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{ProtocolOpenAICompatible: adapter})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{ID: "count", Name: "计数", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, Models: []Model{{ID: "model", Enabled: true}}}}); err != nil {
		t.Fatal(err)
	}
	got, err := registry.CountTokens(context.Background(), Provider{ID: "count", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"}, &adkmodel.LLMRequest{Model: "model"})
	if err != nil || got.Tokens != 42 || !got.Exact {
		t.Fatalf("Registry token counter 结果不正确: %#v err=%v", got, err)
	}
	noCounter, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{ProtocolOpenAICompatible: &testAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noCounter.CountTokens(context.Background(), Provider{ID: "count", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"}, &adkmodel.LLMRequest{}); !errors.Is(err, ErrTokenCounterUnavailable) {
		t.Fatalf("无 token counter 适配器应返回稳定错误: %v", err)
	}
	bad := &tokenCountAdapter{count: func(context.Context, Provider, Model, *adkmodel.LLMRequest) (TokenCount, error) {
		return TokenCount{Tokens: 1, Name: "", Version: "v1", Exact: true}, nil
	}}
	badRegistry, err := NewRegistry(context.Background(), repo, map[Protocol]Adapter{ProtocolOpenAICompatible: bad})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badRegistry.CountTokens(context.Background(), Provider{ID: "count", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"}, &adkmodel.LLMRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("缺少 estimator metadata 应拒绝: %v", err)
	}
}

func TestRegistryContextUsageCalibrationRejectsInvalidAndBoundsSamples(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "calibration", Name: "校准", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible,
		Models: []Model{{ID: "model", Enabled: true}},
	}}); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	if err := registry.RecordContextUsageCalibration(context.Background(), "calibration", "model", -1, 2); !errors.Is(err, ErrInvalidTokenCalibration) {
		t.Fatalf("负 token 数应返回稳定错误: %v", err)
	}
	if err := registry.RecordContextUsageCalibration(context.Background(), "calibration", "model", 1, -1); !errors.Is(err, ErrInvalidTokenCalibration) {
		t.Fatalf("负实际 token 数应返回稳定错误: %v", err)
	}
	for i := 0; i < MaxTokenizerCalibrationSamples+10; i++ {
		if err := registry.RecordContextUsageCalibration(context.Background(), "calibration", "model", 1, int(MaxTokenizerSafetyMultiplier*10)); err != nil {
			t.Fatalf("记录边界样本失败: %v", err)
		}
	}
	profile := mustRegistryProfile(t, registry, "calibration", "model")
	if profile.Tokenizer.Calibration.Samples != MaxTokenizerCalibrationSamples || profile.Tokenizer.Calibration.SafetyMultiplier != MaxTokenizerSafetyMultiplier {
		t.Fatalf("样本和倍数未被正确封顶: %#v", profile.Tokenizer.Calibration)
	}
}

func TestRegistryRuntimeCapabilityEvidenceUsesRouteAndFailureThreshold(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	profile := DefaultCapabilities(Provider{ID: "runtime", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat}, Model{ID: "model"})
	profile.Route = string(OpenAIFormatChat)
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "runtime", Name: "运行证据", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatChat,
		Models: []Model{{ID: "model", Enabled: true, Capabilities: &profile}},
	}}); err != nil {
		t.Fatalf("保存运行证据模型失败: %v", err)
	}
	base := time.Now().UTC()
	for i := 0; i < RuntimeCapabilityFailureThreshold; i++ {
		if err := registry.RecordRuntimeCapabilityObservations(context.Background(), "runtime", "model", []CapabilityObservation{{Feature: "tool_calling", State: SupportUnsupported, Confidence: 0.8, Route: string(OpenAIFormatChat), Reason: "stable unsupported response", ObservedAt: base.Add(time.Duration(i) * time.Second)}}); err != nil {
			t.Fatalf("保存第 %d 条运行拒绝失败: %v", i+1, err)
		}
	}
	if err := registry.ReconcileRuntimeCapabilityEvidence(context.Background(), "runtime", "model"); err != nil {
		t.Fatalf("合并运行证据失败: %v", err)
	}
	updated := mustRegistryProfile(t, registry, "runtime", "model")
	if updated.ToolCalling.State != SupportUnsupported || updated.ToolCalling.Source != "runtime_threshold" {
		t.Fatalf("达到阈值后应降级为 unsupported: %#v", updated.ToolCalling)
	}
	if err := registry.RecordRuntimeCapabilityObservations(context.Background(), "runtime", "model", []CapabilityObservation{{Feature: "tool_calling", State: SupportSupported, Confidence: 0.9, Route: string(OpenAIFormatResponses), Reason: "另一线路成功", ObservedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("保存另一线路证据失败: %v", err)
	}
	if err := registry.ReconcileRuntimeCapabilityEvidence(context.Background(), "runtime", "model"); err != nil {
		t.Fatalf("合并另一线路证据失败: %v", err)
	}
	updated = mustRegistryProfile(t, registry, "runtime", "model")
	if updated.Route != string(OpenAIFormatChat) || updated.ToolCalling.State != SupportUnsupported {
		t.Fatalf("线路不匹配的证据不应改变当前 profile: %#v", updated)
	}
	history, err := repo.ListCapabilityObservations(context.Background(), "runtime", "model", MaxCapabilityObservationHistory)
	if err != nil || len(history) != RuntimeCapabilityFailureThreshold+1 {
		t.Fatalf("运行证据历史应保留不同线路记录: len=%d err=%v", len(history), err)
	}
	for _, item := range history {
		if item.Source != "runtime" || strings.Contains(item.Reason, "secret") {
			t.Fatalf("运行证据元数据不正确或泄密: %#v", item)
		}
	}
}

func TestRegistryRuntimeCapabilityEvidencePreservesUserOverride(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	unsupported := SupportUnsupported
	profile := DefaultCapabilities(Provider{ID: "runtime-override", Protocol: ProtocolOpenAICompatible}, Model{ID: "model"})
	profile.ToolCalling = Support{State: SupportUnsupported, Source: "user_override", Confidence: 1}
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{ID: "runtime-override", Name: "运行证据 override", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, Models: []Model{{ID: "model", Enabled: true, Capabilities: &profile}}}}); err != nil {
		t.Fatalf("保存 override profile 失败: %v", err)
	}
	if err := registry.RecordRuntimeCapabilityEvidence(context.Background(), "runtime-override", "model", []CapabilityObservation{{Feature: "tool_calling", State: SupportSupported, Confidence: 1, Reason: "runtime success"}}); err != nil {
		t.Fatalf("记录运行证据失败: %v", err)
	}
	updated := mustRegistryProfile(t, registry, "runtime-override", "model")
	if updated.ToolCalling.State != unsupported || updated.ToolCalling.Source != "user_override" {
		t.Fatalf("运行证据不应覆盖 user override: %#v", updated.ToolCalling)
	}
}

func TestRegistryRuntimeSchemaEvidenceTracksRouteDialectAndThreshold(t *testing.T) {
	repo := newTestRepository()
	registry, err := NewRegistry(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("创建 Registry 失败: %v", err)
	}
	profile := DefaultCapabilities(Provider{ID: "runtime-schema", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatResponses}, Model{ID: "model"})
	profile.Route = string(OpenAIFormatResponses)
	if _, err := registry.Save(context.Background(), SaveRequest{Provider: Provider{
		ID: "runtime-schema", Name: "schema 运行证据", BaseURL: "http://127.0.0.1:9999/v1", Protocol: ProtocolOpenAICompatible, OpenAIFormat: OpenAIFormatResponses,
		Models: []Model{{ID: "model", Enabled: true, Capabilities: &profile}},
	}}); err != nil {
		t.Fatalf("保存 schema 运行证据模型失败: %v", err)
	}
	if err := registry.RecordRuntimeCapabilityEvidence(context.Background(), "runtime-schema", "model", []CapabilityObservation{{Feature: "structured_output_schema", State: SupportSupported, Confidence: 0.8, Route: string(OpenAIFormatResponses), Reason: "schema success", ObservedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("记录 schema 成功证据失败: %v", err)
	}
	updated := mustRegistryProfile(t, registry, "runtime-schema", "model")
	if updated.StructuredOutputSchema.State != SupportSupported || updated.JSONSchemaDialect != "openai-responses-json-schema" {
		t.Fatalf("schema 成功证据应绑定 responses dialect: %#v", updated)
	}
	base := time.Now().UTC()
	for i := 0; i < RuntimeCapabilityFailureThreshold; i++ {
		if err := registry.RecordRuntimeCapabilityObservations(context.Background(), "runtime-schema", "model", []CapabilityObservation{{Feature: "structured_output_schema", State: SupportUnsupported, Confidence: 0.8, Route: string(OpenAIFormatResponses), Reason: "schema rejected", ObservedAt: base.Add(time.Duration(i+1) * time.Second)}}); err != nil {
			t.Fatalf("记录 schema 拒绝证据失败: %v", err)
		}
	}
	if err := registry.ReconcileRuntimeCapabilityEvidence(context.Background(), "runtime-schema", "model"); err != nil {
		t.Fatalf("合并 schema 拒绝证据失败: %v", err)
	}
	updated = mustRegistryProfile(t, registry, "runtime-schema", "model")
	if updated.StructuredOutputSchema.State != SupportUnsupported || updated.JSONSchemaDialect != "" {
		t.Fatalf("达到阈值后 schema 应 unsupported 且清除 dialect: %#v", updated)
	}
}

func mustRegistryProfile(t *testing.T, registry *Registry, providerID, modelID string) ModelCapabilityProfile {
	t.Helper()
	item, err := registry.Get(providerID)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range item.Models {
		if model.ID == modelID && model.Capabilities != nil {
			return *model.Capabilities
		}
	}
	t.Fatalf("模型 %s/%s 缺少能力 profile", providerID, modelID)
	return ModelCapabilityProfile{}
}

type testRepository struct {
	providers    map[string]Provider
	defaults     DefaultRef
	observations map[string][]CapabilityObservationRecord
}

func newTestRepository() *testRepository {
	return &testRepository{providers: make(map[string]Provider), observations: make(map[string][]CapabilityObservationRecord)}
}

func (r *testRepository) List(context.Context) ([]Provider, error) {
	result := make([]Provider, 0, len(r.providers))
	for _, item := range r.providers {
		result = append(result, cloneProvider(item))
	}
	return result, nil
}

func (r *testRepository) Get(_ context.Context, id string) (Provider, error) {
	item, ok := r.providers[id]
	if !ok {
		return Provider{}, ErrNotFound
	}
	return cloneProvider(item), nil
}

func (r *testRepository) Save(_ context.Context, item Provider) error {
	r.providers[item.ID] = cloneProvider(item)
	return nil
}

func (r *testRepository) Delete(_ context.Context, id string) error {
	if _, ok := r.providers[id]; !ok {
		return ErrNotFound
	}
	delete(r.providers, id)
	for key := range r.observations {
		if strings.HasPrefix(key, id+"\x00") {
			delete(r.observations, key)
		}
	}
	if r.defaults.ProviderID == id {
		r.defaults = DefaultRef{}
	}
	return nil
}

func (r *testRepository) GetDefault(context.Context) (DefaultRef, error) { return r.defaults, nil }

func (r *testRepository) SetDefault(_ context.Context, ref DefaultRef) error {
	r.defaults = ref
	return nil
}

func (r *testRepository) SaveCapabilityObservations(_ context.Context, items []CapabilityObservationRecord) error {
	for _, item := range items {
		if err := item.Validate(); err != nil {
			return err
		}
		key := item.ProviderID + "\x00" + item.ModelID
		found := false
		for index := range r.observations[key] {
			if r.observations[key][index].ID == item.ID {
				r.observations[key][index] = item
				found = true
				break
			}
		}
		if !found {
			r.observations[key] = append(r.observations[key], item)
		}
	}
	return nil
}

func (r *testRepository) ListCapabilityObservations(_ context.Context, providerID, modelID string, limit int) ([]CapabilityObservationRecord, error) {
	items := append([]CapabilityObservationRecord(nil), r.observations[strings.TrimSpace(providerID)+"\x00"+strings.TrimSpace(modelID)]...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].ObservedAt.After(items[j].ObservedAt) })
	limit = NormalizeCapabilityObservationLimit(limit)
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

type testAdapter struct {
	builds   int
	probeErr error
}

type tokenCountAdapter struct {
	testAdapter
	count tokenCountFunc
}

type tokenCountFunc func(context.Context, Provider, Model, *adkmodel.LLMRequest) (TokenCount, error)

func (a *tokenCountAdapter) CountTokens(ctx context.Context, p Provider, m Model, request *adkmodel.LLMRequest) (TokenCount, error) {
	if a.count == nil {
		return TokenCount{}, ErrTokenCounterUnavailable
	}
	return a.count(ctx, p, m, request)
}

type capabilityProbeAdapter struct {
	model      adkmodel.LLM
	tokenCount bool
}

func (a *capabilityProbeAdapter) BuildModel(context.Context, Provider, string) (adkmodel.LLM, error) {
	return a.model, nil
}

func (a *capabilityProbeAdapter) TestConnection(context.Context, Provider) (ProbeResult, error) {
	return ProbeResult{Message: "ok"}, nil
}

func (a *capabilityProbeAdapter) DiscoverModels(context.Context, Provider) ([]Model, error) {
	return nil, nil
}

func (a *capabilityProbeAdapter) ProbeCapabilities(ctx context.Context, p Provider, m Model, options CapabilityProbeOptions) (CapabilityProbeResult, error) {
	profile := DefaultCapabilities(p, m)
	profile.ToolCalling = Support{State: SupportUnknown, Source: "catalog"}
	profile.Streaming = Support{State: SupportUnknown, Source: "catalog"}
	profile.StructuredOutput = Support{State: SupportUnknown, Source: "catalog"}
	profile.Route = "chat"
	result, err := RunCapabilityProbe(ctx, a.model, profile, options)
	if err != nil || !a.tokenCount || !options.IncludeTokenCount {
		return result, err
	}
	ApplyTokenCountProbe(&result, profile.Route, TokenCount{
		Tokens: 13, Name: "gateway-counter", Version: "v1", Source: "provider", Quality: "provider", Exact: true,
	}, nil)
	return result, nil
}

func (a *testAdapter) BuildModel(context.Context, Provider, string) (adkmodel.LLM, error) {
	a.builds++
	return testLLM{name: "second-model"}, nil
}

func (a *testAdapter) TestConnection(context.Context, Provider) (ProbeResult, error) {
	if a.probeErr != nil {
		return ProbeResult{}, a.probeErr
	}
	return ProbeResult{Message: "连接正常"}, nil
}

func (a *testAdapter) DiscoverModels(context.Context, Provider) ([]Model, error) { return nil, nil }

type testLLM struct{ name string }

func (m testLLM) Name() string { return m.name }

func (m testLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{}, nil)
	}
}

func stringPointer(value string) *string { return &value }
