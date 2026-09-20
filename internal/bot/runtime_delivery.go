package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/conversation"
)

const (
	groupTriggerMention = "mention"
	groupTriggerAll     = "all"
	botAttachmentLimit  = 5
	commandHelp         = "help"
	commandNew          = "new"
	commandCancel       = "cancel"
	commandStatus       = "status"
)

type approvalTicket struct {
	ID           string
	InvocationID string
	Choices      []agentruntime.ApprovalChoice
}

type userInputTicket struct {
	InvocationID string
	Request      agentruntime.UserInputRequest
}

// approvalChoices 统一返回审批交互的两个安全选项；旧 Runtime 或旧事件中
// 即使携带了业务 choices，也不能把它们混进工具授权流程。
func approvalChoices(_ []agentruntime.ApprovalChoice) []agentruntime.ApprovalChoice {
	// 普通问题的单选、多选和文本答案由 UserInputRequest 负责，审批本身
	// 固定为“允许一次/拒绝”两个互斥选项，避免授权卡片承载业务选择。
	return agentruntime.DefaultApprovalChoices()
}

// approvalChoicesFromEvent 将 SSE/Runtime 事件中的 JSON 选项恢复为强类型，
// 保证纯文本平台和按钮平台使用同一份选项顺序。
func approvalChoicesFromEvent(data map[string]any) []agentruntime.ApprovalChoice {
	if data != nil {
		if items, ok := data["choices"].([]agentruntime.ApprovalChoice); ok {
			return approvalChoices(items)
		}
		if raw, ok := data["choices"]; ok {
			encoded, err := json.Marshal(raw)
			if err == nil {
				var items []agentruntime.ApprovalChoice
				if json.Unmarshal(encoded, &items) == nil {
					return approvalChoices(items)
				}
			}
		}
	}
	return agentruntime.DefaultApprovalChoices()
}

func choiceResultText(choice agentruntime.ApprovalChoice) string {
	if choice.Approved {
		return "已选择“" + safeProgressText(choice.Label) + "”，操作将继续执行。"
	}
	return "已选择“" + safeProgressText(choice.Label) + "”，操作不会执行。"
}

// userInputOptionsText 给没有原生按钮的聊天平台提供稳定的序号协议；多选
// 使用“1,3”形式，单选使用一个序号，文本题则直接接收正文。
func userInputOptionsText(request agentruntime.UserInputRequest) string {
	var builder strings.Builder
	if request.Title != "" {
		builder.WriteString(request.Title)
		builder.WriteString("\n")
	}
	builder.WriteString(request.Question)
	if request.Step > 0 && request.TotalSteps > 0 {
		builder.WriteString(fmt.Sprintf("（第 %d/%d 题）", request.Step, request.TotalSteps))
	}
	for index, option := range request.Options {
		builder.WriteString(fmt.Sprintf("\n%d. %s", index+1, safeProgressText(option.Label)))
		if option.Description != "" {
			builder.WriteString("：")
			builder.WriteString(safeProgressText(option.Description))
		}
	}
	if request.AllowFreeform {
		builder.WriteString("\n也可以直接输入自定义答案")
	}
	if request.AllowSkip {
		builder.WriteString("\n回复“跳过”可跳过本题")
	}
	if request.Kind == agentruntime.UserInputMultiSelect {
		builder.WriteString("\n多选请回复序号，例如：1,3")
	}
	return builder.String()
}

type botCommand struct {
	Name string
	Arg  string
}

func (b Bot) effectiveGroupTrigger() string {
	if strings.EqualFold(strings.TrimSpace(b.GroupTriggerMode), groupTriggerAll) {
		return groupTriggerAll
	}
	return groupTriggerMention
}

func (m *Manager) handleMessageWithRuntime(ctx context.Context, message Message) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 先登记来源，再做任何消息过滤；这样 AstrBot 风格的“所有已知 UMO”
	// 目录不会因为未 @、未配置唤醒词或只执行内置指令而漏项。
	m.recordMessageSource(ctx, message)
	// 先记录平台送入的完整消息，再做规范化和权限判断，确保被忽略的请求也能追踪。
	rawText := message.Text
	message.Text = strings.TrimSpace(message.Text)
	startedAt := time.Now()
	slog.Info("机器人收到请求", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_type", message.ChatType, "chat_id", message.ChatID, "user_id", message.UserID, "message_id", message.ID, "text", rawText, "normalized_text", message.Text, "mentioned", message.Mentioned, "attachments", message.Attachments, "control", message.Control)
	defer func() {
		if err != nil {
			// 统一记录处理失败及原始请求，避免只看到平台连接层的笼统错误。
			slog.Error("机器人请求处理失败", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "text", rawText, "duration", time.Since(startedAt), "error", err)
		}
	}()
	if message.Control != nil {
		return m.handleApprovalControl(ctx, message)
	}
	if message.Text == "" && len(message.Attachments) == 0 {
		// 空消息不会进入 Agent，但仍保留忽略原因，方便排查平台事件解析问题。
		slog.Info("机器人请求已忽略", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "reason", "empty")
		return nil
	}
	// 结构化选项回复是对当前待办任务的直接控制，不应被群聊 @ 规则或
	// 私聊唤醒词拦截；普通“批准/拒绝”等自然语言仍按普通消息处理。
	if handled, approvalErr := m.handlePendingApprovalReply(ctx, message); handled {
		return approvalErr
	}
	// 普通用户问题的回答可以是单选、多选或自由文本；只有当前聊天确实
	// 有待回答请求时才拦截，普通数字/文字消息不会被误当成控制指令。
	if handled, inputErr := m.handlePendingUserInputReply(ctx, message); handled {
		return inputErr
	}
	allowed, admissionErr := m.messageAllowed(ctx, &message)
	if admissionErr != nil {
		return fmt.Errorf("读取平台消息配置失败: %w", admissionErr)
	}
	if !allowed {
		// 未满足群聊触发或私聊唤醒条件时忽略，同时打印完整判断上下文。
		slog.Info("机器人请求已忽略", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "text", rawText, "mentioned", message.Mentioned, "reason", "wakeup_not_met")
		return nil
	}
	if message.Text == "" && len(message.Attachments) == 0 {
		// 唤醒词本身不是有效问题，去掉前缀后仍为空时不创建空会话。
		slog.Info("机器人请求已忽略", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "reason", "empty_after_wakeup")
		return nil
	}
	command, isCommand := parseBotCommand(message.Text)
	if isCommand {
		return m.handleBotCommand(ctx, message, command)
	}

	userID, conversationID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return err
	}
	lock := m.bindingLock(conversationID)
	lock.Lock()
	defer lock.Unlock()
	item, err := m.conversations.Get(ctx, userID, conversationID)
	if err != nil {
		return fmt.Errorf("读取机器人会话失败: %w", err)
	}
	if item.Status != conversation.StatusActive {
		return conversation.ErrArchived
	}
	timeout, timeoutErr := m.resolveRequestTimeout(ctx)
	if timeoutErr != nil {
		return fmt.Errorf("读取全局请求超时失败: %w", timeoutErr)
	}
	attachments, storeErr := m.storeMessageAttachments(ctx, message, userID, conversationID)
	if storeErr != nil {
		_ = m.send(ctx, message, "附件保存失败："+trimError(storeErr))
		return fmt.Errorf("保存机器人附件失败: %w", storeErr)
	}
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	if coordinator == nil {
		// 兼容没有装配 Durable Runtime 的嵌入方，并记录本次完整输入。
		slog.Info("机器人请求进入兼容 Agent 执行", "adapter_id", message.AdapterID, "platform", message.Platform, "conversation_id", conversationID, "text", message.Text, "attachments", attachments, "timeout", timeout)
		return m.handleLegacyMessage(ctx, message, userID, conversationID, attachments, timeout)
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	request := agent.ChatRequest{
		UserID: userID, BotID: message.AdapterID, ConversationID: conversationID, SessionID: conversationID,
		Message: message.Text, Attachments: attachments, IdempotencyKey: botMessageIdempotencyKey(message), Stream: true,
	}
	if workspaceID := strings.TrimSpace(item.WorkspaceID); workspaceID != "" {
		request.WorkspaceID = &workspaceID
	}
	// 记录交给内置 Agent 的完整请求对象，便于把平台消息和 Invocation 对齐。
	slog.Info("机器人请求启动内置 Agent", "adapter_id", message.AdapterID, "platform", message.Platform, "conversation_id", conversationID, "request", request)
	invocation, startErr := coordinator.StartInvocation(ctx, request)
	if startErr != nil {
		_ = m.send(ctx, message, botRuntimeStartError(startErr))
		return fmt.Errorf("启动机器人 Runtime invocation 失败: %w", startErr)
	}
	// 启动成功后记录 Durable Runtime 返回的任务标识，后续事件和响应都用它关联。
	slog.Info("机器人请求已启动", "adapter_id", message.AdapterID, "platform", message.Platform, "conversation_id", conversationID, "invocation_id", invocation.ID, "status", invocation.Status)
	chatKey := chatBindingKey(message)
	m.mu.Lock()
	m.activeInvocations[chatKey] = invocation.ID
	m.mu.Unlock()
	m.observeInvocation(message, chatKey, invocation.ID, timeout)
	return nil
}

