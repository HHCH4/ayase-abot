package bot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// InboxEvent 是 Bot Runtime 的持久入站记录。Payload 保存归一化后的平台消息，
// 使进程重启后仍能继续尚未组成自然话轮的消息。
type InboxEvent struct {
	ID             string
	BotID          string
	Source         string
	UserID         string
	ConversationID string
	EventType      PlatformEventType
	Payload        []byte
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	QueueLimit     int
}

// ContextEntry 是来源级背景记录；它独立于 Agent Conversation，因此群聊中未唤醒
// 机器人的消息也能在重启后保留，同时不会混入别的 UMO。
type ContextEntry struct {
	ID        string
	BotID     string
	Source    string
	UserID    string
	Text      string
	CreatedAt time.Time
}

// SourceState 是 Bot 与一个 UMO 的常驻状态摘要。
type SourceState struct {
	BotID                 string     `json:"bot_id"`
	Source                string     `json:"source"`
	ChatType              string     `json:"chat_type"`
	Mode                  string     `json:"mode,omitempty"`
	CurrentTopic          string     `json:"current_topic,omitempty"`
	TopicParticipants     string     `json:"topic_participants,omitempty"`
	LastUserID            string     `json:"last_user_id"`
	LastSeenAt            time.Time  `json:"last_seen_at"`
	LastTurnAt            *time.Time `json:"last_turn_at,omitempty"`
	LastUserTurnAt        *time.Time `json:"last_user_turn_at,omitempty"`
	LastBotActionAt       *time.Time `json:"last_bot_action_at,omitempty"`
	RecentActivityScore   float64    `json:"recent_activity_score"`
	SilenceUntil          *time.Time `json:"silence_until,omitempty"`
	ReplyBudget           int        `json:"reply_budget"`
	ReplyBudgetWindowAt   *time.Time `json:"reply_budget_window_at,omitempty"`
	CurrentConversationID string     `json:"current_conversation_id,omitempty"`
	// QueuedTurnCount 是持久 Inbox 中尚未消费的事件数，供管理视图显示来源积压。
	QueuedTurnCount int `json:"queued_turn_count"`
	// QueuedTurnCountSet 区分“明确把队列刷新为 0”和普通来源状态 touch；
	// 后者不能用零值覆盖数据库里刚由话轮聚合器写入的积压数量。
	QueuedTurnCountSet bool  `json:"-"`
	Revision           int64 `json:"revision"`
}

