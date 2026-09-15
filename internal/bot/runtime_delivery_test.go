package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/conversation"
	"google.golang.org/adk/v2/session"
)

func TestBotRuntimeAdmissionUsesArtifactRefsAndStableIdempotency(t *testing.T) {
	manager, runtime, platform, cancel := newBotRuntimeTestManager(t)
	defer cancel()
	manager.SetRuntimeCoordinator(runtime)
	manager.SetAttachmentStorer(func(_ context.Context, request AttachmentStoreRequest) (agent.Attachment, error) {
		expectedUserID, _ := bindingFor(messageForBotRuntimeTest())
		if request.UserID != expectedUserID || request.ProducerType != "bot_message" || request.ProducerID != "bot:bot-1:telegram:message-1:1" {
			t.Fatalf("Artifact storer request boundary 错误: %#v", request)
		}
		ref := testBotAttachmentRef()
		return agent.Attachment{Name: ref.Name, MIMEType: ref.MIMEType, Ref: &ref}, nil
	})
	message := messageForBotRuntimeTest()
	if err := manager.handleMessage(context.Background(), message); err != nil {
		t.Fatalf("Bot Runtime 入站失败: %v", err)
	}
	request := runtime.firstRequest(t)
	if request.IdempotencyKey == "" || len(request.Attachments) != 1 || request.Attachments[0].Ref == nil || len(request.Attachments[0].Data) != 0 {
		t.Fatalf("Runtime 请求没有转换为 metadata-only Artifact ref: %#v", request)
	}
	if request.ConversationID == "" || request.SessionID != request.ConversationID {
		t.Fatalf("Bot conversation/session 绑定错误: %#v", request)
	}

	if err := manager.handleMessage(context.Background(), message); err != nil {
		t.Fatalf("重复 Bot Runtime 入站失败: %v", err)
	}
	requests := runtime.requestsSnapshot()
	if len(requests) != 2 || requests[0].IdempotencyKey != requests[1].IdempotencyKey || requests[0].ConversationID != requests[1].ConversationID {
		t.Fatalf("重复消息没有保持幂等键/会话绑定: %#v", requests)
	}
	if len(platform.sentSnapshot()) != 0 {
		t.Fatalf("无 Runtime 事件时不应伪造回复: %#v", platform.sentSnapshot())
	}
}

func TestBotRuntimeAdmissionCarriesBoundWorkspace(t *testing.T) {
	manager, runtime, _, cancel := newBotRuntimeTestManager(t)
	defer cancel()
	manager.SetRuntimeCoordinator(runtime)
	message := messageForBotRuntimeTest()
	message.Attachments = nil
	userID, conversationID := bindingFor(message)
	if _, err := manager.conversations.Create(context.Background(), conversation.CreateRequest{
		ID: conversationID, UserID: userID, WorkspaceID: "workspace-1", Title: "Bot 工作区会话",
	}); err != nil {
		t.Fatalf("创建工作区绑定会话失败: %v", err)
	}
	if err := manager.handleMessage(context.Background(), message); err != nil {
		t.Fatalf("带工作区绑定的 Bot Runtime 入站失败: %v", err)
	}
	request := runtime.firstRequest(t)
	if request.WorkspaceID == nil || *request.WorkspaceID != "workspace-1" {
		t.Fatalf("Bot Runtime 请求丢失会话工作区绑定: %#v", request)
	}
	message.Text = "/new"
	if err := manager.handleMessage(context.Background(), message); err != nil {
		t.Fatalf("工作区 Bot 会话新建失败: %v", err)
	}
	_, newConversationID, err := manager.conversationForMessage(context.Background(), message)
	if err != nil {
		t.Fatalf("读取新 Bot 会话失败: %v", err)
	}
	newConversation, err := manager.conversations.Get(context.Background(), userID, newConversationID)
	if err != nil {
		t.Fatalf("读取新 Bot 工作区会话失败: %v", err)
	}
	if newConversation.WorkspaceID != "workspace-1" {
		t.Fatalf("/new 不应丢失工作区绑定: %#v", newConversation)
	}
}

