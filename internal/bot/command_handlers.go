package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// This file implements the chat command handlers. Dispatch goes through the
// registry, and every handler receives an already authorised authorization
// value, so permission checks cannot be forgotten in one branch.

// CommandRuntimeInfo lets commands display the configuration that is actually
// in effect for a chat. It is optional: without it, configuration commands
// report that the information is unavailable instead of guessing.
type CommandRuntimeInfo interface {
	CurrentModel(context.Context, string, string, string) (providerID, modelID string, err error)
	CurrentPersona(context.Context, string, string, string) (personaID, personaName string, err error)
	CurrentWorkspace(context.Context, string, string) (workspaceID, workspaceName string, err error)
	AvailableModels(context.Context) ([]CommandOption, error)
	AvailablePersonas(context.Context) ([]CommandOption, error)
	AvailableWorkspaces(context.Context) ([]CommandOption, error)
}

// CommandRuntimeAdmin applies configuration changes requested from a chat.
// It is separate from the read side so a deployment can allow inspection
// without allowing mutation.
type CommandRuntimeAdmin interface {
	SetModel(context.Context, string, string, string, string) error
	SetPersona(context.Context, string, string, string, string) error
	SetWorkspace(context.Context, string, string, string) error
}

// CommandOption is one selectable value for a configuration command.
type CommandOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (m *Manager) runtimeInfoProvider() (CommandRuntimeInfo, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	info := m.commandRuntimeInfo
	m.mu.RUnlock()
	return info, info != nil
}

func (m *Manager) runtimeAdminProvider() (CommandRuntimeAdmin, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	admin := m.commandRuntimeAdmin
	m.mu.RUnlock()
	return admin, admin != nil
}

// SetCommandRuntimeInfo installs the read side of configuration commands.
func (m *Manager) SetCommandRuntimeInfo(info CommandRuntimeInfo) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.commandRuntimeInfo = info
	m.mu.Unlock()
}

// SetCommandRuntimeAdmin installs the write side of configuration commands.
func (m *Manager) SetCommandRuntimeAdmin(admin CommandRuntimeAdmin) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.commandRuntimeAdmin = admin
	m.mu.Unlock()
}

// dispatchBotCommand routes an authorised command to its handler.
func (m *Manager) dispatchBotCommand(ctx context.Context, bot Bot, message Message, authorization commandAuthorization) error {
	switch authorization.Command.ID {
	case "help":
		return m.commandHelp(ctx, bot, message, authorization)
	case "id":
		return m.commandIdentity(ctx, message)
	case "status":
		return m.send(ctx, message, m.statusText(ctx, message))
	case "config":
		return m.commandConfig(ctx, bot, message, authorization)
	case "cancel":
		return m.cancelChatInvocation(ctx, message)
	case "new":
		return m.startNewConversation(ctx, message)
	case "model":
		return m.commandModel(ctx, bot, message, authorization)
	case "persona":
		return m.commandPersona(ctx, bot, message, authorization)
	case "workspace":
		return m.commandWorkspace(ctx, bot, message, authorization)
	case "admin list":
		return m.commandAdminList(ctx, bot, message)
	case "admin add":
		return m.commandAdminAdd(ctx, bot, message, authorization)
	case "admin remove":
		return m.commandAdminRemove(ctx, bot, message, authorization)
	case "admin leave":
		return m.commandAdminLeave(ctx, bot, message)
	case "approve", "reject":
		approved := authorization.Command.ID == "approve"
		ticket := strings.TrimSpace(authorization.Arg)
		if ticket == "" {
			return m.send(ctx, message, "请提供审批序号，例如 /approve 1。")
		}
		if handled, err := m.resolveApprovalChoice(ctx, message, ticket, approved); handled {
			return err
		}
		return m.send(ctx, message, "找不到属于当前聊天的待审批请求，请发送 /status 查看任务状态。")
	default:
		return m.send(ctx, message, "该指令尚未接入。")
	}
}

