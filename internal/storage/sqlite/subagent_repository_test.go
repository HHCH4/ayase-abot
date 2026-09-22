package sqlite

import (
	"context"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteSubAgentRepositoryRoundTrip(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试 SQLite 失败: %v", err)
	}
	defer store.Close()
	repo, ok := store.RuntimeRepository().(agentruntime.SubAgentRepository)
	if !ok {
		t.Fatal("SQLite Runtime 未实现 SubAgentRepository")
	}
	evidenceRepo, ok := store.RuntimeRepository().(agentruntime.SubAgentEvidenceRepository)
	if !ok {
		t.Fatal("SQLite Runtime 未实现 SubAgentEvidenceRepository")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	group := agentruntime.SubAgentGroup{ID: "sqlite-subagent-group", InvocationID: "inv-sqlite", Profile: agentruntime.SubAgentProfileResearch, ExpectedCount: 1, MaxConcurrency: 1, CreatedAt: now, UpdatedAt: now}
	run := agentruntime.SubAgentRun{ID: "sqlite-subagent-run", GroupID: group.ID, InvocationID: group.InvocationID, Profile: agentruntime.SubAgentProfileResearch, Ordinal: 0, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateSubAgentGroup(ctx, group, []agentruntime.SubAgentRun{run}); err != nil {
		t.Fatalf("创建 SQLite 子 Agent group 失败: %v", err)
	}
	claimed, ok, err := repo.ClaimSubAgentRun(ctx, run.ID, "sqlite-worker", now, time.Minute)
	if err != nil || !ok || claimed.Status != agentruntime.SubAgentRunRunning {
		t.Fatalf("claim SQLite 子 Agent 失败: %v %#v %#v", err, ok, claimed)
	}
	if completed, err := repo.CompleteSubAgentRun(ctx, run.ID, "sqlite-worker", agentruntime.SubAgentRunCompleted, "摘要", "", "", now.Add(time.Second)); err != nil || !completed {
		t.Fatalf("完成 SQLite 子 Agent 失败: %v %v", err, completed)
	}
	group.Status = agentruntime.SubAgentGroupCompleted
	group.CompletedCount = 1
	group.ResultDigest = "sha256:result"
	if err := repo.UpdateSubAgentGroup(ctx, group); err != nil {
		t.Fatalf("更新 SQLite 子 Agent group 失败: %v", err)
	}
	if err := evidenceRepo.SaveSubAgentEvidence(ctx, group.ID, []agentruntime.EvidenceItem{{EvidenceID: "sqlite-evidence", SourceKind: "workspace", SourceID: "file", Excerpt: "证据", RetrievedAt: now}}); err != nil {
		t.Fatalf("保存 SQLite 证据失败: %v", err)
	}
	gotGroup, err := repo.GetSubAgentGroup(ctx, group.ID)
	if err != nil || gotGroup.Status != agentruntime.SubAgentGroupCompleted {
		t.Fatalf("读取 SQLite group 失败: %v %#v", err, gotGroup)
	}
	gotRuns, err := repo.ListSubAgentRuns(ctx, group.ID, "", 10)
	if err != nil || len(gotRuns) != 1 || gotRuns[0].ResultText != "摘要" {
		t.Fatalf("读取 SQLite run 失败: %v %#v", err, gotRuns)
	}
	gotEvidence, err := evidenceRepo.ListSubAgentEvidence(ctx, group.ID, 10, "")
	if err != nil || len(gotEvidence) != 1 || gotEvidence[0].Excerpt != "证据" {
		t.Fatalf("读取 SQLite 证据失败: %v %#v", err, gotEvidence)
	}
}
