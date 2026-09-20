package bot

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// This file owns the chat command catalog: descriptors, the registry that
// dispatches them, and the built-in entries. The registry is deliberately
// metadata-first so the WebUI management page and /help read the same source,
// and so a future plugin source can register its own commands without changing
// the data model.

// Permission is the level a chat participant must hold to run a command.
type Permission string

const (
	PermissionEveryone    Permission = "everyone"
	PermissionGroupAdmin  Permission = "group_admin"
	PermissionGlobalAdmin Permission = "global_admin"
)

func validPermission(value Permission) bool {
	switch value {
	case PermissionEveryone, PermissionGroupAdmin, PermissionGlobalAdmin:
		return true
	default:
		return false
	}
}

// PermissionRank orders levels so a policy override can only be compared, never
// silently widened by an unknown value.
func PermissionRank(value Permission) int {
	switch value {
	case PermissionEveryone:
		return 0
	case PermissionGroupAdmin:
		return 1
	case PermissionGlobalAdmin:
		return 2
	default:
		return 3
	}
}

// CommandScope describes what a command can affect.
type CommandScope string

const (
	ScopeChat    CommandScope = "chat"
	ScopeSession CommandScope = "session"
	ScopeBot     CommandScope = "bot"
)

func validScope(value CommandScope) bool {
	switch value {
	case ScopeChat, ScopeSession, ScopeBot:
		return true
	default:
		return false
	}
}

// Command sources. A plugin source is "plugin:<pluginID>"; the shape exists now
// so registering plugin commands later needs no schema change.
const CommandSourceBuiltin = "builtin"

// CommandSourcePluginPrefix marks commands contributed by a plugin.
const CommandSourcePluginPrefix = "plugin:"

// Categories group commands in the management page and /help.
const (
	CategoryInfo    = "info"
	CategorySession = "session"
	CategoryTask    = "task"
	CategoryConfig  = "config"
	CategoryAdmin   = "admin"
)

func validCategory(value string) bool {
	switch value {
	case CategoryInfo, CategorySession, CategoryTask, CategoryConfig, CategoryAdmin:
		return true
	default:
		return false
	}
}

// CommandDescriptor is the immutable metadata of one command.
type CommandDescriptor struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	Aliases           []string     `json:"aliases,omitempty"`
	Source            string       `json:"source"`
	SourceName        string       `json:"source_name"`
	Category          string       `json:"category"`
	Description       string       `json:"description"`
	DefaultPermission Permission   `json:"default_permission"`
	Scope             CommandScope `json:"scope"`
	DefaultEnabled    bool         `json:"default_enabled"`
}

// Plugin reports whether the descriptor came from an external source.
func (d CommandDescriptor) Plugin() bool {
	return strings.HasPrefix(d.Source, CommandSourcePluginPrefix)
}

func (d CommandDescriptor) validate() error {
	name := strings.TrimSpace(d.Name)
	if name == "" || strings.ContainsAny(name, "/\r\n\x00") {
		return fmt.Errorf("%w: 指令名无效", ErrInvalid)
	}
	if strings.TrimSpace(d.ID) == "" {
		return fmt.Errorf("%w: 指令 %q 缺少 ID", ErrInvalid, name)
	}
	if !validPermission(d.DefaultPermission) {
		return fmt.Errorf("%w: 指令 %q 的默认权限无效", ErrInvalid, name)
	}
	if !validScope(d.Scope) {
		return fmt.Errorf("%w: 指令 %q 的作用域无效", ErrInvalid, name)
	}
	if !validCategory(d.Category) {
		return fmt.Errorf("%w: 指令 %q 的分类无效", ErrInvalid, name)
	}
	if strings.TrimSpace(d.Source) == "" {
		return fmt.Errorf("%w: 指令 %q 缺少来源", ErrInvalid, name)
	}
	return nil
}

// CommandRegistry resolves command names to descriptors. Registration conflicts
// fail loudly instead of overwriting: a plugin must never be able to shadow a
// built-in command by registering later.
type CommandRegistry struct {
	mu       sync.RWMutex
	byName   map[string]CommandDescriptor
	ordered  []CommandDescriptor
	bySource map[string][]string
}

func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{byName: make(map[string]CommandDescriptor), bySource: make(map[string][]string)}
}

