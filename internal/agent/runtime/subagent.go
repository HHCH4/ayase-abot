package runtime

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
	"unicode/utf8"

	"Abot/internal/agent"
)

// SubAgentProfile 标识子 Agent 的职责边界。Profile 不是模型名称，模型仍由
// Kernel 的内置 Provider Registry 解析，避免把供应商配置泄漏到任务编排层。
type SubAgentProfile string

const (
	SubAgentProfileDocumentImage      SubAgentProfile = "document_image"
	SubAgentProfileMemoryRetrieval    SubAgentProfile = "memory_retrieval"
	SubAgentProfileKnowledgeRetrieval SubAgentProfile = "knowledge_retrieval"
	SubAgentProfileConversationSearch SubAgentProfile = "conversation_retrieval"
	SubAgentProfileWebResearch        SubAgentProfile = "web_research"
	SubAgentProfileWorkspaceSearch    SubAgentProfile = "workspace_search"
	SubAgentProfileStructuredQuery    SubAgentProfile = "structured_query"
	SubAgentProfileRetrievalAggregate SubAgentProfile = "retrieval_aggregate"
	SubAgentProfileResearch           SubAgentProfile = "research"
	SubAgentProfileWorkspaceReview    SubAgentProfile = "workspace_review"
	SubAgentProfileWorkspaceChange    SubAgentProfile = "workspace_change"
	SubAgentProfileVerification       SubAgentProfile = "verification"
)

// SubAgentGroupStatus 是一组并行子 Agent 的聚合生命周期。
type SubAgentGroupStatus string

const (
	SubAgentGroupQueued    SubAgentGroupStatus = "queued"
	SubAgentGroupRunning   SubAgentGroupStatus = "running"
	SubAgentGroupCompleted SubAgentGroupStatus = "completed"
	SubAgentGroupPartial   SubAgentGroupStatus = "partial_failed"
	SubAgentGroupFailed    SubAgentGroupStatus = "failed"
	SubAgentGroupCancelled SubAgentGroupStatus = "cancelled"
	SubAgentGroupExpired   SubAgentGroupStatus = "expired"
)

func (s SubAgentGroupStatus) terminal() bool {
	return s == SubAgentGroupCompleted || s == SubAgentGroupPartial || s == SubAgentGroupFailed || s == SubAgentGroupCancelled || s == SubAgentGroupExpired
}

// SubAgentRunStatus 是单个子 Agent 节点的生命周期。
type SubAgentRunStatus string

const (
	SubAgentRunQueued    SubAgentRunStatus = "queued"
	SubAgentRunRunning   SubAgentRunStatus = "running"
	SubAgentRunCompleted SubAgentRunStatus = "completed"
	SubAgentRunFailed    SubAgentRunStatus = "failed"
	SubAgentRunCancelled SubAgentRunStatus = "cancelled"
	SubAgentRunExpired   SubAgentRunStatus = "expired"
)

func (s SubAgentRunStatus) terminal() bool {
	return s == SubAgentRunCompleted || s == SubAgentRunFailed || s == SubAgentRunCancelled || s == SubAgentRunExpired
}

// SubAgentFailurePolicy 决定一张图片或一个检索源失败时是否继续汇总其它结果。
type SubAgentFailurePolicy string

const (
	SubAgentFailureContinue SubAgentFailurePolicy = "continue"
	SubAgentFailureAbort    SubAgentFailurePolicy = "abort"
)

const (
	maxSubAgentRuns          = 32
	maxSubAgentDepth         = 2
	maxSubAgentGroups        = 8
	maxSubAgentConcurrency   = 4
	maxSubAgentOutputBytes   = 32 << 10
	maxSubAgentGroupOutput   = 128 << 10
	maxSubAgentInputMetadata = 16 << 10
	maxRetrievalTopK         = 50
	defaultRetrievalSources  = 6
	maxRetrievalSources      = 16
	maxRetrievalResultBytes  = 128 << 10
	maxEvidenceExcerptBytes  = 32 << 10
	maxSubAgentErrorBytes    = 4096
	maxSubAgentLease         = 10 * time.Minute
	maxDocumentImageBytes    = 8 << 20
	maxDocumentImageTotal    = 24 << 20
	maxRetrievalCacheEntries = 256
	// MaxSubAgentListLimit 给恢复和清理流程一个统一的分页上限；HTTP 展示层
	// 可以继续使用更小的页面大小，避免一次返回过多运行记录。
	MaxSubAgentListLimit = 256
)

