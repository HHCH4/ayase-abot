package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeDeliveryGroupSettlementIsDurableAndLeaseProtected(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-settlement-sqlite", []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-settlement-sqlite", DeliveryID: "event-settlement-sqlite"}}, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	group.Members[0].Status = agentruntime.RuntimeDeliveryGroupMemberCompleted
	group.Status = agentruntime.RuntimeDeliveryGroupCompleted
	group.CompletedAt = &now
	settlement, err := agentruntime.NewRuntimeDeliveryGroupSettlement(group, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupSettlementRepository)
	if stored, err := repo.EnqueueRuntimeDeliveryGroupSettlement(ctx, settlement); err != nil || stored.ID != settlement.ID {
		_ = store.Close()
		t.Fatalf("SQLite settlement enqueue 异常: %#v err=%v", stored, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo = store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupSettlementRepository)
	claimed, won, err := repo.ClaimRuntimeDeliveryGroupSettlement(ctx, "worker-a", now.Add(time.Second), time.Minute)
	if err != nil || !won || claimed.Attempt != 1 || claimed.Status != agentruntime.RuntimeDeliveryGroupSettlementProcessing {
		t.Fatalf("SQLite settlement claim 异常: %#v won=%v err=%v", claimed, won, err)
	}
	if completed, err := repo.CompleteRuntimeDeliveryGroupSettlement(ctx, claimed.ID, "worker-b", now.Add(2*time.Second)); err != nil || completed {
		t.Fatalf("错误 owner 不应 complete SQLite settlement: completed=%v err=%v", completed, err)
	}
	if retried, err := repo.RetryRuntimeDeliveryGroupSettlement(ctx, claimed.ID, "worker-a", now.Add(2*time.Second), "远程暂不可达"); err != nil || !retried {
		t.Fatalf("SQLite settlement retry 异常: retried=%v err=%v", retried, err)
	}
	queued, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, claimed.ID)
	if err != nil || queued.Status != agentruntime.RuntimeDeliveryGroupSettlementQueued || queued.LastError != "远程暂不可达" {
		t.Fatalf("SQLite settlement retry 状态错误: %#v err=%v", queued, err)
	}
}

func TestSQLiteRuntimeDeliveryGroupSettlementProgressSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-settlement-progress-sqlite", []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-settlement-progress-sqlite", DeliveryID: "event-settlement-progress-sqlite"}}, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	group.Members[0].Status = agentruntime.RuntimeDeliveryGroupMemberCompleted
	group.Status = agentruntime.RuntimeDeliveryGroupCompleted
	group.CompletedAt = &now
	settlement, err := agentruntime.NewRuntimeDeliveryGroupSettlement(group, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupSettlementRepository)
	if _, err := repo.EnqueueRuntimeDeliveryGroupSettlement(ctx, settlement); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	claimed, ok, err := repo.ClaimRuntimeDeliveryGroupSettlement(ctx, "progress-worker", now, time.Minute)
	if err != nil || !ok {
		_ = store.Close()
		t.Fatalf("SQLite progress claim 异常: %#v ok=%v err=%v", claimed, ok, err)
	}
	progressRepo := repo.(agentruntime.RuntimeDeliveryGroupSettlementProgressRepository)
	progress, duplicate, err := progressRepo.AdvanceRuntimeDeliveryGroupSettlement(ctx, claimed.ID, claimed.LeaseOwner, claimed.Revision, agentruntime.RuntimeDeliveryGroupSettlementStagePreparing, agentruntime.RuntimeDeliveryGroupSettlementRemoteAbsent, agentruntime.RuntimeDeliveryGroupSettlementAlertRemoteUnavailable, "upstream unavailable: secret=should-redact", now.Add(time.Second))
	if err != nil || duplicate || progress.Stage != agentruntime.RuntimeDeliveryGroupSettlementStagePreparing || progress.AlertCode != agentruntime.RuntimeDeliveryGroupSettlementAlertRemoteUnavailable {
		_ = store.Close()
		t.Fatalf("SQLite progress 更新异常: %#v duplicate=%v err=%v", progress, duplicate, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo = store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupSettlementRepository)
	loaded, err := repo.GetRuntimeDeliveryGroupSettlement(ctx, claimed.ID)
	if err != nil || loaded.Stage != agentruntime.RuntimeDeliveryGroupSettlementStagePreparing || loaded.AlertCount != 1 || loaded.AlertCode != agentruntime.RuntimeDeliveryGroupSettlementAlertRemoteUnavailable || !strings.Contains(loaded.LastError, "upstream unavailable") {
		t.Fatalf("SQLite progress 重启后字段错误: %#v err=%v", loaded, err)
	}
	if _, _, err := repo.(agentruntime.RuntimeDeliveryGroupSettlementProgressRepository).AdvanceRuntimeDeliveryGroupSettlement(ctx, loaded.ID, loaded.LeaseOwner, loaded.Revision-1, agentruntime.RuntimeDeliveryGroupSettlementStagePrepared, agentruntime.RuntimeDeliveryGroupSettlementRemotePrepared, "", "", now.Add(2*time.Second)); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("旧 revision 不应覆盖 SQLite progress: %v", err)
	}
}
