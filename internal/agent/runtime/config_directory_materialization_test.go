package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type runtimeConfigDirectoryMaterializerFunc func(context.Context, RuntimeConfigDirectoryRecord) (bool, error)

func (f runtimeConfigDirectoryMaterializerFunc) MaterializeRuntimeConfigDirectory(ctx context.Context, record RuntimeConfigDirectoryRecord) (bool, error) {
	return f(ctx, record)
}

func TestRuntimeConfigDirectoryMaterializationReceiptAndExplicitOptIn(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	secret := []byte(configDirectoryTestSecret)
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeConfigDirectoryReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	transport, err := NewRuntimeConfigDirectoryHTTPTransport(server.URL, "runtime-a", "runtime-b", secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	entry := configDirectoryProfile(t, "profile-materialize", 1, "materialize")
	envelope := configDirectorySigned(t, "runtime-a", "runtime-b", entry, "", now)
	if _, err := transport.Deliver(ctx, envelope, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.MaterializeRuntimeConfigDirectory(ctx, RuntimeConfigDirectoryRecord{Envelope: envelope, Entry: entry, ReceivedAt: now}); !errors.Is(err, ErrRuntimeConfigDirectoryMaterializationUnavailable) {
		t.Fatalf("未启用 materializer 应返回 unavailable，实际=%v", err)
	}

	var calls atomic.Int32
	if err := receiver.SetMaterializer(runtimeConfigDirectoryMaterializerFunc(func(_ context.Context, record RuntimeConfigDirectoryRecord) (bool, error) {
		calls.Add(1)
		if record.Entry.Values["safe.value"] != "materialize" {
			return false, errors.New("materializer 收到错误正文")
		}
		// Mutating the callback copy must not mutate the destination catalog.
		record.Entry.Values["safe.value"] = "callback-mutation"
		return calls.Load() > 1, nil
	})); err != nil {
		t.Fatal(err)
	}
	storedRecord, err := repo.GetRuntimeConfigDirectory(ctx, entry.Kind, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseReceipt, err := NewRuntimeConfigDirectoryMaterializationReceipt(storedRecord, false, now)
	if err != nil {
		t.Fatalf("receipt 构造失败: %v record=%#v", err, storedRecord)
	}
	if _, err := SignRuntimeConfigDirectoryMaterializationReceipt(baseReceipt, secret); err != nil {
		t.Fatalf("receipt 签名失败: %v", err)
	}
	receipt, err := transport.MaterializeRuntimeConfigDirectory(ctx, RuntimeConfigDirectoryRecord{Envelope: envelope, Entry: entry, ReceivedAt: now})
	if err != nil || receipt.Duplicate || receipt.Signature == "" {
		t.Fatalf("首次 materialize receipt 错误: %#v err=%v", receipt, err)
	}
	if strings.Contains(string(mustMarshalRuntimeConfigDirectoryMaterializationReceipt(t, receipt)), "safe.value") {
		t.Fatal("materialization receipt 不得携带目录正文")
	}
	if err := VerifyRuntimeConfigDirectoryMaterializationReceipt(receipt, secret); err != nil {
		t.Fatal(err)
	}
	second, err := transport.MaterializeRuntimeConfigDirectory(ctx, RuntimeConfigDirectoryRecord{Envelope: envelope, Entry: entry, ReceivedAt: now})
	if err != nil || !second.Duplicate || calls.Load() != 2 {
		t.Fatalf("重复 materialize 应幂等并返回 duplicate: %#v calls=%d err=%v", second, calls.Load(), err)
	}
	stored, err := repo.GetRuntimeConfigDirectory(ctx, entry.Kind, entry.ID)
	if err != nil || stored.Entry.Values["safe.value"] != "materialize" {
		t.Fatalf("materializer 不得修改 inbox 防御性副本: %#v err=%v", stored, err)
	}
}

func mustMarshalRuntimeConfigDirectoryMaterializationReceipt(t *testing.T, receipt RuntimeConfigDirectoryMaterializationReceipt) []byte {
	t.Helper()
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
