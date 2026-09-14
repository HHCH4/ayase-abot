package hitlspike

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// TestRestartResumesFromSQLite 回答 §23 第 7 问：ADK Session 在当前持久化后端（SQLite）上，
// 换掉 Session Service 与 Runner 之后是否还能完整恢复暂停点。
func TestRestartResumesFromSQLite(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t, patchCall("notes.txt", "hello"))

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}
	originalCall, err := toolconfirmation.OriginalCallFrom(confirmationCall)
	if err != nil {
		t.Fatalf("取出原始调用失败: %v", err)
	}

	// 丢弃 Runner 与 Session Service，只用同一个 SQLite 文件重建；恢复回合不传 state delta。
	item.restart(t, textResponse("重启后仍然完成了修改。"))
	resume := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if resume.err != nil {
		t.Fatalf("重启后恢复失败: %v", resume.err)
	}

	executes := item.book.entriesOfKind(kindExecute)
	if len(executes) != 1 {
		t.Fatalf("重启后执行记录数量 = %d，期望 1；账本=%+v", len(executes), item.book.readAll())
	}
	if executes[0].FunctionCallID != originalCall.ID {
		t.Fatalf("执行记录的 call ID = %q，期望原始 call ID %q", executes[0].FunctionCallID, originalCall.ID)
	}
	if got, want := finalText(resume.events), "重启后仍然完成了修改。"; got != want {
		t.Fatalf("最终回复 = %q，期望 %q", got, want)
	}

	// §8.5：恢复回合没有再次注入 state delta，工具仍能从持久化 session state 读到 Abot Invocation ID。
	if executes[0].StateAbotInvocationID != item.abotInvocationID {
		t.Fatalf("重启后 session state 中的 Abot Invocation ID = %q，期望 %q",
			executes[0].StateAbotInvocationID, item.abotInvocationID)
	}
	if executes[0].BoundAbotInvocationID != item.abotInvocationID {
		t.Fatalf("闭包绑定的 Abot Invocation ID = %q，期望 %q",
			executes[0].BoundAbotInvocationID, item.abotInvocationID)
	}
}

// ============================================================
// 真子进程重启
// ============================================================

const (
	envHelperPhase  = "ABOT_HITL_SPIKE_PHASE"
	envHelperDB     = "ABOT_HITL_SPIKE_DB"
	envHelperLedger = "ABOT_HITL_SPIKE_LEDGER"
	envHelperCallID = "ABOT_HITL_SPIKE_CONFIRMATION_CALL_ID"
	envHelperOut    = "ABOT_HITL_SPIKE_OUT"
)

// helperReport 是子进程回传给父测试的观测结果。
type helperReport struct {
	Phase                 string   `json:"phase"`
	ConfirmationCallID    string   `json:"confirmation_call_id,omitempty"`
	OriginalCallID        string   `json:"original_call_id,omitempty"`
	InvocationIDs         []string `json:"invocation_ids,omitempty"`
	FinalText             string   `json:"final_text,omitempty"`
	ModelCalls            int      `json:"model_calls"`
	LastModelRequest      string   `json:"last_model_request,omitempty"`
	StateAbotInvocationID string   `json:"state_abot_invocation_id,omitempty"`
	TurnError             string   `json:"turn_error,omitempty"`
	EventDump             string   `json:"event_dump,omitempty"`
}

