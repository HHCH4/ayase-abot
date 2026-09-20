// Package bot 实现平台机器人连接、消息事件路由和机器人配置管理。
package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/conversation"
	"Abot/internal/eventbus"
)

var (
	ErrNotFound    = errors.New("机器人不存在")
	ErrInvalid     = errors.New("机器人配置无效")
	ErrNotRunning  = errors.New("机器人未运行")
	ErrClosed      = errors.New("机器人管理器已关闭")
	ErrUnsupported = errors.New("不支持的机器人平台")
)

// Type 是平台适配器类型。
type Type string

const (
	TypeTelegram Type = "telegram"
	TypeOneBot11 Type = "onebot11"
)

// OneBot 连接模式。反向服务端是 NapCat WebSocket 客户端的标准接入方式。
const (
	OneBotModeReverseServer = "reverse-server"
	OneBotModeClient        = "client"
	DefaultOneBotListenHost = "0.0.0.0"
	DefaultOneBotListenPort = 6199
	DefaultOneBotListenPath = "/ws"
)

// Status 是机器人连接实例状态。
type Status string

const (
	StatusConfigured Status = "configured"
	StatusConnecting Status = "connecting"
	StatusRunning    Status = "running"
	StatusReady      Status = "ready"
	StatusStopped    Status = "stopped"
	StatusError      Status = "error"
)

