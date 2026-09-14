package hitlspike

import (
	"context"
	"slices"
	"strings"
	"testing"

	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

// TestLateRejectionClosesPendingConfirmation 回答 §23 第 10 问：
// 取消或过期时如何安全关闭未决 confirmation。可行手段是在后续回合提交 confirmed=false，
// 让 ADK 完成一次结构化的拒绝恢复，而不是把未决调用永久留在会话里。
func TestLateRejectionClosesPendingConfirmation(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("好的，我先不动这个文件。"),
		textResponse("已经放弃这次修改。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}

	// 暂停之后先经过一段无关对话（模拟“用户没有立刻决定”）。
	if unrelated := runTurn(ctx, item.runner, userText("先说说别的")); unrelated.err != nil {
		t.Fatalf("无关回合失败: %v", unrelated.err)
	}
	if open := openConfirmationCallIDs(loadSession(t, ctx, item.service)); !slices.Contains(open, confirmationCall.ID) {
		t.Fatalf("未决确认应仍然存在，实际 open=%v", open)
	}

	// 过期/取消时提交一次拒绝，把未决调用收口。
	closed := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, false, nil))
	if closed.err != nil {
		t.Fatalf("关闭未决确认失败: %v", closed.err)
	}
	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 0 {
		t.Fatalf("关闭未决确认不应执行副作用，实际 %d 次", len(executes))
	}
	if open := openConfirmationCallIDs(loadSession(t, ctx, item.service)); slices.Contains(open, confirmationCall.ID) {
		t.Fatalf("拒绝之后未决确认应被关闭，实际 open=%v", open)
	}
	if got, want := finalText(closed.events), "已经放弃这次修改。"; got != want {
		t.Fatalf("关闭回合回复 = %q，期望 %q", got, want)
	}
}

// TestAbandonedConfirmationDoesNotBlockLaterTurns 固化另一半事实：
// ADK 不会自动过期未决确认，也不会阻止后续普通对话；Runtime 必须自己决定何时关闭它。
func TestAbandonedConfirmationDoesNotBlockLaterTurns(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("第一轮普通回复。"),
		textResponse("第二轮普通回复。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}

	first := runTurn(ctx, item.runner, userText("继续聊别的"))
	if first.err != nil {
		t.Fatalf("第一轮后续对话失败: %v", first.err)
	}
	second := runTurn(ctx, item.runner, userText("再聊一轮"))
	if second.err != nil {
		t.Fatalf("第二轮后续对话失败: %v", second.err)
	}

	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 0 {
		t.Fatalf("未决确认不应产生副作用，实际 %d 次", len(executes))
	}
	if _, _, again := findConfirmationCall(first.events); again {
		t.Fatal("后续对话不应重新发起确认")
	}
	if _, _, again := findConfirmationCall(second.events); again {
		t.Fatal("后续对话不应重新发起确认")
	}
	// 未决确认仍然存在：ADK 没有超时机制，需要 Runtime 用 Approval.expires_at 自己收口。
	open := openConfirmationCallIDs(loadSession(t, ctx, item.service))
	if !slices.Contains(open, confirmationCall.ID) {
		t.Fatalf("未决确认应一直存在直到被显式关闭，实际 open=%v", open)
	}
	if got, want := finalText(second.events), "第二轮普通回复。"; got != want {
		t.Fatalf("第二轮回复 = %q，期望 %q", got, want)
	}
	if got := item.model.callCount(); got != 3 {
		t.Fatalf("模型调用次数 = %d，期望 3（暂停 1 + 两次普通对话）", got)
	}
	t.Logf("未决 confirmation call ID 在无关对话后仍存在 = %v", open)
}

// openConfirmationCallIDs 是 ADK runner/run_node.go openLongRunningCallIDs 的测试侧镜像：
// 已发出但还没有 FunctionResponse 的长任务调用 ID。Runtime 可以用同样的判定恢复“等待审批”状态。
func openConfirmationCallIDs(stored session.Session) []string {
	if stored == nil {
		return nil
	}
	open := map[string]struct{}{}
	answered := map[string]struct{}{}
	for index := 0; index < stored.Events().Len(); index++ {
		event := stored.Events().At(index)
		if event == nil {
			continue
		}
		for _, id := range event.LongRunningToolIDs {
			open[id] = struct{}{}
		}
		if event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part != nil && part.FunctionResponse != nil && part.FunctionResponse.ID != "" {
				answered[part.FunctionResponse.ID] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(open))
	for id := range open {
		if _, ok := answered[id]; ok {
			continue
		}
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}

// TestUnknownConfirmationIDIsIgnored 验证恢复输入用错 ID 时的行为：
// ADK 不会报错，但也不会恢复原调用，而是把这轮当成普通对话。
func TestUnknownConfirmationIDIsIgnored(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("我没看出这是针对哪个操作的确认。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}

	// 用原始业务工具 call ID（而不是确认 call ID）去恢复：这是最容易犯的错。
	original, err := toolconfirmation.OriginalCallFrom(confirmationCall)
	if err != nil {
		t.Fatalf("取出原始调用失败: %v", err)
	}
	wrong := runTurn(ctx, item.runner, confirmationResponse(original.ID, true, nil))
	if wrong.err != nil {
		t.Fatalf("错误 ID 的恢复不应产生传输错误: %v", wrong.err)
	}
	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 0 {
		t.Fatalf("错误 ID 不应触发执行，实际 %d 次", len(executes))
	}
	if open := openConfirmationCallIDs(loadSession(t, ctx, item.service)); !slices.Contains(open, confirmationCall.ID) {
		t.Fatalf("错误 ID 不应关闭未决确认，实际 open=%v", open)
	}
	t.Logf("错误 ID 恢复后的模型请求（应看不到工具结果）：%s", truncate(item.model.lastRequest(), 200))
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.TrimSpace(text[:limit]) + "..."
}