// SubAgentGroup 是可持久化的子 Agent 编排记录。它只保存摘要和元数据，绝不
// 保存图片字节、附件正文、授权口令或完整模型上下文。
type SubAgentGroup struct {
	ID                 string                `json:"id"`
	InvocationID       string                `json:"invocation_id,omitempty"`
	ConversationID     string                `json:"conversation_id,omitempty"`
	RootInvocationID   string                `json:"root_invocation_id,omitempty"`
	ParentInvocationID string                `json:"parent_invocation_id,omitempty"`
	ParentNodeID       string                `json:"parent_node_id,omitempty"`
	Profile            SubAgentProfile       `json:"profile"`
	Purpose            string                `json:"purpose,omitempty"`
	Status             SubAgentGroupStatus   `json:"status"`
	FailurePolicy      SubAgentFailurePolicy `json:"failure_policy"`
	ExpectedCount      int                   `json:"expected_count"`
	QueuedCount        int                   `json:"queued_count"`
	RunningCount       int                   `json:"running_count"`
	CompletedCount     int                   `json:"completed_count"`
	FailedCount        int                   `json:"failed_count"`
	CancelledCount     int                   `json:"cancelled_count"`
	MaxConcurrency     int                   `json:"max_concurrency"`
	TimeoutSeconds     int                   `json:"timeout_seconds"`
	OutputBudgetBytes  int                   `json:"output_budget_bytes"`
	InputBudgetBytes   int                   `json:"input_budget_bytes,omitempty"`
	ImageBudgetBytes   int                   `json:"image_budget_bytes,omitempty"`
	DeadlineAt         time.Time             `json:"deadline_at,omitempty"`
	SourceKinds        []string              `json:"source_kinds,omitempty"`
	QueryDigest        string                `json:"query_digest,omitempty"`
	ParentPlanID       string                `json:"parent_plan_id,omitempty"`
	ResultDigest       string                `json:"result_digest,omitempty"`
	ErrorSummary       string                `json:"error_summary,omitempty"`
	CreatedAt          time.Time             `json:"created_at"`
	StartedAt          time.Time             `json:"started_at,omitempty"`
	FinishedAt         time.Time             `json:"finished_at,omitempty"`
	UpdatedAt          time.Time             `json:"updated_at"`
}

// SubAgentRun 是一个可独立 claim/续租/完成的子 Agent 节点。
type SubAgentRun struct {
	ID             string            `json:"id"`
	GroupID        string            `json:"group_id"`
	InvocationID   string            `json:"invocation_id,omitempty"`
	ParentNodeID   string            `json:"parent_node_id,omitempty"`
	Ordinal        int               `json:"ordinal"`
	Stage          int               `json:"stage"`
	DependsOn      []string          `json:"depends_on,omitempty"`
	Profile        SubAgentProfile   `json:"profile"`
	SourceKind     string            `json:"source_kind,omitempty"`
	Status         SubAgentRunStatus `json:"status"`
	Attempt        int               `json:"attempt"`
	ProviderID     string            `json:"provider_id,omitempty"`
	ModelID        string            `json:"model_id,omitempty"`
	InputDigest    string            `json:"input_digest,omitempty"`
	InputMetadata  map[string]any    `json:"input_metadata,omitempty"`
	ResultText     string            `json:"result_text,omitempty"`
	ResultMetadata map[string]any    `json:"result_metadata,omitempty"`
	ResultDigest   string            `json:"result_digest,omitempty"`
	ErrorCode      string            `json:"error_code,omitempty"`
	Error          string            `json:"error,omitempty"`
	ErrorRetryable bool              `json:"error_retryable,omitempty"`
	LeaseOwner     string            `json:"-"`
	LeaseExpiresAt time.Time         `json:"-"`
	CreatedAt      time.Time         `json:"created_at"`
	QueuedAt       time.Time         `json:"queued_at"`
	StartedAt      time.Time         `json:"started_at,omitempty"`
	FinishedAt     time.Time         `json:"finished_at,omitempty"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// EvidenceItem 是所有检索适配器的统一输出，供主 Agent 引用和最终答案回溯。
// ContentDigest 用于去重；正文只保留有界 Excerpt，不保存原始文件或秘密。
type EvidenceItem struct {
	EvidenceID     string         `json:"evidence_id"`
	SourceKind     string         `json:"source_kind"`
	SourceID       string         `json:"source_id,omitempty"`
	SourceName     string         `json:"source_name,omitempty"`
	Locator        string         `json:"locator,omitempty"`
	Title          string         `json:"title,omitempty"`
	Excerpt        string         `json:"excerpt,omitempty"`
	ContentDigest  string         `json:"content_digest"`
	RetrievalScore float64        `json:"retrieval_score,omitempty"`
	RerankScore    float64        `json:"rerank_score,omitempty"`
	RetrievedAt    time.Time      `json:"retrieved_at"`
	PublishedAt    *time.Time     `json:"published_at,omitempty"`
	Freshness      string         `json:"freshness,omitempty"`
	TrustLevel     string         `json:"trust_level,omitempty"`
	Citation       string         `json:"citation,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Truncated      bool           `json:"truncated,omitempty"`
	Stale          bool           `json:"stale,omitempty"`
}

