// Package config 提供 Schema 驱动的配置文件、修订、绑定和系统设置服务。
package config

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const SchemaVersion = 1

// BuiltinAIExecutionMode 是当前唯一允许的 AI 执行方式；外部编排服务不进入本项目配置模型。
const BuiltinAIExecutionMode = "builtin"

var (
	ErrNotFound       = errors.New("配置文件不存在")
	ErrInvalidRequest = errors.New("配置请求无效")
	ErrConflict       = errors.New("配置名称或 ID 已存在")
	ErrDefaultProfile = errors.New("默认配置文件不能删除")
	ErrProfileInUse   = errors.New("配置文件仍被绑定，不能删除")
	ErrDefaultPersona = errors.New("默认人格不能删除")
	ErrPersonaInUse   = errors.New("人格仍被绑定，不能删除")
)

type BindingScope string

const (
	BindingBot          BindingScope = "bot"
	BindingConversation BindingScope = "conversation"
)

// Values 是配置文件的 JSON 对象。键采用稳定的点号路径，便于后续扩展嵌套配置。
type Values map[string]any

// Profile 是当前配置修订的公开领域对象；Values 中不允许放入未声明的字段。
type Profile struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Revision  int       `json:"revision"`
	Values    Values    `json:"values"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Revision 是审计用的不可变配置快照，保存配置时递增编号且不覆盖历史记录。
type Revision struct {
	ProfileID string    `json:"profile_id"`
	Number    int       `json:"revision"`
	Values    Values    `json:"values"`
	CreatedAt time.Time `json:"created_at"`
}

// SchemaOption 是下拉选项；动态供应商和模型由 OptionSource 标注，避免把运行时目录硬编码到 Schema。
type SchemaOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field 描述 WebUI 表单和后端校验共用的配置元数据。
type Field struct {
	Key             string         `json:"key"`
	Group           string         `json:"group"`
	Label           string         `json:"label"`
	Type            string         `json:"type"`
	Default         any            `json:"default"`
	Required        bool           `json:"required"`
	Min             *float64       `json:"min,omitempty"`
	Max             *float64       `json:"max,omitempty"`
	Options         []SchemaOption `json:"options,omitempty"`
	OptionSource    string         `json:"option_source,omitempty"`
	DisplayIf       map[string]any `json:"display_if,omitempty"`
	Secret          bool           `json:"secret"`
	RestartRequired bool           `json:"restart_required"`
	Help            string         `json:"help,omitempty"`
}

type Schema struct {
	Version int     `json:"version"`
	Fields  []Field `json:"fields"`
}

// RevisionRepository 是 SQLite 仓储提供的可选扩展；保留可选接口避免影响外部内存仓储实现。
type RevisionRepository interface {
	ListRevisions(context.Context, string) ([]Revision, error)
}

// SubAgentSettings 是统一的通用子 Agent 配置。它只描述模型、预算和允许的只读
// 工具，不描述职责类型，也不包含子 Agent 总超时；总生命周期由父 Invocation 管理。
type SubAgentSettings struct {
	ProviderID        string   `json:"provider_id,omitempty"`
	ModelID           string   `json:"model_id,omitempty"`
	ReasoningEffort   string   `json:"reasoning_effort,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	MaxOutputTokens   int      `json:"max_output_tokens,omitempty"`
	MaxConcurrency    int      `json:"max_concurrency,omitempty"`
	InputBudgetBytes  int      `json:"input_budget_bytes,omitempty"`
	OutputBudgetBytes int      `json:"output_budget_bytes,omitempty"`
	AllowedTools      []string `json:"allowed_tools,omitempty"`
}

