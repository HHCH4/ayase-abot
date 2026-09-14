package artifact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file owns the bounded lifecycle maintenance surface for the local
// content-addressed store: a soft total quota, a sweep for uploads that never
// completed, and object reclamation (orphan garbage collection plus a durable
// deletion outbox). Every capability is opt-in through an optional repository
// or store extension so older embedders keep their current behaviour.

const (
	// DefaultQuotaBytes is the conservative total budget for one local store.
	// It is intentionally generous for a single-user deployment and can be
	// disabled by setting the quota to zero.
	DefaultQuotaBytes int64 = 4 << 30
	// MaxQuotaBytes bounds the configurable quota so a typo cannot disable the
	// accounting entirely with a huge value.
	MaxQuotaBytes int64 = 1 << 40
	// DefaultStaleUploadAge is how long an artifact may stay in the uploading
	// state before its metadata is closed as failed.
	DefaultStaleUploadAge = 24 * time.Hour
	// DefaultObjectGracePeriod protects an object that was just written but
	// whose metadata row is not committed yet from being collected.
	DefaultObjectGracePeriod = time.Hour

	minStaleUploadAge = 5 * time.Minute
	maxStaleUploadAge = 30 * 24 * time.Hour
	maxObjectGrace    = 24 * time.Hour

	// maxSweepBatch and maxObjectDeletionBatch bound one maintenance pass so a
	// large store cannot stall startup for an unbounded time.
	maxSweepBatch           = 500
	maxObjectDeletionBatch  = 200
	maxGarbageCollectBatch  = 2000
	objectDeletionAttempts  = 8
	objectDeletionBaseDelay = time.Minute
	objectDeletionMaxDelay  = 6 * time.Hour
)

var (
	ErrQuotaExceeded = errors.New("artifact 存储配额不足")
	ErrUnsupported   = errors.New("artifact 维护能力未启用")
)

// MaintenancePolicy is the local lifecycle policy. Values are validated by
// SetMaintenancePolicy so a configuration mistake cannot silently disable
// reclamation.
type MaintenancePolicy struct {
	QuotaBytes        int64
	StaleUploadAge    time.Duration
	ObjectGracePeriod time.Duration
}

func DefaultMaintenancePolicy() MaintenancePolicy {
	return MaintenancePolicy{QuotaBytes: DefaultQuotaBytes, StaleUploadAge: DefaultStaleUploadAge, ObjectGracePeriod: DefaultObjectGracePeriod}
}

// StoredUsage is the physical, de-duplicated footprint of the store. Content
// addressed objects shared by several artifacts are counted once.
type StoredUsage struct {
	Bytes     int64 `json:"bytes"`
	Objects   int   `json:"objects"`
	Artifacts int   `json:"artifacts"`
}

// Usage reports the current footprint together with the active policy.
type Usage struct {
	StoredUsage
	QuotaBytes    int64 `json:"quota_bytes"`
	QuotaExceeded bool  `json:"quota_exceeded"`
}

// StoredObjectMeta describes one object that exists on disk. It carries no user
// supplied path: Key is always a validated content-addressed key.
type StoredObjectMeta struct {
	Key        string
	Size       int64
	ModifiedAt time.Time
}

// ObjectDeletion is one durable pending object deletion. It is deliberately
// metadata-only: the key is never derived from user input.
type ObjectDeletion struct {
	StorageKey    string
	Attempts      int
	RequestedAt   time.Time
	NextAttemptAt time.Time
	LastError     string
}

// The following extensions are optional. Each one is detected with a type
// assertion so an embedder that cannot provide the boundary keeps the previous
// behaviour instead of failing.

// UsageRepository reports the de-duplicated physical footprint.
type UsageRepository interface {
	StoredUsage(context.Context) (StoredUsage, error)
}

// ActiveReferenceRepository counts references that still need the object to
// exist. Rows that are already deleting (or failed) are not references: their
// owner has asked for the object to go away.
type ActiveReferenceRepository interface {
	CountActiveByStorageKey(context.Context, string) (int, error)
}

