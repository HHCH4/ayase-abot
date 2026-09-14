package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"Abot/internal/storage/sqlite"
	"Abot/internal/workspace"
)

func newWorkspaceRetryHandler(t *testing.T, workspaceID string) (http.Handler, *workspace.Service) {
	t.Helper()
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := workspace.NewService(store.WorkspaceRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), workspace.SaveRequest{
		Workspace: workspace.Workspace{ID: workspaceID, Name: workspaceID, Type: workspace.TypeLocal, RootPath: t.TempDir(), Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	return NewServer(nil, nil, service).Handler(), service
}

func TestWorkspaceOperationRetryEndpoint(t *testing.T) {
	ctx := context.Background()
	handler, service := newWorkspaceRetryHandler(t, "retry-http")
	operation, err := service.CreateOperation(ctx, workspace.OperationRequest{
		WorkspaceID: "retry-http", UserID: "user-1", Type: workspace.OperationCommand, Command: "printf retry-http", CWD: ".", Timeout: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RejectOperation(ctx, operation.ID); err != nil {
		t.Fatal(err)
	}

	// 被拒绝的操作是用户的明确决定，不能通过重试重新执行。
	rejected := callHTTP(handler, http.MethodPost, "/api/v1/workspace-operations/"+operation.ID+"/retry", "{}")
	if rejected.Code != http.StatusConflict {
		t.Fatalf("拒绝过的操作重试状态码 = %d，响应=%s", rejected.Code, rejected.Body.String())
	}

	// 失败与 unknown 才允许重试；这里用一条真实命令制造失败态。
	failing, err := service.CreateOperation(ctx, workspace.OperationRequest{
		WorkspaceID: "retry-http", UserID: "user-1", Type: workspace.OperationCommand, Command: "exit 3", CWD: ".", Timeout: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveOperation(ctx, failing.ID); err != nil {
		t.Fatal(err)
	}

	retried := callHTTP(handler, http.MethodPost, "/api/v1/workspace-operations/"+failing.ID+"/retry", "{}")
	if retried.Code != http.StatusCreated {
		t.Fatalf("重试状态码 = %d，响应=%s", retried.Code, retried.Body.String())
	}
	var created workspace.Operation
	if err := json.Unmarshal(retried.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == failing.ID || created.RetryOf != failing.ID {
		t.Fatalf("重试必须创建新操作并记录来源: %+v", created)
	}
	if created.Status != workspace.OperationPrepared {
		t.Fatalf("重试必须等待批准: %s", created.Status)
	}

	// 幂等：等待中的重试不会重复堆积。
	again := callHTTP(handler, http.MethodPost, "/api/v1/workspace-operations/"+failing.ID+"/retry", "{}")
	var second workspace.Operation
	if err := json.Unmarshal(again.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if again.Code != http.StatusCreated || second.ID != created.ID {
		t.Fatalf("等待中的重试必须复用: code=%d id=%s want=%s", again.Code, second.ID, created.ID)
	}

	// 重试创建的操作必须经过正常批准路径才能真正执行。
	approved := callHTTP(handler, http.MethodPost, "/api/v1/workspace-operations/"+created.ID+"/approve", "{}")
	if approved.Code != http.StatusOK {
		t.Fatalf("批准重试操作失败: %d %s", approved.Code, approved.Body.String())
	}
	var executed workspace.Operation
	if err := json.Unmarshal(approved.Body.Bytes(), &executed); err != nil {
		t.Fatal(err)
	}
	// 命令正常退出但退出码非零：操作是 completed，结果由 command_outcome 表达。
	if executed.Status != workspace.OperationCompleted || executed.CommandOutcome != "nonzero" {
		t.Fatalf("批准后应按原命令重新执行: %+v", executed)
	}

	missing := callHTTP(handler, http.MethodPost, "/api/v1/workspace-operations/operation-missing/retry", "{}")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在的操作状态码 = %d，响应=%s", missing.Code, missing.Body.String())
	}
}

func TestWorkspaceOperationRetryRequiresService(t *testing.T) {
	handler := NewServer(nil, nil).Handler()
	response := callHTTP(handler, http.MethodPost, "/api/v1/workspace-operations/whatever/retry", "{}")
	if response.Code == http.StatusOK || response.Code == http.StatusCreated {
		t.Fatalf("未装配工作区服务时不应成功: %d %s", response.Code, response.Body.String())
	}
	if strings.TrimSpace(response.Body.String()) == "" {
		t.Fatal("失败必须返回可读错误")
	}
}