func (m *Manager) commandHelp(ctx context.Context, bot Bot, message Message, authorization commandAuthorization) error {
	commands, err := m.effectiveCommands(ctx, bot.ID)
	if err != nil {
		return err
	}
	isGroup := isGroupChatType(message.ChatType)
	text := helpTextFor(commands, isGroup, authorization.GlobalAdmin, authorization.GroupAdmin)
	return m.send(ctx, message, text)
}

func (m *Manager) commandIdentity(ctx context.Context, message Message) error {
	lines := []string{
		"当前标识：",
		"适配器：" + strings.TrimSpace(message.AdapterID),
		"聊天：" + strings.TrimSpace(message.ChatID),
		"用户：" + strings.TrimSpace(message.UserID),
		"类型：" + strings.TrimSpace(message.ChatType),
	}
	return m.send(ctx, message, strings.Join(lines, "\n"))
}

func (m *Manager) commandConfig(ctx context.Context, bot Bot, message Message, authorization commandAuthorization) error {
	userID, conversationID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return err
	}
	lines := []string{"当前会话配置：", "会话：" + conversationID}
	if info, ok := m.runtimeInfoProvider(); ok {
		if providerID, modelID, modelErr := info.CurrentModel(ctx, bot.ID, userID, conversationID); modelErr == nil {
			model := strings.TrimSpace(modelID)
			if model == "" {
				model = "（未设置）"
			}
			if provider := strings.TrimSpace(providerID); provider != "" {
				model = model + " · " + provider
			}
			lines = append(lines, "模型："+model)
		}
		if _, personaName, personaErr := info.CurrentPersona(ctx, bot.ID, userID, conversationID); personaErr == nil && strings.TrimSpace(personaName) != "" {
			lines = append(lines, "人格："+personaName)
		}
		// Workspace details describe server paths, so they stay behind the
		// global administrator boundary.
		if authorization.GlobalAdmin {
			if workspaceID, workspaceName, workspaceErr := info.CurrentWorkspace(ctx, userID, conversationID); workspaceErr == nil {
				workspace := strings.TrimSpace(workspaceName)
				if workspace == "" {
					workspace = strings.TrimSpace(workspaceID)
				}
				if workspace == "" {
					workspace = "（未绑定）"
				}
				lines = append(lines, "工作区："+workspace)
			}
		}
	} else {
		lines = append(lines, "（当前部署未提供配置查询）")
	}
	return m.send(ctx, message, strings.Join(lines, "\n"))
}

func (m *Manager) commandModel(ctx context.Context, bot Bot, message Message, authorization commandAuthorization) error {
	info, ok := m.runtimeInfoProvider()
	if !ok {
		return m.send(ctx, message, "当前部署未提供模型查询。")
	}
	userID, conversationID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return err
	}
	argument := strings.TrimSpace(authorization.Arg)
	if argument == "" {
		_, modelID, modelErr := info.CurrentModel(ctx, bot.ID, userID, conversationID)
		if modelErr != nil {
			return err
		}
		options, optionErr := info.AvailableModels(ctx)
		if optionErr != nil {
			return optionErr
		}
		lines := []string{"当前模型：" + orPlaceholder(modelID)}
		if len(options) > 0 {
			lines = append(lines, "", "可用模型：")
			for _, option := range options {
				lines = append(lines, "· "+option.ID)
			}
			lines = append(lines, "", "发送 /model <模型ID> 切换当前会话。")
		}
		return m.send(ctx, message, strings.Join(lines, "\n"))
	}
	admin, ok := m.runtimeAdminProvider()
	if !ok {
		return m.send(ctx, message, "当前部署不允许在聊天中切换模型。")
	}
	options, err := info.AvailableModels(ctx)
	if err != nil {
		return err
	}
	selected, found := matchCommandOption(options, argument)
	if !found {
		return m.send(ctx, message, "没有找到模型 "+argument+"。发送 /model 查看可用模型。")
	}
	if err := admin.SetModel(ctx, bot.ID, userID, conversationID, selected.ID); err != nil {
		return m.send(ctx, message, "切换模型失败："+trimError(err))
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: bot.ID, ChatID: message.ChatID, UserID: message.UserID, Command: "model",
		Action: AuditModelSwitch, Target: selected.ID, Result: "ok",
	})
	return m.send(ctx, message, "已切换当前会话模型为 "+selected.ID+"。")
}

