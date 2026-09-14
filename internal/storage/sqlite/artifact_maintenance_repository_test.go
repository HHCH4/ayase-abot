package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"Abot/internal/artifact"
)

// artifactMaintenanceRepository asserts that the SQLite adapter really exposes
// the optional maintenance extensions and returns the concrete type so the
// tests can call them without repeating the assertion.
func artifactMaintenanceRepository(t *testing.T, store *Store) *artifactRepository {
	t.Helper()
	repository, ok := store.ArtifactRepository().(*artifactRepository)
	if !ok {
		t.Fatalf("unexpected artifact repository type %T", store.ArtifactRepository())
	}
	return repository
}

func fixtureObject(letter string) (digest, key string) {
	hexValue := strings.Repeat(letter, 64)
	return "sha256:" + hexValue, "sha256/" + hexValue[:2] + "/" + hexValue
}

func fixtureArtifact(id, letter string, status artifact.Status, size int64, updatedAt time.Time) artifact.Artifact {
	digest, key := fixtureObject(letter)
	item := artifact.Artifact{
		ID: id, Version: 1, UserID: "user-1", Kind: artifact.KindBinary, Name: id + ".bin",
		MIMEType: "application/octet-stream", Status: status, Size: size,
		CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}
	if status != artifact.StatusUploading {
		item.Digest = digest
		item.StorageKey = key
	}
	return item
}

func TestSQLiteArtifactStoredUsageIsDeduplicated(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repository := artifactMaintenanceRepository(t, store)
	ctx := context.Background()
	now := time.Now().UTC()

	// 两条记录共享同一对象，配额只能计入一次物理占用。
	for _, id := range []string{"artifact-usage-1", "artifact-usage-2"} {
		if err := repository.Create(ctx, fixtureArtifact(id, "a", artifact.StatusReady, 8, now)); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Create(ctx, fixtureArtifact("artifact-usage-3", "b", artifact.StatusReady, 4, now)); err != nil {
		t.Fatal(err)
	}
	failed := fixtureArtifact("artifact-usage-4", "c", artifact.StatusFailed, 100, now)
	if err := repository.Create(ctx, failed); err != nil {
		t.Fatal(err)
	}

	usage, err := repository.StoredUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Bytes != 12 || usage.Objects != 2 || usage.Artifacts != 3 {
		t.Fatalf("usage mismatch: %+v", usage)
	}
}

func TestSQLiteArtifactActiveReferenceExcludesDeletingAndFailed(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repository := artifactMaintenanceRepository(t, store)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repository.Create(ctx, fixtureArtifact("artifact-active", "a", artifact.StatusReady, 4, now)); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, fixtureArtifact("artifact-deleting", "a", artifact.StatusDeleting, 4, now)); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, fixtureArtifact("artifact-failed", "a", artifact.StatusFailed, 4, now)); err != nil {
		t.Fatal(err)
	}

	_, key := fixtureObject("a")
	active, err := repository.CountActiveByStorageKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("只有 ready 记录算活跃引用，got %d", active)
	}
	total, err := repository.CountByStorageKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("保守计数必须包含 deleting，got %d", total)
	}
}

func TestSQLiteArtifactStaleScans(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repository := artifactMaintenanceRepository(t, store)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	stale := now.Add(-48 * time.Hour)

	if err := repository.Create(ctx, fixtureArtifact("artifact-old-upload", "a", artifact.StatusUploading, 0, stale)); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, fixtureArtifact("artifact-new-upload", "b", artifact.StatusUploading, 0, now)); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, fixtureArtifact("artifact-old-delete", "c", artifact.StatusDeleting, 4, stale)); err != nil {
		t.Fatal(err)
	}

	uploads, err := repository.ListStaleUploading(ctx, now.Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 1 || uploads[0].ID != "artifact-old-upload" {
		t.Fatalf("unexpected stale uploads: %+v", uploads)
	}
	deletions, err := repository.ListStaleDeleting(ctx, now.Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletions) != 1 || deletions[0].ID != "artifact-old-delete" {
		t.Fatalf("unexpected stale deletions: %+v", deletions)
	}
	limited, err := repository.ListStaleUploading(ctx, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit 必须限制单次扫描条数: %+v", limited)
	}
}

func TestSQLiteArtifactObjectDeletionOutbox(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repository := artifactMaintenanceRepository(t, store)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, key := fixtureObject("a")

	if err := repository.EnqueueObjectDeletion(ctx, key, now); err != nil {
		t.Fatal(err)
	}
	// 幂等：重复入队不重置请求时间或尝试次数。
	if err := repository.EnqueueObjectDeletion(ctx, key, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	due, err := repository.ListDueObjectDeletions(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].StorageKey != key || !due[0].RequestedAt.Equal(now) {
		t.Fatalf("unexpected due deletions: %+v", due)
	}
	pending, err := repository.HasObjectDeletion(ctx, key)
	if err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}

	// 退避后本轮不再到期，重启后仍然保留。
	rescheduled := due[0]
	rescheduled.Attempts = 2
	rescheduled.LastError = "模拟失败"
	rescheduled.NextAttemptAt = now.Add(time.Hour)
	if err := repository.RescheduleObjectDeletion(ctx, rescheduled, now); err != nil {
		t.Fatal(err)
	}
	if delayed, err := repository.ListDueObjectDeletions(ctx, now, 10); err != nil || len(delayed) != 0 {
		t.Fatalf("退避期间不应到期: %+v err=%v", delayed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedRepository := artifactMaintenanceRepository(t, reopened)
	after, err := reopenedRepository.ListDueObjectDeletions(ctx, now.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Attempts != 2 || after[0].LastError != "模拟失败" {
		t.Fatalf("重启后 outbox 状态丢失: %+v", after)
	}
	if err := reopenedRepository.CompleteObjectDeletion(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := reopenedRepository.CompleteObjectDeletion(ctx, key); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("重复完成必须返回 ErrNotFound, got %v", err)
	}
	if pending, err := reopenedRepository.HasObjectDeletion(ctx, key); err != nil || pending {
		t.Fatalf("完成后必须清除记录: pending=%v err=%v", pending, err)
	}
}

func TestSQLiteArtifactServiceMaintenanceEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	objectStore, err := artifact.NewLocalContentStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := artifact.NewService(store.ArtifactRepository(), objectStore)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()

	item, err := service.Put(ctx, artifact.PutRequest{UserID: "user-1", Kind: artifact.KindBinary, Name: "queued.bin", MIMEType: "application/octet-stream"}, strings.NewReader("abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, "user-1", item.ID); err != nil {
		t.Fatal(err)
	}
	usage, err := service.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Artifacts != 0 || usage.Bytes != 0 {
		t.Fatalf("删除后占用必须归零: %+v", usage)
	}
	result, err := service.RunMaintenance(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.GarbageCollection.Deleted != 0 {
		t.Fatalf("正常删除不应留下孤儿对象: %+v", result)
	}
}