// SystemSettings 只包含当前已有真实运行时效果的系统设置。
type SystemSettings struct {
	LogLevel              string `json:"log_level"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
	// ArtifactQuotaBytes 是本地内容寻址存储去重后的占用上限。单独出现的 0
	// 表示用户显式关闭配额；配额和未完成上传时长同时为 0 才视为旧数据或
	// 全新记录，此时由 normalizeSystemSettings 补上保守默认值。
	ArtifactQuotaBytes         int64 `json:"artifact_quota_bytes"`
	ArtifactStaleUploadSeconds int   `json:"artifact_stale_upload_seconds"`
	// ArtifactInputRetentionSeconds 控制图片、音频和文档等输入附件的定时
	// 清理周期；会话物理删除时仍会立即级联清理，不受该周期影响。
	ArtifactInputRetentionSeconds  int `json:"artifact_input_retention_seconds"`
	WebSearchDailyCallLimit        int `json:"web_search_daily_call_limit"`
	WebSearchMaxCallsPerInvocation int `json:"web_search_max_calls_per_invocation"`
	WebSearchAlertPercent          int `json:"web_search_alert_percent"`
	// Modal fallback is a system-wide safety valve. Empty provider means the
	// primary provider; empty per-modality model means that modality is not
	// downgraded through another model.
	ModalFallbackEnabled     bool   `json:"modal_fallback_enabled"`
	ModalFallbackProviderID  string `json:"modal_fallback_provider_id"`
	ModalFallbackVisionModel string `json:"modal_fallback_vision_model"`
	ModalFallbackAudioModel  string `json:"modal_fallback_audio_model"`
	// SubAgentEnabled 是系统级默认开关；新系统设置未填写时由规范化过程启用。
	SubAgentEnabled *bool `json:"subagent_enabled,omitempty"`
	// SubAgent 是唯一的通用子 Agent 配置，不按任务职责区分类型。
	SubAgent SubAgentSettings `json:"subagent"`
}

// IsSubAgentEnabled 返回显式配置的系统默认开关状态。
func (settings SystemSettings) IsSubAgentEnabled() bool {
	return settings.SubAgentEnabled != nil && *settings.SubAgentEnabled
}

// Artifact 存储约束的默认值与边界。这些数值同时用于 Schema 校验和运行时维护
// 策略，避免配置层与存储层各自维护一份默认值。
const (
	DefaultArtifactQuotaBytes            int64 = 4 << 30
	MaxArtifactQuotaBytes                int64 = 1 << 40
	DefaultArtifactStaleUploadSeconds          = 24 * 60 * 60
	MinArtifactStaleUploadSeconds              = 300
	MaxArtifactStaleUploadSeconds              = 30 * 24 * 60 * 60
	DefaultArtifactInputRetentionSeconds       = 7 * 24 * 60 * 60
	MinArtifactInputRetentionSeconds           = 60 * 60
	MaxArtifactInputRetentionSeconds           = 365 * 24 * 60 * 60
)

// SystemSchema 描述系统设置的校验范围和是否需要重启，供 WebUI 与外部管理客户端复用。
func SystemSchema() Schema {
	return Schema{Version: SchemaVersion, Fields: []Field{
		{Key: "log_level", Group: "system", Label: "日志等级", Type: "select", Default: "info", Options: []SchemaOption{
			{Value: "debug", Label: "debug"}, {Value: "info", Label: "info"}, {Value: "warn", Label: "warn"}, {Value: "error", Label: "error"},
		}, Help: "保存后立即影响进程日志输出。"},
		{Key: "request_timeout_seconds", Group: "system", Label: "全局请求超时（秒）", Type: "integer", Default: 300, Min: floatPtr(30), Max: floatPtr(3600), Help: "限制 WebUI、API 和机器人消息请求的最长运行时间。"},
		{Key: "artifact_quota_bytes", Group: "system", Label: "Artifact 存储配额（字节）", Type: "integer", Default: DefaultArtifactQuotaBytes, Min: floatPtr(0), Max: floatPtr(float64(MaxArtifactQuotaBytes)), Help: "本地内容寻址存储去重后的总占用上限，0 表示不限制。超过上限时新的上传会被拒绝，已有内容不受影响。"},
		{Key: "artifact_stale_upload_seconds", Group: "system", Label: "未完成上传保留时长（秒）", Type: "integer", Default: DefaultArtifactStaleUploadSeconds, Min: floatPtr(MinArtifactStaleUploadSeconds), Max: floatPtr(MaxArtifactStaleUploadSeconds), Help: "超过该时长仍未完成的 Artifact 上传会被标记为失败，遗留的临时文件一并回收。"},
		{Key: "artifact_input_retention_seconds", Group: "system", Label: "输入附件保留时长（秒）", Type: "integer", Default: DefaultArtifactInputRetentionSeconds, Min: floatPtr(MinArtifactInputRetentionSeconds), Max: floatPtr(MaxArtifactInputRetentionSeconds), Help: "图片、音频、PDF、Word、Excel 等输入附件超过该时长后由定时维护删除；删除会话时会立即删除其关联附件。"},
		{Key: "web_search_daily_call_limit", Group: "system", Label: "网页搜索每日请求上限", Type: "integer", Default: 100, Min: floatPtr(1), Max: floatPtr(100000), Help: "Abot 本地硬限制，按 UTC 日统计实际发起的服务请求；达到上限后不再调用搜索 API。"},
		{Key: "web_search_max_calls_per_invocation", Group: "system", Label: "单任务网页搜索上限", Type: "integer", Default: 8, Min: floatPtr(1), Max: floatPtr(100), Help: "限制一次 Agent 任务（包含回退尝试）实际发起的搜索请求数量。"},
		{Key: "web_search_alert_percent", Group: "system", Label: "网页搜索用量告警比例（%）", Type: "integer", Default: 80, Min: floatPtr(1), Max: floatPtr(100), Help: "达到每日请求上限的该比例时在日志和用量页提示。"},
		{Key: "modal_fallback_enabled", Group: "system", Label: "启用多模态降级", Type: "boolean", Default: false, Help: "主模型不支持图片或音频时，使用配置的模型生成文字转述；失败时仍会以说明文字完成本轮。"},
		{Key: "modal_fallback_provider_id", Group: "system", Label: "多模态降级供应商", Type: "string", Default: "", Help: "WebUI 会从已配置供应商目录提供选择；留空表示沿用主模型供应商。"},
		{Key: "modal_fallback_vision_model", Group: "system", Label: "图片降级模型", Type: "string", Default: "", Help: "从所选供应商的模型目录选择；留空表示不对图片执行模型转述。"},
		{Key: "modal_fallback_audio_model", Group: "system", Label: "音频降级模型", Type: "string", Default: "", Help: "从所选供应商的模型目录选择；留空表示不对音频执行模型转述。"},
		{Key: "subagent_enabled", Group: "system", Label: "启用子 Agent", Type: "boolean", Default: true, Help: "主 Agent 是否允许启动通用子 Agent；关闭后所有子 Agent 类型都不会启动，但主 Agent 仍可继续回答。当前会话可用 /subagent 覆盖。"},
	}}
}

// Runtime 是配置解析后的 Agent 运行参数，不向 HTTP 层暴露内部实现细节。
type Runtime struct {
	AIEnabled                     bool
	AIExecutionMode               string
	ProviderID                    string
	ModelID                       string
	AITemperature                 float64
	AIReasoningEffort             string
	AITopP                        float64
	AIMaxOutputTokens             int
	AIRequestRetries              int
	PersonaID                     string
	Instruction                   string
	CompactionEnabled             bool
	CompactionRatio               float64
	CompactionSafetyTokens        int
	CompactionRetentionEvents     int
	CompactionInterval            int
	CompactionOverlap             int
	CompactionUnknownWindowTokens int
	AgentMaxToolCalls             int
	ToolSchemaBudgetTokens        int
	WorkspaceEnabled              bool
	WorkspaceReadEnabled          bool
	WorkspaceWriteEnabled         bool
	WorkspaceExecEnabled          bool
	WorkspaceGitEnabled           bool
	WorkspaceCommandTimeoutSecs   int
	MessageStreamingEnabled       bool
	MessagePromptPrefix           string
	// PlatformAdminIDs 和 WakeupWords 由 Bot 消息入口使用，保持配置中心与平台鉴权共用一份解析结果。
	PlatformAdminIDs      []string
	WakeupWords           []string
	PrivateRequiresWakeup bool
	// 平台策略由消息入口与平台发送器共同执行，不把无效开关暴露为已实现能力。
	Platform PlatformSettings
	// 扩展配置由 Bot 入口消费；按配置文件解析，避免界面开关只停留在数据库。
	Extensions          ExtensionSettings
	MemoryEnabled       bool
	MemoryAutoRetrieve  bool
	MemoryMaxResults    int
	WebSearchEnabled    bool
	WebSearchServiceIDs []string
}

// PlatformSettings 对齐当前 Telegram/OneBot 能执行的平台通用配置。
type PlatformSettings struct {
	UniqueSession          bool
	ReplyPrefix            string
	ReplyMention           bool
	ReplyQuote             bool
	PrivateReplyQuote      bool
	WhitelistEnabled       bool
	WhitelistIDs           []string
	WhitelistLog           bool
	WhitelistAdminGroup    bool
	WhitelistAdminPrivate  bool
	RateLimitSeconds       int
	RateLimitCount         int
	RateLimitStrategy      string
	IgnoreBotSelfMessage   bool
	IgnoreAtAll            bool
	DisableBuiltinCommands bool
	NoPermissionReply      bool
	EmptyMentionWaiting    bool
	EmptyMentionNeedReply  bool
	BlockPatterns          []string
	CheckResponse          bool
	TelegramPreAckEnabled  bool
	TelegramPreAckEmoji    string
}

// ExtensionSettings 汇总扩展页的三组内置行为，所有上限都在 Schema 校验。
type ExtensionSettings struct {
	SegmentedReplyEnabled     bool
	SegmentOnlyLLM            bool
	SegmentIntervalMethod     string
	SegmentInterval           string
	SegmentLogBase            float64
	SegmentWordsThreshold     int
	SegmentSplitMode          string
	SegmentRegex              string
	SegmentSplitWords         []string
	SegmentCleanupRegex       string
	GroupContextEnabled       bool
	GroupMessageMaxCount      int
	GroupImageCaption         bool
	GroupImageCaptionModel    string
	ProactiveReplyEnabled     bool
	ProactiveReplyMethod      string
	ProactiveReplyProbability float64
	ProactiveReplyWhitelist   []string
}

// Repository 是配置中心需要的持久化能力，SQLite 实现位于 storage/sqlite，便于单元测试替换。
type Repository interface {
	ListProfiles(context.Context) ([]Profile, error)
	GetProfile(context.Context, string) (Profile, error)
	SaveProfile(context.Context, Profile, Revision) error
	DeleteProfile(context.Context, string) error
	SetDefaultProfile(context.Context, string) error
	DefaultProfileID(context.Context) (string, error)
	Bind(context.Context, BindingScope, string, string) error
	GetBinding(context.Context, BindingScope, string) (string, error)
	SaveSystemSettings(context.Context, SystemSettings) error
	GetSystemSettings(context.Context) (SystemSettings, error)
}

// ModelValidator 只验证供应商引用是否存在；模型 ID 允许目录外手动模型，符合现有供应商配置语义。
type ModelValidator func(context.Context, string, string) error

// SystemSettingsApplier 在系统设置写入后立即更新进程中的可热更新模块。
type SystemSettingsApplier func(context.Context, SystemSettings) error

// Service 是配置中心唯一的业务入口，负责默认值、Schema 校验、修订和继承解析。
type Service struct {
	repository     Repository
	modelValidator ModelValidator

	mu            sync.RWMutex
	systemApplier SystemSettingsApplier
	defaultValues Values
	schema        Schema
	fields        map[string]Field
}

// NewService 创建配置服务；创建时不依赖任何平台或插件，因此可以零供应商启动。
func NewService(repository Repository, validator ModelValidator) (*Service, error) {
	if repository == nil {
		return nil, errors.New("配置 Repository 不能为空")
	}
	schema := buildSchema()
	fields := make(map[string]Field, len(schema.Fields))
	defaults := make(Values, len(schema.Fields))
	for _, field := range schema.Fields {
		fields[field.Key] = field
		defaults[field.Key] = cloneValue(field.Default)
	}
	return &Service{repository: repository, modelValidator: validator, defaultValues: defaults, schema: schema, fields: fields}, nil
}

// SetSystemSettingsApplier 设置日志等级等系统设置的运行时应用回调。
func (s *Service) SetSystemSettingsApplier(applier SystemSettingsApplier) {
	s.mu.Lock()
	s.systemApplier = applier
	s.mu.Unlock()
}

// EnsureDefault 保证系统至少有一个配置文件，但不会创建任何用户指定的工作区或项目目录。
func (s *Service) EnsureDefault(ctx context.Context) error {
	profiles, err := s.repository.ListProfiles(ctx)
	if err != nil {
		return fmt.Errorf("加载配置文件失败: %w", err)
	}
	if len(profiles) == 0 {
		now := time.Now().UTC()
		item := Profile{ID: "default", Name: "默认配置", Revision: 1, Values: s.DefaultValues(), IsDefault: true, CreatedAt: now, UpdatedAt: now}
		return s.repository.SaveProfile(ctx, item, Revision{ProfileID: item.ID, Number: item.Revision, Values: item.Values, CreatedAt: now})
	}
	for _, item := range profiles {
		if item.IsDefault {
			return nil
		}
	}
	// 兼容早期手工数据库：没有默认标记时选排序后的第一项。
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return s.repository.SetDefaultProfile(ctx, profiles[0].ID)
}

func (s *Service) Schema() Schema {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSchema(s.schema)
}

func (s *Service) DefaultValues() Values {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneValues(s.defaultValues)
}

func (s *Service) ListProfiles(ctx context.Context) ([]Profile, error) {
	items, err := s.repository.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i] = s.normalizeProfile(items[i])
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDefault != items[j].IsDefault {
			return items[i].IsDefault
		}
		return items[i].Name < items[j].Name
	})
	return items, nil
}

func (s *Service) GetProfile(ctx context.Context, id string) (Profile, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Profile{}, fmt.Errorf("%w: profile_id 不能为空", ErrInvalidRequest)
	}
	item, err := s.repository.GetProfile(ctx, id)
	if err != nil {
		return Profile{}, err
	}
	return s.normalizeProfile(item), nil
}

// ListRevisions 返回指定配置文件的历史快照，最新修订排在最前面。
func (s *Service) ListRevisions(ctx context.Context, id string) ([]Revision, error) {
	item, err := s.GetProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	repository, ok := s.repository.(RevisionRepository)
	if !ok {
		return []Revision{{ProfileID: item.ID, Number: item.Revision, Values: s.publicValues(item.Values), CreatedAt: item.UpdatedAt}}, nil
	}
	items, err := repository.ListRevisions(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	for index := range items {
		items[index].ProfileID = item.ID
		// 修订接口也不能绕过敏感字段脱敏规则。
		items[index].Values = s.publicValues(items[index].Values)
	}
	if len(items) == 0 {
		// 兼容只有当前配置行、没有历史表数据的早期数据库。
		return []Revision{{ProfileID: item.ID, Number: item.Revision, Values: s.publicValues(item.Values), CreatedAt: item.UpdatedAt}}, nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Number > items[j].Number })
	return items, nil
}

// SaveProfile 校验草稿后生成新修订；失败时不会写入或切换当前有效配置。
func (s *Service) SaveProfile(ctx context.Context, id, name string, values Values) (Profile, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" {
		id = newID("profile")
	}
	if err := validateID(id); err != nil {
		return Profile{}, err
	}
	if name == "" {
		return Profile{}, fmt.Errorf("%w: 配置名称不能为空", ErrInvalidRequest)
	}
	// 保存前把旧版本的 JSON 字符串列表转换为真正的字符串数组，避免用户只修改
	// 其他配置时被历史格式卡住，同时保证后续 WebUI 读取到统一的数据形状。
	values = s.normalizeListValues(values)
	if err := s.ValidateValues(ctx, values); err != nil {
		return Profile{}, err
	}
	old, oldErr := s.repository.GetProfile(ctx, id)
	if oldErr != nil && !errors.Is(oldErr, ErrNotFound) {
		return Profile{}, oldErr
	}
	if oldErr == nil {
		// 更新同一 ID 时保留创建时间、默认标记和已有修订序列。
		if old.Name != name {
			if conflict, err := s.nameConflict(ctx, name, id); err != nil {
				return Profile{}, err
			} else if conflict {
				return Profile{}, fmt.Errorf("%w: 配置名称 %q 已存在", ErrConflict, name)
			}
		}
	} else if conflict, err := s.nameConflict(ctx, name, id); err != nil {
		return Profile{}, err
	} else if conflict {
		return Profile{}, fmt.Errorf("%w: 配置名称 %q 已存在", ErrConflict, name)
	}
	now := time.Now().UTC()
	revision := 1
	createdAt := now
	isDefault := false
	if oldErr == nil {
		revision = old.Revision + 1
		createdAt = old.CreatedAt
		isDefault = old.IsDefault
	}
	item := Profile{ID: id, Name: name, Revision: revision, Values: cloneValues(values), IsDefault: isDefault, CreatedAt: createdAt, UpdatedAt: now}
	if err := s.repository.SaveProfile(ctx, item, Revision{ProfileID: id, Number: revision, Values: values, CreatedAt: now}); err != nil {
		return Profile{}, fmt.Errorf("保存配置文件失败: %w", err)
	}
	return item, nil
}

// ValidateValues 是可视化表单和 JSON 编辑器共用的后端验证入口。
func (s *Service) ValidateValues(ctx context.Context, values Values) error {
	for key := range values {
		if _, ok := s.fields[key]; !ok {
			return fmt.Errorf("%w: 未知配置项 %q", ErrInvalidRequest, key)
		}
	}
	for _, field := range s.Schema().Fields {
		value, exists := values[field.Key]
		if !exists {
			continue
		}
		switch field.Type {
		case "boolean":
			if _, ok := boolValue(value); !ok {
				return fmt.Errorf("%w: %s 必须是布尔值", ErrInvalidRequest, field.Label)
			}
		case "integer":
			number, ok := integerValue(value)
			if !ok {
				return fmt.Errorf("%w: %s 必须是整数", ErrInvalidRequest, field.Label)
			}
			if field.Min != nil && float64(number) < *field.Min || field.Max != nil && float64(number) > *field.Max {
				return fmt.Errorf("%w: %s 超出允许范围", ErrInvalidRequest, field.Label)
			}
		case "number":
			number, ok := numberValue(value)
			if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
				return fmt.Errorf("%w: %s 必须是数字", ErrInvalidRequest, field.Label)
			}
			if field.Min != nil && number < *field.Min || field.Max != nil && number > *field.Max {
				return fmt.Errorf("%w: %s 超出允许范围", ErrInvalidRequest, field.Label)
			}
		case "list":
			// 列表字段只接受字符串元素；同时兼容早期保存的 JSON 字符串。
			if _, ok := stringListValue(value); !ok {
				return fmt.Errorf("%w: %s 必须是字符串列表", ErrInvalidRequest, field.Label)
			}
		case "string", "textarea", "select":
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("%w: %s 必须是字符串", ErrInvalidRequest, field.Label)
			}
			if field.Required && strings.TrimSpace(text) == "" {
				return fmt.Errorf("%w: %s 不能为空", ErrInvalidRequest, field.Label)
			}
			// 带有固定选项的下拉框只接受 Schema 声明的值，尤其是执行方式不能绕过内置 AI 边界。
			if field.Type == "select" && len(field.Options) > 0 {
				matched := false
				for _, option := range field.Options {
					if option.Value == text {
						matched = true
						break
					}
				}
				if !matched {
					return fmt.Errorf("%w: %s 的选项无效", ErrInvalidRequest, field.Label)
				}
			}
		}
	}
	if err := validateSlidingCompactionValues(values); err != nil {
		return err
	}
	// 正则和间隔在保存时检查，避免运行中的平台回复因无效配置丢失。
	for _, key := range []string{"extensions.segment_regex", "extensions.segment_cleanup_regex"} {
		if pattern := stringOr(values[key]); pattern != "" {
			if len(pattern) > 512 {
				return fmt.Errorf("%w: %s 长度不能超过 512", ErrInvalidRequest, key)
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("%w: %s 正则表达式无效: %v", ErrInvalidRequest, key, err)
			}
		}
	}
	if _, exists := values["extensions.segment_interval"]; exists {
		if _, _, err := parseSegmentInterval(stringOr(values["extensions.segment_interval"])); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	if boolOr(values["extensions.group_image_caption"], false) && strings.TrimSpace(stringOr(values["extensions.group_image_caption_model"])) == "" {
		return fmt.Errorf("%w: 自动理解群图片需要选择群图片转述模型", ErrInvalidRequest)
	}
	webSearchServices := stringListOr(values["ai.web_search.service_ids"])
	if len(webSearchServices) > 64 {
		return fmt.Errorf("%w: 网页搜索服务优先级最多配置 64 项", ErrInvalidRequest)
	}
	for _, id := range webSearchServices {
		if len(id) > 128 {
			return fmt.Errorf("%w: 网页搜索服务 ID 不能超过 128 个字符", ErrInvalidRequest)
		}
	}
	// 用户自定义屏蔽规则必须有界且可编译，避免保存后在消息路径上反复失败。
	patterns := stringListOr(values["platform.block_patterns"])
	if len(patterns) > 50 {
		return fmt.Errorf("%w: 内容屏蔽规则不能超过 50 条", ErrInvalidRequest)
	}
	for _, pattern := range patterns {
		if len(pattern) > 256 {
			return fmt.Errorf("%w: 单条内容屏蔽规则不能超过 256 字节", ErrInvalidRequest)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("%w: 内容屏蔽正则无效: %v", ErrInvalidRequest, err)
		}
	}
	providerID, _ := values["ai.default_provider_id"].(string)
	modelID, _ := values["ai.default_model_id"].(string)
	if s.modelValidator != nil {
		if err := s.modelValidator(ctx, strings.TrimSpace(providerID), strings.TrimSpace(modelID)); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	return nil
}

// parseSegmentInterval 要求两个有界秒数且下限不大于上限，防止异常配置造成长时间阻塞。
func parseSegmentInterval(value string) (float64, float64, error) {
	// 严格拆分两个数字，拒绝带额外尾随内容的设置。
	parts := strings.Split(value, ",")
	if len(parts) != 2 {
		return 0, 0, errors.New("分段随机间隔必须是 0-10 秒内的 最小值,最大值")
	}
	minimum, minErr := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	maximum, maxErr := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if minErr != nil || maxErr != nil || math.IsNaN(minimum) || math.IsNaN(maximum) || math.IsInf(minimum, 0) || math.IsInf(maximum, 0) || minimum < 0 || maximum > 10 || minimum > maximum {
		return 0, 0, errors.New("分段随机间隔必须是 0-10 秒内的 最小值,最大值")
	}
	return minimum, maximum, nil
}

// validateSlidingCompactionValues enforces the relationship between the two
// sliding-window knobs. It is kept separate so direct callers and tests can
// exercise the cross-field rule without needing a persistence implementation.
func validateSlidingCompactionValues(values Values) error {
	interval := intOr(values["context.compaction.sliding_interval"], 0)
	overlap := intOr(values["context.compaction.sliding_overlap"], 0)
	if overlap > 0 && interval == 0 {
		return fmt.Errorf("%w: 滑动窗口重叠轮次必须在启用压缩间隔后设置", ErrInvalidRequest)
	}
	if overlap > interval {
		return fmt.Errorf("%w: 滑动窗口重叠轮次不能大于压缩间隔", ErrInvalidRequest)
	}
	return nil
}

// ValidateDraft 校验未保存的配置草稿，不产生修订，也不改变当前运行配置。
func (s *Service) ValidateDraft(ctx context.Context, id, name string, values Values) error {
	if strings.TrimSpace(id) != "" {
		if err := validateID(strings.TrimSpace(id)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: 配置名称不能为空", ErrInvalidRequest)
	}
	if err := s.ValidateValues(ctx, values); err != nil {
		return err
	}
	return nil
}

func (s *Service) DeleteProfile(ctx context.Context, id string) error {
	item, err := s.GetProfile(ctx, id)
	if err != nil {
		return err
	}
	if item.IsDefault {
		return ErrDefaultProfile
	}
	return s.repository.DeleteProfile(ctx, item.ID)
}

func (s *Service) SetDefaultProfile(ctx context.Context, id string) error {
	if _, err := s.GetProfile(ctx, id); err != nil {
		return err
	}
	return s.repository.SetDefaultProfile(ctx, strings.TrimSpace(id))
}

func (s *Service) DefaultProfileID(ctx context.Context) (string, error) {
	id, err := s.repository.DefaultProfileID(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(id), nil
}

// EffectiveValues 合并内置默认值和选定配置文件，供运行时和调试页面使用。
func (s *Service) EffectiveValues(ctx context.Context, id string) (Values, error) {
	values := s.DefaultValues()
	if strings.TrimSpace(id) == "" {
		return values, nil
	}
	item, err := s.GetProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	for key, value := range item.Values {
		values[key] = cloneValue(value)
	}
	return s.normalizeListValues(values), nil
}

// RuntimeForProfile 将指定配置文件解析为运行参数，供会话规则和调度任务复用。
func (s *Service) RuntimeForProfile(ctx context.Context, id string) (Runtime, error) {
	values, err := s.EffectiveValues(ctx, strings.TrimSpace(id))
	if err != nil {
		return Runtime{}, err
	}
	return runtimeFromValues(values), nil
}

// Resolve 按系统默认 → 机器人 → 对话的优先级解析配置；会话显式 State 由 Agent 层继续覆盖。
func (s *Service) Resolve(ctx context.Context, botID, conversationID string) (Runtime, error) {
	profileID, err := s.DefaultProfileID(ctx)
	if err != nil {
		return Runtime{}, err
	}
	if strings.TrimSpace(botID) != "" {
		if bound, bindErr := s.repository.GetBinding(ctx, BindingBot, strings.TrimSpace(botID)); bindErr != nil {
			return Runtime{}, bindErr
		} else if bound != "" {
			profileID = bound
		}
	}
	if strings.TrimSpace(conversationID) != "" {
		if bound, bindErr := s.repository.GetBinding(ctx, BindingConversation, strings.TrimSpace(conversationID)); bindErr != nil {
			return Runtime{}, bindErr
		} else if bound != "" {
			profileID = bound
		}
	}
	values, err := s.EffectiveValues(ctx, profileID)
	if err != nil {
		return Runtime{}, err
	}
	return runtimeFromValues(values), nil
}

func (s *Service) Bind(ctx context.Context, scope BindingScope, targetID, profileID string) error {
	if scope != BindingBot && scope != BindingConversation {
		return fmt.Errorf("%w: 绑定范围无效", ErrInvalidRequest)
	}
	targetID = strings.TrimSpace(targetID)
	profileID = strings.TrimSpace(profileID)
	if targetID == "" {
		return fmt.Errorf("%w: 绑定目标不能为空", ErrInvalidRequest)
	}
	if profileID != "" {
		if _, err := s.GetProfile(ctx, profileID); err != nil {
			return err
		}
	}
	return s.repository.Bind(ctx, scope, targetID, profileID)
}

func (s *Service) Binding(ctx context.Context, scope BindingScope, targetID string) (string, error) {
	return s.repository.GetBinding(ctx, scope, strings.TrimSpace(targetID))
}

// Export 只导出 Schema 已知字段；当前没有可读敏感字段，未来加入敏感字段时统一在这里剔除。
func (s *Service) Export(ctx context.Context, id string) (map[string]any, error) {
	item, err := s.GetProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"version": SchemaVersion, "id": item.ID, "name": item.Name, "revision": item.Revision, "values": s.publicValues(item.Values)}, nil
}

// publicValues 统一处理配置读取和导出时的敏感字段过滤。
func (s *Service) publicValues(values Values) Values {
	result := Values{}
	for key, value := range values {
		field, ok := s.fields[key]
		if ok && field.Secret {
			continue
		}
		result[key] = cloneValue(value)
	}
	return result
}

// Import 导入时默认生成新 ID；只有明确 overwrite=true 才允许覆盖同 ID 或同名配置。
func (s *Service) Import(ctx context.Context, id, name string, values Values, overwrite bool) (Profile, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" {
		id = newID("profile")
	}
	if !overwrite {
		if _, err := s.repository.GetProfile(ctx, id); err == nil {
			return Profile{}, fmt.Errorf("%w: 配置 ID %q 已存在", ErrConflict, id)
		} else if !errors.Is(err, ErrNotFound) {
			return Profile{}, err
		}
		if conflict, err := s.nameConflict(ctx, name, ""); err != nil {
			return Profile{}, err
		} else if conflict {
			return Profile{}, fmt.Errorf("%w: 配置名称 %q 已存在，请明确覆盖", ErrConflict, name)
		}
	}
	if overwrite {
		if existing, err := s.repository.GetProfile(ctx, id); err == nil {
			return s.SaveProfile(ctx, existing.ID, nameOr(name, existing.Name), values)
		} else if !errors.Is(err, ErrNotFound) {
			return Profile{}, err
		}
	}
	return s.SaveProfile(ctx, id, name, values)
}

func (s *Service) GetSystemSettings(ctx context.Context) (SystemSettings, error) {
	settings, err := s.repository.GetSystemSettings(ctx)
	if err != nil {
		return SystemSettings{}, err
	}
	return normalizeSystemSettings(settings), nil
}

func (s *Service) SaveSystemSettings(ctx context.Context, settings SystemSettings) (SystemSettings, error) {
	settings = normalizeSystemSettings(settings)
	if err := validateSystemSettings(settings); err != nil {
		return SystemSettings{}, err
	}
	old, err := s.GetSystemSettings(ctx)
	if err != nil {
		return SystemSettings{}, err
	}
	if err := s.repository.SaveSystemSettings(ctx, settings); err != nil {
		return SystemSettings{}, fmt.Errorf("保存系统设置失败: %w", err)
	}
	s.mu.RLock()
	applier := s.systemApplier
	s.mu.RUnlock()
	if applier != nil {
		if err := applier(ctx, settings); err != nil {
			// 运行时应用失败时回写上一份设置，避免数据库和进程状态分裂。
			_ = s.repository.SaveSystemSettings(ctx, old)
			return SystemSettings{}, fmt.Errorf("应用系统设置失败: %w", err)
		}
	}
	return settings, nil
}

func buildSchema() Schema {
	return Schema{Version: SchemaVersion, Fields: []Field{
		{Key: "ai.enabled", Group: "ai", Label: "启用 AI", Type: "boolean", Default: true, Help: "关闭后 Agent 会拒绝新的 AI 请求，但历史对话仍可查看。"},
		{Key: "ai.execution_mode", Group: "ai", Label: "执行方式", Type: "select", Default: BuiltinAIExecutionMode, Options: []SchemaOption{{Value: BuiltinAIExecutionMode, Label: "内置 AI"}}, Help: "Abot 只使用进程内置 Agent Runtime；Dify、Coze、百炼和 DeerFlow 不在本版本中启用。"},
		{Key: "ai.default_provider_id", Group: "ai", Label: "默认供应商", Type: "select", Default: "", OptionSource: "providers", Help: "为空时沿用供应商注册表的兼容默认值。"},
		{Key: "ai.default_model_id", Group: "ai", Label: "默认模型", Type: "string", Default: "", Help: "WebUI 会优先提供已配置模型目录；留空表示沿用供应商默认模型。"},
		{Key: "ai.fallback_models", Group: "ai", Label: "回退对话模型", Type: "list", Default: []string{}, Help: "按顺序添加主模型失败后的回退模型；当前内置 Runtime 只使用已选主模型。"},
		{Key: "ai.temperature", Group: "ai", Label: "温度", Type: "number", Default: 0.7, Min: floatPtr(0), Max: floatPtr(2), Help: "控制输出随机性；较低值更稳定，较高值更有创造性。"},
		{Key: "ai.top_p", Group: "ai", Label: "Top P", Type: "number", Default: 1.0, Min: floatPtr(0.01), Max: floatPtr(1), Help: "限制采样候选范围；通常与温度二选一调整。"},
		{Key: "ai.max_output_tokens", Group: "ai", Label: "最大输出 token", Type: "integer", Default: 0, Min: floatPtr(0), Max: floatPtr(1000000), Help: "0 表示使用模型目录或上游接口默认值。"},
		{Key: "ai.request_retries", Group: "ai", Label: "请求失败重试次数", Type: "integer", Default: 2, Min: floatPtr(0), Max: floatPtr(5), Help: "仅在模型尚未返回任何内容时重试，避免流式输出重复。"},
		{Key: "ai.reasoning_effort", Group: "ai", Label: "思考强度", Type: "select", Default: "", Options: []SchemaOption{{Value: "", Label: "不指定"}, {Value: "minimal", Label: "最少"}, {Value: "low", Label: "低"}, {Value: "medium", Label: "中"}, {Value: "high", Label: "高"}}, Help: "仅在模型能力支持时下发；聊天界面可以按请求临时覆盖。"},
		{Key: "ai.health_mode", Group: "ai", Label: "健康模式", Type: "boolean", Default: false, Help: "向内置 Agent 注入安全和健康内容边界，避免将该设置误认为外部审核服务。"},
		{Key: "ai.knowledge_bases", Group: "capabilities", Label: "知识库", Type: "list", Default: []string{}, Help: "逐项添加知识库 ID；当前版本保留配置入口，未接入外部知识库服务。"},
		{Key: "ai.knowledge_top_k", Group: "capabilities", Label: "知识库返回数量", Type: "integer", Default: 5, Min: floatPtr(1), Max: floatPtr(100), Help: "知识库检索的最大返回条数。"},
		{Key: "ai.agentic_retrieval", Group: "capabilities", Label: "Agentic 知识库检索", Type: "boolean", Default: false, Help: "允许知识库作为 Agent 工具；未配置知识库时不会添加工具。"},
		{Key: "ai.web_search.enabled", Group: "capabilities", Label: "启用网页搜索", Type: "boolean", Default: false, Help: "启用后内置 Agent 和允许使用搜索工具的通用子 Agent 可以按需检索互联网。"},
		{Key: "ai.web_search.service_ids", Group: "capabilities", Label: "网页搜索服务优先级", Type: "list", Default: []string{}, DisplayIf: map[string]any{"ai.web_search.enabled": true}, Help: "从网页搜索服务页添加已配置服务并调整调用顺序；留空时按服务优先级自动选择所有启用项。"},
		{Key: "ai.computer_use.environment", Group: "capabilities", Label: "电脑使用环境", Type: "select", Default: "none", Options: []SchemaOption{{Value: "none", Label: "不启用"}, {Value: "local", Label: "本机"}, {Value: "sandbox", Label: "沙箱"}}, Help: "电脑使用能力仍受工作区工具和人工审批约束。"},
		{Key: "ai.proactive_enabled", Group: "capabilities", Label: "主动型能力", Type: "boolean", Default: true, Help: "允许未来任务唤醒内置 Agent；任务仍由本地调度器执行。"},
		{Key: "persona.id", Group: "persona", Label: "人格 ID", Type: "select", Default: "", OptionSource: "personas", Help: "选择人格目录中的稳定 ID；为空时继续使用当前配置中的系统提示词。"},
		{Key: "persona.system_prompt", Group: "persona", Label: "系统提示词", Type: "textarea", Default: "你是 Abot，一个可靠、简洁、遵守用户意图的中文 AI 助手。", Help: "兼容旧配置；选择人格 ID 后由人格目录中的指令覆盖。"},
		{Key: "persona.failure_reply", Group: "persona", Label: "LLM 失败回复", Type: "textarea", Default: "", Help: "模型失败时的可选固定回复。"},
		{Key: "context.compaction.enabled", Group: "context", Label: "启用上下文压缩", Type: "boolean", Default: true, Help: "使用 ADK 原生压缩保留长对话的最近事件。"},
		{Key: "context.compaction.max_turns", Group: "context", Label: "压缩前最多对话轮数", Type: "integer", Default: -1, Min: floatPtr(-1), Max: floatPtr(10000), Help: "-1 表示不按轮数限制，仍受模型上下文窗口约束。"},
		{Key: "context.compaction.drop_turns", Group: "context", Label: "超限一次丢弃轮数", Type: "integer", Default: 1, Min: floatPtr(1), Max: floatPtr(1000), Help: "按轮数截断模式每次移除的最旧轮数。"},
		{Key: "context.compaction.mode", Group: "context", Label: "超限处理方式", Type: "select", Default: "llm", Options: []SchemaOption{{Value: "turns", Label: "按对话轮数截断"}, {Value: "llm", Label: "由 LLM 压缩上下文"}}, Help: "内置 ADK 当前使用 LLM 压缩；轮数配置用于兼容导入。"},
		{Key: "context.compaction.prompt", Group: "context", Label: "压缩提示词", Type: "textarea", Default: "请保留与当前任务相关的事实、决定和未完成事项。", Help: "仅影响内置 AI 的上下文压缩提示。"},
		{Key: "context.compaction.trigger_ratio", Group: "context", Label: "压缩触发比例", Type: "number", Default: 0.8, Min: floatPtr(0.1), Max: floatPtr(0.99), Help: "上下文窗口达到该比例时触发压缩。"},
		{Key: "context.compaction.safety_tokens", Group: "context", Label: "压缩安全余量", Type: "integer", Default: 512, Min: floatPtr(0), Max: floatPtr(100000), Help: "为模型输出预算保留的 token 余量。"},
		{Key: "context.compaction.retention_events", Group: "context", Label: "压缩后保留事件数", Type: "integer", Default: 10, Min: floatPtr(1), Max: floatPtr(1000), Help: "压缩后保留的最近会话事件数量。"},
		{Key: "context.compaction.sliding_interval", Group: "context", Label: "滑动窗口压缩间隔", Type: "integer", Default: 0, Min: floatPtr(0), Max: floatPtr(1000), Help: "每完成指定数量的用户轮次后生成一次滑动窗口摘要；0 表示关闭。"},
		{Key: "context.compaction.sliding_overlap", Group: "context", Label: "滑动窗口重叠轮次", Type: "integer", Default: 0, Min: floatPtr(0), Max: floatPtr(100), Help: "下一次滑动窗口重复携带的已摘要轮次数；必须不大于压缩间隔。"},
		{Key: "context.compaction.unknown_window_tokens", Group: "context", Label: "未知窗口兜底 token", Type: "integer", Default: 8192, Min: floatPtr(1024), Max: floatPtr(1000000), Help: "模型目录没有上下文窗口时使用的保守估计。"},
		{Key: "agent.max_tool_calls", Group: "agent", Label: "单轮最大工具调用数", Type: "integer", Default: 20, Min: floatPtr(1), Max: floatPtr(100), Help: "达到上限后 Agent 会暂时移除工具声明，要求模型先给出当前结果。"},
		{Key: "agent.tool_schema_budget_tokens", Group: "agent", Label: "工具 Schema token 预算", Type: "integer", Default: 4096, Min: floatPtr(256), Max: floatPtr(100000), Help: "限制单次模型请求携带的工具 Schema 大小；超出时按确定性评分裁剪可选工具。"},
		{Key: "agent.tool_call_mode", Group: "agent", Label: "工具调用模式", Type: "select", Default: "full", Options: []SchemaOption{{Value: "full", Label: "full"}, {Value: "skills-like", Label: "skills-like"}}, Help: "控制内置 Agent 暴露工具的组织方式。"},
		{Key: "agent.tool_call_timeout_seconds", Group: "agent", Label: "工具调用超时（秒）", Type: "integer", Default: 120, Min: floatPtr(1), Max: floatPtr(3600), Help: "单个工具调用的最大等待时间。"},
		{Key: "workspace.enabled", Group: "workspace", Label: "启用项目能力", Type: "boolean", Default: true, Help: "关闭后 Agent 不会加载当前项目工具。"},
		{Key: "workspace.read_enabled", Group: "workspace", Label: "允许读取项目", Type: "boolean", Default: true, DisplayIf: map[string]any{"workspace.enabled": true}, Help: "控制列目录、读文件、搜索和 Git 状态工具。"},
		{Key: "workspace.write_enabled", Group: "workspace", Label: "允许申请写入", Type: "boolean", Default: true, DisplayIf: map[string]any{"workspace.enabled": true}, Help: "写入仍然必须在工作区页面中由用户批准。"},
		{Key: "workspace.exec_enabled", Group: "workspace", Label: "允许申请执行命令", Type: "boolean", Default: true, DisplayIf: map[string]any{"workspace.enabled": true}, Help: "命令仍然必须由用户批准，且只在项目目录边界内执行。"},
		{Key: "workspace.git_enabled", Group: "workspace", Label: "允许读取 Git 状态", Type: "boolean", Default: true, DisplayIf: map[string]any{"workspace.enabled": true, "workspace.read_enabled": true}, Help: "控制 Agent 是否能调用项目 Git 状态工具；页面上的手动查看仍可用。"},
		{Key: "workspace.command_timeout_seconds", Group: "workspace", Label: "命令超时秒数", Type: "integer", Default: 60, Min: floatPtr(1), Max: floatPtr(120), DisplayIf: map[string]any{"workspace.exec_enabled": true}, Help: "限制 Agent 申请的单次项目命令运行时间。"},
		{Key: "message.streaming_enabled", Group: "message", Label: "启用流式输出", Type: "boolean", Default: true, Help: "关闭后 WebUI 仍会等待同一轮结果，但不会逐片推送；平台消息不受影响。"},
		{Key: "message.prompt_prefix", Group: "message", Label: "用户提示词前缀", Type: "string", Default: "", Help: "在每条用户消息前加入固定前缀；留空表示不注入。"},
		{Key: "message.show_thinking", Group: "message", Label: "显示思考内容", Type: "boolean", Default: false, Help: "控制管理台是否显示模型返回的思考摘要，不改变内置 Agent 的安全边界。"},
		{Key: "message.realtime_segmented_reply", Group: "message", Label: "实时分段回复", Type: "boolean", Default: false, Help: "平台不支持流式时按段投递；WebUI 使用 SSE。"},
		{Key: "message.user_identification", Group: "message", Label: "用户识别", Type: "boolean", Default: true, Help: "将会话用户标识作为运行时元数据，而不是拼入用户提示词。"},
		{Key: "message.group_name_awareness", Group: "message", Label: "显示群名称", Type: "boolean", Default: true, Help: "允许平台适配器提供群名称上下文。"},
		{Key: "message.world_time_awareness", Group: "message", Label: "现实世界时间感知", Type: "boolean", Default: true, Help: "为内置 Agent 提供当前服务端时间。"},
		{Key: "message.extra_wakeup_prefix", Group: "message", Label: "额外唤醒前缀", Type: "string", Default: "", Help: "平台消息进入内置 Agent 前追加的唤醒前缀。"},
		{Key: "image.caption_model", Group: "message", Label: "图片转述模型", Type: "string", Default: "", Help: "从已配置模型目录选择；留空表示不强制指定，主模型不支持图片时可使用系统多模态降级。"},
		{Key: "voice.stt_enabled", Group: "message", Label: "启用语音识别", Type: "boolean", Default: false, Help: "语音识别由平台或后续内置适配器提供。"},
		{Key: "voice.stt_model", Group: "message", Label: "默认 STT 模型", Type: "string", Default: "", DisplayIf: map[string]any{"voice.stt_enabled": true}, Help: "从已配置模型目录选择，启用语音识别后使用。"},
		{Key: "voice.tts_enabled", Group: "message", Label: "启用语音回复", Type: "boolean", Default: false, Help: "语音回复由平台适配器决定是否支持。"},
		{Key: "voice.tts_model", Group: "message", Label: "默认 TTS 模型", Type: "string", Default: "", DisplayIf: map[string]any{"voice.tts_enabled": true}, Help: "从已配置模型目录选择，启用语音回复后使用。"},
		{Key: "voice.tts_probability", Group: "message", Label: "TTS 触发概率", Type: "number", Default: 1, Min: floatPtr(0), Max: floatPtr(1), DisplayIf: map[string]any{"voice.tts_enabled": true}, Help: "0-1 之间的语音回复概率。"},
		{Key: "image.compression_enabled", Group: "message", Label: "启用图片压缩", Type: "boolean", Default: true, Help: "上传图片进入内置 Agent 前按边长和质量限制处理。"},
		{Key: "image.max_edge", Group: "message", Label: "图片最大边长", Type: "integer", Default: 2048, Min: floatPtr(128), Max: floatPtr(12000), DisplayIf: map[string]any{"image.compression_enabled": true}, Help: "图片压缩后的最大边长。"},
		{Key: "image.jpeg_quality", Group: "message", Label: "JPEG 质量", Type: "integer", Default: 85, Min: floatPtr(1), Max: floatPtr(100), DisplayIf: map[string]any{"image.compression_enabled": true}, Help: "图片压缩后的 JPEG 质量。"},
		{Key: "message.prompt_template", Group: "message", Label: "用户提示词模板", Type: "textarea", Default: "{{prompt}}", Help: "必须包含 {{prompt}}；模板内容仍经过内置 Agent 的上下文边界。"},
		{Key: "message.parser_depth", Group: "message", Label: "富文本解析深度", Type: "integer", Default: 3, Min: floatPtr(0), Max: floatPtr(20), Help: "平台转发和富文本解析的最大嵌套深度。"},
		{Key: "message.parser_recursive_limit", Group: "message", Label: "递归拉取上限", Type: "integer", Default: 20, Min: floatPtr(0), Max: floatPtr(1000), Help: "平台引用消息递归拉取的最大次数。"},
		{Key: "plugins.enabled", Group: "plugins", Label: "启用插件列表", Type: "list", Default: []string{}, Help: "逐项添加内置工具和插件 ID；未安装插件不会被执行。"},
		{Key: "platform.admin_ids", Group: "platform", Label: "平台管理员 ID 列表", Type: "list", Default: []string{}, Help: "逐项添加平台 UID；配置文件绑定到机器人后会参与全局管理员判断，机器人页面还可配置该 Bot 专属管理员。"},
		{Key: "platform.wakeup_words", Group: "platform", Label: "唤醒词列表", Type: "list", Default: []string{}, Help: "逐项添加唤醒前缀；群聊未 @ 机器人时，以这些前缀开头的消息也会被处理，并会去掉前缀后交给内置 AI。"},
		{Key: "platform.private_requires_wakeup", Group: "platform", Label: "私聊需要唤醒词", Type: "boolean", Default: false, Help: "平台适配器可据此过滤未唤醒消息。"},
		{Key: "platform.reply_prefix", Group: "platform", Label: "回复文本前缀", Type: "string", Default: "", Help: "平台输出前追加的前缀。"},
		{Key: "platform.reply_mention", Group: "platform", Label: "回复时 @ 发送人", Type: "boolean", Default: false, Help: "OneBot 使用 @ 消息段；Telegram 使用用户提及链接。"},
		// 仅列出当前适配器具备实际执行路径的平台选项。
		{Key: "platform.unique_session", Group: "platform", Label: "隔离群成员会话", Type: "boolean", Default: false, Help: "开启后同群不同成员使用独立的对话与任务队列。"},
		{Key: "platform.reply_quote", Group: "platform", Label: "群聊回复时引用发送人消息", Type: "boolean", Default: false, Help: "仅在群聊引用原消息；私聊由独立开关控制。"},
		// 私聊默认直接回复正文，避免每条分段消息都附带相同的引用卡片。
		{Key: "platform.private_reply_quote", Group: "platform", Label: "私聊回复时引用发送人消息", Type: "boolean", Default: false, Help: "默认关闭；开启后私聊回复会引用原消息。"},
		{Key: "platform.empty_mention_waiting", Group: "platform", Label: "仅 @ 时等待下一条消息", Type: "boolean", Default: true, Help: "群成员只 @ 机器人而没有正文时，在一分钟内接收其下一条消息。"},
		{Key: "platform.empty_mention_need_reply", Group: "platform", Label: "等待时发送提示", Type: "boolean", Default: true, DisplayIf: map[string]any{"platform.empty_mention_waiting": true}},
		{Key: "platform.whitelist_enabled", Group: "platform", Label: "启用 ID 白名单", Type: "boolean", Default: true, Help: "名单为空时不限制；按会话来源、群 ID 或用户 ID 匹配。"},
		{Key: "platform.whitelist_ids", Group: "platform", Label: "白名单 ID 列表", Type: "list", Default: []string{}, DisplayIf: map[string]any{"platform.whitelist_enabled": true}, Help: "逐项添加 UMO、群 ID 或用户 ID。"},
		{Key: "platform.whitelist_log", Group: "platform", Label: "白名单拦截日志", Type: "boolean", Default: true, DisplayIf: map[string]any{"platform.whitelist_enabled": true}},
		{Key: "platform.whitelist_admin_group", Group: "platform", Label: "群管理员绕过白名单", Type: "boolean", Default: true, DisplayIf: map[string]any{"platform.whitelist_enabled": true}},
		{Key: "platform.whitelist_admin_private", Group: "platform", Label: "私聊管理员绕过白名单", Type: "boolean", Default: true, DisplayIf: map[string]any{"platform.whitelist_enabled": true}},
		{Key: "platform.rate_limit_seconds", Group: "platform", Label: "消息速率窗口（秒）", Type: "integer", Default: 60, Min: floatPtr(1), Max: floatPtr(3600)},
		{Key: "platform.rate_limit_count", Group: "platform", Label: "窗口内最多消息数", Type: "integer", Default: 30, Min: floatPtr(1), Max: floatPtr(1000)},
		{Key: "platform.rate_limit_strategy", Group: "platform", Label: "超限策略", Type: "select", Default: "stall", Options: []SchemaOption{{Value: "stall", Label: "等待"}, {Value: "discard", Label: "丢弃"}}},
		{Key: "platform.ignore_bot_self_message", Group: "platform", Label: "忽略机器人自身消息", Type: "boolean", Default: false},
		{Key: "platform.ignore_at_all", Group: "platform", Label: "忽略 @ 全体成员", Type: "boolean", Default: false},
		{Key: "platform.disable_builtin_commands", Group: "platform", Label: "禁用内置指令", Type: "boolean", Default: false, Help: "仅禁用普通斜杠指令；审批按钮和待回答问题仍可用。"},
		{Key: "platform.no_permission_reply", Group: "platform", Label: "权限不足时回复", Type: "boolean", Default: true},
		{Key: "platform.block_patterns", Group: "platform", Label: "内容屏蔽规则", Type: "list", Default: []string{}, Help: "逐项添加 Go/RE2 正则；匹配的用户消息不会进入模型。"},
		{Key: "platform.check_response", Group: "platform", Label: "同时检查模型回复", Type: "boolean", Default: false, Help: "启用后，同样用内容屏蔽规则检查模型最终回复。"},
		{Key: "platform.telegram_pre_ack_enabled", Group: "platform", Label: "Telegram 预回应表情", Type: "boolean", Default: false, Help: "接收有效消息后先对原消息添加表情；平台不允许时仅记录日志。"},
		{Key: "platform.telegram_pre_ack_emoji", Group: "platform", Label: "预回应表情", Type: "select", Default: "👀", Options: []SchemaOption{{Value: "👀", Label: "👀"}, {Value: "👍", Label: "👍"}, {Value: "🤔", Label: "🤔"}, {Value: "❤", Label: "❤"}}, DisplayIf: map[string]any{"platform.telegram_pre_ack_enabled": true}},
		// 分段回复只作用于平台文本投递；短消息按标点拆段，长消息保持原样。
		{Key: "extensions.segmented_reply_enabled", Group: "extensions", Label: "启用分段回复", Type: "boolean", Default: false, Help: "按下列规则将 Bot 文本回复分段发送。"},
		{Key: "extensions.segment_only_llm", Group: "extensions", Label: "仅对 LLM 结果分段", Type: "boolean", Default: true, DisplayIf: map[string]any{"extensions.segmented_reply_enabled": true}, Help: "开启时指令、审批和运行状态消息不会被拆分。"},
		{Key: "extensions.segment_interval_method", Group: "extensions", Label: "分段间隔方法", Type: "select", Default: "random", Options: []SchemaOption{{Value: "random", Label: "随机间隔"}, {Value: "log", Label: "按字数计算"}}, DisplayIf: map[string]any{"extensions.segmented_reply_enabled": true}},
		{Key: "extensions.segment_interval", Group: "extensions", Label: "随机间隔（秒）", Type: "string", Default: "1.5,3.5", DisplayIf: map[string]any{"extensions.segment_interval_method": "random", "extensions.segmented_reply_enabled": true}, Help: "填写最小值,最大值，例如 1.5,3.5。"},
		{Key: "extensions.segment_log_base", Group: "extensions", Label: "对数底数", Type: "number", Default: 2.6, Min: floatPtr(1.01), Max: floatPtr(10), DisplayIf: map[string]any{"extensions.segment_interval_method": "log", "extensions.segmented_reply_enabled": true}},
		{Key: "extensions.segment_words_threshold", Group: "extensions", Label: "分段字数阈值", Type: "integer", Default: 150, Min: floatPtr(1), Max: floatPtr(5000), DisplayIf: map[string]any{"extensions.segmented_reply_enabled": true}, Help: "超过阈值的长回复直接发送，不拆段。"},
		{Key: "extensions.segment_split_mode", Group: "extensions", Label: "分段模式", Type: "select", Default: "regex", Options: []SchemaOption{{Value: "regex", Label: "正则表达式"}, {Value: "words", Label: "分段词列表"}}, DisplayIf: map[string]any{"extensions.segmented_reply_enabled": true}},
		{Key: "extensions.segment_regex", Group: "extensions", Label: "分段正则表达式", Type: "string", Default: ".*?[。？！~…]+|.+$", DisplayIf: map[string]any{"extensions.segment_split_mode": "regex", "extensions.segmented_reply_enabled": true}, Help: "按正则匹配片段，使用 Go/RE2 语法。"},
		{Key: "extensions.segment_split_words", Group: "extensions", Label: "分段词列表", Type: "list", Default: []string{"。", "？", "！", "~", "…"}, DisplayIf: map[string]any{"extensions.segment_split_mode": "words", "extensions.segmented_reply_enabled": true}, Help: "逐项添加分隔词；发送时保留命中的分隔词。"},
		{Key: "extensions.segment_cleanup_regex", Group: "extensions", Label: "内容过滤正则表达式", Type: "string", Default: "", DisplayIf: map[string]any{"extensions.segmented_reply_enabled": true}, Help: "在拆分后移除匹配文本。"},
		// 群聊历史记录所有入站群消息，包括未唤醒消息；仅在触发 AI 时注入同一 UMO 的有界历史。
		{Key: "extensions.group_context_enabled", Group: "extensions", Label: "群聊上下文感知", Type: "boolean", Default: false, Help: "记录同一群来源的近期消息，并在触发 AI 时提供给模型。"},
		{Key: "extensions.group_message_max_count", Group: "extensions", Label: "最多注入群消息数", Type: "integer", Default: 300, Min: floatPtr(1), Max: floatPtr(300), DisplayIf: map[string]any{"extensions.group_context_enabled": true}},
		{Key: "extensions.group_image_caption", Group: "extensions", Label: "自动理解群图片", Type: "boolean", Default: false, DisplayIf: map[string]any{"extensions.group_context_enabled": true}, Help: "群图片转述需要选择支持图片的模型。"},
		{Key: "extensions.group_image_caption_model", Group: "extensions", Label: "群图片转述模型", Type: "string", Default: "", DisplayIf: map[string]any{"extensions.group_image_caption": true, "extensions.group_context_enabled": true}, Help: "从已配置模型中选择；与普通图片降级模型独立。"},
		// 主动回复只在未明确唤醒的群消息上抽样，来源白名单使用 /sid 显示的 UMO。
		{Key: "extensions.proactive_reply_enabled", Group: "extensions", Label: "主动回复", Type: "boolean", Default: false, Help: "按概率回复未唤醒的群消息。"},
		{Key: "extensions.proactive_reply_method", Group: "extensions", Label: "主动回复方法", Type: "select", Default: "possibility_reply", Options: []SchemaOption{{Value: "possibility_reply", Label: "概率回复"}}, DisplayIf: map[string]any{"extensions.proactive_reply_enabled": true}},
		{Key: "extensions.proactive_reply_probability", Group: "extensions", Label: "主动回复概率", Type: "number", Default: 0.1, Min: floatPtr(0), Max: floatPtr(1), DisplayIf: map[string]any{"extensions.proactive_reply_enabled": true}},
		{Key: "extensions.proactive_reply_whitelist", Group: "extensions", Label: "主动回复白名单", Type: "list", Default: []string{}, DisplayIf: map[string]any{"extensions.proactive_reply_enabled": true}, Help: "逐项添加 UMO 或群 ID；留空允许所有群。"},
		{Key: "memory.enabled", Group: "memory", Label: "启用长期记忆", Type: "boolean", Default: false, Help: "开启后 Agent 可以检索并在用户明确要求时保存跨会话记忆。"},
		{Key: "memory.auto_retrieve", Group: "memory", Label: "每轮自动检索", Type: "boolean", Default: false, DisplayIf: map[string]any{"memory.enabled": true}, Help: "每轮请求自动把相关记忆放入提示词；关闭时仍可由 Agent 按需检索。"},
		{Key: "memory.max_results", Group: "memory", Label: "最多检索记忆数", Type: "integer", Default: 8, Min: floatPtr(1), Max: floatPtr(50), DisplayIf: map[string]any{"memory.enabled": true}, Help: "限制单轮注入或返回给 Agent 的记忆数量。"},
	}}
}

func runtimeFromValues(values Values) Runtime {
	return Runtime{
		AIEnabled:                     boolOr(values["ai.enabled"], true),
		AIExecutionMode:               stringOrDefault(values["ai.execution_mode"], BuiltinAIExecutionMode),
		ProviderID:                    stringOr(values["ai.default_provider_id"]),
		ModelID:                       stringOr(values["ai.default_model_id"]),
		AITemperature:                 numberOr(values["ai.temperature"], 0.7),
		AIReasoningEffort:             stringOr(values["ai.reasoning_effort"]),
		AITopP:                        numberOr(values["ai.top_p"], 1.0),
		AIMaxOutputTokens:             intOr(values["ai.max_output_tokens"], 0),
		AIRequestRetries:              intOr(values["ai.request_retries"], 2),
		PersonaID:                     stringOr(values["persona.id"]),
		Instruction:                   stringOrDefault(values["persona.system_prompt"], "你是 Abot，一个可靠、简洁、遵守用户意图的中文 AI 助手。"),
		CompactionEnabled:             boolOr(values["context.compaction.enabled"], true),
		CompactionRatio:               numberOr(values["context.compaction.trigger_ratio"], 0.8),
		CompactionSafetyTokens:        intOr(values["context.compaction.safety_tokens"], 512),
		CompactionRetentionEvents:     intOr(values["context.compaction.retention_events"], 10),
		CompactionInterval:            intOr(values["context.compaction.sliding_interval"], 0),
		CompactionOverlap:             intOr(values["context.compaction.sliding_overlap"], 0),
		CompactionUnknownWindowTokens: intOr(values["context.compaction.unknown_window_tokens"], 8192),
		AgentMaxToolCalls:             intOr(values["agent.max_tool_calls"], 20),
		ToolSchemaBudgetTokens:        intOr(values["agent.tool_schema_budget_tokens"], 4096),
		WorkspaceEnabled:              boolOr(values["workspace.enabled"], true),
		WorkspaceReadEnabled:          boolOr(values["workspace.read_enabled"], true),
		WorkspaceWriteEnabled:         boolOr(values["workspace.write_enabled"], true),
		WorkspaceExecEnabled:          boolOr(values["workspace.exec_enabled"], true),
		WorkspaceGitEnabled:           boolOr(values["workspace.git_enabled"], true),
		WorkspaceCommandTimeoutSecs:   intOr(values["workspace.command_timeout_seconds"], 60),
		MessageStreamingEnabled:       boolOr(values["message.streaming_enabled"], true),
		MessagePromptPrefix:           stringOr(values["message.prompt_prefix"]),
		PlatformAdminIDs:              stringListOr(values["platform.admin_ids"]),
		WakeupWords:                   stringListOr(values["platform.wakeup_words"]),
		PrivateRequiresWakeup:         boolOr(values["platform.private_requires_wakeup"], false),
		Platform: PlatformSettings{
			UniqueSession: boolOr(values["platform.unique_session"], false), ReplyPrefix: stringOr(values["platform.reply_prefix"]),
			ReplyMention: boolOr(values["platform.reply_mention"], false), ReplyQuote: boolOr(values["platform.reply_quote"], false),
			PrivateReplyQuote: boolOr(values["platform.private_reply_quote"], false),
			WhitelistEnabled:  boolOr(values["platform.whitelist_enabled"], true), WhitelistIDs: stringListOr(values["platform.whitelist_ids"]),
			WhitelistLog: boolOr(values["platform.whitelist_log"], true), WhitelistAdminGroup: boolOr(values["platform.whitelist_admin_group"], true),
			WhitelistAdminPrivate: boolOr(values["platform.whitelist_admin_private"], true), RateLimitSeconds: intOr(values["platform.rate_limit_seconds"], 60),
			RateLimitCount: intOr(values["platform.rate_limit_count"], 30), RateLimitStrategy: stringOrDefault(values["platform.rate_limit_strategy"], "stall"),
			IgnoreBotSelfMessage: boolOr(values["platform.ignore_bot_self_message"], false), IgnoreAtAll: boolOr(values["platform.ignore_at_all"], false),
			DisableBuiltinCommands: boolOr(values["platform.disable_builtin_commands"], false), NoPermissionReply: boolOr(values["platform.no_permission_reply"], true),
			EmptyMentionWaiting: boolOr(values["platform.empty_mention_waiting"], true), EmptyMentionNeedReply: boolOr(values["platform.empty_mention_need_reply"], true),
			BlockPatterns: stringListOr(values["platform.block_patterns"]), CheckResponse: boolOr(values["platform.check_response"], false),
			TelegramPreAckEnabled: boolOr(values["platform.telegram_pre_ack_enabled"], false), TelegramPreAckEmoji: stringOrDefault(values["platform.telegram_pre_ack_emoji"], "👀"),
		},
		Extensions: ExtensionSettings{
			SegmentedReplyEnabled: boolOr(values["extensions.segmented_reply_enabled"], false), SegmentOnlyLLM: boolOr(values["extensions.segment_only_llm"], true),
			SegmentIntervalMethod: stringOrDefault(values["extensions.segment_interval_method"], "random"), SegmentInterval: stringOrDefault(values["extensions.segment_interval"], "1.5,3.5"),
			SegmentLogBase: numberOr(values["extensions.segment_log_base"], 2.6), SegmentWordsThreshold: intOr(values["extensions.segment_words_threshold"], 150),
			SegmentSplitMode: stringOrDefault(values["extensions.segment_split_mode"], "regex"), SegmentRegex: stringOrDefault(values["extensions.segment_regex"], ".*?[。？！~…]+|.+$"),
			SegmentSplitWords: stringListOr(values["extensions.segment_split_words"]), SegmentCleanupRegex: stringOr(values["extensions.segment_cleanup_regex"]),
			GroupContextEnabled: boolOr(values["extensions.group_context_enabled"], false), GroupMessageMaxCount: intOr(values["extensions.group_message_max_count"], 300),
			GroupImageCaption: boolOr(values["extensions.group_image_caption"], false), GroupImageCaptionModel: stringOr(values["extensions.group_image_caption_model"]),
			ProactiveReplyEnabled: boolOr(values["extensions.proactive_reply_enabled"], false), ProactiveReplyMethod: stringOrDefault(values["extensions.proactive_reply_method"], "possibility_reply"),
			ProactiveReplyProbability: numberOr(values["extensions.proactive_reply_probability"], 0.1), ProactiveReplyWhitelist: stringListOr(values["extensions.proactive_reply_whitelist"]),
		},
		MemoryEnabled:       boolOr(values["memory.enabled"], false),
		MemoryAutoRetrieve:  boolOr(values["memory.auto_retrieve"], false),
		MemoryMaxResults:    intOr(values["memory.max_results"], 8),
		WebSearchEnabled:    boolOr(values["ai.web_search.enabled"], false),
		WebSearchServiceIDs: stringListOr(values["ai.web_search.service_ids"]),
	}
}

func (s *Service) normalizeProfile(item Profile) Profile {
	item.ID = strings.TrimSpace(item.ID)
	item.Name = strings.TrimSpace(item.Name)
	if item.Values == nil {
		item.Values = Values{}
	}
	item.Values = s.normalizeListValues(item.Values)
	return item
}

// normalizeListValues 统一配置列表的持久化形状，并兼容早期 textarea 保存的 JSON 字符串。
func (s *Service) normalizeListValues(values Values) Values {
	result := cloneValues(values)
	for _, field := range s.schema.Fields {
		if field.Type != "list" {
			continue
		}
		value, exists := result[field.Key]
		if !exists {
			continue
		}
		if list, ok := stringListValue(value); ok {
			result[field.Key] = list
		}
	}
	return result
}

func validateID(id string) error {
	if id == "" || len(id) > 64 || id[0] < 'a' || id[0] > 'z' {
		return fmt.Errorf("%w: 配置 ID 必须以小写字母开头且不超过 64 个字符", ErrInvalidRequest)
	}
	for _, value := range id {
		if value != '_' && value != '-' && (value < 'a' || value > 'z') && (value < '0' || value > '9') {
			return fmt.Errorf("%w: 配置 ID 只能使用小写字母、数字、_、-", ErrInvalidRequest)
		}
	}
	return nil
}

func (s *Service) nameConflict(ctx context.Context, name, exceptID string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	items, err := s.repository.ListProfiles(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.ID != exceptID && strings.EqualFold(strings.TrimSpace(item.Name), name) {
			return true, nil
		}
	}
	return false, nil
}

func normalizeSystemSettings(settings SystemSettings) SystemSettings {
	if strings.TrimSpace(settings.LogLevel) == "" {
		settings.LogLevel = "info"
	}
	settings.LogLevel = strings.ToLower(strings.TrimSpace(settings.LogLevel))
	settings.ModalFallbackProviderID = strings.TrimSpace(settings.ModalFallbackProviderID)
	settings.ModalFallbackVisionModel = strings.TrimSpace(settings.ModalFallbackVisionModel)
	settings.ModalFallbackAudioModel = strings.TrimSpace(settings.ModalFallbackAudioModel)
	if settings.SubAgentEnabled == nil {
		enabled := true
		settings.SubAgentEnabled = &enabled
	}
	settings.SubAgent = normalizeSubAgentSettings(settings.SubAgent)
	if settings.RequestTimeoutSeconds == 0 {
		settings.RequestTimeoutSeconds = 300
	}
	// 旧记录和全新配置没有任何 Artifact 字段，此时补上保守默认值。用户显式
	// 关闭配额（0 配额加合法保留时长）不会被这里覆盖。
	if settings.ArtifactQuotaBytes == 0 && settings.ArtifactStaleUploadSeconds == 0 {
		settings.ArtifactQuotaBytes = DefaultArtifactQuotaBytes
		settings.ArtifactStaleUploadSeconds = DefaultArtifactStaleUploadSeconds
	}
	// 旧配置没有该字段时按默认周期补齐，保证升级后历史附件也能进入定时回收。
	if settings.ArtifactInputRetentionSeconds == 0 {
		settings.ArtifactInputRetentionSeconds = DefaultArtifactInputRetentionSeconds
	}
	if settings.WebSearchDailyCallLimit == 0 {
		settings.WebSearchDailyCallLimit = 100
	}
	if settings.WebSearchMaxCallsPerInvocation == 0 {
		settings.WebSearchMaxCallsPerInvocation = 8
	}
	if settings.WebSearchAlertPercent == 0 {
		settings.WebSearchAlertPercent = 80
	}
	return settings
}

// normalizeSubAgentSettings 只做稳定化，不补入会改变语义的模型或超时默认值；
// 空模型表示沿用主 Agent，空工具白名单表示由主 Agent 当前只读工具集合决定。
func normalizeSubAgentSettings(value SubAgentSettings) SubAgentSettings {
	value.ProviderID = strings.TrimSpace(value.ProviderID)
	value.ModelID = strings.TrimSpace(value.ModelID)
	value.ReasoningEffort = strings.ToLower(strings.TrimSpace(value.ReasoningEffort))
	if value.Temperature != nil {
		number := *value.Temperature
		value.Temperature = &number
	}
	if value.TopP != nil {
		number := *value.TopP
		value.TopP = &number
	}
	value.AllowedTools = normalizeStringList(value.AllowedTools, 64)
	return value
}

func normalizeStringList(values []string, limit int) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, item := range values {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	sort.Strings(result)
	return result
}

func validateSystemSettings(settings SystemSettings) error {
	switch settings.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("%w: 日志等级必须是 debug、info、warn 或 error", ErrInvalidRequest)
	}
	if settings.RequestTimeoutSeconds < 30 || settings.RequestTimeoutSeconds > 3600 {
		return fmt.Errorf("%w: 全局请求超时必须在 30-3600 秒之间", ErrInvalidRequest)
	}
	if settings.WebSearchDailyCallLimit < 1 || settings.WebSearchDailyCallLimit > 100000 {
		return fmt.Errorf("%w: 网页搜索每日请求上限必须在 1-100000 之间", ErrInvalidRequest)
	}
	if settings.WebSearchMaxCallsPerInvocation < 1 || settings.WebSearchMaxCallsPerInvocation > 100 {
		return fmt.Errorf("%w: 单任务网页搜索上限必须在 1-100 之间", ErrInvalidRequest)
	}
	if settings.WebSearchAlertPercent < 1 || settings.WebSearchAlertPercent > 100 {
		return fmt.Errorf("%w: 网页搜索用量告警比例必须在 1-100 之间", ErrInvalidRequest)
	}
	if settings.ArtifactQuotaBytes < 0 || settings.ArtifactQuotaBytes > MaxArtifactQuotaBytes {
		return fmt.Errorf("%w: Artifact 存储配额必须在 0 到 %d 字节之间", ErrInvalidRequest, MaxArtifactQuotaBytes)
	}
	if settings.ArtifactStaleUploadSeconds < MinArtifactStaleUploadSeconds || settings.ArtifactStaleUploadSeconds > MaxArtifactStaleUploadSeconds {
		return fmt.Errorf("%w: 未完成上传保留时长必须在 %d-%d 秒之间", ErrInvalidRequest, MinArtifactStaleUploadSeconds, MaxArtifactStaleUploadSeconds)
	}
	if settings.ArtifactInputRetentionSeconds < MinArtifactInputRetentionSeconds || settings.ArtifactInputRetentionSeconds > MaxArtifactInputRetentionSeconds {
		return fmt.Errorf("%w: 输入附件保留时长必须在 %d-%d 秒之间", ErrInvalidRequest, MinArtifactInputRetentionSeconds, MaxArtifactInputRetentionSeconds)
	}
	for key, value := range map[string]string{
		"modal_fallback_provider_id":  settings.ModalFallbackProviderID,
		"modal_fallback_vision_model": settings.ModalFallbackVisionModel,
		"modal_fallback_audio_model":  settings.ModalFallbackAudioModel,
	} {
		if len(value) > 128 {
			return fmt.Errorf("%w: %s 长度不能超过 128 个字符", ErrInvalidRequest, key)
		}
	}
	if err := validateSubAgentSettings(settings.SubAgent); err != nil {
		return err
	}
	return nil
}

func validateSubAgentSettings(value SubAgentSettings) error {
	for key, item := range map[string]string{
		"subagent.provider_id": value.ProviderID,
		"subagent.model_id":    value.ModelID,
	} {
		if len(item) > 128 {
			return fmt.Errorf("%w: 通用子 Agent 设置 %s 长度不能超过 128 个字符", ErrInvalidRequest, key)
		}
	}
	switch value.ReasoningEffort {
	case "", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("%w: 通用子 Agent 的思考强度无效", ErrInvalidRequest)
	}
	if value.Temperature != nil && (*value.Temperature < 0 || *value.Temperature > 2 || math.IsNaN(*value.Temperature) || math.IsInf(*value.Temperature, 0)) {
		return fmt.Errorf("%w: 通用子 Agent 的 temperature 必须在 0-2 之间", ErrInvalidRequest)
	}
	if value.TopP != nil && (*value.TopP < 0.01 || *value.TopP > 1 || math.IsNaN(*value.TopP) || math.IsInf(*value.TopP, 0)) {
		return fmt.Errorf("%w: 通用子 Agent 的 top_p 必须在 0.01-1 之间", ErrInvalidRequest)
	}
	if value.MaxOutputTokens < 0 || value.MaxOutputTokens > 32768 {
		return fmt.Errorf("%w: 通用子 Agent 的最大输出 token 必须在 0-32768 之间", ErrInvalidRequest)
	}
	if value.MaxConcurrency < 0 || value.MaxConcurrency > 4 {
		return fmt.Errorf("%w: 通用子 Agent 的并发数必须在 0-4 之间", ErrInvalidRequest)
	}
	if value.InputBudgetBytes < 0 || value.InputBudgetBytes > 512<<10 {
		return fmt.Errorf("%w: 通用子 Agent 的输入预算必须在 0-524288 字节之间", ErrInvalidRequest)
	}
	if value.OutputBudgetBytes < 0 || value.OutputBudgetBytes > 32<<10 {
		return fmt.Errorf("%w: 通用子 Agent 的输出预算必须在 0-32768 字节之间", ErrInvalidRequest)
	}
	if len(value.AllowedTools) > 64 {
		return fmt.Errorf("%w: 通用子 Agent 的工具白名单不能超过 64 个", ErrInvalidRequest)
	}
	for _, name := range value.AllowedTools {
		if len(name) > 128 {
			return fmt.Errorf("%w: 通用子 Agent 工具名称长度不能超过 128 个字符", ErrInvalidRequest)
		}
	}
	return nil
}

func cloneValues(values Values) Values {
	result := make(Values, len(values))
	for key, value := range values {
		result[key] = cloneValue(value)
	}
	return result
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = cloneValue(child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = cloneValue(child)
		}
		return result
	case []string:
		// []string 是列表 Schema 的默认值，需要独立复制避免草稿和默认值共享底层数组。
		return append([]string(nil), typed...)
	default:
		return value
	}
}

// stringListValue 将数组或旧版 JSON 数组字符串转换为去空白、去重后的字符串列表。
func stringListValue(value any) ([]string, bool) {
	var raw []any
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			raw = append(raw, item)
		}
	case []any:
		raw = typed
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return []string{}, true
		}
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			return nil, false
		}
	case nil:
		return []string{}, true
	default:
		return nil, false
	}
	result := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if len(text) > 256 {
			return nil, false
		}
		if _, exists := seen[text]; exists {
			continue
		}
		seen[text] = struct{}{}
		result = append(result, text)
	}
	if len(result) > 256 {
		return nil, false
	}
	return result, true
}

func stringListOr(value any) []string {
	list, ok := stringListValue(value)
	if !ok {
		return nil
	}
	return list
}

func cloneSchema(schema Schema) Schema {
	schema.Fields = append([]Field(nil), schema.Fields...)
	for index := range schema.Fields {
		schema.Fields[index].Options = append([]SchemaOption(nil), schema.Fields[index].Options...)
		if schema.Fields[index].DisplayIf != nil {
			schema.Fields[index].DisplayIf = cloneValues(schema.Fields[index].DisplayIf)
		}
	}
	return schema
}

func boolValue(value any) (bool, bool) {
	result, ok := value.(bool)
	return result, ok
}

func boolOr(value any, fallback bool) bool {
	if result, ok := boolValue(value); ok {
		return result
	}
	return fallback
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case jsonNumber:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

// jsonNumber 是一个很小的适配接口，避免配置包必须暴露 encoding/json 的实现类型。
type jsonNumber interface{ Float64() (float64, error) }

func integerValue(value any) (int, bool) {
	number, ok := numberValue(value)
	if !ok || math.Trunc(number) != number || number < float64(-int(^uint(0)>>1)-1) || number > float64(int(^uint(0)>>1)) {
		return 0, false
	}
	return int(number), true
}

func intOr(value any, fallback int) int {
	if result, ok := integerValue(value); ok {
		return result
	}
	return fallback
}

func numberOr(value any, fallback float64) float64 {
	if result, ok := numberValue(value); ok {
		return result
	}
	return fallback
}

func stringOr(value any) string {
	result, _ := value.(string)
	return strings.TrimSpace(result)
}

func stringOrDefault(value any, fallback string) string {
	if result := stringOr(value); result != "" {
		return result
	}
	return fallback
}

func nameOr(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func floatPtr(value float64) *float64 { return &value }

func newID(prefix string) string {
	var buffer [6]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buffer[:])
}