// BehaviorDecision 记录可解释的机器人行为，不包含隐藏思维过程。
type BehaviorDecision struct {
	ID              string    `json:"id"`
	BotID           string    `json:"bot_id"`
	Source          string    `json:"source"`
	EventID         string    `json:"event_id"`
	ConversationID  string    `json:"conversation_id,omitempty"`
	TurnID          string    `json:"turn_id,omitempty"`
	Action          string    `json:"action"`
	Reason          string    `json:"reason"`
	ReasonCodes     []string  `json:"reason_codes,omitempty"`
	Confidence      float64   `json:"confidence"`
	RequiresAgent   bool      `json:"requires_agent"`
	AllowedTools    []string  `json:"allowed_tools,omitempty"`
	ResponseUrgency string    `json:"response_urgency,omitempty"`
	FollowUpPolicy  string    `json:"follow_up_policy,omitempty"`
	SafetyPolicy    string    `json:"safety_policy,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// BotTurn 是聚合后的自然话轮，事件原文仍由 Inbox/Artifact 保存。
type BotTurn struct {
	ID             string
	BotID          string
	Source         string
	UserID         string
	ConversationID string
	Text           string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// RelationState 只保存可解释的互动投影；它不参与任何权限判断。
type RelationState struct {
	BotID             string    `json:"bot_id"`
	ScopeType         string    `json:"scope_type"`
	ScopeID           string    `json:"scope_id"`
	PreferredName     string    `json:"preferred_name,omitempty"`
	StablePreferences string    `json:"stable_preferences,omitempty"`
	InteractionStyle  string    `json:"interaction_style,omitempty"`
	TrustLevel        string    `json:"trust_level,omitempty"`
	RecentTopics      string    `json:"recent_topics,omitempty"`
	Commitments       string    `json:"commitments,omitempty"`
	LastInteractionAt time.Time `json:"last_interaction_at"`
	Revision          int64     `json:"revision"`
}

// BotActionPlan 与 BotAction 为平台副作用提供先计划、后执行的持久审计边界。
type BotActionPlan struct {
	ID             string
	BotID          string
	Source         string
	TurnID         string
	ConversationID string
	Status         string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

type BotAction struct {
	ID             string     `json:"id"`
	PlanID         string     `json:"plan_id"`
	Type           string     `json:"type"`
	Sequence       int        `json:"sequence"`
	IdempotencyKey string     `json:"idempotency_key"`
	Status         string     `json:"status"`
	Payload        string     `json:"payload,omitempty"`
	ReplyTarget    string     `json:"reply_target,omitempty"`
	NotBefore      *time.Time `json:"not_before,omitempty"`
	PayloadSummary string     `json:"payload_summary,omitempty"`
	PlatformResult string     `json:"platform_result,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// BotInstanceState 是连接生命周期与当前负载的持久投影。
type BotInstanceState struct {
	BotID               string    `json:"bot_id"`
	LifecycleState      string    `json:"lifecycle_state"`
	Availability        string    `json:"availability"`
	CurrentLoad         int       `json:"current_load"`
	ActiveSources       int       `json:"active_sources"`
	LastHeartbeatAt     time.Time `json:"last_heartbeat_at"`
	LastPlatformEventAt time.Time `json:"last_platform_event_at"`
	LastActionAt        time.Time `json:"last_action_at"`
	ConfigRevision      int64     `json:"config_revision"`
	PersonaRevision     int64     `json:"persona_revision"`
	Revision            int64     `json:"revision"`
}

// ApprovalMessageBinding 把平台发出的审批通知与审批记录绑定，支持引用回复在
// 重启后仍能准确找到目标审批，而不是依赖进程内的临时票据。
type ApprovalMessageBinding struct {
	ID         string
	BotID      string
	ApprovalID string
	AdminID    string
	Platform   Type
	ChatID     string
	MessageID  string
	CreatedAt  time.Time
}

// RuntimeOverview 是 WebUI 的只读投影；它只包含状态、原因码和动作结果，
// 不包含模型隐藏推理、凭据或完整工具参数。
type RuntimeOverview struct {
	BotID     string             `json:"bot_id"`
	Instance  BotInstanceState   `json:"instance"`
	Sources   []SourceState      `json:"sources"`
	Decisions []BehaviorDecision `json:"decisions"`
	Actions   []BotAction        `json:"actions"`
	Relations []RelationState    `json:"relations"`
}

// AdminRoute 保存机器人管理员最近一次可用的私聊地址。
type AdminRoute struct {
	BotID     string
	UserID    string
	Platform  Type
	ChatID    string
	UpdatedAt time.Time
}

// DeleteConfirmation 保存会话物理删除的二次确认，确认码同时绑定管理员、会话和修订。
type DeleteConfirmation struct {
	ID             string
	BotID          string
	AdminUserID    string
	ConversationID string
	Revision       int64
	Code           string
	ExpiresAt      time.Time
	CreatedAt      time.Time
}

// RuntimeStateRepository 是 Bot Runtime 使用的最小持久化边界。
type RuntimeStateRepository interface {
	SaveInbox(context.Context, InboxEvent) (bool, error)
	ListPendingInbox(context.Context, string) ([]InboxEvent, error)
	MarkInboxProcessed(context.Context, []string) error
	AppendContext(context.Context, ContextEntry, int) error
	ListContext(context.Context, string, int) ([]ContextEntry, error)
	DeleteContext(context.Context, string) error
	TouchSource(context.Context, SourceState) error
	RecordDecision(context.Context, BehaviorDecision) error
	CommitTurn(context.Context, BotTurn, []string) error
	TouchRelation(context.Context, RelationState) error
	ListRelations(context.Context, string, string, string, bool) ([]RelationState, error)
	SaveActionPlan(context.Context, BotActionPlan, BotAction) (BotAction, error)
	FinishAction(context.Context, string, string, string, time.Time) error
	UpdateInstanceState(context.Context, BotInstanceState) error
	DeleteBotState(context.Context, string) error
	SaveAdminRoute(context.Context, AdminRoute) error
	ListAdminRoutes(context.Context, string) ([]AdminRoute, error)
	ApprovalDelivered(context.Context, string, string) (bool, error)
	MarkApprovalDelivered(context.Context, string, string, time.Time) error
	SaveDeleteConfirmation(context.Context, DeleteConfirmation) error
	ConsumeDeleteConfirmation(context.Context, string, string, string, string, time.Time) (DeleteConfirmation, error)
}

// RuntimeActionClaimer 用数据库条件更新把“待执行动作”变成唯一执行者；
// Action Plan 先落库后，多个重复事件不能同时越过网络副作用边界。
type RuntimeActionClaimer interface {
	ClaimAction(context.Context, string, time.Time) (bool, error)
}

// RuntimeInboxPayloadUpdater 用于在消息完成 Artifact 入库和会话绑定后更新同一条
// Inbox。平台事件必须先入库，但不能把临时附件路径或尚未确定的会话 ID永久写入事实链。
type RuntimeInboxPayloadUpdater interface {
	UpdateInboxPayload(context.Context, InboxEvent) error
}

// RuntimeInboxReader 读取已存在的消息事实；平台重投时优先复用其中的 Artifact
// 引用，只有首次附件化失败且事实仍是原始消息时才再次尝试读取平台附件。
type RuntimeInboxReader interface {
	GetInbox(context.Context, string) (InboxEvent, error)
}

// RuntimeInboxActivator 把已经接收入 Inbox 的事件正式转入 pending 队列。
// 接收与排队分成两个状态，避免未通过平台准入的观察消息暂时占满来源队列，
// 同时保留进程在“已接收、尚未完成准入”窗口崩溃后的恢复能力。
type RuntimeInboxActivator interface {
	ActivateInbox(context.Context, InboxEvent) error
}

// RuntimeInboxReceivedReader 读取已经接收、但尚未通过来源准入的消息。
// 这类记录通常来自队列达到上限或进程在准入窗口崩溃；心跳必须在来源恢复容量
// 后重新走同一条入口链，不能让 received 状态变成永久积压。
type RuntimeInboxReceivedReader interface {
	ListReceivedInbox(context.Context, string, int) ([]InboxEvent, error)
}

// RuntimeStateReader 是持久运行状态的可选只读扩展，避免破坏未实现管理视图的
// 嵌入式仓储；生产 SQLite 仓储实现该接口。
type RuntimeStateReader interface {
	GetRuntimeOverview(context.Context, string, int) (RuntimeOverview, error)
}

// ErrRelationConflict 表示管理视图提交的关系修订已经过期，调用方必须重新读取
// 当前事实后再编辑，避免覆盖消息入口或其他管理员刚写入的关系状态。
var ErrRelationConflict = errors.New("关系状态已被其他操作更新")

// RuntimeRelationManager 是 WebUI 管理关系状态的可选持久化扩展。
// Runtime 主流程仍使用 TouchRelation，管理修改则使用 revision CAS。
type RuntimeRelationManager interface {
	ListAllRelations(context.Context, string, int) ([]RelationState, error)
	SaveRelation(context.Context, RelationState, int64) error
	DeleteRelation(context.Context, string, string, string, int64) error
}

// RuntimeApprovalBindingRepository 是审批消息引用映射的可选持久化扩展。
type RuntimeApprovalBindingRepository interface {
	SaveApprovalMessageBinding(context.Context, ApprovalMessageBinding) error
	FindApprovalMessageBinding(context.Context, string, Type, string, string) (ApprovalMessageBinding, error)
}

// RuntimeConversationStateCleaner 在会话物理删除前清理 Bot Runtime 的
// 会话级话轮、Inbox、动作计划和审批投递；来源上下文与关系状态保持独立生命周期。
type RuntimeConversationStateCleaner interface {
	DeleteConversationState(context.Context, string) error
}

// RuntimeMaintenanceRepository 执行有界的运行时清理。清理只针对已经结束、
// 超过保留期的事实，不触碰 pending Inbox、运行中动作和活动来源。
type RuntimeMaintenanceRepository interface {
	CleanupRuntime(context.Context, string, time.Time) error
}

// RuntimeProactiveQuotaRepository 为主动消息提供持久化的冷却和小时额度。
// 进程内窗口只能作为无数据库嵌入场景的降级，生产 SQLite 必须在事务内完成
// 额度领取，避免重启或多个调度协程绕过来源级频率边界。
type RuntimeProactiveQuotaRepository interface {
	ReserveProactiveDelivery(context.Context, string, string, string, int, time.Duration, time.Time) (time.Time, bool, error)
	RollbackProactiveDelivery(context.Context, string, string, time.Time) error
}

// RuntimeActionRecoveryRepository 在进程重启后把未完成的外部动作标记为未知。
// 未知动作不能盲目重发，必须留在审计视图中等待平台结果或人工处理，避免
// “平台已经成功、数据库尚未落盘”这个崩溃窗口造成重复消息。
type RuntimeActionRecoveryRepository interface {
	MarkUnfinishedActionsUnknown(context.Context, string, time.Time) error
}

// RuntimeSourceCounter 提供实例状态的活跃来源计数；计数由数据库完成，避免把每个
// UMO 长期复制到 Manager 内存中。
type RuntimeSourceCounter interface {
	CountActiveSources(context.Context, string, time.Time) (int, error)
}

// touchRuntimeEventSource 记录撤回、反应、成员变化和戳一戳等非消息事件的
// 来源活跃度；这些事件只更新状态，不创建 Conversation 或 Agent 任务。
func (m *Manager) touchRuntimeEventSource(ctx context.Context, event PlatformEvent, occurredAt time.Time) error {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil || strings.TrimSpace(event.SourceUMO) == "" {
		return nil
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	state := SourceState{
		BotID:      event.BotID,
		Source:     event.SourceUMO,
		ChatType:   event.ChatType,
		LastUserID: event.UserID,
		LastSeenAt: occurredAt,
		Mode:       "event:" + string(event.EventType),
	}
	if err := repository.TouchSource(ctx, state); err != nil {
		return err
	}
	if !isGroupChat(event.ChatType) || event.UserID == "" {
		return nil
	}
	// 关系记录只保存“最近发生过互动”这一可解释事实，绝不把事件当成授权。
	return repository.TouchRelation(ctx, RelationState{BotID: event.BotID, ScopeType: "group_member", ScopeID: event.ChatID + ":" + event.UserID, LastInteractionAt: occurredAt})
}

func (m *Manager) touchRuntimeSource(ctx context.Context, message Message) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return
	}
	now := time.Now().UTC()
	state := SourceState{BotID: message.AdapterID, Source: messageSource(message), ChatType: message.ChatType, LastUserID: message.UserID, LastSeenAt: now, CurrentConversationID: strings.TrimSpace(message.RuntimeConversationID)}
	if err := repository.TouchSource(ctx, state); err != nil {
		slog.Warn("更新 Bot Runtime 来源状态失败", "source", state.Source, "error", err)
	}
}

