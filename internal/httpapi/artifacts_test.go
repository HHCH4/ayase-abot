package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Abot/internal/agent"
	"Abot/internal/artifact"
)

func newArtifactHTTPHandler(t *testing.T) http.Handler {
	return newArtifactHTTPServer(t).Handler()
}

func newArtifactHTTPServer(t *testing.T) *Server {
	t.Helper()
	store, err := artifact.NewLocalContentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := artifact.NewService(artifact.NewMemoryRepository(), store)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetArtifactService(service)
	return server
}

func serveArtifactRequest(handler http.Handler, method, path, contentType, userID, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if userID != "" {
		request.Header.Set("X-Abot-User-ID", userID)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestArtifactHTTPUploadMetadataContentRangePreviewAndOwnership(t *testing.T) {
	handler := newArtifactHTTPHandler(t)
	created := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts?name=hello.txt&kind=input_attachment", "text/plain", "user-1", "hello world", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var item artifact.Artifact
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if item.ID == "" || item.Status != artifact.StatusReady || item.Size != 11 || item.StorageKey != "" {
		t.Fatalf("unexpected public artifact: %+v", item)
	}
	if location := created.Header().Get("Location"); location != "/api/v1/artifacts/"+item.ID {
		t.Fatalf("unexpected location %q", location)
	}
	metadata := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"?user_id=user-1", "", "", "", nil)
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"status":"ready"`) {
		t.Fatalf("metadata status=%d body=%s", metadata.Code, metadata.Body.String())
	}
	content := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/content", "", "user-1", "", map[string]string{"Range": "bytes=1-3"})
	if content.Code != http.StatusPartialContent || content.Body.String() != "ell" || content.Header().Get("Content-Range") != "bytes 1-3/11" || content.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("range status=%d body=%q headers=%v", content.Code, content.Body.String(), content.Header())
	}
	suffix := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/content", "", "user-1", "", map[string]string{"Range": "bytes=-5"})
	if suffix.Code != http.StatusPartialContent || suffix.Body.String() != "world" {
		t.Fatalf("suffix status=%d body=%q", suffix.Code, suffix.Body.String())
	}
	openEnded := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/content", "", "user-1", "", map[string]string{"Range": "bytes=6-"})
	if openEnded.Code != http.StatusPartialContent || openEnded.Body.String() != "world" {
		t.Fatalf("open-ended status=%d body=%q", openEnded.Code, openEnded.Body.String())
	}
	preview := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/preview", "", "user-1", "", nil)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"preview":"hello world"`) {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	forbidden := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/content", "", "user-2", "", nil)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("expected ownership denial, got %d body=%s", forbidden.Code, forbidden.Body.String())
	}
	list := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts?user_id=user-1", "", "", "", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), item.ID) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	deleted := serveArtifactRequest(handler, http.MethodDelete, "/api/v1/artifacts/"+item.ID, "", "user-1", "", nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	missing := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID, "", "user-1", "", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("expected deleted metadata to be missing, got %d body=%s", missing.Code, missing.Body.String())
	}
}

func TestArtifactHTTPSecretUploadIsQuarantined(t *testing.T) {
	handler := newArtifactHTTPHandler(t)
	secret := "OPENAI_API_KEY=sk-proj-012345678901234567890123"
	created := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts?name=.env&kind=input_attachment", "text/plain", "user-secret", secret, nil)
	if created.Code != http.StatusCreated || strings.Contains(created.Body.String(), "sk-proj-") {
		t.Fatalf("secret upload should return metadata without secret value: status=%d body=%s", created.Code, created.Body.String())
	}
	var item artifact.Artifact
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if item.Status != artifact.StatusQuarantined || item.SecurityClass != "secret" {
		t.Fatalf("secret upload was not quarantined: %+v", item)
	}
	blocked := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/content", "", "user-secret", "", nil)
	if blocked.Code != http.StatusConflict {
		t.Fatalf("quarantined content should be blocked: status=%d body=%s", blocked.Code, blocked.Body.String())
	}
}