// StaleArtifactRepository lists rows whose status stopped making progress.
type StaleArtifactRepository interface {
	ListStaleUploading(context.Context, time.Time, int) ([]Artifact, error)
	ListStaleDeleting(context.Context, time.Time, int) ([]Artifact, error)
}

// ObjectDeletionRepository is the durable delete outbox. It is what makes an
// object-delete failure recoverable without keeping the metadata row alive.
type ObjectDeletionRepository interface {
	EnqueueObjectDeletion(context.Context, string, time.Time) error
	ListDueObjectDeletions(context.Context, time.Time, int) ([]ObjectDeletion, error)
	CompleteObjectDeletion(context.Context, string) error
	RescheduleObjectDeletion(context.Context, ObjectDeletion, time.Time) error
}

// PendingObjectDeletionRepository reports whether a key is already queued, so a
// garbage-collection pass does not enqueue duplicates.
type PendingObjectDeletionRepository interface {
	HasObjectDeletion(context.Context, string) (bool, error)
}

// ObjectLister enumerates stored objects for garbage collection.
type ObjectLister interface {
	ListObjects(context.Context) ([]StoredObjectMeta, error)
}

// TemporaryObjectSweeper removes partially written upload files left behind by
// a crash. It never touches committed content-addressed objects.
type TemporaryObjectSweeper interface {
	SweepTemporaryObjects(context.Context, time.Time) (int, error)
}

// SetMaintenancePolicy installs the local lifecycle policy. Invalid values are
// rejected instead of being clamped, so an operator sees the mistake.
func (s *Service) SetMaintenancePolicy(policy MaintenancePolicy) error {
	if s == nil {
		return fmt.Errorf("%w: artifact Service 不能为空", ErrInvalidRequest)
	}
	if policy.QuotaBytes < 0 || policy.QuotaBytes > MaxQuotaBytes {
		return fmt.Errorf("%w: artifact 配额必须在 0 到 %d 字节之间", ErrInvalidRequest, MaxQuotaBytes)
	}
	if policy.StaleUploadAge < minStaleUploadAge || policy.StaleUploadAge > maxStaleUploadAge {
		return fmt.Errorf("%w: 上传超时阈值必须在 %s 到 %s 之间", ErrInvalidRequest, minStaleUploadAge, maxStaleUploadAge)
	}
	if policy.ObjectGracePeriod < 0 || policy.ObjectGracePeriod > maxObjectGrace {
		return fmt.Errorf("%w: 对象保护期必须在 0 到 %s 之间", ErrInvalidRequest, maxObjectGrace)
	}
	s.policyMu.Lock()
	s.policy = policy
	s.policyMu.Unlock()
	return nil
}

func (s *Service) maintenancePolicy() MaintenancePolicy {
	if s == nil {
		return DefaultMaintenancePolicy()
	}
	s.policyMu.RLock()
	policy := s.policy
	s.policyMu.RUnlock()
	if policy.StaleUploadAge <= 0 {
		policy.StaleUploadAge = DefaultStaleUploadAge
	}
	return policy
}

// Usage returns the current footprint. A repository without the usage
// extension reports an unknown (zero) footprint rather than an error, because
// the caller still needs the effective policy to render.
func (s *Service) Usage(ctx context.Context) (Usage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	policy := s.maintenancePolicy()
	usage := Usage{QuotaBytes: policy.QuotaBytes}
	repository, ok := s.repository.(UsageRepository)
	if !ok {
		return usage, nil
	}
	stored, err := repository.StoredUsage(ctx)
	if err != nil {
		return Usage{}, err
	}
	usage.StoredUsage = stored
	usage.QuotaExceeded = policy.QuotaBytes > 0 && stored.Bytes > policy.QuotaBytes
	return usage, nil
}

