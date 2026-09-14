package contextspike

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// TestBeforeModelCallbackSeesCompactedRequest 回答 AGENT_CONTEXT_ENGINE.md §30 第 1 问：
// BeforeModel callback 收到的 request 是否已经完成 compaction materialization。
func TestBeforeModelCallbackSeesCompactedRequest(t *testing.T) {
	item := newScenario(t,
		textResponse("回复一"), textResponse("回复二"),
		textResponse("回复三"), textResponse("回复四"),
	)

	for _, message := range []string{"第一条", "第二条", "第三条", "第四条"} {
		result := item.run(userText(message))
		if result.err != nil {
			t.Fatalf("第 %q 轮失败: %v", message, result.err)
		}
	}

	if item.summarizer.callCount() == 0 {
		t.Fatal("compaction 从未触发，测试前提不成立（阈值或保留条数不合适）")
	}
	observations := item.probe.snapshot()
	if len(observations) != 4 {
		t.Fatalf("callback 观测次数 = %d，期望 4（每次模型调用一次）", len(observations))
	}
	for index, observation := range observations {
		t.Logf("第 %d 次模型调用：contents=%d chars=%d 含摘要=%v 工具=%d",
			index+1, observation.ContentCount, observation.TotalChars, observation.ContainsSummary, observation.ToolCount)
	}

	last := observations[len(observations)-1]
	if !last.ContainsSummary {
		t.Fatal("最后一次 callback 没有看到压缩摘要，说明 compaction 发生在 callback 之后")
	}
	stored := item.stored()
	rawEvents := stored.Events().Len()
	t.Logf("压缩后 callback 看到的 contents = %d，Session 事件总数 = %d", last.ContentCount, rawEvents)
	if last.ContentCount >= rawEvents {
		t.Fatalf("callback 看到的 contents(%d) 不少于 Session 事件数(%d)，未体现 compaction 已生效",
			last.ContentCount, rawEvents)
	}
}

// TestCallbackMutationLeavesSessionEventsUntouched 回答 §30 第 2 问：
// callback 修改 request 后，Session 原始事件保持不变。
func TestCallbackMutationLeavesSessionEventsUntouched(t *testing.T) {
	echoTool, err := functiontool.New(functiontool.Config{
		Name:        "spike_echo",
		Description: "回显输入的文本。",
	}, func(_ adkagent.Context, args echoArgs) (echoResult, error) {
		return echoResult{Text: args.Text}, nil
	})
	if err != nil {
		t.Fatalf("创建工具失败: %v", err)
	}

	item := newScenarioWithTools(t, []tool.Tool{echoTool},
		textResponse("第一轮回复"), textResponse("第二轮回复"))

	first := item.run(userText("第一轮"))
	if first.err != nil {
		t.Fatalf("第一轮失败: %v", first.err)
	}
	before := eventTexts(item.stored())
	beforeCount := item.stored().Events().Len()

	// 第二轮让 callback 清空工具声明（复刻现有 tool budget 的做法）。
	item.probe.stripToolsFromCall = 2
	second := item.run(userText("第二轮"))
	if second.err != nil {
		t.Fatalf("第二轮失败: %v", second.err)
	}

	after := eventTexts(item.stored())
	afterCount := item.stored().Events().Len()
	if afterCount != beforeCount+2 {
		t.Fatalf("Session 事件数 = %d，期望 %d（user + model 各一条）", afterCount, beforeCount+2)
	}
	// 会话本来就会增长，因此只比对新增之前的既有前缀是否被 callback 改动过。
	if len(after) < len(before) {
		t.Fatalf("Session 文本事件数减少: before=%d after=%d", len(before), len(after))
	}
	for index := range before {
		if before[index] != after[index] {
			t.Fatalf("第 %d 条历史文本被 callback 修改: %q -> %q", index, before[index], after[index])
		}
	}

	// callback 的修改会影响模型请求，但不会影响持久化历史。
	declarations := item.model.toolDeclarations()
	t.Logf("每次模型调用收到的工具声明数 = %v", declarations)
	if len(declarations) != 2 {
		t.Fatalf("模型调用次数 = %d，期望 2", len(declarations))
	}
	if declarations[0] != 1 {
		t.Fatalf("第一次模型调用应收到 1 个工具声明，实际 %d", declarations[0])
	}
	if declarations[1] != 0 {
		t.Fatalf("callback 清空工具声明后，模型仍收到 %d 个工具声明", declarations[1])
	}
	t.Logf("callback 观测 = %+v", item.probe.snapshot())
}