func (m *Manager) handleLegacyMessage(ctx context.Context, message Message, userID, conversationID string, attachments []agent.Attachment, timeout time.Duration) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var response string
	for event, runErr := range m.kernel.Run(requestCtx, agent.ChatRequest{
		UserID: userID, BotID: message.AdapterID, ConversationID: conversationID, SessionID: conversationID,
		Message: message.Text, Attachments: attachments,
	}) {
		if runErr != nil {
			_ = m.send(requestCtx, message, "处理失败："+trimError(runErr))
			return fmt.Errorf("Agent 处理机器人消息失败: %w", runErr)
		}
		if event == nil || event.Partial || event.Content == nil || event.Author == "user" {
			continue
		}
		if text := agent.TextFromContent(event.Content); text != "" {
			response = text
		}
	}
	if strings.TrimSpace(response) == "" {
		return nil
	}
	// 兼容执行模式直接得到最终文本，发送动作本身还会由 Manager.Send 记录。
	slog.Info("机器人兼容 Agent 返回响应", "adapter_id", message.AdapterID, "platform", message.Platform, "conversation_id", conversationID, "text", response)
	return m.send(requestCtx, message, response)
}

func (m *Manager) groupMessageAllowed(message Message) bool {
	if !isGroupChat(message.ChatType) {
		return true
	}
	m.mu.RLock()
	item, ok := m.bots[message.AdapterID]
	m.mu.RUnlock()
	if !ok {
		// Direct embedders may call handleMessage with a platform not managed by
		// this Manager. Keep the safe default in that case.
		return message.Mentioned
	}
	return item.effectiveGroupTrigger() == groupTriggerAll || message.Mentioned
}

func isGroupChat(chatType string) bool {
	switch strings.ToLower(strings.TrimSpace(chatType)) {
	case "group", "supergroup", "channel":
		return true
	default:
		return false
	}
}

func parseBotCommand(text string) (botCommand, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return botCommand{}, false
	}
	name := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return botCommand{}, false
	}
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
	return botCommand{Name: name, Arg: arg}, true
}

// handleBotCommand authorises and dispatches one chat command. Commands are
// handled entirely here: they never enter the model prompt, so a /status or
// /help answer cannot pollute the conversation history.
func (m *Manager) handleBotCommand(ctx context.Context, message Message, command botCommand) error {
	registry := m.commandRegistry()
	if registry == nil {
		return m.send(ctx, message, "指令系统未初始化。")
	}
	bot, err := m.Get(strings.TrimSpace(message.AdapterID))
	if err != nil {
		return m.send(ctx, message, "读取机器人配置失败。")
	}
	descriptor, arg, ok := registry.Resolve(command.Name, command.Arg)
	if !ok {
		return m.send(ctx, message, m.unknownCommandText(ctx, bot, message))
	}
	authorization, err := m.authorizeCommand(ctx, bot, message, descriptor, arg)
	if err != nil {
		return err
	}
	if !authorization.Command.Enabled {
		// A disabled command is reported as disabled to everyone; the reason it
		// is disabled is an administrative detail, not chat content.
		return m.send(ctx, message, "该指令当前已停用。发送 /help 查看可用指令。")
	}
	if !authorization.allows() {
		m.recordAudit(ctx, CommandAudit{
			AdapterID: bot.ID, ChatID: message.ChatID, UserID: message.UserID,
			Command: descriptor.ID, Action: AuditCommandDenied, Result: "denied",
		})
		return m.send(ctx, message, authorization.denyReason())
	}
	return m.dispatchBotCommand(ctx, bot, message, authorization)
}

func (m *Manager) conversationForMessage(ctx context.Context, message Message) (string, string, error) {
	userID, baseConversationID := bindingFor(message)
	key := chatBindingKey(message)
	m.mu.RLock()
	current := m.activeConversations[key]
	m.mu.RUnlock()
	if current != "" {
		if item, err := m.conversations.Get(ctx, userID, current); err == nil && item.Status == conversation.StatusActive {
			if err := m.ensureConversationSource(ctx, message, item); err != nil {
				return "", "", err
			}
			return userID, current, nil
		}
	}
	if item, err := m.conversations.Get(ctx, userID, baseConversationID); err == nil {
		if item.Status == conversation.StatusActive {
			if err := m.ensureConversationSource(ctx, message, item); err != nil {
				return "", "", err
			}
			m.setActiveConversation(key, item.ID)
			return userID, item.ID, nil
		}
	} else if !errors.Is(err, conversation.ErrNotFound) {
		return "", "", fmt.Errorf("读取机器人会话失败: %w", err)
	}
	// /new archives the old binding and creates a marked title. Selecting the
	// newest marked active conversation makes the binding recover after restart
	// without introducing a separate migration table.
	if items, listErr := m.conversations.List(ctx, userID, "", true); listErr == nil {
		prefix := botConversationTitlePrefix(message)
		var newest conversation.Conversation
		for _, item := range items {
			if item.Status != conversation.StatusActive || !strings.HasPrefix(item.Title, prefix) {
				continue
			}
			if newest.ID == "" || item.UpdatedAt.After(newest.UpdatedAt) {
				newest = item
			}
		}
		if newest.ID != "" {
			if err := m.ensureConversationSource(ctx, message, newest); err != nil {
				return "", "", err
			}
			m.setActiveConversation(key, newest.ID)
			return userID, newest.ID, nil
		}
	} else {
		return "", "", fmt.Errorf("恢复机器人会话失败: %w", listErr)
	}
	item, err := m.conversations.Create(ctx, conversation.CreateRequest{
		ID: baseConversationID, UserID: userID, Source: messageSource(message), Title: fmt.Sprintf("%s · %s", message.Platform, message.ChatID),
	})
	if err != nil {
		return "", "", fmt.Errorf("创建机器人会话失败: %w", err)
	}
	m.setActiveConversation(key, item.ID)
	return userID, item.ID, nil
}

