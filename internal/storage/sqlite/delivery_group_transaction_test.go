package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

const sqliteDeliveryGroupTransactionSecret = "0123456789abcdef0123456789abcdef"

func TestSQLiteRuntimeDeliveryGroupTransactionIsDurableAndCASProtected(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-group-txn-sqlite", []agentruntime.RuntimeDeliveryGroupMember{
		{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "event-group-txn-sqlite", DeliveryID: "event-group-txn-sqlite"},
		{Kind: agentruntime.RuntimeDeliveryKindConfig, OutboxID: "config-group-txn-sqlite", DeliveryID: "config-group-txn-sqlite"},
	}, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeDeliveryGroupEnvelope(group, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeDeliveryGroupEnvelope(envelope, []byte(sqliteDeliveryGroupTransactionSecret))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	repo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupTransactionRepository)
	if duplicate, err := repo.PrepareRuntimeDeliveryGroup(ctx, envelope); err != nil || duplicate {
		_ = store.Close()
		t.Fatalf("SQLite 首次 prepare 异常: duplicate=%v err=%v", duplicate, err)
	}
	var row runtimeDeliveryGroupTransactionRow
	if err := store.db.Where("group_id = ?", group.ID).First(&row).Error; err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if row.Status != string(agentruntime.RuntimeDeliveryGroupTransactionPrepared) || row.MembersDigest != envelope.MembersDigest {
		_ = store.Close()
		t.Fatalf("group transaction row 不正确: %#v", row)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repo = store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupTransactionRepository)
	if duplicate, err := repo.PrepareRuntimeDeliveryGroup(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("SQLite 重启后 prepare 应幂等: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := repo.CommitRuntimeDeliveryGroup(ctx, envelope); err != nil || duplicate {
		t.Fatalf("SQLite 首次 commit 异常: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := repo.CommitRuntimeDeliveryGroup(ctx, envelope); err != nil || !duplicate {
		t.Fatalf("SQLite 重复 commit 应幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.AbortRuntimeDeliveryGroup(ctx, envelope); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("SQLite committed group 不应 abort: %v", err)
	}
	transaction, err := repo.GetRuntimeDeliveryGroupTransaction(ctx, group.ID)
	if err != nil || transaction.Status != agentruntime.RuntimeDeliveryGroupTransactionCommitted {
		t.Fatalf("SQLite group transaction 终态错误: %#v err=%v", transaction, err)
	}
}