func (m *Manager) relationContextText(ctx context.Context, message Message) string {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return ""
	}
	items, err := repository.ListRelations(ctx, message.AdapterID, message.ChatID, message.UserID, isGroupChat(message.ChatType))
	if err != nil {
		slog.Warn("读取 Bot Runtime 关系状态失败", "source", messageSource(message), "error", err)
		return ""
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		facts := make([]string, 0, 4)
		if item.PreferredName != "" {
			facts = append(facts, "称呼="+boundedGroupText(item.PreferredName, 64))
		}
		if item.InteractionStyle != "" {
			facts = append(facts, "互动偏好="+boundedGroupText(item.InteractionStyle, 160))
		}
		if item.RecentTopics != "" {
			facts = append(facts, "近期话题="+boundedGroupText(item.RecentTopics, 300))
		}
		if item.Commitments != "" {
			facts = append(facts, "明确承诺="+boundedGroupText(item.Commitments, 300))
		}
		if len(facts) > 0 {
			lines = append(lines, item.ScopeType+"："+strings.Join(facts, "；"))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "[可解释关系状态，仅作交流偏好，不授予任何权限]\n" + strings.Join(lines, "\n")
}

func (m *Manager) recordRuntimeDecision(ctx context.Context, message Message, action, reason string) {
	confidence := 1.0
	requiresAgent := action == "reply_agent"
	reasonCodes := []string{}
	if strings.TrimSpace(reason) != "" {
		reasonCodes = []string{strings.TrimSpace(reason)}
	}
	m.recordRuntimeDecisionDetail(ctx, message, action, reasonCodes, confidence, requiresAgent, nil, "normal", "none", "default")
}

// recordRuntimeDecisionDetail 统一保存行为决策的受限结构化字段；reason 只保留
// 原因码，不保存模型隐藏推理，WebUI 可据此解释“为什么沉默或启动 Agent”。
func (m *Manager) recordRuntimeDecisionDetail(ctx context.Context, message Message, action string, reasonCodes []string, confidence float64, requiresAgent bool, allowedTools []string, urgency, followUpPolicy, safetyPolicy string) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return
	}
	now := time.Now().UTC()
	reasonText := strings.Join(reasonCodes, ",")
	decision := BehaviorDecision{ID: runtimeInboxID(message) + ":" + action, BotID: message.AdapterID, Source: messageSource(message), EventID: message.ID, TurnID: message.RuntimeTurnID, ConversationID: strings.TrimSpace(message.RuntimeConversationID), Action: action, Reason: reasonText, ReasonCodes: append([]string(nil), reasonCodes...), Confidence: confidence, RequiresAgent: requiresAgent, AllowedTools: append([]string(nil), allowedTools...), ResponseUrgency: urgency, FollowUpPolicy: followUpPolicy, SafetyPolicy: safetyPolicy, CreatedAt: now}
	if err := repository.RecordDecision(ctx, decision); err != nil {
		slog.Warn("保存 Bot Runtime 行为决策失败", "source", decision.Source, "action", action, "error", err)
	}
}

type pendingTurn struct {
	events   []InboxEvent
	timer    *time.Timer
	attempts int
	// sequence 在当前进程内记录话轮进入来源队列的顺序；同一 UMO 的不同用户
	// 不能因为各自计时器同时到期而倒序执行。重启后由 Inbox 的 CreatedAt 重新排序。
	sequence uint64
}

const botRuntimeTurnWorkerCount = 4

// startTurnWorkers 为所有来源提供固定数量的刷新 worker。真正的 Agent 调用仍由
// Bot/Conversation 并发边界控制；这里仅负责把持久 Inbox 聚合成自然话轮，避免
// 每条平台消息都派生一个长期 goroutine。
func (m *Manager) startTurnWorkers(ctx context.Context) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if m.turnCancel != nil {
		m.mu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	m.turnContext = workerCtx
	m.turnCancel = cancel
	m.turnWake = make(chan string, 256)
	m.mu.Unlock()
	for index := 0; index < botRuntimeTurnWorkerCount; index++ {
		m.waitGroup.Add(1)
		go func() {
			defer m.waitGroup.Done()
			for {
				select {
				case <-workerCtx.Done():
					return
				case key := <-m.turnWake:
					if strings.TrimSpace(key) == "" {
						continue
					}
					m.turnMu.Lock()
					delete(m.turnWakePending, key)
					m.turnMu.Unlock()
					m.flushRuntimeTurn(key)
				}
			}
		}()
	}
}

