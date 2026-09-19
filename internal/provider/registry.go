package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
)

type modelCacheKey struct {
	ProviderID string
	ModelID    string
}

// Registry 是供应商运行时注册表；保存配置后立即刷新，不需要重启进程。
type Registry struct {
	repo          Repository
	mu            sync.RWMutex
	calibrationMu sync.Mutex
	providers     map[string]Provider
	defaults      DefaultRef
	adapters      map[Protocol]Adapter
	models        map[modelCacheKey]adkmodel.LLM
}

// NewRegistry 从 SQLite 恢复供应商、模型目录和默认模型。
func NewRegistry(ctx context.Context, repo Repository, adapters map[Protocol]Adapter) (*Registry, error) {
	if repo == nil {
		return nil, fmt.Errorf("供应商仓储不能为空")
	}
	r := &Registry{
		repo:      repo,
		providers: make(map[string]Provider),
		adapters:  make(map[Protocol]Adapter),
		models:    make(map[modelCacheKey]adkmodel.LLM),
	}
	for protocol, adapter := range adapters {
		if adapter != nil {
			r.adapters[protocol] = adapter
		}
	}
	if err := r.Refresh(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Refresh 重新从唯一事实来源加载配置，并清理所有旧模型实例。
func (r *Registry) Refresh(ctx context.Context) error {
	providers, err := r.repo.List(ctx)
	if err != nil {
		return fmt.Errorf("加载供应商配置失败: %w", err)
	}
	defaults, err := r.repo.GetDefault(ctx)
	if err != nil {
		return fmt.Errorf("加载默认模型失败: %w", err)
	}
	items := make(map[string]Provider, len(providers))
	for _, item := range providers {
		item.Protocol = normalizeProtocol(item.Protocol)
		items[item.ID] = cloneProvider(item)
	}
	// 兼容旧版本把默认模型只保存到 settings、没有写入模型目录的配置。
	// 先把这个稳定引用补回目录，后续 WebUI 才能为它展示能力选择，运行时也不会
	// 因为缺少目录元数据而把图片能力误判成未知。这里只补默认引用，不猜测任何能力。
	if defaults.ProviderID != "" && defaults.ModelID != "" {
		if item, ok := items[defaults.ProviderID]; ok && modelByID(item.Models, defaults.ModelID).ID == "" {
			item.Models = append(item.Models, Model{
				ID: defaults.ModelID, DisplayName: defaults.ModelID, Enabled: true, Source: "manual",
			})
			if err := item.Validate(); err != nil {
				return fmt.Errorf("迁移旧默认模型 %q 失败: %w", defaults.ModelID, err)
			}
			if err := r.repo.Save(ctx, item); err != nil {
				return fmt.Errorf("保存迁移后的默认模型目录失败: %w", err)
			}
			items[item.ID] = cloneProvider(item)
		}
	}
	r.mu.Lock()
	r.providers = items
	r.defaults = defaults
	r.models = make(map[modelCacheKey]adkmodel.LLM)
	r.mu.Unlock()
	return nil
}

// List 返回副本，调用方可以安全地用于 JSON 序列化。
func (r *Registry) List() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]Provider, 0, len(r.providers))
	for _, item := range r.providers {
		items = append(items, cloneProvider(item))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// CapabilityProfiles returns the current catalog as a deterministic,
// metadata-only projection suitable for a capability release candidate. It
// never performs provider calls and never exposes provider credentials.
func (r *Registry) CapabilityProfiles() ([]ModelCapabilityProfile, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: Registry 为空", ErrInvalidCapabilityProfile)
	}
	return BuildCapabilityProfiles(r.List())
}

// Get 返回一个供应商副本。
func (r *Registry) Get(id string) (Provider, error) {
	r.mu.RLock()
	item, ok := r.providers[strings.TrimSpace(id)]
	r.mu.RUnlock()
	if !ok {
		return Provider{}, ErrNotFound
	}
	return cloneProvider(item), nil
}

// Default 返回全局默认模型引用。
func (r *Registry) Default() DefaultRef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaults
}