// ensureConversationSource 只为旧会话补齐来源，不改变已经存在的稳定 UMO。
func (m *Manager) ensureConversationSource(ctx context.Context, message Message, item conversation.Conversation) error {
	if strings.TrimSpace(item.Source) != "" {
		return nil
	}
	if _, err := m.conversations.EnsureSource(ctx, item.UserID, item.ID, messageSource(message)); err != nil {
		return fmt.Errorf("补齐机器人会话来源失败: %w", err)
	}
	return nil
}

func (m *Manager) setActiveConversation(key, id string) {
	m.mu.Lock()
	m.activeConversations[key] = strings.TrimSpace(id)
	m.mu.Unlock()
}

func (m *Manager) startNewConversation(ctx context.Context, message Message) error {
	userID, oldID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return err
	}
	oldLock := m.bindingLock(oldID)
	oldLock.Lock()
	defer oldLock.Unlock()
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	invocationID := m.activeInvocations[chatBindingKey(message)]
	m.mu.RUnlock()
	if coordinator != nil {
		if item, found, findErr := m.findActiveInvocation(ctx, message, oldID); findErr == nil && found {
			invocationID = item.ID
		} else if findErr != nil {
			return findErr
		}
	}
	if coordinator != nil && invocationID != "" {
		_, _ = coordinator.CancelInvocation(ctx, invocationID)
	}
	workspaceID := ""
	if item, getErr := m.conversations.Get(ctx, userID, oldID); getErr == nil && item.Status == conversation.StatusActive {
		workspaceID = strings.TrimSpace(item.WorkspaceID)
		if _, archiveErr := m.conversations.Archive(ctx, userID, oldID); archiveErr != nil {
			return fmt.Errorf("归档旧机器人会话失败: %w", archiveErr)
		}
	}
	newID := "bot-" + strings.TrimPrefix(agent.NewSessionID(), "session-")
	created, createErr := m.conversations.Create(ctx, conversation.CreateRequest{ID: newID, UserID: userID, Source: messageSource(message), WorkspaceID: workspaceID, Title: botConversationTitle(message)})
	if createErr != nil {
		return fmt.Errorf("创建新机器人会话失败: %w", createErr)
	}
	key := chatBindingKey(message)
	m.mu.Lock()
	m.activeConversations[key] = created.ID
	delete(m.activeInvocations, key)
	delete(m.pendingApprovals, key)
	delete(m.pendingUserInputs, key)
	m.mu.Unlock()
	return m.send(ctx, message, "已新建会话，之前的会话仍可在 WebUI 中查看。")
}

func (m *Manager) statusText(ctx context.Context, message Message) string {
	_, conversationID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return "读取会话状态失败：" + trimError(err)
	}
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	if coordinator == nil {
		return fmt.Sprintf("当前会话：%s。当前使用兼容模式运行。", shortIdentifier(conversationID))
	}
	item, found, findErr := m.findActiveInvocation(ctx, message, conversationID)
	if findErr != nil {
		return "读取任务状态失败：" + trimError(findErr)
	}
	if found {
		// A fresh Manager has no in-memory subscription map. Attaching here makes
		// /status the recovery point for progress and terminal delivery after a
		// process restart, while the Runtime remains the durable source of truth.
		m.observeInvocation(message, chatBindingKey(message), item.ID, 0)
		return fmt.Sprintf("当前任务：%s\n状态：%s", shortIdentifier(item.ID), item.Status)
	}
	return "当前没有运行中的任务。"
}

func (m *Manager) cancelChatInvocation(ctx context.Context, message Message) error {
	return m.controlChatInvocation(ctx, message, "取消", "可取消")
}

// stopChatInvocation 是对 AstrBot /stop 语义的直接实现，只停止任务而不改变会话历史。
func (m *Manager) stopChatInvocation(ctx context.Context, message Message) error {
	return m.controlChatInvocation(ctx, message, "停止", "可停止")
}

// controlChatInvocation 统一处理取消和停止，保证两条命令都只作用于当前会话的活动任务。
func (m *Manager) controlChatInvocation(ctx context.Context, message Message, action, available string) error {
	_, conversationID, conversationErr := m.conversationForMessage(ctx, message)
	if conversationErr != nil {
		return conversationErr
	}
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	id := m.activeInvocations[chatBindingKey(message)]
	m.mu.RUnlock()
	if coordinator == nil {
		return m.send(ctx, message, "当前没有"+available+"的任务。")
	}
	if id == "" {
		item, found, findErr := m.findActiveInvocation(ctx, message, conversationID)
		if findErr != nil {
			return m.send(ctx, message, "读取任务状态失败："+trimError(findErr))
		}
		if found {
			id = item.ID
			m.observeInvocation(message, chatBindingKey(message), id, 0)
		}
	}
	if id == "" {
		return m.send(ctx, message, "当前没有"+available+"的任务。")
	}
	if _, err := coordinator.CancelInvocation(ctx, id); err != nil {
		return m.send(ctx, message, action+"任务失败："+trimError(err))
	}
	return m.send(ctx, message, "已请求"+action+"当前任务。")
}

func (m *Manager) setActiveInvocation(message Message, id string) {
	m.mu.Lock()
	m.activeInvocations[chatBindingKey(message)] = strings.TrimSpace(id)
	m.mu.Unlock()
}

func runtimeActiveInvocationStatuses() []agentruntime.InvocationStatus {
	return []agentruntime.InvocationStatus{
		agentruntime.InvocationQueued, agentruntime.InvocationRunning, agentruntime.InvocationWaitingApproval,
		agentruntime.InvocationWaitingTool, agentruntime.InvocationWaitingUser, agentruntime.InvocationCancelling,
	}
}

// findActiveInvocation recovers the in-memory chat binding from Runtime after
// a process restart. Runtime filters by the bot-scoped user ID, and the final
// conversation comparison prevents one chat from controlling another.
func (m *Manager) findActiveInvocation(ctx context.Context, message Message, conversationID string) (agentruntime.Invocation, bool, error) {
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	id := m.activeInvocations[chatBindingKey(message)]
	m.mu.RUnlock()
	if coordinator == nil {
		return agentruntime.Invocation{}, false, nil
	}
	if id != "" {
		if item, err := coordinator.GetInvocation(ctx, id); err == nil {
			if !item.Status.Terminal() && item.ConversationID == conversationID {
				return item, true, nil
			}
		} else if !errors.Is(err, agentruntime.ErrNotFound) {
			return agentruntime.Invocation{}, false, err
		}
	}
	items, err := coordinator.ListInvocations(ctx, bindingUserID(message), runtimeActiveInvocationStatuses())
	if err != nil {
		return agentruntime.Invocation{}, false, err
	}
	for _, item := range items {
		if item.ConversationID != conversationID || item.Status.Terminal() {
			continue
		}
		m.setActiveInvocation(message, item.ID)
		return item, true, nil
	}
	return agentruntime.Invocation{}, false, nil
}

