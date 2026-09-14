package hitlspike

import (
	"context"
	"strings"
	"testing"

	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// TestDuplicateApprovalReExecutesTool 回答 §23 第 8 问，并且实测结果与直觉相反：
//
//	同一个 confirmation response 重复提交时，ADK **会**再次进入原工具并执行副作用。
//
// 因此它同时覆盖 roadmap Phase 0 的“Tool result 已完成后如何无副作用恢复”：
// 这一保护不能依赖 ADK，必须由 Runtime 自己的执行权判定承担（§16.1），
// 对应的验证见 TestExecutionCASBlocksDuplicateSideEffect。
func TestDuplicateApprovalReExecutesTool(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("已修改 notes.txt。"),
		textResponse("重复的审批决定已被忽略。"),
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

	first := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if first.err != nil {
		t.Fatalf("首次批准失败: %v", first.err)
	}
	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 1 {
		t.Fatalf("首次批准后执行记录数量 = %d，期望 1", len(executes))
	}

	// 同一份确认响应再提交一次。
	duplicate := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if duplicate.err != nil {
		t.Fatalf("重复提交不应产生传输错误: %v", duplicate.err)
	}

	executes := item.book.entriesOfKind(kindExecute)
	if len(executes) != 2 {
		t.Fatalf("重复提交后执行记录数量 = %d；ADK v2.3.0 实测会重复执行，若此断言失败说明 ADK 行为已改变，"+
			"必须重新评估 Runtime 是否仍需自带执行权判定。账本=%+v", len(executes), item.book.readAll())
	}
	for _, entry := range executes {
		if entry.FunctionCallID != originalCall.ID {
			t.Fatalf("两次执行应绑定同一个原始 call ID %q，实际 %q", originalCall.ID, entry.FunctionCallID)
		}
	}
	t.Logf("重复提交导致工具执行 %d 次，原始 call ID = %s", len(executes), originalCall.ID)
	t.Logf("账本=%+v", item.book.readAll())
}

// TestExecutionCASBlocksDuplicateSideEffect 验证 §16.1 要求的执行权判定确实能挡住重复副作用：
// 工具在恢复执行前检查“该原始调用是否已经执行过”，第二次进入只返回 already_applied。
func TestExecutionCASBlocksDuplicateSideEffect(t *testing.T) {
	ctx := context.Background()
	item := newScenarioWith(t, func(book *ledger, abotID string) (tool.Tool, error) {
		return newGuardedPatchToolWith(book, abotID, true)
	},
		patchCall("notes.txt", "hello"),
		textResponse("已修改 notes.txt。"),
		textResponse("这次修改此前已经完成。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}

	first := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if first.err != nil {
		t.Fatalf("首次批准失败: %v", first.err)
	}
	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 1 {
		t.Fatalf("首次批准后执行记录数量 = %d，期望 1", len(executes))
	}
	duplicate := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if duplicate.err != nil {
		t.Fatalf("重复提交不应产生传输错误: %v", duplicate.err)
	}

	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 1 {
		t.Fatalf("启用执行权判定后，副作用执行次数 = %d，期望 1；账本=%+v", len(executes), item.book.readAll())
	}
	if blocked := item.book.entriesOfKind(kindDuplicateBlocked); len(blocked) != 1 {
		t.Fatalf("应记录一次被挡下的重复执行，实际 %d 条；账本=%+v", len(blocked), item.book.readAll())
	}
	if !strings.Contains(item.model.lastRequest(), `"status":"already_applied"`) {
		t.Fatalf("重复提交回合模型应看到 already_applied: %s", item.model.lastRequest())
	}
}

// TestTwoConfirmationsInOneTurn 回答 §23 第 9 问：一个响应含多个 confirmation 时的顺序与关联方式。
func TestTwoConfirmationsInOneTurn(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		twoPatchCalls(),
		textResponse("两个文件都已修改。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 a.txt 和 b.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	pairs := findAllConfirmations(pause.events)
	if len(pairs) != 2 {
		t.Fatalf("确认调用数量 = %d，期望 2；事件如下:\n%s", len(pairs), describeEvents(pause.events))
	}

	// 顺序：确认调用按模型响应中原始调用的出现顺序生成，且与原始调用一一对应。
	if got := pathOf(t, pairs[0].OriginalCall); got != "a.txt" {
		t.Fatalf("第一个确认对应的原始调用路径 = %q，期望 a.txt", got)
	}
	if got := pathOf(t, pairs[1].OriginalCall); got != "b.txt" {
		t.Fatalf("第二个确认对应的原始调用路径 = %q，期望 b.txt", got)
	}
	if pairs[0].Event != pairs[1].Event {
		t.Fatal("两个确认调用应位于同一个事件里")
	}
	if len(pairs[0].Event.LongRunningToolIDs) != 2 {
		t.Fatalf("暂停事件的 LongRunningToolIDs = %v，期望同时包含两个确认 call ID",
			pairs[0].Event.LongRunningToolIDs)
	}
	if pairs[0].ConfirmationCall.ID == pairs[1].ConfirmationCall.ID {
		t.Fatal("两个确认调用不能共用同一个 call ID")
	}

	// 两个决定放在同一个用户事件里，故意用与请求相反的顺序提交。
	resume := runTurn(ctx, item.runner, multiConfirmationResponse(
		confirmationDecision{CallID: pairs[1].ConfirmationCall.ID, Confirmed: true},
		confirmationDecision{CallID: pairs[0].ConfirmationCall.ID, Confirmed: true},
	))
	if resume.err != nil {
		t.Fatalf("多确认恢复失败: %v", resume.err)
	}

	executes := item.book.entriesOfKind(kindExecute)
	if len(executes) != 2 {
		t.Fatalf("执行记录数量 = %d，期望 2；账本=%+v", len(executes), item.book.readAll())
	}
	executedPaths := map[string]bool{}
	for _, entry := range executes {
		executedPaths[entry.Path] = true
	}
	if !executedPaths["a.txt"] || !executedPaths["b.txt"] {
		t.Fatalf("两次执行覆盖的路径 = %v，期望 a.txt 与 b.txt", executedPaths)
	}
	// ADK 并发执行同一响应里的多个工具调用，因此副作用完成顺序不保证与调用顺序一致；
	// 这里只固化“模型看到的结果顺序”，它由组装事件决定。
	responseIDs := allFunctionResponseIDs(resume.events, toolNamePatch)
	if len(responseIDs) != 2 || responseIDs[0] != pairs[0].OriginalCall.ID || responseIDs[1] != pairs[1].OriginalCall.ID {
		t.Fatalf("模型看到的工具结果顺序 = %v，期望 [%s %s]",
			responseIDs, pairs[0].OriginalCall.ID, pairs[1].OriginalCall.ID)
	}
	t.Logf("副作用完成顺序（ADK 并发执行，不保证稳定）= [%s %s]", executes[0].Path, executes[1].Path)
	if got, want := finalText(resume.events), "两个文件都已修改。"; got != want {
		t.Fatalf("最终回复 = %q，期望 %q", got, want)
	}
}

// TestPartialConfirmationKeepsRemainingPending 固化一个必须由 Runtime 接管的边界：
// 只回答多个确认中的一个时，ADK 不会为剩下的调用重新发出确认请求。
func TestPartialConfirmationKeepsRemainingPending(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		twoPatchCalls(),
		textResponse("先处理了 a.txt。"),
		textResponse("b.txt 也处理完了。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 a.txt 和 b.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	pairs := findAllConfirmations(pause.events)
	if len(pairs) != 2 {
		t.Fatalf("确认调用数量 = %d，期望 2", len(pairs))
	}

	// 只批准第一个。
	first := runTurn(ctx, item.runner, confirmationResponse(pairs[0].ConfirmationCall.ID, true, nil))
	if first.err != nil {
		t.Fatalf("部分批准失败: %v", first.err)
	}
	executes := item.book.entriesOfKind(kindExecute)
	if len(executes) != 1 || executes[0].Path != "a.txt" {
		t.Fatalf("部分批准后执行记录 = %+v，期望只有 a.txt", executes)
	}
	if _, _, again := findConfirmationCall(first.events); again {
		t.Fatalf("未答复的调用不应自动重新发起确认；事件如下:\n%s", describeEvents(first.events))
	}

	// 另一个确认仍然可以在后续回合里被答复。
	second := runTurn(ctx, item.runner, confirmationResponse(pairs[1].ConfirmationCall.ID, true, nil))
	if second.err != nil {
		t.Fatalf("延后批准失败: %v", second.err)
	}
	executes = item.book.entriesOfKind(kindExecute)
	if len(executes) != 2 {
		t.Fatalf("延后批准后执行记录数量 = %d，期望 2；账本=%+v", len(executes), item.book.readAll())
	}
	if got, want := finalText(second.events), "b.txt 也处理完了。"; got != want {
		t.Fatalf("延后批准回合回复 = %q，期望 %q", got, want)
	}
}

// TestConfirmationStillValidAfterUnrelatedTurn 验证中间穿插一次普通对话后，
// 未决确认仍然可以恢复（Runtime 必须持久化确认 call ID，而不是依赖“上一轮”）。
func TestConfirmationStillValidAfterUnrelatedTurn(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("好的，我在等你的决定。"),
		textResponse("已按你的批准完成修改。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}

	// 用户在审批前先发了一条普通消息。
	unrelated := runTurn(ctx, item.runner, userText("现在是什么状态？"))
	if unrelated.err != nil {
		t.Fatalf("无关回合失败: %v", unrelated.err)
	}
	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 0 {
		t.Fatalf("无关回合不应执行副作用，实际 %d 次", len(executes))
	}

	approved := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if approved.err != nil {
		t.Fatalf("穿插普通消息后恢复失败: %v", approved.err)
	}
	executes := item.book.entriesOfKind(kindExecute)
	if len(executes) != 1 {
		t.Fatalf("恢复后执行记录数量 = %d，期望 1；账本=%+v", len(executes), item.book.readAll())
	}
	if got, want := finalText(approved.events), "已按你的批准完成修改。"; got != want {
		t.Fatalf("恢复回合回复 = %q，期望 %q", got, want)
	}
	if !strings.Contains(approved.events[0].InvocationID, pause.events[0].InvocationID) &&
		approved.events[0].InvocationID != pause.events[0].InvocationID {
		t.Fatalf("恢复回合应与暂停回合共享 invocation ID: pause=%q resume=%q",
			pause.events[0].InvocationID, approved.events[0].InvocationID)
	}
}

func twoPatchCalls() *genai.Content {
	return &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: toolNamePatch, Args: map[string]any{"path": "a.txt", "content": "A"}}},
			{FunctionCall: &genai.FunctionCall{Name: toolNamePatch, Args: map[string]any{"path": "b.txt", "content": "B"}}},
		},
	}
}

func pathOf(t *testing.T, call *genai.FunctionCall) string {
	t.Helper()
	if call == nil || call.Args == nil {
		t.Fatalf("原始调用为空: %+v", call)
	}
	path, _ := call.Args["path"].(string)
	return path
}