// enforceQuota is the pre-commit admission check for a freshly written object.
// It is a bounded soft limit: the object is already on disk, so the check runs
// before the metadata row is published and deletes the new object on refusal.
// Two concurrent uploads can therefore both observe the same free space; the
// next upload is refused once the footprint is over budget.
func (s *Service) enforceQuota(ctx context.Context, object StoredObject) error {
	policy := s.maintenancePolicy()
	if policy.QuotaBytes <= 0 {
		return nil
	}
	repository, ok := s.repository.(UsageRepository)
	if !ok {
		// Without the usage extension the previous unlimited behaviour is kept
		// instead of refusing every upload.
		return nil
	}
	usage, err := repository.StoredUsage(ctx)
	if err != nil {
		return err
	}
	if usage.Bytes+object.Size <= policy.QuotaBytes {
		return nil
	}
	// A content-addressed hit stores no new bytes: the object is already
	// accounted for by the artifact that references it.
	if count, countErr := s.repository.CountByStorageKey(ctx, object.Key); countErr == nil && count > 0 {
		return nil
	}
	return fmt.Errorf("%w: 当前占用 %d 字节，上限 %d 字节", ErrQuotaExceeded, usage.Bytes, policy.QuotaBytes)
}

// activeReferenceCount counts the artifacts that still require the object.
// Repositories without the extension fall back to the conservative count that
// also treats deleting rows as references.
func (s *Service) activeReferenceCount(ctx context.Context, storageKey string) (int, error) {
	if repository, ok := s.repository.(ActiveReferenceRepository); ok {
		return repository.CountActiveByStorageKey(ctx, storageKey)
	}
	return s.repository.CountByStorageKey(ctx, storageKey)
}

// enqueueObjectDeletion registers a durable deletion intent. It reports
// ErrUnsupported when the repository cannot persist the intent.
func (s *Service) enqueueObjectDeletion(ctx context.Context, storageKey string, now time.Time) error {
	repository, ok := s.repository.(ObjectDeletionRepository)
	if !ok {
		return ErrUnsupported
	}
	return repository.EnqueueObjectDeletion(ctx, storageKey, now)
}

// UploadSweepResult summarises one stale-artifact sweep.
type UploadSweepResult struct {
	Scanned          int `json:"scanned"`
	UploadsFailed    int `json:"uploads_failed"`
	DeletionsClosed  int `json:"deletions_closed"`
	TemporaryRemoved int `json:"temporary_removed"`
	Deferred         int `json:"deferred"`
}

// ObjectDeletionResult summarises one delete-outbox drain. It reports counts
// only: the outbox stores the object key, not its size, so no reclaimed byte
// total is claimed here. The garbage-collection result carries byte totals
// because that pass enumerates the objects themselves.
type ObjectDeletionResult struct {
	Scanned  int `json:"scanned"`
	Deleted  int `json:"deleted"`
	Skipped  int `json:"skipped"`
	Deferred int `json:"deferred"`
}