func (m *Manager) commandPersona(ctx context.Context, bot Bot, message Message, authorization commandAuthorization) error {
	info, ok := m.runtimeInfoProvider()
	if !ok {
		return m.send(ctx, message, "当前部署未提供人格查询。")
	}
	userID, conversationID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return err
	}
	argument := strings.TrimSpace(authorization.Arg)
	if argument == "" {
		_, name, personaErr := info.CurrentPersona(ctx, bot.ID, userID, conversationID)
		if personaErr != nil {
			return personaErr
		}
		options, optionErr := info.AvailablePersonas(ctx)
		if optionErr != nil {
			return optionErr
		}
		lines := []string{"当前人格：" + orPlaceholder(name)}
		if len(options) > 0 {
			lines = append(lines, "", "可用人格：")
			for _, option := range options {
				lines = append(lines, "· "+option.Name)
			}
			lines = append(lines, "", "发送 /persona <名称> 切换当前会话。")
		}
		return m.send(ctx, message, strings.Join(lines, "\n"))
	}
	admin, ok := m.runtimeAdminProvider()
	if !ok {
		return m.send(ctx, message, "当前部署不允许在聊天中切换人格。")
	}
	options, err := info.AvailablePersonas(ctx)
	if err != nil {
		return err
	}
	selected, found := matchCommandOption(options, argument)
	if !found {
		return m.send(ctx, message, "没有找到人格 "+argument+"。发送 /persona 查看可用人格。")
	}
	if err := admin.SetPersona(ctx, bot.ID, userID, conversationID, selected.ID); err != nil {
		return m.send(ctx, message, "切换人格失败："+trimError(err))
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: bot.ID, ChatID: message.ChatID, UserID: message.UserID, Command: "persona",
		Action: AuditPersonaSwitch, Target: selected.ID, Result: "ok",
	})
	return m.send(ctx, message, "已切换当前会话人格为 "+selected.Name+"。")
}

func (m *Manager) commandWorkspace(ctx context.Context, bot Bot, message Message, authorization commandAuthorization) error {
	info, ok := m.runtimeInfoProvider()
	if !ok {
		return m.send(ctx, message, "当前部署未提供工作区查询。")
	}
	userID, conversationID, err := m.conversationForMessage(ctx, message)
	if err != nil {
		return err
	}
	argument := strings.TrimSpace(authorization.Arg)
	if argument == "" {
		workspaceID, name, workspaceErr := info.CurrentWorkspace(ctx, userID, conversationID)
		if workspaceErr != nil {
			return workspaceErr
		}
		current := strings.TrimSpace(name)
		if current == "" {
			current = strings.TrimSpace(workspaceID)
		}
		options, optionErr := info.AvailableWorkspaces(ctx)
		if optionErr != nil {
			return optionErr
		}
		lines := []string{"当前工作区：" + orPlaceholder(current)}
		if len(options) > 0 {
			lines = append(lines, "", "可用工作区：")
			for _, option := range options {
				lines = append(lines, "· "+option.Name)
			}
			lines = append(lines, "", "发送 /workspace <名称> 绑定当前会话。")
		}
		return m.send(ctx, message, strings.Join(lines, "\n"))
	}
	admin, ok := m.runtimeAdminProvider()
	if !ok {
		return m.send(ctx, message, "当前部署不允许在聊天中绑定工作区。")
	}
	options, err := info.AvailableWorkspaces(ctx)
	if err != nil {
		return err
	}
	selected, found := matchCommandOption(options, argument)
	if !found {
		return m.send(ctx, message, "没有找到工作区 "+argument+"。发送 /workspace 查看可用工作区。")
	}
	if err := admin.SetWorkspace(ctx, userID, conversationID, selected.ID); err != nil {
		return m.send(ctx, message, "绑定工作区失败："+trimError(err))
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: bot.ID, ChatID: message.ChatID, UserID: message.UserID, Command: "workspace",
		Action: AuditWorkspaceBinding, Target: selected.ID, Result: "ok",
	})
	return m.send(ctx, message, "已把当前会话绑定到工作区 "+selected.Name+"。")
}

