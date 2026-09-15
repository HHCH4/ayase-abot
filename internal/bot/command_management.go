package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// This file exposes the command catalog to the management surface. The WebUI
// reads exactly the same descriptors the chat dispatcher uses, so a command
// cannot appear enabled in the console while being refused in chat.

// ListCommands returns the command catalog with the per-bot policy applied.
func (m *Manager) ListCommands(ctx context.Context, botID string) ([]EffectiveCommand, error) {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return nil, fmt.Errorf("%w: 机器人 ID 不能为空", ErrInvalid)
	}
	if _, err := m.Get(botID); err != nil {
		return nil, err
	}
	return m.effectiveCommands(ctx, botID)
}

// UpdateCommandPolicy stores one per-bot permission or enable override and
// records it in the audit trail.
func (m *Manager) UpdateCommandPolicy(ctx context.Context, botID string, policy CommandPolicy) error {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return fmt.Errorf("%w: 机器人 ID 不能为空", ErrInvalid)
	}
	if _, err := m.Get(botID); err != nil {
		return err
	}
	descriptor, ok := m.descriptorByID(policy.CommandID)
	if !ok {
		return fmt.Errorf("%w: 未知指令 %q", ErrInvalid, policy.CommandID)
	}
	if !validPermission(policy.Permission) {
		return fmt.Errorf("%w: 指令权限级别无效", ErrInvalid)
	}
	repository, ok := m.commandPolicyRepository()
	if !ok {
		return errors.New("当前部署未启用指令策略存储")
	}
	if err := repository.SaveCommandPolicy(ctx, botID, CommandPolicy{
		CommandID: descriptor.ID, Permission: policy.Permission, Enabled: policy.Enabled,
	}); err != nil {
		return err
	}
	result := "disabled"
	if policy.Enabled {
		result = "enabled"
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: botID, Command: descriptor.ID, Action: AuditCommandPolicy,
		Target: permissionLabel(policy.Permission), Result: result,
	})
	return nil
}

// ListCommandAudits returns the newest audit entries for one bot.
func (m *Manager) ListCommandAudits(ctx context.Context, botID string, limit int) ([]CommandAudit, error) {
	repository, ok := m.commandAuditRepository()
	if !ok {
		return nil, nil
	}
	return repository.ListCommandAudits(ctx, strings.TrimSpace(botID), limit)
}

// ListGroupAdmins returns the delegated administrators of one group chat.
func (m *Manager) ListGroupAdmins(ctx context.Context, botID, chatID string) ([]string, error) {
	return m.groupAdmins(ctx, strings.TrimSpace(botID), strings.TrimSpace(chatID))
}

// AddGroupAdmin grants the delegated group administrator role from the
// management surface. Chat commands go through commandAdminAdd instead.
func (m *Manager) AddGroupAdmin(ctx context.Context, admin GroupAdmin) error {
	repository, ok := m.groupAdminRepository()
	if !ok {
		return errors.New("当前部署未启用群管理员授权")
	}
	admin.AdapterID = strings.TrimSpace(admin.AdapterID)
	admin.ChatID = strings.TrimSpace(admin.ChatID)
	admin.UserID = strings.TrimSpace(admin.UserID)
	if admin.AdapterID == "" || admin.ChatID == "" {
		return fmt.Errorf("%w: 适配器与聊天标识不能为空", ErrInvalid)
	}
	if err := validAdminTarget(admin.UserID); err != nil {
		return err
	}
	if err := repository.AddGroupAdmin(ctx, admin); err != nil {
		return err
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: admin.AdapterID, ChatID: admin.ChatID, UserID: admin.AddedBy,
		Command: "admin add", Action: AuditGroupAdminAdd, Target: admin.UserID, Result: "ok",
	})
	return nil
}

// RemoveGroupAdmin revokes the delegated group administrator role.
func (m *Manager) RemoveGroupAdmin(ctx context.Context, botID, chatID, userID string) error {
	repository, ok := m.groupAdminRepository()
	if !ok {
		return errors.New("当前部署未启用群管理员授权")
	}
	if err := repository.RemoveGroupAdmin(ctx, strings.TrimSpace(botID), strings.TrimSpace(chatID), strings.TrimSpace(userID)); err != nil {
		return err
	}
	m.recordAudit(ctx, CommandAudit{
		AdapterID: strings.TrimSpace(botID), ChatID: strings.TrimSpace(chatID),
		Command: "admin remove", Action: AuditGroupAdminRemove, Target: strings.TrimSpace(userID), Result: "ok",
	})
	return nil
}

// descriptorByID finds a descriptor by its stable id.
func (m *Manager) descriptorByID(id string) (CommandDescriptor, bool) {
	registry := m.commandRegistry()
	if registry == nil {
		return CommandDescriptor{}, false
	}
	id = strings.TrimSpace(id)
	for _, descriptor := range registry.List() {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return CommandDescriptor{}, false
}
