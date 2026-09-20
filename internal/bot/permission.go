package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file owns permission resolution: how a declared permission level maps to
// the current chat, how administrators are recognised, and how per-bot policy
// overrides are merged with the built-in defaults.

// CommandPolicy is one per-bot override of a command's permission or enabled
// state. A missing row means "use the descriptor default".
type CommandPolicy struct {
	CommandID  string     `json:"command_id"`
	Permission Permission `json:"permission"`
	Enabled    bool       `json:"enabled"`
}

// GroupAdmin is one delegated group administrator.
type GroupAdmin struct {
	AdapterID string    `json:"adapter_id"`
	ChatID    string    `json:"chat_id"`
	UserID    string    `json:"user_id"`
	AddedBy   string    `json:"added_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CommandAudit records a permission-affecting action. It never carries message
// bodies or credentials.
type CommandAudit struct {
	ID        string    `json:"id"`
	AdapterID string    `json:"adapter_id"`
	ChatID    string    `json:"chat_id,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	Command   string    `json:"command,omitempty"`
	Action    string    `json:"action"`
	Target    string    `json:"target,omitempty"`
	Result    string    `json:"result"`
	CreatedAt time.Time `json:"created_at"`
}

// Audit actions.
const (
	AuditGroupAdminAdd    = "group_admin_add"
	AuditGroupAdminRemove = "group_admin_remove"
	AuditGroupAdminLeave  = "group_admin_leave"
	AuditModelSwitch      = "model_switch"
	AuditPersonaSwitch    = "persona_switch"
	AuditWorkspaceBinding = "workspace_binding"
	AuditSourceName       = "source_name_update"
	AuditDashboardUpdate  = "dashboard_update"
	AuditCommandPolicy    = "command_policy"
	AuditCommandDenied    = "command_denied"
)

// The following repositories are optional extensions. A Bot repository that
// cannot persist them keeps the built-in defaults and no audit trail, instead of
// failing the whole manager.
type GroupAdminRepository interface {
	ListGroupAdmins(context.Context, string, string) ([]string, error)
	AddGroupAdmin(context.Context, GroupAdmin) error
	RemoveGroupAdmin(context.Context, string, string, string) error
}

type CommandPolicyRepository interface {
	ListCommandPolicies(context.Context, string) ([]CommandPolicy, error)
	SaveCommandPolicy(context.Context, string, CommandPolicy) error
}

type CommandAuditRepository interface {
	AppendCommandAudit(context.Context, CommandAudit) error
	ListCommandAudits(context.Context, string, int) ([]CommandAudit, error)
}

// EffectiveCommand is a descriptor after per-bot policy overrides.
type EffectiveCommand struct {
	CommandDescriptor
	Permission Permission `json:"effective_permission"`
	Enabled    bool       `json:"effective_enabled"`
	// Overridden reports whether a per-bot policy changed the default.
	Overridden bool `json:"overridden"`
}

// requiredPermissionInChat maps a declared permission to the current chat type.
// A private chat has no group to administer, so the group level is tightened
// rather than relaxed.
func requiredPermissionInChat(required Permission, isGroup bool) Permission {
	if !isGroup && required == PermissionGroupAdmin {
		return PermissionGlobalAdmin
	}
	return required
}

// isGroupChat reports whether the platform chat type is a group conversation.
func isGroupChatType(chatType string) bool {
	switch strings.ToLower(strings.TrimSpace(chatType)) {
	case "group", "supergroup", "channel":
		return true
	default:
		return false
	}
}

func (m *Manager) groupAdminRepository() (GroupAdminRepository, bool) {
	if m == nil || m.repository == nil {
		return nil, false
	}
	repository, ok := m.repository.(GroupAdminRepository)
	return repository, ok
}

func (m *Manager) commandPolicyRepository() (CommandPolicyRepository, bool) {
	if m == nil || m.repository == nil {
		return nil, false
	}
	repository, ok := m.repository.(CommandPolicyRepository)
	return repository, ok
}

