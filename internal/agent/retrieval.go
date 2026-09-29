package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// RetrievalSourceKind 是检索适配器的受控来源类型。来源权限由 Runtime 在
// 创建请求时冻结，模型不能在查询参数中扩展 source_scope。
type RetrievalSourceKind string

const (
	RetrievalSourceMemory       RetrievalSourceKind = "memory"
	RetrievalSourceKnowledge    RetrievalSourceKind = "knowledge"
	RetrievalSourceConversation RetrievalSourceKind = "conversation"
	RetrievalSourceWeb          RetrievalSourceKind = "web"
	RetrievalSourceWorkspace    RetrievalSourceKind = "workspace"
	RetrievalSourceStructured   RetrievalSourceKind = "structured"
)

// RetrievalRequest 是所有检索来源共用的有界请求契约。
type RetrievalRequest struct {
	Query string `json:"query"`
	// InvocationID 只用于把检索阶段投影到父 Runtime 事件流，不参与缓存键，
	// 也不会进入适配器的持久化数据。
	InvocationID   string                `json:"-"`
	QueryType      string                `json:"query_type,omitempty"`
	UserID         string                `json:"user_id"`
	ConversationID string                `json:"conversation_id,omitempty"`
	SessionID      string                `json:"session_id,omitempty"`
	SourceKinds    []RetrievalSourceKind `json:"source_kinds"`
	SourceIDs      []string              `json:"source_ids,omitempty"`
	Scope          []string              `json:"scope,omitempty"`
	Filters        map[string]string     `json:"filters,omitempty"`
	TopK           int                   `json:"top_k"`
	RerankEnabled  bool                  `json:"rerank_enabled"`
	Freshness      string                `json:"freshness,omitempty"`
	MaxQueryCount  int                   `json:"max_query_count"`
	MaxResultBytes int64                 `json:"max_result_bytes"`
	// WebSearch* 是父 Runtime 冻结到本次检索的额度策略；来源适配器不得
	// 自行恢复全局默认值，否则多来源并行时会绕过调用级预算。
	WebSearchDailyCallLimit        int           `json:"web_search_daily_call_limit,omitempty"`
	WebSearchMaxCallsPerInvocation int           `json:"web_search_max_calls_per_invocation,omitempty"`
	WebSearchAlertPercent          int           `json:"web_search_alert_percent,omitempty"`
	Deadline                       time.Time     `json:"deadline,omitempty"`
	ParentRunID                    string        `json:"parent_run_id,omitempty"`
	RequestedBy                    string        `json:"requested_by,omitempty"`
	PermissionDigest               string        `json:"permission_digest,omitempty"`
	CacheTTL                       time.Duration `json:"-"`
}

