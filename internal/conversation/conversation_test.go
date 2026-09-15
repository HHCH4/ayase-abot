package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestConversationMustArchiveBeforePhysicalDelete(t *testing.T) {
	ctx := context.Background()
	repository := newMemoryRepository()
	sessions := session.InMemoryService()
	service, err := NewService(repository, sessions, "abot")
	if err != nil {
		t.Fatalf("创建对话服务失败: %v", err)
	}
	item, err := service.Create(ctx, CreateRequest{UserID: "user-1", WorkspaceID: "project", Title: "项目对话"})
	if err != nil {
		t.Fatalf("创建工作区对话失败: %v", err)
	}
	if err := service.Delete(ctx, item.UserID, item.ID); !errors.Is(err, ErrDeleteRequiresArchive) {
		t.Fatalf("活跃对话直接删除错误 = %v，期望 ErrDeleteRequiresArchive", err)
	}
	archived, err := service.Archive(ctx, item.UserID, item.ID)
	if err != nil || archived.Status != StatusArchived {
		t.Fatalf("归档对话失败: item=%#v err=%v", archived, err)
	}
	if _, err := service.Get(ctx, item.UserID, item.ID); err != nil {
		t.Fatalf("归档后对话不应消失: %v", err)
	}
	if err := service.Delete(ctx, item.UserID, item.ID); err != nil {
		t.Fatalf("物理删除归档对话失败: %v", err)
	}
	if _, err := service.Get(ctx, item.UserID, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("物理删除后仍能读取对话: %v", err)
	}
	if _, err := sessions.Get(ctx, &session.GetRequest{AppName: "abot", UserID: item.UserID, SessionID: item.ID}); err == nil {
		t.Fatal("物理删除后 ADK Session 仍存在")
	}
}