// enqueueTurnFlush 把一个来源加入固定 worker 队列；同一来源同时只保留一个
// 唤醒项，避免定时器竞争产生重复刷新。
func (m *Manager) enqueueTurnFlush(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	m.mu.RLock()
	wake := m.turnWake
	turnContext := m.turnContext
	m.mu.RUnlock()
	if wake == nil || turnContext == nil {
		// 未调用 Start 的嵌入式调用方没有 worker 生命周期；同步刷新，避免
		// 兜底路径为每个来源继续派生不可控的 goroutine。生产路径始终走固定 worker。
		m.flushRuntimeTurn(key)
		return
	}
	m.turnMu.Lock()
	if _, exists := m.turnWakePending[key]; exists {
		m.turnMu.Unlock()
		return
	}
	m.turnWakePending[key] = struct{}{}
	m.turnMu.Unlock()
	select {
	case wake <- key:
	case <-turnContext.Done():
		m.turnMu.Lock()
		delete(m.turnWakePending, key)
		m.turnMu.Unlock()
	}
}

// armTurnFlush 重置某个待处理话轮的唯一计时器；到期后只投递一个有界唤醒项。
func (m *Manager) armTurnFlush(key string, delay time.Duration) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	m.turnMu.Lock()
	turn := m.pendingTurns[key]
	if turn == nil {
		m.turnMu.Unlock()
		return
	}
	if turn.timer != nil {
		turn.timer.Stop()
		turn.timer = nil
	}
	if delay <= 0 {
		m.turnMu.Unlock()
		m.enqueueTurnFlush(key)
		return
	}
	turn.timer = time.AfterFunc(delay, func() { m.enqueueTurnFlush(key) })
	m.turnMu.Unlock()
}

// SetRuntimeStateRepository 安装 Bot Runtime 的持久状态仓储。
func (m *Manager) SetRuntimeStateRepository(repository RuntimeStateRepository) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.runtimeState = repository
	m.mu.Unlock()
}

func (m *Manager) runtimeActionRecoveryRepository() (RuntimeActionRecoveryRepository, bool) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	recovery, ok := repository.(RuntimeActionRecoveryRepository)
	return recovery, ok
}

// GetRuntimeOverview 返回单个 Bot 的运行状态投影，供 WebUI 和排障工具读取。
func (m *Manager) GetRuntimeOverview(ctx context.Context, botID string, limit int) (RuntimeOverview, error) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return RuntimeOverview{BotID: strings.TrimSpace(botID)}, nil
	}
	reader, ok := repository.(RuntimeStateReader)
	if !ok {
		return RuntimeOverview{BotID: strings.TrimSpace(botID)}, nil
	}
	return reader.GetRuntimeOverview(ctx, strings.TrimSpace(botID), limit)
}

// ListRuntimeRelations 返回当前 Bot 的关系目录；管理接口通过 Manager 访问，
// 避免 HTTP 层直接依赖 SQLite 具体实现，同时保留嵌入方替换仓储的能力。
func (m *Manager) ListRuntimeRelations(ctx context.Context, botID string, limit int) ([]RelationState, error) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	manager, ok := repository.(RuntimeRelationManager)
	if !ok {
		return []RelationState{}, nil
	}
	return manager.ListAllRelations(ctx, strings.TrimSpace(botID), limit)
}

// SaveRuntimeRelation 以 revision CAS 写入关系状态，防止 WebUI 的过期编辑覆盖
// 消息入口刚刚形成的最新关系事实。
func (m *Manager) SaveRuntimeRelation(ctx context.Context, state RelationState, expectedRevision int64) error {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	manager, ok := repository.(RuntimeRelationManager)
	if !ok {
		return errors.New("关系状态仓储尚未装配")
	}
	return manager.SaveRelation(ctx, state, expectedRevision)
}

// DeleteRuntimeRelation 使用关系修订号删除指定关系；删除关系不会触碰会话历史、
// 来源上下文或附件，符合关系与 Conversation 生命周期分离的设计。
func (m *Manager) DeleteRuntimeRelation(ctx context.Context, botID, scopeType, scopeID string, expectedRevision int64) error {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	manager, ok := repository.(RuntimeRelationManager)
	if !ok {
		return errors.New("关系状态仓储尚未装配")
	}
	return manager.DeleteRelation(ctx, strings.TrimSpace(botID), strings.TrimSpace(scopeType), strings.TrimSpace(scopeID), expectedRevision)
}

// restorePendingInbox 在平台连接启动后恢复未消费的 Inbox。恢复消息使用同一聚合器，
// 从而保持同一来源顺序且不会为每条旧消息各建一个 Invocation。
func (m *Manager) launchInboxRestore(ctx context.Context, botID string) {
	// 恢复任务必须登记到 Manager 的 WaitGroup；否则 Close 可能先返回并释放
	// 平台资源，后台恢复 goroutine 随后仍访问已关闭的仓储或适配器。
	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		if strings.TrimSpace(botID) == "" {
			m.restorePendingInbox(ctx)
			return
		}
		m.restorePendingInboxForBot(ctx, botID)
	}()
}

func (m *Manager) restorePendingInbox(ctx context.Context) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return
	}
	events, err := repository.ListPendingInbox(ctx, "")
	if err != nil {
		slog.Warn("恢复 Bot Runtime Inbox 失败", "error", err)
		return
	}
	m.restorePendingInboxEvents(ctx, repository, events)
}

// restorePendingInboxForBot 是单实例启动/重启时的恢复入口，避免必须重新启动
// 整个应用才能消费该 Bot 已经持久化的自然话轮。
func (m *Manager) restorePendingInboxForBot(ctx context.Context, botID string) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil || !m.runtimeBotEnabled(botID) {
		return
	}
	events, err := repository.ListPendingInbox(ctx, strings.TrimSpace(botID))
	if err != nil {
		slog.Warn("恢复单个 Bot Runtime Inbox 失败", "bot_id", botID, "error", err)
		return
	}
	m.restorePendingInboxEvents(ctx, repository, events)
}

func (m *Manager) restorePendingInboxEvents(ctx context.Context, repository RuntimeStateRepository, events []InboxEvent) {
	for _, event := range events {
		m.recoverRuntimeInboxEvent(ctx, repository, event)
	}
}

// restoreReceivedInbox 重试已经收到但尚未进入 pending 的事件。来源队列满时，
// 入口会保留 received 事实而不是向用户发送“任务忙”；心跳在任务完成后重新
// 调用同一入口，保证消息最终按原顺序进入自然话轮。
func (m *Manager) restoreReceivedInbox(ctx context.Context, enabled map[string]struct{}) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	reader, ok := repository.(RuntimeInboxReceivedReader)
	if !ok {
		return
	}
	events, err := reader.ListReceivedInbox(ctx, "", 200)
	if err != nil {
		slog.Warn("读取待重试的 Bot Runtime Inbox 失败", "error", err)
		return
	}
	for _, event := range events {
		if _, enabledForBot := enabled[strings.TrimSpace(event.BotID)]; !enabledForBot {
			continue
		}
		m.recoverRuntimeInboxEvent(ctx, repository, event)
	}
}