// TestCallbackRunsOnEveryModelCallIncludingToolLoops 回答 §30 第 4 问：
// 工具循环的每次模型调用都会再次触发 callback。
func TestCallbackRunsOnEveryModelCallIncludingToolLoops(t *testing.T) {
	echoTool, err := functiontool.New(functiontool.Config{
		Name:        "spike_echo",
		Description: "回显输入的文本。",
	}, func(_ adkagent.Context, args echoArgs) (echoResult, error) {
		return echoResult{Text: args.Text}, nil
	})
	if err != nil {
		t.Fatalf("创建工具失败: %v", err)
	}

	item := newScenarioWithTools(t, []tool.Tool{echoTool},
		genai.NewContentFromFunctionCall("spike_echo", map[string]any{"text": "hello"}, genai.RoleModel),
		textResponse("工具已执行。"),
	)

	result := item.run(userText("请调用工具"))
	if result.err != nil {
		t.Fatalf("执行失败: %v", result.err)
	}
	if item.model.callCount() != 2 {
		t.Fatalf("模型调用次数 = %d，期望 2（工具循环各一次）", item.model.callCount())
	}
	observations := item.probe.snapshot()
	if len(observations) != 2 {
		t.Fatalf("callback 观测次数 = %d，期望 2", len(observations))
	}
	// 第二次 callback 能看到上一轮的工具调用，因此 FunctionCalls 不为 0。
	if observations[1].FunctionCalls == 0 {
		t.Fatalf("第二次 callback 未看到工具调用历史: %+v", observations[1])
	}
	t.Logf("两次 callback 观测 = %+v", observations)
}

// TestCompactionEventIsObservable 回答 §30 第 6 问：compaction 事件能否稳定映射为 AgentEvent。
func TestCompactionEventIsObservable(t *testing.T) {
	item := newScenario(t,
		textResponse("回复一"), textResponse("回复二"), textResponse("回复三"),
	)
	for _, message := range []string{"第一条", "第二条", "第三条"} {
		if result := item.run(userText(message)); result.err != nil {
			t.Fatalf("第 %q 轮失败: %v", message, result.err)
		}
	}

	stored := item.stored()
	summaries := compactionEvents(stored)
	if len(summaries) == 0 {
		t.Fatal("没有找到携带 EventCompaction 的事件")
	}
	summary := summaries[len(summaries)-1]
	if text := summarizeText(summary); !strings.Contains(text, summaryMarker) {
		t.Fatalf("压缩事件内容 = %q，期望包含摘要标记", text)
	}
	// 原始事件必须保留：压缩是提示词投影，不是删除历史。
	texts := eventTexts(stored)
	for _, expected := range []string{"第一条", "回复一"} {
		found := false
		for _, text := range texts {
			if strings.Contains(text, expected) {
				found = true
			}
		}
		if !found {
			t.Fatalf("压缩后原始事件 %q 丢失，说明 compaction 删除了历史", expected)
		}
	}

	raw, err := json.Marshal(summary.Actions.Compaction)
	if err != nil {
		t.Fatalf("序列化压缩事件失败: %v", err)
	}
	if len(raw) > 900 {
		raw = append(raw[:900], []byte("...")...)
	}
	t.Logf("压缩事件 Actions.Compaction = %s", raw)
	t.Logf("压缩事件 UsageMetadata = %+v", summary.UsageMetadata)
	t.Logf("摘要器调用次数 = %d，覆盖事件数 = %v", item.summarizer.callCount(), item.summarizer.covered)
}