// RetrievalRequest 是检索层的统一输入。SourceKinds 为空时使用当前已注册的
// 全部来源；SourceIDs、Scope 和 Filters 由适配器自行执行最小权限过滤。
type RetrievalRequest struct {
	InvocationID   string            `json:"invocation_id,omitempty"`
	UserID         string            `json:"user_id"`
	ConversationID string            `json:"conversation_id,omitempty"`
	SessionID      string            `json:"session_id,omitempty"`
	WorkspaceID    string            `json:"workspace_id,omitempty"`
	Query          string            `json:"query"`
	QueryType      string            `json:"query_type,omitempty"`
	SourceKinds    []string          `json:"source_kinds,omitempty"`
	SourceIDs      []string          `json:"source_ids,omitempty"`
	Scope          map[string]string `json:"scope,omitempty"`
	Filters        map[string]string `json:"filters,omitempty"`
	TopK           int               `json:"top_k,omitempty"`
	RerankEnabled  bool              `json:"rerank_enabled,omitempty"`
	Freshness      string            `json:"freshness,omitempty"`
	MaxQueryCount  int               `json:"max_query_count,omitempty"`
	MaxResultBytes int               `json:"max_result_bytes,omitempty"`
	Deadline       time.Time         `json:"deadline,omitempty"`
	// Runtime is resolved by Coordinator from the current conversation. It is
	// deliberately excluded from HTTP JSON so callers cannot inject a policy.
	Runtime agent.RuntimeOptions `json:"-"`
}