func TestBotArtifactProducerIDIsNamespacedByPlatform(t *testing.T) {
	manager, _, _, cancel := newBotRuntimeTestManager(t)
	defer cancel()
	var requests []AttachmentStoreRequest
	manager.SetAttachmentStorer(func(_ context.Context, request AttachmentStoreRequest) (agent.Attachment, error) {
		requests = append(requests, request)
		ref := testBotAttachmentRef()
		return agent.Attachment{Name: ref.Name, MIMEType: ref.MIMEType, Ref: &ref}, nil
	})
	for _, platform := range []Type{TypeTelegram, TypeOneBot11} {
		message := Message{ID: "same-message-id", AdapterID: "same-bot", Platform: platform, UserID: "user-1", ChatID: "chat-1", ChatType: "private", Attachments: []agent.Attachment{{Name: "screen.png", MIMEType: "image/png", Data: []byte{1}}}}
		if _, err := manager.storeMessageAttachments(context.Background(), message, "user-1", "conversation-1"); err != nil {
			t.Fatalf("%s 附件存储失败: %v", platform, err)
		}
	}
	if len(requests) != 2 || requests[0].ProducerID == requests[1].ProducerID || !strings.Contains(requests[0].ProducerID, ":telegram:") || !strings.Contains(requests[1].ProducerID, ":onebot11:") {
		t.Fatalf("Artifact producer ID 未按平台隔离: %#v", requests)
	}
}

func messageForBotRuntimeTest() Message {
	return Message{
		ID: "message-1", AdapterID: "bot-1", Platform: TypeTelegram, UserID: "user-1", ChatID: "chat-1", ChatType: "private",
		Text: "分析这个附件", Attachments: []agent.Attachment{{Name: "screen.png", MIMEType: "image/png", Data: []byte{1, 2, 3}}},
	}
}

func TestBotCommandsGroupTriggerAndApproval(t *testing.T) {
	manager, runtime, platform, cancel := newBotRuntimeTestManager(t)
	defer cancel()
	manager.SetRuntimeCoordinator(runtime)

	group := Message{ID: "group-1", AdapterID: "bot-1", Platform: TypeOneBot11, UserID: "user-1", ChatID: "group-1", ChatType: "group", Text: "普通消息"}
	if manager.groupMessageAllowed(group) {
		t.Fatal("默认 mention 模式不应接收未 @ 的群消息")
	}
	group.Mentioned = true
	if !manager.groupMessageAllowed(group) {
		t.Fatal("@ 机器人群消息应被接收")
	}
	manager.mu.Lock()
	item := manager.bots["bot-1"]
	item.GroupTriggerMode = groupTriggerAll
	manager.bots["bot-1"] = item
	manager.mu.Unlock()
	group.Mentioned = false
	if !manager.groupMessageAllowed(group) {
		t.Fatal("all 模式应接收未 @ 的群消息")
	}

	private := Message{ID: "command-1", AdapterID: "bot-1", Platform: TypeTelegram, UserID: "user-1", ChatID: "chat-1", ChatType: "private"}
	private.Text = "/help"
	if err := manager.handleMessage(context.Background(), private); err != nil {
		t.Fatalf("/help 失败: %v", err)
	}
	if last := platform.lastSent(); last == "" || !containsAny(last, "/new", "/status", "/cancel") {
		t.Fatalf("/help 输出缺少命令: %q", last)
	}
	private.Text = "/status"
	if err := manager.handleMessage(context.Background(), private); err != nil {
		t.Fatalf("/status 失败: %v", err)
	}
	if last := platform.lastSent(); last != "当前没有运行中的任务。" {
		t.Fatalf("/status 输出=%q", last)
	}

	_, conversationID, err := manager.conversationForMessage(context.Background(), private)
	if err != nil {
		t.Fatal(err)
	}
	runtime.setInvocation(agentruntime.Invocation{ID: "invocation-1", UserID: bindingUserID(private), ConversationID: conversationID, SessionID: conversationID, Status: agentruntime.InvocationWaitingApproval})
	runtime.setApproval(agentruntime.Approval{ID: "approval-1", InvocationID: "invocation-1", ConversationID: conversationID, ToolName: "write_file", Status: agentruntime.ApprovalPending})
	manager.deliverApproval(context.Background(), private, agentruntime.AgentEvent{InvocationID: "invocation-1", Type: agentruntime.EventApprovalRequested, Data: map[string]any{"approval_id": "approval-1", "tool_name": "write_file", "hint": "将修改工作区"}})
	if last := platform.lastSent(); !containsAny(last, "/approve 1", "/reject 1") {
		t.Fatalf("文本审批提示缺少序号命令: %q", last)
	}
	private.Text = "/approve 1"
	if err := manager.handleMessage(context.Background(), private); err != nil {
		t.Fatalf("/approve 1 失败: %v", err)
	}
	if runtime.resolvedApproval() != "approval-1" {
		t.Fatalf("审批没有进入 Runtime ResolveApproval: %q", runtime.resolvedApproval())
	}
}

