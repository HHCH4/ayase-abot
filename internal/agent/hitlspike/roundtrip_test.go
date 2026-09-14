package hitlspike

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// scenario 是一次完整的 spike 场景：同一个 SQLite 会话库、同一份账本、同一个脚本化模型。
type scenario struct {
	dir              string
	dbPath           string
	ledgerPath       string
	book             *ledger
	service          session.Service
	model            *scriptedModel
	runner           *adkrunner.Runner
	abotInvocationID string
	factory          func(*ledger, string) (tool.Tool, error)
}

func newScenario(t *testing.T, responses ...*genai.Content) *scenario {
	t.Helper()
	return newScenarioWith(t, newGuardedPatchTool, responses...)
}

// newStaticScenario 使用 ADK 静态 RequireConfirmation 路径，用于对比框架自动确认与
// handler 内显式确认的差异（AGENT_APPROVAL_RESUME.md §8.1）。
func newStaticScenario(t *testing.T, responses ...*genai.Content) *scenario {
	t.Helper()
	return newScenarioWith(t, func(book *ledger, _ string) (tool.Tool, error) {
		return newStaticConfirmationTool(book)
	}, responses...)
}

func newScenarioWith(t *testing.T, factory func(*ledger, string) (tool.Tool, error), responses ...*genai.Content) *scenario {
	t.Helper()
	dir := t.TempDir()
	item := &scenario{
		dir:              dir,
		dbPath:           filepath.Join(dir, "sessions.db"),
		ledgerPath:       filepath.Join(dir, "ledger.jsonl"),
		abotInvocationID: "abot-inv-1",
		factory:          factory,
	}
	item.book = newLedger(item.ledgerPath)
	item.service = openSessionService(t, item.dbPath)
	if err := ensureSession(context.Background(), item.service, sessionID); err != nil {
		t.Fatalf("创建 spike 会话失败: %v", err)
	}
	item.model = newScriptedModel("scripted-flash", responses...)
	guarded, err := factory(item.book, item.abotInvocationID)
	if err != nil {
		t.Fatalf("创建 spike 工具失败: %v", err)
	}
	item.runner = newRunner(t, item.service, item.model, []tool.Tool{guarded})
	return item
}

// restart 模拟“进程重启”：丢弃当前 Runner 与 Session Service，只用同一个 SQLite 文件重建。
func (s *scenario) restart(t *testing.T, responses ...*genai.Content) {
	t.Helper()
	s.service = openSessionService(t, s.dbPath)
	s.model = newScriptedModel("scripted-flash", responses...)
	guarded, err := s.factory(s.book, s.abotInvocationID)
	if err != nil {
		t.Fatalf("重建 spike 工具失败: %v", err)
	}
	s.runner = newRunner(t, s.service, s.model, []tool.Tool{guarded})
}

func (s *scenario) stateDelta() adkrunner.RunOption {
	return adkrunner.WithStateDelta(map[string]any{stateAbotInvocationID: s.abotInvocationID})
}

