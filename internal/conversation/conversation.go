// Package conversation 提供对话元数据和生命周期管理。
package conversation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/session"
)

var (
	ErrNotFound              = errors.New("对话不存在")
	ErrInvalidRequest        = errors.New("对话请求无效")
	ErrArchived              = errors.New("对话已归档")
	ErrDeleteRequiresArchive = errors.New("只能删除已归档的对话")
)

// Status 是对话生命周期状态。删除后不会留下任何状态记录。
type Status string

const (
	StatusActive   Status = "active"
	StatusArchived Status = "archived"
)

// Conversation 保存对话元数据；WorkspaceID 为空表示普通对话。
type Conversation struct {
	ID      string `json:"id"`
	AppName string `json:"app_name"`
	UserID  string `json:"user_id"`
	// Source 保存平台消息的稳定来源标识，供会话规则按 UMO 命中；普通 WebUI 对话为空。
	Source string `json:"source,omitempty"`
	// SourceName 是 /name 为平台来源设置的可读别名；它不改变稳定来源键。
	SourceName  string `json:"source_name,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	// ProviderID and ModelID are an optional per-conversation model override.
	// Empty means "inherit whatever the configuration bindings resolve", so a
	// chat can switch models without touching the bot or the global default.
	ProviderID string     `json:"provider_id,omitempty"`
	ModelID    string     `json:"model_id,omitempty"`
	Title      string     `json:"title"`
	Status     Status     `json:"status"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Message 是聊天历史的公开视图；附件正文仍由 ADK 事件存储，列表只返回数量和元信息。
type Message struct {
	Role            string    `json:"role"`
	Text            string    `json:"text,omitempty"`
	AttachmentCount int       `json:"attachment_count,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
}

// ContextStatus 是会话上下文的可见摘要；压缩不会删除原始消息，只在这里报告内部状态。
type ContextStatus struct {
	Compressed       bool       `json:"compressed"`
	CompactionCount  int        `json:"compaction_count"`
	LastCompactionAt *time.Time `json:"last_compaction_at,omitempty"`
	CoveredFrom      *time.Time `json:"covered_from,omitempty"`
	CoveredTo        *time.Time `json:"covered_to,omitempty"`
}

// CreateRequest 是创建普通对话或工作区对话的输入。
type CreateRequest struct {
	ID          string
	AppName     string
	UserID      string
	Source      string
	WorkspaceID string
	Title       string
}

// Repository 是对话元数据存储接口。
type Repository interface {
	List(context.Context, string, string, bool) ([]Conversation, error)
	Get(context.Context, string, string) (Conversation, error)
	GetByID(context.Context, string) (Conversation, error)
	CountByWorkspace(context.Context, string) (int, error)
	Save(context.Context, Conversation) error
	Delete(context.Context, string, string) error
}

// AllLister 是管理台读取实例级会话的可选仓储扩展。
// 普通对话接口仍然必须携带 user_id；只有明确的管理查询才允许使用该扩展。
type AllLister interface {
	ListAll(context.Context, string, bool) ([]Conversation, error)
}

// TotalCounter 是仓储的可选统计扩展，用于总览页展示实例内的会话数量。
// 采用可选接口不改变第三方内存仓储的最小实现契约。
type TotalCounter interface {
	CountAll(context.Context, bool) (int, error)
}

// SourceNameResolver 是会话列表读取来源别名的可选边界，避免会话包依赖 Bot 实现。
type SourceNameResolver interface {
	GetSourceName(context.Context, string) (string, error)
}

// Service 管理对话与 ADK Session 的关系。Conversation ID 与 ADK Session ID 一致，
// 这样可以保证删除对话时能准确清理底层消息事件。
type Service struct {
	repository         Repository
	sessions           session.Service
	appName            string
	artifactDeletion   func(context.Context, string) error
	workspaceValidator WorkspaceValidator
	sourceNameResolver SourceNameResolver
	mu                 sync.Mutex
	sourceNameMu       sync.RWMutex
}

// NewService 创建对话服务。
func NewService(repository Repository, sessions session.Service, appName string) (*Service, error) {
	if repository == nil {
		return nil, errors.New("对话 Repository 不能为空")
	}
	if strings.TrimSpace(appName) == "" {
		appName = "abot"
	}
	return &Service{repository: repository, sessions: sessions, appName: strings.TrimSpace(appName)}, nil
}

// SetArtifactDeletionHook wires the physical Artifact cleanup into the
// conversation lifecycle without making the conversation package depend on a
// concrete storage implementation. The hook runs before Session and metadata
// deletion so an unavailable object store aborts the destructive operation.
func (s *Service) SetArtifactDeletionHook(hook func(context.Context, string) error) {
	s.artifactDeletion = hook
}

// SetSourceNameResolver 让管理台读取 /name 的来源别名；未安装时保持原有会话结构不变。
func (s *Service) SetSourceNameResolver(resolver SourceNameResolver) {
	s.sourceNameMu.Lock()
	s.sourceNameResolver = resolver
	s.sourceNameMu.Unlock()
}

// List 列出用户的对话；workspaceID 为空时列出普通对话，传入 "*" 时列出所有对话。
func (s *Service) List(ctx context.Context, userID, workspaceID string, includeArchived bool) ([]Conversation, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id 不能为空", ErrInvalidRequest)
	}
	items, err := s.repository.List(ctx, userID, strings.TrimSpace(workspaceID), includeArchived)
	if err != nil {
		return nil, err
	}
	for index := range items {
		if err := s.enrichSourceName(ctx, &items[index]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// ListAll 列出当前实例的全部会话，供管理台和来源目录使用。
// 通过可选接口保留第三方仓储的最小实现契约，避免普通用户查询意外越权。
func (s *Service) ListAll(ctx context.Context, workspaceID string, includeArchived bool) ([]Conversation, error) {
	lister, ok := s.repository.(AllLister)
	if !ok {
		return nil, errors.New("对话 Repository 不支持实例级列表")
	}
	items, err := lister.ListAll(ctx, strings.TrimSpace(workspaceID), includeArchived)
	if err != nil {
		return nil, err
	}
	for index := range items {
		if err := s.enrichSourceName(ctx, &items[index]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// CountByWorkspace 返回工作区中仍保留的对话数，活跃和已归档都计算在内。
func (s *Service) CountByWorkspace(ctx context.Context, workspaceID string) (int, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return 0, fmt.Errorf("%w: workspace_id 不能为空", ErrInvalidRequest)
	}
	return s.repository.CountByWorkspace(ctx, workspaceID)
}

// CountAll 返回当前实例保留的会话数；includeArchived=false 时只统计活跃对话。
func (s *Service) CountAll(ctx context.Context, includeArchived bool) (int, error) {
	counter, ok := s.repository.(TotalCounter)
	if !ok {
		return 0, nil
	}
	return counter.CountAll(ctx, includeArchived)
}

// Get 读取属于指定用户的对话，避免仅凭 ID 访问其他用户的会话。
func (s *Service) Get(ctx context.Context, userID, id string) (Conversation, error) {
	userID = strings.TrimSpace(userID)
	id = strings.TrimSpace(id)
	if userID == "" || id == "" {
		return Conversation{}, fmt.Errorf("%w: user_id 和 conversation_id 不能为空", ErrInvalidRequest)
	}
	item, err := s.repository.Get(ctx, userID, id)
	if err != nil {
		return Conversation{}, err
	}
	if item.AppName == "" {
		item.AppName = s.appName
	}
	if err := s.enrichSourceName(ctx, &item); err != nil {
		return Conversation{}, err
	}
	return item, nil
}

// Create 创建一个对话。工作区是否存在及是否可用由 HTTP/Workspace 层在调用前校验。
func (s *Service) Create(ctx context.Context, request CreateRequest) (Conversation, error) {
	request.AppName = strings.TrimSpace(request.AppName)
	if request.AppName == "" {
		request.AppName = s.appName
	}
	request.UserID = strings.TrimSpace(request.UserID)
	request.ID = strings.TrimSpace(request.ID)
	request.Source = strings.TrimSpace(request.Source)
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	request.Title = strings.TrimSpace(request.Title)
	if request.UserID == "" {
		return Conversation{}, fmt.Errorf("%w: user_id 不能为空", ErrInvalidRequest)
	}
	if request.ID == "" {
		request.ID = newID("conversation")
	}
	if request.Title == "" {
		request.Title = "新对话"
	}
	now := time.Now().UTC()
	item := Conversation{
		ID: request.ID, AppName: request.AppName, UserID: request.UserID,
		Source: request.Source, WorkspaceID: request.WorkspaceID, Title: request.Title,
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if s.sessions != nil {
		if _, err := s.sessions.Create(ctx, &session.CreateRequest{AppName: item.AppName, UserID: item.UserID, SessionID: item.ID}); err != nil {
			return Conversation{}, fmt.Errorf("创建对话 Session 失败: %w", err)
		}
	}
	if err := s.repository.Save(ctx, item); err != nil {
		if s.sessions != nil {
			_ = s.sessions.Delete(ctx, &session.DeleteRequest{AppName: item.AppName, UserID: item.UserID, SessionID: item.ID})
		}
		return Conversation{}, err
	}
	if err := s.enrichSourceName(ctx, &item); err != nil {
		return Conversation{}, err
	}
	return item, nil
}

// enrichSourceName 只补充展示字段，任何持久化写入仍以 Source 稳定键为准。
func (s *Service) enrichSourceName(ctx context.Context, item *Conversation) error {
	if s == nil || item == nil || strings.TrimSpace(item.Source) == "" {
		return nil
	}
	s.sourceNameMu.RLock()
	resolver := s.sourceNameResolver
	s.sourceNameMu.RUnlock()
	if resolver == nil {
		return nil
	}
	name, err := resolver.GetSourceName(ctx, item.Source)
	if err != nil {
		return fmt.Errorf("读取来源显示名称失败: %w", err)
	}
	item.SourceName = strings.TrimSpace(name)
	return nil
}

// Messages 读取对话历史，归档对话仍然允许查看，但已删除对话无法读取。
func (s *Service) Messages(ctx context.Context, userID, id string) ([]Message, error) {
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if s.sessions == nil {
		return []Message{}, nil
	}
	response, err := s.sessions.Get(ctx, &session.GetRequest{AppName: item.AppName, UserID: item.UserID, SessionID: item.ID})
	if err != nil {
		return nil, fmt.Errorf("读取对话消息失败: %w", err)
	}
	result := make([]Message, 0)
	for event := range response.Session.Events().All() {
		// EventCompaction 是上下文管理的内部记录，不属于用户可见聊天消息。
		if event == nil || event.Actions.Compaction != nil || event.Content == nil || event.Partial {
			continue
		}
		message := Message{Role: "assistant", Timestamp: event.Timestamp}
		if event.Author == "user" || event.Content.Role == "user" {
			message.Role = "user"
		}
		for _, part := range event.Content.Parts {
			if part == nil {
				continue
			}
			message.Text += part.Text
			if part.InlineData != nil {
				message.AttachmentCount++
			}
			if part.PartMetadata != nil {
				if _, ok := part.PartMetadata["abot_attachment_ref"]; ok {
					message.AttachmentCount++
				}
			}
		}
		if strings.TrimSpace(message.Text) == "" && message.AttachmentCount == 0 {
			continue
		}
		result = append(result, message)
	}
	return result, nil
}

// ContextStatus 返回会话压缩状态，调试信息只包含时间范围，不返回内部摘要正文。
func (s *Service) ContextStatus(ctx context.Context, userID, id string) (ContextStatus, error) {
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return ContextStatus{}, err
	}
	status := ContextStatus{}
	if s.sessions == nil {
		return status, nil
	}
	response, err := s.sessions.Get(ctx, &session.GetRequest{AppName: item.AppName, UserID: item.UserID, SessionID: item.ID})
	if err != nil {
		return ContextStatus{}, fmt.Errorf("读取会话压缩状态失败: %w", err)
	}
	for event := range response.Session.Events().All() {
		if event == nil || event.Actions.Compaction == nil {
			continue
		}
		compaction := event.Actions.Compaction
		status.Compressed = true
		status.CompactionCount++
		if status.LastCompactionAt == nil || event.Timestamp.After(*status.LastCompactionAt) {
			at := event.Timestamp
			status.LastCompactionAt = &at
			from, to := compaction.StartTimestamp, compaction.EndTimestamp
			status.CoveredFrom = &from
			status.CoveredTo = &to
		}
	}
	return status, nil
}

// WorkspaceValidator checks that a workspace may be bound to a conversation.
// It is injected by the application so the conversation service does not depend
// on the workspace service.
type WorkspaceValidator func(context.Context, string) error

// SetWorkspaceValidator installs the optional workspace existence check.
func (s *Service) SetWorkspaceValidator(validator WorkspaceValidator) {
	s.mu.Lock()
	s.workspaceValidator = validator
	s.mu.Unlock()
}

// SetWorkspace binds or clears the workspace of one conversation. An empty
// workspace id clears the binding. A bound workspace is validated first so a
// conversation cannot point at a workspace that no longer exists.
func (s *Service) SetWorkspace(ctx context.Context, userID, id, workspaceID string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return Conversation{}, err
	}
	if item.Status != StatusActive {
		return Conversation{}, fmt.Errorf("%w: 当前状态为 %s", ErrInvalidRequest, item.Status)
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID != "" && s.workspaceValidator != nil {
		if err := s.workspaceValidator(ctx, workspaceID); err != nil {
			return Conversation{}, fmt.Errorf("%w: 工作区不可用", ErrInvalidRequest)
		}
	}
	if item.WorkspaceID == workspaceID {
		return item, nil
	}
	item.WorkspaceID = workspaceID
	item.UpdatedAt = time.Now().UTC()
	if err := s.repository.Save(ctx, item); err != nil {
		return Conversation{}, err
	}
	return item, nil
}

// SetRuntimeOverride stores or clears the per-conversation model override.
// Empty values clear the override and restore inheritance.
func (s *Service) SetRuntimeOverride(ctx context.Context, userID, id, providerID, modelID string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return Conversation{}, err
	}
	if item.Status != StatusActive {
		return Conversation{}, fmt.Errorf("%w: 当前状态为 %s", ErrInvalidRequest, item.Status)
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if len(providerID) > 100 || len(modelID) > 200 {
		return Conversation{}, fmt.Errorf("%w: 模型标识过长", ErrInvalidRequest)
	}
	if item.ProviderID == providerID && item.ModelID == modelID {
		return item, nil
	}
	item.ProviderID = providerID
	item.ModelID = modelID
	item.UpdatedAt = time.Now().UTC()
	if err := s.repository.Save(ctx, item); err != nil {
		return Conversation{}, err
	}
	return item, nil
}

// Archive 将活跃对话归档；归档保留全部信息，但停止聊天和工作区操作。
func (s *Service) Archive(ctx context.Context, userID, id string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return Conversation{}, err
	}
	if item.Status != StatusActive {
		return Conversation{}, fmt.Errorf("%w: 当前状态为 %s", ErrInvalidRequest, item.Status)
	}
	now := time.Now().UTC()
	item.Status = StatusArchived
	item.ArchivedAt = &now
	item.UpdatedAt = now
	if err := s.repository.Save(ctx, item); err != nil {
		return Conversation{}, err
	}
	return item, nil
}

// Unarchive 恢复归档对话。
func (s *Service) Unarchive(ctx context.Context, userID, id string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return Conversation{}, err
	}
	if item.Status != StatusArchived {
		return Conversation{}, fmt.Errorf("%w: 当前状态为 %s", ErrInvalidRequest, item.Status)
	}
	now := time.Now().UTC()
	item.Status = StatusActive
	item.ArchivedAt = nil
	item.UpdatedAt = now
	if err := s.repository.Save(ctx, item); err != nil {
		return Conversation{}, err
	}
	return item, nil
}

// Delete 只允许删除已归档对话，并先清除 ADK Session，再清除对话元数据和关联操作。
// 删除不是软删除：成功后不再存在可查询的对话记录。
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	if item.Status != StatusArchived {
		return fmt.Errorf("%w: 当前状态为 %s", ErrDeleteRequiresArchive, item.Status)
	}
	if s.artifactDeletion != nil {
		if err := s.artifactDeletion(ctx, item.ID); err != nil {
			return fmt.Errorf("删除对话 Artifact 失败: %w", err)
		}
	}
	if s.sessions != nil {
		if err := s.sessions.Delete(ctx, &session.DeleteRequest{AppName: item.AppName, UserID: item.UserID, SessionID: item.ID}); err != nil {
			return fmt.Errorf("删除对话消息失败: %w", err)
		}
	}
	if err := s.repository.Delete(ctx, item.UserID, item.ID); err != nil {
		return fmt.Errorf("删除对话元数据失败: %w", err)
	}
	return nil
}

// IsActive 供工作区审批层判断某个对话是否仍可执行操作。
func (s *Service) IsActive(ctx context.Context, userID, id string) (bool, error) {
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return false, err
	}
	return item.Status == StatusActive, nil
}

// IsActiveByID 供工作区审批回调使用；操作记录已保存对话 ID，不需要暴露用户信息。
func (s *Service) IsActiveByID(ctx context.Context, id string) (bool, error) {
	item, err := s.repository.GetByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return false, err
	}
	return item.Status == StatusActive, nil
}

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(value[:])
}
