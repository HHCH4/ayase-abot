package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deleteFailingStore simulates a local filesystem that refuses to remove a
// specific object, which is exactly the window the durable delete outbox and
// the garbage-collection retry exist for.
type deleteFailingStore struct {
	*LocalContentStore
	blocked map[string]bool
}

func (s *deleteFailingStore) Delete(ctx context.Context, key string) error {
	if s.blocked[key] {
		return errors.New("模拟对象删除失败")
	}
	return s.LocalContentStore.Delete(ctx, key)
}

func newTestServiceWithStore(t *testing.T, store ObjectStore) (*Service, *MemoryRepository) {
	t.Helper()
	repository := NewMemoryRepository()
	service, err := NewService(repository, store)
	if err != nil {
		t.Fatal(err)
	}
	return service, repository
}

func setTestPolicy(t *testing.T, service *Service, policy MaintenancePolicy) {
	t.Helper()
	if err := service.SetMaintenancePolicy(policy); err != nil {
		t.Fatalf("设置维护策略失败: %v", err)
	}
}

func putTestArtifact(t *testing.T, service *Service, request PutRequest, content string) Artifact {
	t.Helper()
	item, err := service.Put(context.Background(), request, bytes.NewBufferString(content))
	if err != nil {
		t.Fatalf("写入 artifact 失败: %v", err)
	}
	return item
}

func objectFilePath(store *LocalContentStore, storageKey string) string {
	return filepath.Join(store.root, "objects", filepath.FromSlash(storageKey))
}

func countStoredObjects(t *testing.T, store *LocalContentStore) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(filepath.Join(store.root, "objects"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("统计对象失败: %v", err)
	}
	return count
}

func agePath(t *testing.T, path string, age time.Duration) {
	t.Helper()
	stamp := time.Now().UTC().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("调整文件时间失败: %v", err)
	}
}

func textPutRequest(userID, name string) PutRequest {
	return PutRequest{UserID: userID, Kind: KindInputAttachment, Name: name, MIMEType: "text/plain"}
}

func TestPutRejectsUploadAboveQuota(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	setTestPolicy(t, service, MaintenancePolicy{QuotaBytes: 8, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod})

	putTestArtifact(t, service, textPutRequest("user-1", "first.txt"), "12345678")
	_, err := service.Put(ctx, textPutRequest("user-1", "second.txt"), bytes.NewBufferString("abcdefghij"))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("超配额上传必须被拒绝，got %v", err)
	}
	items, listErr := repository.List(ctx, "user-1", "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(items) != 1 {
		t.Fatalf("被拒绝的上传不能留下元数据: %+v", items)
	}
	if count := countStoredObjects(t, store); count != 1 {
		t.Fatalf("被拒绝的上传不能留下对象，当前 %d 个", count)
	}
}

func TestPutAllowsDeduplicatedContentAtQuotaLimit(t *testing.T) {
	service, _, store := newTestService(t)
	setTestPolicy(t, service, MaintenancePolicy{QuotaBytes: 8, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod})

	putTestArtifact(t, service, textPutRequest("user-1", "first.txt"), "12345678")
	// 相同内容由内容寻址存储共享，不增加物理占用，因此即使配额已满也应放行。
	second := putTestArtifact(t, service, textPutRequest("user-1", "copy.txt"), "12345678")
	if second.StorageKey == "" || second.Digest == "" {
		t.Fatalf("去重上传必须返回有效对象身份: %+v", second)
	}
	if count := countStoredObjects(t, store); count != 1 {
		t.Fatalf("相同内容必须共享同一对象，当前 %d 个", count)
	}
}

func TestPutAllowsUploadWhenQuotaDisabled(t *testing.T) {
	service, _, _ := newTestService(t)
	ctx := context.Background()
	setTestPolicy(t, service, MaintenancePolicy{QuotaBytes: 0, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod})

	if _, err := service.Put(ctx, textPutRequest("user-1", "large.txt"), bytes.NewBufferString(strings.Repeat("x", 4096))); err != nil {
		t.Fatalf("关闭配额后上传不应被拒绝: %v", err)
	}
	usage, err := service.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage.QuotaBytes != 0 || usage.QuotaExceeded {
		t.Fatalf("关闭配额时不应报告超限: %+v", usage)
	}
}

