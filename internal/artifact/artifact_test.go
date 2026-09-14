package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestService(t *testing.T) (*Service, *MemoryRepository, *LocalContentStore) {
	t.Helper()
	root := t.TempDir()
	store, err := NewLocalContentStore(root)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewMemoryRepository()
	service, err := NewService(repository, store)
	if err != nil {
		t.Fatal(err)
	}
	return service, repository, store
}

func TestPutOpenRangeOwnershipAndDelete(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	item, err := service.Put(ctx, PutRequest{
		UserID: "user-1", ConversationID: "conversation-1", Kind: KindInputAttachment,
		Name: "notes.txt", MIMEType: "text/plain", Metadata: map[string]any{"source": "test"},
	}, bytes.NewBufferString("abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != StatusReady || item.Size != 6 || !validDigest(item.Digest) || item.StorageKey == "" {
		t.Fatalf("unexpected stored artifact: %+v", item)
	}
	if _, err := os.Stat(filepath.Join(store.root, "objects", filepath.FromSlash(item.StorageKey))); err != nil {
		t.Fatalf("stored object missing: %v", err)
	}
	ref := item.Ref()
	if ref.ID != item.ID || ref.Digest != item.Digest || ref.Size != item.Size {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	opened, info, got, err := service.Open(ctx, "user-1", item.ID, ByteRange{Start: 2, Length: 3, HasRange: true})
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(opened)
	_ = opened.Close()
	if readErr != nil || string(data) != "cde" || info.RangeStart != 2 || info.RangeLength != 3 || got.ID != item.ID {
		t.Fatalf("unexpected range: data=%q info=%+v got=%+v err=%v", data, info, got, readErr)
	}
	if _, _, _, err := service.Open(ctx, "user-2", item.ID, ByteRange{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden open, got %v", err)
	}
	if _, err := service.Get(ctx, "user-2", item.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden get, got %v", err)
	}
	if err := service.Delete(ctx, "user-2", item.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden delete, got %v", err)
	}
	if err := service.Delete(ctx, "user-1", item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(ctx, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected repository deletion, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "objects", filepath.FromSlash(item.StorageKey))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected object deletion, got %v", err)
	}
	// A ready metadata row with a missing content object fails closed rather
	// than returning an empty or partially reconstructed payload.
	missingItem, err := service.Put(ctx, PutRequest{UserID: "user-1", Kind: KindBinary, Name: "missing.bin"}, bytes.NewBufferString("missing"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.root, "objects", filepath.FromSlash(missingItem.StorageKey))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.Open(ctx, "user-1", missingItem.ID, ByteRange{}); !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("expected missing object error, got %v", err)
	}
	if err := service.Delete(ctx, "user-1", item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing deletion, got %v", err)
	}
}

func TestPutDedupeOnlyForSameProducerAndPreservesSharedObject(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	request := PutRequest{UserID: "user-1", ConversationID: "conversation-1", ProducerType: "command", ProducerID: "run-1", Kind: KindCommandLog, Name: "log.txt", MIMEType: "text/plain"}
	first, err := service.Put(ctx, request, bytes.NewBufferString("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Put(ctx, request, bytes.NewBufferString("same"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected producer retry dedupe, first=%s second=%s", first.ID, second.ID)
	}
	opened, _, _, err := service.Open(ctx, "user-1", first.ID, ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || string(data) != "same" {
		t.Fatalf("dedupe removed shared object: data=%q err=%v", data, err)
	}
	other := request
	other.ProducerID = "run-2"
	other.ConversationID = "conversation-2"
	third, err := service.Put(ctx, other, bytes.NewBufferString("same"))
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("different producer unexpectedly deduped")
	}
	count, err := repository.CountByStorageKey(ctx, first.StorageKey)
	if err != nil || count != 2 {
		t.Fatalf("expected two refs, count=%d err=%v", count, err)
	}
	if err := service.Delete(ctx, "user-1", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "objects", filepath.FromSlash(third.StorageKey))); err != nil {
		t.Fatalf("shared object removed too early: %v", err)
	}
	if err := service.Delete(ctx, "user-2", third.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden shared-ref delete, got %v", err)
	}
	if err := service.Delete(ctx, "user-1", third.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "objects", filepath.FromSlash(third.StorageKey))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected final object deletion, got %v", err)
	}
}

func TestPutValidationMIMESizeMetadataAndExpiry(t *testing.T) {
	service, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.Put(ctx, PutRequest{UserID: "u", Kind: KindBinary, Name: "x.bin", MIMEType: "image/png"}, bytes.NewBufferString("plain text")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected MIME mismatch, got %v", err)
	}
	if _, err := service.Put(ctx, PutRequest{UserID: "u", Kind: KindBinary, Name: "x.bin", MIMEType: "text/plain", MaxBytes: 3}, bytes.NewBufferString("four")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected size error, got %v", err)
	}
	if _, err := service.Put(ctx, PutRequest{UserID: "u", Kind: KindBinary, Name: "x.bin", MIMEType: "text/plain", Metadata: map[string]any{"api_token": "redacted"}}, bytes.NewBufferString("ok")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected sensitive metadata rejection, got %v", err)
	}
	expires := time.Now().UTC().Add(-time.Minute)
	if _, err := service.Put(ctx, PutRequest{UserID: "u", Kind: KindBinary, Name: "x.bin", MIMEType: "text/plain", ExpiresAt: &expires}, bytes.NewBufferString("ok")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected invalid expiry rejection, got %v", err)
	}
	future := time.Now().UTC().Add(time.Minute)
	item, err := service.Put(ctx, PutRequest{UserID: "u", Kind: KindBinary, Name: "x.bin", MIMEType: "text/plain", ExpiresAt: &future}, bytes.NewBufferString("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, "u", item.ID); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return future.Add(time.Minute) }
	if _, err := service.Get(ctx, "u", item.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected expiry, got %v", err)
	}
}

func TestPutSecretScanQuarantinesArtifact(t *testing.T) {
	service, _, _ := newTestService(t)
	ctx := context.Background()
	item, err := service.Put(ctx, PutRequest{UserID: "u", Kind: KindInputAttachment, Name: "env.txt", MIMEType: "text/plain"}, bytes.NewBufferString("OPENAI_API_KEY=sk-proj-012345678901234567890123"))
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != StatusQuarantined || item.SecurityClass != "secret" || item.Error == "" || item.Digest == "" {
		t.Fatalf("secret artifact should be quarantined: %+v", item)
	}
	if _, _, _, err := service.Open(ctx, "u", item.ID, ByteRange{}); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("quarantined artifact must not open: %v", err)
	}
	if _, err := service.Get(ctx, "u", item.ID); err != nil {
		t.Fatalf("quarantined metadata should remain readable: %v", err)
	}
	safe, err := service.Put(ctx, PutRequest{UserID: "u", Kind: KindInputAttachment, Name: "note.txt", MIMEType: "text/plain"}, bytes.NewBufferString("ordinary notes"))
	if err != nil || safe.Status != StatusReady {
		t.Fatalf("ordinary content should remain ready: item=%+v err=%v", safe, err)
	}
}

func TestSecretScanReaderDetectsChunkBoundary(t *testing.T) {
	rule := scanSecretBytes([]byte("password=0123456789abcdef"))
	if rule != "named_secret" {
		t.Fatalf("expected named secret rule, got %q", rule)
	}
	reader := newSecretScanReader(io.MultiReader(bytes.NewBufferString("OPENAI_"), bytes.NewBufferString("API_KEY=sk-proj-012345678901234567890123")))
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if reader.matchedRule() != "openai_key" && reader.matchedRule() != "named_secret" {
		t.Fatalf("chunk-boundary secret was not detected: %q", reader.matchedRule())
	}
}

func TestLocalContentStoreRejectsUnsafeKeysAndRanges(t *testing.T) {
	_, _, store := newTestService(t)
	ctx := context.Background()
	if _, _, err := store.Open(ctx, "../secret", ByteRange{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected unsafe key rejection, got %v", err)
	}
	if _, _, err := store.Open(ctx, "sha256/ab/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ByteRange{}); !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("expected missing object, got %v", err)
	}
	if err := (ByteRange{Start: 0, Length: 1, HasRange: true}).Validate(0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected empty range rejection, got %v", err)
	}
}

func TestDeleteConversationReclaimsOnlyUnreferencedObjects(t *testing.T) {
	service, repository, store := newTestService(t)
	ctx := context.Background()
	first, err := service.Put(ctx, PutRequest{UserID: "u", ConversationID: "conversation-a", Kind: KindBinary, Name: "a.bin"}, bytes.NewBufferString("shared"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Put(ctx, PutRequest{UserID: "u", ConversationID: "conversation-b", Kind: KindBinary, Name: "b.bin"}, bytes.NewBufferString("shared"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Put(ctx, PutRequest{UserID: "u", ConversationID: "conversation-a", Kind: KindBinary, Name: "c.bin"}, bytes.NewBufferString("only-a")); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteConversation(ctx, "conversation-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("conversation artifact metadata remains: %v", err)
	}
	if _, _, _, err := service.Open(ctx, "u", second.ID, ByteRange{}); err != nil {
		t.Fatalf("shared object was reclaimed while another ref exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "objects", filepath.FromSlash(second.StorageKey))); err != nil {
		t.Fatalf("shared object missing: %v", err)
	}
	if err := service.DeleteConversation(ctx, "conversation-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "objects", filepath.FromSlash(second.StorageKey))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final object was not reclaimed: %v", err)
	}
}