func (m *Manager) storeMessageAttachments(ctx context.Context, message Message, userID, conversationID string) ([]agent.Attachment, error) {
	if len(message.Attachments) == 0 {
		return nil, nil
	}
	m.mu.RLock()
	storer := m.attachmentStorer
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	if storer == nil {
		if coordinator != nil {
			if len(message.Attachments) > botAttachmentLimit {
				return nil, fmt.Errorf("附件数量不能超过 %d 个", botAttachmentLimit)
			}
			if err := validateBotAttachmentSizes(message.Attachments); err != nil {
				return nil, err
			}
			for index, attachment := range message.Attachments {
				if attachment.Ref == nil || len(attachment.Data) > 0 {
					return nil, fmt.Errorf("第 %d 个附件需要先保存为 Artifact ref", index+1)
				}
			}
		}
		return append([]agent.Attachment(nil), message.Attachments...), nil
	}
	if len(message.Attachments) > botAttachmentLimit {
		return nil, fmt.Errorf("附件数量不能超过 %d 个", botAttachmentLimit)
	}
	if err := validateBotAttachmentSizes(message.Attachments); err != nil {
		return nil, err
	}
	result := make([]agent.Attachment, 0, len(message.Attachments))
	for index, attachment := range message.Attachments {
		if attachment.Ref != nil {
			if len(attachment.Data) > 0 {
				return nil, fmt.Errorf("第 %d 个附件不能同时包含 ref 和 inline data", index+1)
			}
			result = append(result, attachment)
			continue
		}
		// Artifact producer IDs are shared across all adapters. Use the same
		// platform/adapter-scoped identity as Invocation idempotency so a
		// Telegram message ID cannot collide with a OneBot message ID.
		producerID := botMessageIdempotencyKey(message)
		stored, err := storer(ctx, AttachmentStoreRequest{
			UserID: userID, ConversationID: conversationID, ProducerType: "bot_message", ProducerID: fmt.Sprintf("%s:%d", producerID, index+1), Attachment: attachment,
		})
		if err != nil {
			return nil, fmt.Errorf("第 %d 个附件: %w", index+1, err)
		}
		if stored.Ref == nil || len(stored.Data) > 0 {
			return nil, fmt.Errorf("第 %d 个附件存储器必须返回只含 ref 的附件", index+1)
		}
		result = append(result, stored)
	}
	return result, nil
}

func validateBotAttachmentSizes(items []agent.Attachment) error {
	var total int64
	for index, attachment := range items {
		if attachment.Ref != nil && len(attachment.Data) > 0 {
			return fmt.Errorf("第 %d 个附件不能同时包含 ref 和 inline data", index+1)
		}
		size := int64(len(attachment.Data))
		if attachment.Ref != nil {
			size = attachment.Ref.Size
		}
		if size <= 0 || size > platformAttachmentMax {
			return fmt.Errorf("第 %d 个附件大小必须在 1 到 %d MB 之间", index+1, platformAttachmentMax>>20)
		}
		total += size
		if total > platformAttachmentSum {
			return fmt.Errorf("附件总大小不能超过 %d MB", platformAttachmentSum>>20)
		}
	}
	return nil
}

func (m *Manager) observeInvocation(message Message, chatKey, invocationID string, timeout time.Duration) {
	m.mu.Lock()
	if _, exists := m.observedInvocations[invocationID]; exists {
		m.mu.Unlock()
		return
	}
	m.observedInvocations[invocationID] = struct{}{}
	coordinator := m.runtimeCoordinator
	baseCtx := m.baseCtx
	interval := m.progressInterval
	m.mu.Unlock()
	if coordinator == nil {
		return
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		if timeout > 0 {
			timer := time.AfterFunc(timeout, func() {
				_, _ = coordinator.CancelInvocation(context.Background(), invocationID)
			})
			defer timer.Stop()
		}
		backlog, live, unsubscribe, err := coordinator.Subscribe(baseCtx, invocationID, 0)
		if err != nil {
			m.clearActiveInvocation(message, invocationID)
			_ = m.send(baseCtx, message, "任务订阅失败："+trimError(err))
			return
		}
		defer unsubscribe()
		sentResponse := false
		var streamedText strings.Builder
		streamedDeltaCount := 0
		flushStreamLog := func(status, finalText string) {
			if streamedDeltaCount == 0 {
				return
			}
			text := finalText
			streamedDeltaText := streamedText.String()
			if strings.TrimSpace(text) == "" {
				text = streamedDeltaText
			}
			// 把 assistant.delta 合并成一条完整响应，避免思考或回答的每个分片各自刷日志。
			attributes := []any{"adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "invocation_id", invocationID, "status", status, "delta_count", streamedDeltaCount, "text", text}
			if streamedDeltaText != "" && streamedDeltaText != text {
				// 思考增量和最终正文可能不是同一份文本，两者都保留但仍只生成一条日志。
				attributes = append(attributes, "streamed_text", streamedDeltaText)
			}
			slog.Info("机器人 Runtime 流式响应", attributes...)
			streamedText.Reset()
			streamedDeltaCount = 0
		}
		defer func() {
			// 订阅被取消或 Runtime 异常关闭时也要落下已经收到的部分正文。
			flushStreamLog("closed", "")
		}()
		process := func(event agentruntime.AgentEvent) bool {
			if event.InvocationID != invocationID {
				return false
			}
			if event.Type == agentruntime.EventAssistantDelta {
				if text := eventString(event.Data, "text"); text != "" {
					streamedText.WriteString(text)
					streamedDeltaCount++
					return false
				}
			}
			suppressResponseEventLog := false
			if event.Type == agentruntime.EventAssistantMessage {
				if text := eventString(event.Data, "text"); text != "" {
					if scope, _ := event.Data["scope"].(string); scope != "modal_fallback" && streamedDeltaCount > 0 {
						// 主模型已经发过增量时，用最终正文结束这次聚合日志，避免再次按事件拆开。
						flushStreamLog("completed", text)
						suppressResponseEventLog = true
					}
				}
			}
			// Runtime 事件包含工具调用、模型响应和终态信息；非文本增量继续逐条保留完整数据。
			if !suppressResponseEventLog {
				slog.Info("机器人 Runtime 响应事件", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "invocation_id", invocationID, "event_type", event.Type, "data", event.Data)
			}
			switch event.Type {
			case agentruntime.EventAssistantMessage:
				if text := eventString(event.Data, "text"); text != "" {
					sentResponse = true
					_ = m.send(baseCtx, message, text)
				}
			case agentruntime.EventApprovalRequested:
				m.deliverApproval(baseCtx, message, event)
			case agentruntime.EventUserInputRequested:
				m.deliverUserInput(baseCtx, message, event)
			case agentruntime.EventApprovalExpired:
				_ = m.send(baseCtx, message, "审批已过期，任务不会继续执行。")
			case agentruntime.EventApprovalResolved:
				_ = m.send(baseCtx, message, "审批决定已记录。")
			case agentruntime.EventInvocationWaiting:
				reason := eventString(event.Data, "reason")
				if reason != "approval" && reason != "user" {
					_ = m.send(baseCtx, message, "任务正在等待外部操作完成。")
				}
			case agentruntime.EventToolStarted, agentruntime.EventCommandStarted:
				name := eventString(event.Data, "name")
				if name == "" {
					name = "工作步骤"
				}
				_ = m.send(baseCtx, message, "正在执行："+safeProgressText(name))
			case agentruntime.EventInvocationCompleted:
				flushStreamLog("completed", "")
				if !sentResponse {
					_ = m.send(baseCtx, message, "任务已完成，但没有返回文字结果。")
				}
				m.clearActiveInvocation(message, invocationID)
				return true
			case agentruntime.EventInvocationFailed:
				flushStreamLog("failed", "")
				reason := eventString(event.Data, "error")
				if reason == "" {
					reason = "运行时返回失败"
				}
				_ = m.send(baseCtx, message, "任务失败："+trimError(errors.New(reason)))
				m.clearActiveInvocation(message, invocationID)
				return true
			case agentruntime.EventInvocationCancelled:
				flushStreamLog("cancelled", "")
				_ = m.send(baseCtx, message, "任务已取消。")
				m.clearActiveInvocation(message, invocationID)
				return true
			case agentruntime.EventInvocationExpired:
				flushStreamLog("expired", "")
				_ = m.send(baseCtx, message, "任务已过期。")
				m.clearActiveInvocation(message, invocationID)
				return true
			}
			return false
		}
		for _, event := range backlog {
			if process(event) {
				return
			}
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-baseCtx.Done():
				return
			case event, ok := <-live:
				if !ok || process(event) {
					return
				}
			case <-ticker.C:
				_ = m.send(baseCtx, message, "任务仍在处理中，请稍候。")
			}
		}
	}()
}