// TestHelperProcess 不是普通测试：它作为独立进程内的一个阶段运行，
// 用来提供「真正跨进程重启」的证据，而不是同进程重建对象。
func TestHelperProcess(t *testing.T) {
	phase := os.Getenv(envHelperPhase)
	if phase == "" {
		t.Skip("仅作为子进程助手运行")
	}

	ctx := context.Background()
	dbPath := os.Getenv(envHelperDB)
	book := newLedger(os.Getenv(envHelperLedger))
	service := openSessionService(t, dbPath)
	if err := ensureSession(ctx, service, sessionID); err != nil {
		t.Fatalf("子进程创建会话失败: %v", err)
	}

	report := helperReport{Phase: phase}
	var responses []*genai.Content
	switch phase {
	case "pause":
		responses = []*genai.Content{patchCall("notes.txt", "hello")}
	case "resume":
		responses = []*genai.Content{textResponse("子进程重启后完成了修改。")}
	default:
		t.Fatalf("未知阶段: %s", phase)
	}

	model := newScriptedModel("scripted-flash", responses...)
	guarded, err := newGuardedPatchTool(book, "abot-inv-1")
	if err != nil {
		t.Fatalf("子进程创建工具失败: %v", err)
	}
	runner := newRunner(t, service, model, []tool.Tool{guarded})

	var turn turnResult
	switch phase {
	case "pause":
		turn = runTurn(ctx, runner, userText("请修改 notes.txt"),
			adkrunner.WithStateDelta(map[string]any{stateAbotInvocationID: "abot-inv-1"}),
			adkrunner.WithYieldUserMessage())
	case "resume":
		callID := os.Getenv(envHelperCallID)
		if callID == "" {
			t.Fatal("恢复阶段缺少 confirmation call ID")
		}
		// 注意：恢复阶段刻意不注入 state delta，用来验证状态是否真的落库。
		turn = runTurn(ctx, runner, confirmationResponse(callID, true, nil))
	}

	report.ModelCalls = model.callCount()
	report.LastModelRequest = model.lastRequest()
	report.FinalText = finalText(turn.events)
	report.InvocationIDs = eventInvocationIDs(turn.events)
	report.EventDump = describeEvents(turn.events)
	if turn.err != nil {
		report.TurnError = turn.err.Error()
	}
	if call, _, ok := findConfirmationCall(turn.events); ok {
		report.ConfirmationCallID = call.ID
		if original, err := toolconfirmation.OriginalCallFrom(call); err == nil {
			report.OriginalCallID = original.ID
		}
	}
	if stored, err := service.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID}); err == nil {
		if value, stateErr := stored.Session.State().Get(stateAbotInvocationID); stateErr == nil {
			report.StateAbotInvocationID, _ = value.(string)
		}
	}

	writeHelperReport(t, os.Getenv(envHelperOut), report)
}

func writeHelperReport(t *testing.T, path string, report helperReport) {
	t.Helper()
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("序列化子进程报告失败: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("写入子进程报告失败: %v", err)
	}
}

