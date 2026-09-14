package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"Abot/internal/agent/runtime"
	"Abot/internal/config"
)

func TestRuntimeConfigDirectoryServerRouteIsOptInAndSigned(t *testing.T) {
	ctx := context.Background()
	secret := []byte("config-directory-http-secret-32-bytes")
	without := NewServer(nil, nil)
	// The generic server still responds with its normal 404 when the receiver
	// has not been explicitly mounted.
	missing := httptest.NewRecorder()
	without.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/runtime/config-directory", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("未装配目录 receiver 应保持 404，实际=%d", missing.Code)
	}
	repo := runtime.NewMemoryRepository()
	receiver, err := runtime.NewRuntimeConfigDirectoryReceiver("runtime-a", "runtime-b", secret, repo)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	receiver.Now = func() time.Time { return now }
	var materializeCalls atomic.Int32
	if err := receiver.SetMaterializer(runtime.RuntimeConfigDirectoryMaterializerFunc(func(_ context.Context, record runtime.RuntimeConfigDirectoryRecord) (bool, error) {
		materializeCalls.Add(1)
		if record.Entry.ID != "profile-server" {
			return false, runtime.ErrRuntimeConfigDirectoryConflict
		}
		return materializeCalls.Load() > 1, nil
	})); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetRuntimeConfigDirectoryReceiver(receiver)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	transport, err := runtime.NewRuntimeConfigDirectoryHTTPTransport(httpServer.URL+"/api/v1/runtime/config-directory", "runtime-a", "runtime-b", secret, httpServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := runtime.NewRuntimeConfigDirectoryProfileEntry(config.Profile{ID: "profile-server", Name: "server", Revision: 1, Values: config.Values{"safe": "value"}})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := runtime.NewRuntimeConfigDirectoryEnvelope("runtime-a", "runtime-b", entry, "", "http-correlation", "server-key", now)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := transport.ReconcileRuntimeConfigDirectory(ctx, envelope, entry)
	if err != nil || absent.Phase != runtime.RuntimeConfigDirectoryStatusAbsent || absent.Found {
		t.Fatalf("Server status 首次应返回 absent: %#v err=%v", absent, err)
	}
	first, err := transport.Deliver(ctx, envelope, entry)
	if err != nil || first.Duplicate {
		t.Fatalf("Server 目录首次投递失败 receipt=%#v err=%v", first, err)
	}
	accepted, err := transport.ReconcileRuntimeConfigDirectory(ctx, envelope, entry)
	if err != nil || accepted.Phase != runtime.RuntimeConfigDirectoryStatusAccepted || !accepted.Found {
		t.Fatalf("Server status 投递后应返回 accepted: %#v err=%v", accepted, err)
	}
	materialized, err := transport.MaterializeRuntimeConfigDirectory(ctx, runtime.RuntimeConfigDirectoryRecord{Envelope: envelope, Entry: entry, ReceivedAt: now})
	if err != nil || materialized.Duplicate || materialized.Signature == "" || materializeCalls.Load() != 1 {
		t.Fatalf("Server materialize 首次调用失败: %#v calls=%d err=%v", materialized, materializeCalls.Load(), err)
	}
	materialized, err = transport.MaterializeRuntimeConfigDirectory(ctx, runtime.RuntimeConfigDirectoryRecord{Envelope: envelope, Entry: entry, ReceivedAt: now})
	if err != nil || !materialized.Duplicate || materializeCalls.Load() != 2 {
		t.Fatalf("Server materialize 重复调用应幂等: %#v calls=%d err=%v", materialized, materializeCalls.Load(), err)
	}
	retry := envelope
	retry.IssuedAt = now.Add(time.Second)
	retry.Signature = ""
	retry, err = runtime.SignRuntimeConfigDirectoryEnvelope(retry, secret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := transport.Deliver(ctx, retry, entry)
	if err != nil || !second.Duplicate {
		t.Fatalf("Server 目录重试应幂等 receipt=%#v err=%v", second, err)
	}
	if _, err := repo.GetRuntimeConfigDirectory(ctx, runtime.RuntimeConfigDirectoryKindProfile, entry.ID); err != nil {
		t.Fatalf("Server route 未持久化正文: %v", err)
	}
}
