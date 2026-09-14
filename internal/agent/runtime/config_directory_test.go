package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/config"
)

const configDirectoryTestSecret = "config-directory-test-secret-32-bytes"

func configDirectoryProfile(t *testing.T, id string, revision int, value any) RuntimeConfigDirectoryEntry {
	t.Helper()
	entry, err := NewRuntimeConfigDirectoryProfileEntry(config.Profile{ID: id, Revision: revision, Name: "profile-" + id, Values: config.Values{"safe.value": value}})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func configDirectorySigned(t *testing.T, source, destination string, entry RuntimeConfigDirectoryEntry, previous string, at time.Time) RuntimeConfigDirectoryEnvelope {
	t.Helper()
	envelope, err := NewRuntimeConfigDirectoryEnvelope(source, destination, entry, previous, "corr-1", "key-"+entry.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err = SignRuntimeConfigDirectoryEnvelope(envelope, []byte(configDirectoryTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestRuntimeConfigDirectoryEntryDigestAndSecretBoundary(t *testing.T) {
	first := configDirectoryProfile(t, "profile-a", 1, map[string]any{"z": 2, "a": []any{"x", true}})
	second := configDirectoryProfile(t, "profile-a", 1, map[string]any{"a": []any{"x", true}, "z": 2})
	firstDigest, err := RuntimeConfigDirectoryEntryDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := RuntimeConfigDirectoryEntryDigest(second)
	if err != nil || firstDigest != secondDigest {
		t.Fatalf("map key 顺序不应改变 digest: first=%s second=%s err=%v", firstDigest, secondDigest, err)
	}
	encoded, _, err := canonicalRuntimeConfigDirectoryEntry(first)
	if err != nil || bytes.Contains(encoded, []byte("is_default")) {
		t.Fatalf("目录正文不应携带默认选择状态: %s err=%v", encoded, err)
	}
	secret := first
	secret.Values = map[string]any{"nested": map[string]any{"api_key": "do-not-send"}}
	if _, err := secret.Normalize(); !errors.Is(err, ErrInvalidRuntimeConfigDirectory) {
		t.Fatalf("嵌套敏感字段应拒绝，实际=%v", err)
	}
	rawSecret := first
	rawSecret.Values = map[string]any{"nested": json.RawMessage(`{"password":"do-not-send"}`)}
	if _, err := rawSecret.Normalize(); !errors.Is(err, ErrInvalidRuntimeConfigDirectory) {
		t.Fatalf("RawMessage 嵌套敏感字段也应拒绝，实际=%v", err)
	}
	persona, err := NewRuntimeConfigDirectoryPersonaEntry(config.Persona{ID: "persona-a", Revision: 2, Name: "persona", Instruction: "可靠助手", Enabled: false, IsDefault: true})
	if err != nil || persona.Enabled == nil || *persona.Enabled {
		t.Fatalf("persona 正文转换错误: %#v err=%v", persona, err)
	}
	if _, err := (RuntimeConfigDirectoryEntry{Kind: RuntimeConfigDirectoryKindPersona, ID: "persona-a", Revision: 1, Name: "bad", Values: map[string]any{}}).Normalize(); !errors.Is(err, ErrInvalidRuntimeConfigDirectory) {
		t.Fatalf("persona 携带 values 应拒绝，实际=%v", err)
	}
}

func TestRuntimeConfigDirectoryEnvelopeSignatureAndPair(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := configDirectoryProfile(t, "profile-sign", 1, "one")
	envelope := configDirectorySigned(t, "runtime-a", "runtime-b", entry, "", now)
	if err := VerifyRuntimeConfigDirectoryEnvelope(envelope, []byte(configDirectoryTestSecret)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NormalizeRuntimeConfigDirectoryPair(envelope, entry); err != nil {
		t.Fatal(err)
	}
	retry := envelope
	retry.IssuedAt = now.Add(time.Second)
	retry.Signature = ""
	retry, err := SignRuntimeConfigDirectoryEnvelope(retry, []byte(configDirectoryTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	if retry.DeliveryID != envelope.DeliveryID || retry.Signature == envelope.Signature {
		t.Fatalf("重试应保持 identity 并刷新签名: first=%#v retry=%#v", envelope, retry)
	}
	tampered := retry
	tampered.BodyDigest = "sha256:" + strings.Repeat("f", 64)
	if err := VerifyRuntimeConfigDirectoryEnvelope(tampered, []byte(configDirectoryTestSecret)); !errors.Is(err, ErrRuntimeConfigDirectoryAuth) {
		t.Fatalf("篡改 digest 应认证失败，实际=%v", err)
	}
	wrongEntry := configDirectoryProfile(t, "profile-sign", 2, "two")
	if _, _, err := NormalizeRuntimeConfigDirectoryPair(envelope, wrongEntry); !errors.Is(err, ErrRuntimeConfigDirectoryAuth) {
		t.Fatalf("正文与 envelope 不匹配应拒绝，实际=%v", err)
	}
}

func TestMemoryRuntimeConfigDirectoryIsMonotonicAndDefensive(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := NewMemoryRepository()
	first := configDirectoryProfile(t, "profile-memory", 1, "one")
	firstEnvelope := configDirectorySigned(t, "runtime-a", "runtime-b", first, "", now)
	if duplicate, err := repo.AcceptRuntimeConfigDirectory(ctx, firstEnvelope, first); err != nil || duplicate {
		t.Fatalf("首次接收失败 duplicate=%v err=%v", duplicate, err)
	}
	updated := first
	updated.Values = map[string]any{"safe.value": "changed"}
	updated.Revision = 2
	updatedEnvelope := configDirectorySigned(t, "runtime-a", "runtime-b", updated, firstEnvelope.BodyDigest, now.Add(time.Second))
	if duplicate, err := repo.AcceptRuntimeConfigDirectory(ctx, updatedEnvelope, updated); err != nil || duplicate {
		t.Fatalf("连续新版本接收失败 duplicate=%v err=%v", duplicate, err)
	}
	retry := updatedEnvelope
	retry.IssuedAt = retry.IssuedAt.Add(time.Second)
	retry.Signature = ""
	retry, _ = SignRuntimeConfigDirectoryEnvelope(retry, []byte(configDirectoryTestSecret))
	if duplicate, err := repo.AcceptRuntimeConfigDirectory(ctx, retry, updated); err != nil || !duplicate {
		t.Fatalf("重复新版本应幂等 duplicate=%v err=%v", duplicate, err)
	}
	old := firstEnvelope
	if _, err := repo.AcceptRuntimeConfigDirectory(ctx, old, first); !errors.Is(err, ErrRuntimeConfigDirectoryStale) {
		t.Fatalf("旧版本应 stale，实际=%v", err)
	}
	fork := updated
	fork.Values = map[string]any{"safe.value": "fork"}
	forkEnvelope := configDirectorySigned(t, "runtime-a", "runtime-b", fork, firstEnvelope.BodyDigest, now.Add(2*time.Second))
	if _, err := repo.AcceptRuntimeConfigDirectory(ctx, forkEnvelope, fork); !errors.Is(err, ErrRuntimeConfigDirectoryConflict) {
		t.Fatalf("同 revision fork 应 conflict，实际=%v", err)
	}
	wrongPrevious := updated
	wrongPrevious.Revision = 3
	wrongEnvelope := configDirectorySigned(t, "runtime-a", "runtime-b", wrongPrevious, "sha256:"+strings.Repeat("0", 64), now.Add(3*time.Second))
	if _, err := repo.AcceptRuntimeConfigDirectory(ctx, wrongEnvelope, wrongPrevious); !errors.Is(err, ErrRuntimeConfigDirectoryConflict) {
		t.Fatalf("错误 previous digest 应 conflict，实际=%v", err)
	}
	stored, err := repo.GetRuntimeConfigDirectory(ctx, RuntimeConfigDirectoryKindProfile, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Entry.Values["safe.value"] = "mutated"
	again, err := repo.GetRuntimeConfigDirectory(ctx, RuntimeConfigDirectoryKindProfile, first.ID)
	if err != nil || again.Entry.Values["safe.value"] != "changed" {
		t.Fatalf("Get 必须返回防御性副本: %#v err=%v", again, err)
	}
	bootstrap := configDirectoryProfile(t, "profile-bootstrap", 2, "two")
	bootstrapEnvelope := configDirectorySigned(t, "runtime-a", "runtime-b", bootstrap, firstEnvelope.BodyDigest, now)
	if _, err := repo.AcceptRuntimeConfigDirectory(ctx, bootstrapEnvelope, bootstrap); !errors.Is(err, ErrRuntimeConfigDirectoryConflict) {
		t.Fatalf("不存在 entry 不应接受 previous digest，实际=%v", err)
	}
}

func TestRuntimeConfigDirectoryHTTPTransportAndStrictReceiver(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := NewMemoryRepository()
	receiver, err := NewRuntimeConfigDirectoryReceiver("runtime-a", "runtime-b", []byte(configDirectoryTestSecret), repo)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Now = func() time.Time { return now }
	server := httptest.NewServer(receiver.Handler())
	defer server.Close()
	transport, err := NewRuntimeConfigDirectoryHTTPTransport(server.URL, "runtime-a", "runtime-b", []byte(configDirectoryTestSecret), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	entry := configDirectoryProfile(t, "profile-http", 1, "one")
	envelope := configDirectorySigned(t, "runtime-a", "runtime-b", entry, "", now)
	receipt, err := transport.Deliver(ctx, envelope, entry)
	if err != nil || receipt.Duplicate || receipt.Signature == "" {
		t.Fatalf("HTTP 首次正文投递失败 receipt=%#v err=%v", receipt, err)
	}
	retry := envelope
	retry.IssuedAt = now.Add(time.Second)
	retry.Signature = ""
	retry, _ = SignRuntimeConfigDirectoryEnvelope(retry, []byte(configDirectoryTestSecret))
	retryReceipt, err := transport.Deliver(ctx, retry, entry)
	if err != nil || !retryReceipt.Duplicate {
		t.Fatalf("HTTP 重试应 duplicate receipt=%#v err=%v", retryReceipt, err)
	}

	// Unknown fields and trailing JSON documents are rejected before storage.
	signedBody, _ := json.Marshal(RuntimeConfigDirectoryDelivery{Envelope: envelope, Entry: entry})
	var object map[string]any
	if err := json.Unmarshal(signedBody, &object); err != nil {
		t.Fatal(err)
	}
	object["extra"] = true
	unknown, _ := json.Marshal(object)
	request := httptest.NewRequest(http.MethodPost, server.URL, bytes.NewReader(unknown))
	request.Header.Set("X-Abot-Config-Directory-Version", "1")
	request.Header.Set("X-Abot-Config-Directory-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.DeliveryID)
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field 应 400，实际=%d body=%s", response.Code, response.Body.String())
	}
	trailing := append(signedBody, []byte(" {}")...)
	request = httptest.NewRequest(http.MethodPost, server.URL, bytes.NewReader(trailing))
	request.Header.Set("X-Abot-Config-Directory-Version", "1")
	request.Header.Set("X-Abot-Config-Directory-Signature", envelope.Signature)
	request.Header.Set("Idempotency-Key", envelope.DeliveryID)
	response = httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON 应 400，实际=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := repo.GetRuntimeConfigDirectory(ctx, RuntimeConfigDirectoryKindProfile, entry.ID); err != nil {
		t.Fatalf("严格失败不应删除已存正文: %v", err)
	}
}
