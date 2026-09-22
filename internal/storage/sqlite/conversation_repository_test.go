package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/conversation"
	"Abot/internal/memory"
	"Abot/internal/workspace"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestConversationDeleteRemovesADKEventsAndWorkspaceOperations(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 SQLite Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	conversationService, err := conversation.NewService(store.ConversationRepository(), store.SessionService(), "abot")
	if err != nil {
		t.Fatalf("创建对话服务失败: %v", err)
	}
	item, err := conversationService.Create(context.Background(), conversation.CreateRequest{UserID: "user-1", WorkspaceID: "project"})
	if err != nil {
		t.Fatalf("创建对话失败: %v", err)
	}
	saved, err := store.SessionService().Get(context.Background(), &session.GetRequest{AppName: "abot", UserID: item.UserID, SessionID: item.ID})
	if err != nil {
		t.Fatalf("创建 ADK Session 失败: %v", err)
	}
	event := session.NewEvent(context.Background(), "test-invocation")
	event.Author = "user"
	event.Content = genai.NewContentFromText("需要物理删除的消息", genai.RoleUser)
	if err := store.SessionService().AppendEvent(context.Background(), saved.Session, event); err != nil {
		t.Fatalf("写入 ADK 事件失败: %v", err)
	}
	workspaceRepository := store.WorkspaceRepository()
	exitCode := 7
	if err := workspaceRepository.SaveOperation(context.Background(), workspace.Operation{
		ID: "operation-conversation-delete", WorkspaceID: "project", ConversationID: item.ID,
		Type: workspace.OperationWriteFile, Path: "secret.txt", Content: "secret",
		ExitCode: &exitCode, OutputDigest: "digest", OutputTruncated: true, DurationMS: 12, TimedOut: true, Unknown: true,
	}); err != nil {
		t.Fatalf("写入工作区操作失败: %v", err)
	}
	commandRunRepo, ok := workspaceRepository.(workspace.CommandRunRepository)
	if !ok {
		t.Fatal("SQLite workspace repository should implement CommandRunRepository")
	}
	if err := commandRunRepo.CreateCommandRun(context.Background(), workspace.CommandRun{
		ID: "command-run-conversation-delete", WorkspaceID: "project", ConversationID: item.ID,
		OperationID: "operation-conversation-delete", Executor: "local", Mode: "shell", Status: workspace.CommandRunQueued,
		Revision: 1, QueuedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("写入 command run 失败: %v", err)
	}
	storedOperation, err := workspaceRepository.GetOperation(context.Background(), "operation-conversation-delete")
	if err != nil {
		t.Fatalf("读取工作区操作失败: %v", err)
	}
	if storedOperation.ExitCode == nil || *storedOperation.ExitCode != exitCode || storedOperation.OutputDigest != "digest" || !storedOperation.OutputTruncated || storedOperation.DurationMS != 12 || !storedOperation.TimedOut || !storedOperation.Unknown {
		t.Fatalf("SQLite 未完整保存命令结果元数据: %#v", storedOperation)
	}
	if err := store.MemoryRepository().Save(context.Background(), memory.Item{
		ID: "memory-conversation-delete", AppName: "abot", UserID: item.UserID,
		ConversationID: item.ID, Source: "manual", Content: "也需要物理删除的长期记忆",
	}); err != nil {
		t.Fatalf("写入长期记忆失败: %v", err)
	}
	if _, err := conversationService.Archive(context.Background(), item.UserID, item.ID); err != nil {
		t.Fatalf("归档对话失败: %v", err)
	}
	if err := conversationService.Delete(context.Background(), item.UserID, item.ID); err != nil {
		t.Fatalf("删除对话失败: %v", err)
	}
	if _, err := store.SessionService().Get(context.Background(), &session.GetRequest{AppName: "abot", UserID: item.UserID, SessionID: item.ID}); err == nil {
		t.Fatal("删除对话后 ADK Session 仍存在")
	}
	operations, err := workspaceRepository.ListOperations(context.Background(), "project")
	if err != nil {
		t.Fatalf("读取工作区操作失败: %v", err)
	}
	for _, operation := range operations {
		if operation.ConversationID == item.ID {
			t.Fatal("删除对话后关联工作区操作仍存在")
		}
	}
	if _, err := commandRunRepo.GetCommandRun(context.Background(), "command-run-conversation-delete"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("删除对话后关联 command run 仍存在: %v", err)
	}
	memories, err := store.MemoryRepository().List(context.Background(), item.UserID, "abot", "", 50)
	if err != nil {
		t.Fatalf("读取对话关联长期记忆失败: %v", err)
	}
	if len(memories) != 0 {
		t.Fatalf("删除对话后关联长期记忆仍存在: %#v", memories)
	}
	invocationRepo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "invocation-conversation-delete", UserID: item.UserID, ConversationID: item.ID, SessionID: item.ID, Status: agentruntime.InvocationWaitingTool, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	// The conversation has already been deleted above, so create a second
	// archived conversation to exercise the cascade against a live invocation.
	second, err := conversationService.Create(context.Background(), conversation.CreateRequest{UserID: item.UserID, WorkspaceID: "project"})
	if err != nil {
		t.Fatalf("创建第二个对话失败: %v", err)
	}
	invocation.ConversationID = second.ID
	invocation.SessionID = second.ID
	if err := invocationRepo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatalf("写入待恢复 invocation 失败: %v", err)
	}
	subAgentRepo, ok := invocationRepo.(agentruntime.SubAgentRepository)
	if !ok {
		t.Fatal("SQLite runtime 未实现 SubAgentRepository")
	}
	subAgentEvidenceRepo, ok := invocationRepo.(agentruntime.SubAgentEvidenceRepository)
	if !ok {
		t.Fatal("SQLite runtime 未实现 SubAgentEvidenceRepository")
	}
	subAgentGroup := agentruntime.SubAgentGroup{ID: "group-conversation-delete", InvocationID: invocation.ID, Profile: agentruntime.SubAgentProfileResearch, ExpectedCount: 1, MaxConcurrency: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	subAgentRun := agentruntime.SubAgentRun{ID: "run-conversation-delete", GroupID: subAgentGroup.ID, InvocationID: invocation.ID, Profile: agentruntime.SubAgentProfileResearch, Ordinal: 0, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := subAgentRepo.CreateSubAgentGroup(context.Background(), subAgentGroup, []agentruntime.SubAgentRun{subAgentRun}); err != nil {
		t.Fatalf("写入子 Agent group 失败: %v", err)
	}
	if err := subAgentEvidenceRepo.SaveSubAgentEvidence(context.Background(), subAgentGroup.ID, []agentruntime.EvidenceItem{{EvidenceID: "evidence-conversation-delete", SourceKind: "conversation", Excerpt: "要随对话删除的证据"}}); err != nil {
		t.Fatalf("写入子 Agent evidence 失败: %v", err)
	}
	handoffRepo, ok := invocationRepo.(agentruntime.InvocationResumeRepository)
	if !ok {
		t.Fatal("SQLite runtime repository 未实现 InvocationResumeRepository")
	}
	if _, err := handoffRepo.SaveInvocationResume(context.Background(), agentruntime.InvocationResume{ID: "resume-conversation-delete", InvocationID: invocation.ID, WaitID: "wait-1", Name: "external_wait", ResponseJSON: []byte(`{"output":"ready"}`), RequestDigest: "sha256:conversation-delete", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("写入待恢复 handoff 失败: %v", err)
	}
	if _, err := conversationService.Archive(context.Background(), second.UserID, second.ID); err != nil {
		t.Fatalf("归档第二个对话失败: %v", err)
	}
	if err := conversationService.Delete(context.Background(), second.UserID, second.ID); err != nil {
		t.Fatalf("删除第二个对话失败: %v", err)
	}
	if _, err := handoffRepo.GetInvocationResume(context.Background(), invocation.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("删除会话后待恢复 handoff 仍存在: %v", err)
	}
	if _, err := subAgentRepo.GetSubAgentGroup(context.Background(), subAgentGroup.ID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("删除会话后子 Agent group 仍存在: %v", err)
	}
	if _, err := subAgentEvidenceRepo.ListSubAgentEvidence(context.Background(), subAgentGroup.ID, 10, ""); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("删除会话后子 Agent evidence 仍存在: %v", err)
	}
}