// Bot 是机器人连接配置和运行状态。秘密字段只在进程内部使用，API 层不会直接序列化它。
type Bot struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Type             Type   `json:"type"`
	Endpoint         string `json:"endpoint,omitempty"` // 正向 WS 客户端模式的目标地址，保留用于兼容旧配置。
	OneBotMode       string `json:"onebot_mode,omitempty"`
	ListenHost       string `json:"listen_host,omitempty"`
	ListenPort       int    `json:"listen_port,omitempty"`
	ListenPath       string `json:"listen_path,omitempty"`
	GroupTriggerMode string `json:"group_trigger_mode,omitempty"`
	// AdminUserIDs are the global administrators of this bot. They are the root
	// of trust for chat commands and can only be edited from the WebUI, never
	// from a chat message.
	AdminUserIDs      []string   `json:"admin_user_ids,omitempty"`
	TelegramToken     string     `json:"-"`
	OneBotAccessToken string     `json:"-"`
	Enabled           bool       `json:"enabled"`
	Status            Status     `json:"status"`
	StatusMessage     string     `json:"status_message,omitempty"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// SaveRequest 区分“保留旧秘密字段”和“写入新秘密字段”。
type SaveRequest struct {
	Bot               Bot
	TelegramToken     *string
	OneBotAccessToken *string
}

// Repository 是机器人配置持久化接口。
type Repository interface {
	List(context.Context) ([]Bot, error)
	Get(context.Context, string) (Bot, error)
	Save(context.Context, Bot) error
	Delete(context.Context, string) error
}

// TestResult 是平台连接测试结果。
type TestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Message 是所有平台适配器统一输出的入站消息。
type Message struct {
	ID          string
	AdapterID   string
	Platform    Type
	UserID      string
	ChatID      string
	ChatType    string
	Text        string
	Attachments []agent.Attachment
	// Mentioned is set by a platform adapter when a group message explicitly
	// addresses this bot. Private messages are treated as addressed.
	Mentioned bool
	// ReplyToUserID is the author of the message this one replies to, when the
	// platform reports it. It lets /admin add target the replied-to user.
	ReplyToUserID string
	// Mentions lists the user ids this message addressed, when the platform
	// reports them directly (OneBot "at" segments do).
	Mentions []string
	// Control is an adapter-produced approval decision and never enters the
	// model prompt.
	Control *MessageControl
}

type MessageControl struct {
	Kind       string
	ApprovalID string
	// ChoiceID 是平台控件回传的结构化选项；它比 Decision 更完整，能够
	// 保留“允许本次”“会话内允许”等未来策略的语义。
	ChoiceID string
	// Decision 保留给旧适配器读取，Runtime 会再次根据 Approval.Choices
	// 校验 ChoiceID，不能仅凭这个布尔值越权。
	Decision   bool
	CallbackID string
}

type ApprovalPrompt struct {
	ApprovalID string
	ToolName   string
	Hint       string
	Choices    []agentruntime.ApprovalChoice
	ExpiresAt  *time.Time
}

type ApprovalSender interface {
	SendApproval(context.Context, Message, ApprovalPrompt) error
}

// UserInputSender 用于向平台展示普通用户问题。它与审批发送器分开，避免
// 平台适配器把“工具授权”和“用户选择题”渲染成同一种按钮。
type UserInputSender interface {
	SendUserInput(context.Context, Message, agentruntime.UserInputRequest) error
}

// Handler 是平台适配器发布消息时调用的统一回调。
type Handler func(context.Context, Message) error

// RequestTimeoutResolver 读取当前全局请求超时；配置中心更新后下一条平台消息即可使用新值。
type RequestTimeoutResolver func(context.Context) (time.Duration, error)

// AttachmentStoreRequest is the narrow Bot→Artifact boundary. The callback
// must return an Attachment containing only an immutable Ref; inline bytes are
// never handed to Runtime in the production wiring.
type AttachmentStoreRequest struct {
	UserID         string
	ConversationID string
	ProducerType   string
	ProducerID     string
	Attachment     agent.Attachment
}

type AttachmentStorer func(context.Context, AttachmentStoreRequest) (agent.Attachment, error)

// RuntimeCoordinator is the subset of durable Runtime used by Bot delivery.
// Keeping this interface small makes the message boundary testable without a
// live provider or database worker.
type RuntimeCoordinator interface {
	StartInvocation(context.Context, agent.ChatRequest) (agentruntime.Invocation, error)
	GetInvocation(context.Context, string) (agentruntime.Invocation, error)
	GetApproval(context.Context, string) (agentruntime.Approval, error)
	ListApprovals(context.Context, string, agentruntime.ApprovalStatus) ([]agentruntime.Approval, error)
	ListInvocations(context.Context, string, []agentruntime.InvocationStatus) ([]agentruntime.Invocation, error)
	Subscribe(context.Context, string, int64) ([]agentruntime.AgentEvent, <-chan agentruntime.AgentEvent, func(), error)
	CancelInvocation(context.Context, string) (agentruntime.Invocation, error)
	ResolveApproval(context.Context, string, bool, string) (agentruntime.Approval, error)
}

// platform 是 Telegram、OneBot 等连接实现必须满足的最小接口。
type platform interface {
	Run(context.Context, Handler) error
	Send(context.Context, Message, string) error
	Test(context.Context) error
	Close() error
}

var botIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Validate 检查平台配置，避免错误配置进入后台运行。
func (b Bot) Validate() error {
	b.ID = strings.TrimSpace(b.ID)
	b.Name = strings.TrimSpace(b.Name)
	b.Endpoint = strings.TrimSpace(b.Endpoint)
	b.GroupTriggerMode = strings.ToLower(strings.TrimSpace(b.GroupTriggerMode))
	if b.GroupTriggerMode != "" && b.GroupTriggerMode != "mention" && b.GroupTriggerMode != "all" {
		return fmt.Errorf("%w: 群聊触发模式 %q 无效", ErrInvalid, b.GroupTriggerMode)
	}
	if !botIDPattern.MatchString(b.ID) {
		return fmt.Errorf("%w: adapter ID 必须匹配 [a-z][a-z0-9_-]{0,63}", ErrInvalid)
	}
	if b.Name == "" {
		return fmt.Errorf("%w: 显示名称不能为空", ErrInvalid)
	}
	switch b.Type {
	case TypeTelegram:
		if strings.TrimSpace(b.TelegramToken) == "" {
			return fmt.Errorf("%w: Telegram Bot Token 不能为空", ErrInvalid)
		}
	case TypeOneBot11:
		if mode := b.effectiveOneBotMode(); mode == OneBotModeClient {
			if b.Endpoint == "" {
				return fmt.Errorf("%w: OneBot WebSocket 地址不能为空", ErrInvalid)
			}
			if err := validateWebSocketURL(b.Endpoint); err != nil {
				return fmt.Errorf("%w: OneBot WebSocket 地址无效: %v", ErrInvalid, err)
			}
		} else if mode != OneBotModeReverseServer {
			return fmt.Errorf("%w: OneBot 连接模式 %q", ErrInvalid, mode)
		} else if _, _, _, err := b.oneBotListenConfig(); err != nil {
			return fmt.Errorf("%w: OneBot 监听配置无效: %v", ErrInvalid, err)
		}
	default:
		return fmt.Errorf("%w: 平台类型 %q", ErrUnsupported, b.Type)
	}
	return nil
}

// effectiveOneBotMode 为新配置使用反向服务端默认值，并兼容旧的 endpoint 客户端配置。
func (b Bot) effectiveOneBotMode() string {
	mode := strings.TrimSpace(b.OneBotMode)
	if mode == "" {
		if strings.TrimSpace(b.Endpoint) != "" {
			return OneBotModeClient
		}
		return OneBotModeReverseServer
	}
	return mode
}

// oneBotListenConfig 返回反向 WebSocket 服务端的监听参数，并填充安全默认值。
func (b Bot) oneBotListenConfig() (string, string, string, error) {
	host := strings.TrimSpace(b.ListenHost)
	if host == "" {
		host = DefaultOneBotListenHost
	}
	if strings.ContainsAny(host, "/?#") {
		return "", "", "", errors.New("监听主机不能包含路径或查询参数")
	}
	port := b.ListenPort
	if port == 0 {
		port = DefaultOneBotListenPort
	}
	if port < 1 || port > 65535 {
		return "", "", "", fmt.Errorf("监听端口必须在 1-65535 之间，当前为 %d", port)
	}
	path := strings.TrimSpace(b.ListenPath)
	if path == "" {
		path = DefaultOneBotListenPath
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return "", "", "", errors.New("监听路径必须是不含查询参数的绝对路径")
	}
	return host, net.JoinHostPort(host, strconv.Itoa(port)), path, nil
}

// oneBotListenNetwork 让 IPv4 局域网默认走 tcp4；只有明确填写 IPv6 地址时才走 tcp6。
func oneBotListenNetwork(host string) string {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.To4() == nil {
		return "tcp6"
	}
	return "tcp4"
}

// Manager 管理多个机器人实例，并把入站消息投递到 Agent 内核。
type Manager struct {
	repository    Repository
	kernel        *agent.Kernel
	conversations *conversation.Service
	bus           *eventbus.Bus
	// requestTimeoutResolver 不缓存设置，避免系统设置修改后必须重启机器人管理器。
	requestTimeoutResolver RequestTimeoutResolver

	mu                  sync.RWMutex
	saveMu              sync.Mutex
	bots                map[string]Bot
	runtimes            map[string]runtimeEntry
	locks               map[string]*sync.Mutex
	sequence            uint64
	baseCtx             context.Context
	closed              bool
	waitGroup           sync.WaitGroup
	runtimeCoordinator  RuntimeCoordinator
	attachmentStorer    AttachmentStorer
	activeConversations map[string]string
	activeInvocations   map[string]string
	observedInvocations map[string]struct{}
	pendingApprovals    map[string][]approvalTicket
	pendingUserInputs   map[string][]userInputTicket
	progressInterval    time.Duration
	// commands is the chat command catalog. It is replaced only during
	// construction or plugin registration, never while dispatching.
	commands *CommandRegistry
	// commandRuntimeInfo/Admin are optional configuration read/write hooks
	// installed by the application after construction.
	commandRuntimeInfo      CommandRuntimeInfo
	commandRuntimeAdmin     CommandRuntimeAdmin
	commandRuntimeStats     CommandRuntimeStats
	commandDashboardUpdater CommandDashboardUpdater
	// messageConfigResolver 读取平台级管理员和唤醒配置；未装配时保持嵌入式旧行为。
	messageConfigResolver MessageConfigResolver
	// sourceNames 是没有持久化扩展的嵌入方的安全兜底；生产 SQLite 会优先使用数据库。
	sourceNames map[string]string
}

type runtimeEntry struct {
	platform platform
	cancel   context.CancelFunc
	token    uint64
}

// NewManager 从数据库恢复机器人配置；是否启动由 Start 决定。
func NewManager(ctx context.Context, repository Repository, kernel *agent.Kernel, conversations *conversation.Service) (*Manager, error) {
	if repository == nil {
		return nil, errors.New("机器人 Repository 不能为空")
	}
	if kernel == nil {
		return nil, errors.New("Agent Kernel 不能为空")
	}
	if conversations == nil {
		return nil, errors.New("对话服务不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	items, err := repository.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("加载机器人配置失败: %w", err)
	}
	commands, err := NewBuiltinCommandRegistry()
	if err != nil {
		return nil, fmt.Errorf("初始化指令注册表失败: %w", err)
	}
	m := &Manager{
		repository: repository, kernel: kernel, conversations: conversations,
		bus: eventbus.New(), bots: make(map[string]Bot), runtimes: make(map[string]runtimeEntry),
		locks: make(map[string]*sync.Mutex), baseCtx: ctx,
		activeConversations: make(map[string]string), activeInvocations: make(map[string]string), observedInvocations: make(map[string]struct{}), pendingApprovals: make(map[string][]approvalTicket), pendingUserInputs: make(map[string][]userInputTicket),
		progressInterval: 30 * time.Second,
		commands:         commands,
		sourceNames:      make(map[string]string),
	}
	for _, item := range items {
		if item.Status == "" {
			if item.Enabled {
				item.Status = StatusConfigured
			} else {
				item.Status = StatusStopped
			}
		}
		// 进程重启后旧的 running/connecting 只是上次运行留下的快照，不能伪装成当前连接。
		if item.Status == StatusRunning || item.Status == StatusConnecting {
			if item.Enabled {
				item.Status = StatusConfigured
			} else {
				item.Status = StatusStopped
			}
		}
		m.bots[item.ID] = cloneBot(item)
	}
	if _, err := m.bus.Subscribe(eventbus.MessageReceived, m.handleEvent); err != nil {
		return nil, err
	}
	return m, nil
}

// Bus 返回内核事件总线，供后续功能插件订阅平台消息。
func (m *Manager) Bus() *eventbus.Bus { return m.bus }

// SetRequestTimeoutResolver 装配配置中心的全局请求超时读取器。
func (m *Manager) SetRequestTimeoutResolver(resolver RequestTimeoutResolver) {
	m.mu.Lock()
	m.requestTimeoutResolver = resolver
	m.mu.Unlock()
}

// SetRuntimeCoordinator switches Bot message execution to durable Runtime.
func (m *Manager) SetRuntimeCoordinator(coordinator RuntimeCoordinator) {
	m.mu.Lock()
	m.runtimeCoordinator = coordinator
	m.mu.Unlock()
}

// SetAttachmentStorer installs the Artifact boundary for platform uploads.
func (m *Manager) SetAttachmentStorer(storer AttachmentStorer) {
	m.mu.Lock()
	m.attachmentStorer = storer
	m.mu.Unlock()
}

// SetProgressInterval is primarily useful to embedders and deterministic
// tests; production defaults to one visible heartbeat every 30 seconds.
func (m *Manager) SetProgressInterval(interval time.Duration) {
	m.mu.Lock()
	if interval > 0 {
		m.progressInterval = interval
	}
	m.mu.Unlock()
}

// Start 启动所有启用的机器人；单个错误只影响对应实例，不阻塞 WebUI。
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	m.baseCtx = ctx
	ids := make([]string, 0, len(m.bots))
	for id, item := range m.bots {
		if item.Enabled {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		if err := m.StartBot(id); err != nil {
			// 启动失败会写入该实例状态，管理员仍可从 WebUI 修正配置。
			slog.Warn("机器人启动失败", "adapter_id", id, "error", err)
		}
	}
	return nil
}

// List 返回机器人配置副本，秘密字段仍只存在于内存副本中。
func (m *Manager) List() []Bot {
	m.mu.RLock()
	items := make([]Bot, 0, len(m.bots))
	for _, item := range m.bots {
		items = append(items, cloneBot(item))
	}
	m.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// Get 返回指定机器人配置副本。
func (m *Manager) Get(id string) (Bot, error) {
	m.mu.RLock()
	item, ok := m.bots[strings.TrimSpace(id)]
	m.mu.RUnlock()
	if !ok {
		return Bot{}, ErrNotFound
	}
	return cloneBot(item), nil
}

// Save 保存配置并只重启目标机器人实例。
func (m *Manager) Save(ctx context.Context, request SaveRequest) (Bot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if m.isClosed() {
		return Bot{}, ErrClosed
	}
	candidate := cloneBot(request.Bot)
	candidate.ID = strings.TrimSpace(candidate.ID)
	candidate.Name = strings.TrimSpace(candidate.Name)
	candidate.Endpoint = strings.TrimSpace(candidate.Endpoint)
	candidate.GroupTriggerMode = strings.ToLower(strings.TrimSpace(candidate.GroupTriggerMode))
	old, oldErr := m.Get(candidate.ID)
	if request.TelegramToken != nil {
		candidate.TelegramToken = strings.TrimSpace(*request.TelegramToken)
	} else if oldErr == nil {
		candidate.TelegramToken = old.TelegramToken
	}
	if request.OneBotAccessToken != nil {
		candidate.OneBotAccessToken = strings.TrimSpace(*request.OneBotAccessToken)
	} else if oldErr == nil {
		candidate.OneBotAccessToken = old.OneBotAccessToken
	}
	if oldErr != nil && !errors.Is(oldErr, ErrNotFound) {
		return Bot{}, oldErr
	}
	if oldErr == nil {
		candidate.CreatedAt = old.CreatedAt
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now().UTC()
	}
	candidate.UpdatedAt = time.Now().UTC()
	candidate.Status = StatusConfigured
	candidate.StatusMessage = ""
	candidate.LastCheckedAt = nil
	if err := candidate.Validate(); err != nil {
		return Bot{}, err
	}
	if err := m.repository.Save(ctx, candidate); err != nil {
		return Bot{}, fmt.Errorf("保存机器人失败: %w", err)
	}
	// 配置已经落库后再停止旧连接，保证保存失败不会影响当前运行实例。
	m.stopRuntime(candidate.ID)
	m.mu.Lock()
	m.bots[candidate.ID] = cloneBot(candidate)
	m.mu.Unlock()
	if candidate.Enabled {
		if err := m.startBot(candidate.ID); err != nil {
			// 配置保存已经成功；连接错误显示在状态字段中，不回滚用户刚保存的配置。
			slog.Warn("机器人配置已保存但启动失败", "adapter_id", candidate.ID, "error", err)
		}
	}
	return m.Get(candidate.ID)
}

// Delete 停止实例并删除连接配置，历史对话不受影响。
func (m *Manager) Delete(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if m.isClosed() {
		return ErrClosed
	}
	id = strings.TrimSpace(id)
	if _, err := m.Get(id); err != nil {
		return err
	}
	m.stopRuntime(id)
	if err := m.repository.Delete(ctx, id); err != nil {
		return fmt.Errorf("删除机器人失败: %w", err)
	}
	m.mu.Lock()
	delete(m.bots, id)
	m.mu.Unlock()
	return nil
}

// StartBot 启动单个机器人实例。
func (m *Manager) StartBot(id string) error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	return m.startBot(id)
}

func (m *Manager) startBot(id string) error {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	item, ok := m.bots[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if _, running := m.runtimes[id]; running {
		m.mu.Unlock()
		return nil
	}
	if err := item.Validate(); err != nil {
		m.mu.Unlock()
		m.setError(id, err)
		return err
	}
	adapter, err := newPlatform(item)
	if err != nil {
		m.mu.Unlock()
		m.setError(id, err)
		return err
	}
	ctx, cancel := context.WithCancel(m.baseCtx)
	token := atomic.AddUint64(&m.sequence, 1)
	// 先登记 WaitGroup，再释放管理锁，避免 Close 与 StartBot 并发时遗漏读取循环。
	m.waitGroup.Add(1)
	m.runtimes[id] = runtimeEntry{platform: adapter, cancel: cancel, token: token}
	item.Enabled = true
	item.Status = StatusRunning
	item.StatusMessage = "已启动，正在等待平台消息"
	m.bots[id] = cloneBot(item)
	m.mu.Unlock()
	_ = m.persist(item)
	go m.runPlatform(id, token, adapter, ctx)
	return nil
}

// StopBot 停止单个机器人实例并保留配置。
func (m *Manager) StopBot(id string) error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	return m.stopBot(id)
}

func (m *Manager) stopBot(id string) error {
	id = strings.TrimSpace(id)
	item, err := m.Get(id)
	if err != nil {
		return err
	}
	m.stopRuntime(id)
	m.mu.Lock()
	item = cloneBot(m.bots[id])
	item.Enabled = false
	item.Status = StatusStopped
	item.StatusMessage = "已停止"
	item.UpdatedAt = time.Now().UTC()
	m.bots[id] = cloneBot(item)
	m.mu.Unlock()
	if persistErr := m.persist(item); persistErr != nil {
		return persistErr
	}
	return nil
}

// RestartBot 重启单个机器人实例。
func (m *Manager) RestartBot(id string) error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if err := m.stopBot(id); err != nil {
		return err
	}
	m.mu.Lock()
	item, ok := m.bots[strings.TrimSpace(id)]
	if ok {
		item.Enabled = true
		m.bots[item.ID] = item
	}
	m.mu.Unlock()
	return m.startBot(id)
}

// Test 测试已经保存的机器人配置，不改变正在运行的连接。
func (m *Manager) Test(ctx context.Context, id string) (TestResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	item, err := m.Get(id)
	if err != nil {
		return TestResult{}, err
	}
	result := TestResult{}
	var testErr error
	if item.Type == TypeOneBot11 && item.effectiveOneBotMode() == OneBotModeReverseServer && m.reverseServerRunning(item.ID, item) {
		result = TestResult{OK: true, Message: "反向 WebSocket 服务正在监听"}
	} else {
		result, testErr = testPlatform(ctx, item)
	}
	m.mu.Lock()
	current, exists := m.bots[item.ID]
	if exists {
		if testErr != nil {
			current.Status = StatusError
			current.StatusMessage = trimError(testErr)
		} else if _, running := m.runtimes[item.ID]; !running {
			current.Status = StatusReady
			current.StatusMessage = result.Message
		}
		now := time.Now().UTC()
		current.LastCheckedAt = &now
		m.bots[item.ID] = cloneBot(current)
		item = current
	}
	m.mu.Unlock()
	if exists {
		_ = m.persist(item)
	}
	if testErr != nil {
		return result, testErr
	}
	return result, nil
}

// TestPreview 测试未保存表单，避免为了获取模型目录式的配置而先写数据库。
func (m *Manager) TestPreview(ctx context.Context, item Bot) (TestResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := item.Validate(); err != nil {
		return TestResult{}, err
	}
	if item.Type == TypeOneBot11 && item.effectiveOneBotMode() == OneBotModeReverseServer && m.reverseServerRunning(item.ID, item) {
		return TestResult{OK: true, Message: "反向 WebSocket 服务正在监听"}, nil
	}
	return testPlatform(ctx, item)
}

// reverseServerRunning 判断表单配置是否就是当前已经运行的反向 WS 服务。
func (m *Manager) reverseServerRunning(id string, item Bot) bool {
	m.mu.RLock()
	_, running := m.runtimes[id]
	current, exists := m.bots[id]
	m.mu.RUnlock()
	if !running || !exists || current.Type != TypeOneBot11 || current.effectiveOneBotMode() != OneBotModeReverseServer {
		return false
	}
	_, currentAddress, currentPath, currentErr := current.oneBotListenConfig()
	_, itemAddress, itemPath, itemErr := item.oneBotListenConfig()
	return currentErr == nil && itemErr == nil && currentAddress == itemAddress && currentPath == itemPath
}

// Close 停止所有连接并等待读取循环退出。
func (m *Manager) Close() error {
	m.saveMu.Lock()
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		m.saveMu.Unlock()
		m.waitGroup.Wait()
		return nil
	}
	ids := make([]string, 0, len(m.runtimes))
	for id := range m.runtimes {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	for _, id := range ids {
		m.stopRuntime(id)
	}
	m.saveMu.Unlock()
	m.waitGroup.Wait()
	return nil
}

func (m *Manager) isClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}

func (m *Manager) runPlatform(id string, token uint64, adapter platform, ctx context.Context) {
	defer m.waitGroup.Done()
	err := adapter.Run(ctx, func(handlerCtx context.Context, message Message) error {
		message.AdapterID = id
		errs := m.bus.Publish(handlerCtx, eventbus.Event{Name: eventbus.MessageReceived, Payload: message})
		if len(errs) == 0 {
			return nil
		}
		// 消息处理错误不能让一个平台的收消息循环退出，记录后继续接收后续消息。
		slog.Warn("机器人消息处理失败", "adapter_id", id, "error", errors.Join(errs...))
		return nil
	})
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		m.finishRuntime(id, token, fmt.Errorf("平台连接断开: %w", err))
	} else {
		m.finishRuntime(id, token, nil)
	}
}

func (m *Manager) handleEvent(ctx context.Context, event eventbus.Event) error {
	message, ok := event.Payload.(Message)
	if !ok {
		return errors.New("message.received 事件负载类型无效")
	}
	return m.handleMessage(ctx, message)
}

// handleMessage 为每个平台聊天建立稳定会话，并调用同一套 Agent 内核。
func (m *Manager) handleMessage(ctx context.Context, message Message) error {
	return m.handleMessageWithRuntime(ctx, message)
}

// resolveRequestTimeout 返回机器人消息的兼容默认值或配置中心的当前值。
func (m *Manager) resolveRequestTimeout(ctx context.Context) (time.Duration, error) {
	timeout := 5 * time.Minute
	m.mu.RLock()
	resolver := m.requestTimeoutResolver
	m.mu.RUnlock()
	if resolver == nil {
		return timeout, nil
	}
	resolvedTimeout, err := resolver(ctx)
	if err != nil {
		return 0, err
	}
	if resolvedTimeout > 0 {
		timeout = resolvedTimeout
	}
	return timeout, nil
}

func (m *Manager) send(ctx context.Context, message Message, text string) error {
	return m.Send(ctx, message, text)
}

// Send 允许内置能力或后续插件向已连接的平台主动推送消息。
func (m *Manager) Send(ctx context.Context, message Message, text string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	entry, ok := m.runtimes[message.AdapterID]
	m.mu.RUnlock()
	if !ok {
		// 连接不存在时也记录完整响应正文，便于定位“生成成功但没有发出”的问题。
		slog.Error("机器人发送响应失败", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "text", text, "error", ErrNotRunning)
		return ErrNotRunning
	}
	// 发送前记录完整响应正文，确保普通回复、指令回复和主动投递走同一条日志链路。
	slog.Info("机器人发送响应", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_type", message.ChatType, "chat_id", message.ChatID, "user_id", message.UserID, "message_id", message.ID, "text", text, "text_length", len([]rune(text)))
	if err := entry.platform.Send(ctx, message, text); err != nil {
		// 发送失败仍带上原始正文，便于区分平台拒绝、连接断开和内容生成问题。
		slog.Error("机器人发送响应失败", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "text", text, "error", err)
		return err
	}
	// 成功日志单独记录结果，便于检索一条响应是否真正交给平台。
	slog.Info("机器人发送响应成功", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "text_length", len([]rune(text)))
	return nil
}

func (m *Manager) bindingLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lock := m.locks[id]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	m.locks[id] = lock
	return lock
}

func (m *Manager) stopRuntime(id string) {
	m.mu.Lock()
	entry, ok := m.runtimes[id]
	delete(m.runtimes, id)
	m.mu.Unlock()
	if !ok {
		return
	}
	entry.cancel()
	_ = entry.platform.Close()
}

func (m *Manager) finishRuntime(id string, token uint64, err error) {
	m.mu.Lock()
	entry, ok := m.runtimes[id]
	if !ok || entry.token != token {
		m.mu.Unlock()
		return
	}
	delete(m.runtimes, id)
	item := m.bots[id]
	if err != nil {
		item.Status = StatusError
		item.StatusMessage = trimError(err)
	} else {
		item.Status = StatusStopped
		item.StatusMessage = "连接已停止"
	}
	m.bots[id] = cloneBot(item)
	m.mu.Unlock()
	_ = m.persist(item)
}

func (m *Manager) setError(id string, err error) {
	m.mu.Lock()
	item, ok := m.bots[id]
	if ok {
		item.Status = StatusError
		item.StatusMessage = trimError(err)
		m.bots[id] = cloneBot(item)
	}
	m.mu.Unlock()
	if ok {
		_ = m.persist(item)
	}
}

func (m *Manager) persist(item Bot) error {
	return m.repository.Save(context.Background(), item)
}

func testPlatform(ctx context.Context, item Bot) (TestResult, error) {
	adapter, err := newPlatform(item)
	if err != nil {
		return TestResult{OK: false, Message: trimError(err)}, err
	}
	defer func() { _ = adapter.Close() }()
	if err := adapter.Test(ctx); err != nil {
		return TestResult{OK: false, Message: trimError(err)}, err
	}
	return TestResult{OK: true, Message: "连接正常"}, nil
}

func bindingFor(message Message) (string, string) {
	chatID := strings.TrimSpace(message.ChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(message.UserID)
	}
	key := strings.Join([]string{string(message.Platform), message.AdapterID, message.ChatType, chatID}, "\x00")
	hash := sha256.Sum256([]byte(key))
	short := hex.EncodeToString(hash[:])[:32]
	return "bot:" + message.AdapterID + ":" + short, "bot-" + short
}

func validateWebSocketURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return errors.New("必须使用 ws:// 或 wss:// 地址")
	}
	return nil
}

func trimError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 1000 {
		return message[:1000]
	}
	return message
}

func cloneBot(item Bot) Bot {
	if item.LastCheckedAt != nil {
		value := *item.LastCheckedAt
		item.LastCheckedAt = &value
	}
	return item
}