// RetrievalFailure 保留失败来源的可展示摘要，不暴露内部调用栈或请求正文。
type RetrievalFailure struct {
	SourceKind string `json:"source_kind"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
}

// RetrievalSummary 是一次检索的稳定输出，可直接作为主 Agent 的证据上下文。
type RetrievalSummary struct {
	GroupID      string             `json:"group_id"`
	Query        string             `json:"query"`
	Evidence     []EvidenceItem     `json:"evidence"`
	Failures     []RetrievalFailure `json:"failures,omitempty"`
	NoEvidence   bool               `json:"no_evidence"`
	Partial      bool               `json:"partial"`
	ResultDigest string             `json:"result_digest"`
}

// SubAgentRepository 是对核心 Repository 的可选扩展，保持第三方 Runtime
// 嵌入者的最小接口不变。SQLite 和 Memory 都提供完整实现。
type SubAgentRepository interface {
	CreateSubAgentGroup(context.Context, SubAgentGroup, []SubAgentRun) error
	GetSubAgentGroup(context.Context, string) (SubAgentGroup, error)
	ListSubAgentGroups(context.Context, string, SubAgentGroupStatus, int) ([]SubAgentGroup, error)
	UpdateSubAgentGroup(context.Context, SubAgentGroup) error
	GetSubAgentRun(context.Context, string) (SubAgentRun, error)
	ListSubAgentRuns(context.Context, string, SubAgentRunStatus, int) ([]SubAgentRun, error)
	ClaimSubAgentRun(context.Context, string, string, time.Time, time.Duration) (SubAgentRun, bool, error)
	CompleteSubAgentRun(context.Context, string, string, SubAgentRunStatus, string, string, string, time.Time) (bool, error)
	TransitionSubAgentRun(context.Context, string, SubAgentRunStatus, SubAgentRunStatus, string, time.Time) (bool, error)
}

// SubAgentEvidenceRepository 持久化有界证据摘要，来源正文仍由各领域服务保留。
type SubAgentEvidenceRepository interface {
	SaveSubAgentEvidence(context.Context, string, []EvidenceItem) error
	ListSubAgentEvidence(context.Context, string, int, string) ([]EvidenceItem, error)
}

// SubAgentCleanupRepository 只删除有明确终态且超过保留期限的子 Agent 记录，
// 运行中任务、父任务关联和最近的审计摘要不会被定时清理。
type SubAgentCleanupRepository interface {
	PruneSubAgentRecords(context.Context, time.Time, int) (int, error)
}

// RetrievalAdapter 是一个可插拔、可审计的检索来源。适配器不得绕过用户、会话
// 和工作区边界；所有返回内容都会再次经过 Runtime 的大小与字段限制。
type RetrievalAdapter interface {
	Kind() string
	Retrieve(context.Context, RetrievalRequest) ([]EvidenceItem, error)
}

type retrievalAdapterFunc struct {
	kind string
	fn   func(context.Context, RetrievalRequest) ([]EvidenceItem, error)
}

func (a retrievalAdapterFunc) Kind() string { return a.kind }
func (a retrievalAdapterFunc) Retrieve(ctx context.Context, request RetrievalRequest) ([]EvidenceItem, error) {
	return a.fn(ctx, request)
}

// NewRetrievalAdapter 将普通函数包装成 Runtime 检索适配器，方便 app 层装配。
func NewRetrievalAdapter(kind string, fn func(context.Context, RetrievalRequest) ([]EvidenceItem, error)) RetrievalAdapter {
	return retrievalAdapterFunc{kind: strings.TrimSpace(kind), fn: fn}
}

// DocumentImageRunner 是 Kernel 单图调用的窄边界。Runtime 负责编排与生命周期，
// Kernel 负责解析内置 Provider 和发送原生图片输入。
type DocumentImageRunner func(context.Context, agent.DocumentImageAnalysisRequest) (string, error)

// BuiltInSubAgentTask 是一个不落盘的文本子 Agent 节点。Prompt 只在本次
// 调用栈内存在；持久化层只保存摘要、哈希和预算元数据，避免把完整上下文写入日志。
type BuiltInSubAgentTask struct {
	NodeID    string         `json:"node_id"`
	Stage     int            `json:"stage,omitempty"`
	DependsOn []string       `json:"depends_on,omitempty"`
	Prompt    string         `json:"prompt"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// BuiltInSubAgentGroupRequest 描述一次通用内置文本子 Agent 编排。它可以
// 表达研究、工作区审查、验证等并行节点，但执行仍统一走 Abot 已注册的内置 AI。
type BuiltInSubAgentGroupRequest struct {
	InvocationID   string          `json:"invocation_id,omitempty"`
	UserID         string          `json:"user_id"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Profile        SubAgentProfile `json:"profile"`
	Purpose        string          `json:"purpose,omitempty"`
	ProviderID     string          `json:"provider_id,omitempty"`
	ModelID        string          `json:"model_id,omitempty"`
	// Runtime 只由 Coordinator 从当前会话配置解析，不能由 HTTP/JSON 请求体注入；
	// 内部测试和已装配的 Runtime 可以直接传递它验证生命周期行为。
	Runtime           agent.RuntimeOptions  `json:"-"`
	Tasks             []BuiltInSubAgentTask `json:"tasks"`
	MaxConcurrency    int                   `json:"max_concurrency,omitempty"`
	TimeoutSeconds    int                   `json:"timeout_seconds,omitempty"`
	OutputBudgetBytes int                   `json:"output_budget_bytes,omitempty"`
	FailurePolicy     SubAgentFailurePolicy `json:"failure_policy,omitempty"`
}

// BuiltInSubAgentTaskResult 是文本子 Agent 的有序结果。单节点失败不会
// 丢失同组其它结果，调用方可依据 Error 决定是否继续汇总。
type BuiltInSubAgentTaskResult struct {
	GroupID string `json:"group_id"`
	RunID   string `json:"run_id"`
	NodeID  string `json:"node_id"`
	Text    string `json:"text,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SubAgentManager 管理所有通用子 Agent。它不依赖外部 Agent 服务，执行实际由
// 注入的 Kernel 回调或领域检索适配器完成。
type SubAgentManager struct {
	repo         Repository
	subRepo      SubAgentRepository
	evidenceRepo SubAgentEvidenceRepository
	imageRunner  DocumentImageRunner
	textRunner   agent.BuiltInSubAgentRunner

	mu        sync.Mutex
	adapters  map[string]RetrievalAdapter
	active    map[string]context.CancelFunc
	eventSink func(context.Context, AgentEvent) (AgentEvent, error)
	closed    bool

	// quotaMu 把“读取当前 group 数量”和“创建新 group”之间的窗口串行化，
	// 防止多个并行检索请求同时通过每个 invocation 的 group 上限。
	quotaMu       sync.Mutex
	quotaReserved map[string]int

	// retrievalCache 只保留有界 EvidenceItem，键包含用户、会话、工作区和
	// 查询范围；缓存不落盘，过期或权限变化时可整体失效。
	retrievalCacheMu sync.Mutex
	retrievalCache   map[string]retrievalCacheEntry
}

type retrievalCacheEntry struct {
	Items          []EvidenceItem
	UserID         string
	ConversationID string
	WorkspaceID    string
	CreatedAt      time.Time
	LastUsedAt     time.Time
	ExpiresAt      time.Time
}

// NewSubAgentManager 创建子 Agent 管理器。没有持久化扩展时使用进程内后备仓储，
// 使旧嵌入者仍能使用图片解析和检索 API，但生产 SQLite 会自动使用 durable rows。
func NewSubAgentManager(repo Repository, imageRunner DocumentImageRunner) (*SubAgentManager, error) {
	if repo == nil {
		return nil, errors.New("运行时 Repository 不能为空")
	}
	subRepo, ok := repo.(SubAgentRepository)
	if !ok {
		subRepo = newMemorySubAgentRepository()
	}
	evidenceRepo, _ := subRepo.(SubAgentEvidenceRepository)
	return &SubAgentManager{
		repo: repo, subRepo: subRepo, evidenceRepo: evidenceRepo, imageRunner: imageRunner,
		adapters: make(map[string]RetrievalAdapter), active: make(map[string]context.CancelFunc), quotaReserved: make(map[string]int), retrievalCache: make(map[string]retrievalCacheEntry),
	}, nil
}

// SetEventSink 设置统一事件出口。事件失败不会中断模型或检索工作，但会被调用方
// 记录为 Runtime 诊断；子 Agent 本身的 durable 状态仍以仓储为准。
func (m *SubAgentManager) SetEventSink(sink func(context.Context, AgentEvent) (AgentEvent, error)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.eventSink = sink
	m.mu.Unlock()
}

// SetBuiltInTextRunner 安装通用文本子 Agent 的内置模型执行器。Runtime 只
// 保存这个窄接口，不直接依赖 Provider 或 ADK，保证执行边界仍由 Kernel 控制。
func (m *SubAgentManager) SetBuiltInTextRunner(runner agent.BuiltInSubAgentRunner) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.textRunner = runner
	m.mu.Unlock()
}

// SetRetrievalAdapter 注册或替换一个来源适配器，kind 为空或函数为空时拒绝安装。
func (m *SubAgentManager) SetRetrievalAdapter(adapter RetrievalAdapter) error {
	if m == nil || adapter == nil || strings.TrimSpace(adapter.Kind()) == "" {
		return errors.New("检索适配器不能为空")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrConflict
	}
	m.adapters[strings.TrimSpace(adapter.Kind())] = adapter
	return nil
}

func (m *SubAgentManager) emit(ctx context.Context, invocationID, eventType string, data map[string]any) {
	invocationID = strings.TrimSpace(invocationID)
	if m == nil || invocationID == "" {
		return
	}
	m.mu.Lock()
	sink := m.eventSink
	m.mu.Unlock()
	if sink == nil {
		return
	}
	_, _ = sink(context.WithoutCancel(ctx), AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: eventType, Timestamp: time.Now().UTC(), Data: data})
}