// TestPauseTurnEventShapeAndTwoCallIDs 回答 AGENT_APPROVAL_RESUME.md §23 的第 1、2 问：
// RequestConfirmation 产生的完整事件形状，以及两个 call ID 的准确位置。
func TestPauseTurnEventShapeAndTwoCallIDs(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t, patchCall("notes.txt", "hello"), textResponse("done"))

	turn := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if turn.err != nil {
		t.Fatalf("暂停回合不应产生错误: %v", turn.err)
	}

	confirmationCall, confirmationEvent, ok := findConfirmationCall(turn.events)
	if !ok {
		t.Fatalf("暂停回合没有产生 adk_request_confirmation 事件；事件如下:\n%s", describeEvents(turn.events))
	}
	if confirmationCall.Name != toolconfirmation.FunctionCallName {
		t.Fatalf("确认调用名 = %q，期望 %q", confirmationCall.Name, toolconfirmation.FunctionCallName)
	}

	originalCall, err := toolconfirmation.OriginalCallFrom(confirmationCall)
	if err != nil {
		t.Fatalf("从确认事件里取出原始调用失败: %v", err)
	}
	if originalCall.Name != toolNamePatch {
		t.Fatalf("原始调用名 = %q，期望 %q", originalCall.Name, toolNamePatch)
	}
	// 第 2 问的核心结论：两个 ID 必须不同，且各自出现在固定位置。
	if originalCall.ID == "" || confirmationCall.ID == "" {
		t.Fatalf("两个 call ID 都不应缺失: original=%q confirmation=%q", originalCall.ID, confirmationCall.ID)
	}
	if originalCall.ID == confirmationCall.ID {
		t.Fatalf("原始 call ID 与确认 call ID 相同（%q），设计假设不成立", originalCall.ID)
	}
	if len(confirmationEvent.LongRunningToolIDs) != 1 || confirmationEvent.LongRunningToolIDs[0] != confirmationCall.ID {
		t.Fatalf("暂停标记应只包含确认 call ID: LongRunningToolIDs=%v confirmation=%q",
			confirmationEvent.LongRunningToolIDs, confirmationCall.ID)
	}

	// 暂停回合还必须已经持久化了工具首次执行的 FunctionResponse，否则客户端看不到准备结果。
	response, ok := findFunctionResponse(turn.events, toolNamePatch)
	if !ok {
		t.Fatalf("暂停回合缺少工具 FunctionResponse；事件如下:\n%s", describeEvents(turn.events))
	}
	if response.ID != originalCall.ID {
		t.Fatalf("FunctionResponse ID = %q，期望绑定原始 call ID %q", response.ID, originalCall.ID)
	}
	if got, _ := response.Response["status"].(string); got != "waiting_approval" {
		t.Fatalf("首次进入 handler 的返回状态 = %q，期望 waiting_approval", got)
	}

	// 模型在本回合只被调用一次：确认请求不会在同一回合里继续驱动模型。
	if item.model.callCount() != 1 {
		t.Fatalf("暂停回合模型调用次数 = %d，期望 1", item.model.callCount())
	}

	prepares := item.book.entriesOfKind(kindPrepare)
	if len(prepares) != 1 {
		t.Fatalf("准备记录数量 = %d，期望 1", len(prepares))
	}
	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 0 {
		t.Fatalf("暂停阶段不应执行副作用，实际执行 %d 次", len(executes))
	}

	// §8.5 关心的两种 Invocation ID 注入来源，在这里如实记录。
	t.Logf("原始 call ID = %s", originalCall.ID)
	t.Logf("确认 call ID = %s", confirmationCall.ID)
	t.Logf("handler 内 ctx.InvocationID() = %s", prepares[0].AdkInvocationID)
	t.Logf("闭包绑定 Abot Invocation ID = %q", prepares[0].BoundAbotInvocationID)
	t.Logf("session state 中的 Abot Invocation ID = %q", prepares[0].StateAbotInvocationID)
}

// TestApproveResumesOriginalToolAndFinishes 回答 §23 的第 4、6 问：
// confirmed=true 再次 runner.Run 是否进入原 handler，以及恢复前后 ADK invocation ID 的变化。
func TestApproveResumesOriginalToolAndFinishes(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("已修改 notes.txt 并完成验证。"),
	)

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

	resume := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if resume.err != nil {
		t.Fatalf("恢复回合失败: %v", resume.err)
	}

	// 原 handler 必须被再次进入，并且这次真的执行。
	executes := item.book.entriesOfKind(kindExecute)
	if len(executes) != 1 {
		t.Fatalf("执行记录数量 = %d，期望 1；账本=%+v", len(executes), item.book.readAll())
	}
	if executes[0].FunctionCallID != originalCall.ID {
		t.Fatalf("执行记录的 call ID = %q，期望原始 call ID %q（说明恢复确实回到原调用）",
			executes[0].FunctionCallID, originalCall.ID)
	}
	if got, _ := item.book.findPrepare(item.abotInvocationID, originalCall.ID); got.Digest != executes[0].Digest {
		t.Fatalf("恢复执行的 digest 与准备阶段不一致: prepare=%q execute=%q", got.Digest, executes[0].Digest)
	}

	// 模型必须看到真实工具结果并据此作答。
	if item.model.callCount() != 2 {
		t.Fatalf("模型调用次数 = %d，期望 2（暂停回合 1 次 + 恢复回合 1 次）", item.model.callCount())
	}
	lastRequest := item.model.lastRequest()
	if !strings.Contains(lastRequest, `"status":"applied"`) {
		t.Fatalf("恢复回合的模型请求里没有真实工具结果: %s", lastRequest)
	}
	if got, want := finalText(resume.events), "已修改 notes.txt 并完成验证。"; got != want {
		t.Fatalf("最终回复 = %q，期望 %q", got, want)
	}

	// 第 6 问：恢复前后的 ADK invocation ID。
	pauseIDs := eventInvocationIDs(pause.events)
	resumeIDs := eventInvocationIDs(resume.events)
	t.Logf("暂停回合 ADK invocation ID = %v", pauseIDs)
	t.Logf("恢复回合 ADK invocation ID = %v", resumeIDs)
	if len(pauseIDs) != 1 || len(resumeIDs) != 1 {
		t.Fatalf("两个回合各自应只有一个 invocation ID: pause=%v resume=%v", pauseIDs, resumeIDs)
	}
	if pauseIDs[0] != resumeIDs[0] {
		t.Fatalf("ADK 在恢复时复用了新的 invocation ID: pause=%q resume=%q", pauseIDs[0], resumeIDs[0])
	}
}