// Save 保存供应商配置并热更新注册表。
func (r *Registry) Save(ctx context.Context, req SaveRequest) (Provider, error) {
	candidate := cloneProvider(req.Provider)
	candidate.ID = strings.TrimSpace(candidate.ID)
	candidate.Name = strings.TrimSpace(candidate.Name)
	candidate.BaseURL = strings.TrimSpace(candidate.BaseURL)
	candidate.Protocol = normalizeProtocol(candidate.Protocol)
	candidate.TokenCountProtocol = TokenCountProtocol(strings.TrimSpace(string(candidate.TokenCountProtocol)))
	for index := range candidate.Models {
		candidate.Models[index].ID = strings.TrimSpace(candidate.Models[index].ID)
		candidate.Models[index].DisplayName = strings.TrimSpace(candidate.Models[index].DisplayName)
		if candidate.Models[index].DisplayName == "" {
			candidate.Models[index].DisplayName = candidate.Models[index].ID
		}
		candidate.Models[index].Source = strings.TrimSpace(candidate.Models[index].Source)
		if candidate.Models[index].Source == "" {
			candidate.Models[index].Source = "manual"
		}
	}
	// Validate 会在值拷贝上补默认格式，因此这里要先把默认值写回待保存实体。
	if candidate.Protocol == ProtocolOpenAICompatible && candidate.OpenAIFormat == "" {
		candidate.OpenAIFormat = OpenAIFormatAuto
	}
	var old Provider
	oldLoaded := false
	if previous, oldErr := r.Get(candidate.ID); oldErr == nil {
		old = previous
		oldLoaded = true
	} else if oldErr != ErrNotFound && req.APIKey == nil {
		return Provider{}, oldErr
	}
	if req.APIKey != nil {
		candidate.APIKey = strings.TrimSpace(*req.APIKey)
	} else {
		if oldLoaded {
			candidate.APIKey = old.APIKey
		}
	}
	if oldLoaded {
		for index := range candidate.Models {
			if candidate.Models[index].Capabilities != nil {
				continue
			}
			previous := modelByID(old.Models, candidate.Models[index].ID)
			if previous.Capabilities != nil && capabilityTargetUnchanged(old, candidate) && modelCapabilityTargetUnchanged(previous, candidate.Models[index]) {
				profile := *previous.Capabilities
				candidate.Models[index].Capabilities = &profile
			}
		}
	}
	if candidate.Status == "" {
		candidate.Status = StatusConfigured
	}
	if err := candidate.Validate(); err != nil {
		return Provider{}, err
	}
	if err := r.repo.Save(ctx, candidate); err != nil {
		return Provider{}, fmt.Errorf("保存供应商失败: %w", err)
	}
	if err := r.Refresh(ctx); err != nil {
		return Provider{}, err
	}
	return r.Get(candidate.ID)
}

func capabilityTargetUnchanged(previous, current Provider) bool {
	return previous.BaseURL == current.BaseURL && previous.APIKey == current.APIKey && normalizeProtocol(previous.Protocol) == normalizeProtocol(current.Protocol) && previous.OpenAIFormat == current.OpenAIFormat && previous.TokenCountProtocol == current.TokenCountProtocol
}

func modelCapabilityTargetUnchanged(previous, current Model) bool {
	return previous.ID == current.ID && previous.Enabled == current.Enabled && previous.ContextWindow == current.ContextWindow && previous.MaxOutputTokens == current.MaxOutputTokens
}

// Delete 删除供应商；仓储会同步清理失效的默认引用。
func (r *Registry) Delete(ctx context.Context, id string) error {
	if _, err := r.Get(id); err != nil {
		return err
	}
	if err := r.repo.Delete(ctx, strings.TrimSpace(id)); err != nil {
		return fmt.Errorf("删除供应商失败: %w", err)
	}
	return r.Refresh(ctx)
}

