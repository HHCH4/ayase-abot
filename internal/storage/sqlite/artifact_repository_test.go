package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"Abot/internal/artifact"
)

func TestSQLiteArtifactRepositoryRoundTripCASAndConversationDelete(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	repository := store.ArtifactRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	item := artifact.Artifact{
		ID: "artifact-sqlite", Version: 1, UserID: "user-1", ConversationID: "conversation-sqlite", InvocationID: "inv-sqlite",
		ProducerType: "test", ProducerID: "producer-1", Kind: artifact.KindTestReport, Name: "report.json", MIMEType: "application/json",
		Size: 4, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", StorageKey: "sha256/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SecurityClass: "private", Status: artifact.StatusReady, Metadata: map[string]any{"preview": "okay", "count": float64(1)}, CreatedAt: now, UpdatedAt: now,
	}
	ctx := context.Background()
	if err := repository.Create(ctx, item); err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != item.ID || loaded.Metadata["preview"] != "okay" || loaded.StorageKey != item.StorageKey || !loaded.CreatedAt.Equal(item.CreatedAt) {
		t.Fatalf("roundtrip mismatch: %#v", loaded)
	}
	loaded.Status = artifact.StatusDeleting
	loaded.Version = 2
	loaded.UpdatedAt = now.Add(time.Second)
	if err := repository.Update(ctx, loaded, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	stale := loaded
	stale.Status = artifact.StatusReady
	stale.Version = 2
	if err := repository.Update(ctx, stale, 1); !errors.Is(err, artifact.ErrConflict) {
		t.Fatalf("expected stale CAS conflict, got %v", err)
	}
	if count, err := repository.CountByStorageKey(ctx, item.StorageKey); err != nil || count != 1 {
		t.Fatalf("unexpected storage ref count=%d err=%v", count, err)
	}
	second := item
	second.ID = "artifact-sqlite-second"
	second.ProducerID = "producer-2"
	second.ConversationID = "conversation-sqlite"
	if err := repository.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.DeleteByConversation(ctx, "conversation-sqlite")
	if err != nil || len(deleted) != 2 {
		t.Fatalf("conversation delete returned %d err=%v", len(deleted), err)
	}
	if _, err := repository.Get(ctx, item.ID); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("expected deleted artifact, got %v", err)
	}
	if _, err := repository.FindReadyByProducerDigest(ctx, item.ProducerType, item.ProducerID, item.Digest); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("expected no ready producer after delete, got %v", err)
	}
}
