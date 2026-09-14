package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"Abot/internal/artifact"
)

func TestArtifactHTTPUsageAndMaintenanceEndpoints(t *testing.T) {
	server := newArtifactHTTPServer(t)
	handler := server.Handler()
	created := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts?name=hello.txt&kind=input_attachment", "text/plain", "user-1", "hello world", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	usageResponse := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/usage", "", "", "", nil)
	if usageResponse.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", usageResponse.Code, usageResponse.Body.String())
	}
	var usage artifact.Usage
	if err := json.Unmarshal(usageResponse.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.Bytes != int64(len("hello world")) || usage.Objects != 1 || usage.Artifacts != 1 {
		t.Fatalf("usage mismatch: %+v", usage)
	}
	if usage.QuotaBytes != artifact.DefaultQuotaBytes || usage.QuotaExceeded {
		t.Fatalf("unexpected quota reporting: %+v", usage)
	}

	maintenanceResponse := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts/maintenance", "", "", "", nil)
	if maintenanceResponse.Code != http.StatusOK {
		t.Fatalf("maintenance status=%d body=%s", maintenanceResponse.Code, maintenanceResponse.Body.String())
	}
	var result artifact.MaintenanceResult
	if err := json.Unmarshal(maintenanceResponse.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.UploadSweep.UploadsFailed != 0 || result.GarbageCollection.Deleted != 0 {
		t.Fatalf("maintenance must be a no-op on a healthy store: %+v", result)
	}
}

func TestArtifactHTTPQuotaRejectionIsInsufficientStorage(t *testing.T) {
	server := newArtifactHTTPServer(t)
	handler := server.Handler()
	if err := server.artifacts.SetMaintenancePolicy(artifact.MaintenancePolicy{
		QuotaBytes: 4, StaleUploadAge: artifact.DefaultStaleUploadAge, ObjectGracePeriod: artifact.DefaultObjectGracePeriod,
	}); err != nil {
		t.Fatal(err)
	}
	response := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts?name=big.txt&kind=input_attachment", "text/plain", "user-1", "0123456789", nil)
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("quota rejection status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "配额") {
		t.Fatalf("quota rejection must explain the cause: %s", response.Body.String())
	}
	// 被拒绝的上传不能出现在列表里。
	listResponse := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts", "", "user-1", "", nil)
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), "big.txt") {
		t.Fatalf("rejected upload must not be listed: %s", listResponse.Body.String())
	}
}

func TestArtifactHTTPMaintenanceSweepsAbandonedUpload(t *testing.T) {
	ctx := context.Background()
	store, err := artifact.NewLocalContentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := artifact.NewMemoryRepository()
	service, err := artifact.NewService(repository, store)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil)
	server.SetArtifactService(service)
	handler := server.Handler()

	// 直接构造一条卡住的 uploading 记录，模拟进程在提交对象前退出。
	now := time.Now().UTC()
	stale := artifact.Artifact{
		ID: "artifact-http-stale", Version: 1, UserID: "user-1", Kind: artifact.KindInputAttachment,
		Name: "stale.txt", MIMEType: "text/plain", Status: artifact.StatusUploading,
		CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-72 * time.Hour),
	}
	if err := repository.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}

	response := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts/maintenance", "", "", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("maintenance status=%d body=%s", response.Code, response.Body.String())
	}
	var result artifact.MaintenanceResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.UploadSweep.UploadsFailed != 1 {
		t.Fatalf("maintenance must close the abandoned upload: %+v", result)
	}
	closed, err := repository.Get(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != artifact.StatusFailed {
		t.Fatalf("abandoned upload must end as failed: %+v", closed)
	}
}

func TestArtifactHTTPMaintenanceWithoutService(t *testing.T) {
	server := NewServer(nil, nil)
	handler := server.Handler()
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/artifacts/usage"},
		{http.MethodPost, "/api/v1/artifacts/maintenance"},
	} {
		response := serveArtifactRequest(handler, request.method, request.path, "", "", "", nil)
		if response.Code == http.StatusOK {
			t.Fatalf("%s %s must fail without an artifact service", request.method, request.path)
		}
	}
}

func TestArtifactHTTPExtractAndPreviewProjection(t *testing.T) {
	server := newArtifactHTTPServer(t)
	handler := server.Handler()
	created := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts?name=notes.txt&kind=input_attachment", "text/plain", "user-1", "bounded extraction", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var uploaded artifact.Artifact
	if err := json.Unmarshal(created.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}

	extracted := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts/"+uploaded.ID+"/extract", "", "user-1", "", nil)
	if extracted.Code != http.StatusOK {
		t.Fatalf("extract status=%d body=%s", extracted.Code, extracted.Body.String())
	}
	var extraction artifact.Extraction
	if err := json.Unmarshal(extracted.Body.Bytes(), &extraction); err != nil {
		t.Fatal(err)
	}
	if extraction.Kind != artifact.ExtractionText || extraction.Preview != "bounded extraction" {
		t.Fatalf("unexpected extraction: %+v", extraction)
	}
	if extraction.SourceDigest != uploaded.Digest || extraction.Version != artifact.ExtractionVersion {
		t.Fatalf("extraction must be bound to the object identity: %+v", extraction)
	}

	// preview 端点必须优先投影已持久化的有界结果。
	preview := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/"+uploaded.ID+"/preview", "", "user-1", "", nil)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var payload struct {
		Preview    string               `json:"preview"`
		Extraction *artifact.Extraction `json:"extraction"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Extraction == nil || payload.Extraction.Kind != artifact.ExtractionText || payload.Preview != "bounded extraction" {
		t.Fatalf("preview 必须反映已存储的提取结果: %s", preview.Body.String())
	}

	// 其他用户不能借用提取端点读取内容。
	forbidden := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts/"+uploaded.ID+"/extract", "", "user-2", "", nil)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("跨用户提取状态码 = %d，响应=%s", forbidden.Code, forbidden.Body.String())
	}
}

func TestArtifactUsageUsesSnakeCaseFields(t *testing.T) {
	server := newArtifactHTTPServer(t)
	handler := server.Handler()
	created := serveArtifactRequest(handler, http.MethodPost, "/api/v1/artifacts?name=x.txt&kind=input_attachment", "text/plain", "user-1", "abc", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d", created.Code)
	}
	response := serveArtifactRequest(handler, http.MethodGet, "/api/v1/artifacts/usage", "", "", "", nil)
	var raw map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	// 其它字段都是 snake_case，usage 不能例外，否则前端要为一个响应特判。
	for _, key := range []string{"bytes", "objects", "artifacts", "quota_bytes", "quota_exceeded"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("usage 缺少 snake_case 字段 %q: %s", key, response.Body.String())
		}
	}
	for _, key := range []string{"Bytes", "Objects", "Artifacts"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("usage 不应暴露 Go 字段名 %q: %s", key, response.Body.String())
		}
	}
}