// SetDefault 设置全局默认供应商和模型。默认模型必须来自已启用的供应商目录，
// 这样配置页、能力覆盖和运行时解析使用的是同一个模型引用。
func (r *Registry) SetDefault(ctx context.Context, ref DefaultRef) error {
	ref.ProviderID = strings.TrimSpace(ref.ProviderID)
	ref.ModelID = strings.TrimSpace(ref.ModelID)
	if ref.ProviderID == "" || ref.ModelID == "" {
		return fmt.Errorf("%w: 默认供应商和模型不能为空", ErrInvalidRequest)
	}
	item, err := r.Get(ref.ProviderID)
	if err != nil {
		return err
	}
	model := modelByID(item.Models, ref.ModelID)
	if model.ID == "" || !model.Enabled {
		return fmt.Errorf("%w: 默认模型必须来自已启用的供应商模型目录", ErrNoModel)
	}
	if err := r.repo.SetDefault(ctx, ref); err != nil {
		return fmt.Errorf("保存默认模型失败: %w", err)
	}
	return r.Refresh(ctx)
}

// TestConnection 调用协议适配器的探测接口，并把结果写回状态字段。
func (r *Registry) TestConnection(ctx context.Context, id string) (ProbeResult, error) {
	p, err := r.Get(id)
	if err != nil {
		return ProbeResult{}, err
	}
	adapter, ok := r.adapter(p.Protocol)
	if !ok {
		return ProbeResult{}, fmt.Errorf("协议 %q 未注册适配器", p.Protocol)
	}
	result, probeErr := adapter.TestConnection(ctx, p)
	if probeErr != nil {
		p.Status = StatusError
		p.StatusMessage = trimError(probeErr)
	} else {
		now := timeNow()
		p.Status = StatusReady
		p.StatusMessage = result.Message
		p.LastCheckedAt = &now
	}
	// 状态写回失败不能覆盖原始探测错误，但成功探测时必须让 WebUI 看见最新状态。
	if saveErr := r.repo.Save(ctx, p); saveErr != nil && probeErr == nil {
		return ProbeResult{}, fmt.Errorf("保存连接状态失败: %w", saveErr)
	}
	// 探测失败时也要刷新内存状态，让 WebUI 能立即看到 error 状态。
	if refreshErr := r.Refresh(ctx); refreshErr != nil && probeErr == nil {
		return ProbeResult{}, refreshErr
	} else if refreshErr != nil {
		// 原始探测错误更有诊断价值，刷新失败不覆盖它。
		return result, probeErr
	}
	if probeErr != nil {
		return result, probeErr
	}
	return result, nil
}

// CountTokens delegates a materialized request to an optional provider-side
// count endpoint. The registry resolves the adapter from the same normalized
// protocol used by BuildModel, so a count cannot accidentally target a
// different provider or model route. Unsupported adapters return the stable
// ErrTokenCounterUnavailable sentinel and callers should retain heuristic
// estimates.
func (r *Registry) CountTokens(ctx context.Context, p Provider, m Model, request *adkmodel.LLMRequest) (TokenCount, error) {
	if r == nil {
		return TokenCount{}, ErrTokenCounterUnavailable
	}
	adapter, ok := r.adapter(p.Protocol)
	if !ok {
		return TokenCount{}, ErrTokenCounterUnavailable
	}
	counter, ok := adapter.(TokenCounter)
	if !ok {
		return TokenCount{}, ErrTokenCounterUnavailable
	}
	count, err := counter.CountTokens(ctx, p, m, request)
	if err != nil {
		return TokenCount{}, err
	}
	if count.Tokens < 0 {
		return TokenCount{}, fmt.Errorf("%w: provider 返回负 token 数", ErrInvalidRequest)
	}
	if strings.TrimSpace(count.Name) == "" || strings.TrimSpace(count.Version) == "" {
		return TokenCount{}, fmt.Errorf("%w: provider token count 缺少 estimator metadata", ErrInvalidRequest)
	}
	if strings.TrimSpace(count.Source) == "" {
		count.Source = "provider"
	}
	if strings.TrimSpace(count.Quality) == "" {
		count.Quality = "provider"
	}
	if !count.Exact {
		return TokenCount{}, fmt.Errorf("%w: provider token count 未声明 exact", ErrInvalidRequest)
	}
	return count, nil
}