// TestSubprocessRestartRoundTrip 是本包最强的证据：暂停发生在一个进程里，
// 进程退出后由另一个进程读取同一个 SQLite 会话库完成批准与恢复。
func TestSubprocessRestartRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	ledgerPath := filepath.Join(dir, "ledger.jsonl")

	pauseReport := runHelper(t, "pause", dbPath, ledgerPath, "", filepath.Join(dir, "pause.json"))
	if pauseReport.TurnError != "" {
		t.Fatalf("暂停阶段失败: %s\n事件:\n%s", pauseReport.TurnError, pauseReport.EventDump)
	}
	if pauseReport.ConfirmationCallID == "" || pauseReport.OriginalCallID == "" {
		t.Fatalf("暂停阶段没有拿到两个 call ID: %+v", pauseReport)
	}
	if pauseReport.ModelCalls != 1 {
		t.Fatalf("暂停阶段模型调用次数 = %d，期望 1", pauseReport.ModelCalls)
	}
	if pauseReport.StateAbotInvocationID != "abot-inv-1" {
		t.Fatalf("暂停阶段 session state 未落库: %q", pauseReport.StateAbotInvocationID)
	}

	// 父测试直接验证磁盘上的中间状态：暂停已经持久化，且没有任何副作用记录。
	book := newLedger(ledgerPath)
	if prepares := book.entriesOfKind(kindPrepare); len(prepares) != 1 {
		t.Fatalf("暂停阶段准备记录数量 = %d，期望 1", len(prepares))
	}
	if executes := book.entriesOfKind(kindExecute); len(executes) != 0 {
		t.Fatalf("暂停阶段不应有执行记录，实际 %d 条", len(executes))
	}
	verifyPersistedPauseState(t, dbPath, pauseReport)

	resumeReport := runHelper(t, "resume", dbPath, ledgerPath, pauseReport.ConfirmationCallID, filepath.Join(dir, "resume.json"))
	if resumeReport.TurnError != "" {
		t.Fatalf("恢复阶段失败: %s\n事件:\n%s", resumeReport.TurnError, resumeReport.EventDump)
	}
	if got, want := resumeReport.FinalText, "子进程重启后完成了修改。"; got != want {
		t.Fatalf("恢复阶段最终回复 = %q，期望 %q", got, want)
	}
	if !strings.Contains(resumeReport.LastModelRequest, `"status":"applied"`) {
		t.Fatalf("恢复阶段模型没有看到真实工具结果: %s", resumeReport.LastModelRequest)
	}

	executes := book.entriesOfKind(kindExecute)
	if len(executes) != 1 {
		t.Fatalf("跨进程恢复后执行记录数量 = %d，期望 1；账本=%+v", len(executes), book.readAll())
	}
	if executes[0].FunctionCallID != pauseReport.OriginalCallID {
		t.Fatalf("执行记录 call ID = %q，期望原始 call ID %q", executes[0].FunctionCallID, pauseReport.OriginalCallID)
	}
	if executes[0].StateAbotInvocationID != "abot-inv-1" {
		t.Fatalf("跨进程恢复后 session state 中的 Abot Invocation ID = %q，期望 abot-inv-1", executes[0].StateAbotInvocationID)
	}

	// 第 6 问的跨进程版本：暂停进程与恢复进程看到的 ADK invocation ID 是否一致。
	if len(pauseReport.InvocationIDs) != 1 || len(resumeReport.InvocationIDs) != 1 {
		t.Fatalf("两个阶段的 invocation ID 数量异常: pause=%v resume=%v",
			pauseReport.InvocationIDs, resumeReport.InvocationIDs)
	}
	if pauseReport.InvocationIDs[0] != resumeReport.InvocationIDs[0] {
		t.Fatalf("跨进程恢复改写了 ADK invocation ID: pause=%q resume=%q",
			pauseReport.InvocationIDs[0], resumeReport.InvocationIDs[0])
	}
	if executes[0].AdkInvocationID != pauseReport.InvocationIDs[0] {
		t.Fatalf("恢复进程内 ctx.InvocationID() = %q，期望沿用 %q",
			executes[0].AdkInvocationID, pauseReport.InvocationIDs[0])
	}

	t.Logf("暂停进程确认 call ID = %s", pauseReport.ConfirmationCallID)
	t.Logf("两个进程共享的 ADK invocation ID = %s", pauseReport.InvocationIDs[0])
}

func verifyPersistedPauseState(t *testing.T, dbPath string, report helperReport) {
	t.Helper()
	ctx := context.Background()
	service := openSessionService(t, dbPath)
	stored := loadSession(t, ctx, service)

	found := false
	for index := 0; index < stored.Events().Len(); index++ {
		event := stored.Events().At(index)
		if event == nil {
			continue
		}
		for _, id := range event.LongRunningToolIDs {
			if id == report.ConfirmationCallID {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("SQLite 中没有持久化确认 call ID %q 的暂停标记", report.ConfirmationCallID)
	}
	value, err := stored.State().Get(stateAbotInvocationID)
	if err != nil {
		t.Fatalf("读取持久化 session state 失败: %v", err)
	}
	if text, _ := value.(string); text != "abot-inv-1" {
		t.Fatalf("持久化 session state 中的 Abot Invocation ID = %q，期望 abot-inv-1", text)
	}
}

// runHelper 在本进程之外启动同一个测试二进制，只在其中执行指定阶段。
func runHelper(t *testing.T, phase, dbPath, ledgerPath, callID, outPath string) helperReport {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.timeout=90s")
	command.Env = append(os.Environ(),
		envHelperPhase+"="+phase,
		envHelperDB+"="+dbPath,
		envHelperLedger+"="+ledgerPath,
		envHelperCallID+"="+callID,
		envHelperOut+"="+outPath,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程阶段 %s 失败: %v\n子进程输出:\n%s", phase, err, string(output))
	}
	payload, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("读取子进程阶段 %s 的报告失败: %v\n子进程输出:\n%s", phase, err, string(output))
	}
	var report helperReport
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatalf("解析子进程报告失败: %v", err)
	}
	return report
}
