package hitlspike

import (
	"context"
	"testing"

	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// TestOriginalCallFromInputs 回答 AGENT_APPROVAL_RESUME.md §23 的第 3 问：
// OriginalCallFrom 的适用输入和失败语义。
func TestOriginalCallFromInputs(t *testing.T) {
	call := &genai.FunctionCall{ID: "call-1", Name: toolNamePatch, Args: map[string]any{"path": "a.txt"}}

	cases := []struct {
		name    string
		input   *genai.FunctionCall
		wantErr bool
	}{
		{name: "nil 调用", input: nil, wantErr: true},
		{name: "缺少 Args", input: &genai.FunctionCall{ID: "call-2", Name: toolNamePatch}, wantErr: true},
		{name: "缺少 originalFunctionCall 键", input: &genai.FunctionCall{ID: "call-3", Args: map[string]any{"other": 1}}, wantErr: true},
		{name: "originalFunctionCall 类型非法", input: &genai.FunctionCall{ID: "call-4", Args: map[string]any{"originalFunctionCall": "not-a-call"}}, wantErr: true},
		{name: "内存中的类型化调用", input: &genai.FunctionCall{ID: "call-5", Args: map[string]any{"originalFunctionCall": call}}},
		{name: "JSON 往返后的 map 调用", input: &genai.FunctionCall{ID: "call-6", Args: map[string]any{
			"originalFunctionCall": map[string]any{"id": "call-1", "name": toolNamePatch, "args": map[string]any{"path": "a.txt"}},
		}}},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			parsed, err := toolconfirmation.OriginalCallFrom(item.input)
			if item.wantErr {
				if err == nil {
					t.Fatalf("期望解析失败，实际得到 %+v", parsed)
				}
				t.Logf("失败语义: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("期望解析成功，实际失败: %v", err)
			}
			if parsed.Name != toolNamePatch || parsed.ID != "call-1" {
				t.Fatalf("解析结果不正确: %+v", parsed)
			}
		})
	}
}

// TestConfirmationArgsSerializationForm 固化一个实现细节：暂停事件里的
// originalFunctionCall 在内存中是 *genai.FunctionCall，落库再读回后变成 map[string]any。
// Runtime 必须两种形态都能处理，否则重启后会读不出原始调用。
func TestConfirmationArgsSerializationForm(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t, patchCall("notes.txt", "hello"), textResponse("done"))

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	liveCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}
	liveForm := argTypeName(liveCall)

	// 从 SQLite 重新读回同一个事件，模拟恢复回合或重启后 Runtime 看到的数据。
	stored := loadSession(t, ctx, item.service)
	var storedCall *genai.FunctionCall
	for index := 0; index < stored.Events().Len(); index++ {
		event := stored.Events().At(index)
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part != nil && part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
				storedCall = part.FunctionCall
			}
		}
	}
	if storedCall == nil {
		t.Fatal("SQLite 中没有持久化确认请求事件")
	}
	storedForm := argTypeName(storedCall)

	liveOriginal, err := toolconfirmation.OriginalCallFrom(liveCall)
	if err != nil {
		t.Fatalf("内存形态解析失败: %v", err)
	}
	storedOriginal, err := toolconfirmation.OriginalCallFrom(storedCall)
	if err != nil {
		t.Fatalf("落库形态解析失败: %v", err)
	}
	if liveOriginal.ID != storedOriginal.ID || liveOriginal.Name != storedOriginal.Name {
		t.Fatalf("两种形态解析出的原始调用不一致: live=%+v stored=%+v", liveOriginal, storedOriginal)
	}

	t.Logf("内存形态 originalFunctionCall 类型 = %s", liveForm)
	t.Logf("落库形态 originalFunctionCall 类型 = %s", storedForm)
	t.Logf("两种形态解析出的原始 call ID = %s", liveOriginal.ID)
	if liveForm == storedForm {
		t.Fatalf("预期两种形态类型不同（内存类型化、落库 map），实际都是 %s；需修正结论", liveForm)
	}
}

func argTypeName(call *genai.FunctionCall) string {
	if call == nil || call.Args == nil {
		return "<nil>"
	}
	value, ok := call.Args["originalFunctionCall"]
	if !ok {
		return "<missing>"
	}
	return typeName(value)
}

func typeName(value any) string {
	switch value.(type) {
	case *genai.FunctionCall:
		return "*genai.FunctionCall"
	case map[string]any:
		return "map[string]any"
	case nil:
		return "<nil>"
	default:
		return "other"
	}
}