// DiscoverModels 探测模型目录并与手动目录合并。
func (r *Registry) DiscoverModels(ctx context.Context, id string) ([]Model, error) {
	p, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	adapter, ok := r.adapter(p.Protocol)
	if !ok {
		return nil, fmt.Errorf("协议 %q 未注册适配器", p.Protocol)
	}
	discovered, err := adapter.DiscoverModels(ctx, p)
	if err != nil {
		p.Status = StatusError
		p.StatusMessage = trimError(err)
		_ = r.repo.Save(ctx, p)
		_ = r.Refresh(ctx)
		return nil, err
	}
	merged := mergeModels(p.Models, discovered)
	p.Models = merged
	p.Status = StatusReady
	p.StatusMessage = fmt.Sprintf("已探测 %d 个模型", len(discovered))
	now := timeNow()
	p.LastCheckedAt = &now
	if err := r.repo.Save(ctx, p); err != nil {
		return nil, fmt.Errorf("保存模型目录失败: %w", err)
	}
	if err := r.Refresh(ctx); err != nil {
		return nil, err
	}
	return merged, nil
}

// DiscoverModelsForProvider 只探测传入的临时配置，不保存供应商或模型目录。
// 新增供应商还没有 ID 和名称时，使用内部占位值完成上游地址和协议校验。
func (r *Registry) DiscoverModelsForProvider(ctx context.Context, p Provider) ([]Model, error) {
	candidate := cloneProvider(p)
	candidate.ID = "preview"
	candidate.Name = "preview"
	candidate.Models = nil
	candidate.Protocol = normalizeProtocol(candidate.Protocol)
	if candidate.Protocol == ProtocolOpenAICompatible && candidate.OpenAIFormat == "" {
		candidate.OpenAIFormat = OpenAIFormatAuto
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	adapter, ok := r.adapter(candidate.Protocol)
	if !ok {
		return nil, fmt.Errorf("协议 %q 未注册适配器", candidate.Protocol)
	}
	return adapter.DiscoverModels(ctx, candidate)
}

// ApplyCapabilityOverrides 保存保守的模型级能力覆盖；注册表统一写入，保证内存缓存和持久化目录同时刷新。
func (r *Registry) ApplyCapabilityOverrides(ctx context.Context, providerID, modelID string, overrides CapabilityOverrides) (Model, error) {
	return r.applyCapabilityOverrides(ctx, providerID, modelID, overrides, false)
}

// ApplyManualCapabilityOverrides 保存管理员明确选择的模型能力；它与运行时保守接口分开，避免普通路径伪造探测证据。
func (r *Registry) ApplyManualCapabilityOverrides(ctx context.Context, providerID, modelID string, overrides CapabilityOverrides) (Model, error) {
	return r.applyCapabilityOverrides(ctx, providerID, modelID, overrides, true)
}

// applyCapabilityOverrides 统一完成模型查找、profile 更新和目录持久化，避免两个入口出现不同的保存行为。
func (r *Registry) applyCapabilityOverrides(ctx context.Context, providerID, modelID string, overrides CapabilityOverrides, manual bool) (Model, error) {
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	item, err := r.Get(providerID)
	if err != nil {
		return Model{}, err
	}
	for index := range item.Models {
		if item.Models[index].ID != modelID {
			continue
		}
		current := item.Models[index]
		profile := current.Capabilities
		if profile == nil {
			value := DefaultCapabilities(item, current)
			profile = &value
		}
		var effective ModelCapabilityProfile
		var applyErr error
		if manual {
			effective, applyErr = ApplyManualCapabilityOverrides(*profile, overrides)
		} else {
			effective, applyErr = ApplyCapabilityOverrides(*profile, overrides)
		}
		if applyErr != nil {
			return Model{}, applyErr
		}
		current.Capabilities = &effective
		item.Models[index] = current
		if _, saveErr := r.Save(ctx, SaveRequest{Provider: item}); saveErr != nil {
			return Model{}, saveErr
		}
		return current, nil
	}
	return Model{}, ErrNoModel
}

// ProbeCapabilities runs a bounded, metadata-only capability probe for one
// catalog model and persists the resulting profile. The adapter owns the
// protocol-specific model construction; the registry remains the single
// writer for the durable catalog and never stores prompts or provider output.
func (r *Registry) ProbeCapabilities(ctx context.Context, providerID, modelID string, options CapabilityProbeOptions) (CapabilityProbeResult, error) {
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	p, err := r.Get(providerID)
	if err != nil {
		return CapabilityProbeResult{}, err
	}
	index := -1
	for i := range p.Models {
		if p.Models[i].ID == modelID {
			index = i
			break
		}
	}
	if index < 0 {
		return CapabilityProbeResult{}, ErrNoModel
	}
	adapter, ok := r.adapter(p.Protocol)
	if !ok {
		return CapabilityProbeResult{}, fmt.Errorf("协议 %q 未注册适配器", p.Protocol)
	}
	prober, ok := adapter.(CapabilityProber)
	if !ok {
		return CapabilityProbeResult{}, ErrCapabilityProbeUnavailable
	}
	result, probeErr := prober.ProbeCapabilities(ctx, p, p.Models[index], options)
	if probeErr != nil {
		return result, probeErr
	}
	completedAt := result.CompletedAt
	if completedAt.IsZero() {
		completedAt = timeNow()
		result.CompletedAt = completedAt
	}
	if result.StartedAt.IsZero() || result.StartedAt.After(completedAt) {
		result.StartedAt = completedAt
	}
	// Do not let a slow probe overwrite a provider route/model edit made while
	// it was running. A concurrent capability override is merged below so the
	// explicit user safety decision remains authoritative.
	latest, err := r.Get(providerID)
	if err != nil {
		return CapabilityProbeResult{}, err
	}
	latestIndex := -1
	for i := range latest.Models {
		if latest.Models[i].ID == modelID {
			latestIndex = i
			break
		}
	}
	if latestIndex < 0 || !sameProbeTarget(p, p.Models[index], latest, latest.Models[latestIndex]) {
		return CapabilityProbeResult{}, ErrCapabilitySnapshotConflict
	}
	profile := result.Profile
	// An adapter may omit identity fields when it starts from a hand-written
	// model entry, but it must never be able to persist a profile for another
	// catalog item.
	if profile.ProviderID == "" {
		profile.ProviderID = p.ID
	}
	if profile.ModelID == "" {
		profile.ModelID = modelID
	}
	if profile.Protocol == "" {
		profile.Protocol = p.Protocol
	}
	if strings.TrimSpace(profile.SourceRevision) == "" {
		profile.SourceRevision = probeSourceRevision("catalog", completedAt)
	}
	if profile.UpdatedAt.IsZero() {
		profile.UpdatedAt = completedAt
	}
	if profile.ProviderID != p.ID || profile.ModelID != modelID || normalizeProtocol(profile.Protocol) != normalizeProtocol(p.Protocol) {
		return CapabilityProbeResult{}, fmt.Errorf("%w: 探测结果身份与模型目录不匹配", ErrInvalidCapabilityProfile)
	}
	if err := profile.Validate(); err != nil {
		return CapabilityProbeResult{}, err
	}
	currentProfile := latest.Models[latestIndex].Capabilities
	if currentProfile != nil {
		currentSnapshot := currentProfile.SnapshotID()
		initialProfile := p.Models[index].Capabilities
		if initialProfile == nil {
			initial := DefaultCapabilities(p, p.Models[index])
			initialProfile = &initial
		}
		if currentSnapshot != initialProfile.SnapshotID() {
			merged := *currentProfile
			for _, observation := range result.Observations {
				if observation.Feature == "token_count" && observation.State == SupportSupported && result.Profile.Tokenizer.Known {
					// Tokenizer metadata is richer than the generic Support fields;
					// carry the successful probe result across a concurrent
					// conservative override instead of silently dropping it.
					merged.Tokenizer = result.Profile.Tokenizer
				}
				applyProbeSupport(&merged, observation.Feature, observation.State, observation.Confidence, observation.Reason, observation.ObservedAt)
			}
			merged.UsageDetails = appendUnique(merged.UsageDetails, result.Profile.UsageDetails...)
			merged.SourceRevision = probeSourceRevision(merged.SourceRevision, completedAt)
			merged.UpdatedAt = completedAt
			profile = merged
		}
	}
	// Concurrent merge applies observations to the latest profile, so validate
	// the merged value as well; a malformed adapter result or stale catalog
	// value must never reach durable storage through this path.
	if err := profile.Validate(); err != nil {
		return CapabilityProbeResult{}, err
	}
	result.Profile = profile
	latest.Models[latestIndex].Capabilities = &profile
	latest.Status = StatusReady
	latest.StatusMessage = result.Message
	now := completedAt
	latest.LastCheckedAt = &now
	if err := r.repo.Save(ctx, latest); err != nil {
		return CapabilityProbeResult{}, fmt.Errorf("保存模型能力探测结果失败: %w", err)
	}
	if observationRepo, ok := r.repo.(CapabilityObservationRepository); ok && len(result.Observations) > 0 {
		records, recordErr := capabilityObservationRecords(p, p.Models[index], profile, result, completedAt)
		if recordErr != nil {
			_ = r.Refresh(ctx)
			return CapabilityProbeResult{}, fmt.Errorf("整理模型能力 observation 失败: %w", recordErr)
		}
		if saveErr := observationRepo.SaveCapabilityObservations(ctx, records); saveErr != nil {
			_ = r.Refresh(ctx)
			return CapabilityProbeResult{}, fmt.Errorf("保存模型能力 observation 失败: %w", saveErr)
		}
	}
	if err := r.Refresh(ctx); err != nil {
		return CapabilityProbeResult{}, err
	}
	return result, nil
}

// ListCapabilityObservations returns bounded historical evidence for one
// catalog model. The Registry checks the catalog first so a caller cannot use
// this endpoint to enumerate observations for a deleted or unknown model.
func (r *Registry) ListCapabilityObservations(ctx context.Context, providerID, modelID string, limit int) ([]CapabilityObservationRecord, error) {
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	p, err := r.Get(providerID)
	if err != nil {
		return nil, err
	}
	if modelByID(p.Models, modelID).ID != modelID {
		return nil, ErrNoModel
	}
	observationRepo, ok := r.repo.(CapabilityObservationRepository)
	if !ok {
		return nil, ErrCapabilityObservationUnavailable
	}
	return observationRepo.ListCapabilityObservations(ctx, providerID, modelID, normalizeCapabilityObservationLimit(limit))
}

func sameProbeTarget(start Provider, startModel Model, latest Provider, latestModel Model) bool {
	return start.ID == latest.ID && start.BaseURL == latest.BaseURL &&
		normalizeProtocol(start.Protocol) == normalizeProtocol(latest.Protocol) &&
		start.OpenAIFormat == latest.OpenAIFormat && start.TokenCountProtocol == latest.TokenCountProtocol && start.APIKey == latest.APIKey &&
		startModel.ID == latestModel.ID && startModel.Enabled == latestModel.Enabled &&
		startModel.ContextWindow == latestModel.ContextWindow && startModel.MaxOutputTokens == latestModel.MaxOutputTokens
}

// Resolve 根据会话级引用或全局默认值创建/取得 ADK 模型。
func (r *Registry) Resolve(ctx context.Context, providerID, modelID string) (ResolvedModel, error) {
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	r.mu.RLock()
	defaults := r.defaults
	providers := cloneProvidersFromMap(r.providers)
	r.mu.RUnlock()

	if providerID == "" {
		if modelID != "" {
			providerID = findProviderForModel(providers, modelID)
		}
		if providerID == "" {
			providerID = defaults.ProviderID
		}
	}
	p, ok := providerFromSnapshot(providers, providerID)
	if !ok {
		if providerID == "" {
			return ResolvedModel{}, ErrNoDefault
		}
		return ResolvedModel{}, ErrNotFound
	}
	if modelID == "" {
		if defaults.ProviderID == p.ID && defaults.ModelID != "" {
			modelID = defaults.ModelID
		} else {
			modelID = firstEnabledModel(p.Models)
		}
	}
	if modelID == "" {
		return ResolvedModel{}, fmt.Errorf("供应商 %q: %w", p.ID, ErrNoModel)
	}
	adapter, ok := r.adapter(p.Protocol)
	if !ok {
		return ResolvedModel{}, fmt.Errorf("协议 %q 未注册适配器", p.Protocol)
	}
	modelMeta := modelByID(p.Models, modelID)
	key := modelCacheKey{ProviderID: p.ID, ModelID: modelID}
	r.mu.RLock()
	cached := r.models[key]
	r.mu.RUnlock()
	if cached != nil {
		return ResolvedModel{Provider: p, Model: ensureCapabilities(p, modelMetaOrDefault(modelMeta, modelID)), LLM: cached}, nil
	}
	llm, err := adapter.BuildModel(ctx, p, modelID)
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("创建 %s/%s 模型失败: %w", p.ID, modelID, err)
	}
	if llm == nil {
		return ResolvedModel{}, fmt.Errorf("创建 %s/%s 模型失败: 适配器返回空模型", p.ID, modelID)
	}
	r.mu.Lock()
	if existing := r.models[key]; existing != nil {
		cached = existing
	} else {
		r.models[key] = llm
		cached = llm
	}
	r.mu.Unlock()
	return ResolvedModel{Provider: p, Model: ensureCapabilities(p, modelMetaOrDefault(modelMeta, modelID)), LLM: cached}, nil
}