func (m *Manager) deliverApproval(ctx context.Context, message Message, event agentruntime.AgentEvent) {
	approvalID := eventString(event.Data, "approval_id")
	if approvalID == "" {
		return
	}
	chatKey := chatBindingKey(message)
	m.mu.Lock()
	tickets := m.pendingApprovals[chatKey]
	for _, ticket := range tickets {
		if ticket.ID == approvalID && ticket.InvocationID == event.InvocationID {
			// Runtime 回放可能再次投递同一事件；去重后避免用户看到重复审批卡片。
			m.mu.Unlock()
			return
		}
	}
	choices := approvalChoicesFromEvent(event.Data)
	tickets = append(tickets, approvalTicket{ID: approvalID, InvocationID: event.InvocationID, Choices: choices})
	if len(tickets) > 20 {
		tickets = tickets[len(tickets)-20:]
	}
	m.pendingApprovals[chatKey] = tickets
	m.mu.Unlock()
	prompt := ApprovalPrompt{ApprovalID: approvalID, ToolName: eventString(event.Data, "tool_name"), Hint: eventString(event.Data, "hint"), Choices: choices}
	prompt.ExpiresAt = eventTime(event.Data, "expires_at")
	// 审批请求可能由平台专用交互组件发送，先在 Manager 层记录完整提示内容。
	slog.Info("机器人发送审批请求", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "invocation_id", event.InvocationID, "approval_id", approvalID, "prompt", prompt)
	m.mu.RLock()
	entry, running := m.runtimes[message.AdapterID]
	m.mu.RUnlock()
	if !running {
		return
	}
	if sender, ok := entry.platform.(ApprovalSender); ok {
		if err := sender.SendApproval(ctx, message, prompt); err == nil {
			slog.Info("机器人审批请求发送成功", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "approval_id", approvalID)
			return
		} else {
			slog.Error("机器人审批请求发送失败", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "approval_id", approvalID, "prompt", prompt, "error", err)
		}
	}
	position := len(tickets)
	text := fmt.Sprintf("需要确认（请求 %d）：工具 %s\n%s", position, safeProgressText(prompt.ToolName), safeProgressText(prompt.Hint))
	if prompt.ExpiresAt != nil {
		text += "\n有效期至：" + prompt.ExpiresAt.Format(time.RFC3339)
	}
	text += "\n请选择："
	for index, choice := range prompt.Choices {
		text += fmt.Sprintf("\n%d. %s", index+1, safeProgressText(choice.Label))
	}
	text += "\n请只回复选项序号；当前聊天没有待确认请求时，数字不会被当作审批。"
	if len(tickets) > 1 {
		text += "\n若同时有多个请求，请回复：审批 <请求序号> <选项序号>。"
	}
	_ = m.send(ctx, message, text)
}

// deliverUserInput 将 ADK 的 RequestInput 投影为平台问题卡片。回答题目时
// 只保存 request ID 和 Invocation ID，恢复仍由 Runtime 重新校验边界。
func (m *Manager) deliverUserInput(ctx context.Context, message Message, event agentruntime.AgentEvent) {
	encoded, err := json.Marshal(event.Data)
	if err != nil {
		return
	}
	var request agentruntime.UserInputRequest
	if err := json.Unmarshal(encoded, &request); err != nil || strings.TrimSpace(request.ID) == "" {
		return
	}
	request.InvocationID = event.InvocationID
	chatKey := chatBindingKey(message)
	m.mu.Lock()
	// 兼容测试桩和旧嵌入方直接构造 Manager 的情况，首次收到问题时再补齐
	// 内存票据表，避免普通用户问题因为 nil map 触发 panic。
	if m.pendingUserInputs == nil {
		m.pendingUserInputs = make(map[string][]userInputTicket)
	}
	tickets := m.pendingUserInputs[chatKey]
	for _, ticket := range tickets {
		if ticket.Request.ID == request.ID && ticket.InvocationID == event.InvocationID {
			m.mu.Unlock()
			return
		}
	}
	tickets = append(tickets, userInputTicket{InvocationID: event.InvocationID, Request: request})
	if len(tickets) > 8 {
		tickets = tickets[len(tickets)-8:]
	}
	m.pendingUserInputs[chatKey] = tickets
	m.mu.Unlock()
	slog.Info("机器人发送用户问题", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "user_id", message.UserID, "invocation_id", event.InvocationID, "request", request)
	m.mu.RLock()
	entry, running := m.runtimes[message.AdapterID]
	m.mu.RUnlock()
	if running {
		if sender, ok := entry.platform.(UserInputSender); ok {
			if err := sender.SendUserInput(ctx, message, request); err == nil {
				return
			} else {
				slog.Error("机器人用户问题发送失败", "adapter_id", message.AdapterID, "platform", message.Platform, "chat_id", message.ChatID, "request", request, "error", err)
			}
		}
	}
	_ = m.send(ctx, message, userInputOptionsText(request))
}

func (m *Manager) handleApprovalControl(ctx context.Context, message Message) error {
	if message.Control == nil || message.Control.Kind != "approval" {
		return nil
	}
	choiceID := strings.TrimSpace(message.Control.ChoiceID)
	if choiceID == "" {
		// 旧平台只回传布尔值时仍允许兼容，但真实 Approval 会再次把
		// 布尔值映射到已持久化选项，避免控件直接绕过选项校验。
		if message.Control.Decision {
			choiceID = "approve"
		} else {
			choiceID = "reject"
		}
	}
	allowed, authErr := m.approvalReplyAllowed(ctx, message)
	if authErr != nil {
		_ = m.send(ctx, message, "审批权限校验失败："+trimError(authErr))
		return authErr
	}
	if !allowed {
		return nil
	}
	if _, err := m.resolveApprovalChoiceID(ctx, message, message.Control.ApprovalID, choiceID); err != nil {
		_ = m.send(ctx, message, "审批处理失败："+trimError(err))
		return err
	}
	return nil
}