func digestSubAgentText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func boundedSubAgentError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	return limitSubAgentText(message, maxSubAgentErrorBytes)
}

// LimitSubAgentText 为持久化、事件和模型结果提供统一的 UTF-8 有界截断。
// 导出给 SQLite 等边界层使用，避免各层按字节切片后写入无效 UTF-8。
func LimitSubAgentText(value string, maxBytes int) string {
	return limitSubAgentText(value, maxBytes)
}

func limitSubAgentText(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len([]byte(value)) <= maxBytes && utf8.ValidString(value) {
		return value
	}
	suffix := "\n[内容已截断]"
	if len([]byte(suffix)) >= maxBytes {
		data := []byte(value)
		if len(data) > maxBytes {
			data = data[:maxBytes]
		}
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
		return string(data)
	}
	limit := maxBytes - len([]byte(suffix))
	data := []byte(value[:limit])
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data) + suffix
}

func normalizeSubAgentGroup(group SubAgentGroup, now time.Time) (SubAgentGroup, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	group.ID = strings.TrimSpace(group.ID)
	group.InvocationID = strings.TrimSpace(group.InvocationID)
	group.Profile = SubAgentProfile(strings.TrimSpace(string(group.Profile)))
	group.Purpose = strings.TrimSpace(group.Purpose)
	group.ErrorSummary = strings.TrimSpace(group.ErrorSummary)
	if group.ID == "" {
		group.ID = newID("subagent-group")
	}
	if !validSubAgentProfile(group.Profile) || group.ExpectedCount <= 0 || group.ExpectedCount > maxSubAgentRuns {
		return SubAgentGroup{}, fmt.Errorf("%w: 子 Agent group 参数无效", ErrConflict)
	}
	if group.FailurePolicy == "" {
		group.FailurePolicy = SubAgentFailureContinue
	}
	if group.FailurePolicy != SubAgentFailureContinue && group.FailurePolicy != SubAgentFailureAbort {
		return SubAgentGroup{}, fmt.Errorf("%w: 子 Agent failure policy 无效", ErrConflict)
	}
	if group.MaxConcurrency <= 0 {
		group.MaxConcurrency = 1
	}
	if group.MaxConcurrency > maxSubAgentConcurrency {
		return SubAgentGroup{}, fmt.Errorf("%w: 子 Agent 并发数超出上限", ErrConflict)
	}
	if group.TimeoutSeconds <= 0 {
		group.TimeoutSeconds = 120
	}
	if group.TimeoutSeconds > 300 {
		return SubAgentGroup{}, fmt.Errorf("%w: 子 Agent 超时超出 5 分钟上限", ErrConflict)
	}
	if group.OutputBudgetBytes <= 0 {
		group.OutputBudgetBytes = maxSubAgentGroupOutput
	}
	if group.OutputBudgetBytes > maxSubAgentGroupOutput {
		return SubAgentGroup{}, fmt.Errorf("%w: 子 Agent 输出预算超出上限", ErrConflict)
	}
	if group.OutputBudgetBytes < group.ExpectedCount {
		return SubAgentGroup{}, fmt.Errorf("%w: 子 Agent 输出预算不足以覆盖每个 run", ErrConflict)
	}
	if group.InputBudgetBytes < 0 || group.InputBudgetBytes > maxBuiltInSubAgentPromptTotalBytes || group.ImageBudgetBytes < 0 || group.ImageBudgetBytes > maxDocumentImageTotal {
		return SubAgentGroup{}, fmt.Errorf("%w: 子 Agent 输入预算超出上限", ErrConflict)
	}
	if group.Status == "" {
		group.Status = SubAgentGroupQueued
	}
	if !validSubAgentGroupStatus(group.Status) {
		return SubAgentGroup{}, fmt.Errorf("%w: 子 Agent group 状态无效", ErrConflict)
	}
	if group.CreatedAt.IsZero() {
		group.CreatedAt = now
	}
	if group.UpdatedAt.IsZero() {
		group.UpdatedAt = now
	}
	if group.StartedAt.IsZero() && group.Status == SubAgentGroupRunning {
		group.StartedAt = now
	}
	if group.DeadlineAt.IsZero() && group.Status == SubAgentGroupRunning {
		group.DeadlineAt = now.Add(time.Duration(group.TimeoutSeconds) * time.Second)
	}
	group.SourceKinds = normalizeStringSlice(group.SourceKinds, maxRetrievalSources)
	group.ErrorSummary = limitSubAgentText(group.ErrorSummary, maxSubAgentErrorBytes)
	return group, nil
}

