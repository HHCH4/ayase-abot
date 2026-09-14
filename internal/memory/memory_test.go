package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestServiceCRUDSearchAndLimit(t *testing.T) {
	ctx := context.Background()
	repository := newTestRepository()
	service, err := NewService(repository, "abot")
	if err != nil {
		t.Fatalf("创建长期记忆服务失败: %v", err)
	}

	first, err := service.Save(ctx, Item{UserID: "user-1", Content: "用户偏好中文回答", Tags: "偏好"})
	if err != nil {
		t.Fatalf("保存第一条记忆失败: %v", err)
	}
	second, err := service.Save(ctx, Item{UserID: "user-1", Content: "项目需要使用 Go", Tags: "项目"})
	if err != nil {
		t.Fatalf("保存第二条记忆失败: %v", err)
	}
	if first.ID == second.ID || first.Source != "manual" {
		t.Fatalf("新记忆 ID 或来源不正确: first=%#v second=%#v", first, second)
	}

	items, err := service.List(ctx, "user-1", "中文", 0)
	if err != nil || len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("按内容搜索结果不正确: items=%#v err=%v", items, err)
	}
	result, err := service.SearchMemory(ctx, &adkmemory.SearchRequest{UserID: "user-1", AppName: "abot", Query: "项目"})
	if err != nil || len(result.Memories) != 1 || result.Memories[0].ID != second.ID {
		t.Fatalf("ADK 记忆检索结果不正确: result=%#v err=%v", result, err)
	}

	updated, err := service.Update(ctx, "user-1", first.ID, "用户偏好简洁中文回答", "偏好,语言")
	if err != nil {
		t.Fatalf("更新记忆失败: %v", err)
	}
	if updated.ID != first.ID || updated.CreatedAt != first.CreatedAt || updated.Content != "用户偏好简洁中文回答" {
		t.Fatalf("更新记忆不应改变 ID/创建时间: before=%#v after=%#v", first, updated)
	}
	if err := service.Delete(ctx, "user-1", first.ID); err != nil {
		t.Fatalf("删除记忆失败: %v", err)
	}
	if _, err := service.Get(ctx, "user-1", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("物理删除后记忆仍可读取: %v", err)
	}
	if err := service.Clear(ctx, "user-1"); err != nil {
		t.Fatalf("清空记忆失败: %v", err)
	}
	if count, err := service.Count(ctx, "user-1"); err != nil || count != 0 {
		t.Fatalf("清空后记忆数量 = %d，错误 = %v", count, err)
	}

	if limited, ok := service.WithLimit(1).(*limitedService); !ok || limited.limit != 1 {
		t.Fatal("WithLimit 没有保存检索上限")
	}
	if tools, err := service.Tools(); err != nil || len(tools) != 1 || tools[0].Name() != "memory_request_save" {
		t.Fatalf("明确保存工具创建失败: tools=%#v err=%v", tools, err)
	}
}

