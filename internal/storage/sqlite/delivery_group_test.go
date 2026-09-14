package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeDeliveryGroupIsDurableAndCASProtected(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-group-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocation.ID, []agentruntime.RuntimeDeliveryGroupMember{
		{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-sqlite", DeliveryID: "event-sqlite"},
		{Kind: agentruntime.RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-sqlite", DeliveryID: "checkpoint-sqlite"},
	}, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	stored, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group)
	if err != nil || !stored.MatchesIdentity(group) {
		_ = store.Close()
		t.Fatalf("SQLite group enqueue 失败: %#v err=%v", stored, err)
	}
	if _, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		_ = store.Close()
		t.Fatalf("SQLite group 重复 enqueue 应幂等: %v", err)
	}
	claimed, ok, err := groupRepo.ClaimRuntimeDeliveryGroup(ctx, "sqlite-group-worker", now, time.Minute)
	if err != nil || !ok || claimed.Status != agentruntime.RuntimeDeliveryGroupProcessing {
		_ = store.Close()
		t.Fatalf("SQLite group claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	updated, duplicate, err := groupRepo.MarkRuntimeDeliveryGroupMember(ctx, claimed.ID, claimed.Revision, agentruntime.RuntimeDeliveryKindEvent, "event-sqlite", "event-sqlite", agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || updated.Revision != claimed.Revision+1 {
		_ = store.Close()
		t.Fatalf("SQLite group member CAS 错误: %#v duplicate=%v err=%v", updated, duplicate, err)
	}
	if _, _, err := groupRepo.MarkRuntimeDeliveryGroupMember(ctx, claimed.ID, claimed.Revision, agentruntime.RuntimeDeliveryKindCheckpoint, "checkpoint-sqlite", "checkpoint-sqlite", agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now); !errors.Is(err, agentruntime.ErrConflict) {
		_ = store.Close()
		t.Fatalf("SQLite 过期 revision 必须拒绝: %v", err)
	}
	completed, duplicate, err := groupRepo.MarkRuntimeDeliveryGroupMember(ctx, claimed.ID, updated.Revision, agentruntime.RuntimeDeliveryKindCheckpoint, "checkpoint-sqlite", "checkpoint-sqlite", agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || completed.Status != agentruntime.RuntimeDeliveryGroupCompleted {
		_ = store.Close()
		t.Fatalf("SQLite 最后 member 未完成 group: %#v duplicate=%v err=%v", completed, duplicate, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reopened := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupRepository)
	loaded, err := reopened.GetRuntimeDeliveryGroup(ctx, group.ID)
	if err != nil || loaded.Status != agentruntime.RuntimeDeliveryGroupCompleted || len(loaded.Members) != 2 || loaded.CompletedAt == nil {
		t.Fatalf("SQLite 重启后 group 状态/成员丢失: %#v err=%v", loaded, err)
	}
}

func TestSQLiteRuntimeDeliveryGroupTerminalMarkAtomicallyEnqueuesSettlement(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocationID := "inv-group-terminal-settlement-sqlite"
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: invocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []agentruntime.RuntimeDeliveryGroupMember{
		{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-terminal-settlement-sqlite", DeliveryID: "event-terminal-settlement-sqlite"},
		{Kind: agentruntime.RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-terminal-settlement-sqlite", DeliveryID: "checkpoint-terminal-settlement-sqlite"},
	}, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	if _, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	claimed, won, err := groupRepo.ClaimRuntimeDeliveryGroup(ctx, "group-worker", now, time.Minute)
	if err != nil || !won {
		_ = store.Close()
		t.Fatalf("claim group 失败: %#v won=%v err=%v", claimed, won, err)
	}
	commitRepo := repo.(agentruntime.RuntimeDeliveryGroupSettlementCommitRepository)
	if _, _, _, err := commitRepo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, claimed.ID, claimed.Revision, claimed.Members[0].Kind, claimed.Members[0].OutboxID, claimed.Members[0].DeliveryID, agentruntime.RuntimeDeliveryGroupMemberStatus("corrupt"), "", now); !errors.Is(err, agentruntime.ErrConflict) {
		_ = store.Close()
		t.Fatalf("非法 member status 必须拒绝且不能污染 group: %v", err)
	}
	unchanged, err := groupRepo.GetRuntimeDeliveryGroup(ctx, claimed.ID)
	if err != nil || unchanged.Revision != claimed.Revision || unchanged.Status != agentruntime.RuntimeDeliveryGroupProcessing || unchanged.Members[0].Status != agentruntime.RuntimeDeliveryGroupMemberPending {
		_ = store.Close()
		t.Fatalf("非法 member status 不应改变 group: %#v err=%v", unchanged, err)
	}
	first, duplicate, settlement, err := commitRepo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, claimed.ID, claimed.Revision, claimed.Members[0].Kind, claimed.Members[0].OutboxID, claimed.Members[0].DeliveryID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || settlement != nil || first.Status != agentruntime.RuntimeDeliveryGroupProcessing {
		_ = store.Close()
		t.Fatalf("非终态 member 不应提前登记 settlement: group=%#v duplicate=%v settlement=%#v err=%v", first, duplicate, settlement, err)
	}
	second, duplicate, settlement, err := commitRepo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, claimed.ID, first.Revision, claimed.Members[1].Kind, claimed.Members[1].OutboxID, claimed.Members[1].DeliveryID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || second.Status != agentruntime.RuntimeDeliveryGroupCompleted || settlement == nil {
		_ = store.Close()
		t.Fatalf("最后 member 应与 settlement 一起提交: group=%#v duplicate=%v settlement=%#v err=%v", second, duplicate, settlement, err)
	}
	if settlement.ID != agentruntime.RuntimeDeliveryGroupSettlementID(group.ID, agentruntime.RuntimeDeliveryGroupSettlementCommit) || settlement.Status != agentruntime.RuntimeDeliveryGroupSettlementQueued {
		_ = store.Close()
		t.Fatalf("settlement identity/status 错误: %#v", settlement)
	}
	settlementRepo := repo.(agentruntime.RuntimeDeliveryGroupSettlementRepository)
	stored, err := settlementRepo.GetRuntimeDeliveryGroupSettlement(ctx, settlement.ID)
	if err != nil || !stored.MatchesIdentity(*settlement) {
		_ = store.Close()
		t.Fatalf("settlement 未与 group 一起持久化: %#v err=%v", stored, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reopened := store.RuntimeRepository()
	reopenedGroup, err := reopened.(agentruntime.RuntimeDeliveryGroupRepository).GetRuntimeDeliveryGroup(ctx, group.ID)
	if err != nil || reopenedGroup.Status != agentruntime.RuntimeDeliveryGroupCompleted || reopenedGroup.Revision != second.Revision {
		t.Fatalf("SQLite 重启后 group 终态丢失: %#v err=%v", reopenedGroup, err)
	}
	reopenedSettlement, err := reopened.(agentruntime.RuntimeDeliveryGroupSettlementRepository).GetRuntimeDeliveryGroupSettlement(ctx, settlement.ID)
	if err != nil || reopenedSettlement.Status != agentruntime.RuntimeDeliveryGroupSettlementQueued || !reopenedSettlement.MatchesIdentity(*settlement) {
		t.Fatalf("SQLite 重启后 settlement 丢失或身份漂移: %#v err=%v", reopenedSettlement, err)
	}
	third, duplicate, repeated, err := reopened.(agentruntime.RuntimeDeliveryGroupSettlementCommitRepository).MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, reopenedGroup.ID, reopenedGroup.Revision, reopenedGroup.Members[1].Kind, reopenedGroup.Members[1].OutboxID, reopenedGroup.Members[1].DeliveryID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || !duplicate || repeated == nil || !third.MatchesIdentity(reopenedGroup) || repeated.ID != settlement.ID {
		t.Fatalf("终态重复 mark 应幂等返回同一 settlement: group=%#v duplicate=%v settlement=%#v err=%v", third, duplicate, repeated, err)
	}
}

func TestSQLiteRuntimeDeliveryGroupTerminalSettlementConflictRollsBackGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	invocationID := "inv-group-terminal-settlement-rollback"
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: invocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []agentruntime.RuntimeDeliveryGroupMember{
		{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-terminal-settlement-rollback", DeliveryID: "event-terminal-settlement-rollback"},
		{Kind: agentruntime.RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-terminal-settlement-rollback", DeliveryID: "checkpoint-terminal-settlement-rollback"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	if _, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	claimed, won, err := groupRepo.ClaimRuntimeDeliveryGroup(ctx, "rollback-worker", now, time.Minute)
	if err != nil || !won {
		t.Fatalf("claim group 失败: %#v won=%v err=%v", claimed, won, err)
	}
	commitRepo := repo.(agentruntime.RuntimeDeliveryGroupSettlementCommitRepository)
	first, duplicate, settlement, err := commitRepo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, claimed.ID, claimed.Revision, claimed.Members[0].Kind, claimed.Members[0].OutboxID, claimed.Members[0].DeliveryID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || settlement != nil {
		t.Fatalf("首个 member mark 异常: %#v duplicate=%v settlement=%#v err=%v", first, duplicate, settlement, err)
	}

	terminal := first
	for index := range terminal.Members {
		terminal.Members[index].Status = agentruntime.RuntimeDeliveryGroupMemberCompleted
	}
	terminal.Status = agentruntime.RuntimeDeliveryGroupCompleted
	terminal.LeaseOwner = ""
	terminal.LeaseExpiresAt = nil
	terminal.CompletedAt = &now
	conflicting, err := agentruntime.NewRuntimeDeliveryGroupSettlement(terminal, now)
	if err != nil {
		t.Fatal(err)
	}
	conflictRow, err := runtimeDeliveryGroupSettlementToRow(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	conflictRow.Source = "spoofed-source"
	if result := store.db.Create(conflictRow); result.Error != nil || result.RowsAffected != 1 {
		t.Fatalf("构造 settlement identity conflict 失败: rows=%d err=%v", result.RowsAffected, result.Error)
	}
	_, duplicate, settlement, err = commitRepo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, first.ID, first.Revision, first.Members[1].Kind, first.Members[1].OutboxID, first.Members[1].DeliveryID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if !errors.Is(err, agentruntime.ErrConflict) || duplicate || settlement != nil {
		t.Fatalf("settlement identity conflict 应拒绝且回滚: duplicate=%v settlement=%#v err=%v", duplicate, settlement, err)
	}
	rolledBack, err := groupRepo.GetRuntimeDeliveryGroup(ctx, group.ID)
	if err != nil || rolledBack.Status != agentruntime.RuntimeDeliveryGroupProcessing || rolledBack.Revision != first.Revision || rolledBack.Members[1].Status != agentruntime.RuntimeDeliveryGroupMemberPending {
		t.Fatalf("settlement 冲突后 group 必须保持可重试的旧状态: %#v err=%v", rolledBack, err)
	}
	if result := store.db.Where("id = ?", conflicting.ID).Delete(&runtimeDeliveryGroupSettlementRow{}); result.Error != nil || result.RowsAffected != 1 {
		t.Fatalf("清理冲突 settlement 失败: rows=%d err=%v", result.RowsAffected, result.Error)
	}
	completed, duplicate, settlement, err := commitRepo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, first.ID, first.Revision, first.Members[1].Kind, first.Members[1].OutboxID, first.Members[1].DeliveryID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || completed.Status != agentruntime.RuntimeDeliveryGroupCompleted || settlement == nil || settlement.ID != conflicting.ID {
		t.Fatalf("清理冲突后重试应完成 group+settlement: group=%#v duplicate=%v settlement=%#v err=%v", completed, duplicate, settlement, err)
	}
}

func TestSQLiteRuntimeDeliveryGroupTerminalMarkConcurrentCASHasOneWinner(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	invocationID := "inv-group-terminal-settlement-concurrent"
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: invocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []agentruntime.RuntimeDeliveryGroupMember{
		{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-terminal-settlement-concurrent", DeliveryID: "event-terminal-settlement-concurrent"},
		{Kind: agentruntime.RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-terminal-settlement-concurrent", DeliveryID: "checkpoint-terminal-settlement-concurrent"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	if _, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	claimed, won, err := groupRepo.ClaimRuntimeDeliveryGroup(ctx, "concurrent-worker", now, time.Minute)
	if err != nil || !won {
		t.Fatalf("claim group 失败: %#v won=%v err=%v", claimed, won, err)
	}
	commitRepo := repo.(agentruntime.RuntimeDeliveryGroupSettlementCommitRepository)
	type result struct {
		group      agentruntime.RuntimeDeliveryGroup
		duplicate  bool
		settlement *agentruntime.RuntimeDeliveryGroupSettlement
		err        error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, member := range claimed.Members {
		member := member
		wg.Add(1)
		go func() {
			defer wg.Done()
			updated, duplicate, settlement, markErr := commitRepo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, claimed.ID, claimed.Revision, member.Kind, member.OutboxID, member.DeliveryID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
			results <- result{group: updated, duplicate: duplicate, settlement: settlement, err: markErr}
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	conflicts := 0
	for item := range results {
		if item.err == nil && !item.duplicate {
			winners++
			if item.settlement != nil {
				t.Fatalf("并发首个 winner 不应提前创建 settlement: %#v", item.settlement)
			}
		} else if errors.Is(item.err, agentruntime.ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("并发 member mark 返回未预期结果: %#v", item)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("同 revision 并发 CAS 必须恰好一个 winner: winners=%d conflicts=%d", winners, conflicts)
	}
	latest, err := groupRepo.GetRuntimeDeliveryGroup(ctx, claimed.ID)
	if err != nil || latest.Status != agentruntime.RuntimeDeliveryGroupProcessing {
		t.Fatalf("并发 winner 后 group 应仍等待另一 member: %#v err=%v", latest, err)
	}
	completedMembers := 0
	for _, member := range latest.Members {
		if member.Status == agentruntime.RuntimeDeliveryGroupMemberCompleted {
			completedMembers++
		}
	}
	if completedMembers != 1 {
		t.Fatalf("并发 CAS 不应丢失或重复 member 更新: %#v", latest.Members)
	}
}

func TestSQLiteRuntimeDeliveryGroupGlobalClaimRepairsTerminalAggregateWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	invocationID := "inv-group-global-repair-sqlite"
	if err := repo.CreateInvocation(ctx, agentruntime.Invocation{ID: invocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []agentruntime.RuntimeDeliveryGroupMember{
		{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-global-repair-sqlite", DeliveryID: "event-global-repair-sqlite"},
		{Kind: agentruntime.RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-global-repair-sqlite", DeliveryID: "checkpoint-global-repair-sqlite"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	if _, err := groupRepo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}

	var row runtimeDeliveryGroupRow
	if err := store.db.Where("id = ?", group.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	var members []agentruntime.RuntimeDeliveryGroupMember
	if err := json.Unmarshal([]byte(row.MembersJSON), &members); err != nil {
		t.Fatal(err)
	}
	for index := range members {
		members[index].Status = agentruntime.RuntimeDeliveryGroupMemberCompleted
		members[index].UpdatedAt = now
	}
	encodedMembers, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	leaseExpires := now.Add(time.Minute)
	staleRevision := row.Revision + 8
	if result := store.db.Model(&runtimeDeliveryGroupRow{}).Where("id = ? AND revision = ?", group.ID, row.Revision).Updates(map[string]any{
		"members_json": string(encodedMembers), "status": string(agentruntime.RuntimeDeliveryGroupProcessing), "attempt": 3,
		"revision": staleRevision, "lease_owner": "old-worker", "lease_expires_at": &leaseExpires, "completed_at": nil,
	}); result.Error != nil || result.RowsAffected != 1 {
		t.Fatalf("构造 SQLite stale aggregate 失败: rows=%d err=%v", result.RowsAffected, result.Error)
	}

	if claimed, won, err := groupRepo.ClaimRuntimeDeliveryGroup(ctx, "new-worker", now, time.Minute); err != nil || won || claimed.ID != "" {
		t.Fatalf("SQLite 全局 claim 应先修复终态 aggregate: claimed=%#v won=%v err=%v", claimed, won, err)
	}
	stored, err := groupRepo.GetRuntimeDeliveryGroup(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != agentruntime.RuntimeDeliveryGroupCompleted || stored.Attempt != 3 || stored.LeaseOwner != "" || stored.LeaseExpiresAt != nil || stored.CompletedAt == nil || stored.Revision != staleRevision+1 {
		t.Fatalf("SQLite completed aggregate 修复错误: %#v", stored)
	}
}

func TestSQLiteSnapshotCheckpointCommitRegistersRuntimeDeliveryGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-group-snapshot-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	commitRepo := repo.(agentruntime.RuntimeSnapshotCheckpointCommitRepository)
	saved, event, checkpoint, err := commitRepo.CommitRuntimeSnapshotWithCheckpoint(ctx, sqliteCheckpointSnapshot(invocation.ID, 1, now), agentruntime.AgentEvent{ID: "group-snapshot-sqlite-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || event.Sequence != 1 || checkpoint.ID == "" {
		t.Fatalf("snapshot/checkpoint 提交结果错误: snapshot=%#v event=%#v checkpoint=%#v", saved, event, checkpoint)
	}
	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	groups, err := groupRepo.ListRuntimeDeliveryGroups(ctx, invocation.ID, "", 10)
	if err != nil || len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("SQLite snapshot/checkpoint 未注册 group: %#v err=%v", groups, err)
	}
	eventOutbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	hasEvent, hasCheckpoint := false, false
	for _, member := range groups[0].Members {
		if member.Kind == agentruntime.RuntimeDeliveryKindEvent && member.OutboxID == eventOutbox.ID && member.DeliveryID == event.ID {
			hasEvent = true
		}
		if member.Kind == agentruntime.RuntimeDeliveryKindCheckpoint && member.OutboxID == checkpoint.ID && member.DeliveryID == checkpoint.DeliveryID {
			hasCheckpoint = true
		}
	}
	if !hasEvent || !hasCheckpoint {
		t.Fatalf("SQLite group member identity 错误: %#v", groups[0].Members)
	}
	eventOutboxRepo := repo.(agentruntime.RuntimeEventOutboxRepository)
	storedEvent, err := eventOutboxRepo.GetRuntimeEventOutbox(ctx, eventOutbox.ID)
	if err != nil || storedEvent.GroupID != groups[0].ID || checkpoint.GroupID != groups[0].ID {
		t.Fatalf("SQLite outbox 未保存 group ID: event=%#v checkpoint=%#v group=%s err=%v", storedEvent, checkpoint, groups[0].ID, err)
	}
}

func TestSQLiteRuntimeDeliveryGroupReconcileDeferralSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	invocation := agentruntime.Invocation{ID: "inv-group-reconcile-sqlite", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	_, event, checkpoint, err := repo.(agentruntime.RuntimeSnapshotCheckpointCommitRepository).CommitRuntimeSnapshotWithCheckpoint(ctx, sqliteCheckpointSnapshot(invocation.ID, 1, now), agentruntime.AgentEvent{ID: "group-reconcile-sqlite-event", InvocationID: invocation.ID, Type: agentruntime.EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	eventOutbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	eventRepo := repo.(agentruntime.RuntimeEventOutboxRepository)
	claimedEvent, ok, err := eventRepo.ClaimRuntimeEventOutbox(ctx, "sqlite-event-reconcile", now, time.Minute)
	if err != nil || !ok || claimedEvent.ID != eventOutbox.ID {
		_ = store.Close()
		t.Fatalf("SQLite event claim 失败: %#v ok=%v err=%v", claimedEvent, ok, err)
	}
	if completed, err := eventRepo.CompleteRuntimeEventOutbox(ctx, claimedEvent.ID, claimedEvent.LeaseOwner, now); err != nil || !completed {
		_ = store.Close()
		t.Fatalf("SQLite event complete 失败: completed=%v err=%v", completed, err)
	}

	groupRepo := repo.(agentruntime.RuntimeDeliveryGroupRepository)
	groups, err := groupRepo.ListRuntimeDeliveryGroups(ctx, invocation.ID, "", 10)
	if err != nil || len(groups) != 1 {
		_ = store.Close()
		t.Fatalf("SQLite group 缺失: %#v err=%v", groups, err)
	}
	// Claim/defer directly exercises the durable group lease contract.  The
	// coordinator-level targeted reconciliation is covered by Memory, while
	// this test proves its restart boundary and exact attempt preservation.
	claimer := groupRepo.(agentruntime.RuntimeDeliveryGroupByIDClaimer)
	claimedGroup, won, err := claimer.ClaimRuntimeDeliveryGroupByID(ctx, groups[0].ID, "sqlite-group-reconcile", now, time.Minute)
	if err != nil || !won {
		_ = store.Close()
		t.Fatalf("SQLite targeted group claim 失败: %#v won=%v err=%v", claimedGroup, won, err)
	}
	marked, duplicate, err := groupRepo.MarkRuntimeDeliveryGroupMember(ctx, claimedGroup.ID, claimedGroup.Revision, agentruntime.RuntimeDeliveryKindEvent, eventOutbox.ID, event.ID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || marked.Status != agentruntime.RuntimeDeliveryGroupProcessing {
		_ = store.Close()
		t.Fatalf("SQLite group member mark 错误: %#v duplicate=%v err=%v", marked, duplicate, err)
	}
	deferrer := groupRepo.(agentruntime.RuntimeDeliveryGroupDeferrer)
	if deferred, err := deferrer.DeferRuntimeDeliveryGroup(ctx, claimedGroup.ID, claimedGroup.LeaseOwner, now, now.Add(time.Second), "等待 checkpoint outbox"); err != nil || !deferred {
		_ = store.Close()
		t.Fatalf("SQLite group defer 失败: deferred=%v err=%v", deferred, err)
	}
	deferredGroup, err := groupRepo.GetRuntimeDeliveryGroup(ctx, groups[0].ID)
	if err != nil || deferredGroup.Status != agentruntime.RuntimeDeliveryGroupQueued || deferredGroup.Attempt != 0 || !deferredGroup.AvailableAt.After(now) {
		_ = store.Close()
		t.Fatalf("SQLite group defer 应保留 attempt=0: %#v err=%v", deferredGroup, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reopened := store.RuntimeRepository()
	reopenedGroups, err := reopened.(agentruntime.RuntimeDeliveryGroupRepository).ListRuntimeDeliveryGroups(ctx, invocation.ID, "", 10)
	if err != nil || len(reopenedGroups) != 1 || reopenedGroups[0].Attempt != 0 || reopenedGroups[0].Status != agentruntime.RuntimeDeliveryGroupQueued {
		t.Fatalf("SQLite 重启后 group defer 状态丢失: %#v err=%v", reopenedGroups, err)
	}
	claimNow := reopenedGroups[0].AvailableAt.Add(time.Microsecond)
	checkpointRepo := reopened.(agentruntime.RuntimeCheckpointDeliveryOutboxRepository)
	claimedCheckpoint, ok, err := checkpointRepo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "sqlite-checkpoint-reconcile", claimNow, time.Minute)
	if err != nil || !ok || claimedCheckpoint.ID != checkpoint.ID {
		t.Fatalf("SQLite checkpoint claim 失败: %#v ok=%v err=%v", claimedCheckpoint, ok, err)
	}
	if completed, err := checkpointRepo.CompleteRuntimeCheckpointDeliveryOutbox(ctx, claimedCheckpoint.ID, claimedCheckpoint.LeaseOwner, claimNow); err != nil || !completed {
		t.Fatalf("SQLite checkpoint complete 失败: completed=%v err=%v", completed, err)
	}
	groupRepo = reopened.(agentruntime.RuntimeDeliveryGroupRepository)
	claimedGroup, won, err = reopened.(agentruntime.RuntimeDeliveryGroupByIDClaimer).ClaimRuntimeDeliveryGroupByID(ctx, reopenedGroups[0].ID, "sqlite-group-reconcile-2", claimNow, time.Minute)
	if err != nil || !won {
		t.Fatalf("SQLite 重启后 targeted group claim 失败: %#v won=%v err=%v", claimedGroup, won, err)
	}
	marked, duplicate, err = groupRepo.MarkRuntimeDeliveryGroupMember(ctx, claimedGroup.ID, claimedGroup.Revision, agentruntime.RuntimeDeliveryKindCheckpoint, checkpoint.ID, checkpoint.DeliveryID, agentruntime.RuntimeDeliveryGroupMemberCompleted, "", claimNow)
	if err != nil || duplicate || marked.Status != agentruntime.RuntimeDeliveryGroupCompleted {
		t.Fatalf("SQLite 重启后最后 member 未完成 group: %#v duplicate=%v err=%v", marked, duplicate, err)
	}
}
