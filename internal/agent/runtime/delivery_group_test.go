package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeDeliveryGroupIdentityValidationAndMemoryLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	members := []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-outbox", DeliveryID: "checkpoint-delivery"},
		{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-outbox", DeliveryID: "event-id"},
	}
	group, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-memory", members, now)
	if err != nil {
		t.Fatal(err)
	}
	if group.ID != RuntimeDeliveryGroupID("runtime-a", "runtime-b", "inv-group-memory", members) || group.Status != RuntimeDeliveryGroupQueued || len(group.Members) != 2 {
		t.Fatalf("group identity/default 错误: %#v", group)
	}
	if group.Members[0].Kind != RuntimeDeliveryKindCheckpoint {
		t.Fatalf("members 应按稳定 identity 排序: %#v", group.Members)
	}
	reordered, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-memory", []RuntimeDeliveryGroupMember{members[1], members[0]}, now)
	if err != nil || reordered.ID != group.ID {
		t.Fatalf("member 顺序不应改变 group ID: %#v err=%v", reordered, err)
	}
	invalid := group
	invalid.Members = append(invalid.Members, invalid.Members[0])
	if _, err := invalid.Normalize(now); !errors.Is(err, ErrInvalidRuntimeDeliveryGroup) {
		t.Fatalf("重复 member 必须拒绝: %v", err)
	}

	repo := NewMemoryRepository()
	if err := repo.CreateInvocation(ctx, Invocation{ID: "inv-group-memory", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.EnqueueRuntimeDeliveryGroup(ctx, group)
	if err != nil || !stored.MatchesIdentity(group) {
		t.Fatalf("enqueue group 失败: %#v err=%v", stored, err)
	}
	duplicate, err := repo.EnqueueRuntimeDeliveryGroup(ctx, group)
	if err != nil || !duplicate.MatchesIdentity(group) {
		t.Fatalf("重复 enqueue 应幂等: %#v err=%v", duplicate, err)
	}
	claimed, ok, err := repo.ClaimRuntimeDeliveryGroup(ctx, "group-worker", now, time.Minute)
	if err != nil || !ok || claimed.Status != RuntimeDeliveryGroupProcessing || claimed.Attempt != 1 || claimed.LeaseOwner != "group-worker" {
		t.Fatalf("claim group 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	if done, err := repo.CompleteRuntimeDeliveryGroup(ctx, claimed.ID, claimed.LeaseOwner, now); !errors.Is(err, ErrConflict) || done {
		t.Fatalf("未完成 members 的 group 不应 complete: done=%v err=%v", done, err)
	}
	marked, duplicateMark, err := repo.MarkRuntimeDeliveryGroupMember(ctx, claimed.ID, claimed.Revision, RuntimeDeliveryKindEvent, "event-outbox", "event-id", RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicateMark || marked.Status != RuntimeDeliveryGroupProcessing || marked.Revision != claimed.Revision+1 {
		t.Fatalf("标记第一个 member 错误: %#v duplicate=%v err=%v", marked, duplicateMark, err)
	}
	if _, _, err := repo.MarkRuntimeDeliveryGroupMember(ctx, claimed.ID, claimed.Revision, RuntimeDeliveryKindCheckpoint, "checkpoint-outbox", "checkpoint-delivery", RuntimeDeliveryGroupMemberCompleted, "", now); !errors.Is(err, ErrConflict) {
		t.Fatalf("过期 revision 必须拒绝: %v", err)
	}
	completed, duplicateMark, err := repo.MarkRuntimeDeliveryGroupMember(ctx, claimed.ID, marked.Revision, RuntimeDeliveryKindCheckpoint, "checkpoint-outbox", "checkpoint-delivery", RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicateMark || completed.Status != RuntimeDeliveryGroupCompleted || completed.CompletedAt == nil {
		t.Fatalf("最后一个 member 应完成 group: %#v duplicate=%v err=%v", completed, duplicateMark, err)
	}
	if done, err := repo.CompleteRuntimeDeliveryGroup(ctx, claimed.ID, claimed.LeaseOwner, now); err != nil || done {
		t.Fatalf("自动完成后的 group 不应被旧 lease 再次 complete: done=%v err=%v", done, err)
	}
}

func TestRuntimeDeliveryGroupMemoryCASAndRetryBound(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	if err := repo.CreateInvocation(ctx, Invocation{ID: "inv-group-cas", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	group, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-cas", []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-cas", DeliveryID: "event-cas"},
		{Kind: RuntimeDeliveryKindConfig, OutboxID: "config-cas", DeliveryID: "config-cas"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := repo.ClaimRuntimeDeliveryGroup(ctx, "worker-a", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim 失败: %#v ok=%v err=%v", claimed, ok, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, member := range []RuntimeDeliveryGroupMember{claimed.Members[0], claimed.Members[1]} {
		wg.Add(1)
		go func(member RuntimeDeliveryGroupMember) {
			defer wg.Done()
			_, _, markErr := repo.MarkRuntimeDeliveryGroupMember(ctx, claimed.ID, claimed.Revision, member.Kind, member.OutboxID, member.DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
			results <- markErr
		}(member)
	}
	wg.Wait()
	close(results)
	var successes, conflicts int
	for markErr := range results {
		switch {
		case markErr == nil:
			successes++
		case errors.Is(markErr, ErrConflict):
			conflicts++
		default:
			t.Fatalf("CAS 返回意外错误: %v", markErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("同一 revision 只能一个 member 更新成功: successes=%d conflicts=%d", successes, conflicts)
	}

	// A fresh group exercises retry and bounded attempt exhaustion without
	// mutating private storage state.
	retryRepo := NewMemoryRepository()
	retryGroup, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-retry", []RuntimeDeliveryGroupMember{{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-retry", DeliveryID: "event-retry"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := retryRepo.CreateInvocation(ctx, Invocation{ID: "inv-group-retry", UserID: "user", ConversationID: "conversation-2", SessionID: "session-2", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := retryRepo.EnqueueRuntimeDeliveryGroup(ctx, retryGroup); err != nil {
		t.Fatal(err)
	}
	claimAt := now
	for attempt := 1; attempt <= MaxRuntimeDeliveryGroupAttempts; attempt++ {
		claimed, ok, err := retryRepo.ClaimRuntimeDeliveryGroup(ctx, "retry-worker", claimAt, time.Minute)
		if err != nil || !ok {
			t.Fatalf("第 %d 次 claim 失败: %#v ok=%v err=%v", attempt, claimed, ok, err)
		}
		retried, err := retryRepo.RetryRuntimeDeliveryGroup(ctx, claimed.ID, claimed.LeaseOwner, claimAt, "api_key=secret-value")
		if err != nil || !retried {
			t.Fatalf("第 %d 次 retry 失败: retried=%v err=%v", attempt, retried, err)
		}
		stored, err := retryRepo.GetRuntimeDeliveryGroup(ctx, claimed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < MaxRuntimeDeliveryGroupAttempts && stored.Status != RuntimeDeliveryGroupQueued {
			t.Fatalf("第 %d 次 retry 后应 queued: %#v", attempt, stored)
		}
		if attempt == MaxRuntimeDeliveryGroupAttempts && stored.Status != RuntimeDeliveryGroupFailed {
			t.Fatalf("达到上限后应 failed: %#v", stored)
		}
		if stored.LastError == "" || stored.LastError == "api_key=secret-value" {
			t.Fatalf("错误信息必须脱敏: %#v", stored)
		}
		claimAt = stored.AvailableAt.Add(time.Nanosecond)
	}
	if _, ok, err := retryRepo.ClaimRuntimeDeliveryGroup(ctx, "retry-worker", claimAt, time.Minute); err != nil || ok {
		t.Fatalf("failed group 不应再次 claim: ok=%v err=%v", ok, err)
	}
}

func TestRuntimeDeliveryGroupGlobalClaimRepairsTerminalAggregateWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocationID := "inv-group-global-repair"
	if err := repo.CreateInvocation(ctx, Invocation{ID: invocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	completedGroup, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-global-repair", DeliveryID: "event-global-repair"},
		{Kind: RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-global-repair", DeliveryID: "checkpoint-global-repair"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, completedGroup); err != nil {
		t.Fatal(err)
	}
	failedGroup, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-global-failed", DeliveryID: "event-global-failed"},
		{Kind: RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-global-failed", DeliveryID: "checkpoint-global-failed"},
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, failedGroup); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after member CAS updates but before the aggregate row was
	// persisted. Both rows carry a live lease and a non-zero attempt; repair
	// must win before normal next-due claiming and leave the attempt untouched.
	leaseExpires := now.Add(time.Minute)
	repo.mu.Lock()
	staleCompleted := repo.deliveryGroups[completedGroup.ID]
	staleCompleted.Status = RuntimeDeliveryGroupProcessing
	staleCompleted.Attempt = 3
	staleCompleted.Revision = 9
	staleCompleted.LeaseOwner = "old-worker"
	staleCompleted.LeaseExpiresAt = &leaseExpires
	staleCompleted.CompletedAt = nil
	for index := range staleCompleted.Members {
		staleCompleted.Members[index].Status = RuntimeDeliveryGroupMemberCompleted
		staleCompleted.Members[index].UpdatedAt = now
	}
	repo.deliveryGroups[completedGroup.ID] = staleCompleted

	staleFailed := repo.deliveryGroups[failedGroup.ID]
	staleFailed.Status = RuntimeDeliveryGroupProcessing
	staleFailed.Attempt = 4
	staleFailed.Revision = 11
	staleFailed.LeaseOwner = "old-worker"
	staleFailed.LeaseExpiresAt = &leaseExpires
	staleFailed.CompletedAt = nil
	staleFailed.Members[0].Status = RuntimeDeliveryGroupMemberFailed
	staleFailed.Members[0].LastError = "remote delivery rejected"
	staleFailed.Members[0].UpdatedAt = now
	staleFailed.Members[1].Status = RuntimeDeliveryGroupMemberCompleted
	staleFailed.Members[1].UpdatedAt = now
	repo.deliveryGroups[failedGroup.ID] = staleFailed
	repo.mu.Unlock()

	if claimed, won, err := repo.ClaimRuntimeDeliveryGroup(ctx, "new-worker", now, time.Minute); err != nil || won || claimed.ID != "" {
		t.Fatalf("全局 claim 应先修复终态 aggregate 而不抢租约: claimed=%#v won=%v err=%v", claimed, won, err)
	}
	storedCompleted, err := repo.GetRuntimeDeliveryGroup(ctx, completedGroup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedCompleted.Status != RuntimeDeliveryGroupCompleted || storedCompleted.Attempt != 3 || storedCompleted.LeaseOwner != "" || storedCompleted.LeaseExpiresAt != nil || storedCompleted.CompletedAt == nil || storedCompleted.Revision != 10 {
		t.Fatalf("completed aggregate 修复错误: %#v", storedCompleted)
	}
	storedFailed, err := repo.GetRuntimeDeliveryGroup(ctx, failedGroup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedFailed.Status != RuntimeDeliveryGroupFailed || storedFailed.Attempt != 4 || storedFailed.LeaseOwner != "" || storedFailed.LeaseExpiresAt != nil || storedFailed.CompletedAt != nil || storedFailed.Revision != 12 || storedFailed.LastError != "remote delivery rejected" {
		t.Fatalf("failed aggregate 修复错误: %#v", storedFailed)
	}
}

func TestRuntimeDeliveryGroupRegisteredBySnapshotAndConfigTransactions(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-group-snapshot", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, event, checkpoint, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "group-snapshot-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := repo.ListRuntimeDeliveryGroups(ctx, invocation.ID, "", 10)
	if err != nil || len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("snapshot/checkpoint 未注册 delivery group: %#v err=%v", groups, err)
	}
	group := groups[0]
	eventOutbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	hasEventMember := false
	hasCheckpointMember := false
	for _, member := range group.Members {
		if member.Kind == RuntimeDeliveryKindEvent && member.OutboxID == eventOutbox.ID && member.DeliveryID == event.ID {
			hasEventMember = true
		}
		if member.Kind == RuntimeDeliveryKindCheckpoint && member.OutboxID == checkpoint.ID && member.DeliveryID == checkpoint.DeliveryID {
			hasCheckpointMember = true
		}
	}
	if !hasEventMember || !hasCheckpointMember {
		t.Fatalf("group members 未绑定稳定 delivery identity: %#v", group.Members)
	}
	storedEventOutbox, err := repo.GetRuntimeEventOutbox(ctx, eventOutbox.ID)
	if err != nil || storedEventOutbox.GroupID != group.ID || checkpoint.GroupID != group.ID {
		t.Fatalf("snapshot/checkpoint outbox 未保存 group ID: event=%#v checkpoint=%#v group=%s err=%v", storedEventOutbox, checkpoint, group.ID, err)
	}

	old := migrationSnapshot(t, "group-config-old", "demo", "model")
	target := migrationSnapshot(t, "group-config-target", "demo", "model")
	configInvocation := Invocation{ID: "inv-group-config", UserID: "user", ConversationID: "conversation-config", SessionID: "session-config", ConfigSnapshot: old, Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, configInvocation); err != nil {
		t.Fatal(err)
	}
	oldDigest := runtimeSnapshotDigestForTest(old)
	targetDigest := runtimeSnapshotDigestForTest(target)
	_, _, delivery, err := repo.CommitRuntimeConfigMigrationWithDelivery(ctx, RuntimeConfigMigrationCommit{InvocationID: configInvocation.ID, ExpectedDigest: oldDigest, NewSnapshot: target, IdempotencyKey: "group-config", Event: AgentEvent{ID: RuntimeConfigMigrationEventID(configInvocation.ID, oldDigest, targetDigest, "group-config"), Timestamp: now}}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	groups, err = repo.ListRuntimeDeliveryGroups(ctx, configInvocation.ID, "", 10)
	if err != nil || len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("config migration 未注册 delivery group: %#v err=%v", groups, err)
	}
	if groups[0].Members[0].DeliveryID != delivery.DeliveryID && groups[0].Members[1].DeliveryID != delivery.DeliveryID {
		t.Fatalf("config group 未绑定 config delivery: %#v", groups[0])
	}
	storedConfig, err := repo.GetRuntimeConfigDeliveryOutbox(ctx, delivery.ID)
	if err != nil || storedConfig.GroupID != groups[0].ID {
		t.Fatalf("config outbox 未保存 group ID: %#v group=%s err=%v", storedConfig, groups[0].ID, err)
	}
}

func TestCoordinatorCompletesRuntimeDeliveryGroupAfterMemberAcks(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 15, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-group-dispatch", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, _, checkpoint, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "group-dispatch-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{
		repo: repo, eventOutboxWorkerID: "event-group-worker", checkpointOutboxWorkerID: "checkpoint-group-worker",
		eventOutboxHandler: func(context.Context, RuntimeEventOutbox, AgentEvent) error { return nil },
		checkpointDeliveryTransport: checkpointDeliveryTransportFunc(func(context.Context, RuntimeCheckpointDeliveryEnvelope, RuntimeCheckpointProjection) (RuntimeCheckpointDeliveryReceipt, error) {
			return RuntimeCheckpointDeliveryReceipt{Version: RuntimeCheckpointDeliveryEnvelopeVersion, DeliveryID: checkpoint.DeliveryID, InvocationID: invocation.ID, SnapshotRevision: checkpoint.SnapshotRevision, EventSequence: checkpoint.EventSequence, SnapshotDigest: checkpoint.SnapshotDigest}, nil
		}),
		checkpointDeliverySource: "runtime-a", checkpointDeliveryDestination: "runtime-b",
	}
	coordinator.DispatchDueRuntimeDeliveries(ctx, now)
	groups, err := repo.ListRuntimeDeliveryGroups(ctx, invocation.ID, RuntimeDeliveryGroupCompleted, 10)
	if err != nil || len(groups) != 1 || groups[0].Status != RuntimeDeliveryGroupCompleted {
		t.Fatalf("dispatcher 成员确认后 group 应完成: %#v err=%v", groups, err)
	}
	for _, member := range groups[0].Members {
		if member.Status != RuntimeDeliveryGroupMemberCompleted {
			t.Fatalf("group member 未完成: %#v", groups[0])
		}
	}
}

func TestCoordinatorReconcilesRuntimeDeliveryGroupWithTargetedLeaseAndDeferral(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 16, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocation := Invocation{ID: "inv-group-reconcile", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	_, event, checkpoint, err := repo.CommitRuntimeSnapshotWithCheckpoint(ctx, checkpointDeliveryTestSnapshot(t, invocation.ID, now), AgentEvent{ID: "group-reconcile-event", InvocationID: invocation.ID, Type: EventRuntimeSnapshot, Timestamp: now}, "runtime-a", "runtime-b")
	if err != nil {
		t.Fatal(err)
	}
	eventOutbox, err := NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	claimedEvent, ok, err := repo.ClaimRuntimeEventOutbox(ctx, "event-reconcile-worker", now, time.Minute)
	if err != nil || !ok || claimedEvent.ID != eventOutbox.ID {
		t.Fatalf("event outbox claim 失败: %#v ok=%v err=%v", claimedEvent, ok, err)
	}
	if completed, err := repo.CompleteRuntimeEventOutbox(ctx, claimedEvent.ID, claimedEvent.LeaseOwner, now); err != nil || !completed {
		t.Fatalf("event outbox complete 失败: completed=%v err=%v", completed, err)
	}

	coordinator := &Coordinator{repo: repo, deliveryGroupWorkerID: "group-reconcile-worker"}
	coordinator.ReconcileRuntimeDeliveryGroups(ctx, now)
	var groupRepo RuntimeDeliveryGroupRepository = repo
	groups, err := groupRepo.ListRuntimeDeliveryGroups(ctx, invocation.ID, "", 10)
	if err != nil || len(groups) != 1 {
		t.Fatalf("reconcile 后 group 缺失: %#v err=%v", groups, err)
	}
	deferred := groups[0]
	if deferred.Status != RuntimeDeliveryGroupQueued || deferred.Attempt != 0 || !deferred.AvailableAt.After(now) {
		t.Fatalf("等待 checkpoint 时必须释放 group lease 且不消耗 attempt: %#v", deferred)
	}
	if deferred.LastError == "" || strings.Contains(deferred.LastError, "api_key") {
		t.Fatalf("group 等待原因应有界且脱敏: %#v", deferred)
	}

	claimNow := deferred.AvailableAt.Add(time.Nanosecond)
	claimedCheckpoint, ok, err := repo.ClaimRuntimeCheckpointDeliveryOutbox(ctx, "checkpoint-reconcile-worker", claimNow, time.Minute)
	if err != nil || !ok || claimedCheckpoint.ID != checkpoint.ID {
		t.Fatalf("checkpoint outbox claim 失败: %#v ok=%v err=%v", claimedCheckpoint, ok, err)
	}
	if completed, err := repo.CompleteRuntimeCheckpointDeliveryOutbox(ctx, claimedCheckpoint.ID, claimedCheckpoint.LeaseOwner, claimNow); err != nil || !completed {
		t.Fatalf("checkpoint outbox complete 失败: completed=%v err=%v", completed, err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			coordinator.ReconcileRuntimeDeliveryGroups(ctx, claimNow)
		}()
	}
	wg.Wait()
	groups, err = groupRepo.ListRuntimeDeliveryGroups(ctx, invocation.ID, RuntimeDeliveryGroupCompleted, 10)
	if err != nil || len(groups) != 1 || groups[0].Attempt != 1 {
		t.Fatalf("终态 group 应只由一个 targeted lease 协调完成: %#v err=%v", groups, err)
	}
	for _, member := range groups[0].Members {
		if member.Status != RuntimeDeliveryGroupMemberCompleted {
			t.Fatalf("终态 group member 未收口: %#v", groups[0])
		}
	}
}

func TestMemoryRuntimeDeliveryGroupTerminalMarkAtomicallyEnqueuesSettlement(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)
	repo := NewMemoryRepository()
	invocationID := "inv-group-terminal-settlement-memory"
	if err := repo.CreateInvocation(ctx, Invocation{ID: invocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	group, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-terminal-settlement", DeliveryID: "event-terminal-settlement"},
		{Kind: RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-terminal-settlement", DeliveryID: "checkpoint-terminal-settlement"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	claimed, won, err := repo.ClaimRuntimeDeliveryGroup(ctx, "group-worker", now, time.Minute)
	if err != nil || !won {
		t.Fatalf("claim group 失败: %#v won=%v err=%v", claimed, won, err)
	}
	first, duplicate, settlement, err := repo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, claimed.ID, claimed.Revision, claimed.Members[0].Kind, claimed.Members[0].OutboxID, claimed.Members[0].DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || settlement != nil || first.Status != RuntimeDeliveryGroupProcessing {
		t.Fatalf("非终态 member 不应提前登记 settlement: group=%#v duplicate=%v settlement=%#v err=%v", first, duplicate, settlement, err)
	}
	second, duplicate, settlement, err := repo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, claimed.ID, first.Revision, claimed.Members[1].Kind, claimed.Members[1].OutboxID, claimed.Members[1].DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || duplicate || second.Status != RuntimeDeliveryGroupCompleted || settlement == nil {
		t.Fatalf("最后 member 应与 settlement 一起提交: group=%#v duplicate=%v settlement=%#v err=%v", second, duplicate, settlement, err)
	}
	if settlement.ID != RuntimeDeliveryGroupSettlementID(group.ID, RuntimeDeliveryGroupSettlementCommit) || settlement.Status != RuntimeDeliveryGroupSettlementQueued {
		t.Fatalf("settlement identity/status 错误: %#v", settlement)
	}
	stored, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, settlement.ID)
	if err != nil || stored.ID != settlement.ID {
		t.Fatalf("settlement 未持久化: %#v err=%v", stored, err)
	}
	third, duplicate, repeated, err := repo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, second.ID, second.Revision, second.Members[1].Kind, second.Members[1].OutboxID, second.Members[1].DeliveryID, RuntimeDeliveryGroupMemberCompleted, "", now)
	if err != nil || !duplicate || repeated == nil || !third.MatchesIdentity(second) {
		t.Fatalf("终态重复 mark 应只幂等补查 settlement: group=%#v duplicate=%v settlement=%#v err=%v", third, duplicate, repeated, err)
	}

	failedInvocationID := "inv-group-terminal-settlement-memory-failed"
	if err := repo.CreateInvocation(ctx, Invocation{ID: failedInvocationID, UserID: "user", ConversationID: "conversation", SessionID: "session", Status: InvocationWaitingTool, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	failedGroup, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", failedInvocationID, []RuntimeDeliveryGroupMember{
		{Kind: RuntimeDeliveryKindEvent, OutboxID: "event-terminal-settlement-memory-failed", DeliveryID: "event-terminal-settlement-memory-failed"},
		{Kind: RuntimeDeliveryKindCheckpoint, OutboxID: "checkpoint-terminal-settlement-memory-failed", DeliveryID: "checkpoint-terminal-settlement-memory-failed"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueRuntimeDeliveryGroup(ctx, failedGroup); err != nil {
		t.Fatal(err)
	}
	failedClaimed, won, err := repo.ClaimRuntimeDeliveryGroup(ctx, "group-worker-failed", now, time.Minute)
	if err != nil || !won {
		t.Fatalf("failed group claim 失败: %#v won=%v err=%v", failedClaimed, won, err)
	}
	failedMember, duplicate, abortSettlement, err := repo.MarkRuntimeDeliveryGroupMemberWithSettlement(ctx, failedClaimed.ID, failedClaimed.Revision, failedClaimed.Members[0].Kind, failedClaimed.Members[0].OutboxID, failedClaimed.Members[0].DeliveryID, RuntimeDeliveryGroupMemberFailed, "remote rejected", now)
	if err != nil || duplicate || failedMember.Status != RuntimeDeliveryGroupFailed || abortSettlement == nil || abortSettlement.Phase != RuntimeDeliveryGroupSettlementAbort || abortSettlement.Status != RuntimeDeliveryGroupSettlementQueued {
		t.Fatalf("failed member 应登记 abort settlement: group=%#v duplicate=%v settlement=%#v err=%v", failedMember, duplicate, abortSettlement, err)
	}
}