// handlePendingApprovalReply 只解析“当前审批卡片”里的结构化数字选择。
// 它不会把批准、拒绝、可以等自然语言当作控制信号，避免普通聊天被拦截。
func (m *Manager) handlePendingApprovalReply(ctx context.Context, message Message) (bool, error) {
	selection, ok := parseApprovalSelection(message.Text)
	if !ok {
		return false, nil
	}
	tickets, err := m.pendingApprovalTickets(ctx, message)
	if err != nil {
		return true, err
	}
	if len(tickets) == 0 {
		return false, nil
	}
	ticketIndex := selection.TicketIndex
	if ticketIndex == 0 {
		if len(tickets) != 1 {
			return true, m.send(ctx, message, "当前有多个待确认请求，请回复：审批 <请求序号> <选项序号>。")
		}
		ticketIndex = 1
	}
	if ticketIndex < 1 || ticketIndex > len(tickets) {
		return true, m.send(ctx, message, "请求序号无效，请按审批提示中的序号选择。")
	}
	ticket := tickets[ticketIndex-1]
	if selection.ChoiceIndex < 1 || selection.ChoiceIndex > len(ticket.Choices) {
		return true, m.send(ctx, message, "选项序号无效，请按审批提示中的选项选择。")
	}
	choice := ticket.Choices[selection.ChoiceIndex-1]
	allowed, authErr := m.approvalReplyAllowed(ctx, message)
	if authErr != nil {
		_ = m.send(ctx, message, "审批权限校验失败："+trimError(authErr))
		return true, authErr
	}
	if !allowed {
		return true, nil
	}
	_, err = m.resolveApprovalChoiceID(ctx, message, ticket.ID, choice.ID)
	return true, err
}

// handlePendingUserInputReply 只在当前聊天存在待回答问题时接管消息。数字、
// 逗号分隔的数字和自定义文本都会先转换成结构化 payload，再交给 ADK。
func (m *Manager) handlePendingUserInputReply(ctx context.Context, message Message) (bool, error) {
	tickets, err := m.pendingUserInputTickets(ctx, message)
	if err != nil {
		return true, err
	}
	if len(tickets) == 0 {
		return false, nil
	}
	if len(tickets) > 1 {
		return true, m.send(ctx, message, "当前有多个待回答问题，请先完成最早的一题。")
	}
	ticket := tickets[0]
	response, summary, ok, parseErr := parseUserInputReply(message.Text, ticket.Request)
	if parseErr != nil {
		return true, m.send(ctx, message, parseErr.Error())
	}
	if !ok {
		return true, m.send(ctx, message, userInputOptionsText(ticket.Request))
	}
	if err := m.resolveUserInput(ctx, message, ticket, response); err != nil {
		_ = m.send(ctx, message, "提交回答失败："+trimError(err))
		return true, err
	}
	if summary == "" {
		summary = "已提交回答"
	}
	return true, m.send(ctx, message, summary+"，任务继续执行。")
}

func parseUserInputReply(value string, request agentruntime.UserInputRequest) (map[string]any, string, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, "", false, nil
	}
	if request.AllowSkip && (value == "跳过" || strings.EqualFold(value, "skip")) {
		return map[string]any{"skipped": true}, "已跳过本题", true, nil
	}
	if request.Kind == agentruntime.UserInputText {
		return map[string]any{"text": value}, "已填写答案", true, nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ' ' || r == '\t'
	})
	if len(parts) == 0 {
		return nil, "", false, nil
	}
	selected := make([]agentruntime.UserInputOption, 0, len(parts))
	seen := make(map[string]struct{})
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "选择" || part == "选" {
			continue
		}
		index, indexErr := strconv.Atoi(part)
		var option agentruntime.UserInputOption
		if indexErr == nil && index >= 1 && index <= len(request.Options) {
			option = request.Options[index-1]
		} else {
			for _, candidate := range request.Options {
				if candidate.ID == part {
					option = candidate
					break
				}
			}
		}
		if option.ID == "" {
			if request.AllowFreeform {
				return map[string]any{"text": value}, "已填写自定义答案", true, nil
			}
			return nil, "", false, fmt.Errorf("选项“%s”无效，请按题目中的序号选择。", part)
		}
		if _, exists := seen[option.ID]; exists {
			continue
		}
		seen[option.ID] = struct{}{}
		selected = append(selected, option)
	}
	if len(selected) == 0 {
		return nil, "", false, nil
	}
	if request.Kind == agentruntime.UserInputSingleSelect && len(selected) != 1 {
		return nil, "", false, errors.New("本题只能选择一个选项。")
	}
	if request.Kind == agentruntime.UserInputMultiSelect {
		if request.MinSelections > 0 && len(selected) < request.MinSelections {
			return nil, "", false, fmt.Errorf("本题至少选择 %d 项。", request.MinSelections)
		}
		if request.MaxSelections > 0 && len(selected) > request.MaxSelections {
			return nil, "", false, fmt.Errorf("本题最多选择 %d 项。", request.MaxSelections)
		}
	}
	ids := make([]string, 0, len(selected))
	labels := make([]string, 0, len(selected))
	for _, option := range selected {
		ids = append(ids, option.ID)
		labels = append(labels, option.Label)
	}
	if request.Kind == agentruntime.UserInputMultiSelect {
		return map[string]any{"selected_ids": ids, "selected_labels": labels}, "已选择：" + strings.Join(labels, "、"), true, nil
	}
	return map[string]any{"selected_id": ids[0], "selected_label": labels[0]}, "已选择：" + labels[0], true, nil
}

type userInputResumeCoordinator interface {
	ResumeInvocation(context.Context, string, agentruntime.InvocationResumeRequest) (agentruntime.Invocation, error)
}

type runtimeEventReader interface {
	ListEvents(context.Context, string, int64, int) ([]agentruntime.AgentEvent, error)
}

func (m *Manager) pendingUserInputTickets(ctx context.Context, message Message) ([]userInputTicket, error) {
	key := chatBindingKey(message)
	m.mu.RLock()
	tickets := append([]userInputTicket(nil), m.pendingUserInputs[key]...)
	m.mu.RUnlock()
	if len(tickets) > 0 {
		return tickets, nil
	}
	_, conversationID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return nil, err
	}
	item, found, err := m.findActiveInvocation(ctx, message, conversationID)
	if err != nil || !found || item.Status != agentruntime.InvocationWaitingUser {
		return nil, err
	}
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	reader, ok := coordinator.(runtimeEventReader)
	if !ok {
		return nil, nil
	}
	events, err := reader.ListEvents(ctx, item.ID, 0, 5000)
	if err != nil {
		return nil, err
	}
	resolved := make(map[string]struct{})
	var recovered []userInputTicket
	for _, event := range events {
		if event.Type == agentruntime.EventUserInputResolved || event.Type == agentruntime.EventInvocationResumed && eventString(event.Data, "reason") == "user" {
			id := eventString(event.Data, "request_id")
			if id == "" {
				id = eventString(event.Data, "wait_id")
			}
			if id != "" {
				resolved[id] = struct{}{}
			}
			continue
		}
		if event.Type != agentruntime.EventUserInputRequested {
			continue
		}
		var request agentruntime.UserInputRequest
		encoded, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil || json.Unmarshal(encoded, &request) != nil || request.ID == "" {
			continue
		}
		request.InvocationID = event.InvocationID
		if _, done := resolved[request.ID]; !done {
			recovered = append(recovered, userInputTicket{InvocationID: event.InvocationID, Request: request})
		}
	}
	if len(recovered) > 0 {
		m.mu.Lock()
		if m.pendingUserInputs == nil {
			m.pendingUserInputs = make(map[string][]userInputTicket)
		}
		m.pendingUserInputs[key] = append([]userInputTicket(nil), recovered...)
		m.mu.Unlock()
	}
	return recovered, nil
}

