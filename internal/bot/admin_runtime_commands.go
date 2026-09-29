package bot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/conversation"
)

const adminCommandPageSize = 10

func (m *Manager) botConversations(ctx context.Context, botID string, includeArchived bool) ([]conversation.Conversation, error) {
	items, err := m.conversations.ListAll(ctx, "", includeArchived)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSpace(botID) + ":"
	result := make([]conversation.Conversation, 0, len(items))
	for _, item := range items {
		if strings.HasPrefix(strings.TrimSpace(item.Source), prefix) {
			result = append(result, item)
		}
	}
	return result, nil
}

func parseListPage(argument string) (string, int) {
	status := "active"
	page := 1
	for _, field := range strings.Fields(strings.ToLower(argument)) {
		if field == "active" || field == "archived" || field == "all" || field == "pending" {
			status = field
			continue
		}
		if parsed, err := strconv.Atoi(field); err == nil && parsed > 0 {
			page = parsed
		}
	}
	return status, page
}

func (m *Manager) commandSessions(ctx context.Context, bot Bot, message Message, argument string) error {
	status, page := parseListPage(argument)
	items, err := m.botConversations(ctx, bot.ID, status != "active")
	if err != nil {
		return m.send(ctx, message, "读取会话失败："+runtimeUserFacingError(err))
	}
	filtered := items[:0]
	for _, item := range items {
		if status == "all" || string(item.Status) == status {
			filtered = append(filtered, item)
		}
	}
	start := (page - 1) * adminCommandPageSize
	if start >= len(filtered) {
		return m.send(ctx, message, fmt.Sprintf("会话列表为空（第 %d 页）。", page))
	}
	end := start + adminCommandPageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	lines := []string{fmt.Sprintf("会话（%s，第 %d/%d 页）：", status, page, (len(filtered)+adminCommandPageSize-1)/adminCommandPageSize)}
	for _, item := range filtered[start:end] {
		name := strings.TrimSpace(item.SourceName)
		if name == "" {
			name = strings.TrimSpace(item.Source)
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s", publicReference("S", item.ID), item.Status, name))
	}
	lines = append(lines, "使用 /session archive <会话编号>、/session restore <会话编号> 或 /session delete <会话编号>。")
	m.recordRuntimeAdminAudit(ctx, bot, message, "sessions", AuditSessionList, status, "ok")
	return m.send(ctx, message, strings.Join(lines, "\n"))
}

func (m *Manager) resolveBotConversation(ctx context.Context, botID, reference string) (conversation.Conversation, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return conversation.Conversation{}, errors.New("缺少会话编号")
	}
	items, err := m.botConversations(ctx, botID, true)
	if err != nil {
		return conversation.Conversation{}, err
	}
	var matched []conversation.Conversation
	for _, item := range items {
		if item.ID == reference || publicReference("S", item.ID) == strings.ToUpper(reference) {
			matched = append(matched, item)
		}
	}
	if len(matched) == 0 {
		return conversation.Conversation{}, conversation.ErrNotFound
	}
	if len(matched) > 1 {
		return conversation.Conversation{}, errors.New("会话编号不唯一，请输入更长编号")
	}
	return matched[0], nil
}

func (m *Manager) commandSessionArchive(ctx context.Context, bot Bot, message Message, argument string) error {
	item, err := m.resolveBotConversation(ctx, bot.ID, argument)
	if err != nil {
		return m.send(ctx, message, "归档会话失败："+runtimeUserFacingError(err))
	}
	if active, activeErr := m.conversationHasActiveInvocation(ctx, item); activeErr != nil {
		return m.send(ctx, message, "检查会话任务失败："+runtimeUserFacingError(activeErr))
	} else if active {
		return m.send(ctx, message, "该会话仍有运行中、排队中或待审批任务，请先在对应会话使用 /stop 或 /cancel，并处理待审批事项。")
	}
	if _, err := m.conversations.Archive(ctx, item.UserID, item.ID); err != nil {
		return m.send(ctx, message, "归档会话失败："+runtimeUserFacingError(err))
	}
	m.recordRuntimeAdminAudit(ctx, bot, message, "session archive", AuditSessionArchive, item.ID, "ok")
	return m.send(ctx, message, "已归档会话 "+publicReference("S", item.ID)+"。")
}

