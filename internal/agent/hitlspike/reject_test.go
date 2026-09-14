package hitlspike

import (
	"context"
	"strings"
	"testing"

	adkrunner "google.golang.org/adk/v2/runner"
)

// TestRejectShortCircuitsFunctionToolHandler 固化一个实测结论，它比
// AGENT_APPROVAL_RESUME.md §8.3 的表述更强：
//
//	confirmed=false 时，functiontool 包装的工具不会把 handler 重新带入，
//	无论确认是「handler 内显式 RequestConfirmation」还是「静态 RequireConfirmation」触发的。
//
// functiontool 在调用 handler 之前就检查 confirmation.Confirmed 并返回
// tool.ErrConfirmationRejected（ADK tool/functiontool/function.go:202-205）。
// 因此 Runtime 不能指望业务 handler 自己把 ToolCall/Approval 置为 rejected，
// 必须在消费恢复事件时同步维护状态；§8.3 最后一句正是这个意思，但适用范围是全路径。
func TestRejectShortCircuitsFunctionToolHandler(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("已按你的拒绝取消修改。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}

	resume := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, false, nil))
	if resume.err != nil {
		t.Fatalf("拒绝恢复回合不应产生传输错误: %v", resume.err)
	}

	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 0 {
		t.Fatalf("拒绝后不应执行副作用，实际执行 %d 次", len(executes))
	}
	if rejects := item.book.entriesOfKind(kindReject); len(rejects) != 0 {
		t.Fatalf("拒绝时 handler 不应被重新进入，但记录了 %d 条拒绝条目", len(rejects))
	}
	if entries := item.book.readAll(); len(entries) != 1 || entries[0].Kind != kindPrepare {
		t.Fatalf("拒绝后账本应只剩准备记录，实际=%+v", entries)
	}

	rejected, ok := findFunctionResponse(resume.events, toolNamePatch)
	if !ok {
		t.Fatalf("拒绝回合缺少工具 FunctionResponse；事件如下:\n%s", describeEvents(resume.events))
	}
	rejectedText, _ := rejected.Response["error"].(string)
	if rejectedText == "" {
		t.Fatalf("拒绝回合的 FunctionResponse 应包含错误说明，实际: %v", rejected.Response)
	}
	t.Logf("显式确认路径拒绝后模型看到的错误 = %s", rejectedText)

	// 模型仍然拿到结构化拒绝结果并继续作答，而不是被普通文本打断。
	if !strings.Contains(item.model.lastRequest(), "rejected") {
		t.Fatalf("恢复回合模型请求里没有拒绝信息: %s", item.model.lastRequest())
	}
	if got, want := finalText(resume.events), "已按你的拒绝取消修改。"; got != want {
		t.Fatalf("最终回复 = %q，期望 %q", got, want)
	}
}