func TestBotApprovalCannotCrossChatOrUserBoundary(t *testing.T) {
	manager, runtime, _, cancel := newBotRuntimeTestManager(t)
	defer cancel()
	manager.SetRuntimeCoordinator(runtime)

	owner := Message{ID: "owner-1", AdapterID: "bot-1", Platform: TypeTelegram, UserID: "user-1", ChatID: "owner-chat", ChatType: "private"}
	ownerUser, ownerConversation, err := manager.conversationForMessage(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	runtime.setInvocation(agentruntime.Invocation{ID: "invocation-owner", UserID: ownerUser, ConversationID: ownerConversation, SessionID: ownerConversation, Status: agentruntime.InvocationWaitingApproval})
	runtime.setApproval(agentruntime.Approval{ID: "approval-owner", InvocationID: "invocation-owner", ConversationID: ownerConversation, ToolName: "write_file", Status: agentruntime.ApprovalPending})

	other := owner
	other.ID = "other-1"
	other.ChatID = "other-chat"
	other.UserID = "other-user"
	if _, _, err := manager.conversationForMessage(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.resolveApproval(context.Background(), other, "approval-owner", true); err == nil {
		t.Fatal("不同聊天不应解析审批")
	}
	if got := runtime.resolvedApproval(); got != "" {
		t.Fatalf("跨聊天审批意外进入 Runtime: %q", got)
	}
}

func newBotRuntimeTestManager(t *testing.T) (*Manager, *botRuntimeTestCoordinator, *botTestPlatform, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	botRepo := &botTestRepository{items: map[string]Bot{"bot-1": {ID: "bot-1", Name: "测试 Bot", Type: TypeTelegram, TelegramToken: "token"}}}
	conversationRepo := newBotConversationRepository()
	conversationService, err := conversation.NewService(conversationRepo, session.InMemoryService(), "abot")
	if err != nil {
		cancel()
		t.Fatalf("创建测试 Conversation Service 失败: %v", err)
	}
	manager, err := NewManager(ctx, botRepo, &agent.Kernel{}, conversationService)
	if err != nil {
		cancel()
		t.Fatalf("创建测试 Bot Manager 失败: %v", err)
	}
	runtime := &botRuntimeTestCoordinator{invocations: make(map[string]agentruntime.Invocation), byIdempotency: make(map[string]string), approvals: make(map[string]agentruntime.Approval)}
	platform := &botTestPlatform{}
	manager.mu.Lock()
	manager.runtimes["bot-1"] = runtimeEntry{platform: platform}
	manager.mu.Unlock()
	return manager, runtime, platform, cancel
}

func testBotAttachmentRef() agent.AttachmentRef {
	return agent.AttachmentRef{ID: "artifact-1", Version: 1, Digest: "sha256:" + "a" /* only identity is needed by this boundary test */, Kind: "input_attachment", MIMEType: "image/png", Size: 3, Name: "screen.png"}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

type botTestPlatform struct {
	mu   sync.Mutex
	sent []string
}

func (p *botTestPlatform) Run(context.Context, Handler) error { return nil }
func (p *botTestPlatform) Test(context.Context) error         { return nil }
func (p *botTestPlatform) Close() error                       { return nil }
func (p *botTestPlatform) Send(_ context.Context, _ Message, text string) error {
	p.mu.Lock()
	p.sent = append(p.sent, text)
	p.mu.Unlock()
	return nil
}
func (p *botTestPlatform) sentSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sent...)
}
func (p *botTestPlatform) lastSent() string {
	items := p.sentSnapshot()
	if len(items) == 0 {
		return ""
	}
	return items[len(items)-1]
}

type botRuntimeTestCoordinator struct {
	mu            sync.Mutex
	invocations   map[string]agentruntime.Invocation
	byIdempotency map[string]string
	approvals     map[string]agentruntime.Approval
	requests      []agent.ChatRequest
	resolved      string
}

func (c *botRuntimeTestCoordinator) StartInvocation(_ context.Context, request agent.ChatRequest) (agentruntime.Invocation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	if id := c.byIdempotency[request.IdempotencyKey]; id != "" {
		return c.invocations[id], nil
	}
	for _, item := range c.invocations {
		if item.ConversationID == request.ConversationID && !item.Status.Terminal() {
			return agentruntime.Invocation{}, agentruntime.ErrConflict
		}
	}
	id := "invocation-1"
	item := agentruntime.Invocation{ID: id, UserID: request.UserID, ConversationID: request.ConversationID, SessionID: request.SessionID, Status: agentruntime.InvocationQueued}
	c.invocations[id] = item
	c.byIdempotency[request.IdempotencyKey] = id
	return item, nil
}
func (c *botRuntimeTestCoordinator) setInvocation(item agentruntime.Invocation) {
	c.mu.Lock()
	c.invocations[item.ID] = item
	c.mu.Unlock()
}
func (c *botRuntimeTestCoordinator) GetInvocation(_ context.Context, id string) (agentruntime.Invocation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.invocations[id]
	if !ok {
		return agentruntime.Invocation{}, agentruntime.ErrNotFound
	}
	return item, nil
}
func (c *botRuntimeTestCoordinator) GetApproval(_ context.Context, id string) (agentruntime.Approval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.approvals[id]
	if !ok {
		return agentruntime.Approval{}, agentruntime.ErrNotFound
	}
	return item, nil
}
func (c *botRuntimeTestCoordinator) ListApprovals(_ context.Context, invocationID string, status agentruntime.ApprovalStatus) ([]agentruntime.Approval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]agentruntime.Approval, 0, len(c.approvals))
	for _, item := range c.approvals {
		if item.InvocationID == invocationID && (status == "" || item.Status == status) {
			result = append(result, item)
		}
	}
	return result, nil
}
func (c *botRuntimeTestCoordinator) ListInvocations(_ context.Context, _ string, _ []agentruntime.InvocationStatus) ([]agentruntime.Invocation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]agentruntime.Invocation, 0, len(c.invocations))
	for _, item := range c.invocations {
		if !item.Status.Terminal() {
			result = append(result, item)
		}
	}
	return result, nil
}
func (c *botRuntimeTestCoordinator) Subscribe(_ context.Context, id string, _ int64) ([]agentruntime.AgentEvent, <-chan agentruntime.AgentEvent, func(), error) {
	c.mu.Lock()
	_, ok := c.invocations[id]
	c.mu.Unlock()
	if !ok {
		return nil, nil, func() {}, agentruntime.ErrNotFound
	}
	return nil, make(chan agentruntime.AgentEvent), func() {}, nil
}
func (c *botRuntimeTestCoordinator) CancelInvocation(_ context.Context, id string) (agentruntime.Invocation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.invocations[id]
	if !ok {
		return agentruntime.Invocation{}, agentruntime.ErrNotFound
	}
	item.Status = agentruntime.InvocationCancelling
	c.invocations[id] = item
	return item, nil
}
func (c *botRuntimeTestCoordinator) ResolveApproval(_ context.Context, id string, decision bool, _ string) (agentruntime.Approval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.approvals[id]
	if !ok {
		return agentruntime.Approval{}, agentruntime.ErrNotFound
	}
	if decision {
		item.Status = agentruntime.ApprovalApproved
	} else {
		item.Status = agentruntime.ApprovalRejected
	}
	c.approvals[id] = item
	c.resolved = id
	return item, nil
}
func (c *botRuntimeTestCoordinator) firstRequest(t *testing.T) agent.ChatRequest {
	t.Helper()
	requests := c.requestsSnapshot()
	if len(requests) == 0 {
		t.Fatal("Runtime 没有收到请求")
	}
	return requests[0]
}
func (c *botRuntimeTestCoordinator) requestsSnapshot() []agent.ChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agent.ChatRequest(nil), c.requests...)
}
func (c *botRuntimeTestCoordinator) setApproval(item agentruntime.Approval) {
	c.mu.Lock()
	c.approvals[item.ID] = item
	c.mu.Unlock()
}
func (c *botRuntimeTestCoordinator) resolvedApproval() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolved
}