// ValidSubAgentProfile 供存储和外部装配层复用受控 profile 白名单。
func ValidSubAgentProfile(profile SubAgentProfile) bool { return validSubAgentProfile(profile) }

func validSubAgentGroupStatus(status SubAgentGroupStatus) bool {
	switch status {
	case SubAgentGroupQueued, SubAgentGroupRunning, SubAgentGroupCompleted, SubAgentGroupPartial, SubAgentGroupFailed, SubAgentGroupCancelled, SubAgentGroupExpired:
		return true
	default:
		return false
	}
}

// ValidSubAgentGroupStatus 让持久化实现可以在写入前拒绝未知状态。
func ValidSubAgentGroupStatus(status SubAgentGroupStatus) bool {
	return validSubAgentGroupStatus(status)
}

func validSubAgentRunStatus(status SubAgentRunStatus) bool {
	switch status {
	case SubAgentRunQueued, SubAgentRunRunning, SubAgentRunCompleted, SubAgentRunFailed, SubAgentRunCancelled, SubAgentRunExpired:
		return true
	default:
		return false
	}
}

// ValidSubAgentRunStatus 让持久化实现可以在写入前拒绝未知状态。
func ValidSubAgentRunStatus(status SubAgentRunStatus) bool { return validSubAgentRunStatus(status) }

func validSubAgentProfile(profile SubAgentProfile) bool {
	switch profile {
	case SubAgentProfileDocumentImage, SubAgentProfileMemoryRetrieval, SubAgentProfileKnowledgeRetrieval,
		SubAgentProfileConversationSearch, SubAgentProfileWebResearch, SubAgentProfileWorkspaceSearch,
		SubAgentProfileStructuredQuery, SubAgentProfileRetrievalAggregate, SubAgentProfileResearch,
		SubAgentProfileWorkspaceReview, SubAgentProfileWorkspaceChange, SubAgentProfileVerification:
		return true
	default:
		return false
	}
}

func normalizeSubAgentRun(run SubAgentRun, now time.Time) (SubAgentRun, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	run.ID = strings.TrimSpace(run.ID)
	run.GroupID = strings.TrimSpace(run.GroupID)
	run.InvocationID = strings.TrimSpace(run.InvocationID)
	run.ParentNodeID = strings.TrimSpace(run.ParentNodeID)
	run.Profile = SubAgentProfile(strings.TrimSpace(string(run.Profile)))
	run.SourceKind = strings.TrimSpace(run.SourceKind)
	if run.ID == "" {
		run.ID = newID("subagent-run")
	}
	if run.GroupID == "" || run.Profile == "" || run.Ordinal < 0 || run.Ordinal >= maxSubAgentRuns || run.Stage < 0 {
		return SubAgentRun{}, fmt.Errorf("%w: 子 Agent run 参数无效", ErrConflict)
	}
	if run.Status == "" {
		run.Status = SubAgentRunQueued
	}
	if !validSubAgentRunStatus(run.Status) {
		return SubAgentRun{}, fmt.Errorf("%w: 子 Agent run 状态无效", ErrConflict)
	}
	if run.Attempt < 0 || run.Attempt > 16 {
		return SubAgentRun{}, fmt.Errorf("%w: 子 Agent attempt 无效", ErrConflict)
	}
	if run.Stage > maxSubAgentDepth {
		return SubAgentRun{}, fmt.Errorf("%w: 子 Agent stage 超出深度上限", ErrConflict)
	}
	run.DependsOn = normalizeStringSlice(run.DependsOn, maxSubAgentRuns)
	if run.InputDigest == "" && len(run.InputMetadata) > 0 {
		encoded, _ := json.Marshal(run.InputMetadata)
		run.InputDigest = digestSubAgentText(string(encoded))
	}
	if len(run.ResultText) > maxSubAgentOutputBytes {
		run.ResultText = limitSubAgentText(run.ResultText, maxSubAgentOutputBytes)
	}
	if len(run.InputMetadata) > 0 {
		encoded, _ := json.Marshal(run.InputMetadata)
		if len(encoded) > maxSubAgentInputMetadata {
			return SubAgentRun{}, fmt.Errorf("%w: 子 Agent 输入元数据超出上限", ErrConflict)
		}
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	if run.QueuedAt.IsZero() {
		run.QueuedAt = now
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = now
	}
	if run.Error != "" {
		run.Error = boundedSubAgentError(errors.New(run.Error))
	}
	return run, nil
}

// ValidateSubAgentRunDependencies 检查持久化 run 依赖是否指向同一 group
// 的已知节点，并拒绝自依赖与循环，避免恢复时出现无法推进的队列。
func ValidateSubAgentRunDependencies(runs []SubAgentRun) error {
	ids := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		if _, exists := ids[run.ID]; exists || strings.TrimSpace(run.ID) == "" {
			return fmt.Errorf("%w: 子 Agent run ID 重复或为空", ErrConflict)
		}
		ids[run.ID] = struct{}{}
	}
	byID := make(map[string]SubAgentRun, len(runs))
	for _, run := range runs {
		byID[run.ID] = run
		for _, dependency := range run.DependsOn {
			dependency = strings.TrimSpace(dependency)
			if dependency == "" || dependency == run.ID {
				return fmt.Errorf("%w: 子 Agent run 依赖无效", ErrConflict)
			}
			if _, exists := ids[dependency]; !exists {
				return fmt.Errorf("%w: 子 Agent run 依赖不存在", ErrConflict)
			}
		}
	}
	state := make(map[string]uint8, len(runs))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("%w: 子 Agent run 依赖存在循环", ErrConflict)
		case 2:
			return nil
		}
		state[id] = 1
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func normalizeStringSlice(values []string, limit int) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	sort.Strings(result)
	return result
}

