package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Abot/internal/agent"
	"Abot/internal/document"
)

func TestSubAgentManagerImageFanoutAndParentLifecycle(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-subagent-image", UserID: "user-1", ConversationID: "conversation-1", SessionID: "session-1", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatalf("创建 invocation 失败: %v", err)
	}
	var current atomic.Int32
	var peak atomic.Int32
	manager, err := NewSubAgentManager(repo, func(_ context.Context, request agent.DocumentImageAnalysisRequest) (string, error) {
		if len(request.Images) != 1 {
			return "", errors.New("图片批次不正确")
		}
		value := current.Add(1)
		for {
			old := peak.Load()
			if value <= old || peak.CompareAndSwap(old, value) {
				break
			}
		}
		defer current.Add(-1)
		if request.Images[0].Locator == "page-2" {
			return "", errors.New("图片读取失败")
		}
		time.Sleep(5 * time.Millisecond)
		return "图片中的事实：" + request.Images[0].Locator, nil
	})
	if err != nil {
		t.Fatalf("创建子 Agent 管理器失败: %v", err)
	}
	images := []document.Image{{Locator: "page-1", Name: "one.png", MIMEType: "image/png", Data: []byte("one")}, {Locator: "page-2", Name: "two.png", MIMEType: "image/png", Data: []byte("two")}, {Locator: "page-3", Name: "three.png", MIMEType: "image/png", Data: []byte("three")}}
	results, err := manager.AnalyzeDocumentImages(context.Background(), agent.DocumentImageAnalysisRequest{InvocationID: invocation.ID, UserID: invocation.UserID, DocumentName: "report.docx", Query: "表格", Images: images})
	if err != nil {
		t.Fatalf("图片子 Agent 执行失败: %v", err)
	}
	if len(results) != len(images) || results[0].Text == "" || results[1].Error == "" || results[2].Text == "" {
		t.Fatalf("图片结果不完整: %#v", results)
	}
	if peak.Load() < 2 {
		t.Fatalf("图片没有按预算并发执行，峰值=%d", peak.Load())
	}
	groupItems, err := manager.ListSubAgentGroups(context.Background(), invocation.ID, "", 10)
	if err != nil || len(groupItems) != 1 {
		t.Fatalf("读取图片 group 失败: %v %#v", err, groupItems)
	}
	group := groupItems[0]
	if group.Status != SubAgentGroupPartial || group.CompletedCount != 2 || group.FailedCount != 1 {
		t.Fatalf("图片 group 聚合状态不正确: %#v", group)
	}
	updated, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil || updated.Status != InvocationRunning {
		t.Fatalf("主 invocation 未恢复 running: %v %#v", err, updated)
	}
}

func TestSubAgentManagerMasterSwitchBlocksEverySubAgentEntryPoint(t *testing.T) {
	manager, err := NewSubAgentManager(NewMemoryRepository(), nil)
	if err != nil {
		t.Fatalf("创建子 Agent 管理器失败: %v", err)
	}
	disabled := false
	runtime := agent.RuntimeOptions{SubAgentEnabled: &disabled}

	if _, err := manager.AnalyzeDocumentImages(context.Background(), agent.DocumentImageAnalysisRequest{Runtime: runtime}); !errors.Is(err, agent.ErrSubAgentsDisabled) {
		t.Fatalf("视觉子 Agent 未被总开关拦截: %v", err)
	}
	if _, err := manager.RunBuiltInSubAgentGroup(context.Background(), BuiltInSubAgentGroupRequest{Runtime: runtime}); !errors.Is(err, agent.ErrSubAgentsDisabled) {
		t.Fatalf("通用文本子 Agent 未被总开关拦截: %v", err)
	}
	if _, err := manager.RunRetrieval(context.Background(), RetrievalRequest{Runtime: runtime}); !errors.Is(err, agent.ErrSubAgentsDisabled) {
		t.Fatalf("检索子 Agent 未被总开关拦截: %v", err)
	}
}