func TestUsageReportsDeduplicatedFootprint(t *testing.T) {
	service, _, _ := newTestService(t)
	ctx := context.Background()
	putTestArtifact(t, service, textPutRequest("user-1", "first.txt"), "abcdefgh")
	putTestArtifact(t, service, textPutRequest("user-1", "copy.txt"), "abcdefgh")
	putTestArtifact(t, service, textPutRequest("user-1", "other.txt"), "1234")

	usage, err := service.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Bytes != 12 || usage.Objects != 2 || usage.Artifacts != 3 {
		t.Fatalf("占用统计不正确: %+v", usage)
	}
	if usage.QuotaBytes != DefaultQuotaBytes || usage.QuotaExceeded {
		t.Fatalf("配额状态不正确: %+v", usage)
	}
}

func TestUsageFlagsExceededQuota(t *testing.T) {
	service, _, _ := newTestService(t)
	ctx := context.Background()
	setTestPolicy(t, service, MaintenancePolicy{QuotaBytes: 4, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod})
	putTestArtifact(t, service, textPutRequest("user-1", "first.txt"), "abcd")
	// 已存在的内容仍可下载；配额只在新的物理占用上判定。
	setTestPolicy(t, service, MaintenancePolicy{QuotaBytes: 2, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod})
	usage, err := service.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.QuotaExceeded || usage.Bytes != 4 || usage.QuotaBytes != 2 {
		t.Fatalf("超限标志不正确: %+v", usage)
	}
}

func TestSetMaintenancePolicyRejectsInvalidValues(t *testing.T) {
	service, _, _ := newTestService(t)
	cases := map[string]MaintenancePolicy{
		"负数配额":   {QuotaBytes: -1, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod},
		"配额过大":   {QuotaBytes: MaxQuotaBytes + 1, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod},
		"保留时长过短": {QuotaBytes: DefaultQuotaBytes, StaleUploadAge: time.Second, ObjectGracePeriod: DefaultObjectGracePeriod},
		"保留时长过长": {QuotaBytes: DefaultQuotaBytes, StaleUploadAge: 90 * 24 * time.Hour, ObjectGracePeriod: DefaultObjectGracePeriod},
		"保护期过长":  {QuotaBytes: DefaultQuotaBytes, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: 48 * time.Hour},
	}
	for name, policy := range cases {
		if err := service.SetMaintenancePolicy(policy); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s 必须被拒绝，got %v", name, err)
		}
	}
}

func TestSweepStaleArtifactsClosesAbandonedUpload(t *testing.T) {
	service, repository, _ := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	abandoned := Artifact{
		ID: "artifact-abandoned", Version: 1, UserID: "user-1", Kind: KindInputAttachment,
		Name: "abandoned.txt", MIMEType: "text/plain", Status: StatusUploading,
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour),
	}
	if err := repository.Create(ctx, abandoned); err != nil {
		t.Fatal(err)
	}
	result, err := service.SweepStaleArtifacts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadsFailed != 1 {
		t.Fatalf("过期上传必须被关闭: %+v", result)
	}
	item, err := repository.Get(ctx, abandoned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != StatusFailed || strings.TrimSpace(item.Error) == "" {
		t.Fatalf("过期上传的终态不正确: %+v", item)
	}
	second, err := service.SweepStaleArtifacts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if second.UploadsFailed != 0 {
		t.Fatalf("清理必须幂等: %+v", second)
	}
}