// Register adds one built-in descriptor.
func (r *CommandRegistry) Register(descriptor CommandDescriptor) error {
	return r.register(CommandDescriptor{
		Source:     CommandSourceBuiltin,
		SourceName: "内置",
	}, descriptor)
}

// RegisterFrom adds descriptors contributed by an external source, such as a
// plugin. Plugin commands are forced to the strictest default permission: a
// plugin must not be able to grant itself "everyone" access. The declared value
// is kept for display and an administrator may widen it explicitly.
func (r *CommandRegistry) RegisterFrom(source, sourceName string, descriptors []CommandDescriptor) error {
	source = strings.TrimSpace(source)
	if source == "" || source == CommandSourceBuiltin {
		return fmt.Errorf("%w: 插件来源无效", ErrInvalid)
	}
	if !strings.HasPrefix(source, CommandSourcePluginPrefix) {
		return fmt.Errorf("%w: 插件来源必须以 %q 开头", ErrInvalid, CommandSourcePluginPrefix)
	}
	if strings.TrimSpace(sourceName) == "" {
		sourceName = source
	}
	for _, descriptor := range descriptors {
		descriptor.DefaultPermission = PermissionGlobalAdmin
		if err := r.register(CommandDescriptor{Source: source, SourceName: sourceName}, descriptor); err != nil {
			return err
		}
	}
	return nil
}

func (r *CommandRegistry) register(base CommandDescriptor, descriptor CommandDescriptor) error {
	if r == nil {
		return fmt.Errorf("%w: 指令注册表未初始化", ErrInvalid)
	}
	descriptor.Source = base.Source
	descriptor.SourceName = base.SourceName
	if err := descriptor.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	names := append([]string{descriptor.Name}, descriptor.Aliases...)
	for _, name := range names {
		key := commandKey(name)
		if key == "" {
			return fmt.Errorf("%w: 指令别名无效", ErrInvalid)
		}
		if existing, ok := r.byName[key]; ok {
			return fmt.Errorf("%w: 指令 %q 已被 %s 注册", ErrInvalid, name, existing.SourceName)
		}
	}
	for _, name := range names {
		r.byName[commandKey(name)] = descriptor
	}
	r.ordered = append(r.ordered, descriptor)
	r.bySource[descriptor.Source] = append(r.bySource[descriptor.Source], descriptor.ID)
	return nil
}

// UnregisterSource removes every command contributed by one source. It is the
// hook a plugin lifecycle uses when a plugin is disabled or uninstalled.
func (r *CommandRegistry) UnregisterSource(source string) error {
	source = strings.TrimSpace(source)
	if r == nil || source == "" {
		return fmt.Errorf("%w: 指令来源无效", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.bySource[source]; !ok {
		return nil
	}
	kept := make([]CommandDescriptor, 0, len(r.ordered))
	for _, descriptor := range r.ordered {
		if descriptor.Source != source {
			kept = append(kept, descriptor)
			continue
		}
		for _, name := range append([]string{descriptor.Name}, descriptor.Aliases...) {
			delete(r.byName, commandKey(name))
		}
	}
	r.ordered = kept
	delete(r.bySource, source)
	return nil
}

// List returns descriptors in stable order: built-in first, then by name.
func (r *CommandRegistry) List() []CommandDescriptor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := append([]CommandDescriptor(nil), r.ordered...)
	sort.SliceStable(result, func(i, j int) bool {
		leftPlugin := result[i].Plugin()
		rightPlugin := result[j].Plugin()
		if leftPlugin != rightPlugin {
			return !leftPlugin
		}
		return result[i].Name < result[j].Name
	})
	return result
}

// Lookup resolves a single command name.
func (r *CommandRegistry) Lookup(name string) (CommandDescriptor, bool) {
	if r == nil {
		return CommandDescriptor{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	descriptor, ok := r.byName[commandKey(name)]
	return descriptor, ok
}

// Resolve maps a parsed command to its descriptor. Multi-word commands such as
// "admin add" are matched before the single-word fallback, so each subcommand
// keeps its own permission level and enable state.
func (r *CommandRegistry) Resolve(name, arg string) (CommandDescriptor, string, bool) {
	if descriptor, ok := r.Lookup(name); ok {
		return descriptor, arg, true
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return CommandDescriptor{}, "", false
	}
	if descriptor, ok := r.Lookup(name + " " + strings.ToLower(fields[0])); ok {
		return descriptor, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0])), true
	}
	return CommandDescriptor{}, "", false
}

func commandKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(name)), " "))
}

// builtinCommandDescriptors is the built-in catalog described in
// docs/runtime/AGENT_BOT_COMMANDS.md.
func builtinCommandDescriptors() []CommandDescriptor {
	return []CommandDescriptor{
		{ID: "help", Name: "help", Category: CategoryInfo, Description: "列出可用指令", DefaultPermission: PermissionEveryone, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "id", Name: "id", Category: CategoryInfo, Description: "显示当前聊天与用户标识", DefaultPermission: PermissionEveryone, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "sid", Name: "sid", Category: CategoryInfo, Description: "显示当前消息来源与会话标识", DefaultPermission: PermissionEveryone, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "name", Name: "name", Category: CategoryInfo, Description: "设置当前消息来源的显示名称", DefaultPermission: PermissionGlobalAdmin, Scope: ScopeBot, DefaultEnabled: true},
		{ID: "status", Name: "status", Category: CategoryInfo, Description: "查看当前会话与任务状态", DefaultPermission: PermissionEveryone, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "config", Name: "config", Category: CategoryInfo, Description: "查看当前生效配置", DefaultPermission: PermissionGroupAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "cancel", Name: "cancel", Category: CategoryTask, Description: "终止本会话进行中的任务", DefaultPermission: PermissionGroupAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "stop", Name: "stop", Category: CategoryTask, Description: "停止当前会话进行中的任务", DefaultPermission: PermissionEveryone, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "new", Name: "new", Category: CategorySession, Description: "归档当前会话并新建", DefaultPermission: PermissionGroupAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "reset", Name: "reset", Category: CategorySession, Description: "归档当前会话并新建", DefaultPermission: PermissionGroupAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "stats", Name: "stats", Category: CategoryInfo, Description: "查看当前会话 Token 用量", DefaultPermission: PermissionEveryone, Scope: ScopeSession, DefaultEnabled: true},
		{ID: "dashboard_update", Name: "dashboard_update", Category: CategoryAdmin, Description: "检查并更新内嵌管理台资源", DefaultPermission: PermissionGlobalAdmin, Scope: ScopeBot, DefaultEnabled: true},
		{ID: "model", Name: "model", Category: CategoryConfig, Description: "查看或切换当前会话模型", DefaultPermission: PermissionGroupAdmin, Scope: ScopeSession, DefaultEnabled: true},
		{ID: "persona", Name: "persona", Category: CategoryConfig, Description: "查看或切换当前会话人格", DefaultPermission: PermissionGroupAdmin, Scope: ScopeSession, DefaultEnabled: true},
		{ID: "workspace", Name: "workspace", Category: CategoryConfig, Description: "查看或绑定工作区", DefaultPermission: PermissionGlobalAdmin, Scope: ScopeBot, DefaultEnabled: true},
		{ID: "admin list", Name: "admin list", Category: CategoryAdmin, Description: "列出本群管理员", DefaultPermission: PermissionGlobalAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "admin add", Name: "admin add", Category: CategoryAdmin, Description: "添加群管理员", DefaultPermission: PermissionGlobalAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "admin remove", Name: "admin remove", Category: CategoryAdmin, Description: "移除群管理员", DefaultPermission: PermissionGlobalAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "admin leave", Name: "admin leave", Category: CategoryAdmin, Description: "退出本群管理员", DefaultPermission: PermissionGroupAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "approve", Name: "approve", Category: CategoryTask, Description: "兼容入口：批准待审批操作（正常流程可直接回复批准）", DefaultPermission: PermissionGroupAdmin, Scope: ScopeChat, DefaultEnabled: true},
		{ID: "reject", Name: "reject", Category: CategoryTask, Description: "兼容入口：拒绝待审批操作（正常流程可直接回复拒绝）", DefaultPermission: PermissionGroupAdmin, Scope: ScopeChat, DefaultEnabled: true},
	}
}

// NewBuiltinCommandRegistry builds the registry with the built-in catalog.
func NewBuiltinCommandRegistry() (*CommandRegistry, error) {
	registry := NewCommandRegistry()
	for _, descriptor := range builtinCommandDescriptors() {
		if err := registry.Register(descriptor); err != nil {
			return nil, err
		}
	}
	return registry, nil
}