func (m *Manager) conversationHasActiveInvocation(ctx context.Context, item conversation.Conversation) (bool, error) {
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	if coordinator == nil {
		return false, nil
	}
	items, err := coordinator.ListInvocations(ctx, item.UserID, runtimeActiveInvocationStatuses())
	if err != nil {
		return false, err
	}
	for _, invocation := range items {
		if invocation.ConversationID == item.ID && !invocation.Status.Terminal() {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) commandSessionRestore(ctx context.Context, bot Bot, message Message, argument string) error {
	item, err := m.resolveBotConversation(ctx, bot.ID, argument)
	if err != nil {
		return m.send(ctx, message, "恢复会话失败："+runtimeUserFacingError(err))
	}
	if _, err := m.conversations.Unarchive(ctx, item.UserID, item.ID); err != nil {
		return m.send(ctx, message, "恢复会话失败："+runtimeUserFacingError(err))
	}
	m.recordRuntimeAdminAudit(ctx, bot, message, "session restore", AuditSessionRestore, item.ID, "ok")
	return m.send(ctx, message, "已恢复会话 "+publicReference("S", item.ID)+"。")
}

func (m *Manager) commandSessionDelete(ctx context.Context, bot Bot, message Message, argument string) error {
	fields := strings.Fields(argument)
	if len(fields) == 0 {
		return m.send(ctx, message, "用法：/session delete <会话编号>；收到确认码后再发送 /session delete <会话编号> confirm <确认码>。")
	}
	item, err := m.resolveBotConversation(ctx, bot.ID, fields[0])
	if err != nil {
		return m.send(ctx, message, "删除会话失败："+runtimeUserFacingError(err))
	}
	if item.Status != conversation.StatusArchived {
		return m.send(ctx, message, "只能删除已归档会话，请先使用 /session archive "+publicReference("S", item.ID)+"。")
	}
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if repository == nil {
		return m.send(ctx, message, "Bot Runtime 持久仓储未配置，不能安全执行删除确认。")
	}
	if len(fields) >= 3 && strings.EqualFold(fields[1], "confirm") {
		confirmation, consumeErr := repository.ConsumeDeleteConfirmation(ctx, bot.ID, message.UserID, item.ID, fields[2], time.Now().UTC())
		if consumeErr != nil || confirmation.Revision != item.UpdatedAt.UnixNano() {
			return m.send(ctx, message, "确认码无效、已过期或会话状态已经变化。")
		}
		if err := m.conversations.Delete(ctx, item.UserID, item.ID); err != nil {
			return m.send(ctx, message, "删除会话失败："+runtimeUserFacingError(err))
		}
		m.recordRuntimeAdminAudit(ctx, bot, message, "session delete", AuditSessionDelete, item.ID, "deleted")
		return m.send(ctx, message, "已永久删除会话 "+publicReference("S", item.ID)+" 及其对话、附件和任务数据。")
	}
	code, err := newDeleteConfirmationCode()
	if err != nil {
		return m.send(ctx, message, "生成删除确认失败："+runtimeUserFacingError(err))
	}
	now := time.Now().UTC()
	confirmation := DeleteConfirmation{ID: bot.ID + ":" + message.UserID + ":" + item.ID, BotID: bot.ID, AdminUserID: message.UserID, ConversationID: item.ID, Revision: item.UpdatedAt.UnixNano(), Code: code, ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now}
	if err := repository.SaveDeleteConfirmation(ctx, confirmation); err != nil {
		return m.send(ctx, message, "保存删除确认失败："+runtimeUserFacingError(err))
	}
	ref := publicReference("S", item.ID)
	return m.send(ctx, message, fmt.Sprintf("即将永久删除已归档会话 %s。此操作会一并删除对话、附件和任务数据。\n请在 10 分钟内发送：/session delete %s confirm %s", ref, ref, code))
}

func newDeleteConfirmationCode() (string, error) {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(buffer)), nil
}

type botApprovalItem struct {
	Approval   agentruntime.Approval
	Invocation agentruntime.Invocation
}

func (m *Manager) botApprovals(ctx context.Context, botID string, pendingOnly bool) ([]botApprovalItem, error) {
	m.mu.RLock()
	coordinator := m.runtimeCoordinator
	m.mu.RUnlock()
	if coordinator == nil {
		return nil, errors.New("Agent Runtime 未配置")
	}
	invocations, err := coordinator.ListInvocations(ctx, "", nil)
	if err != nil {
		return nil, err
	}
	items := make([]botApprovalItem, 0)
	for _, invocation := range invocations {
		if invocation.BotID != botID {
			continue
		}
		status := agentruntime.ApprovalStatus("")
		if pendingOnly {
			status = agentruntime.ApprovalPending
		}
		approvals, listErr := coordinator.ListApprovals(ctx, invocation.ID, status)
		if listErr != nil {
			return nil, listErr
		}
		for _, approval := range approvals {
			items = append(items, botApprovalItem{Approval: approval, Invocation: invocation})
		}
	}
	return items, nil
}

func (m *Manager) commandApprovals(ctx context.Context, bot Bot, message Message, argument string) error {
	status, page := parseListPage(argument)
	items, err := m.botApprovals(ctx, bot.ID, status != "all")
	if err != nil {
		return m.send(ctx, message, "读取审批失败："+runtimeUserFacingError(err))
	}
	start := (page - 1) * adminCommandPageSize
	if start >= len(items) {
		return m.send(ctx, message, fmt.Sprintf("审批列表为空（第 %d 页）。", page))
	}
	end := start + adminCommandPageSize
	if end > len(items) {
		end = len(items)
	}
	displayStatus := status
	if displayStatus == "active" {
		// 审批没有 active 状态；不带参数时沿用列表命令的解析器，但向用户
		// 显示真实语义，避免把 pending 误报成会话式 active。
		displayStatus = "pending"
	}
	lines := []string{fmt.Sprintf("审批（%s，第 %d/%d 页）：", displayStatus, page, (len(items)+adminCommandPageSize-1)/adminCommandPageSize)}
	for _, item := range items[start:end] {
		lines = append(lines, fmt.Sprintf("%s  %s  %s", publicReference("A", item.Approval.ID), item.Approval.Status, safeProgressText(item.Approval.ToolName)))
	}
	lines = append(lines, "使用 /approve <审批编号> <选项编号> 或 /reject <审批编号> <选项编号> [原因]。")
	m.recordRuntimeAdminAudit(ctx, bot, message, "approvals", AuditApprovalList, status, "ok")
	return m.send(ctx, message, strings.Join(lines, "\n"))
}

func (m *Manager) commandApprovalDecision(ctx context.Context, bot Bot, message Message, argument string, approved bool) error {
	fields := strings.Fields(argument)
	if len(fields) < 2 {
		return m.send(ctx, message, "用法：/approve <审批编号> <选项编号>；拒绝用 /reject <审批编号> <选项编号> [原因]。")
	}
	items, err := m.botApprovals(ctx, bot.ID, true)
	if err != nil {
		return m.send(ctx, message, "读取审批失败："+runtimeUserFacingError(err))
	}
	isGlobalAdmin := m.globalAdminForMessage(ctx, bot, message)
	conversationID := ""
	if !isGlobalAdmin {
		if !isGroupChat(message.ChatType) {
			return m.send(ctx, message, "只有平台管理员可以在私聊中处理审批。")
		}
		_, conversationID, err = m.conversationForMessage(ctx, message)
		if err != nil {
			return m.send(ctx, message, "读取当前群会话失败："+runtimeUserFacingError(err))
		}
	}
	var matched []botApprovalItem
	for _, item := range items {
		if !isGlobalAdmin && item.Invocation.ConversationID != conversationID {
			continue
		}
		if item.Approval.ID == fields[0] || publicReference("A", item.Approval.ID) == strings.ToUpper(fields[0]) {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		return m.send(ctx, message, "审批编号不存在或不唯一。")
	}
	reason := "管理员聊天审批"
	if !approved && len(fields) > 2 {
		reason = strings.Join(fields[2:], " ")
	}
	selectors, selectorErr := parseChoiceIndexes(fields[1])
	if selectorErr != nil {
		return m.send(ctx, message, "审批选项无效，请使用编号或逗号分隔的多个编号。")
	}
	choiceIDs := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		choice, choiceErr := approvalChoiceBySelector(matched[0].Approval, strconv.Itoa(selector), approved)
		if choiceErr != nil {
			return m.send(ctx, message, choiceErr.Error())
		}
		choiceIDs = append(choiceIDs, choice.ID)
	}
	if _, err := m.resolveApprovalChoiceIDsWithReason(ctx, message, matched[0].Approval.ID, choiceIDs, reason); err != nil {
		return m.send(ctx, message, "审批处理失败："+runtimeUserFacingError(err))
	}
	m.recordRuntimeAdminAudit(ctx, bot, message, map[bool]string{true: "approve", false: "reject"}[approved], AuditApprovalDecision, matched[0].Approval.ID, "ok")
	if approved {
		if len(choiceIDs) > 1 {
			return m.send(ctx, message, "已按所选多个选项处理审批 "+publicReference("A", matched[0].Approval.ID)+"。")
		}
		return m.send(ctx, message, "已允许审批 "+publicReference("A", matched[0].Approval.ID)+" 一次。")
	}
	return m.send(ctx, message, "已拒绝审批 "+publicReference("A", matched[0].Approval.ID)+"。")
}

// approvalChoiceBySelector 把聊天端的选项序号或稳定 ChoiceID 解析成持久化
// 选项，并校验 /approve 与 /reject 不会反向选择另一类结果。
func approvalChoiceBySelector(approval agentruntime.Approval, selector string, approved bool) (agentruntime.ApprovalChoice, error) {
	selector = strings.TrimSpace(selector)
	choices := approval.Choices
	if len(choices) == 0 {
		choices = agentruntime.DefaultApprovalChoices()
	}
	var matched *agentruntime.ApprovalChoice
	if index, err := strconv.Atoi(selector); err == nil {
		if index < 1 || index > len(choices) {
			return agentruntime.ApprovalChoice{}, errors.New("审批选项编号无效，请按审批消息中的编号选择")
		}
		matched = &choices[index-1]
	} else {
		for index := range choices {
			if choices[index].ID == selector {
				matched = &choices[index]
				break
			}
		}
	}
	if matched == nil {
		return agentruntime.ApprovalChoice{}, errors.New("审批选项不存在，请使用选项编号或 ChoiceID")
	}
	if matched.Approved != approved {
		if approved {
			return agentruntime.ApprovalChoice{}, errors.New("/approve 只能选择允许执行的选项")
		}
		return agentruntime.ApprovalChoice{}, errors.New("/reject 只能选择拒绝执行的选项")
	}
	return *matched, nil
}

func publicReference(prefix, id string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(id)))
	return strings.ToUpper(prefix) + "-" + strings.ToUpper(hex.EncodeToString(digest[:4]))
}

func (m *Manager) recordRuntimeAdminAudit(ctx context.Context, bot Bot, message Message, command, action, target, result string) {
	m.recordAudit(ctx, CommandAudit{AdapterID: bot.ID, ChatID: message.ChatID, UserID: message.UserID, Command: command, Action: action, Target: target, Result: result})
}