func (m *Manager) commandAdminList(ctx context.Context, bot Bot, message Message) error {
	admins, err := m.groupAdmins(ctx, bot.ID, message.ChatID)
	if err != nil {
		return err
	}
	lines := []string{"本群管理员："}
	for _, admin := range admins {
		lines = append(lines, "· "+admin)
	}
	if len(admins) == 0 {
		lines = append(lines, "（暂无）")
	}
	if len(bot.AdminUserIDs) > 0 {
		lines = append(lines, "", "全局管理员（WebUI 配置）：")
		sorted := append([]string(nil), bot.AdminUserIDs...)
		sort.Strings(sorted)
		for _, admin := range sorted {
			lines = append(lines, "· "+admin)
		}
	}
	lines = append(lines, "", "添加：/admin add <用户ID> 或回复对方消息后发送 /admin add。")
	return m.send(ctx, message, strings.Join(lines, "\n"))
}

func (m *Manager) commandAdminAdd(ctx context.Context, bot Bot, message Message, authorization commandAuthorization) error {
	repository, ok := m.groupAdminRepository()
	if !ok {
		return m.send(ctx, message, "当前部署未启用群管理员授权。")
	}
	if !isGroupChatType(message.ChatType) {
		return m.send(ctx, message, "群管理员只能在群聊中授权。")
	}
	target := adminTargetFromMessage(message, authorization.Arg)
	if err := validAdminTarget(target); err != nil {
		return m.send(ctx, message, "请提供用户 ID，或回复对方的消息后再发送 /admin add。")
	}
	if isGlobalAdmin(bot, target) {
		return m.send(ctx, message, "该用户已经是全局管理员，无需重复授权。")
	}
	if err := repository.AddGroupAdmin(ctx, GroupAdmin{
		AdapterID: bot.ID, ChatID: message.ChatID, UserID: target, AddedBy: message.UserID,
	}); err != nil {
		return m.send(ctx, message, "添加群管理员失败："+trimError(err))
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: bot.ID, ChatID: message.ChatID, UserID: message.UserID, Command: "admin add",
		Action: AuditGroupAdminAdd, Target: target, Result: "ok",
	})
	return m.send(ctx, message, "已把 "+target+" 设为本群管理员。")
}

func (m *Manager) commandAdminRemove(ctx context.Context, bot Bot, message Message, authorization commandAuthorization) error {
	repository, ok := m.groupAdminRepository()
	if !ok {
		return m.send(ctx, message, "当前部署未启用群管理员授权。")
	}
	if !isGroupChatType(message.ChatType) {
		return m.send(ctx, message, "群管理员只能在群聊中移除。")
	}
	target := adminTargetFromMessage(message, authorization.Arg)
	if err := validAdminTarget(target); err != nil {
		return m.send(ctx, message, "请提供用户 ID，或回复对方的消息后再发送 /admin remove。")
	}
	if err := repository.RemoveGroupAdmin(ctx, bot.ID, message.ChatID, target); err != nil {
		return m.send(ctx, message, "移除失败：该用户不是本群管理员。")
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: bot.ID, ChatID: message.ChatID, UserID: message.UserID, Command: "admin remove",
		Action: AuditGroupAdminRemove, Target: target, Result: "ok",
	})
	return m.send(ctx, message, "已移除本群管理员 "+target+"。")
}