// recoverRuntimeInboxEvent 对单条持久事件加进程内声明，防止启动恢复和心跳重试
// 同时把同一消息送入聚合器。声明只覆盖解析和准入窗口，真正的来源顺序仍由
// queueRuntimeEvent 的 turnMu/sourceRunning 维护。
func (m *Manager) recoverRuntimeInboxEvent(ctx context.Context, repository RuntimeStateRepository, event InboxEvent) {
	if strings.TrimSpace(event.ID) == "" || !m.runtimeBotEnabled(event.BotID) || !m.claimInboxRecovery(event.ID) {
		return
	}
	defer m.releaseInboxRecovery(event.ID)
	var message Message
	if err := json.Unmarshal(event.Payload, &message); err != nil {
		slog.Warn("跳过无法恢复的 Bot Runtime 事件", "event_id", event.ID, "error", err)
		_ = repository.MarkInboxProcessed(ctx, []string{event.ID})
		return
	}
	message.Restored = true
	message.runtimeInboxPersisted = true
	// 恢复必须重新经过同一条入口链：来源规则、白名单、指令、审批、
	// 附件校验和队列上限都可能在停机期间发生变化，不能直接把旧 JSON
	// 投递给 Agent，否则会绕过新配置或把观察消息误当成任务。
	if restoreErr := m.handleMessageWithRuntime(ctx, message); restoreErr != nil {
		slog.Warn("恢复 Bot Runtime 消息处理失败，将保留 Inbox 等待下次恢复", "event_id", event.ID, "error", restoreErr)
	}
}

// runtimeBotEnabled 只恢复仍存在且明确启用的 Bot；删除或停用实例的 Inbox
// 由删除/维护流程处理，不能因为恢复扫描而重新启动它们。
func (m *Manager) runtimeBotEnabled(botID string) bool {
	m.mu.RLock()
	item, ok := m.bots[strings.TrimSpace(botID)]
	m.mu.RUnlock()
	return ok && item.Enabled
}

func (m *Manager) claimInboxRecovery(eventID string) bool {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return false
	}
	m.turnMu.Lock()
	defer m.turnMu.Unlock()
	if m.inboxRecovering == nil {
		m.inboxRecovering = make(map[string]struct{})
	}
	if _, exists := m.inboxRecovering[eventID]; exists {
		return false
	}
	m.inboxRecovering[eventID] = struct{}{}
	return true
}

func (m *Manager) releaseInboxRecovery(eventID string) {
	m.turnMu.Lock()
	delete(m.inboxRecovering, strings.TrimSpace(eventID))
	m.turnMu.Unlock()
}

// enqueueRuntimeMessage 先持久化再进入内存等待窗；平台重复投递同一事件时只消费一次。
func (m *Manager) enqueueRuntimeMessage(ctx context.Context, message Message, delay time.Duration, turnLimit, queueLimit int) error {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		if err := m.processRuntimeTurn(ctx, []Message{message}); err != nil {
			// 无持久仓储时没有恢复重试边界，因此直接在本次调用结束时
			// 投影一次脱敏错误，避免嵌入式调用方只拿到内部错误。
			_ = m.send(ctx, message, botRuntimeStartError(err))
			return err
		}
		return nil
	}
	// 没有平台事件 ID 的嵌入式调用也必须先补一个稳定 ID；否则持久化时
	// 使用的 Inbox 主键和聚合完成后的回收 ID 会不一致，消息会永久留在队列里。
	if strings.TrimSpace(message.ID) == "" {
		message.ID = fmt.Sprintf("event-%d", time.Now().UnixNano())
	}
	if strings.TrimSpace(message.RuntimeConversationID) == "" {
		_, conversationID, conversationErr := m.conversationForMessage(ctx, message)
		if conversationErr != nil {
			return conversationErr
		}
		message.RuntimeConversationID = conversationID
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("序列化 Bot Runtime 消息失败: %w", err)
	}
	now := time.Now().UTC()
	event := InboxEvent{
		ID: runtimeInboxID(message), BotID: strings.TrimSpace(message.AdapterID),
		Source: messageSource(message), UserID: strings.TrimSpace(message.UserID), ConversationID: strings.TrimSpace(message.RuntimeConversationID), EventType: PlatformEventMessageCreated,
		Payload: payload, Status: "pending", CreatedAt: now, UpdatedAt: now, QueueLimit: queueLimit,
	}
	created, err := repository.SaveInbox(ctx, event)
	if err != nil {
		return fmt.Errorf("保存 Bot Runtime Inbox 失败: %w", err)
	}
	if !created {
		if message.runtimeInboxPersisted {
			// 入口阶段已经先保存了接收事实；这里回写 Artifact 引用和最终
			// 会话 ID，并在来源队列仍有容量时激活同一条记录，避免丢失附件、
			// 重复建 Inbox 或让未唤醒消息消耗任务队列。
			if activator, ok := repository.(RuntimeInboxActivator); ok {
				if err := retryRuntimePersistence(ctx, "激活 Bot Runtime Inbox 消息", func(updateCtx context.Context) error {
					return activator.ActivateInbox(updateCtx, event)
				}); err != nil {
					return fmt.Errorf("激活 Bot Runtime Inbox 失败: %w", err)
				}
			} else if updater, ok := repository.(RuntimeInboxPayloadUpdater); ok {
				if err := retryRuntimePersistence(ctx, "更新 Bot Runtime Inbox 消息", func(updateCtx context.Context) error {
					return updater.UpdateInboxPayload(updateCtx, event)
				}); err != nil {
					return fmt.Errorf("更新 Bot Runtime Inbox 失败: %w", err)
				}
			}
			m.queueRuntimeEvent(event, message, delay, turnLimit)
		}
		return nil
	}
	m.queueRuntimeEvent(event, message, delay, turnLimit)
	return nil
}

func runtimeInboxID(message Message) string {
	id := strings.TrimSpace(message.ID)
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return strings.TrimSpace(message.AdapterID) + ":" + string(message.Platform) + ":" + id
}

// queueRuntimeEvent 为每个来源维护一个有界等待窗；计时器只负责唤醒，真正执行由
// 来源互斥锁串行化，因此新消息可以在上一任务运行时继续形成后续话轮。
func (m *Manager) queueRuntimeEvent(event InboxEvent, message Message, delay time.Duration, limit int) {
	if delay < 0 {
		delay = 0
	}
	// 不同用户在同一群中的连续发言不能合并成同一个用户话轮。
	key := event.Source + "\x00" + strings.TrimSpace(message.UserID)
	m.turnMu.Lock()
	turn := m.pendingTurns[key]
	if turn == nil {
		m.turnSequence++
		turn = &pendingTurn{sequence: m.turnSequence}
		m.pendingTurns[key] = turn
	}
	turn.events = append(turn.events, event)
	if limit <= 0 {
		limit = 12
	}
	if turn.timer != nil {
		turn.timer.Stop()
		turn.timer = nil
	}
	queuedCount := m.pendingTurnCountLocked(event.Source)
	if len(turn.events) >= limit {
		m.turnMu.Unlock()
		m.persistQueuedTurnCount(event.BotID, event.Source, queuedCount)
		m.enqueueTurnFlush(key)
		return
	}
	m.turnMu.Unlock()
	m.persistQueuedTurnCount(event.BotID, event.Source, queuedCount)
	m.armTurnFlush(key, delay)
}