func TestSweepStaleArtifactsClosesInterruptedDeletion(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	item := putTestArtifact(t, service, textPutRequest("user-1", "gone.txt"), "abcdef")
	interrupted := item
	interrupted.Status = StatusDeleting
	interrupted.Version = item.Version + 1
	interrupted.UpdatedAt = time.Now().UTC().Add(-48 * time.Hour)
	if err := repository.Update(ctx, interrupted, int64(item.Version)); err != nil {
		t.Fatal(err)
	}

	result, err := service.SweepStaleArtifacts(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletionsClosed != 1 {
		t.Fatalf("中断的删除必须收口: %+v", result)
	}
	if _, getErr := repository.Get(ctx, item.ID); !errors.Is(getErr, ErrNotFound) {
		t.Fatalf("中断的删除应收口元数据, got %v", getErr)
	}
	pending, err := repository.HasObjectDeletion(ctx, item.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("中断的删除必须把对象登记到删除 outbox")
	}
	if _, statErr := os.Stat(objectFilePath(store, item.StorageKey)); statErr != nil {
		t.Fatalf("收口阶段不应直接删除对象: %v", statErr)
	}
}

func TestSweepStaleArtifactsRemovesTemporaryUploads(t *testing.T) {
	service, _, store := newTestService(t)
	ctx := context.Background()
	stalePath := filepath.Join(store.root, "tmp", "upload-stale")
	freshPath := filepath.Join(store.root, "tmp", "upload-fresh")
	for _, path := range []string{stalePath, freshPath} {
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agePath(t, stalePath, 48*time.Hour)

	result, err := service.SweepStaleArtifacts(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.TemporaryRemoved != 1 {
		t.Fatalf("必须只清理过期的临时对象: %+v", result)
	}
	if _, statErr := os.Stat(stalePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("过期临时对象应被删除: %v", statErr)
	}
	if _, statErr := os.Stat(freshPath); statErr != nil {
		t.Fatalf("新的临时对象不应被删除: %v", statErr)
	}
}

func TestDrainObjectDeletionsRemovesUnreferencedObject(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	item := putTestArtifact(t, service, textPutRequest("user-1", "gone.txt"), "abcdef")
	if err := repository.Delete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnqueueObjectDeletion(ctx, item.StorageKey, now); err != nil {
		t.Fatal(err)
	}
	result, err := service.DrainObjectDeletions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || result.Deferred != 0 {
		t.Fatalf("无引用对象必须被回收: %+v", result)
	}
	if _, statErr := os.Stat(objectFilePath(store, item.StorageKey)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("对象应被删除: %v", statErr)
	}
	pending, err := repository.HasObjectDeletion(ctx, item.StorageKey)
	if err != nil || pending {
		t.Fatalf("完成后 outbox 必须清空: pending=%v err=%v", pending, err)
	}
}

func TestDrainObjectDeletionsKeepsReferencedObject(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	item := putTestArtifact(t, service, textPutRequest("user-1", "kept.txt"), "abcdef")
	if err := repository.EnqueueObjectDeletion(ctx, item.StorageKey, now); err != nil {
		t.Fatal(err)
	}
	result, err := service.DrainObjectDeletions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 0 || result.Skipped != 1 {
		t.Fatalf("仍被引用的对象不能删除: %+v", result)
	}
	if _, statErr := os.Stat(objectFilePath(store, item.StorageKey)); statErr != nil {
		t.Fatalf("对象必须保留: %v", statErr)
	}
	pending, err := repository.HasObjectDeletion(ctx, item.StorageKey)
	if err != nil || pending {
		t.Fatalf("过时的删除意图必须收口: pending=%v err=%v", pending, err)
	}
}

func TestDrainObjectDeletionsReschedulesAfterFailure(t *testing.T) {
	root := t.TempDir()
	contentStore, err := NewLocalContentStore(root)
	if err != nil {
		t.Fatal(err)
	}
	failing := &deleteFailingStore{LocalContentStore: contentStore, blocked: make(map[string]bool)}
	service, repository := newTestServiceWithStore(t, failing)
	ctx := context.Background()
	now := time.Now().UTC()
	item := putTestArtifact(t, service, textPutRequest("user-1", "stuck.txt"), "abcdef")
	if err := repository.Delete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnqueueObjectDeletion(ctx, item.StorageKey, now); err != nil {
		t.Fatal(err)
	}
	failing.blocked[item.StorageKey] = true

	result, err := service.DrainObjectDeletions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deferred != 1 {
		t.Fatalf("删除失败必须延后重试: %+v", result)
	}
	due, err := repository.ListDueObjectDeletions(ctx, now, maxObjectDeletionBatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("退避期间不应再次到期: %+v", due)
	}
	later, err := repository.ListDueObjectDeletions(ctx, now.Add(objectDeletionBackoff(1)+time.Second), maxObjectDeletionBatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || later[0].Attempts != 1 || strings.TrimSpace(later[0].LastError) == "" {
		t.Fatalf("退避记录不正确: %+v", later)
	}

	failing.blocked[item.StorageKey] = false
	retry, err := service.DrainObjectDeletions(ctx, now.Add(objectDeletionBackoff(1)+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retry.Deleted != 1 {
		t.Fatalf("恢复后必须完成删除: %+v", retry)
	}
}

func TestDeleteQueuesObjectRemovalWhenStoreFails(t *testing.T) {
	root := t.TempDir()
	contentStore, err := NewLocalContentStore(root)
	if err != nil {
		t.Fatal(err)
	}
	failing := &deleteFailingStore{LocalContentStore: contentStore, blocked: make(map[string]bool)}
	service, repository := newTestServiceWithStore(t, failing)
	ctx := context.Background()
	item := putTestArtifact(t, service, textPutRequest("user-1", "delete-me.txt"), "abcdef")
	failing.blocked[item.StorageKey] = true

	if err := service.Delete(ctx, "user-1", item.ID); err != nil {
		t.Fatalf("对象删除失败不应阻塞元数据删除: %v", err)
	}
	if _, getErr := repository.Get(ctx, item.ID); !errors.Is(getErr, ErrNotFound) {
		t.Fatalf("元数据必须已删除, got %v", getErr)
	}
	pending, err := repository.HasObjectDeletion(ctx, item.StorageKey)
	if err != nil || !pending {
		t.Fatalf("对象删除必须进入 outbox: pending=%v err=%v", pending, err)
	}

	failing.blocked[item.StorageKey] = false
	drained, err := service.DrainObjectDeletions(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if drained.Deleted != 1 {
		t.Fatalf("outbox 重试必须回收对象: %+v", drained)
	}
}

func TestDeleteConversationQueuesObjectRemovalWhenStoreFails(t *testing.T) {
	root := t.TempDir()
	contentStore, err := NewLocalContentStore(root)
	if err != nil {
		t.Fatal(err)
	}
	failing := &deleteFailingStore{LocalContentStore: contentStore, blocked: make(map[string]bool)}
	service, repository := newTestServiceWithStore(t, failing)
	ctx := context.Background()
	request := textPutRequest("user-1", "shared.txt")
	request.ConversationID = "conversation-1"
	item := putTestArtifact(t, service, request, "abcdef")
	failing.blocked[item.StorageKey] = true

	if err := service.DeleteConversation(ctx, "conversation-1"); err != nil {
		t.Fatalf("对象删除失败不应阻塞对话删除: %v", err)
	}
	pending, err := repository.HasObjectDeletion(ctx, item.StorageKey)
	if err != nil || !pending {
		t.Fatalf("对象删除必须进入 outbox: pending=%v err=%v", pending, err)
	}
}

func TestGarbageCollectObjectsReclaimsOrphan(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	item := putTestArtifact(t, service, textPutRequest("user-1", "orphan.txt"), "abcdef")
	objectPath := objectFilePath(store, item.StorageKey)
	agePath(t, objectPath, 2*time.Hour)
	if err := repository.Delete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}

	result, err := service.GarbageCollectObjects(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || result.ReclaimedBytes != item.Size {
		t.Fatalf("孤儿对象必须被回收: %+v", result)
	}
	if _, statErr := os.Stat(objectPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("孤儿对象应被删除: %v", statErr)
	}
}

func TestGarbageCollectObjectsKeepsReferencedAndFreshObjects(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	referenced := putTestArtifact(t, service, textPutRequest("user-1", "kept.txt"), "abcdef")
	fresh := putTestArtifact(t, service, textPutRequest("user-1", "fresh.txt"), "uvwxyz")
	agePath(t, objectFilePath(store, referenced.StorageKey), 2*time.Hour)
	if err := repository.Delete(ctx, fresh.ID); err != nil {
		t.Fatal(err)
	}

	result, err := service.GarbageCollectObjects(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 0 || result.Skipped != 2 {
		t.Fatalf("引用中与保护期内的对象都不能被回收: %+v", result)
	}
	for _, key := range []string{referenced.StorageKey, fresh.StorageKey} {
		if _, statErr := os.Stat(objectFilePath(store, key)); statErr != nil {
			t.Fatalf("对象 %s 必须保留: %v", key, statErr)
		}
	}
}

func TestGarbageCollectObjectsSkipsQueuedDeletion(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	item := putTestArtifact(t, service, textPutRequest("user-1", "queued.txt"), "abcdef")
	agePath(t, objectFilePath(store, item.StorageKey), 2*time.Hour)
	if err := repository.Delete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnqueueObjectDeletion(ctx, item.StorageKey, now); err != nil {
		t.Fatal(err)
	}

	result, err := service.GarbageCollectObjects(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 0 || result.Skipped != 1 {
		t.Fatalf("已入队的对象应由 outbox 处理: %+v", result)
	}
	if _, statErr := os.Stat(objectFilePath(store, item.StorageKey)); statErr != nil {
		t.Fatalf("outbox 处理前对象必须保留: %v", statErr)
	}
}

func TestGarbageCollectObjectsQueuesRetryWhenDeleteFails(t *testing.T) {
	root := t.TempDir()
	contentStore, err := NewLocalContentStore(root)
	if err != nil {
		t.Fatal(err)
	}
	failing := &deleteFailingStore{LocalContentStore: contentStore, blocked: make(map[string]bool)}
	service, repository := newTestServiceWithStore(t, failing)
	ctx := context.Background()
	item := putTestArtifact(t, service, textPutRequest("user-1", "retry.txt"), "abcdef")
	agePath(t, objectFilePath(contentStore, item.StorageKey), 2*time.Hour)
	if err := repository.Delete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	failing.blocked[item.StorageKey] = true

	result, err := service.GarbageCollectObjects(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 0 || result.Deferred != 1 {
		t.Fatalf("回收失败必须转为 durably 重试: %+v", result)
	}
	pending, err := repository.HasObjectDeletion(ctx, item.StorageKey)
	if err != nil || !pending {
		t.Fatalf("失败对象必须进入 outbox: pending=%v err=%v", pending, err)
	}
}

func TestRunMaintenanceCombinesStages(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	abandoned := Artifact{
		ID: "artifact-abandoned", Version: 1, UserID: "user-1", Kind: KindInputAttachment,
		Name: "abandoned.txt", MIMEType: "text/plain", Status: StatusUploading,
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour),
	}
	if err := repository.Create(ctx, abandoned); err != nil {
		t.Fatal(err)
	}
	orphan := putTestArtifact(t, service, textPutRequest("user-1", "orphan.txt"), "abcdef")
	agePath(t, objectFilePath(store, orphan.StorageKey), 2*time.Hour)
	if err := repository.Delete(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}

	result, err := service.RunMaintenance(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadSweep.UploadsFailed != 1 {
		t.Fatalf("维护必须关闭未完成上传: %+v", result)
	}
	if result.GarbageCollection.Deleted != 1 || result.GarbageCollection.ReclaimedBytes != orphan.Size {
		t.Fatalf("维护必须回收孤儿对象: %+v", result)
	}
}

func TestMaintenanceMethodsWithoutExtensions(t *testing.T) {
	// 只有基础 Repository/ObjectStore 能力时，维护调用必须是显式的空操作或
	// 明确错误，而不是 panic 或静默破坏。
	repository := NewMemoryRepository()
	store, err := NewLocalContentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, serviceErr := NewService(legacyRepository{Repository: repository}, legacyStore{ObjectStore: store})
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}
	ctx := context.Background()
	if _, err := service.Usage(ctx); err != nil {
		t.Fatalf("缺少 usage 扩展时不应报错: %v", err)
	}
	if _, err := service.DrainObjectDeletions(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("缺少 outbox 扩展时应为空操作: %v", err)
	}
	if _, err := service.GarbageCollectObjects(ctx, time.Now().UTC()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("缺少对象枚举能力时应返回 ErrUnsupported, got %v", err)
	}
	result, err := service.RunMaintenance(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("维护组合在缺少扩展时不应失败: %v", err)
	}
	if result.GarbageCollection.Deleted != 0 {
		t.Fatalf("缺少扩展时不应回收任何对象: %+v", result)
	}
}

// legacyRepository hides the optional maintenance interfaces on purpose.
type legacyRepository struct {
	Repository
}

// legacyStore hides the optional store extensions on purpose.
type legacyStore struct {
	ObjectStore
}

func TestPutQuotaRejectionKeepsPreExistingObject(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	putTestArtifact(t, service, textPutRequest("user-1", "first.txt"), "abcd")
	// 写入第二个对象后删除它的 metadata，让它成为一个无引用的既存对象。
	orphanContent := "abcdef"
	orphan := putTestArtifact(t, service, textPutRequest("user-1", "orphan.txt"), orphanContent)
	if err := repository.Delete(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}

	// 配额 4 字节、占用 4 字节：再次上传同一内容会被拒绝。
	setTestPolicy(t, service, MaintenancePolicy{QuotaBytes: 4, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod})
	if _, err := service.Put(ctx, textPutRequest("user-1", "again.txt"), bytes.NewBufferString(orphanContent)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("超配额上传必须被拒绝，got %v", err)
	}
	// 该对象不是本次请求创建的，拒绝路径必须把它留给 GC，而不是删除可能是
	// 并发写入方刚提交的内容。
	if _, statErr := os.Stat(objectFilePath(store, orphan.StorageKey)); statErr != nil {
		t.Fatalf("既存对象不应被拒绝路径删除: %v", statErr)
	}
	// 而 GC 仍然可以在保护期后回收它。
	agePath(t, objectFilePath(store, orphan.StorageKey), 2*time.Hour)
	result, err := service.GarbageCollectObjects(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Fatalf("孤儿对象最终必须被 GC 回收: %+v", result)
	}
}

func TestExpiredArtifactStaysReadableByOwnerButNotDeletableForever(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()

	// 先放入一个真实对象，再构造一条保留期已经结束的元数据。
	object, err := store.Put(ctx, PutRequest{UserID: "user-1", Kind: KindDiff, Name: "old.patch", MIMEType: "text/plain"}, bytes.NewReader([]byte("old diff")))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	created := now.Add(-2 * time.Hour)
	expired := now.Add(-time.Hour)
	item := Artifact{
		ID: "artifact-expired", Version: 1, UserID: "user-1", Kind: KindDiff, Name: "old.patch",
		MIMEType: object.MIMEType, Size: object.Size, Digest: object.Digest, StorageKey: object.Key,
		Status: StatusReady, ExpiresAt: &expired, CreatedAt: created, UpdatedAt: created,
	}
	if err := repository.Create(ctx, item); err != nil {
		t.Fatal(err)
	}

	// 读路径必须继续 fail-closed。
	if _, err := service.Get(ctx, "user-1", item.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期内容仍应拒绝读取, got %v", err)
	}
	if _, _, _, err := service.Open(ctx, "user-1", item.ID, ByteRange{}); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期内容仍应拒绝下载, got %v", err)
	}
	// 归属检查不因过期而放宽。
	if err := service.Delete(ctx, "user-2", item.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("跨用户删除仍应被拒绝, got %v", err)
	}
	usage, err := service.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Artifacts != 1 {
		t.Fatalf("前置条件：过期内容确实占用配额: %+v", usage)
	}

	// 拥有者必须能删除它，否则过期内容永远无法回收并一直占配额。
	if err := service.Delete(ctx, "user-1", item.ID); err != nil {
		t.Fatalf("拥有者必须能删除过期内容: %v", err)
	}
	if _, err := repository.Get(ctx, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期内容的元数据必须被删除, got %v", err)
	}
	if _, statErr := os.Stat(objectFilePath(store, object.Key)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("过期内容的对象必须被回收: %v", statErr)
	}
	released, err := service.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if released.Artifacts != 0 || released.Bytes != 0 || released.Objects != 0 {
		t.Fatalf("删除过期内容后必须释放配额: %+v", released)
	}
}