func (m *Manager) commandAdminLeave(ctx context.Context, bot Bot, message Message) error {
	repository, ok := m.groupAdminRepository()
	if !ok {
		return m.send(ctx, message, "当前部署未启用群管理员授权。")
	}
	if !isGroupChatType(message.ChatType) {
		return m.send(ctx, message, "该指令只能在群聊中使用。")
	}
	userID := strings.TrimSpace(message.UserID)
	if isGlobalAdmin(bot, userID) {
		return m.send(ctx, message, "全局管理员不能退出，请在 WebUI 中调整。")
	}
	if err := repository.RemoveGroupAdmin(ctx, bot.ID, message.ChatID, userID); err != nil {
		return m.send(ctx, message, "你不是本群管理员。")
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: bot.ID, ChatID: message.ChatID, UserID: userID, Command: "admin leave",
		Action: AuditGroupAdminLeave, Target: userID, Result: "ok",
	})
	return m.send(ctx, message, "你已退出本群管理员。")
}

// adminTargetFromMessage resolves the administrator target from an explicit
// argument, a replied-to message, or a platform mention, in that order.
func adminTargetFromMessage(message Message, argument string) string {
	if target := normalizeAdminTarget(argument); target != "" {
		return target
	}
	if target := normalizeAdminTarget(message.ReplyToUserID); target != "" {
		return target
	}
	for _, mention := range message.Mentions {
		if target := normalizeAdminTarget(mention); target != "" {
			return target
		}
	}
	return ""
}

// matchCommandOption resolves an option by exact ID, exact name, or unique
// case-insensitive name prefix.
func matchCommandOption(options []CommandOption, value string) (CommandOption, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return CommandOption{}, false
	}
	lower := strings.ToLower(value)
	for _, option := range options {
		if option.ID == value {
			return option, true
		}
	}
	for _, option := range options {
		if strings.ToLower(option.Name) == lower {
			return option, true
		}
	}
	var matched CommandOption
	count := 0
	for _, option := range options {
		if strings.HasPrefix(strings.ToLower(option.Name), lower) {
			matched = option
			count++
		}
	}
	if count == 1 {
		return matched, true
	}
	return CommandOption{}, false
}

func orPlaceholder(value string) string {
	if strings.TrimSpace(value) == "" {
		return "（未设置）"
	}
	return value
}

// unknownCommandText keeps an unrecognised command from reaching the model,
// which would otherwise improvise an answer to something that looks like an
// instruction. It shows the same list /help would, so a disabled or
// unauthorised command is visibly absent rather than silently ignored.
func (m *Manager) unknownCommandText(ctx context.Context, bot Bot, message Message) string {
	commands, err := m.effectiveCommands(ctx, bot.ID)
	if err != nil {
		return "未知命令。发送 /help 查看可用指令。"
	}
	groupAdmin := false
	if isGroupChatType(message.ChatType) {
		if admins, adminErr := m.groupAdmins(ctx, bot.ID, message.ChatID); adminErr == nil {
			for _, admin := range admins {
				if strings.TrimSpace(admin) == strings.TrimSpace(message.UserID) {
					groupAdmin = true
					break
				}
			}
		}
	}
	help := helpTextFor(commands, isGroupChatType(message.ChatType), isGlobalAdmin(bot, message.UserID), groupAdmin)
	return "未知命令。\n\n" + help
}

func formatCommandList(commands []EffectiveCommand, isGroup bool) string {
	lines := make([]string, 0, len(commands))
	for _, command := range commands {
		permission := requiredPermissionInChat(command.Permission, isGroup)
		state := "已启用"
		if !command.Enabled {
			state = "已停用"
		}
		lines = append(lines, fmt.Sprintf("/%s · %s · %s · %s", command.Name, command.Description, permissionLabel(permission), state))
	}
	return strings.Join(lines, "\n")
}

func permissionLabel(permission Permission) string {
	switch permission {
	case PermissionEveryone:
		return "所有人"
	case PermissionGroupAdmin:
		return "群管理员"
	case PermissionGlobalAdmin:
		return "全局管理员"
	default:
		return string(permission)
	}
}
