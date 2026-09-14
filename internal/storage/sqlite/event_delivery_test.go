package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentruntime "Abot/internal/agent/runtime"
)

func TestSQLiteRuntimeEventDeliveryInboxIsDurableIdempotentAndConflictSafe(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	event := agentruntime.AgentEvent{
		ID:           "sqlite-delivery-event-1",
		InvocationID: "sqlite-delivery-invocation-1",
		Sequence:     1,
		Type:         agentruntime.EventRuntimeNotice,
		Timestamp:    now,
		Data:         map[string]any{"message": "private body stays in source log"},
	}
	outbox, err := agentruntime.NewRuntimeEventOutbox(event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "audit-b", outbox, event)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = agentruntime.SignRuntimeEventDeliveryEnvelope(envelope, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	inbox, ok := store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryInbox)
	if !ok {
		_ = store.Close()
		t.Fatal("SQLite runtime repository 未实现 RuntimeEventDeliveryInbox")
	}
	duplicate, err := inbox.AcceptRuntimeEventDelivery(ctx, envelope)
	if err != nil || duplicate {
		t.Fatalf("首次写入应成功且非 duplicate: duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = inbox.AcceptRuntimeEventDelivery(ctx, envelope)
	if err != nil || !duplicate {
		t.Fatalf("相同 envelope 重试应幂等: duplicate=%v err=%v", duplicate, err)
	}
	var row runtimeEventDeliveryInboxRow
	if err := store.db.Where("event_id = ?", envelope.EventID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Version != envelope.Version || row.EventID != envelope.EventID || row.Timestamp != envelope.Timestamp || row.EventDigest != envelope.EventDigest || row.ReceivedAt.IsZero() {
		t.Fatalf("inbox metadata 持久化错误: %#v", row)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	inbox = store.RuntimeRepository().(agentruntime.RuntimeEventDeliveryInbox)
	duplicate, err = inbox.AcceptRuntimeEventDelivery(ctx, envelope)
	if err != nil || !duplicate {
		t.Fatalf("重启后相同 envelope 应仍幂等: duplicate=%v err=%v", duplicate, err)
	}

	conflict := envelope
	conflict.Sequence = 2
	conflict, err = agentruntime.SignRuntimeEventDeliveryEnvelope(conflict, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := inbox.AcceptRuntimeEventDelivery(ctx, conflict); !errors.Is(err, agentruntime.ErrConflict) || duplicate {
		t.Fatalf("同 EventID 的不同 metadata 必须冲突: duplicate=%v err=%v", duplicate, err)
	}

	concurrentEvent := event
	concurrentEvent.ID = "sqlite-delivery-event-concurrent"
	concurrentOutbox, err := agentruntime.NewRuntimeEventOutbox(concurrentEvent)
	if err != nil {
		t.Fatal(err)
	}
	concurrentEnvelope, err := agentruntime.NewRuntimeEventDeliveryEnvelope("runtime-a", "audit-b", concurrentOutbox, concurrentEvent)
	if err != nil {
		t.Fatal(err)
	}
	concurrentEnvelope, err = agentruntime.SignRuntimeEventDeliveryEnvelope(concurrentEnvelope, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan struct {
		duplicate bool
		err       error
	}, 12)
	var group sync.WaitGroup
	for index := 0; index < 12; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			duplicate, acceptErr := inbox.AcceptRuntimeEventDelivery(ctx, concurrentEnvelope)
			results <- struct {
				duplicate bool
				err       error
			}{duplicate: duplicate, err: acceptErr}
		}()
	}
	close(start)
	group.Wait()
	close(results)
	firstCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("相同 envelope 并发接收不应失败: %v", result.err)
		}
		if !result.duplicate {
			firstCount++
		}
	}
	if firstCount != 1 {
		t.Fatalf("并发 inbox 只能有一个首次写入，得到 %d", firstCount)
	}
}