func TestAddSessionToMemorySkipsInternalEventsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repository := newTestRepository()
	service, err := NewService(repository, "abot")
	if err != nil {
		t.Fatalf("创建长期记忆服务失败: %v", err)
	}
	sessions := session.InMemoryService()
	created, err := sessions.Create(ctx, &session.CreateRequest{AppName: "abot", UserID: "user-1", SessionID: "conversation-1"})
	if err != nil {
		t.Fatalf("创建测试会话失败: %v", err)
	}
	now := time.Now().UTC()
	partialEvent := &session.Event{ID: "partial-event", Author: "user", Timestamp: now.Add(2 * time.Second), LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("不完整", genai.RoleUser)}}
	partialEvent.Partial = true
	events := []*session.Event{
		{ID: "user-event", Author: "user", Timestamp: now, LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("用户希望使用中文", genai.RoleUser)}},
		{ID: "assistant-event", Author: "assistant", Timestamp: now.Add(time.Second), LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("好的，我会使用中文", genai.RoleModel)}},
		partialEvent,
		{ID: "compaction-event", Timestamp: now.Add(3 * time.Second), Actions: session.EventActions{Compaction: &session.EventCompaction{StartTimestamp: now, EndTimestamp: now.Add(time.Second), CompactedContent: genai.NewContentFromText("内部摘要", genai.RoleModel)}}},
	}
	for _, event := range events {
		if err := sessions.AppendEvent(ctx, created.Session, event); err != nil {
			t.Fatalf("追加测试事件失败: %v", err)
		}
	}

	if err := service.AddSessionToMemory(ctx, created.Session); err != nil {
		t.Fatalf("导入会话记忆失败: %v", err)
	}
	if err := service.AddSessionToMemory(ctx, created.Session); err != nil {
		t.Fatalf("重复导入会话记忆失败: %v", err)
	}
	items, err := service.List(ctx, "user-1", "", 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("内部事件或重复导入处理错误: items=%#v err=%v", items, err)
	}
	for _, item := range items {
		if item.Source != "conversation" || item.ConversationID != "conversation-1" || item.SourceEventID == "" {
			t.Fatalf("导入记忆来源字段不正确: %#v", item)
		}
	}

	// 不同会话即使复用了同一个事件 ID，也必须生成不同的记忆键，避免相互覆盖。
	createdSecond, err := sessions.Create(ctx, &session.CreateRequest{AppName: "abot", UserID: "user-1", SessionID: "conversation-2"})
	if err != nil {
		t.Fatalf("创建第二个测试会话失败: %v", err)
	}
	if err := sessions.AppendEvent(ctx, createdSecond.Session, &session.Event{
		ID: "user-event", Author: "user", Timestamp: now.Add(4 * time.Second),
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("第二个会话也使用中文", genai.RoleUser)},
	}); err != nil {
		t.Fatalf("追加第二个会话事件失败: %v", err)
	}
	if err := service.AddSessionToMemory(ctx, createdSecond.Session); err != nil {
		t.Fatalf("导入第二个会话记忆失败: %v", err)
	}
	items, err = service.List(ctx, "user-1", "", 0)
	if err != nil || len(items) != 3 {
		t.Fatalf("不同会话复用事件 ID 后记忆被覆盖: items=%#v err=%v", items, err)
	}
}

type testRepository struct {
	items map[string]Item
}

func newTestRepository() *testRepository {
	return &testRepository{items: make(map[string]Item)}
}

func (r *testRepository) List(_ context.Context, userID, appName, query string, limit int) ([]Item, error) {
	items := make([]Item, 0)
	query = strings.ToLower(query)
	for _, item := range r.items {
		if item.UserID != userID || item.AppName != appName {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.Content), query) && !strings.Contains(strings.ToLower(item.Tags), query) {
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *testRepository) Get(_ context.Context, userID, appName, id string) (Item, error) {
	item, ok := r.items[id]
	if !ok || item.UserID != userID || item.AppName != appName {
		return Item{}, ErrNotFound
	}
	return item, nil
}

func (r *testRepository) Save(_ context.Context, item Item) error {
	if old, ok := r.items[item.ID]; ok && (old.UserID != item.UserID || old.AppName != item.AppName) {
		return ErrConflict
	}
	r.items[item.ID] = item
	return nil
}

func (r *testRepository) Delete(_ context.Context, userID, appName, id string) error {
	if _, err := r.Get(context.Background(), userID, appName, id); err != nil {
		return err
	}
	delete(r.items, id)
	return nil
}

func (r *testRepository) Clear(_ context.Context, userID, appName string) error {
	for id, item := range r.items {
		if item.UserID == userID && item.AppName == appName {
			delete(r.items, id)
		}
	}
	return nil
}

func (r *testRepository) DeleteByConversation(_ context.Context, conversationID string) error {
	for id, item := range r.items {
		if item.ConversationID == conversationID {
			delete(r.items, id)
		}
	}
	return nil
}

func (r *testRepository) Count(_ context.Context, userID, appName string) (int, error) {
	count := 0
	for _, item := range r.items {
		if item.UserID == userID && item.AppName == appName {
			count++
		}
	}
	return count, nil
}

var _ adkmemory.Service = (*Service)(nil)