func TestConversationDeleteInvokesArtifactCascadeBeforeMetadataDelete(t *testing.T) {
	ctx := context.Background()
	repository := newMemoryRepository()
	service, err := NewService(repository, session.InMemoryService(), "abot")
	if err != nil {
		t.Fatal(err)
	}
	item, err := service.Create(ctx, CreateRequest{UserID: "user-hook"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Archive(ctx, item.UserID, item.ID); err != nil {
		t.Fatal(err)
	}
	called := ""
	service.SetArtifactDeletionHook(func(_ context.Context, conversationID string) error {
		called = conversationID
		return nil
	})
	if err := service.Delete(ctx, item.UserID, item.ID); err != nil {
		t.Fatal(err)
	}
	if called != item.ID {
		t.Fatalf("artifact cascade was not called with conversation id: %q", called)
	}

	item, err = service.Create(ctx, CreateRequest{UserID: "user-hook-failure"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Archive(ctx, item.UserID, item.ID); err != nil {
		t.Fatal(err)
	}
	service.SetArtifactDeletionHook(func(context.Context, string) error { return errors.New("object store unavailable") })
	if err := service.Delete(ctx, item.UserID, item.ID); err == nil {
		t.Fatal("expected artifact cleanup failure to abort conversation deletion")
	}
	if _, err := service.Get(ctx, item.UserID, item.ID); err != nil {
		t.Fatalf("conversation should remain after failed artifact cleanup: %v", err)
	}
}

func TestMessagesDoesNotExposeContextCompactionEvents(t *testing.T) {
	ctx := context.Background()
	repository := newMemoryRepository()
	sessions := session.InMemoryService()
	service, err := NewService(repository, sessions, "abot")
	if err != nil {
		t.Fatalf("创建对话服务失败: %v", err)
	}
	item, err := service.Create(ctx, CreateRequest{UserID: "user-1", Title: "压缩测试"})
	if err != nil {
		t.Fatalf("创建对话失败: %v", err)
	}
	created, err := sessions.Get(ctx, &session.GetRequest{AppName: "abot", UserID: item.UserID, SessionID: item.ID})
	if err != nil {
		t.Fatalf("读取测试 Session 失败: %v", err)
	}
	now := time.Now().UTC()
	if err := sessions.AppendEvent(ctx, created.Session, &session.Event{
		ID: "user-event", Author: "user", Timestamp: now,
		LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("可见消息", "user")},
	}); err != nil {
		t.Fatalf("追加用户消息失败: %v", err)
	}
	if err := sessions.AppendEvent(ctx, created.Session, &session.Event{
		ID: "compaction-event", Author: "agent", Timestamp: now.Add(time.Second),
		Actions: session.EventActions{Compaction: &session.EventCompaction{
			StartTimestamp: now, EndTimestamp: now,
			CompactedContent: genai.NewContentFromText("内部摘要", "model"),
		}},
	}); err != nil {
		t.Fatalf("追加压缩事件失败: %v", err)
	}

	messages, err := service.Messages(ctx, item.UserID, item.ID)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	if len(messages) != 1 || messages[0].Text != "可见消息" {
		t.Fatalf("普通消息接口暴露了压缩事件: %#v", messages)
	}
	status, err := service.ContextStatus(ctx, item.UserID, item.ID)
	if err != nil {
		t.Fatalf("读取会话压缩状态失败: %v", err)
	}
	if !status.Compressed || status.CompactionCount != 1 || status.LastCompactionAt == nil || status.CoveredFrom == nil || status.CoveredTo == nil {
		t.Fatalf("会话压缩状态不完整: %#v", status)
	}
}

type memoryRepository struct {
	items map[string]Conversation
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{items: map[string]Conversation{}}
}

func (r *memoryRepository) List(_ context.Context, userID, workspaceID string, includeArchived bool) ([]Conversation, error) {
	result := make([]Conversation, 0)
	for _, item := range r.items {
		if item.UserID != userID || (workspaceID != "*" && item.WorkspaceID != workspaceID) {
			continue
		}
		if !includeArchived && item.Status != StatusActive {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *memoryRepository) Get(_ context.Context, userID, id string) (Conversation, error) {
	item, ok := r.items[id]
	if !ok || item.UserID != userID {
		return Conversation{}, ErrNotFound
	}
	return item, nil
}

func (r *memoryRepository) GetByID(_ context.Context, id string) (Conversation, error) {
	item, ok := r.items[id]
	if !ok {
		return Conversation{}, ErrNotFound
	}
	return item, nil
}

func (r *memoryRepository) CountByWorkspace(_ context.Context, workspaceID string) (int, error) {
	count := 0
	for _, item := range r.items {
		if item.WorkspaceID == workspaceID {
			count++
		}
	}
	return count, nil
}

func (r *memoryRepository) Save(_ context.Context, item Conversation) error {
	r.items[item.ID] = item
	return nil
}

func (r *memoryRepository) Delete(_ context.Context, userID, id string) error {
	item, ok := r.items[id]
	if !ok || item.UserID != userID {
		return ErrNotFound
	}
	delete(r.items, id)
	return nil
}

func TestSetWorkspaceValidatesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repository := newMemoryRepository()
	service, err := NewService(repository, session.InMemoryService(), "abot")
	if err != nil {
		t.Fatal(err)
	}
	item, err := service.Create(ctx, CreateRequest{UserID: "user-1", Title: "会话"})
	if err != nil {
		t.Fatal(err)
	}

	// 校验失败时必须拒绝，且不改变已有绑定。
	service.SetWorkspaceValidator(func(context.Context, string) error { return errors.New("工作区不存在") })
	if _, err := service.SetWorkspace(ctx, "user-1", item.ID, "missing"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("无效工作区必须被拒绝, got %v", err)
	}
	stored, err := service.Get(ctx, "user-1", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkspaceID != "" {
		t.Fatalf("被拒绝的绑定不应写入: %q", stored.WorkspaceID)
	}

	// 校验通过时写入，并保持幂等。
	service.SetWorkspaceValidator(func(_ context.Context, workspaceID string) error {
		if workspaceID != "project-a" {
			return errors.New("未知工作区")
		}
		return nil
	})
	bound, err := service.SetWorkspace(ctx, "user-1", item.ID, "project-a")
	if err != nil || bound.WorkspaceID != "project-a" {
		t.Fatalf("绑定工作区失败: %+v err=%v", bound, err)
	}
	again, err := service.SetWorkspace(ctx, "user-1", item.ID, "project-a")
	if err != nil || again.UpdatedAt != bound.UpdatedAt {
		t.Fatalf("重复绑定应幂等: %+v err=%v", again, err)
	}

	// 空值解除绑定。
	cleared, err := service.SetWorkspace(ctx, "user-1", item.ID, "")
	if err != nil || cleared.WorkspaceID != "" {
		t.Fatalf("清空工作区失败: %+v err=%v", cleared, err)
	}
}

func TestSetRuntimeOverrideAndArchiveGuard(t *testing.T) {
	ctx := context.Background()
	repository := newMemoryRepository()
	service, err := NewService(repository, session.InMemoryService(), "abot")
	if err != nil {
		t.Fatal(err)
	}
	item, err := service.Create(ctx, CreateRequest{UserID: "user-1", Title: "会话"})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := service.SetRuntimeOverride(ctx, "user-1", item.ID, "provider-a", "model-b")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProviderID != "provider-a" || updated.ModelID != "model-b" {
		t.Fatalf("模型覆盖未写入: %+v", updated)
	}
	// 幂等：值未变化时不推进时间戳。
	again, err := service.SetRuntimeOverride(ctx, "user-1", item.ID, "provider-a", "model-b")
	if err != nil || !again.UpdatedAt.Equal(updated.UpdatedAt) {
		t.Fatalf("重复设置应幂等: %+v err=%v", again, err)
	}
	// 清空覆盖，恢复继承。
	cleared, err := service.SetRuntimeOverride(ctx, "user-1", item.ID, "", "")
	if err != nil || cleared.ProviderID != "" || cleared.ModelID != "" {
		t.Fatalf("清空覆盖失败: %+v err=%v", cleared, err)
	}
	// 过长标识被拒绝。
	if _, err := service.SetRuntimeOverride(ctx, "user-1", item.ID, "p", strings.Repeat("m", 201)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("过长模型标识必须被拒绝, got %v", err)
	}

	// 归档后不允许再改绑定。
	if _, err := service.Archive(ctx, "user-1", item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetRuntimeOverride(ctx, "user-1", item.ID, "p", "m"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("归档会话不应接受覆盖, got %v", err)
	}
	if _, err := service.SetWorkspace(ctx, "user-1", item.ID, "project-a"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("归档会话不应接受工作区绑定, got %v", err)
	}
}