// GarbageCollectResult summarises one orphan-object pass.
type GarbageCollectResult struct {
	Scanned        int   `json:"scanned"`
	Deleted        int   `json:"deleted"`
	Skipped        int   `json:"skipped"`
	Deferred       int   `json:"deferred"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
}

// MaintenanceResult is the combined outcome of one maintenance pass.
type MaintenanceResult struct {
	UploadSweep       UploadSweepResult    `json:"upload_sweep"`
	ObjectDeletions   ObjectDeletionResult `json:"object_deletions"`
	GarbageCollection GarbageCollectResult `json:"garbage_collection"`
}

func resolveMaintenanceNow(now time.Time, fallback func() time.Time) time.Time {
	if !now.IsZero() {
		return now.UTC()
	}
	if fallback != nil {
		return fallback().UTC()
	}
	return time.Now().UTC()
}

// SweepStaleArtifacts closes lifecycle rows that stopped making progress:
//
//  1. an upload that never completed is closed as failed. Its metadata stays
//     visible so the user can see what happened;
//  2. a deletion that was interrupted after the status flip is completed. The
//     user already asked for the object to disappear, so finishing the removal
//     is the safe direction, and the object itself is reclaimed by the delete
//     outbox or the garbage-collection pass;
//  3. temporary upload files left behind by a crash are removed.
//
// The pass is idempotent, bounded and safe to run from several workers.
func (s *Service) SweepStaleArtifacts(ctx context.Context, now time.Time) (UploadSweepResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now = resolveMaintenanceNow(now, s.now)
	policy := s.maintenancePolicy()
	cutoff := now.Add(-policy.StaleUploadAge)
	result := UploadSweepResult{}

	repository, ok := s.repository.(StaleArtifactRepository)
	if ok {
		uploading, err := repository.ListStaleUploading(ctx, cutoff, maxSweepBatch)
		if err != nil {
			return result, err
		}
		result.Scanned += len(uploading)
		for _, item := range uploading {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if item.Status != StatusUploading {
				continue
			}
			updated := item
			updated.Status = StatusFailed
			updated.Error = "上传未在保留期限内完成，已安全终止"
			updated.UpdatedAt = now
			updated.Version = item.Version + 1
			if err := s.repository.Update(ctx, updated, int64(item.Version)); err != nil {
				if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
					// Another worker closed or removed the row; the next pass
					// inspects the winner.
					result.Deferred++
					continue
				}
				return result, err
			}
			result.UploadsFailed++
		}

		deleting, err := repository.ListStaleDeleting(ctx, cutoff, maxSweepBatch)
		if err != nil {
			return result, err
		}
		result.Scanned += len(deleting)
		for _, item := range deleting {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if item.Status != StatusDeleting {
				continue
			}
			if key := strings.TrimSpace(item.StorageKey); key != "" {
				// The object may be shared with a concurrent upload. Only queue
				// it when nothing still references it, otherwise the next pass
				// re-checks after the other reference disappears.
				count, countErr := s.activeReferenceCount(ctx, key)
				if countErr != nil {
					return result, countErr
				}
				if count <= 1 {
					if enqueueErr := s.enqueueObjectDeletion(ctx, key, now); enqueueErr != nil && !errors.Is(enqueueErr, ErrUnsupported) {
						return result, enqueueErr
					}
				}
			}
			if err := s.repository.Delete(ctx, item.ID); err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return result, err
			}
			result.DeletionsClosed++
		}
	}

	if sweeper, ok := s.store.(TemporaryObjectSweeper); ok {
		removed, err := sweeper.SweepTemporaryObjects(ctx, now.Add(-policy.StaleUploadAge))
		if err != nil {
			return result, err
		}
		result.TemporaryRemoved = removed
	}
	return result, nil
}

// DrainObjectDeletions retries queued object deletions. A queued key that is
// referenced again is dropped from the queue instead of being deleted: the
// content is live again, so the previous deletion intent is obsolete.
func (s *Service) DrainObjectDeletions(ctx context.Context, now time.Time) (ObjectDeletionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now = resolveMaintenanceNow(now, s.now)
	result := ObjectDeletionResult{}
	repository, ok := s.repository.(ObjectDeletionRepository)
	if !ok {
		return result, nil
	}
	pending, err := repository.ListDueObjectDeletions(ctx, now, maxObjectDeletionBatch)
	if err != nil {
		return result, err
	}
	result.Scanned = len(pending)
	for _, deletion := range pending {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		count, countErr := s.activeReferenceCount(ctx, deletion.StorageKey)
		if countErr != nil {
			return result, countErr
		}
		if count > 0 {
			if err := repository.CompleteObjectDeletion(ctx, deletion.StorageKey); err != nil && !errors.Is(err, ErrNotFound) {
				return result, err
			}
			result.Skipped++
			continue
		}
		deleteErr := s.store.Delete(ctx, deletion.StorageKey)
		if deleteErr == nil || errors.Is(deleteErr, ErrObjectMissing) {
			if err := repository.CompleteObjectDeletion(ctx, deletion.StorageKey); err != nil && !errors.Is(err, ErrNotFound) {
				return result, err
			}
			result.Deleted++
			continue
		}
		deletion.Attempts++
		deletion.LastError = safeError(deleteErr)
		deletion.NextAttemptAt = now.Add(objectDeletionBackoff(deletion.Attempts))
		if err := repository.RescheduleObjectDeletion(ctx, deletion, now); err != nil && !errors.Is(err, ErrNotFound) {
			return result, err
		}
		result.Deferred++
	}
	return result, nil
}

func objectDeletionBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := objectDeletionBaseDelay
	for index := 1; index < attempts && delay < objectDeletionMaxDelay; index++ {
		delay *= 2
	}
	if delay > objectDeletionMaxDelay {
		delay = objectDeletionMaxDelay
	}
	return delay
}

// GarbageCollectObjects removes content-addressed objects that no artifact
// references any more. Objects younger than the grace period are left alone so
// an upload that is still committing its metadata row cannot be collected.
// Failures are queued in the delete outbox when the repository supports it and
// never abort the whole pass.
func (s *Service) GarbageCollectObjects(ctx context.Context, now time.Time) (GarbageCollectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now = resolveMaintenanceNow(now, s.now)
	lister, ok := s.store.(ObjectLister)
	if !ok {
		return GarbageCollectResult{}, ErrUnsupported
	}
	objects, err := lister.ListObjects(ctx)
	if err != nil {
		return GarbageCollectResult{}, err
	}
	policy := s.maintenancePolicy()
	cutoff := now.Add(-policy.ObjectGracePeriod)
	result := GarbageCollectResult{}
	// One pass is bounded. The object list is enumerated in a deterministic
	// order, so a large store is drained over several passes instead of being
	// skipped forever.
	limit := len(objects)
	if limit > maxGarbageCollectBatch {
		limit = maxGarbageCollectBatch
	}
	result.Deferred = len(objects) - limit
	for _, object := range objects[:limit] {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Scanned++
		if object.ModifiedAt.After(cutoff) {
			result.Skipped++
			continue
		}
		count, countErr := s.activeReferenceCount(ctx, object.Key)
		if countErr != nil {
			return result, countErr
		}
		if count > 0 {
			result.Skipped++
			continue
		}
		if pending, pendingErr := s.hasPendingObjectDeletion(ctx, object.Key); pendingErr == nil && pending {
			result.Skipped++
			continue
		}
		if deleteErr := s.store.Delete(ctx, object.Key); deleteErr != nil {
			if errors.Is(deleteErr, ErrObjectMissing) {
				continue
			}
			if enqueueErr := s.enqueueObjectDeletion(ctx, object.Key, now); enqueueErr == nil {
				result.Deferred++
				continue
			}
			// Neither removal nor a durable retry is available: leave the
			// object in place and report it instead of failing the pass.
			result.Deferred++
			continue
		}
		result.Deleted++
		result.ReclaimedBytes += object.Size
	}
	return result, nil
}

func (s *Service) hasPendingObjectDeletion(ctx context.Context, storageKey string) (bool, error) {
	repository, ok := s.repository.(PendingObjectDeletionRepository)
	if !ok {
		return false, ErrUnsupported
	}
	return repository.HasObjectDeletion(ctx, storageKey)
}

// RunMaintenance runs the sweep, the delete-outbox drain and the orphan pass in
// the order that keeps the footprint consistent: close abandoned rows first,
// then retry known deletions, then collect what is left over.
func (s *Service) RunMaintenance(ctx context.Context, now time.Time) (MaintenanceResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now = resolveMaintenanceNow(now, s.now)
	result := MaintenanceResult{}
	sweep, err := s.SweepStaleArtifacts(ctx, now)
	result.UploadSweep = sweep
	if err != nil {
		return result, err
	}
	deletions, err := s.DrainObjectDeletions(ctx, now)
	result.ObjectDeletions = deletions
	if err != nil {
		return result, err
	}
	collected, err := s.GarbageCollectObjects(ctx, now)
	if errors.Is(err, ErrUnsupported) {
		return result, nil
	}
	result.GarbageCollection = collected
	if err != nil {
		return result, err
	}
	return result, nil
}
