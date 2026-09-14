package runtime

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const runtimeDeliveryGroupFenceTestSecret = "0123456789abcdef0123456789abcdef"

func fencedRuntimeDeliveryGroup(t *testing.T, invocationID, memberID string, now time.Time, revision int64) (RuntimeDeliveryGroup, RuntimeDeliveryGroupEnvelope) {
	t.Helper()
	group, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", invocationID, []RuntimeDeliveryGroupMember{{Kind: RuntimeDeliveryKindEvent, OutboxID: memberID, DeliveryID: memberID}}, now)
	if err != nil {
		t.Fatal(err)
	}
	group.FenceID = RuntimeDeliveryGroupFenceID(group.Source, group.Destination, group.InvocationID)
	group.FenceRevision = revision
	group, err = group.Normalize(now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := NewRuntimeDeliveryGroupEnvelope(group, now)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = SignRuntimeDeliveryGroupEnvelope(envelope, []byte(runtimeDeliveryGroupFenceTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	return group, envelope
}

func TestRuntimeDeliveryGroupFenceMonotonicAndCompatibility(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repo := NewMemoryRepository()
	groupOne, envelopeOne := fencedRuntimeDeliveryGroup(t, "inv-fence-memory", "member-one", now, 1)
	fenceOne, err := repo.IssueRuntimeDeliveryGroupFence(ctx, groupOne.Source, groupOne.Destination, groupOne.InvocationID, groupOne.ID, now)
	if err != nil || fenceOne.Revision != 1 || fenceOne.FenceID != groupOne.FenceID {
		t.Fatalf("首次 issue fence 异常: %#v err=%v", fenceOne, err)
	}
	if duplicate, err := repo.PrepareRuntimeDeliveryGroup(ctx, envelopeOne); err != nil || duplicate {
		t.Fatalf("fenced prepare 异常: duplicate=%v err=%v", duplicate, err)
	}
	groupTwo, envelopeTwo := fencedRuntimeDeliveryGroup(t, "inv-fence-memory", "member-two", now.Add(time.Second), 2)
	fenceTwo, err := repo.IssueRuntimeDeliveryGroupFence(ctx, groupTwo.Source, groupTwo.Destination, groupTwo.InvocationID, groupTwo.ID, now.Add(time.Second))
	if err != nil || fenceTwo.Revision != 2 {
		t.Fatalf("推进 fence 异常: %#v err=%v", fenceTwo, err)
	}
	if duplicate, err := repo.PrepareRuntimeDeliveryGroup(ctx, envelopeTwo); err != nil || duplicate {
		t.Fatalf("新 fence prepare 异常: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := repo.CommitRuntimeDeliveryGroup(ctx, envelopeOne); !errors.Is(err, ErrRuntimeDeliveryFenceStale) {
		t.Fatalf("旧 fence commit 应拒绝: %v", err)
	}
	if duplicate, err := repo.CommitRuntimeDeliveryGroup(ctx, envelopeTwo); err != nil || duplicate {
		t.Fatalf("新 fence commit 异常: duplicate=%v err=%v", duplicate, err)
	}
	_, conflict := fencedRuntimeDeliveryGroup(t, "inv-fence-memory", "member-three", now.Add(2*time.Second), 2)
	if _, err = repo.PrepareRuntimeDeliveryGroup(ctx, conflict); !errors.Is(err, ErrRuntimeDeliveryFenceConflict) {
		t.Fatalf("同 revision 不同 group 应拒绝: %v", err)
	}
	// A legacy unfenced receiver/repository remains compatible.
	legacy, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-fence-legacy", []RuntimeDeliveryGroupMember{{Kind: RuntimeDeliveryKindEvent, OutboxID: "legacy", DeliveryID: "legacy"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyEnvelope, err := NewRuntimeDeliveryGroupEnvelope(legacy, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyEnvelope, err = SignRuntimeDeliveryGroupEnvelope(legacyEnvelope, []byte(runtimeDeliveryGroupFenceTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PrepareRuntimeDeliveryGroup(ctx, legacyEnvelope); err != nil {
		t.Fatalf("legacy unfenced prepare 不应回归: %v", err)
	}
}

func TestRuntimeDeliveryGroupFenceConcurrentIssueIsSingleWinner(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	now := time.Now().UTC()
	const workers = 16
	results := make(chan RuntimeDeliveryGroupFence, workers)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := 0; index < workers; index++ {
		go func() {
			defer wait.Done()
			fence, err := repo.IssueRuntimeDeliveryGroupFence(ctx, "runtime-a", "runtime-b", "inv-fence-concurrent", "group-fence-concurrent", now)
			if err != nil {
				errorsCh <- err
				return
			}
			results <- fence
		}()
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	for fence := range results {
		if fence.Revision != 1 {
			t.Fatalf("同一 group 并发 issue 不应推进 revision: %#v", fence)
		}
	}
	stored, err := repo.GetRuntimeDeliveryGroupFence(ctx, RuntimeDeliveryGroupFenceID("runtime-a", "runtime-b", "inv-fence-concurrent"))
	if err != nil || stored.Revision != 1 || stored.GroupID != "group-fence-concurrent" {
		t.Fatalf("并发 fence ledger 异常: %#v err=%v", stored, err)
	}
}

func TestRuntimeDeliveryGroupFenceCoordinatorBindsRequiredTransport(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	group, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-fence-coordinator", []RuntimeDeliveryGroupMember{{Kind: RuntimeDeliveryKindEvent, OutboxID: "coordinator-member", DeliveryID: "coordinator-member"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.EnqueueRuntimeDeliveryGroup(context.Background(), group)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{repo: repo}
	transport := &RuntimeDeliveryGroupHTTPTransport{Source: group.Source, Destination: group.Destination, requireFence: true}
	bound, err := coordinator.ensureRuntimeDeliveryGroupFence(context.Background(), stored, now, transport)
	if err != nil {
		t.Fatal(err)
	}
	if bound.FenceRevision != 1 || bound.FenceID != RuntimeDeliveryGroupFenceID(group.Source, group.Destination, group.InvocationID) || bound.Revision != stored.Revision+1 {
		t.Fatalf("Coordinator 未绑定首个 fence: %#v", bound)
	}
	reloaded, err := repo.GetRuntimeDeliveryGroup(context.Background(), group.ID)
	if err != nil || reloaded.FenceRevision != 1 || reloaded.Revision != bound.Revision {
		t.Fatalf("绑定后的 source group 未持久化: %#v err=%v", reloaded, err)
	}
	// A second coordinator observes the bound row and must not allocate a new
	// epoch or mutate the group revision.
	again, err := coordinator.ensureRuntimeDeliveryGroupFence(context.Background(), reloaded, now, transport)
	if err != nil || again.FenceRevision != 1 || again.Revision != reloaded.Revision {
		t.Fatalf("重复绑定不应推进 fence/revision: %#v err=%v", again, err)
	}
}

func TestRuntimeDeliveryGroupFenceHTTPRequirementAndStatusBinding(t *testing.T) {
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeDeliveryGroupReceiver("runtime-a", "runtime-b", []byte(runtimeDeliveryGroupFenceTestSecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.RequireFence = true
	server := httptest.NewServer(receiver)
	defer server.Close()
	now := time.Now().UTC()
	legacy, err := NewRuntimeDeliveryGroup("runtime-a", "runtime-b", "inv-fence-http-legacy", []RuntimeDeliveryGroupMember{{Kind: RuntimeDeliveryKindEvent, OutboxID: "legacy-http", DeliveryID: "legacy-http"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyEnvelope, err := NewRuntimeDeliveryGroupEnvelope(legacy, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyEnvelope, err = SignRuntimeDeliveryGroupEnvelope(legacyEnvelope, []byte(runtimeDeliveryGroupFenceTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewRuntimeDeliveryGroupHTTPTransport(server.URL, "runtime-a", "runtime-b", []byte(runtimeDeliveryGroupFenceTestSecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.PrepareRuntimeDeliveryGroup(context.Background(), legacyEnvelope); !errors.Is(err, ErrConflict) {
		t.Fatalf("RequireFence 未拒绝 legacy envelope: %v", err)
	}
	group, envelope := fencedRuntimeDeliveryGroup(t, "inv-fence-http", "member-http", now, 1)
	if _, err := repo.IssueRuntimeDeliveryGroupFence(context.Background(), group.Source, group.Destination, group.InvocationID, group.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.PrepareRuntimeDeliveryGroup(context.Background(), envelope); err != nil {
		t.Fatalf("RequireFence fenced prepare 异常: %v", err)
	}
	status, err := transport.ReconcileRuntimeDeliveryGroup(context.Background(), envelope)
	if err != nil || status.FenceID != envelope.FenceID || status.FenceRevision != envelope.FenceRevision {
		t.Fatalf("status 未绑定 fence: %#v err=%v", status, err)
	}
	// A signed status carrying a different fence must fail identity binding.
	status.FenceRevision++
	if err := status.ValidateAgainst(envelope); !errors.Is(err, ErrRuntimeDeliveryGroupTransactionAuth) {
		t.Fatalf("status fence 漂移未拒绝: %v", err)
	}
}