// ============================================================
// HITL × compaction 交叉验证
// ============================================================

type echoArgs struct {
	Text string `json:"text" jsonschema:"需要回显的文本"`
}

type echoResult struct {
	Text string `json:"text"`
}

type confirmArgs struct {
	Path string `json:"path" jsonschema:"目标相对路径"`
}

type confirmResult struct {
	Status string `json:"status"`
}

type confirmRecorder struct {
	mu       sync.Mutex
	prepares int
	executes int
}

func (r *confirmRecorder) record(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch kind {
	case "prepare":
		r.prepares++
	case "execute":
		r.executes++
	}
}

func (r *confirmRecorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepares, r.executes
}

func newConfirmTool(recorder *confirmRecorder) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "spike_patch",
		Description: "写入工作区文件前需要用户批准。",
	}, func(ctx adkagent.Context, args confirmArgs) (confirmResult, error) {
		confirmation := ctx.ToolConfirmation()
		if confirmation == nil {
			recorder.record("prepare")
			if err := ctx.RequestConfirmation("请审批对 "+args.Path+" 的写入", map[string]any{"digest": "phase0"}); err != nil {
				return confirmResult{}, err
			}
			return confirmResult{Status: "waiting_approval"}, nil
		}
		recorder.record("execute")
		return confirmResult{Status: "applied"}, nil
	})
}

// TestConfirmationResumeSurvivesCompaction 是本阶段两个切片的交叉验证：
// compaction 生效的前提下，等待审批的工具仍能在后续回合被批准并执行。
func TestConfirmationResumeSurvivesCompaction(t *testing.T) {
	recorder := &confirmRecorder{}
	confirmTool, err := newConfirmTool(recorder)
	if err != nil {
		t.Fatalf("创建确认工具失败: %v", err)
	}

	item := newScenarioWithTools(t, []tool.Tool{confirmTool},
		genai.NewContentFromFunctionCall("spike_patch", map[string]any{"path": "notes.txt"}, genai.RoleModel),
		textResponse("我先不动这个文件。"),
		textResponse("好的，继续聊别的。"),
		textResponse("已按批准完成写入。"),
	)

	pause := item.run(userText("请修改 notes.txt"))
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	if prepares, executes := recorder.counts(); prepares != 1 || executes != 0 {
		t.Fatalf("暂停阶段计数不正确: prepares=%d executes=%d", prepares, executes)
	}

	// 制造足够多的轮次，让 compaction 在等待审批期间真实发生。
	if result := item.run(userText("先聊点别的")); result.err != nil {
		t.Fatalf("无关回合失败: %v", result.err)
	}
	if result := item.run(userText("再聊一句")); result.err != nil {
		t.Fatalf("第二个无关回合失败: %v", result.err)
	}
	if item.summarizer.callCount() == 0 {
		t.Fatal("等待审批期间 compaction 从未触发，交叉验证前提不成立")
	}

	approval := confirmationResponse(t, pause.events)
	approved := item.run(approval)
	if approved.err != nil {
		t.Fatalf("批准恢复失败: %v", approved.err)
	}
	if prepares, executes := recorder.counts(); prepares != 1 || executes != 1 {
		t.Fatalf("批准后计数不正确: prepares=%d executes=%d", prepares, executes)
	}
	if got := lastText(approved.events); !strings.Contains(got, "已按批准完成写入") {
		t.Fatalf("恢复回合最终文本 = %q", got)
	}
	t.Logf("compaction 期间恢复成功：摘要器调用 %d 次，压缩事件 %d 个",
		item.summarizer.callCount(), len(compactionEvents(item.stored())))
}