func (m *Manager) resolveUserInput(ctx context.Context, message Message, ticket userInputTicket, response map[string]any) error {
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	resumeCoordinator, ok := coordinator.(userInputResumeCoordinator)
	if !ok {
		return errors.New("Runtime 尚未支持用户问题恢复")
	}
	invocation, err := coordinator.GetInvocation(ctx, ticket.InvocationID)
	if err != nil {
		return err
	}
	_, conversationID, conversationErr := m.conversationForMessage(ctx, message)
	if conversationErr != nil {
		return conversationErr
	}
	if invocation.ConversationID != conversationID || invocation.UserID != bindingUserID(message) {
		return errors.New("该问题不属于当前聊天")
	}
	if _, err := resumeCoordinator.ResumeInvocation(ctx, ticket.InvocationID, agentruntime.InvocationResumeRequest{
		WaitID: ticket.Request.ID, Name: "adk_request_input", Response: response,
	}); err != nil {
		return err
	}
	m.removePendingUserInput(message, ticket.Request.ID)
	return nil
}

func (m *Manager) removePendingUserInput(message Message, requestID string) {
	key := chatBindingKey(message)
	m.mu.Lock()
	tickets := m.pendingUserInputs[key]
	filtered := tickets[:0]
	for _, ticket := range tickets {
		if ticket.Request.ID != requestID {
			filtered = append(filtered, ticket)
		}
	}
	if len(filtered) == 0 {
		delete(m.pendingUserInputs, key)
	} else {
		m.pendingUserInputs[key] = filtered
	}
	m.mu.Unlock()
}

// approvalReplyAllowed 让按钮和结构化选项共用审批权限策略，避免新增的
// 交互入口绕过原有的群管理员/全局管理员边界。
func (m *Manager) approvalReplyAllowed(ctx context.Context, message Message) (bool, error) {
	bot, err := m.Get(strings.TrimSpace(message.AdapterID))
	if err != nil {
		return false, fmt.Errorf("读取机器人配置失败: %w", err)
	}
	globalAdmin := m.globalAdminForMessage(ctx, bot, message)
	groupAdmin := false
	if isGroupChatType(message.ChatType) {
		admins, adminErr := m.groupAdmins(ctx, bot.ID, message.ChatID)
		if adminErr != nil {
			return false, adminErr
		}
		for _, admin := range admins {
			if strings.TrimSpace(admin) == strings.TrimSpace(message.UserID) {
				groupAdmin = true
				break
			}
		}
	}
	if globalAdmin || (isGroupChatType(message.ChatType) && groupAdmin) {
		return true, nil
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: bot.ID, ChatID: message.ChatID, UserID: message.UserID, Command: "approval",
		Action: AuditCommandDenied, Result: "denied",
	})
	if isGroupChatType(message.ChatType) {
		return false, m.send(ctx, message, "审批需要本群管理员权限。")
	}
	return false, m.send(ctx, message, "审批在私聊中需要全局管理员权限。")
}

// parseApprovalSelection 解析文本平台的选项选择。单个待办时允许回复“1”；
// 多个待办时必须显式写成“审批 请求序号 选项序号”，避免数字含义歧义。
type approvalSelection struct {
	TicketIndex int
	ChoiceIndex int
}

func parseApprovalSelection(value string) (approvalSelection, bool) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 1 {
		choice, err := parsePositiveInt(fields[0])
		if err != nil {
			return approvalSelection{}, false
		}
		return approvalSelection{ChoiceIndex: choice}, true
	}
	if len(fields) == 2 && (fields[0] == "选" || fields[0] == "选择") {
		choice, err := parsePositiveInt(fields[1])
		if err != nil {
			return approvalSelection{}, false
		}
		return approvalSelection{ChoiceIndex: choice}, true
	}
	if len(fields) == 3 && fields[0] == "审批" {
		ticket, ticketErr := parsePositiveInt(fields[1])
		choice, choiceErr := parsePositiveInt(fields[2])
		if ticketErr != nil || choiceErr != nil {
			return approvalSelection{}, false
		}
		return approvalSelection{TicketIndex: ticket, ChoiceIndex: choice}, true
	}
	return approvalSelection{}, false
}

// pendingApprovalTickets 优先读取内存票据，进程重启后再从 Durable Runtime 恢复当前聊天的审批。
func (m *Manager) pendingApprovalTickets(ctx context.Context, message Message) ([]approvalTicket, error) {
	chatKey := chatBindingKey(message)
	m.mu.RLock()
	tickets := append([]approvalTicket(nil), m.pendingApprovals[chatKey]...)
	m.mu.RUnlock()
	if len(tickets) > 0 {
		return tickets, nil
	}
	return m.hydratePendingApprovals(ctx, message)
}

func (m *Manager) hydratePendingApprovals(ctx context.Context, message Message) ([]approvalTicket, error) {
	_, conversationID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return nil, err
	}
	item, found, err := m.findActiveInvocation(ctx, message, conversationID)
	if err != nil || !found {
		return nil, err
	}
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	if coordinator == nil {
		return nil, nil
	}
	approvals, err := coordinator.ListApprovals(ctx, item.ID, agentruntime.ApprovalPending)
	if err != nil {
		return nil, err
	}
	tickets := make([]approvalTicket, 0, len(approvals))
	for _, approval := range approvals {
		if approval.ConversationID != "" && approval.ConversationID != conversationID {
			continue
		}
		tickets = append(tickets, approvalTicket{ID: approval.ID, InvocationID: item.ID, Choices: approvalChoices(approval.Choices)})
	}
	m.mu.Lock()
	if len(tickets) > 20 {
		tickets = tickets[len(tickets)-20:]
	}
	if len(tickets) > 0 {
		m.pendingApprovals[chatBindingKey(message)] = tickets
	}
	m.mu.Unlock()
	return tickets, nil
}

type approvalChoiceCoordinator interface {
	ResolveApprovalChoice(context.Context, string, string, string) (agentruntime.Approval, error)
}

// resolveApproval 保留旧的内部调用形态，真正的交互入口使用
// resolveApprovalChoiceID，以确保选项语义不会在 Bot 边界丢失。
func (m *Manager) resolveApproval(ctx context.Context, message Message, approvalID string, approved bool) (agentruntime.Approval, error) {
	choiceID := "reject"
	if approved {
		choiceID = "approve"
	}
	return m.resolveApprovalChoiceID(ctx, message, approvalID, choiceID)
}