func ensureCapabilities(p Provider, m Model) Model {
	if m.Capabilities == nil {
		profile := DefaultCapabilities(p, m)
		m.Capabilities = &profile
	} else {
		profile := NormalizeCapabilityProfile(*m.Capabilities)
		m.Capabilities = &profile
	}
	return m
}

func (r *Registry) adapter(protocol Protocol) (Adapter, bool) {
	protocol = normalizeProtocol(protocol)
	r.mu.RLock()
	adapter, ok := r.adapters[protocol]
	r.mu.RUnlock()
	return adapter, ok
}

// normalizeProtocol 让旧配置名和当前 WebUI 名称共享同一套适配器。
func normalizeProtocol(protocol Protocol) Protocol {
	if protocol == ProtocolOpenAICompletions {
		return ProtocolOpenAICompatible
	}
	return protocol
}

func providerFromSnapshot(items []Provider, id string) (Provider, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return Provider{}, false
}

func cloneProvidersFromMap(items map[string]Provider) []Provider {
	result := make([]Provider, 0, len(items))
	for _, item := range items {
		result = append(result, cloneProvider(item))
	}
	return result
}

func findProviderForModel(items []Provider, modelID string) string {
	var found string
	for _, item := range items {
		if modelByID(item.Models, modelID).ID == modelID {
			if found != "" {
				return ""
			}
			found = item.ID
		}
	}
	return found
}

func firstEnabledModel(items []Model) string {
	for _, item := range items {
		if item.Enabled {
			return item.ID
		}
	}
	return ""
}

func modelByID(items []Model, id string) Model {
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	return Model{}
}

func modelMetaOrDefault(item Model, id string) Model {
	if item.ID == "" {
		return Model{ID: id, DisplayName: id, Enabled: true, Source: "manual"}
	}
	return item
}

func mergeModels(existing, discovered []Model) []Model {
	byID := make(map[string]Model, len(existing)+len(discovered))
	for _, item := range existing {
		if item.ID != "" {
			byID[item.ID] = item
		}
	}
	for _, item := range discovered {
		if item.ID == "" {
			continue
		}
		old, exists := byID[item.ID]
		if exists {
			item.Enabled = old.Enabled
			if old.Source == "manual" {
				item.Source = old.Source
			}
		}
		if item.Source == "" {
			item.Source = "discovered"
		}
		byID[item.ID] = item
	}
	result := make([]Model, 0, len(byID))
	for _, item := range byID {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func trimError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 500 {
		return message[:500]
	}
	return message
}

// 单独封装时间，便于测试稳定地替换状态时间。
var timeNow = func() time.Time { return time.Now().UTC() }