// pendingTurnCountLocked 统计某个 UMO 当前仍等待执行的话轮数量；调用方必须持有
// turnMu，避免管理视图看到半更新的内存队列。
func (m *Manager) pendingTurnCountLocked(source string) int {
	count := 0
	for _, turn := range m.pendingTurns {
		if turn == nil || len(turn.events) == 0 || turn.events[0].Source != source {
			continue
		}
		count++
	}
	return count
}

// persistQueuedTurnCount 只刷新来源的队列投影，不更新最后活跃时间；这样
// WebUI 能看到实时积压，同时不会把后台队列变化误记成用户刚刚发言。
func (m *Manager) persistQueuedTurnCount(botID, source string, count int) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil || strings.TrimSpace(botID) == "" || strings.TrimSpace(source) == "" {
		return
	}
	if err := repository.TouchSource(m.runtimeContext(), SourceState{BotID: botID, Source: source, QueuedTurnCount: count, QueuedTurnCountSet: true}); err != nil {
		slog.Warn("更新 Bot Runtime 来源队列长度失败", "bot_id", botID, "source", source, "queued_turn_count", count, "error", err)
	}
}

func (m *Manager) flushRuntimeTurn(source string) {
	m.turnMu.Lock()
	if m.sourceRunning == nil {
		m.sourceRunning = make(map[string]bool)
	}
	turn := m.pendingTurns[source]
	serializedSource := ""
	botID := ""
	queuedCount := 0
	if turn != nil && len(turn.events) > 0 {
		serializedSource = turn.events[0].Source
		botID = turn.events[0].BotID
		if m.sourceRunning[serializedSource] {
			// 当前来源正在执行上一话轮，不让计时器 goroutine 阻塞在 Mutex 上；
			// 保留积压事件并稍后重新唤醒，避免长任务导致 goroutine 无界增长。
			m.turnMu.Unlock()
			m.armTurnFlush(source, 100*time.Millisecond)
			return
		}
		// 同一个 UMO 可能有多个用户分别形成待处理话轮。按首次入队序号
		// 选择，避免后到期的用户话轮抢在更早话轮前执行。
		for key, candidate := range m.pendingTurns {
			if key == source || candidate == nil || len(candidate.events) == 0 || candidate.sequence >= turn.sequence {
				continue
			}
			if candidate.events[0].Source == serializedSource {
				m.turnMu.Unlock()
				m.armTurnFlush(source, 100*time.Millisecond)
				return
			}
		}
		m.sourceRunning[serializedSource] = true
	}
	delete(m.pendingTurns, source)
	if serializedSource != "" {
		queuedCount = m.pendingTurnCountLocked(serializedSource)
	}
	m.turnMu.Unlock()
	if turn == nil || len(turn.events) == 0 {
		return
	}
	m.persistQueuedTurnCount(botID, serializedSource, queuedCount)
	defer func() {
		m.turnMu.Lock()
		delete(m.sourceRunning, serializedSource)
		nextReady := false
		if next := m.pendingTurns[source]; next != nil && len(next.events) > 0 {
			// 失败分支可能已经安装退避定时器；成功分支才立即消费同一来源
			// 在执行期间到达的下一话轮，避免把有界重试变成忙循环。
			nextReady = next.timer == nil
		}
		queuedCount := m.pendingTurnCountLocked(serializedSource)
		m.turnMu.Unlock()
		m.persistQueuedTurnCount(botID, serializedSource, queuedCount)
		if nextReady {
			m.armTurnFlush(source, 0)
		}
	}()
	sort.SliceStable(turn.events, func(i, j int) bool { return turn.events[i].CreatedAt.Before(turn.events[j].CreatedAt) })
	messages := make([]Message, 0, len(turn.events))
	ids := make([]string, 0, len(turn.events))
	for _, event := range turn.events {
		var message Message
		if err := json.Unmarshal(event.Payload, &message); err != nil {
			slog.Warn("Bot Runtime 事件反序列化失败", "event_id", event.ID, "error", err)
			ids = append(ids, event.ID)
			continue
		}
		messages = append(messages, message)
		ids = append(ids, event.ID)
	}
	if len(messages) == 0 {
		m.markRuntimeEvents(ids)
		return
	}
	sourceKey := serializedSource
	err := m.processRuntimeTurn(m.runtimeContext(), messages)
	if err != nil {
		slog.Error("Bot Runtime 自然话轮处理失败", "source", sourceKey, "event_ids", ids, "error", err)
		// 短暂的数据库锁、平台重连或模型初始化失败不应让 pending Inbox
		// 永久停在数据库里；最多重试三次，之后保留 pending 交给下一次重启恢复。
		if turn.attempts < 3 {
			turn.attempts++
			m.turnMu.Lock()
			if existing := m.pendingTurns[source]; existing == nil {
				m.pendingTurns[source] = turn
			} else {
				existing.events = append(turn.events, existing.events...)
				existing.attempts = turn.attempts
			}
			queuedCount := m.pendingTurnCountLocked(serializedSource)
			m.turnMu.Unlock()
			m.persistQueuedTurnCount(botID, serializedSource, queuedCount)
			backoff := time.Duration(turn.attempts) * time.Second
			m.armTurnFlush(source, backoff)
		} else {
			// 有限重试耗尽后只发送一次脱敏结果；中间重试不打扰聊天，完整错误
			// 和每次尝试仍保留在内部日志中，便于定位数据库或平台瞬态问题。
			_ = m.send(m.runtimeContext(), mergeRuntimeMessages(messages), botRuntimeStartError(err))
		}
		return
	}
	m.markRuntimeEvents(ids)
}

func (m *Manager) runtimeContext() context.Context {
	m.mu.RLock()
	ctx := m.baseCtx
	m.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// acquireBotSlot 对单个 Bot 的 Agent 任务实施硬并发上限；等待期间不向聊天发送忙碌提示。
func (m *Manager) acquireBotSlot(ctx context.Context, botID string, limit int) error {
	if limit <= 0 {
		limit = 4
	}
	for {
		m.turnMu.Lock()
		if m.botActive[botID] < limit {
			m.botActive[botID]++
			m.turnMu.Unlock()
			return nil
		}
		m.turnMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.botSlotWake:
		}
	}
}