func (m *Manager) resolveApprovalChoiceID(ctx context.Context, message Message, approvalID, choiceID string) (agentruntime.Approval, error) {
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return agentruntime.Approval{}, errors.New("审批 ID 不能为空")
	}
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	if coordinator == nil {
		return agentruntime.Approval{}, errors.New("Runtime 尚未装配")
	}
	approval, err := coordinator.GetApproval(ctx, approvalID)
	if err != nil {
		return agentruntime.Approval{}, err
	}
	_, conversationID, conversationErr := m.conversationForMessage(ctx, message)
	if conversationErr != nil {
		return agentruntime.Approval{}, conversationErr
	}
	// ConversationID on old/custom approval rows may be empty, so use the
	// Runtime invocation as the authoritative ownership boundary. This also
	// prevents a forged approval callback from resolving a task belonging to a
	// different chat or bot-scoped user.
	invocation, invocationErr := coordinator.GetInvocation(ctx, approval.InvocationID)
	if invocationErr != nil {
		return agentruntime.Approval{}, invocationErr
	}
	if invocation.ConversationID != conversationID || invocation.UserID != bindingUserID(message) {
		return agentruntime.Approval{}, errors.New("审批不属于当前聊天")
	}
	if approval.ConversationID != "" && approval.ConversationID != invocation.ConversationID {
		return agentruntime.Approval{}, errors.New("审批与 Runtime 会话不一致")
	}
	choiceID = strings.TrimSpace(choiceID)
	choices := approvalChoices(approval.Choices)
	var choice agentruntime.ApprovalChoice
	for _, candidate := range choices {
		if candidate.ID == choiceID {
			choice = candidate
			break
		}
	}
	if choice.ID == "" {
		return agentruntime.Approval{}, fmt.Errorf("审批选项 %q 无效", choiceID)
	}
	var resolved agentruntime.Approval
	if choiceResolver, ok := coordinator.(approvalChoiceCoordinator); ok {
		resolved, err = choiceResolver.ResolveApprovalChoice(ctx, approvalID, choice.ID, "机器人聊天选择")
	} else {
		// 兼容尚未升级的嵌入 Runtime；真实 Coordinator 始终走上面的
		// ChoiceID 路径，旧实现只能安全地接收允许/拒绝布尔值。
		resolved, err = coordinator.ResolveApproval(ctx, approvalID, choice.Approved, "机器人聊天选择")
	}
	if err != nil {
		return agentruntime.Approval{}, err
	}
	// 决定已经持久化后立刻删除本地票据，避免用户重复回复造成二次决议错误。
	m.removePendingApproval(message, approvalID)
	_ = m.send(ctx, message, choiceResultText(choice))
	return resolved, nil
}

// removePendingApproval 只删除当前聊天中指定的审批票据，不影响其他任务或聊天。
func (m *Manager) removePendingApproval(message Message, approvalID string) {
	chatKey := chatBindingKey(message)
	m.mu.Lock()
	tickets := m.pendingApprovals[chatKey]
	filtered := tickets[:0]
	for _, ticket := range tickets {
		if ticket.ID != approvalID {
			filtered = append(filtered, ticket)
		}
	}
	if len(filtered) == 0 {
		delete(m.pendingApprovals, chatKey)
	} else {
		m.pendingApprovals[chatKey] = filtered
	}
	m.mu.Unlock()
}

func (m *Manager) clearActiveInvocation(message Message, invocationID string) {
	key := chatBindingKey(message)
	m.mu.Lock()
	if m.activeInvocations[key] == invocationID {
		delete(m.activeInvocations, key)
	}
	if tickets := m.pendingApprovals[key]; len(tickets) > 0 {
		filtered := tickets[:0]
		for _, ticket := range tickets {
			if ticket.InvocationID != invocationID {
				filtered = append(filtered, ticket)
			}
		}
		if len(filtered) == 0 {
			delete(m.pendingApprovals, key)
		} else {
			m.pendingApprovals[key] = filtered
		}
	}
	if tickets := m.pendingUserInputs[key]; len(tickets) > 0 {
		filtered := tickets[:0]
		for _, ticket := range tickets {
			if ticket.InvocationID != invocationID {
				filtered = append(filtered, ticket)
			}
		}
		if len(filtered) == 0 {
			delete(m.pendingUserInputs, key)
		} else {
			m.pendingUserInputs[key] = filtered
		}
	}
	m.mu.Unlock()
}

func chatBindingKey(message Message) string {
	chatID := strings.TrimSpace(message.ChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(message.UserID)
	}
	return strings.Join([]string{string(message.Platform), strings.TrimSpace(message.AdapterID), strings.TrimSpace(message.ChatType), chatID}, "\x00")
}

// messageSessionID 返回平台侧会话 ID；群聊使用群/频道 ID，私聊使用聊天 ID，缺失时回退到用户 ID。
// 该值是 UMO 的第三段，不是 Abot 内部的 Conversation ID 或 Runtime Session ID。
func messageSessionID(message Message) string {
	chatID := strings.TrimSpace(message.ChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(message.UserID)
	}
	return chatID
}

// messageUMOMessageType 将平台适配器的聊天类型映射为 AstrBot 兼容的 MessageType。
// UMO 只允许 FriendMessage、GroupMessage、OtherMessage 三类，避免把 Telegram/OneBot 的原始枚举直接写进规则键。
func messageUMOMessageType(message Message) string {
	switch strings.ToLower(strings.TrimSpace(message.ChatType)) {
	case "private", "friend", "direct", "dm", "user":
		return "FriendMessage"
	case "group", "supergroup", "channel":
		return "GroupMessage"
	default:
		return "OtherMessage"
	}
}

// messageSource 生成 AstrBot 兼容的稳定 UMO：platform_id:message_type:session_id。
// AdapterID 对应 AstrBot 的 platform_id；适配器尚未注入 ID 时才回退到平台类型，避免产生空的来源键。
func messageSource(message Message) string {
	platformID := strings.TrimSpace(message.AdapterID)
	if platformID == "" {
		platformID = strings.TrimSpace(string(message.Platform))
	}
	return strings.Join([]string{platformID, messageUMOMessageType(message), messageSessionID(message)}, ":")
}

func bindingUserID(message Message) string {
	userID, _ := bindingFor(message)
	return userID
}

func botConversationTitlePrefix(message Message) string {
	return fmt.Sprintf("abot bot %s · %s ·", message.Platform, strings.TrimSpace(message.ChatID))
}

func botConversationTitle(message Message) string {
	_, conversationID := bindingFor(message)
	return fmt.Sprintf("%s %s", botConversationTitlePrefix(message), shortIdentifier(conversationID))
}

func botMessageIdempotencyKey(message Message) string {
	if id := strings.TrimSpace(message.ID); id != "" {
		return "bot:" + strings.TrimSpace(message.AdapterID) + ":" + string(message.Platform) + ":" + id
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(chatBindingKey(message)))
	_, _ = hash.Write([]byte("\x00" + strings.TrimSpace(message.Text)))
	for _, attachment := range message.Attachments {
		if attachment.Ref != nil {
			_, _ = hash.Write([]byte(attachment.Ref.ID + ":" + attachment.Ref.Digest))
		} else {
			sum := sha256.Sum256(attachment.Data)
			_, _ = hash.Write([]byte(hex.EncodeToString(sum[:])))
		}
	}
	return "bot:" + hex.EncodeToString(hash.Sum(nil))[:32]
}

func shortIdentifier(value any) string {
	text := fmt.Sprint(value)
	if len(text) > 16 {
		return text[:16]
	}
	return text
}

func botRuntimeStartError(err error) string {
	if errors.Is(err, agentruntime.ErrConflict) {
		return "当前聊天已有任务在运行，请发送 /status 或 /cancel。"
	}
	return "任务启动失败：" + trimError(err)
}

func eventString(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	value, _ := data[key].(string)
	return strings.TrimSpace(value)
}

func eventTime(data map[string]any, key string) *time.Time {
	if data == nil {
		return nil
	}
	var value time.Time
	switch raw := data[key].(type) {
	case time.Time:
		value = raw
	case *time.Time:
		if raw == nil {
			return nil
		}
		value = *raw
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
		if err != nil {
			return nil
		}
		value = parsed
	default:
		return nil
	}
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func safeProgressText(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	if len([]rune(value)) > 160 {
		return string([]rune(value)[:160]) + "…"
	}
	return value
}

func parsePositiveInt(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("序号为空")
	}
	result := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("序号无效")
		}
		result = result*10 + int(r-'0')
		if result > 100000 {
			return 0, errors.New("序号过大")
		}
	}
	if result <= 0 {
		return 0, errors.New("序号必须为正数")
	}
	return result, nil
}