func TestArtifactHTTPJSONUploadValidationAndLargeRange(t *testing.T) {
	handler := newArtifactHTTPHandler(t)
	encoded := base64.StdEncoding.EncodeToString([]byte("json payload"))
	body, err := json.Marshal(map[string]any{
		"user_id": "user-json", "name": "payload.json", "kind": string(artifact.KindGeneratedDoc), "mime_type": "application/json",
		"metadata": map[string]any{"preview": "precomputed"}, "data": encoded,
	})
	if err != nil {
		t.Fatal(err)
	}
	created := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts", "application/json", "", string(body), nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("json create status=%d body=%s", created.Code, created.Body.String())
	}
	var item artifact.Artifact
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	preview := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/preview?user_id=user-json", "", "", "", nil)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"preview":"precomputed"`) {
		t.Fatalf("precomputed preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	invalid := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts", "application/json", "user-json", `{"name":"x","data":"","unexpected":true}`, nil)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("unknown JSON field status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	badRange := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/content?user_id=user-json", "", "", "", map[string]string{"Range": "bytes=999-1000"})
	if badRange.Code != http.StatusRequestedRangeNotSatisfiable && badRange.Code != http.StatusBadRequest {
		t.Fatalf("invalid range status=%d body=%s", badRange.Code, badRange.Body.String())
	}
}

func TestArtifactHTTPRawUploadEmptyBodyAndMissingUser(t *testing.T) {
	handler := newArtifactHTTPHandler(t)
	missingUser := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts?name=x.txt", "text/plain", "", "x", nil)
	if missingUser.Code != http.StatusBadRequest && missingUser.Code != http.StatusForbidden {
		t.Fatalf("missing user status=%d body=%s", missingUser.Code, missingUser.Body.String())
	}
	empty := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts?name=empty.txt", "application/octet-stream", "user-empty", "", nil)
	if empty.Code != http.StatusCreated {
		t.Fatalf("empty upload status=%d body=%s", empty.Code, empty.Body.String())
	}
	var item artifact.Artifact
	if err := json.Unmarshal(empty.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	response := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+item.ID+"/content?user_id=user-empty", "", "", "", nil)
	if response.Code != http.StatusOK || response.Body.Len() != 0 || response.Header().Get("Content-Length") != "0" {
		t.Fatalf("empty content status=%d len=%d headers=%v", response.Code, response.Body.Len(), response.Header())
	}
}

func TestChatAttachmentRefsValidateOwnershipAndMaterializeInlineToArtifact(t *testing.T) {
	// Exercise the server helper directly so this test does not require a live
	// model/provider. Inline bytes are converted to an immutable ref and a
	// retry with the same idempotency key reuses the CAS object.
	api := newArtifactHTTPServer(t)
	inline := []agent.Attachment{{Name: "note.txt", MIMEType: "text/plain", Data: []byte("hello")}}
	first, err := api.materializeChatAttachments(context.Background(), "user-1", "conversation-1", "request-1:0", inline)
	if err != nil || len(first) != 1 || first[0].Ref == nil || len(first[0].Data) != 0 {
		t.Fatalf("inline attachment 未转换为 ref: %#v err=%v", first, err)
	}
	if first[0].Ref.Preview != "" {
		t.Fatalf("durable chat attachment ref 不应携带 preview: %#v", first[0].Ref)
	}
	second, err := api.materializeChatAttachments(context.Background(), "user-1", "conversation-1", "request-1:0", inline)
	if err != nil || second[0].Ref == nil || second[0].Ref.ID != first[0].Ref.ID {
		t.Fatalf("同一 producer 重试未幂等复用 Artifact: first=%#v second=%#v err=%v", first, second, err)
	}
	refPayload := []chatAttachmentPayload{{ArtifactID: first[0].Ref.ID, ArtifactVersion: first[0].Ref.Version, ArtifactDigest: first[0].Ref.Digest, ArtifactSize: first[0].Ref.Size, Name: first[0].Ref.Name}}
	refs, err := normalizeChatAttachments(refPayload)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := api.materializeChatAttachments(context.Background(), "user-1", "conversation-1", "request-ref", refs)
	if err != nil || canonical[0].Ref == nil || canonical[0].Ref.Digest != first[0].Ref.Digest || len(canonical[0].Data) != 0 {
		t.Fatalf("合法 ref 未通过 canonical 校验: %#v err=%v", canonical, err)
	}
	if canonical[0].Ref.Preview != "" {
		t.Fatalf("canonical durable chat attachment ref 不应携带 preview: %#v", canonical[0].Ref)
	}
	if _, err := api.materializeChatAttachments(context.Background(), "user-2", "conversation-1", "request-ref", refs); !errors.Is(err, artifact.ErrForbidden) && !strings.Contains(err.Error(), "不可访问") {
		t.Fatalf("跨用户 ref 应拒绝: %v", err)
	}
	if _, err := api.materializeChatAttachments(context.Background(), "user-1", "conversation-2", "request-ref", refs); !errors.Is(err, artifact.ErrForbidden) {
		t.Fatalf("跨对话 ref 应拒绝: %v", err)
	}
}