func (m *Manager) releaseBotSlot(botID string) {
	m.turnMu.Lock()
	if m.botActive[botID] > 0 {
		m.botActive[botID]--
	}
	m.turnMu.Unlock()
	select {
	case m.botSlotWake <- struct{}{}:
	default:
	}
}

func (m *Manager) reserveRestoredBotSlot(botID string) {
	m.turnMu.Lock()
	m.botActive[botID]++
	m.turnMu.Unlock()
}

func (m *Manager) markRuntimeEvents(ids []string) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil || len(ids) == 0 {
		return
	}
	if err := repository.MarkInboxProcessed(m.runtimeContext(), ids); err != nil {
		slog.Warn("更新 Bot Runtime Inbox 状态失败", "event_ids", ids, "error", err)
	}
}

// mergeRuntimeMessages 将同一自然话轮整理成一次 Agent 输入，保留全部附件并以换行
// 区分连续文本，避免用户分开发送的句子被错误拼接成一个词。
func mergeRuntimeMessages(messages []Message) Message {
	result := messages[len(messages)-1]
	result.ID = messages[0].ID
	result.ReplyMessageID = messages[0].ReplyMessageID
	var texts []string
	result.Attachments = nil
	for _, message := range messages {
		if text := strings.TrimSpace(message.Text); text != "" {
			texts = append(texts, text)
		}
		result.Attachments = append(result.Attachments, message.Attachments...)
		result.Mentioned = result.Mentioned || message.Mentioned
	}
	result.Text = strings.Join(texts, "\n")
	return result
}

// processRuntimeTurn 把聚合话轮交给原有 Agent Runtime 启动阶段。该函数只接收已经
// 通过入口策略的消息，避免恢复时再次执行指令或审批控制。
func (m *Manager) processRuntimeTurn(ctx context.Context, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	message := mergeRuntimeMessages(messages)
	config, configErr := m.resolveMessageConfig(ctx, message)
	if configErr != nil {
		return fmt.Errorf("读取 Bot Runtime 配置失败: %w", configErr)
	}
	// 在写入 Turn 前固定当前会话，保证会话归档/删除和动作记录都引用同一个
	// Conversation；后续 startAgentForMessage 只会复用这个活跃绑定。
	if strings.TrimSpace(message.RuntimeConversationID) == "" {
		_, conversationID, conversationErr := m.conversationForMessage(ctx, message)
		if conversationErr != nil {
			return conversationErr
		}
		message.RuntimeConversationID = conversationID
	}
	now := time.Now().UTC()
	eventIDs := make([]string, 0, len(messages))
	for _, item := range messages {
		eventIDs = append(eventIDs, runtimeInboxID(item))
	}
	message.RuntimeTurnID = newRuntimeID("turn", message.AdapterID, messageSource(message), strings.Join(eventIDs, "\x00"))
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository != nil {
		turn := BotTurn{ID: message.RuntimeTurnID, BotID: message.AdapterID, Source: messageSource(message), UserID: message.UserID, ConversationID: strings.TrimSpace(message.RuntimeConversationID), Text: boundedGroupText(message.Text, 4000), Status: "ready", CreatedAt: now, UpdatedAt: now}
		if err := repository.CommitTurn(ctx, turn, eventIDs); err != nil {
			return fmt.Errorf("保存 Bot Runtime 自然话轮失败: %w", err)
		}
		_ = repository.TouchSource(ctx, SourceState{BotID: message.AdapterID, Source: messageSource(message), ChatType: message.ChatType, LastUserID: message.UserID, LastSeenAt: now, LastTurnAt: &now, LastUserTurnAt: &now, CurrentConversationID: strings.TrimSpace(message.RuntimeConversationID)})
		if config.Extensions.RelationEnabled {
			_ = repository.TouchRelation(ctx, RelationState{BotID: message.AdapterID, ScopeType: "user", ScopeID: message.UserID, PreferredName: boundedGroupText(message.AutoName, 64), LastInteractionAt: now})
		}
		if config.Extensions.RelationEnabled && isGroupChat(message.ChatType) {
			_ = repository.TouchRelation(ctx, RelationState{BotID: message.AdapterID, ScopeType: "group", ScopeID: message.ChatID, PreferredName: boundedGroupText(message.AutoName, 64), LastInteractionAt: now})
			_ = repository.TouchRelation(ctx, RelationState{BotID: message.AdapterID, ScopeType: "group_member", ScopeID: message.ChatID + ":" + message.UserID, PreferredName: boundedGroupText(message.AutoName, 64), LastInteractionAt: now})
		}
	}
	if err := m.startAgentForMessage(ctx, message); err != nil {
		// Agent 启动阶段只向话轮边界返回原始错误；刷新器负责有限重试，
		// 最终失败时统一投影一次脱敏结果，避免每次重试重复打扰聊天。
		return err
	}
	return nil
}

func newRuntimeID(prefix string, values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + fmt.Sprintf("%x", digest[:16])
}

func (m *Manager) prepareRuntimeAction(ctx context.Context, message Message, actionType, idempotencyKey, summary string) (BotAction, bool, error) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return BotAction{}, true, nil
	}
	now := time.Now().UTC()
	planID := newRuntimeID("plan", message.AdapterID, messageSource(message), message.RuntimeTurnID, idempotencyKey)
	actionID := newRuntimeID("action", planID, actionType, idempotencyKey)
	plan := BotActionPlan{ID: planID, BotID: message.AdapterID, Source: messageSource(message), TurnID: message.RuntimeTurnID, ConversationID: strings.TrimSpace(message.RuntimeConversationID), Status: "running", CreatedAt: now, StartedAt: &now}
	action := BotAction{ID: actionID, PlanID: planID, Type: actionType, Sequence: 1, IdempotencyKey: idempotencyKey, Status: "pending", Payload: boundedGroupText(summary, 4000), ReplyTarget: strings.TrimSpace(message.ReplyMessageID), PayloadSummary: boundedGroupText(summary, 1000), CreatedAt: now, UpdatedAt: now}
	stored, err := repository.SaveActionPlan(ctx, plan, action)
	if err != nil {
		return BotAction{}, false, err
	}
	if stored.Status == "completed" || stored.Status == "unknown" {
		if stored.Status == "unknown" {
			// 外部副作用的结果在重启边界上不可判定，不能为了“自动恢复”
			// 重发一条可能已经成功的平台消息。
			slog.Warn("Bot Runtime 动作结果未知，跳过自动重发", "action_id", stored.ID, "action", stored.Type, "idempotency_key", stored.IdempotencyKey)
		}
		return stored, false, nil
	}
	if claimer, ok := repository.(RuntimeActionClaimer); ok {
		// pending/failed 只有一个调用方能原子领取；running 说明其他协程已经
		// 进入平台调用，当前重试必须等待其结果，不能再次发送。
		claimed, claimErr := claimer.ClaimAction(ctx, stored.ID, now)
		if claimErr != nil {
			return BotAction{}, false, claimErr
		}
		if !claimed {
			return stored, false, nil
		}
		stored.Status = "running"
	}
	return stored, true, nil
}

