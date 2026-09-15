package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	commandApprove      = "approve"
	commandReject       = "reject"
)

type approvalTicket struct {
	ID           string
	InvocationID string
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

func (m *Manager) handleMessageWithRuntime(ctx context.Context, message Message) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if message.Control != nil {
		return m.handleApprovalControl(ctx, message)
	}
	message.Text = strings.TrimSpace(message.Text)
	if message.Text == "" && len(message.Attachments) == 0 {
		return nil
	}
	if !m.groupMessageAllowed(message) {
		return nil
	}
	command, isCommand := parseBotCommand(message.Text)
	if isCommand {
		return m.handleBotCommand(ctx, message, command)
	}
	// A bare number is the compact OneBot approval form. It is checked before
	// ordinary chat admission and only resolves a ticket bound to this chat.
	if strings.TrimSpace(message.Text) != "" && isDecimalApprovalChoice(message.Text) {
		if handled, err := m.resolveApprovalChoice(ctx, message, message.Text, true); handled {
			return err
		}
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
	invocation, startErr := coordinator.StartInvocation(ctx, request)
	if startErr != nil {
		_ = m.send(ctx, message, botRuntimeStartError(startErr))
		return fmt.Errorf("启动机器人 Runtime invocation 失败: %w", startErr)
	}
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
			return userID, current, nil
		}
	}
	if item, err := m.conversations.Get(ctx, userID, baseConversationID); err == nil {
		if item.Status == conversation.StatusActive {
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
			m.setActiveConversation(key, newest.ID)
			return userID, newest.ID, nil
		}
	} else {
		return "", "", fmt.Errorf("恢复机器人会话失败: %w", listErr)
	}
	item, err := m.conversations.Create(ctx, conversation.CreateRequest{
		ID: baseConversationID, UserID: userID, Title: fmt.Sprintf("%s · %s", message.Platform, message.ChatID),
	})
	if err != nil {
		return "", "", fmt.Errorf("创建机器人会话失败: %w", err)
	}
	m.setActiveConversation(key, item.ID)
	return userID, item.ID, nil
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
	created, createErr := m.conversations.Create(ctx, conversation.CreateRequest{ID: newID, UserID: userID, WorkspaceID: workspaceID, Title: botConversationTitle(message)})
	if createErr != nil {
		return fmt.Errorf("创建新机器人会话失败: %w", createErr)
	}
	key := chatBindingKey(message)
	m.mu.Lock()
	m.activeConversations[key] = created.ID
	delete(m.activeInvocations, key)
	delete(m.pendingApprovals, key)
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
	_, conversationID, conversationErr := m.conversationForMessage(ctx, message)
	if conversationErr != nil {
		return conversationErr
	}
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	id := m.activeInvocations[chatBindingKey(message)]
	m.mu.RUnlock()
	if coordinator == nil {
		return m.send(ctx, message, "当前没有可取消的任务。")
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
		return m.send(ctx, message, "当前没有可取消的任务。")
	}
	if _, err := coordinator.CancelInvocation(ctx, id); err != nil {
		return m.send(ctx, message, "取消任务失败："+trimError(err))
	}
	return m.send(ctx, message, "已请求取消当前任务。")
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
		process := func(event agentruntime.AgentEvent) bool {
			if event.InvocationID != invocationID {
				return false
			}
			switch event.Type {
			case agentruntime.EventAssistantMessage:
				if text := eventString(event.Data, "text"); text != "" {
					sentResponse = true
					_ = m.send(baseCtx, message, text)
				}
			case agentruntime.EventApprovalRequested:
				m.deliverApproval(baseCtx, message, event)
			case agentruntime.EventApprovalExpired:
				_ = m.send(baseCtx, message, "审批已过期，任务不会继续执行。")
			case agentruntime.EventApprovalResolved:
				_ = m.send(baseCtx, message, "审批决定已记录。")
			case agentruntime.EventInvocationWaiting:
				if eventString(event.Data, "reason") != "approval" {
					_ = m.send(baseCtx, message, "任务正在等待外部操作完成。")
				}
			case agentruntime.EventToolStarted, agentruntime.EventCommandStarted:
				name := eventString(event.Data, "name")
				if name == "" {
					name = "工作步骤"
				}
				_ = m.send(baseCtx, message, "正在执行："+safeProgressText(name))
			case agentruntime.EventInvocationCompleted:
				if !sentResponse {
					_ = m.send(baseCtx, message, "任务已完成，但没有返回文字结果。")
				}
				m.clearActiveInvocation(message, invocationID)
				return true
			case agentruntime.EventInvocationFailed:
				reason := eventString(event.Data, "error")
				if reason == "" {
					reason = "运行时返回失败"
				}
				_ = m.send(baseCtx, message, "任务失败："+trimError(errors.New(reason)))
				m.clearActiveInvocation(message, invocationID)
				return true
			case agentruntime.EventInvocationCancelled:
				_ = m.send(baseCtx, message, "任务已取消。")
				m.clearActiveInvocation(message, invocationID)
				return true
			case agentruntime.EventInvocationExpired:
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
	tickets = append(tickets, approvalTicket{ID: approvalID, InvocationID: event.InvocationID})
	if len(tickets) > 20 {
		tickets = tickets[len(tickets)-20:]
	}
	m.pendingApprovals[chatKey] = tickets
	m.mu.Unlock()
	prompt := ApprovalPrompt{ApprovalID: approvalID, ToolName: eventString(event.Data, "tool_name"), Hint: eventString(event.Data, "hint")}
	prompt.ExpiresAt = eventTime(event.Data, "expires_at")
	m.mu.RLock()
	entry, running := m.runtimes[message.AdapterID]
	m.mu.RUnlock()
	if !running {
		return
	}
	if sender, ok := entry.platform.(ApprovalSender); ok {
		if err := sender.SendApproval(ctx, message, prompt); err == nil {
			return
		}
	}
	position := len(tickets)
	text := fmt.Sprintf("需要审批（%d）：工具 %s\n%s", position, safeProgressText(prompt.ToolName), safeProgressText(prompt.Hint))
	if prompt.ExpiresAt != nil {
		text += "\n有效期至：" + prompt.ExpiresAt.Format(time.RFC3339)
	}
	text += fmt.Sprintf("\n回复 /approve %d 批准，/reject %d 拒绝。", position, position)
	_ = m.send(ctx, message, text)
}

func (m *Manager) handleApprovalControl(ctx context.Context, message Message) error {
	if message.Control == nil || message.Control.Kind != "approval" {
		return nil
	}
	approved := message.Control.Decision
	if _, err := m.resolveApproval(ctx, message, message.Control.ApprovalID, approved); err != nil {
		_ = m.send(ctx, message, "审批处理失败："+trimError(err))
		return err
	}
	return nil
}

func (m *Manager) resolveApprovalChoice(ctx context.Context, message Message, choice string, approved bool) (bool, error) {
	choice = strings.TrimSpace(choice)
	chatKey := chatBindingKey(message)
	m.mu.RLock()
	tickets := append([]approvalTicket(nil), m.pendingApprovals[chatKey]...)
	m.mu.RUnlock()
	if len(tickets) == 0 {
		var hydrateErr error
		tickets, hydrateErr = m.hydratePendingApprovals(ctx, message)
		if hydrateErr != nil {
			return false, hydrateErr
		}
		if len(tickets) == 0 {
			return false, nil
		}
	}
	approvalID := choice
	if number, err := parsePositiveInt(choice); err == nil && number <= len(tickets) {
		approvalID = tickets[number-1].ID
	}
	for _, ticket := range tickets {
		if ticket.ID == approvalID {
			_, err := m.resolveApproval(ctx, message, approvalID, approved)
			return true, err
		}
	}
	return false, nil
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
		tickets = append(tickets, approvalTicket{ID: approval.ID, InvocationID: item.ID})
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

func (m *Manager) resolveApproval(ctx context.Context, message Message, approvalID string, approved bool) (agentruntime.Approval, error) {
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
	resolved, err := coordinator.ResolveApproval(ctx, approvalID, approved, "机器人聊天决策")
	if err != nil {
		return agentruntime.Approval{}, err
	}
	_ = m.send(ctx, message, map[bool]string{true: "已批准该操作。", false: "已拒绝该操作。"}[approved])
	return resolved, nil
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
	m.mu.Unlock()
}

func chatBindingKey(message Message) string {
	chatID := strings.TrimSpace(message.ChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(message.UserID)
	}
	return strings.Join([]string{string(message.Platform), strings.TrimSpace(message.AdapterID), strings.TrimSpace(message.ChatType), chatID}, "\x00")
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

func isDecimalApprovalChoice(value string) bool {
	_, err := parsePositiveInt(value)
	return err == nil
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