type botTestRepository struct {
	mu    sync.Mutex
	items map[string]Bot
}

func (r *botTestRepository) List(context.Context) ([]Bot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Bot, 0, len(r.items))
	for _, item := range r.items {
		result = append(result, item)
	}
	return result, nil
}
func (r *botTestRepository) Get(_ context.Context, id string) (Bot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return Bot{}, ErrNotFound
	}
	return item, nil
}
func (r *botTestRepository) Save(_ context.Context, item Bot) error {
	r.mu.Lock()
	r.items[item.ID] = item
	r.mu.Unlock()
	return nil
}
func (r *botTestRepository) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[id]; !ok {
		return ErrNotFound
	}
	delete(r.items, id)
	return nil
}

type botConversationRepository struct {
	mu    sync.Mutex
	items map[string]conversation.Conversation
}

func newBotConversationRepository() *botConversationRepository {
	return &botConversationRepository{items: make(map[string]conversation.Conversation)}
}
func (r *botConversationRepository) List(_ context.Context, userID, workspaceID string, includeArchived bool) ([]conversation.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]conversation.Conversation, 0)
	for _, item := range r.items {
		if item.UserID != userID || (workspaceID != "*" && item.WorkspaceID != workspaceID) || (!includeArchived && item.Status != conversation.StatusActive) {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}
func (r *botConversationRepository) Get(_ context.Context, userID, id string) (conversation.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok || item.UserID != userID {
		return conversation.Conversation{}, conversation.ErrNotFound
	}
	return item, nil
}
func (r *botConversationRepository) GetByID(_ context.Context, id string) (conversation.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return conversation.Conversation{}, conversation.ErrNotFound
	}
	return item, nil
}
func (r *botConversationRepository) CountByWorkspace(_ context.Context, workspaceID string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, item := range r.items {
		if item.WorkspaceID == workspaceID {
			count++
		}
	}
	return count, nil
}
func (r *botConversationRepository) Save(_ context.Context, item conversation.Conversation) error {
	r.mu.Lock()
	r.items[item.ID] = item
	r.mu.Unlock()
	return nil
}
func (r *botConversationRepository) Delete(_ context.Context, userID, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok || item.UserID != userID {
		return conversation.ErrNotFound
	}
	delete(r.items, id)
	return nil
}

var _ RuntimeCoordinator = (*botRuntimeTestCoordinator)(nil)
var _ platform = (*botTestPlatform)(nil)
var _ Repository = (*botTestRepository)(nil)
var _ conversation.Repository = (*botConversationRepository)(nil)

var _ = errors.New
var _ = time.Second