func (m *Manager) finishRuntimeAction(ctx context.Context, action BotAction, status, result string) {
	if action.ID == "" {
		return
	}
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return
	}
	if err := repository.FinishAction(context.WithoutCancel(ctx), action.ID, status, boundedGroupText(result, 1000), time.Now().UTC()); err != nil {
		slog.Warn("更新 Bot Runtime 动作状态失败", "action_id", action.ID, "status", status, "error", err)
	}
}

func (m *Manager) updateInstanceState(ctx context.Context, botID, lifecycle, availability string, platformEvent, action bool) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return
	}
	m.turnMu.Lock()
	load := m.botActive[botID]
	m.turnMu.Unlock()
	now := time.Now().UTC()
	state := BotInstanceState{BotID: botID, LifecycleState: lifecycle, Availability: availability, CurrentLoad: load, LastHeartbeatAt: now}
	if counter, ok := repository.(RuntimeSourceCounter); ok {
		if count, countErr := counter.CountActiveSources(context.WithoutCancel(ctx), botID, now.Add(-24*time.Hour)); countErr == nil {
			state.ActiveSources = count
		} else {
			slog.Warn("统计 Bot Runtime 活跃来源失败", "bot_id", botID, "error", countErr)
		}
	}
	if configured, err := m.Get(botID); err == nil {
		state.ConfigRevision = configured.RuntimeConfig.ConfigRevision
		state.PersonaRevision = configured.RuntimeConfig.PersonaRevision
	}
	if platformEvent {
		state.LastPlatformEventAt = now
	}
	if action {
		state.LastActionAt = now
	}
	if err := repository.UpdateInstanceState(context.WithoutCancel(ctx), state); err != nil {
		slog.Warn("更新 Bot Runtime 生命周期失败", "bot_id", botID, "state", lifecycle, "error", err)
	}
}

func (m *Manager) startRuntimeHeartbeat(ctx context.Context) {
	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		lastHeartbeat := make(map[string]time.Time)
		lastCleanup := time.Time{}
		for {
			now := time.Now().UTC()
			m.mu.RLock()
			ids := make([]string, 0, len(m.bots))
			for id, item := range m.bots {
				if item.Enabled {
					ids = append(ids, id)
				}
			}
			m.mu.RUnlock()
			enabled := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				enabled[id] = struct{}{}
			}
			// 先重试 received，再刷新心跳。这样来源队列释放后通常一个心跳周期
			// 内即可把暂存消息转入 pending，而不是只能等到下一次进程重启。
			m.restoreReceivedInbox(ctx, enabled)
			for _, id := range ids {
				// 心跳只维护已经进入连接生命周期的实例；平台连接断开或
				// 降级时不能被后台定时器错误地改回 online。
				m.mu.RLock()
				entry, runtimeOK := m.runtimes[id]
				m.mu.RUnlock()
				if !runtimeOK || (entry.lifecycle != "starting" && entry.lifecycle != "online") {
					continue
				}
				interval := 30 * time.Second
				if config, err := m.resolveBotExtensionConfig(ctx, id); err == nil && config.HeartbeatSeconds >= 5 {
					interval = time.Duration(config.HeartbeatSeconds) * time.Second
				}
				if last, ok := lastHeartbeat[id]; ok && now.Sub(last) < interval {
					continue
				}
				lastHeartbeat[id] = now
				// starting 只能表示适配器尚未完成连接；心跳只刷新存活时间，
				// 不能把尚未收到 platform.connected 的实例提前伪装成 online。
				lifecycle := entry.lifecycle
				availability := "unavailable"
				if lifecycle == "online" {
					availability = "available"
				}
				m.updateInstanceState(ctx, id, lifecycle, availability, false, false)
			}
			// 维护任务低频执行，避免心跳把清理 SQL 变成高频写入；每个 Bot
			// 使用自己的关系保留期，不能用一个全局值覆盖机器人级配置。
			if lastCleanup.IsZero() || now.Sub(lastCleanup) >= time.Hour {
				if repository, ok := m.runtimeMaintenanceRepository(); ok {
					for _, id := range ids {
						retention := 30 * 24 * time.Hour
						if config, configErr := m.resolveBotExtensionConfig(ctx, id); configErr == nil && config.RelationRetentionSeconds > 0 {
							retention = time.Duration(config.RelationRetentionSeconds) * time.Second
						}
						if err := repository.CleanupRuntime(ctx, id, now.Add(-retention)); err != nil {
							slog.Warn("Bot Runtime 维护清理失败", "bot_id", id, "error", err)
						}
					}
				}
				lastCleanup = now
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *Manager) resolveBotExtensionConfig(ctx context.Context, botID string) (ExtensionConfig, error) {
	m.mu.RLock()
	resolver := m.messageConfigResolver
	m.mu.RUnlock()
	if resolver == nil {
		return ExtensionConfig{HeartbeatSeconds: 30}, nil
	}
	config, err := resolver(ctx, strings.TrimSpace(botID), "", "")
	if err != nil {
		return ExtensionConfig{}, err
	}
	return config.Extensions, nil
}

func (m *Manager) runtimeMaintenanceRepository() (RuntimeMaintenanceRepository, bool) {
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	maintenance, ok := repository.(RuntimeMaintenanceRepository)
	return maintenance, ok
}

func (m *Manager) saveAdminRoute(ctx context.Context, bot Bot, message Message) {
	if isGroupChat(message.ChatType) || !m.globalAdminForMessage(ctx, bot, message) {
		return
	}
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return
	}
	if err := repository.SaveAdminRoute(ctx, AdminRoute{BotID: bot.ID, UserID: message.UserID, Platform: message.Platform, ChatID: message.ChatID, UpdatedAt: time.Now().UTC()}); err != nil {
		slog.Warn("保存管理员私聊路由失败", "bot_id", bot.ID, "user_id", message.UserID, "error", err)
	}
}

var errDeleteConfirmationNotFound = errors.New("删除确认不存在或已失效")

// ErrSourceQueueFull 表示当前来源的持久 Inbox 达到配置上限。
var ErrSourceQueueFull = errors.New("Bot Runtime 来源队列已满")

// ErrDeleteConfirmationNotFound 供持久化实现返回统一领域错误。
func ErrDeleteConfirmationNotFound() error { return errDeleteConfirmationNotFound }