func (m *Manager) commandAuditRepository() (CommandAuditRepository, bool) {
	if m == nil || m.repository == nil {
		return nil, false
	}
	repository, ok := m.repository.(CommandAuditRepository)
	return repository, ok
}

// commandRegistry returns the registry the manager dispatches through,
// falling back to the built-in catalog for embedders that never installed one.
func (m *Manager) commandRegistry() *CommandRegistry {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	registry := m.commands
	m.mu.RUnlock()
	return registry
}

func newAuditID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("audit-%d", time.Now().UnixNano())
	}
	return "audit-" + hex.EncodeToString(value)
}

// recordAudit appends an audit entry when the repository supports it.
// Audit failures never block the command that produced them.
func (m *Manager) recordAudit(ctx context.Context, entry CommandAudit) {
	repository, ok := m.commandAuditRepository()
	if !ok {
		return
	}
	if strings.TrimSpace(entry.ID) == "" {
		entry.ID = newAuditID()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_ = repository.AppendCommandAudit(context.WithoutCancel(ctx), entry)
}

// effectiveCommands resolves the whole catalog for one bot: descriptor defaults
// merged with per-bot overrides, in stable registry order.
func (m *Manager) effectiveCommands(ctx context.Context, botID string) ([]EffectiveCommand, error) {
	registry := m.commandRegistry()
	if registry == nil {
		return nil, nil
	}
	overrides := map[string]CommandPolicy{}
	if repository, ok := m.commandPolicyRepository(); ok && strings.TrimSpace(botID) != "" {
		policies, err := repository.ListCommandPolicies(ctx, strings.TrimSpace(botID))
		if err != nil {
			return nil, err
		}
		for _, policy := range policies {
			overrides[policy.CommandID] = policy
		}
	}
	descriptors := registry.List()
	result := make([]EffectiveCommand, 0, len(descriptors))
	for _, descriptor := range descriptors {
		effective := EffectiveCommand{CommandDescriptor: descriptor, Permission: descriptor.DefaultPermission, Enabled: descriptor.DefaultEnabled}
		if policy, ok := overrides[descriptor.ID]; ok {
			if validPermission(policy.Permission) {
				effective.Permission = policy.Permission
			}
			effective.Enabled = policy.Enabled
			effective.Overridden = true
		}
		result = append(result, effective)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// effectiveCommand resolves one command for one bot.
func (m *Manager) effectiveCommand(ctx context.Context, botID string, descriptor CommandDescriptor) (EffectiveCommand, error) {
	commands, err := m.effectiveCommands(ctx, botID)
	if err != nil {
		return EffectiveCommand{}, err
	}
	for _, command := range commands {
		if command.ID == descriptor.ID {
			return command, nil
		}
	}
	return EffectiveCommand{CommandDescriptor: descriptor, Permission: descriptor.DefaultPermission, Enabled: descriptor.DefaultEnabled}, nil
}

// groupAdmins lists the delegated administrators of one group chat.
func (m *Manager) groupAdmins(ctx context.Context, adapterID, chatID string) ([]string, error) {
	repository, ok := m.groupAdminRepository()
	if !ok {
		return nil, nil
	}
	return repository.ListGroupAdmins(ctx, strings.TrimSpace(adapterID), strings.TrimSpace(chatID))
}

// isGlobalAdmin reports whether the sender is configured as a global
// administrator of this bot. The list is only editable from the WebUI.
func isGlobalAdmin(bot Bot, userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	for _, candidate := range bot.AdminUserIDs {
		if strings.TrimSpace(candidate) == userID {
			return true
		}
	}
	return false
}

// commandAuthorization is the outcome of resolving one command for one sender.
type commandAuthorization struct {
	Command     EffectiveCommand
	Permission  Permission
	IsGroup     bool
	GlobalAdmin bool
	GroupAdmin  bool
	// Arg is the command argument after multi-word resolution.
	Arg string
}

// allows reports whether the sender satisfies the required permission.
func (a commandAuthorization) allows() bool {
	switch a.Permission {
	case PermissionEveryone:
		return true
	case PermissionGroupAdmin:
		// Private chats never produce this level; requiredPermissionInChat
		// already tightened it to the global level.
		return a.GroupAdmin || a.GlobalAdmin
	case PermissionGlobalAdmin:
		return a.GlobalAdmin
	default:
		return false
	}
}

// denyReason explains a refusal without disclosing the administrator list.
func (a commandAuthorization) denyReason() string {
	switch a.Permission {
	case PermissionGroupAdmin:
		return "该指令需要本群管理员权限。"
	case PermissionGlobalAdmin:
		if !a.IsGroup {
			return "该指令在私聊中需要全局管理员权限。"
		}
		return "该指令需要全局管理员权限。"
	default:
		return "该指令需要更高权限。"
	}
}

// authorizeCommand resolves the descriptor, the per-bot policy, the sender's
// administrator status and the effective permission for one message.
func (m *Manager) authorizeCommand(ctx context.Context, bot Bot, message Message, descriptor CommandDescriptor, arg string) (commandAuthorization, error) {
	command, err := m.effectiveCommand(ctx, bot.ID, descriptor)
	if err != nil {
		return commandAuthorization{}, err
	}
	isGroup := isGroupChatType(message.ChatType)
	authorization := commandAuthorization{
		Command:     command,
		IsGroup:     isGroup,
		GlobalAdmin: m.globalAdminForMessage(ctx, bot, message),
		Arg:         arg,
	}
	authorization.Permission = requiredPermissionInChat(command.Permission, isGroup)
	if authorization.Permission == PermissionGroupAdmin && isGroup {
		admins, adminErr := m.groupAdmins(ctx, bot.ID, message.ChatID)
		if adminErr != nil {
			return commandAuthorization{}, adminErr
		}
		for _, admin := range admins {
			if strings.TrimSpace(admin) == strings.TrimSpace(message.UserID) {
				authorization.GroupAdmin = true
				break
			}
		}
	}
	return authorization, nil
}

// helpTextFor renders /help from the catalog so it always reflects the current
// bot's enable state and effective permissions.
func helpTextFor(commands []EffectiveCommand, isGroup, globalAdmin, groupAdmin bool) string {
	var builder strings.Builder
	builder.WriteString("可用命令：")
	hiddenCount := 0
	for _, command := range commands {
		if !command.Enabled {
			continue
		}
		required := requiredPermissionInChat(command.Permission, isGroup)
		granted := required == PermissionEveryone ||
			(required == PermissionGroupAdmin && (groupAdmin || globalAdmin)) ||
			(required == PermissionGlobalAdmin && globalAdmin)
		if !granted {
			hiddenCount++
			continue
		}
		builder.WriteString("\n/")
		builder.WriteString(command.Name)
		builder.WriteString("  ")
		builder.WriteString(command.Description)
	}
	if hiddenCount > 0 {
		// 不向普通用户泄露受限命令名称，但明确说明帮助不是完整目录，避免误以为指令不存在。
		builder.WriteString(fmt.Sprintf("\n\n另有 %d 条管理员指令未显示；请在 WebUI 配置全局管理员 UID。", hiddenCount))
	}
	return builder.String()
}

// findDescriptorByName resolves a raw command name (as typed by a user) to a
// descriptor, including multi-word subcommands.
func findDescriptorByName(registry *CommandRegistry, name string) (CommandDescriptor, bool) {
	if registry == nil {
		return CommandDescriptor{}, false
	}
	return registry.Lookup(name)
}

// normalizeAdminTarget trims a user id supplied through a command argument.
func normalizeAdminTarget(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "@")
	return value
}

func validAdminTarget(value string) error {
	if value == "" {
		return fmt.Errorf("%w: 请提供用户 ID 或回复对方的消息", ErrInvalid)
	}
	if len(value) > 64 || strings.ContainsAny(value, " \t\r\n\x00") {
		return fmt.Errorf("%w: 用户 ID 无效", ErrInvalid)
	}
	return nil
}