func TestSubAgentManagerRetrievalDeduplicatesAndPersistsEvidence(t *testing.T) {
	repo := NewMemoryRepository()
	manager, err := NewSubAgentManager(repo, nil)
	if err != nil {
		t.Fatalf("创建子 Agent 管理器失败: %v", err)
	}
	shared := EvidenceItem{SourceID: "same", Locator: "doc:1", Excerpt: "共同证据", ContentDigest: "sha256:shared", RetrievalScore: 0.8}
	var memoryCalls atomic.Int32
	if err := manager.SetRetrievalAdapter(NewRetrievalAdapter("memory", func(context.Context, RetrievalRequest) ([]EvidenceItem, error) {
		memoryCalls.Add(1)
		return []EvidenceItem{shared, {SourceID: "memory-2", Locator: "doc:2", Excerpt: "记忆证据", RetrievalScore: 0.3}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	var knowledgeCalls atomic.Int32
	if err := manager.SetRetrievalAdapter(NewRetrievalAdapter("knowledge", func(context.Context, RetrievalRequest) ([]EvidenceItem, error) {
		knowledgeCalls.Add(1)
		return []EvidenceItem{shared, {SourceID: "knowledge-2", Locator: "doc:3", Excerpt: "知识证据", RetrievalScore: 0.9}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	result, err := manager.RunRetrieval(context.Background(), RetrievalRequest{UserID: "user-1", Query: "Abot", SourceKinds: []string{"memory", "knowledge"}, TopK: 10})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(result.Evidence) != 3 || result.NoEvidence || result.Partial {
		t.Fatalf("检索汇总不正确: %#v", result)
	}
	evidence, err := manager.ListSubAgentEvidence(context.Background(), result.GroupID, 10, "")
	if err != nil || len(evidence) != 3 {
		t.Fatalf("证据没有持久化: %v %#v", err, evidence)
	}
	group, err := manager.GetSubAgentGroup(context.Background(), result.GroupID)
	if err != nil || group.Status != SubAgentGroupCompleted || group.ResultDigest == "" {
		t.Fatalf("检索 group 状态不正确: %v %#v", err, group)
	}
	if _, err := manager.RunRetrieval(context.Background(), RetrievalRequest{UserID: "user-1", Query: "Abot", SourceKinds: []string{"memory", "knowledge"}, TopK: 10}); err != nil {
		t.Fatalf("缓存命中的重复检索失败: %v", err)
	}
	if memoryCalls.Load() != 1 || knowledgeCalls.Load() != 1 {
		t.Fatalf("重复检索没有复用有界缓存: memory=%d knowledge=%d", memoryCalls.Load(), knowledgeCalls.Load())
	}
}

func TestSubAgentManagerRetrievalBindsWorkspaceToInvocation(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-subagent-workspace", UserID: "user-1", ConversationID: "conversation-1", WorkspaceID: "workspace-1", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	manager, err := NewSubAgentManager(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetRetrievalAdapter(NewRetrievalAdapter("workspace", func(context.Context, RetrievalRequest) ([]EvidenceItem, error) {
		return []EvidenceItem{{SourceID: "workspace-1", Excerpt: "工作区证据"}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunRetrieval(context.Background(), RetrievalRequest{InvocationID: invocation.ID, UserID: invocation.UserID, WorkspaceID: "workspace-2", Query: "证据", SourceKinds: []string{"workspace"}}); err == nil {
		t.Fatal("不匹配 invocation 的 workspace 不应被允许检索")
	}
	result, err := manager.RunRetrieval(context.Background(), RetrievalRequest{InvocationID: invocation.ID, UserID: invocation.UserID, WorkspaceID: invocation.WorkspaceID, Query: "证据", SourceKinds: []string{"workspace"}})
	if err != nil || len(result.Evidence) != 1 {
		t.Fatalf("匹配 invocation workspace 的检索失败: %v %#v", err, result)
	}
}

func TestSubAgentManagerRejectsExpiredRetrievalDeadline(t *testing.T) {
	manager, err := NewSubAgentManager(NewMemoryRepository(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunRetrieval(context.Background(), RetrievalRequest{UserID: "user-1", Query: "过期", Deadline: time.Now().UTC().Add(-time.Second)}); err == nil {
		t.Fatal("过期检索 deadline 应被拒绝")
	}
}

func TestSubAgentManagerRecoveryExpiresRunningGroups(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	group := SubAgentGroup{ID: "group-recovery", Profile: SubAgentProfileResearch, ExpectedCount: 1, MaxConcurrency: 1, CreatedAt: now, UpdatedAt: now}
	run := SubAgentRun{ID: "run-recovery", GroupID: group.ID, Profile: SubAgentProfileResearch, Ordinal: 0, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateSubAgentGroup(context.Background(), group, []SubAgentRun{run}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.ClaimSubAgentRun(context.Background(), run.ID, "worker", now, time.Minute); err != nil || !ok {
		t.Fatalf("claim 子 Agent 失败: %v %v", err, ok)
	}
	manager, err := NewSubAgentManager(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("恢复子 Agent 失败: %v", err)
	}
	gotGroup, _ := manager.GetSubAgentGroup(context.Background(), group.ID)
	gotRun, _ := manager.subRepo.GetSubAgentRun(context.Background(), run.ID)
	if gotGroup.Status != SubAgentGroupExpired || gotRun.Status != SubAgentRunExpired {
		t.Fatalf("恢复没有安全过期子 Agent: group=%#v run=%#v", gotGroup, gotRun)
	}
	if gotRun.Error == "" {
		t.Fatal("过期子 Agent 缺少原因")
	}
}

type builtInSubAgentRunnerFunc func(context.Context, agent.BuiltInSubAgentRequest) (string, error)

func (f builtInSubAgentRunnerFunc) RunBuiltInSubAgent(ctx context.Context, request agent.BuiltInSubAgentRequest) (string, error) {
	return f(ctx, request)
}

func TestSubAgentManagerBuiltInTextGroupUsesBudgetedLifecycle(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	invocation := Invocation{ID: "inv-subagent-text", UserID: "user-1", ConversationID: "conversation-1", SessionID: "session-1", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(context.Background(), invocation); err != nil {
		t.Fatalf("创建 invocation 失败: %v", err)
	}
	manager, err := NewSubAgentManager(repo, nil)
	if err != nil {
		t.Fatalf("创建子 Agent 管理器失败: %v", err)
	}
	var received []string
	var receivedMu sync.Mutex
	manager.SetBuiltInTextRunner(builtInSubAgentRunnerFunc(func(_ context.Context, request agent.BuiltInSubAgentRequest) (string, error) {
		receivedMu.Lock()
		received = append(received, request.Prompt)
		receivedMu.Unlock()
		return "完成：" + request.Prompt, nil
	}))
	results, err := manager.RunBuiltInSubAgentGroup(context.Background(), BuiltInSubAgentGroupRequest{
		InvocationID: invocation.ID, UserID: invocation.UserID, ConversationID: invocation.ConversationID,
		Profile: SubAgentProfileResearch, Purpose: "并行研究摘要", MaxConcurrency: 2,
		Tasks: []BuiltInSubAgentTask{{NodeID: "first", Prompt: "读取记忆"}, {NodeID: "second", Prompt: "读取会话"}},
	})
	if err != nil {
		t.Fatalf("文本子 Agent 组执行失败: %v", err)
	}
	if len(results) != 2 || results[0].Text != "完成：读取记忆" || results[1].Text != "完成：读取会话" {
		t.Fatalf("文本子 Agent 结果不正确: %#v", results)
	}
	if len(received) != 2 {
		t.Fatalf("文本子 Agent 没有按节点调用，收到=%#v", received)
	}
	groups, err := manager.ListSubAgentGroups(context.Background(), invocation.ID, "", 10)
	if err != nil || len(groups) != 1 || groups[0].Status != SubAgentGroupCompleted || groups[0].CompletedCount != 2 {
		t.Fatalf("文本子 Agent group 状态不正确: %v %#v", err, groups)
	}
	runs, err := manager.ListSubAgentRuns(context.Background(), groups[0].ID, "", 10)
	if err != nil || len(runs) != 2 || runs[0].ResultText == "" || runs[0].InputMetadata["prompt_digest"] == nil {
		t.Fatalf("文本子 Agent run 没有保存有界摘要: %v %#v", err, runs)
	}
	updated, err := repo.GetInvocation(context.Background(), invocation.ID)
	if err != nil || updated.Status != InvocationRunning {
		t.Fatalf("主 invocation 未恢复 running: %v %#v", err, updated)
	}
}

func TestSubAgentManagerUsesProfileExecutionDefaults(t *testing.T) {
	repo := NewMemoryRepository()
	manager, err := NewSubAgentManager(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	var received agent.BuiltInSubAgentRequest
	manager.SetBuiltInTextRunner(builtInSubAgentRunnerFunc(func(_ context.Context, request agent.BuiltInSubAgentRequest) (string, error) {
		received = request
		return "已完成", nil
	}))
	temperature := 0.2
	results, err := manager.RunBuiltInSubAgentGroup(context.Background(), BuiltInSubAgentGroupRequest{
		UserID: "user-profile", Profile: SubAgentProfileResearch,
		Runtime: agent.RuntimeOptions{
			AIEnabled: true,
			SubAgentProfiles: map[string]agent.SubAgentProfileOptions{
				string(SubAgentProfileResearch): {
					ProviderID: "provider-research", ModelID: "model-research", ReasoningEffort: "high",
					Temperature: &temperature, MaxOutputTokens: 512, TimeoutSeconds: 45,
					MaxConcurrency: 1, OutputBudgetBytes: 4096, FailurePolicy: string(SubAgentFailureAbort),
				},
			},
		},
		Tasks: []BuiltInSubAgentTask{{NodeID: "research", Prompt: "整理证据"}},
	})
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("按 profile 执行失败: %v %#v", err, results)
	}
	groups, err := manager.ListSubAgentGroups(context.Background(), "", "", 10)
	if err != nil || len(groups) != 1 {
		t.Fatalf("读取 profile group 失败: %v %#v", err, groups)
	}
	group := groups[0]
	if group.MaxConcurrency != 1 || group.TimeoutSeconds != 45 || group.OutputBudgetBytes != 4096 || group.FailurePolicy != SubAgentFailureAbort {
		t.Fatalf("profile 执行预算未生效: %#v", group)
	}
	profile, ok := received.Runtime.SubAgentProfile(string(SubAgentProfileResearch))
	if !ok || profile.ProviderID != "provider-research" || profile.ModelID != "model-research" || profile.ReasoningEffort != "high" || profile.MaxOutputTokens != 512 || profile.Temperature == nil || *profile.Temperature != temperature {
		t.Fatalf("profile 模型参数未传到 Runner: %#v", received.Runtime)
	}
}

func TestSubAgentManagerBuiltInTextGroupHonorsDependencies(t *testing.T) {
	repo := NewMemoryRepository()
	manager, err := NewSubAgentManager(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	var orderMu sync.Mutex
	var order []string
	manager.SetBuiltInTextRunner(builtInSubAgentRunnerFunc(func(_ context.Context, request agent.BuiltInSubAgentRequest) (string, error) {
		orderMu.Lock()
		order = append(order, request.Prompt)
		orderMu.Unlock()
		return "完成：" + request.Prompt, nil
	}))
	results, err := manager.RunBuiltInSubAgentGroup(context.Background(), BuiltInSubAgentGroupRequest{
		UserID: "user-1", Profile: SubAgentProfileResearch, MaxConcurrency: 2,
		Tasks: []BuiltInSubAgentTask{
			{NodeID: "inspect", Prompt: "检查来源"},
			{NodeID: "review", Prompt: "审查结果", DependsOn: []string{"inspect"}},
			{NodeID: "verify", Prompt: "验证汇总", DependsOn: []string{"review"}},
		},
	})
	if err != nil {
		t.Fatalf("带依赖的文本子 Agent 执行失败: %v", err)
	}
	if len(results) != 3 || results[0].Error != "" || results[1].Error != "" || results[2].Error != "" {
		t.Fatalf("带依赖的文本子 Agent 结果不完整: %#v", results)
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 3 || !strings.HasPrefix(order[0], "检查来源") || !strings.HasPrefix(order[1], "审查结果") || !strings.HasPrefix(order[2], "验证汇总") {
		t.Fatalf("子 Agent 没有按依赖 stage 执行: %#v", order)
	}
	if !strings.Contains(order[1], "完成：检查来源") || !strings.Contains(order[2], "完成：审查结果") {
		t.Fatalf("下游子 Agent 没有收到上游有界结果: %#v", order)
	}

	groups, err := manager.ListSubAgentGroups(context.Background(), "", "", 10)
	if err != nil || len(groups) != 1 || groups[0].Status != SubAgentGroupCompleted {
		t.Fatalf("带依赖的 group 状态不正确: %v %#v", err, groups)
	}
	runs, err := manager.ListSubAgentRuns(context.Background(), groups[0].ID, "", 10)
	if err != nil || len(runs) != 3 || runs[1].Stage != 1 || len(runs[1].DependsOn) != 1 || runs[2].Stage != 2 {
		t.Fatalf("依赖 stage 没有持久化: %v %#v", err, runs)
	}
}

func TestSubAgentManagerRejectsProfilesWithoutTextRunnerPermission(t *testing.T) {
	manager, err := NewSubAgentManager(NewMemoryRepository(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetBuiltInTextRunner(builtInSubAgentRunnerFunc(func(context.Context, agent.BuiltInSubAgentRequest) (string, error) {
		return "不应执行", nil
	}))
	if _, err := manager.RunBuiltInSubAgentGroup(context.Background(), BuiltInSubAgentGroupRequest{
		UserID: "user-1", Profile: SubAgentProfileWorkspaceChange,
		Tasks: []BuiltInSubAgentTask{{NodeID: "change", Prompt: "修改文件"}},
	}); err == nil {
		t.Fatal("workspace_change 不应进入无工具文本 Runner")
	}
}

func TestSubAgentManagerDependencyFailureDoesNotRunDescendants(t *testing.T) {
	repo := NewMemoryRepository()
	manager, err := NewSubAgentManager(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	manager.SetBuiltInTextRunner(builtInSubAgentRunnerFunc(func(_ context.Context, request agent.BuiltInSubAgentRequest) (string, error) {
		calls.Add(1)
		if request.Prompt == "失败节点" {
			return "", errors.New("上游失败")
		}
		return "不应执行", nil
	}))
	results, err := manager.RunBuiltInSubAgentGroup(context.Background(), BuiltInSubAgentGroupRequest{
		UserID: "user-1", Profile: SubAgentProfileResearch, FailurePolicy: SubAgentFailureContinue,
		Tasks: []BuiltInSubAgentTask{{NodeID: "failed", Prompt: "失败节点"}, {NodeID: "descendant", Prompt: "后续节点", DependsOn: []string{"failed"}}},
	})
	if err != nil {
		t.Fatalf("失败传播执行异常: %v", err)
	}
	if calls.Load() != 1 || results[0].Error == "" || results[1].Error == "" || results[1].Text != "" {
		t.Fatalf("依赖失败没有阻断后续节点: calls=%d results=%#v", calls.Load(), results)
	}
}

func TestSubAgentManagerPrunesOnlyExpiredTerminalGroups(t *testing.T) {
	repo := NewMemoryRepository()
	old := time.Now().UTC().Add(-48 * time.Hour)
	group := SubAgentGroup{ID: "group-prune", Profile: SubAgentProfileResearch, Status: SubAgentGroupCompleted, ExpectedCount: 1, CompletedCount: 1, MaxConcurrency: 1, CreatedAt: old, UpdatedAt: old, FinishedAt: old}
	run := SubAgentRun{ID: "run-prune", GroupID: group.ID, Profile: SubAgentProfileResearch, Status: SubAgentRunCompleted, Ordinal: 0, CreatedAt: old, QueuedAt: old, FinishedAt: old, UpdatedAt: old, ResultText: "旧摘要"}
	if err := repo.CreateSubAgentGroup(context.Background(), group, []SubAgentRun{run}); err != nil {
		t.Fatalf("创建过期子 Agent 失败: %v", err)
	}
	manager, err := NewSubAgentManager(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := manager.Prune(context.Background(), time.Now().UTC().Add(-24*time.Hour), 10)
	if err != nil || deleted != 1 {
		t.Fatalf("清理过期子 Agent 失败: %v %d", err, deleted)
	}
	if _, err := manager.GetSubAgentGroup(context.Background(), group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期子 Agent 未删除: %v", err)
	}
}