// RetrievalSourceFailure 保留来源失败可见性，但只保存有界错误摘要。
type RetrievalSourceFailure struct {
	SourceKind string `json:"source_kind"`
	SourceID   string `json:"source_id,omitempty"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
}

// RetrievalResult 是多来源检索的结构化结果，不把无来源的字符串直接交给主 Agent。
type RetrievalResult struct {
	RequestID        string                   `json:"request_id"`
	Items            []EvidenceItem           `json:"items"`
	Failures         []RetrievalSourceFailure `json:"failures,omitempty"`
	SourceCounts     map[string]int           `json:"source_counts,omitempty"`
	QueryDigest      string                   `json:"query_digest"`
	PermissionDigest string                   `json:"permission_digest,omitempty"`
	Cached           bool                     `json:"cached"`
	Partial          bool                     `json:"partial"`
	RetrievedAt      time.Time                `json:"retrieved_at"`
}

// RetrievalSourceAdapter 是记忆、知识库、会话、网页、工作区和结构化数据的
// 唯一接入接口。适配器不能发送平台消息、改变权限或返回未带来源的模型猜测。
type RetrievalSourceAdapter interface {
	Kind() RetrievalSourceKind
	Search(context.Context, RetrievalRequest) ([]EvidenceItem, error)
}

// RetrievalAccessValidator 是缓存命中前的权限复核边界。缓存只保存短期证据，
// 但不能把上一次查询时的授权假设当成当前授权；没有复核实现的来源不会命中缓存。
type RetrievalAccessValidator interface {
	ValidateAccess(context.Context, RetrievalRequest) error
}

// RetrievalEventSink 将检索阶段投影到父 Runtime 的事件流；只发送 digest、计数
// 和来源状态，避免把原始查询、网页正文或凭据写入任务事件。
type RetrievalEventSink func(context.Context, string, string, map[string]any) error

// RetrievalOrchestratorOptions 是检索计划的硬边界。
type RetrievalOrchestratorOptions struct {
	MaxSources       int
	MaxQueries       int
	MaxCandidates    int
	MaxEvidenceBytes int64
	MaxConcurrency   int
	DefaultCacheTTL  time.Duration
	EventSink        RetrievalEventSink
}

type retrievalCacheEntry struct {
	result    RetrievalResult
	request   RetrievalRequest
	expiresAt time.Time
}

// RetrievalOrchestrator 负责有界并行、权限复核、去重、确定性重排、引用和缓存。
// 它不自行调用模型；需要 AI 汇总时由父 Agent 再显式调用通用子 Agent。
type RetrievalOrchestrator struct {
	mu       sync.RWMutex
	options  RetrievalOrchestratorOptions
	adapters map[RetrievalSourceKind]RetrievalSourceAdapter
	cache    map[string]retrievalCacheEntry
}

func NewRetrievalOrchestrator(options RetrievalOrchestratorOptions) *RetrievalOrchestrator {
	if options.MaxSources <= 0 || options.MaxSources > 6 {
		options.MaxSources = 6
	}
	if options.MaxQueries <= 0 || options.MaxQueries > 24 {
		options.MaxQueries = 24
	}
	if options.MaxCandidates <= 0 || options.MaxCandidates > 20 {
		options.MaxCandidates = 20
	}
	if options.MaxEvidenceBytes <= 0 || options.MaxEvidenceBytes > 128<<10 {
		options.MaxEvidenceBytes = 128 << 10
	}
	if options.MaxConcurrency <= 0 || options.MaxConcurrency > 4 {
		options.MaxConcurrency = 4
	}
	if options.DefaultCacheTTL <= 0 {
		options.DefaultCacheTTL = 30 * time.Second
	}
	return &RetrievalOrchestrator{options: options, adapters: make(map[RetrievalSourceKind]RetrievalSourceAdapter), cache: make(map[string]retrievalCacheEntry)}
}

// RegisterAdapter 注册一个受控来源；同一来源只能存在一个适配器，避免模型
// 通过重复注册绕过权限和用量限制。
func (o *RetrievalOrchestrator) RegisterAdapter(adapter RetrievalSourceAdapter) error {
	if o == nil || adapter == nil {
		return errors.New("检索适配器不能为空")
	}
	kind := adapter.Kind()
	if !validRetrievalSourceKind(kind) {
		return fmt.Errorf("不支持的检索来源: %s", kind)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.adapters[kind]; exists {
		return fmt.Errorf("检索来源已注册: %s", kind)
	}
	o.adapters[kind] = adapter
	return nil
}

// Search 执行一个无环、只读、多来源检索计划。没有任何可信证据时返回
// subagent_no_evidence，部分来源失败时保留已成功结果和失败列表。
func (o *RetrievalOrchestrator) Search(ctx context.Context, request RetrievalRequest) (RetrievalResult, error) {
	if o == nil {
		return RetrievalResult{}, &SubAgentRuntimeError{Code: "subagent_retrieval_adapter_unavailable", HumanMessage: "检索编排器不可用", Retryable: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request = normalizeRetrievalRequest(request, o.options)
	if err := validateRetrievalRequest(request, o.options); err != nil {
		return RetrievalResult{}, err
	}
	now := time.Now().UTC()
	searchCtx := ctx
	if !request.Deadline.IsZero() {
		if !request.Deadline.After(now) {
			return RetrievalResult{}, &SubAgentRuntimeError{Code: "subagent_timeout", HumanMessage: "检索预算期限已到", Retryable: false}
		}
		// Deadline 是父任务预算的一部分；检索层只继承它，不再叠加固定的
		// 短超时，避免大文件或慢速只读来源被错误截断。
		var cancel context.CancelFunc
		searchCtx, cancel = context.WithDeadline(ctx, request.Deadline)
		defer cancel()
	}
	o.emitEvent(searchCtx, request, "retrieval.requested", map[string]any{
		"source_kinds": retrievalSourceKindNames(request.SourceKinds), "top_k": request.TopK,
		"query_digest": digestSubAgentText(request.Query),
	})
	cacheKey := retrievalCacheKey(request)
	// 缓存命中前必须重新调用每个来源的权限复核接口。仅靠 user_id 或
	// source_scope 不能证明当前 ACL 没有变化；复核失败就丢弃缓存并回源。
	o.mu.RLock()
	cached, cacheHit := o.cache[cacheKey]
	o.mu.RUnlock()
	if cacheHit && cached.expiresAt.After(now) {
		if err := o.validateCachedAccess(searchCtx, request); err == nil {
			result := cloneRetrievalResult(cached.result)
			result.Cached = true
			o.emitEvent(searchCtx, request, "retrieval.summarized", map[string]any{
				"cached": true, "item_count": len(result.Items), "failure_count": len(result.Failures),
			})
			return result, nil
		} else {
			// 权限复核失败时丢弃缓存并重新走来源；来源本身会给出稳定的
			// 拒绝码，不能把旧证据继续交给模型。
			o.emitEvent(searchCtx, request, "retrieval.stale", map[string]any{"reason": "permission_recheck_failed"})
		}
	}
	if cacheHit && !cached.expiresAt.After(now) {
		o.emitEvent(searchCtx, request, "retrieval.stale", map[string]any{"reason": "cache_expired"})
		o.mu.Lock()
		delete(o.cache, cacheKey)
		o.mu.Unlock()
	}

	o.mu.RLock()
	adapters := make([]RetrievalSourceAdapter, 0, len(request.SourceKinds))
	missing := make([]RetrievalSourceFailure, 0)
	for _, kind := range request.SourceKinds {
		adapter, exists := o.adapters[kind]
		if !exists {
			missing = append(missing, RetrievalSourceFailure{SourceKind: string(kind), Code: "subagent_retrieval_adapter_unavailable", Message: "检索来源适配器不可用", Retryable: true})
			continue
		}
		adapters = append(adapters, adapter)
	}
	o.mu.RUnlock()

	type sourceResult struct {
		kind  RetrievalSourceKind
		items []EvidenceItem
		err   error
	}
	results := make(chan sourceResult, len(adapters))
	semaphore := make(chan struct{}, o.options.MaxConcurrency)
	var wait sync.WaitGroup
	for _, adapter := range adapters {
		adapter := adapter
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				o.emitEvent(searchCtx, request, "retrieval.source_started", map[string]any{"source_kind": string(adapter.Kind())})
			case <-searchCtx.Done():
				results <- sourceResult{kind: adapter.Kind(), err: searchCtx.Err()}
				return
			}
			items, err := adapter.Search(searchCtx, request)
			<-semaphore
			o.emitEvent(searchCtx, request, "retrieval.source_completed", map[string]any{
				"source_kind": string(adapter.Kind()), "item_count": len(items), "failed": err != nil,
			})
			results <- sourceResult{kind: adapter.Kind(), items: items, err: err}
		}()
	}
	wait.Wait()
	close(results)

	result := RetrievalResult{
		RequestID: "retrieval-" + newSessionID(), QueryDigest: digestSubAgentText(request.Query), PermissionDigest: strings.TrimSpace(request.PermissionDigest),
		SourceCounts: make(map[string]int), RetrievedAt: now, Failures: missing,
	}
	for item := range results {
		if item.err != nil {
			result.Failures = append(result.Failures, retrievalSourceFailure(item.kind, item.err))
			continue
		}
		count := 0
		for _, evidence := range item.items {
			if count >= o.options.MaxCandidates {
				break
			}
			if !validEvidence(evidence) {
				result.Failures = append(result.Failures, RetrievalSourceFailure{
					SourceKind: string(item.kind), SourceID: strings.TrimSpace(evidence.SourceID),
					Code: "subagent_citation_missing", Message: "检索来源缺少稳定引用字段", Retryable: false,
				})
				continue
			}
			if evidence.Stale {
				result.Failures = append(result.Failures, RetrievalSourceFailure{
					SourceKind: string(item.kind), SourceID: evidence.SourceID,
					Code: "subagent_evidence_stale", Message: "检索证据已过期", Retryable: false,
				})
				continue
			}
			bounded := boundEvidence(evidence)
			if strings.TrimSpace(bounded.Citation) == "" {
				result.Failures = append(result.Failures, RetrievalSourceFailure{
					SourceKind: string(item.kind), SourceID: evidence.SourceID,
					Code: "subagent_citation_missing", Message: "检索证据无法生成引用定位", Retryable: false,
				})
				continue
			}
			result.Items = append(result.Items, bounded)
			count++
		}
		result.SourceCounts[string(item.kind)] = count
	}
	beforeDedup := len(result.Items)
	maxEvidenceBytes := request.MaxResultBytes
	if maxEvidenceBytes <= 0 || maxEvidenceBytes > o.options.MaxEvidenceBytes {
		maxEvidenceBytes = o.options.MaxEvidenceBytes
	}
	result.Items = deduplicateEvidence(result.Items, request.TopK, request.RerankEnabled, request.Query, maxEvidenceBytes)
	o.emitEvent(searchCtx, request, "retrieval.deduplicated", map[string]any{
		"before_count": beforeDedup, "after_count": len(result.Items),
	})
	if request.RerankEnabled {
		o.emitEvent(searchCtx, request, "retrieval.reranked", map[string]any{"item_count": len(result.Items)})
	}
	result.Partial = len(result.Failures) > 0
	if len(result.Items) == 0 {
		return result, &SubAgentRuntimeError{Code: "subagent_no_evidence", HumanMessage: "未检索到可用证据", Retryable: false, RequestID: result.RequestID}
	}
	ttl := request.CacheTTL
	if ttl <= 0 {
		ttl = o.options.DefaultCacheTTL
	}
	o.mu.Lock()
	o.cache[cacheKey] = retrievalCacheEntry{result: cloneRetrievalResult(result), request: cloneRetrievalRequest(request), expiresAt: now.Add(ttl)}
	o.mu.Unlock()
	o.emitEvent(searchCtx, request, "retrieval.summarized", map[string]any{
		"cached": false, "item_count": len(result.Items), "failure_count": len(result.Failures), "partial": result.Partial,
	})
	return result, nil
}

func (o *RetrievalOrchestrator) validateCachedAccess(ctx context.Context, request RetrievalRequest) error {
	if o == nil {
		return errors.New("检索编排器不可用")
	}
	o.mu.RLock()
	adapters := make([]RetrievalSourceAdapter, 0, len(request.SourceKinds))
	for _, kind := range request.SourceKinds {
		adapter, ok := o.adapters[kind]
		if !ok {
			o.mu.RUnlock()
			return fmt.Errorf("检索来源适配器不可用: %s", kind)
		}
		adapters = append(adapters, adapter)
	}
	o.mu.RUnlock()
	for _, adapter := range adapters {
		validator, ok := adapter.(RetrievalAccessValidator)
		if !ok {
			return fmt.Errorf("检索来源未提供缓存权限复核: %s", adapter.Kind())
		}
		if err := validator.ValidateAccess(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

// Invalidate 按用户/会话/权限范围删除缓存。调用方在会话删除、权限撤销、
// 知识库版本变化或工作区内容变化后必须调用，不能继续使用旧证据。
func (o *RetrievalOrchestrator) Invalidate(request RetrievalRequest) {
	if o == nil {
		return
	}
	key := retrievalCacheKey(request)
	o.mu.Lock()
	delete(o.cache, key)
	// 用户、会话、来源或权限范围的删除/撤销都必须能清掉更宽范围的缓存。
	// 只在指定 conversation/session 时扫描会让“删除用户全部会话”留下旧证据。
	if strings.TrimSpace(request.UserID) != "" || strings.TrimSpace(request.ConversationID) != "" || strings.TrimSpace(request.SessionID) != "" || strings.TrimSpace(request.PermissionDigest) != "" || len(request.SourceKinds) > 0 || len(request.SourceIDs) > 0 || len(request.Scope) > 0 {
		for cacheKey, item := range o.cache {
			if retrievalCacheEntryMatchesScope(item.request, request) {
				delete(o.cache, cacheKey)
			}
		}
	}
	o.mu.Unlock()
}

// CleanupExpired 主动回收短 TTL 结果，避免只有“下一次相同查询”才能触发
// 惰性清理。这里只删除内存中的有界证据缓存，不触碰来源数据或凭据。
func (o *RetrievalOrchestrator) CleanupExpired(now time.Time) int {
	if o == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	deleted := 0
	for key, item := range o.cache {
		if !item.expiresAt.After(now.UTC()) {
			delete(o.cache, key)
			deleted++
		}
	}
	return deleted
}

func normalizeRetrievalRequest(request RetrievalRequest, options RetrievalOrchestratorOptions) RetrievalRequest {
	request.Query = strings.TrimSpace(request.Query)
	request.UserID = strings.TrimSpace(request.UserID)
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	request.SessionID = strings.TrimSpace(request.SessionID)
	request.PermissionDigest = strings.TrimSpace(request.PermissionDigest)
	request.SourceKinds = uniqueRetrievalSourceKinds(request.SourceKinds)
	if request.TopK <= 0 {
		request.TopK = 5
	}
	if request.MaxQueryCount <= 0 {
		request.MaxQueryCount = options.MaxQueries
	}
	if request.MaxResultBytes <= 0 {
		request.MaxResultBytes = options.MaxEvidenceBytes
	}
	return request
}

func uniqueRetrievalSourceKinds(values []RetrievalSourceKind) []RetrievalSourceKind {
	result := make([]RetrievalSourceKind, 0, len(values))
	seen := make(map[RetrievalSourceKind]struct{}, len(values))
	for _, value := range values {
		value = RetrievalSourceKind(strings.ToLower(strings.TrimSpace(string(value))))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func retrievalSourceKindNames(values []RetrievalSourceKind) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, string(value))
	}
	return result
}

func cloneRetrievalRequest(value RetrievalRequest) RetrievalRequest {
	value.SourceKinds = append([]RetrievalSourceKind(nil), value.SourceKinds...)
	value.SourceIDs = append([]string(nil), value.SourceIDs...)
	value.Scope = append([]string(nil), value.Scope...)
	if value.Filters != nil {
		value.Filters = make(map[string]string, len(value.Filters))
		for key, item := range value.Filters {
			value.Filters[key] = item
		}
	}
	return value
}

func retrievalCacheEntryMatchesScope(cached, requested RetrievalRequest) bool {
	if strings.TrimSpace(requested.UserID) != "" && strings.TrimSpace(cached.UserID) != strings.TrimSpace(requested.UserID) {
		return false
	}
	if strings.TrimSpace(requested.ConversationID) != "" && strings.TrimSpace(cached.ConversationID) != strings.TrimSpace(requested.ConversationID) {
		return false
	}
	if strings.TrimSpace(requested.SessionID) != "" && strings.TrimSpace(cached.SessionID) != strings.TrimSpace(requested.SessionID) {
		return false
	}
	if strings.TrimSpace(requested.PermissionDigest) != "" && strings.TrimSpace(cached.PermissionDigest) != strings.TrimSpace(requested.PermissionDigest) {
		return false
	}
	if len(requested.SourceKinds) > 0 && !retrievalStringSetOverlap(retrievalSourceKindStrings(cached.SourceKinds), retrievalSourceKindStrings(requested.SourceKinds)) {
		return false
	}
	if len(requested.SourceIDs) > 0 && !retrievalStringSetOverlap(cached.SourceIDs, requested.SourceIDs) {
		return false
	}
	if len(requested.Scope) > 0 && !retrievalStringSetOverlap(cached.Scope, requested.Scope) {
		return false
	}
	return true
}

func retrievalSourceKindStrings(values []RetrievalSourceKind) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, string(value))
	}
	return result
}

func retrievalStringSetOverlap(left, right []string) bool {
	seen := make(map[string]struct{}, len(left))
	for _, value := range left {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	for _, value := range right {
		if _, ok := seen[strings.TrimSpace(value)]; ok {
			return true
		}
	}
	return false
}

func (o *RetrievalOrchestrator) emitEvent(ctx context.Context, request RetrievalRequest, eventType string, data map[string]any) {
	if o == nil || o.options.EventSink == nil || strings.TrimSpace(request.InvocationID) == "" {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["query_digest"] = digestSubAgentText(request.Query)
	data["permission_digest"] = strings.TrimSpace(request.PermissionDigest)
	if err := o.options.EventSink(context.WithoutCancel(ctx), strings.TrimSpace(request.InvocationID), eventType, data); err != nil {
		// 事件是可观测性旁路，不能反向阻塞只读检索结果。
		return
	}
}

func validateRetrievalRequest(request RetrievalRequest, options RetrievalOrchestratorOptions) error {
	if strings.TrimSpace(request.Query) == "" || strings.TrimSpace(request.UserID) == "" {
		return &SubAgentRuntimeError{Code: "subagent_source_scope_denied", HumanMessage: "检索请求缺少用户或查询范围", Retryable: false}
	}
	if len(request.SourceKinds) == 0 || len(request.SourceKinds) > options.MaxSources {
		return &SubAgentRuntimeError{Code: "subagent_query_limit_exceeded", HumanMessage: "检索来源数量超出上限", Retryable: false}
	}
	if request.TopK <= 0 || request.TopK > options.MaxCandidates {
		return &SubAgentRuntimeError{Code: "subagent_query_limit_exceeded", HumanMessage: "检索结果数量超出上限", Retryable: false}
	}
	if request.MaxQueryCount <= 0 || request.MaxQueryCount > options.MaxQueries || len(request.SourceKinds) > request.MaxQueryCount {
		return &SubAgentRuntimeError{Code: "subagent_query_limit_exceeded", HumanMessage: "检索查询次数超出上限", Retryable: false}
	}
	if request.MaxResultBytes < 0 || request.MaxResultBytes > options.MaxEvidenceBytes {
		return &SubAgentRuntimeError{Code: "subagent_budget_exhausted", HumanMessage: "检索结果字节预算无效", Retryable: false}
	}
	return nil
}

func validRetrievalSourceKind(kind RetrievalSourceKind) bool {
	switch kind {
	case RetrievalSourceMemory, RetrievalSourceKnowledge, RetrievalSourceConversation, RetrievalSourceWeb, RetrievalSourceWorkspace, RetrievalSourceStructured:
		return true
	default:
		return false
	}
}

func validEvidence(item EvidenceItem) bool {
	return strings.TrimSpace(item.EvidenceID) != "" && strings.TrimSpace(item.SourceKind) != "" && strings.TrimSpace(item.SourceID) != "" && strings.TrimSpace(item.Excerpt) != ""
}

func boundEvidence(item EvidenceItem) EvidenceItem {
	item.Excerpt = limitGenericSubAgentText(strings.TrimSpace(item.Excerpt), 8<<10)
	item.Title = limitGenericSubAgentText(strings.TrimSpace(item.Title), 512)
	item.Locator = limitGenericSubAgentText(strings.TrimSpace(item.Locator), 512)
	if strings.TrimSpace(item.ContentDigest) == "" {
		item.ContentDigest = digestSubAgentText(item.Excerpt)
	}
	if strings.TrimSpace(item.Citation) == "" {
		item.Citation = strings.TrimSpace(item.SourceName + " " + item.Locator)
	}
	if item.RetrievedAt.IsZero() {
		item.RetrievedAt = time.Now().UTC()
	}
	return item
}

func deduplicateEvidence(items []EvidenceItem, topK int, rerank bool, query string, maxBytes int64) []EvidenceItem {
	seen := make(map[string]struct{}, len(items))
	result := make([]EvidenceItem, 0, len(items))
	var bytesUsed int64
	terms := strings.Fields(strings.ToLower(query))
	for index := range items {
		item := items[index]
		key := item.ContentDigest
		if key == "" {
			key = item.SourceKind + ":" + item.SourceID + ":" + item.Locator
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if rerank {
			item.RerankScore = item.RetrievalScore
			value := strings.ToLower(item.Title + " " + item.Excerpt)
			for _, term := range terms {
				if term != "" && strings.Contains(value, term) {
					item.RerankScore++
				}
			}
		}
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.RerankScore != right.RerankScore {
			return left.RerankScore > right.RerankScore
		}
		if left.SourceKind != right.SourceKind {
			return left.SourceKind < right.SourceKind
		}
		return left.EvidenceID < right.EvidenceID
	})
	bounded := result[:0]
	for _, item := range result {
		if len(bounded) >= topK {
			break
		}
		size := int64(len([]byte(item.Title)) + len([]byte(item.Excerpt)) + len([]byte(item.Citation)))
		if maxBytes > 0 && bytesUsed+size > maxBytes {
			break
		}
		bytesUsed += size
		bounded = append(bounded, item)
	}
	return bounded
}

func retrievalSourceFailure(kind RetrievalSourceKind, err error) RetrievalSourceFailure {
	if err == nil {
		return RetrievalSourceFailure{SourceKind: string(kind), Code: "subagent_retrieval_adapter_unavailable", Message: "检索来源失败", Retryable: true}
	}
	var runtimeErr *SubAgentRuntimeError
	if errors.As(err, &runtimeErr) && runtimeErr != nil {
		message := strings.TrimSpace(runtimeErr.HumanMessage)
		if message == "" {
			message = "检索来源失败"
		}
		return RetrievalSourceFailure{SourceKind: string(kind), Code: runtimeErr.Code, Message: limitSubAgentError(message), Retryable: runtimeErr.Retryable}
	}
	code := "subagent_retrieval_adapter_unavailable"
	retryable := true
	if errors.Is(err, context.Canceled) {
		code, retryable = "subagent_cancelled", false
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = "subagent_timeout"
	}
	return RetrievalSourceFailure{SourceKind: string(kind), Code: code, Message: limitSubAgentError(err.Error()), Retryable: retryable}
}

func retrievalCacheKey(request RetrievalRequest) string {
	filters := make([]string, 0, len(request.Filters))
	for key, value := range request.Filters {
		filters = append(filters, key+"="+value)
	}
	sort.Strings(filters)
	payload := struct {
		UserID, ConversationID, SessionID, Query, QueryType, Freshness, PermissionDigest string
		SourceKinds                                                                      []RetrievalSourceKind
		SourceIDs, Scope, Filters                                                        []string
		TopK                                                                             int
		Rerank                                                                           bool
	}{
		UserID: request.UserID, ConversationID: request.ConversationID, SessionID: request.SessionID,
		Query: request.Query, QueryType: request.QueryType, Freshness: request.Freshness, PermissionDigest: request.PermissionDigest,
		SourceKinds: append([]RetrievalSourceKind(nil), request.SourceKinds...), SourceIDs: append([]string(nil), request.SourceIDs...),
		Scope: append([]string(nil), request.Scope...), Filters: filters, TopK: request.TopK, Rerank: request.RerankEnabled,
	}
	data, _ := json.Marshal(payload)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cloneRetrievalResult(value RetrievalResult) RetrievalResult {
	value.Items = append([]EvidenceItem(nil), value.Items...)
	value.Failures = append([]RetrievalSourceFailure(nil), value.Failures...)
	if value.SourceCounts != nil {
		value.SourceCounts = map[string]int{}
		for key, count := range value.SourceCounts {
			value.SourceCounts[key] = count
		}
	}
	return value
}