func cloneSubAgentGroup(group SubAgentGroup) SubAgentGroup {
	group.SourceKinds = append([]string(nil), group.SourceKinds...)
	return group
}

func cloneSubAgentRun(run SubAgentRun) SubAgentRun {
	run.DependsOn = append([]string(nil), run.DependsOn...)
	run.InputMetadata = cloneAnyMap(run.InputMetadata)
	run.ResultMetadata = cloneAnyMap(run.ResultMetadata)
	return run
}

func cloneAnyMap(value map[string]any) map[string]any {
	if len(value) == 0 {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func normalizeEvidence(item EvidenceItem, now time.Time) (EvidenceItem, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	item.EvidenceID = strings.TrimSpace(item.EvidenceID)
	item.SourceKind = strings.TrimSpace(item.SourceKind)
	item.SourceID = strings.TrimSpace(item.SourceID)
	item.SourceName = strings.TrimSpace(item.SourceName)
	item.Locator = strings.TrimSpace(item.Locator)
	item.Title = strings.TrimSpace(item.Title)
	item.Excerpt = strings.TrimSpace(item.Excerpt)
	item.ContentDigest = strings.TrimSpace(item.ContentDigest)
	if item.EvidenceID == "" {
		item.EvidenceID = newID("evidence")
	}
	if item.SourceKind == "" || item.Excerpt == "" {
		return EvidenceItem{}, fmt.Errorf("%w: 证据缺少来源或摘要", ErrConflict)
	}
	if item.ContentDigest == "" {
		item.ContentDigest = digestSubAgentText(item.SourceKind + "\x00" + item.SourceID + "\x00" + item.Locator + "\x00" + item.Excerpt)
	}
	if len([]byte(item.Excerpt)) > maxEvidenceExcerptBytes {
		item.Excerpt = limitSubAgentText(item.Excerpt, maxEvidenceExcerptBytes)
		item.Truncated = true
	}
	if item.RerankScore == 0 {
		item.RerankScore = item.RetrievalScore
	}
	if item.RetrievedAt.IsZero() {
		item.RetrievedAt = now
	} else {
		item.RetrievedAt = item.RetrievedAt.UTC()
	}
	item.Metadata = cloneAnyMap(item.Metadata)
	return item, nil
}

func cloneSubAgentEvidence(item EvidenceItem) EvidenceItem {
	item.Metadata = cloneAnyMap(item.Metadata)
	if item.PublishedAt != nil {
		published := item.PublishedAt.UTC()
		item.PublishedAt = &published
	}
	return item
}

// AnalyzeDocumentImages implements agent.DocumentImageSubagentRunner. 它按图片
// 分配独立 run，限制并发和输出，并将失败作为单张图片的 Error 返回给文档层。
func (m *SubAgentManager) AnalyzeDocumentImages(ctx context.Context, request agent.DocumentImageAnalysisRequest) ([]agent.DocumentImageAnalysisResult, error) {
	if m == nil {
		return nil, errors.New("子 Agent 管理器不能为空")
	}
	if !request.Runtime.SubAgentsEnabled() {
		return nil, agent.ErrSubAgentsDisabled
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, ErrConflict
	}
	if len(request.Images) == 0 || len(request.Images) > 32 {
		return nil, fmt.Errorf("文档图片数量必须在 1 到 32 之间")
	}
	if m.imageRunner == nil {
		return nil, errors.New("视觉子 Agent 未装配")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request.UserID = strings.TrimSpace(request.UserID)
	if request.UserID == "" {
		return nil, errors.New("文档图片子 Agent user_id 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.validateInvocationScope(ctx, request.InvocationID, request.UserID, "", ""); err != nil {
		return nil, err
	}
	releaseQuota, err := m.reserveInvocationGroup(ctx, request.InvocationID)
	if err != nil {
		return nil, err
	}
	defer releaseQuota()
	now := time.Now().UTC()
	var totalImageBytes int
	for _, image := range request.Images {
		if len(image.Data) == 0 || len(image.Data) > maxDocumentImageBytes || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(image.MIMEType)), "image/") {
			return nil, errors.New("文档图片大小或 MIME 类型超出子 Agent 边界")
		}
		totalImageBytes += len(image.Data)
		if totalImageBytes > maxDocumentImageTotal {
			return nil, errors.New("文档图片累计大小超出 24 MiB 子 Agent 预算")
		}
	}
	profileOptions, _ := request.Runtime.SubAgentProfile(string(SubAgentProfileDocumentImage))
	maxConcurrency := profileOptions.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = 2
	}
	timeoutSeconds := profileOptions.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 120
	}
	outputBudgetBytes := profileOptions.OutputBudgetBytes
	if outputBudgetBytes <= 0 {
		outputBudgetBytes = maxSubAgentGroupOutput
	}
	failurePolicy := SubAgentFailurePolicy(profileOptions.FailurePolicy)
	if failurePolicy == "" {
		failurePolicy = SubAgentFailureContinue
	}
	group := SubAgentGroup{
		ID: newID("subagent-group"), InvocationID: strings.TrimSpace(request.InvocationID), Profile: SubAgentProfileDocumentImage,
		Purpose: "解析文档内嵌图片并返回事实摘要", FailurePolicy: failurePolicy, ExpectedCount: len(request.Images),
		MaxConcurrency: maxConcurrency, TimeoutSeconds: timeoutSeconds, OutputBudgetBytes: outputBudgetBytes, ImageBudgetBytes: totalImageBytes,
	}
	runs := make([]SubAgentRun, 0, len(request.Images))
	for index, image := range request.Images {
		run := SubAgentRun{
			ID: newID("subagent-run"), GroupID: group.ID, InvocationID: group.InvocationID, Ordinal: index,
			Profile: SubAgentProfileDocumentImage, Status: SubAgentRunQueued,
			InputMetadata: map[string]any{"document": strings.TrimSpace(request.DocumentName), "locator": strings.TrimSpace(image.Locator), "mime_type": strings.TrimSpace(image.MIMEType), "image_bytes": len(image.Data)},
		}
		normalized, normalizeErr := normalizeSubAgentRun(run, now)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		runs = append(runs, normalized)
	}
	group, err = normalizeSubAgentGroup(group, now)
	if err != nil {
		return nil, err
	}
	if err := m.subRepo.CreateSubAgentGroup(ctx, group, runs); err != nil {
		return nil, err
	}
	group.Status = SubAgentGroupRunning
	group.StartedAt = now
	group.DeadlineAt = now.Add(time.Duration(group.TimeoutSeconds) * time.Second)
	group.QueuedCount = len(runs)
	group.UpdatedAt = now
	if err := m.subRepo.UpdateSubAgentGroup(ctx, group); err != nil {
		return nil, err
	}
	groupCtx, release := m.startGroup(group.ID, ctx, group.TimeoutSeconds)
	defer release()
	m.emit(ctx, group.InvocationID, EventSubAgentRequested, map[string]any{"group_id": group.ID, "profile": group.Profile, "count": len(runs)})
	m.emit(ctx, group.InvocationID, EventSubAgentQueued, map[string]any{"group_id": group.ID, "profile": group.Profile, "count": len(runs)})
	if group.InvocationID != "" {
		m.markInvocationWaiting(ctx, group.InvocationID, group.ID)
	}

	results := make([]agent.DocumentImageAnalysisResult, len(request.Images))
	for index, image := range request.Images {
		results[index] = agent.DocumentImageAnalysisResult{Locator: image.Locator}
	}
	workerCount := group.MaxConcurrency
	if workerCount > len(runs) {
		workerCount = len(runs)
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index] = m.runDocumentImage(groupCtx, group, runs[index], request, request.Images[index])
			}
		}()
	}
	for index := range runs {
		select {
		case jobs <- index:
		case <-groupCtx.Done():
			status := subAgentContextTerminalStatus(groupCtx.Err())
			message := boundedSubAgentError(groupCtx.Err())
			_, _ = m.subRepo.TransitionSubAgentRun(context.WithoutCancel(ctx), runs[index].ID, SubAgentRunQueued, status, message, time.Now().UTC())
			m.emitContextTerminal(group.InvocationID, group.ID, runs[index].ID, status, message)
			results[index] = agent.DocumentImageAnalysisResult{Locator: request.Images[index].Locator, Error: message}
		}
	}
	close(jobs)
	workers.Wait()
	m.finishGroup(groupCtx, group, runs, results)
	m.clearGroup(group.ID)
	return results, nil
}
