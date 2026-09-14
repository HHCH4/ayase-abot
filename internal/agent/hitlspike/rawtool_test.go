package hitlspike

import (
	"context"
	"fmt"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// rawGuardedTool 直接实现 ADK 的 tool.Tool + Declaration/Run，不经过 functiontool 包装。
// 目的是验证一个对设计有直接影响的差异：拒绝是否会传到业务代码。
type rawGuardedTool struct {
	book                  *ledger
	boundAbotInvocationID string
}

func (t *rawGuardedTool) Name() string        { return toolNamePatch }
func (t *rawGuardedTool) Description() string { return "自实现 tool.Tool 的 guarded 工具。" }
func (t *rawGuardedTool) IsLongRunning() bool { return false }

// ProcessRequest 是 ADK 把工具声明放进 LLM 请求的入口；自实现工具必须提供，
// 否则 Runner 会直接报 "does not implement RequestProcessor() method"。
func (t *rawGuardedTool) ProcessRequest(_ adkagent.Context, req *adkmodel.LLMRequest) error {
	return toolutils.PackTool(req, t)
}

func (t *rawGuardedTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        toolNamePatch,
		Description: "对工作区文件执行一次精确补丁；写入前必须获得用户批准。",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path":    {Type: genai.TypeString, Description: "工作区根目录下的相对路径"},
				"content": {Type: genai.TypeString, Description: "要写入文件的完整文本内容"},
			},
			Required: []string{"path", "content"},
		},
	}
}

func (t *rawGuardedTool) Run(ctx adkagent.Context, args any) (map[string]any, error) {
	mapping, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("参数类型不正确: %T", args)
	}
	path, _ := mapping["path"].(string)
	content, _ := mapping["content"].(string)
	digest := digestOf(path, content)

	entry := ledgerEntry{
		BoundAbotInvocationID: t.boundAbotInvocationID,
		StateAbotInvocationID: readStateString(ctx, stateAbotInvocationID),
		AdkInvocationID:       ctx.InvocationID(),
		FunctionCallID:        ctx.FunctionCallID(),
		Path:                  path,
		Digest:                digest,
	}

	confirmation := ctx.ToolConfirmation()
	if confirmation == nil {
		entry.Kind = kindPrepare
		if err := t.book.append(entry); err != nil {
			return nil, err
		}
		if err := ctx.RequestConfirmation("请审批对 "+path+" 的写入", map[string]any{"digest": digest}); err != nil {
			return nil, err
		}
		return map[string]any{"status": "waiting_approval", "digest": digest}, nil
	}

	confirmed := confirmation.Confirmed
	entry.Confirmed = &confirmed
	if !confirmed {
		entry.Kind = kindReject
		if err := t.book.append(entry); err != nil {
			return nil, err
		}
		return map[string]any{"status": "rejected", "digest": digest}, nil
	}

	prepared, ok := t.book.findPrepare(t.boundAbotInvocationID, ctx.FunctionCallID())
	if !ok || prepared.Digest != digest {
		return nil, fmt.Errorf("prepare 记录缺失或 digest 不一致")
	}
	entry.Kind = kindExecute
	if err := t.book.append(entry); err != nil {
		return nil, err
	}
	return map[string]any{"status": "applied", "digest": digest}, nil
}

func newRawToolScenario(t *testing.T, responses ...*genai.Content) *scenario {
	t.Helper()
	return newScenarioWith(t, func(book *ledger, abotID string) (tool.Tool, error) {
		return &rawGuardedTool{book: book, boundAbotInvocationID: abotID}, nil
	}, responses...)
}

// TestRawToolObservesRejection 验证：如果 Abot 自己实现 tool.Tool 而不是用 functiontool 包装，
// 拒绝就能进入业务代码，Runtime 因此可以在 handler 内直接维护 ToolCall/Approval 的 rejected 状态。
// 这是选择 Guarded Tool 实现方式时必须知道的差异。
func TestRawToolObservesRejection(t *testing.T) {
	ctx := context.Background()
	item := newRawToolScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("已取消该修改。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("自实现工具同样应产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}

	resume := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, false, nil))
	if resume.err != nil {
		t.Fatalf("拒绝恢复回合不应产生传输错误: %v", resume.err)
	}

	rejects := item.book.entriesOfKind(kindReject)
	if len(rejects) != 1 {
		t.Fatalf("自实现工具应在拒绝时进入 handler，拒绝记录=%d；账本=%+v", len(rejects), item.book.readAll())
	}
	if rejects[0].Confirmed == nil || *rejects[0].Confirmed {
		t.Fatalf("拒绝记录的 confirmed 值不正确: %+v", rejects[0].Confirmed)
	}
	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 0 {
		t.Fatalf("拒绝后不应执行副作用，实际执行 %d 次", len(executes))
	}
	if got, want := finalText(resume.events), "已取消该修改。"; got != want {
		t.Fatalf("最终回复 = %q，期望 %q", got, want)
	}
}

// TestRawToolApproveExecutesOnce 确认自实现工具在批准路径上同样只执行一次。
func TestRawToolApproveExecutesOnce(t *testing.T) {
	ctx := context.Background()
	item := newRawToolScenario(t,
		patchCall("notes.txt", "hello"),
		textResponse("已修改 notes.txt。"),
	)

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("自实现工具应产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}

	resume := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if resume.err != nil {
		t.Fatalf("批准恢复回合失败: %v", resume.err)
	}
	if executes := item.book.entriesOfKind(kindExecute); len(executes) != 1 {
		t.Fatalf("执行记录数量 = %d，期望 1；账本=%+v", len(executes), item.book.readAll())
	}
	if got, want := finalText(resume.events), "已修改 notes.txt。"; got != want {
		t.Fatalf("最终回复 = %q，期望 %q", got, want)
	}
}
