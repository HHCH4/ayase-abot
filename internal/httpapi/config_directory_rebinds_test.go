package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/provider"
	"Abot/internal/storage/sqlite"
	"google.golang.org/adk/v2/session"
)

func httpRebindSnapshot(t *testing.T) (string, string) {
	t.Helper()
	instructionDigest := sha256.Sum256([]byte("http-rebind-instruction"))
	snapshot := agent.RuntimeConfigSnapshot{
		Version:                agent.RuntimeConfigSnapshotVersion,
		AgentDefinitionVersion: "kernel-runtime-v2",
		AppName:                "http-rebind",
		ProviderID:             "demo",
		ModelID:                "model",
		ProviderProtocol:       string(provider.ProtocolOpenAICompatible),
		PersonaID:              "persona:default",
		InstructionDigest:      "sha256:" + hex.EncodeToString(instructionDigest[:]),
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest := agent.RuntimeConfigSnapshotDigest(string(encoded))
	if digest == "" {
		t.Fatal("HTTP rebind 测试快照必须可解析")
	}
	return string(encoded), digest
}

func TestRuntimeConfigDirectoryRebindAPIIsMetadataOnlyAndExplicit(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := provider.NewRegistry(ctx, store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot, digest := httpRebindSnapshot(t)
	invocation := agentruntime.Invocation{ID: "inv-http-rebind", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingUser, ConfigSnapshot: snapshot, Message: "private request body", CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	catalog := agentruntime.NewMemoryRuntimeConfigDirectoryRebindRouteCatalog()
	if err := catalog.Set("runtime-source", agentruntime.RuntimeConfigDirectoryRebindCapability{Destination: "runtime-a", Revision: 4, SupportsRebind: true, SupportsCheckpoint: true, SupportsResume: true, SupportsConfigMaterialization: true}); err != nil {
		t.Fatal(err)
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	payload, err := json.Marshal(map[string]any{
		"source":                          "runtime-source",
		"invocation_id":                   invocation.ID,
		"destinations":                    []string{"runtime-a"},
		"expected_config_snapshot_digest": digest,
		"idempotency_key":                 "body-key-ignored-by-header",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans", strings.NewReader(string(payload)))
	request.Header.Set("Idempotency-Key", "http-rebind-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("创建 rebind plan 应返回 202: status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "private request body") || strings.Contains(response.Body.String(), "http-rebind-key") || strings.Contains(response.Body.String(), "body-key-ignored") {
		t.Fatalf("rebind API 响应不得泄露正文或幂等键: %s", response.Body.String())
	}
	var plan agentruntime.RuntimeConfigDirectoryRebindPlan
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil || plan.ID == "" || plan.ConfigSnapshotDigest != digest {
		t.Fatalf("创建响应 plan 错误: %#v err=%v", plan, err)
	}
	list := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/rebind-plans?source=runtime-source&invocation_id="+invocation.ID+"&limit=10", "")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "http-rebind-key") {
		t.Fatalf("rebind list 错误或泄露幂等键: status=%d body=%s", list.Code, list.Body.String())
	}
	var listed []agentruntime.RuntimeConfigDirectoryRebindPlan
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].ID != plan.ID {
		t.Fatalf("rebind list 内容错误: %#v err=%v", listed, err)
	}
	detail := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), plan.ID) {
		t.Fatalf("rebind detail 错误: status=%d body=%s", detail.Code, detail.Body.String())
	}
	invalidStatus := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/rebind-plans?status=invalid", "")
	if invalidStatus.Code != http.StatusBadRequest {
		t.Fatalf("非法 rebind status 应返回 400: status=%d body=%s", invalidStatus.Code, invalidStatus.Body.String())
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(nil)
	missingCatalog := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans", strings.NewReader(string(payload)))
	missingCatalog.Header.Set("Idempotency-Key", "missing-catalog")
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingCatalog)
	if missingResponse.Code != http.StatusNotImplemented {
		t.Fatalf("未装配 route catalog 应返回 501: status=%d body=%s", missingResponse.Code, missingResponse.Body.String())
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	stalePayload := strings.Replace(string(payload), digest, strings.Repeat("sha256:0", 1)+strings.Repeat("0", 64-1), 1)
	staleRequest := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans", strings.NewReader(stalePayload))
	staleRequest.Header.Set("Idempotency-Key", "stale-key")
	staleResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleResponse, staleRequest)
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("过期 config snapshot 应返回 409: status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}
}

func TestRuntimeConfigDirectoryRebindApplyAPIRequiresConfirmationAndListsDurableApply(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := provider.NewRegistry(ctx, store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshotText, digest := httpRebindSnapshot(t)
	invocation := agentruntime.Invocation{ID: "inv-http-rebind-apply", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingUser, ConfigSnapshot: snapshotText, ConfigSnapshotDigest: digest, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "http-rebind-apply-event", InvocationID: invocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.(agentruntime.RuntimeSnapshotRepository).SaveRuntimeSnapshot(ctx, agentruntime.RuntimeSnapshot{InvocationID: invocation.ID, Revision: 1, Phase: string(agentruntime.InvocationWaitingUser), WorkflowPhase: agentruntime.WorkflowPhaseWaitingUser, Workflow: agentruntime.WorkflowCheckpoint{Status: agentruntime.WorkflowCheckpointWaiting}, GeneratedAt: now}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	catalog := agentruntime.NewMemoryRuntimeConfigDirectoryRebindRouteCatalog()
	if err := catalog.Set("runtime-source", agentruntime.RuntimeConfigDirectoryRebindCapability{Destination: "runtime-target", Revision: 1, SupportsRebind: true, SupportsCheckpoint: true, SupportsResume: true, SupportsConfigMaterialization: true}); err != nil {
		t.Fatal(err)
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	statusSecret := []byte("http-rebind-status-secret-012345678901234567")
	statusInbox, ok := repo.(agentruntime.RuntimeConfigDirectoryRebindInbox)
	if !ok {
		t.Fatal("SQLite runtime repository 必须提供 rebind inbox")
	}
	statusReceiver, err := agentruntime.NewRuntimeConfigDirectoryRebindApplyReceiver("runtime-source", "runtime-target", statusSecret, statusInbox)
	if err != nil {
		t.Fatal(err)
	}
	server.SetRuntimeConfigDirectoryRebindApplyReceiver(statusReceiver)
	handler := server.Handler()
	planBody := `{"source":"runtime-source","invocation_id":"` + invocation.ID + `","destinations":["runtime-target"],"expected_config_snapshot_digest":"` + digest + `"}`
	planResponse := callHTTP(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans", planBody)
	if planResponse.Code != http.StatusAccepted {
		t.Fatalf("创建 apply plan 失败: status=%d body=%s", planResponse.Code, planResponse.Body.String())
	}
	var plan agentruntime.RuntimeConfigDirectoryRebindPlan
	if err := json.Unmarshal(planResponse.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	confirmationResponse := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/confirm", `{}`, map[string]string{"Idempotency-Key": "http-confirm"})
	if confirmationResponse.Code != http.StatusAccepted {
		t.Fatalf("创建 confirmation 失败: status=%d body=%s", confirmationResponse.Code, confirmationResponse.Body.String())
	}
	var confirmation agentruntime.RuntimeConfigDirectoryRebindConfirmation
	if err := json.Unmarshal(confirmationResponse.Body.Bytes(), &confirmation); err != nil || confirmation.ID == "" {
		t.Fatalf("confirmation 响应错误: %#v err=%v", confirmation, err)
	}
	applyResponse := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/apply", `{"confirmation_id":"`+confirmation.ID+`"}`, map[string]string{"Idempotency-Key": "http-apply"})
	if applyResponse.Code != http.StatusAccepted || strings.Contains(applyResponse.Body.String(), "http-apply") {
		t.Fatalf("apply 应返回 queued 且不泄露幂等键: status=%d body=%s", applyResponse.Code, applyResponse.Body.String())
	}
	var apply agentruntime.RuntimeConfigDirectoryRebindApply
	if err := json.Unmarshal(applyResponse.Body.Bytes(), &apply); err != nil || apply.Status != agentruntime.RuntimeConfigDirectoryRebindApplyQueued {
		t.Fatalf("apply 响应错误: %#v err=%v", apply, err)
	}
	apply, err = coordinator.GetRuntimeConfigDirectoryRebindApply(ctx, apply.ID)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := apply.Envelope(now)
	if err != nil {
		t.Fatal(err)
	}
	signedEnvelope, err := agentruntime.SignRuntimeConfigDirectoryRebindApplyEnvelope(envelope, statusSecret)
	if err != nil {
		t.Fatal(err)
	}
	statusBody, err := json.Marshal(signedEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	statusHeaders := map[string]string{
		"Content-Type":                  "application/json",
		"X-Abot-Rebind-Apply-Version":   "1",
		"X-Abot-Rebind-Apply-Signature": signedEnvelope.Signature,
		"Idempotency-Key":               signedEnvelope.ApplyID,
	}
	statusResponse := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-apply/status", string(statusBody), statusHeaders)
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"phase":"absent"`) {
		t.Fatalf("rebind apply status 路由应返回 absent proof: status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	if _, err := repo.(agentruntime.RuntimeConfigDirectoryRebindInbox).AcceptRuntimeConfigDirectoryRebind(ctx, signedEnvelope, apply.Projection); err != nil {
		t.Fatal(err)
	}
	statusResponse = callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-apply/status", string(statusBody), statusHeaders)
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"phase":"accepted"`) {
		t.Fatalf("rebind apply status 路由应返回 accepted proof: status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	list := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/applies?status=queued", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), apply.ID) || strings.Contains(list.Body.String(), "http-apply") {
		t.Fatalf("apply list 错误或泄露幂等键: status=%d body=%s", list.Code, list.Body.String())
	}
	missingKey := callHTTP(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/confirm", `{}`)
	if missingKey.Code != http.StatusBadRequest {
		t.Fatalf("缺少 confirmation 幂等键应返回 400: status=%d body=%s", missingKey.Code, missingKey.Body.String())
	}
	unknown := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/apply", `{"confirmation_id":"`+confirmation.ID+`","unknown":true}`, map[string]string{"Idempotency-Key": "http-apply-unknown"})
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("未知 apply 字段应返回 400: status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	trailingConfirm := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/confirm", `{} {}`, map[string]string{"Idempotency-Key": "http-confirm-trailing"})
	if trailingConfirm.Code != http.StatusBadRequest {
		t.Fatalf("confirm 尾部 JSON 应返回 400: status=%d body=%s", trailingConfirm.Code, trailingConfirm.Body.String())
	}
	trailingApply := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/apply", `{"confirmation_id":"`+confirmation.ID+`"} {}`, map[string]string{"Idempotency-Key": "http-apply-trailing"})
	if trailingApply.Code != http.StatusBadRequest {
		t.Fatalf("apply 尾部 JSON 应返回 400: status=%d body=%s", trailingApply.Code, trailingApply.Body.String())
	}
}

func callHTTPWithHeadersForRebindTest(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestRuntimeConfigDirectoryRebindMultiApplyAPIIsStrictAndMetadataOnly(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := provider.NewRegistry(ctx, store.ProviderRepository(), map[provider.Protocol]provider.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := agent.NewKernel(agent.Config{SessionService: session.InMemoryService(), Providers: registry})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.RuntimeRepository()
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshotText, digest := httpRebindSnapshot(t)
	invocation := agentruntime.Invocation{ID: "inv-http-rebind-multi-apply", UserID: "user", ConversationID: "conversation", SessionID: "session", Status: agentruntime.InvocationWaitingUser, ConfigSnapshot: snapshotText, ConfigSnapshotDigest: digest, Message: "private multi request", CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, agentruntime.AgentEvent{ID: "http-rebind-multi-event", InvocationID: invocation.ID, Type: agentruntime.EventInvocationWaiting, Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.(agentruntime.RuntimeSnapshotRepository).SaveRuntimeSnapshot(ctx, agentruntime.RuntimeSnapshot{InvocationID: invocation.ID, Revision: 1, Phase: string(agentruntime.InvocationWaitingUser), WorkflowPhase: agentruntime.WorkflowPhaseWaitingUser, Workflow: agentruntime.WorkflowCheckpoint{Status: agentruntime.WorkflowCheckpointWaiting}, GeneratedAt: now}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agentruntime.NewCoordinator(kernel, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	catalog := agentruntime.NewMemoryRuntimeConfigDirectoryRebindRouteCatalog()
	for _, destination := range []string{"runtime-target-a", "runtime-target-b"} {
		if err := catalog.Set("runtime-source", agentruntime.RuntimeConfigDirectoryRebindCapability{Destination: destination, Revision: 2, SupportsRebind: true, SupportsCheckpoint: true, SupportsResume: true, SupportsConfigMaterialization: true}); err != nil {
			t.Fatal(err)
		}
	}
	coordinator.SetRuntimeConfigDirectoryRebindRouteCatalog(catalog)
	server := NewServer(nil, nil)
	server.SetRuntimeCoordinator(coordinator)
	handler := server.Handler()
	planBody := `{"source":"runtime-source","invocation_id":"` + invocation.ID + `","destinations":["runtime-target-b","runtime-target-a"],"expected_config_snapshot_digest":"` + digest + `"}`
	planResponse := callHTTP(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans", planBody)
	if planResponse.Code != http.StatusAccepted {
		t.Fatalf("创建 multi plan 失败: status=%d body=%s", planResponse.Code, planResponse.Body.String())
	}
	var plan agentruntime.RuntimeConfigDirectoryRebindPlan
	if err := json.Unmarshal(planResponse.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	confirmationResponse := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/multi-confirm", `{}`, map[string]string{"Idempotency-Key": "http-multi-confirm"})
	if confirmationResponse.Code != http.StatusAccepted || strings.Contains(confirmationResponse.Body.String(), "http-multi-confirm") {
		t.Fatalf("multi confirmation 响应错误或泄露 key: status=%d body=%s", confirmationResponse.Code, confirmationResponse.Body.String())
	}
	var confirmation agentruntime.RuntimeConfigDirectoryRebindMultiConfirmation
	if err := json.Unmarshal(confirmationResponse.Body.Bytes(), &confirmation); err != nil || len(confirmation.Destinations) != 2 {
		t.Fatalf("multi confirmation 解析错误: %#v err=%v", confirmation, err)
	}
	applyResponse := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/multi-apply", `{"confirmation_id":"`+confirmation.ID+`"}`, map[string]string{"Idempotency-Key": "http-multi-apply"})
	if applyResponse.Code != http.StatusAccepted || strings.Contains(applyResponse.Body.String(), "http-multi-apply") || strings.Contains(applyResponse.Body.String(), "private multi request") {
		t.Fatalf("multi apply 响应错误或泄露敏感值: status=%d body=%s", applyResponse.Code, applyResponse.Body.String())
	}
	var parent agentruntime.RuntimeConfigDirectoryRebindMultiApply
	if err := json.Unmarshal(applyResponse.Body.Bytes(), &parent); err != nil || parent.Status != agentruntime.RuntimeConfigDirectoryRebindMultiApplyQueued || len(parent.ChildApplyIDs) != 2 {
		t.Fatalf("multi apply 响应错误: %#v err=%v", parent, err)
	}
	list := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/multi-applies?status=queued", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), parent.ID) || strings.Contains(list.Body.String(), "private multi request") {
		t.Fatalf("multi parent list 错误或泄露正文: status=%d body=%s", list.Code, list.Body.String())
	}
	detail := callHTTP(handler, http.MethodGet, "/api/v1/runtime/config-directory/rebind-multi-applies/"+parent.ID, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), parent.ID) {
		t.Fatalf("multi parent detail 错误: status=%d body=%s", detail.Code, detail.Body.String())
	}
	unknown := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/multi-apply", `{"confirmation_id":"`+confirmation.ID+`","unknown":true}`, map[string]string{"Idempotency-Key": "http-multi-unknown"})
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("multi apply 未知字段应返回 400: status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	trailing := callHTTPWithHeadersForRebindTest(handler, http.MethodPost, "/api/v1/runtime/config-directory/rebind-plans/"+plan.ID+"/multi-confirm", `{} {}`, map[string]string{"Idempotency-Key": "http-multi-trailing"})
	if trailing.Code != http.StatusBadRequest {
		t.Fatalf("multi confirm 尾部 JSON 应返回 400: status=%d body=%s", trailing.Code, trailing.Body.String())
	}
}
