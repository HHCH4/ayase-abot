package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

const sqliteRuntimeDeliveryGroupFenceSecret = "0123456789abcdef0123456789abcdef"

func TestSQLiteRuntimeDeliveryGroupFenceBindsSourceGroupAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-fence-bind", []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "bind-member", DeliveryID: "bind-member"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	groups := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupRepository)
	stored, err := groups.EnqueueRuntimeDeliveryGroup(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	issuer := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupFenceIssuer)
	fence, err := issuer.IssueRuntimeDeliveryGroupFence(ctx, group.Source, group.Destination, group.InvocationID, group.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	binder := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupFenceBinder)
	bound, err := binder.BindRuntimeDeliveryGroupFence(ctx, group.ID, stored.Revision, fence, now)
	if err != nil || bound.FenceRevision != 1 {
		t.Fatalf("SQLite bind fence 异常: %#v err=%v", bound, err)
	}
	retry, err := binder.BindRuntimeDeliveryGroupFence(ctx, group.ID, bound.Revision, fence, now)
	if err != nil || retry.Revision != bound.Revision {
		t.Fatalf("SQLite 重复 bind 应幂等: %#v err=%v", retry, err)
	}
	if _, err := binder.BindRuntimeDeliveryGroupFence(ctx, group.ID, stored.Revision, fence, now); !errors.Is(err, agentruntime.ErrConflict) {
		t.Fatalf("SQLite 旧 revision bind 应拒绝: %v", err)
	}
}

func TestSQLiteRuntimeDeliveryGroupFenceIsDurableAndRejectsStaleGroups(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	groupOne, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-fence-sqlite", []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "sqlite-fence-one", DeliveryID: "sqlite-fence-one"}}, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	issuer := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupFenceIssuer)
	fenceOne, err := issuer.IssueRuntimeDeliveryGroupFence(ctx, groupOne.Source, groupOne.Destination, groupOne.InvocationID, groupOne.ID, now)
	if err != nil || fenceOne.Revision != 1 {
		_ = store.Close()
		t.Fatalf("SQLite 首次 fence issue 异常: %#v err=%v", fenceOne, err)
	}
	groupOne.FenceID, groupOne.FenceRevision = fenceOne.FenceID, fenceOne.Revision
	groupOne, err = groupOne.Normalize(now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelopeOne, err := agentruntime.NewRuntimeDeliveryGroupEnvelope(groupOne, now)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelopeOne, err = agentruntime.SignRuntimeDeliveryGroupEnvelope(envelopeOne, []byte(sqliteRuntimeDeliveryGroupFenceSecret))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	txnRepo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupTransactionRepository)
	if _, err := txnRepo.PrepareRuntimeDeliveryGroup(ctx, envelopeOne); err != nil {
		_ = store.Close()
		t.Fatalf("SQLite fenced prepare 异常: %v", err)
	}
	groupTwo, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-fence-sqlite", []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "sqlite-fence-two", DeliveryID: "sqlite-fence-two"}}, now.Add(time.Second))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	fenceTwo, err := issuer.IssueRuntimeDeliveryGroupFence(ctx, groupTwo.Source, groupTwo.Destination, groupTwo.InvocationID, groupTwo.ID, now.Add(time.Second))
	if err != nil || fenceTwo.Revision != 2 {
		_ = store.Close()
		t.Fatalf("SQLite fence 推进异常: %#v err=%v", fenceTwo, err)
	}
	groupTwo.FenceID, groupTwo.FenceRevision = fenceTwo.FenceID, fenceTwo.Revision
	groupTwo, err = groupTwo.Normalize(now.Add(time.Second))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelopeTwo, err := agentruntime.NewRuntimeDeliveryGroupEnvelope(groupTwo, now.Add(time.Second))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	envelopeTwo, err = agentruntime.SignRuntimeDeliveryGroupEnvelope(envelopeTwo, []byte(sqliteRuntimeDeliveryGroupFenceSecret))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := txnRepo.PrepareRuntimeDeliveryGroup(ctx, envelopeTwo); err != nil {
		_ = store.Close()
		t.Fatalf("SQLite 新 fence prepare 异常: %v", err)
	}
	if _, err := txnRepo.CommitRuntimeDeliveryGroup(ctx, envelopeOne); !errors.Is(err, agentruntime.ErrRuntimeDeliveryFenceStale) {
		_ = store.Close()
		t.Fatalf("SQLite 旧 fence commit 应拒绝: %v", err)
	}
	if _, err := txnRepo.CommitRuntimeDeliveryGroup(ctx, envelopeTwo); err != nil {
		_ = store.Close()
		t.Fatalf("SQLite 新 fence commit 异常: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reopenedIssuer := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupFenceIssuer)
	stored, err := reopenedIssuer.GetRuntimeDeliveryGroupFence(ctx, fenceTwo.FenceID)
	if err != nil || stored.Revision != 2 || stored.GroupID != groupTwo.ID {
		t.Fatalf("SQLite 重启后 fence 丢失或漂移: %#v err=%v", stored, err)
	}
}

func TestSQLiteRuntimeDeliveryGroupFenceUnknownCommitCannotAdvanceLedger(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC()
	group, err := agentruntime.NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-fence-unknown", []agentruntime.RuntimeDeliveryGroupMember{{Kind: agentruntime.RuntimeDeliveryKindEvent, OutboxID: "unknown", DeliveryID: "unknown"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	group.FenceID, group.FenceRevision = agentruntime.RuntimeDeliveryGroupFenceID(group.Source, group.Destination, group.InvocationID), 4
	group, err = group.Normalize(now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeDeliveryGroupEnvelope(group, now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeDeliveryGroupEnvelope(envelope, []byte(sqliteRuntimeDeliveryGroupFenceSecret))
	if err != nil {
		t.Fatal(err)
	}
	txnRepo := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupTransactionRepository)
	if _, err := txnRepo.CommitRuntimeDeliveryGroup(ctx, envelope); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("unknown commit 应返回 not found: %v", err)
	}
	issuer := store.RuntimeRepository().(agentruntime.RuntimeDeliveryGroupFenceIssuer)
	if _, err := issuer.GetRuntimeDeliveryGroupFence(ctx, group.FenceID); !errors.Is(err, agentruntime.ErrNotFound) {
		t.Fatalf("unknown commit 不应抢占 fence ledger: %v", err)
	}
}
