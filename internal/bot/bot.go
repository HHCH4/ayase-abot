// Package bot 实现平台机器人连接、消息事件路由和机器人配置管理。
package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       Type   `json:"type"`
	Endpoint   string `json:"endpoint,omitempty"` // 正向 WS 客户端模式的目标地址，保留用于兼容旧配置。
	OneBotMode string `json:"onebot_mode,omitempty"`
	ListenHost string `json:"listen_host,omitempty"`
	ListenPort int    `json:"listen_port,omitempty"`
	ListenPath string `json:"listen_path,omitempty"`
	// OneBotFileRoot 是宿主机上对应 OneBot 容器附件目录的根路径；它属于
	// 当前机器人实例，供 file_id 解析时把容器路径映射回宿主机文件。
	OneBotFileRoot   string `json:"onebot_file_root,omitempty"`
	GroupTriggerMode string `json:"group_trigger_mode,omitempty"`
	// AdminUserIDs are the global administrators of this bot. They are the root
	// of trust for chat commands and can only be edited from the WebUI, never
	// from a chat message.
	AdminUserIDs []string `json:"admin_user_ids,omitempty"`
	// RuntimeConfig 是机器人级显式覆盖；未填写的字段继续继承配置文件和会话规则。
	RuntimeConfig     BotRuntimeConfig `json:"runtime_config,omitempty"`
	TelegramToken     string           `json:"-"`
	OneBotAccessToken string           `json:"-"`
	Enabled           bool             `json:"enabled"`
	Status            Status           `json:"status"`
	StatusMessage     string           `json:"status_message,omitempty"`
	LastCheckedAt     *time.Time       `json:"last_checked_at,omitempty"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
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
	ID        string
	AdapterID string
	Platform  Type
	UserID    string
	ChatID    string
	ChatType  string
	// SourceUMO 是已确认的统一消息来源；普通平台入站为空时由 messageSource
	// 按 adapter、消息类型和会话 ID 推导，动作恢复时优先使用该值。
	SourceUMO string
	// 原始消息 ID 与接收事件 ID 分开，供平台引用回复使用。
	ReplyMessageID string
	// ReplyToMessageID 是当前入站消息引用的上一条平台消息 ID，供审批通知
	// 的引用回复映射使用；普通回复仍使用 ReplyMessageID。
	ReplyToMessageID string
	// 平台事件元数据与配置状态由入口设置，不进入模型提示词。
	IsSelf        bool
	AtAll         bool
	UniqueSession bool
	ReplyMention  bool
	ReplyQuote    bool
	// IsProactive 标记由明确 Follow-up/管理员配置触发的主动投递；只有这类
	// 出站消息执行 Bot Runtime 的冷却和小时上限，用户明确提问不被限流吞掉。
	IsProactive bool
	// TextFormat 由表达层设置；适配器只接受 plain 或经过转义的安全 html。
	TextFormat string
	// RuntimeTurnID 只在 Bot Runtime 内部流转，用于把决策和出站动作关联到自然话轮。
	RuntimeTurnID string
	// RuntimeConversationID 只用于持久化清理和审计关联，不会进入模型提示词。
	RuntimeConversationID string
	RuntimeActionSequence int
	Restored              bool
	// runtimeInboxPersisted 表示平台事件已经先落入持久 Inbox；它只在进程内传播，
	// 用于让后续消息处理更新同一条记录而不是重复插入。
	runtimeInboxPersisted bool
	// runtimeInboxRetain 表示来源队列已满；此时保留 Inbox 事实而不是把“无忙碌提示”
	// 错误实现成静默丢消息，恢复器会在来源有容量后再次尝试。
	runtimeInboxRetain bool
	// AutoName 是平台提供的群名、昵称或用户名，仅用于 UMO 目录展示；它不
	// 参与来源键计算，也不会覆盖用户通过 /name 设置的手工别名。
	AutoName    string
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
	// Source 保存触发审批的稳定 UMO，管理员可以据此区分私聊、群聊以及不同平台实例。
	Source string
	// Requester 保存触发审批的用户标识或显示名，避免管理员只看到工具名而无法判断请求来源。
	Requester string
}

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
	ResolveApprovalChoice(context.Context, string, string, string) (agentruntime.Approval, error)
}

// platform 是 Telegram、OneBot 等连接实现必须满足的最小接口。
type platform interface {
	RunEvents(context.Context, EventHandler) error
	Capabilities() PlatformCapabilities
	// 适配器只接收统一的抽象动作，平台差异必须封装在 DispatchAction 内。
	DispatchAction(context.Context, PlatformAction) (PlatformActionResult, error)
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
	if err := b.RuntimeConfig.Validate(); err != nil {
		return fmt.Errorf("%w: Bot Runtime 配置无效: %v", ErrInvalid, err)
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
	observedOrder       []string
	pendingApprovals    map[string][]approvalTicket
	pendingUserInputs   map[string][]userInputTicket
	// runtimeState 持久化 Bot Inbox、来源上下文和管理员路由。
	runtimeState RuntimeStateRepository
	// turnMu 只保护短等待窗和来源执行锁；长期上下文不再保存在进程内存。
	turnMu                 sync.Mutex
	pendingTurns           map[string]*pendingTurn
	sourceRunning          map[string]bool
	turnSequence           uint64
	turnWake               chan string
	turnWakePending        map[string]struct{}
	turnContext            context.Context
	turnCancel             context.CancelFunc
	inboxRecovering        map[string]struct{}
	botActive              map[string]int
	botSlotWake            chan struct{}
	groupImageCaption      func(context.Context, string, agent.Attachment) (string, error)
	runtimeFollowUpCreator RuntimeFollowUpCreator
	// 限速时间窗仅保留有界活跃来源，避免长期运行的树莓派积累状态。
	rateWindows   map[string][]time.Time
	rateOrder     []string
	mentionWait   map[string]time.Time
	proactiveLast map[string]time.Time
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
	// done 用于破坏性配置操作等待平台读取循环真正退出，避免删除状态后旧循环继续写回数据库。
	done  chan struct{}
	token uint64
	// lifecycle 只表示当前进程中适配器的真实连接阶段；心跳不能把断线实例
	// 重新伪装成 online。
	lifecycle string
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
		commands: commands, sourceNames: make(map[string]string),
		pendingTurns:    make(map[string]*pendingTurn),
		sourceRunning:   make(map[string]bool),
		turnWakePending: make(map[string]struct{}),
		inboxRecovering: make(map[string]struct{}),
		botActive:       make(map[string]int), botSlotWake: make(chan struct{}, 1),
		rateWindows: make(map[string][]time.Time), mentionWait: make(map[string]time.Time), proactiveLast: make(map[string]time.Time),
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
	if recovery, ok := m.runtimeActionRecoveryRepository(); ok {
		// 这些动作来自上一次进程；执行前已经越过网络边界，重启后无法安全
		// 推断“未执行”，统一转为 unknown，避免恢复流程重复发送副作用。
		for _, id := range ids {
			if err := recovery.MarkUnfinishedActionsUnknown(ctx, id, time.Now().UTC()); err != nil {
				slog.Warn("恢复 Bot Runtime 未完成动作失败", "adapter_id", id, "error", err)
			}
		}
	}
	// 先启动固定话轮 worker，再打开平台连接，避免平台刚连上时的突发消息
	// 走无界 goroutine 兜底路径。
	m.startTurnWorkers(ctx)
	for _, id := range ids {
		if err := m.StartBot(id); err != nil {
			// 启动失败会写入该实例状态，管理员仍可从 WebUI 修正配置。
			slog.Warn("机器人启动失败", "adapter_id", id, "error", err)
		}
	}
	// Runtime 先于适配器恢复；连接启动后按持久化回传目标重新订阅排队任务。
	m.restoreBotObservers(ctx)
	// 平台开始重连后恢复持久 Inbox；未连接完成的最终回复由既有恢复投递逻辑等待。
	m.launchInboxRestore(ctx, "")
	m.startRuntimeHeartbeat(ctx)
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
	candidate.OneBotFileRoot = strings.TrimSpace(candidate.OneBotFileRoot)
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
	m.mu.RLock()
	runtimeState := m.runtimeState
	m.mu.RUnlock()
	if runtimeState != nil {
		if err := runtimeState.DeleteBotState(ctx, id); err != nil {
			return fmt.Errorf("删除机器人运行状态失败: %w", err)
		}
	}
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
	m.runtimes[id] = runtimeEntry{platform: adapter, cancel: cancel, done: make(chan struct{}), token: token, lifecycle: "starting"}
	item.Enabled = true
	item.Status = StatusRunning
	item.StatusMessage = "正在启动平台连接"
	m.bots[id] = cloneBot(item)
	m.mu.Unlock()
	_ = m.persist(item)
	// 连接循环真正开始后再进入 online；starting 状态用于区分配置已保存但
	// 平台还没有完成监听或握手的时间窗口。
	m.updateInstanceState(ctx, id, "starting", "unavailable", false, false)
	go m.runPlatform(id, token, adapter, ctx)
	// 单独启动或重启 Bot 时也要恢复它自己的 pending Inbox；不能只依赖进程
	// 启动阶段的全量恢复，否则运行中的 WebUI 重启操作会留下永久积压。
	m.launchInboxRestore(ctx, id)
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
	m.updateInstanceState(m.runtimeContext(), id, "stopped", "offline", false, false)
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
	// Inbox 已持久化，关停时只取消尚未触发的内存计时器；下次启动会恢复。
	m.mu.Lock()
	turnCancel := m.turnCancel
	m.turnContext = nil
	m.turnCancel = nil
	m.mu.Unlock()
	if turnCancel != nil {
		turnCancel()
	}
	m.turnMu.Lock()
	for _, turn := range m.pendingTurns {
		if turn != nil && turn.timer != nil {
			turn.timer.Stop()
		}
	}
	m.pendingTurns = make(map[string]*pendingTurn)
	m.turnWakePending = make(map[string]struct{})
	m.turnMu.Unlock()
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
	m.mu.RLock()
	entry, entryOK := m.runtimes[id]
	done := entry.done
	m.mu.RUnlock()
	defer func() {
		if entryOK && done != nil {
			close(done)
		}
	}()
	handle := func(handlerCtx context.Context, event PlatformEvent) error {
		return m.handlePlatformEvent(handlerCtx, id, event)
	}
	// 内置适配器必须统一发布 PlatformEvent；不再保留绕过事件网关的旧消息入口。
	err := adapter.RunEvents(ctx, handle)
	if err != nil {
		m.finishRuntime(id, token, fmt.Errorf("平台连接断开: %w", err))
	} else {
		m.finishRuntime(id, token, nil)
	}
}

// handlePlatformEvent 是平台事件到 Bot Runtime 的唯一入口。非消息事件只记录和
// 更新状态，不会被误投递给 Agent；消息事件才发布给聊天策略和 Inbox 聚合器。
func (m *Manager) handlePlatformEvent(ctx context.Context, botID string, event PlatformEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	event.BotID = botID
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = event.ReceivedAt
	}
	// 平台事件 ID 只在单个适配器内保证唯一；数据库表由多个 Bot 共享，
	// 因此必须把 bot_id 纳入最终幂等键，避免不同 Bot 的 update_id 互相吞掉。
	if eventID := strings.TrimSpace(event.ID); eventID != "" && !strings.HasPrefix(eventID, botID+":") {
		event.ID = botID + ":" + eventID
	}
	// 嵌入式适配器可能不提供平台事件 ID；先用稳定事件字段补齐，避免
	// SQLite 去重把不同事件错误地视为同一条，或因空主键无法进入事实链。
	if strings.TrimSpace(event.ID) == "" {
		event.ID = newRuntimeID("platform-event", botID, string(event.Platform), string(event.EventType), event.MessageID, event.SourceUMO, event.UserID, event.OccurredAt.UTC().Format(time.RFC3339Nano))
	}
	m.mu.RLock()
	runtimeState := m.runtimeState
	m.mu.RUnlock()
	// 消息先写入 Inbox，再记录平台事件和发布总线。这样平台重试、进程崩溃或
	// 总线订阅失败时，都不会出现“平台事件已去重但用户消息没有事实记录”的窗口。
	isMessageEvent := event.Message != nil && (event.EventType == PlatformEventMessageCreated || (event.EventType == PlatformEventApprovalCallback && event.Message.Control != nil))
	var message Message
	if isMessageEvent {
		message = *event.Message
		normalizePlatformMessage(event, &message)
		// 适配器有时只把稳定身份放在 Message 中；回填 PlatformEvent 后，
		// 事件审计、来源状态和平台去重使用同一份 UMO/会话元数据。
		if strings.TrimSpace(event.SourceUMO) == "" {
			event.SourceUMO = messageSource(message)
		}
		if strings.TrimSpace(event.ChatType) == "" {
			event.ChatType = message.ChatType
		}
		if strings.TrimSpace(event.ChatID) == "" {
			event.ChatID = message.ChatID
		}
		if strings.TrimSpace(event.UserID) == "" {
			event.UserID = message.UserID
		}
		if strings.TrimSpace(event.MessageID) == "" {
			event.MessageID = message.ID
		}
		if event.Text == "" {
			event.Text = message.Text
		}
		if runtimeState != nil {
			if err := m.persistRuntimeMessageInbox(ctx, event, &message); err != nil {
				slog.Error("保存机器人消息 Inbox 失败，事件处理已中断", "adapter_id", botID, "event_id", event.ID, "error", err)
				return err
			}
			message.runtimeInboxPersisted = true
		}
	}
	if deduplicator, ok := runtimeState.(PlatformEventDeduplicator); ok {
		var created bool
		err := retryRuntimePersistence(ctx, "保存平台事件", func(persistCtx context.Context) error {
			var retryErr error
			created, retryErr = deduplicator.RecordPlatformEventIfNew(persistCtx, event)
			return retryErr
		})
		if err != nil {
			slog.Error("保存平台事件失败，事件处理已中断", "adapter_id", botID, "event_type", event.EventType, "event_id", event.ID, "error", err)
			return err
		}
		if !created {
			slog.Info("平台事件重复投递，已跳过", "adapter_id", botID, "event_id", event.ID, "event_type", event.EventType)
			return nil
		}
	} else if recorder, ok := m.runtimeStateRecorder(); ok {
		if err := retryRuntimePersistence(ctx, "保存平台事件", func(persistCtx context.Context) error {
			return recorder.RecordPlatformEvent(persistCtx, event)
		}); err != nil {
			slog.Error("保存平台事件失败，事件处理已中断", "adapter_id", botID, "event_type", event.EventType, "event_id", event.ID, "error", err)
			return err
		}
	}
	// 所有平台事件都会刷新来源活跃度；非消息事件也不能因为没有 Message
	// 结构就从 Runtime 事实链中消失。它们不会创建 Agent Invocation。
	if event.SourceUMO != "" {
		now := event.OccurredAt
		if now.IsZero() {
			now = event.ReceivedAt
		}
		if err := m.touchRuntimeEventSource(ctx, event, now); err != nil {
			slog.Warn("更新 Bot Runtime 平台事件来源失败", "adapter_id", botID, "event_type", event.EventType, "error", err)
		}
	}
	switch event.EventType {
	case PlatformEventPlatformConnected:
		m.setRuntimeLifecycle(botID, "online")
		m.updateInstanceState(ctx, botID, "online", "available", true, false)
		return m.persistRuntimePlatformEventInbox(ctx, event)
	case PlatformEventPlatformDisconnected:
		m.setRuntimeLifecycle(botID, "degraded")
		m.updateInstanceState(ctx, botID, "degraded", "unavailable", true, false)
		return m.persistRuntimePlatformEventInbox(ctx, event)
	}
	if !isMessageEvent {
		return m.persistRuntimePlatformEventInbox(ctx, event)
	}
	m.updateInstanceState(ctx, botID, "online", "available", true, false)
	publishErr := retryRuntimePersistence(ctx, "发布平台消息事件", func(publishCtx context.Context) error {
		errs := m.bus.Publish(publishCtx, eventbus.Event{Name: eventbus.MessageReceived, Payload: message})
		if len(errs) == 0 {
			return nil
		}
		return errors.Join(errs...)
	})
	if publishErr == nil {
		return nil
	}
	// 发布失败时把原始消息补入待处理 Inbox，保证平台循环继续接收消息而不
	// 丢失用户请求；下次启动恢复流程会重新聚合并执行。若补偿也失败，向
	// 适配器返回错误，让连接层和日志明确暴露持久化故障，而不是静默丢消息。
	if recoveryErr := m.persistRuntimeMessageInbox(ctx, event, &message); recoveryErr != nil {
		slog.Error("机器人消息处理失败且无法写入恢复 Inbox", "adapter_id", botID, "event_id", event.ID, "error", errors.Join(publishErr, recoveryErr))
		return errors.Join(publishErr, recoveryErr)
	}
	slog.Warn("机器人消息处理失败，已写入恢复 Inbox", "adapter_id", botID, "event_id", event.ID, "error", publishErr)
	return nil
}

// persistRuntimePlatformEventInbox 把撤回、反应、成员变化和连接状态也落入
// Inbox 事实链；这些记录立即标记为 processed，不会被恢复流程误送进 Agent。
func (m *Manager) persistRuntimePlatformEventInbox(ctx context.Context, event PlatformEvent) error {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil || strings.TrimSpace(event.ID) == "" {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		slog.Warn("序列化 Bot Runtime 平台事件失败", "event_id", event.ID, "error", err)
		return err
	}
	now := time.Now().UTC()
	if err := retryRuntimePersistence(ctx, "保存 Bot Runtime 平台事件 Inbox", func(persistCtx context.Context) error {
		_, err := repository.SaveInbox(persistCtx, InboxEvent{
			ID: newRuntimeID("platform-inbox", event.BotID, event.ID), BotID: event.BotID, Source: event.SourceUMO,
			UserID: event.UserID, EventType: event.EventType, Payload: payload,
			Status: "processed", CreatedAt: now, UpdatedAt: now,
		})
		return err
	}); err != nil {
		slog.Error("保存 Bot Runtime 平台事件 Inbox 失败", "event_id", event.ID, "event_type", event.EventType, "error", err)
		return err
	}
	return nil
}

// persistRuntimeMessageInbox 是平台消息发布失败时的最终补偿路径。它只保存
// 已经归一化的消息，不执行 Agent；重启恢复器会在运行时重新检查来源和配置。
func (m *Manager) persistRuntimeMessageInbox(ctx context.Context, event PlatformEvent, message *Message) error {
	if message == nil {
		return errors.New("机器人消息不能为空")
	}
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return errors.New("Bot Runtime Inbox 未装配")
	}
	normalizePlatformMessage(event, message)
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("序列化恢复消息失败: %w", err)
	}
	now := time.Now().UTC()
	eventRecord := InboxEvent{
		ID: runtimeInboxID(*message), BotID: strings.TrimSpace(message.AdapterID), Source: messageSource(*message),
		UserID: strings.TrimSpace(message.UserID), ConversationID: strings.TrimSpace(message.RuntimeConversationID),
		EventType: PlatformEventMessageCreated, Payload: payload, Status: "received", CreatedAt: now, UpdatedAt: now,
	}
	created := false
	if err := retryRuntimePersistence(ctx, "保存恢复消息 Inbox", func(persistCtx context.Context) error {
		var saveErr error
		created, saveErr = repository.SaveInbox(persistCtx, eventRecord)
		return saveErr
	}); err != nil {
		return err
	}
	if !created {
		// 平台重复投递优先复用已持久化的 Artifact 引用；如果上一次进程在
		// 原始消息入库后、附件化完成前崩溃，则针对同一事实重试一次附件化。
		if reader, ok := repository.(RuntimeInboxReader); ok {
			stored, readErr := reader.GetInbox(ctx, eventRecord.ID)
			if readErr != nil {
				return fmt.Errorf("读取已有 Bot Runtime Inbox 失败: %w", readErr)
			}
			// 已经消费过的事实不再触碰平台临时附件路径；平台重投稍后
			// 仍会经过平台事件幂等检查，不能因为临时文件过期而把已完成
			// 的消息重新变成一次附件解析失败。
			if stored.Status == "processed" {
				return nil
			}
			var storedMessage Message
			if len(stored.Payload) > 0 && json.Unmarshal(stored.Payload, &storedMessage) == nil && botMessageAttachmentsAreStored(storedMessage) {
				*message = storedMessage
				message.runtimeInboxPersisted = true
				return nil
			}
			if err := m.prepareRuntimeMessageArtifacts(ctx, message); err != nil {
				return err
			}
			updatedPayload, marshalErr := json.Marshal(message)
			if marshalErr != nil {
				return fmt.Errorf("序列化重试后的 Artifact 消息失败: %w", marshalErr)
			}
			eventRecord.Status = stored.Status
			if eventRecord.Status != "received" && eventRecord.Status != "pending" {
				eventRecord.Status = "received"
			}
			eventRecord.QueueLimit = stored.QueueLimit
			eventRecord.Payload = updatedPayload
			if updater, updateOK := repository.(RuntimeInboxPayloadUpdater); updateOK {
				return retryRuntimePersistence(ctx, "回写重试后的 Artifact 化 Inbox 消息", func(updateCtx context.Context) error {
					return updater.UpdateInboxPayload(updateCtx, eventRecord)
				})
			}
			return errors.New("Bot Runtime Inbox 未提供 Artifact 回写能力")
		}
		// 没有读取扩展的嵌入式仓储只能保留已存在事实，不能凭空覆盖它。
		return nil
	}
	// 大附件必须在持久 Inbox 事实建立后立即进入 Artifact 边界；如果进程在
	// 这一步崩溃，received 记录会由恢复入口重新执行同一套转换。
	if err := m.prepareRuntimeMessageArtifacts(ctx, message); err != nil {
		return err
	}
	updatedPayload, marshalErr := json.Marshal(message)
	if marshalErr != nil {
		return fmt.Errorf("序列化 Artifact 化消息失败: %w", marshalErr)
	}
	eventRecord.Source = messageSource(*message)
	eventRecord.UserID = strings.TrimSpace(message.UserID)
	eventRecord.ConversationID = strings.TrimSpace(message.RuntimeConversationID)
	eventRecord.Payload = updatedPayload
	if updater, ok := repository.(RuntimeInboxPayloadUpdater); ok {
		if err := retryRuntimePersistence(ctx, "回写 Artifact 化 Inbox 消息", func(updateCtx context.Context) error {
			return updater.UpdateInboxPayload(updateCtx, eventRecord)
		}); err != nil {
			return fmt.Errorf("回写 Artifact 化 Inbox 失败: %w", err)
		}
	} else if len(message.Attachments) > 0 {
		return errors.New("Bot Runtime Inbox 未提供 Artifact 回写能力")
	}
	return nil
}

// botMessageAttachmentsAreStored 判断持久载荷是否已经完成 Artifact 化；空附件
// 也视为可复用，避免平台重复投递时把正常消息误判为附件恢复任务。
func botMessageAttachmentsAreStored(message Message) bool {
	for _, attachment := range message.Attachments {
		if attachment.Ref == nil || len(attachment.Data) > 0 {
			return false
		}
	}
	return true
}

// prepareRuntimeMessageArtifacts 在消息事实第一次落盘前把平台上传物转成不可变
// Artifact 引用。这样 Inbox 永远不承担大块二进制和易失临时路径的生命周期。
func (m *Manager) prepareRuntimeMessageArtifacts(ctx context.Context, message *Message) error {
	if message == nil || len(message.Attachments) == 0 {
		return nil
	}
	allRefs := true
	for _, attachment := range message.Attachments {
		if attachment.Ref == nil {
			allRefs = false
			break
		}
	}
	if allRefs {
		return nil
	}
	m.mu.RLock()
	storer := m.attachmentStorer
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	// 没有 Runtime 的嵌入式消息入口允许保留旧的内联附件；生产 Runtime
	// 必须装配 Artifact 存储器，否则宁可拒绝也不能把二进制写进 Inbox。
	if storer == nil && coordinator == nil {
		return nil
	}
	config, err := m.resolveMessageConfig(ctx, *message)
	if err != nil {
		return fmt.Errorf("读取附件来源配置失败: %w", err)
	}
	message.UniqueSession = config.Platform.UniqueSession
	userID, conversationID, err := m.conversationForMessage(ctx, *message)
	if err != nil {
		return fmt.Errorf("为附件建立会话边界失败: %w", err)
	}
	stored, err := m.storeMessageAttachments(ctx, *message, userID, conversationID)
	if err != nil {
		return fmt.Errorf("保存机器人附件失败: %w", err)
	}
	message.Attachments = stored
	message.RuntimeConversationID = conversationID
	return nil
}

// normalizePlatformMessage 补齐平台事件已经提供的稳定字段，确保 Inbox 主键、UMO
// 和后续动作引用使用同一份消息身份，即使适配器只把字段放在 PlatformEvent 上。
func normalizePlatformMessage(event PlatformEvent, message *Message) {
	if message == nil {
		return
	}
	if strings.TrimSpace(message.AdapterID) == "" {
		message.AdapterID = strings.TrimSpace(event.BotID)
	}
	if message.Platform == "" {
		message.Platform = event.Platform
	}
	if strings.TrimSpace(message.ID) == "" {
		message.ID = strings.TrimSpace(event.MessageID)
	}
	if strings.TrimSpace(message.ID) == "" {
		message.ID = strings.TrimSpace(event.ID)
	}
	if strings.TrimSpace(message.SourceUMO) == "" {
		message.SourceUMO = strings.TrimSpace(event.SourceUMO)
	}
	if strings.TrimSpace(message.ChatType) == "" {
		message.ChatType = strings.TrimSpace(event.ChatType)
	}
	if strings.TrimSpace(message.ChatID) == "" {
		message.ChatID = strings.TrimSpace(event.ChatID)
	}
	if strings.TrimSpace(message.UserID) == "" {
		message.UserID = strings.TrimSpace(event.UserID)
	}
	if strings.TrimSpace(message.ReplyToMessageID) == "" {
		message.ReplyToMessageID = strings.TrimSpace(event.ReplyToMessageID)
	}
	if message.Text == "" {
		message.Text = event.Text
	}
	if len(message.Attachments) == 0 && len(event.Attachments) > 0 {
		message.Attachments = append([]agent.Attachment(nil), event.Attachments...)
	}
	if !message.Mentioned && event.MentionsBot {
		message.Mentioned = true
	}
	if len(message.Mentions) == 0 && len(event.MentionedUserIDs) > 0 {
		message.Mentions = append([]string(nil), event.MentionedUserIDs...)
	}
}

const runtimePersistenceAttempts = 4

// retryRuntimePersistence 将 SQLite 忙、短暂连接抖动和事件总线瞬态错误纳入
// 统一的有界重试；超过次数后返回错误，由上层决定中断或写入恢复 Inbox。
func retryRuntimePersistence(ctx context.Context, operation string, fn func(context.Context) error) error {
	if fn == nil {
		return errors.New(operation + "操作为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx := context.WithoutCancel(ctx)
	var lastErr error
	for attempt := 1; attempt <= runtimePersistenceAttempts; attempt++ {
		if err := fn(persistCtx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt == runtimePersistenceAttempts {
			break
		}
		backoff := time.Duration(attempt*150) * time.Millisecond
		slog.Warn("Bot Runtime 持久化失败，准备重试", "operation", operation, "attempt", attempt, "next_attempt", attempt+1, "backoff_ms", backoff.Milliseconds(), "error", lastErr)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return fmt.Errorf("%s被取消: %w", operation, lastErr)
		case <-timer.C:
		}
	}
	return fmt.Errorf("%s重试 %d 次后仍失败: %w", operation, runtimePersistenceAttempts, lastErr)
}

func (m *Manager) runtimeStateRecorder() (PlatformEventRecorder, bool) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	recorder, ok := repository.(PlatformEventRecorder)
	return recorder, ok
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
	return m.sendConfigured(ctx, message, text, false)
}

// sendRaw 将一段文本交给平台；分段逻辑在调用前决定，避免递归拆分。
func (m *Manager) sendRaw(ctx context.Context, message Message, text string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// 非表达层调用也统一应用当前平台样式，然后以纯文本发送。
	if config, configErr := m.resolveMessageConfig(ctx, message); configErr == nil {
		text = config.Platform.ReplyPrefix + text
		message.ReplyMention = config.Platform.ReplyMention
		message.ReplyQuote = config.Platform.ReplyQuote
		// 私聊使用独立开关；群聊仍遵循原有引用设置。
		if !isGroupChat(message.ChatType) {
			message.ReplyQuote = config.Platform.PrivateReplyQuote
		}
	} else {
		slog.Warn("读取平台回复配置失败", "adapter_id", message.AdapterID, "error", configErr)
	}
	return m.sendRawPrepared(ctx, message, text, "plain")
}

// sendRawPrepared 投递表达层已经规范化的一段文本，不再次添加前缀或引用。
func (m *Manager) sendRawPrepared(ctx context.Context, message Message, text, format string) error {
	if !m.runtimeActionAllowed(ctx, message, "send_text") {
		return errors.New("当前 Bot 未允许发送文本动作")
	}
	message.TextFormat = strings.TrimSpace(format)
	idempotencyKey := newRuntimeID("send", message.AdapterID, string(message.Platform), message.ChatID, message.ID, message.RuntimeTurnID, strconv.Itoa(message.RuntimeActionSequence), text)
	action, execute, actionErr := m.prepareRuntimeAction(ctx, message, "send_text", idempotencyKey, text)
	if actionErr != nil {
		return fmt.Errorf("保存平台发送动作失败: %w", actionErr)
	}
	if !execute {
		return nil
	}
	// 发送前记录完整响应正文，确保普通回复、指令回复和主动投递走同一条日志链路。
	slog.Info("机器人发送响应", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_type", message.ChatType, "chat_id", message.ChatID, "user_id", message.UserID, "message_id", message.ID, "text", text, "text_length", len([]rune(text)))
	platformResult, err := m.executeRuntimeAction(ctx, PlatformAction{Type: "send_text", Message: message, Text: text})
	if errors.Is(err, ErrNotRunning) && message.Restored {
		// 重启恢复的排队任务可能早于 OneBot 重连完成；仅对确定未发送的错误等待连接。
		deadline := time.NewTimer(2 * time.Minute)
		defer deadline.Stop()
		for errors.Is(err, ErrNotRunning) {
			pause := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				pause.Stop()
				return ctx.Err()
			case <-deadline.C:
				pause.Stop()
				return err
			case <-pause.C:
			}
			platformResult, err = m.executeRuntimeAction(ctx, PlatformAction{Type: "send_text", Message: message, Text: text})
		}
	}
	if err != nil {
		// 发送失败仍带上原始正文，便于区分平台拒绝、连接断开和内容生成问题。
		slog.Error("机器人发送响应失败", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "text", text, "error", err)
		m.finishRuntimeAction(ctx, action, "failed", err.Error())
		return err
	}
	// 成功日志单独记录结果，便于检索一条响应是否真正交给平台。
	slog.Info("机器人发送响应成功", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "text_length", len([]rune(text)))
	resultBytes, marshalErr := json.Marshal(platformResult)
	result := platformResult.Raw
	if marshalErr == nil {
		result = string(resultBytes)
	}
	m.finishRuntimeAction(ctx, action, "completed", result)
	m.updateInstanceState(ctx, message.AdapterID, "online", "available", false, true)
	return nil
}

// runtimeActionAllowed 在执行平台副作用前重新读取机器人级动作权限；空映射
// 表示采用平台默认能力，显式 false 才会阻止动作。
func (m *Manager) runtimeActionAllowed(ctx context.Context, message Message, action string) bool {
	config, err := m.resolveMessageConfig(ctx, message)
	if err != nil {
		slog.Warn("读取 Bot Runtime 动作权限失败", "adapter_id", message.AdapterID, "action", action, "error", err)
		return false
	}
	if enabled, exists := config.Extensions.ActionPermissions[strings.TrimSpace(action)]; exists && !enabled {
		slog.Info("Bot Runtime 动作被权限配置阻止", "adapter_id", message.AdapterID, "action", action, "source", messageSource(message))
		return false
	}
	if strings.TrimSpace(action) == "create_follow_up" {
		if !config.Extensions.FollowUpEnabled {
			slog.Info("Bot Runtime Follow-up 被配置关闭", "adapter_id", message.AdapterID, "source", messageSource(message))
			return false
		}
		if !explicitFollowUpRequest(message.Text) {
			slog.Info("Bot Runtime 拒绝非明确请求创建 Follow-up", "adapter_id", message.AdapterID, "source", messageSource(message))
			return false
		}
	}
	return true
}

// explicitFollowUpRequest 是动作层的第二道确定性门槛；模型即使误调用工具，
// 普通闲聊也不能创建后台任务。真正的时间和数量校验仍由调度器负责。
func explicitFollowUpRequest(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false
	}
	for _, marker := range []string{"提醒", "记得", "稍后", "过一会", "之后", "定时", "每天", "每周", "每月", "跟进", "follow-up", "follow up", "remind", "later", "schedule"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
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
	// 先持久化 draining，再取消运行上下文，避免重启或管理操作期间把正在收尾的实例误认为仍在线。
	m.updateInstanceState(m.runtimeContext(), id, "draining", "unavailable", false, false)
	entry.cancel()
	_ = entry.platform.Close()
	if entry.done != nil {
		// 适配器关闭应当让 RunEvents 尽快返回；这里设置有界等待，不能因为
		// 第三方平台实现卡死而永久阻塞 WebUI 的删除或重启操作。
		select {
		case <-entry.done:
		case <-time.After(10 * time.Second):
			slog.Warn("等待平台读取循环退出超时", "adapter_id", id)
		}
	}
	// 适配器关闭后明确落为 stopped，下一次启动会创建新的运行实例和生命周期记录。
	m.updateInstanceState(m.runtimeContext(), id, "stopped", "offline", false, false)
}

// setRuntimeLifecycle 更新进程内连接阶段；持久状态由统一事件处理器另行写入。
// 该短状态只用于心跳筛选，不承载业务数据，实例结束时随运行时条目一起回收。
func (m *Manager) setRuntimeLifecycle(id, lifecycle string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.runtimes[id]
	if !ok {
		return
	}
	entry.lifecycle = strings.TrimSpace(lifecycle)
	m.runtimes[id] = entry
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
	state := "stopped"
	availability := "offline"
	if err != nil {
		state = "degraded"
		availability = "unavailable"
	}
	m.updateInstanceState(m.runtimeContext(), id, state, availability, false, false)
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
	parts := []string{string(message.Platform), message.AdapterID, message.ChatType, chatID}
	// 群成员隔离仅改变内部会话键，平台回复仍发回原群。
	if message.UniqueSession && isGroupChat(message.ChatType) {
		parts = append(parts, strings.TrimSpace(message.UserID))
	}
	key := strings.Join(parts, "\x00")
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
	item.AdminUserIDs = append([]string(nil), item.AdminUserIDs...)
	item.RuntimeConfig.FollowUpAllowedSources = append([]string(nil), item.RuntimeConfig.FollowUpAllowedSources...)
	item.RuntimeConfig.AllowedReadOnlyTools = append([]string(nil), item.RuntimeConfig.AllowedReadOnlyTools...)
	if item.RuntimeConfig.ActionPermissions != nil {
		item.RuntimeConfig.ActionPermissions = make(map[string]bool, len(item.RuntimeConfig.ActionPermissions))
		for key, value := range item.RuntimeConfig.ActionPermissions {
			item.RuntimeConfig.ActionPermissions[key] = value
		}
	}
	if item.LastCheckedAt != nil {
		value := *item.LastCheckedAt
		item.LastCheckedAt = &value
	}
	return item
}
