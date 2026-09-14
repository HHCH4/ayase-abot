// Package memory 提供 Abot 的长期记忆领域服务。
//
// 记忆默认不自动从聊天中抽取，只有用户通过管理 API 或 Agent 明确调用保存工具时
// 才会写入，避免把普通聊天内容悄悄变成长期数据。
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

const (
	defaultAppName     = "abot"
	defaultListLimit   = 50
	defaultSearchLimit = 20
	maxContentLength   = 100000
	maxTagsLength      = 1000
)

var (
	ErrNotFound       = errors.New("记忆不存在")
	ErrInvalidRequest = errors.New("记忆请求无效")
	ErrConflict       = errors.New("记忆冲突")
)

// Item 是一条可被用户管理的长期记忆。Content 只保存文本，二进制附件仍留在聊天会话中。
type Item struct {
	ID             string    `json:"id"`
	AppName        string    `json:"app_name,omitempty"`
	UserID         string    `json:"user_id"`
	ConversationID string    `json:"conversation_id,omitempty"`
	SourceEventID  string    `json:"-"`
	Author         string    `json:"author,omitempty"`
	Source         string    `json:"source,omitempty"`
	Content        string    `json:"content"`
	Tags           string    `json:"tags,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Repository 是长期记忆的持久化接口，SQLite 实现位于 storage/sqlite。
type Repository interface {
	List(context.Context, string, string, string, int) ([]Item, error)
	Get(context.Context, string, string, string) (Item, error)
	Save(context.Context, Item) error
	Delete(context.Context, string, string, string) error
	Clear(context.Context, string, string) error
	DeleteByConversation(context.Context, string) error
	Count(context.Context, string, string) (int, error)
}

// Service 同时实现管理 API 需要的业务方法和 ADK memory.Service。
type Service struct {
	repository Repository
	appName    string
}

// NewService 创建长期记忆服务。appName 为空时使用 Abot 默认应用名。
func NewService(repository Repository, appNames ...string) (*Service, error) {
	if repository == nil {
		return nil, errors.New("记忆 Repository 不能为空")
	}
	appName := defaultAppName
	if len(appNames) > 0 && strings.TrimSpace(appNames[0]) != "" {
		appName = strings.TrimSpace(appNames[0])
	}
	return &Service{repository: repository, appName: appName}, nil
}

// AppName 返回当前 Agent 使用的应用命名空间。
func (s *Service) AppName() string { return s.appName }

// List 返回指定用户的记忆，query 为空时按更新时间倒序返回。
func (s *Service) List(ctx context.Context, userID, query string, limit int) ([]Item, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id 不能为空", ErrInvalidRequest)
	}
	return s.repository.List(ctx, userID, s.appName, strings.TrimSpace(query), normalizeLimit(limit, defaultListLimit))
}

// Get 读取属于指定用户的一条记忆，防止仅凭 ID 访问其他用户数据。
func (s *Service) Get(ctx context.Context, userID, id string) (Item, error) {
	userID = strings.TrimSpace(userID)
	id = strings.TrimSpace(id)
	if userID == "" || id == "" {
		return Item{}, fmt.Errorf("%w: user_id 和 id 不能为空", ErrInvalidRequest)
	}
	return s.repository.Get(ctx, userID, s.appName, id)
}

// Save 保存或更新一条记忆。带有 ID 的请求用于更新；没有 ID 时生成新 ID。
func (s *Service) Save(ctx context.Context, item Item) (Item, error) {
	item.AppName = s.appName
	item.UserID = strings.TrimSpace(item.UserID)
	item.ID = strings.TrimSpace(item.ID)
	item.ConversationID = strings.TrimSpace(item.ConversationID)
	item.Author = strings.TrimSpace(item.Author)
	item.Source = strings.TrimSpace(item.Source)
	item.Content = strings.TrimSpace(item.Content)
	item.Tags = strings.TrimSpace(item.Tags)
	if item.UserID == "" {
		return Item{}, fmt.Errorf("%w: user_id 不能为空", ErrInvalidRequest)
	}
	if item.Content == "" {
		return Item{}, fmt.Errorf("%w: content 不能为空", ErrInvalidRequest)
	}
	if len([]rune(item.Content)) > maxContentLength {
		return Item{}, fmt.Errorf("%w: content 不能超过 %d 个字符", ErrInvalidRequest, maxContentLength)
	}
	if len([]rune(item.Tags)) > maxTagsLength {
		return Item{}, fmt.Errorf("%w: tags 不能超过 %d 个字符", ErrInvalidRequest, maxTagsLength)
	}
	if item.ID == "" {
		item.ID = newID()
	} else if old, err := s.repository.Get(ctx, item.UserID, s.appName, item.ID); err == nil {
		// 更新时保留原始创建时间和来源信息，避免编辑记忆改变审计语义。
		item.CreatedAt = old.CreatedAt
		if item.Source == "" {
			item.Source = old.Source
		}
		if item.Author == "" {
			item.Author = old.Author
		}
	} else if !errors.Is(err, ErrNotFound) {
		return Item{}, err
	}
	if item.Source == "" {
		item.Source = "manual"
	}
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	if err := s.repository.Save(ctx, item); err != nil {
		return Item{}, fmt.Errorf("保存记忆失败: %w", err)
	}
	return item, nil
}

// Update 只允许更新指定用户的记忆正文和标签，保留来源与创建时间。
func (s *Service) Update(ctx context.Context, userID, id, content, tags string) (Item, error) {
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return Item{}, err
	}
	item.Content = content
	item.Tags = tags
	return s.Save(ctx, item)
}

// Delete 物理删除一条记忆。
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	userID = strings.TrimSpace(userID)
	id = strings.TrimSpace(id)
	if userID == "" || id == "" {
		return fmt.Errorf("%w: user_id 和 id 不能为空", ErrInvalidRequest)
	}
	if err := s.repository.Delete(ctx, userID, s.appName, id); err != nil {
		return err
	}
	return nil
}

// Clear 物理删除指定用户在当前应用下的全部记忆。
func (s *Service) Clear(ctx context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("%w: user_id 不能为空", ErrInvalidRequest)
	}
	return s.repository.Clear(ctx, userID, s.appName)
}

// Count 返回指定用户的记忆数量，供管理页面或总览扩展使用。
func (s *Service) Count(ctx context.Context, userID string) (int, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0, fmt.Errorf("%w: user_id 不能为空", ErrInvalidRequest)
	}
	return s.repository.Count(ctx, userID, s.appName)
}

// SearchMemory 实现 ADK 的跨会话记忆检索接口。
func (s *Service) SearchMemory(ctx context.Context, request *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	if request == nil {
		return nil, fmt.Errorf("%w: memory search request 不能为空", ErrInvalidRequest)
	}
	userID := strings.TrimSpace(request.UserID)
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id 不能为空", ErrInvalidRequest)
	}
	appName := strings.TrimSpace(request.AppName)
	if appName == "" {
		appName = s.appName
	}
	items, err := s.repository.List(ctx, userID, appName, strings.TrimSpace(request.Query), defaultSearchLimit)
	if err != nil {
		return nil, fmt.Errorf("检索记忆失败: %w", err)
	}
	response := &adkmemory.SearchResponse{Memories: make([]adkmemory.Entry, 0, len(items))}
	for _, item := range items {
		response.Memories = append(response.Memories, memoryEntry(item))
	}
	return response, nil
}

// WithLimit 为每次 Agent 运行包一层检索结果上限；配置变化后下次运行即可生效。
func (s *Service) WithLimit(limit int) adkmemory.Service {
	return &limitedService{inner: s, limit: normalizeLimit(limit, defaultSearchLimit)}
}

// Tools 创建 Agent 可按需调用的记忆保存工具。工具描述要求模型只有在用户明确要求时才保存。
func (s *Service) Tools() ([]tool.Tool, error) {
	saveTool, err := functiontool.New[saveMemoryArgs, saveMemoryResult](functiontool.Config{
		Name:        "memory_request_save",
		Description: "仅当用户明确要求记住、保存或长期保留某条信息时，保存这条长期记忆；普通聊天不要调用。",
	}, func(ctx adkagent.Context, args saveMemoryArgs) (saveMemoryResult, error) {
		item, saveErr := s.Save(ctx, Item{
			UserID:         ctx.UserID(),
			ConversationID: ctx.SessionID(),
			Author:         "user",
			Source:         "agent",
			Content:        args.Content,
			Tags:           args.Tags,
		})
		if saveErr != nil {
			return saveMemoryResult{}, saveErr
		}
		return saveMemoryResult{ID: item.ID, Message: "长期记忆已保存"}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("创建记忆工具失败: %w", err)
	}
	return []tool.Tool{saveTool}, nil
}

// AddSessionToMemory 把指定会话的完整文本事件显式导入长期记忆。
// Agent 当前不会自动调用它，避免用户未同意时扩大数据留存范围。
func (s *Service) AddSessionToMemory(ctx context.Context, current session.Session) error {
	if current == nil {
		return fmt.Errorf("%w: session 不能为空", ErrInvalidRequest)
	}
	index := 0
	for event := range current.Events().All() {
		if event == nil || event.Partial || event.Actions.Compaction != nil {
			continue
		}
		content := textFromContent(event.Content)
		if content == "" {
			continue
		}
		if !isConversationContent(event) {
			continue
		}
		eventID := strings.TrimSpace(event.ID)
		if eventID == "" {
			eventID = fmt.Sprintf("%d:%d:%s", index, event.Timestamp.UnixNano(), content)
		}
		item := Item{
			ID:             eventMemoryID(current.AppName(), current.UserID(), current.ID(), eventID),
			AppName:        current.AppName(),
			UserID:         current.UserID(),
			ConversationID: current.ID(),
			SourceEventID:  eventID,
			Author:         strings.TrimSpace(event.Author),
			Source:         "conversation",
			Content:        content,
			CreatedAt:      event.Timestamp,
		}
		if item.Author == "" {
			item.Author = contentRole(event.Content)
		}
		if _, err := s.Save(ctx, item); err != nil {
			return err
		}
		index++
	}
	return nil
}

type saveMemoryArgs struct {
	Content string `json:"content"`
	Tags    string `json:"tags,omitempty"`
}

type saveMemoryResult struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type limitedService struct {
	inner adkmemory.Service
	limit int
}

func (s *limitedService) AddSessionToMemory(ctx context.Context, current session.Session) error {
	return s.inner.AddSessionToMemory(ctx, current)
}

func (s *limitedService) SearchMemory(ctx context.Context, request *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	response, err := s.inner.SearchMemory(ctx, request)
	if err != nil || response == nil || len(response.Memories) <= s.limit {
		return response, err
	}
	response.Memories = response.Memories[:s.limit]
	return response, nil
}

func memoryEntry(item Item) adkmemory.Entry {
	metadata := map[string]any{"source": item.Source}
	if item.ConversationID != "" {
		metadata["conversation_id"] = item.ConversationID
	}
	if item.Tags != "" {
		metadata["tags"] = item.Tags
	}
	return adkmemory.Entry{
		ID:             item.ID,
		Content:        genai.NewContentFromText(item.Content, genai.RoleUser),
		Author:         item.Author,
		Timestamp:      item.CreatedAt,
		CustomMetadata: metadata,
	}
}

func isConversationContent(event *session.Event) bool {
	role := contentRole(event.Content)
	if role == genai.RoleUser || role == genai.RoleModel || role == "assistant" {
		return true
	}
	author := strings.ToLower(strings.TrimSpace(event.Author))
	return author == "user" || author == "assistant" || author == "model"
}

func contentRole(content *genai.Content) string {
	if content == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(content.Role))
}

func textFromContent(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range content.Parts {
		if part == nil || part.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(part.Text)
	}
	return strings.TrimSpace(builder.String())
}

func eventMemoryID(appName, userID, conversationID, eventID string) string {
	// 把会话 ID 纳入稳定键，避免不同会话使用手工重复事件 ID 时互相覆盖。
	sum := sha256.Sum256([]byte(strings.Join([]string{appName, userID, conversationID, eventID}, "\x00")))
	return "conversation-" + hex.EncodeToString(sum[:])
}

func newID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("memory-%d", time.Now().UnixNano())
	}
	return "memory-" + hex.EncodeToString(buffer[:])
}

func normalizeLimit(limit, fallback int) int {
	if limit <= 0 {
		limit = fallback
	}
	if limit > 50 {
		limit = 50
	}
	return limit
}
