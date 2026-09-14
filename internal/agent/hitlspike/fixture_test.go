package hitlspike

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// updateFixture 用 `go test ./internal/agent/hitlspike/ -update-hitl-fixture` 更新基线。
var updateFixture = flag.Bool("update-hitl-fixture", false, "更新 HITL 事件 fixture 基线")

type fixtureFile struct {
	Scenario string        `json:"scenario"`
	Note     string        `json:"note"`
	Turns    []fixtureTurn `json:"turns"`
}

type fixtureTurn struct {
	Name   string         `json:"name"`
	Events []fixtureEvent `json:"events"`
}

type fixtureEvent struct {
	Author             string             `json:"author"`
	InvocationID       string             `json:"invocation_id"`
	LongRunningToolIDs []string           `json:"long_running_tool_ids,omitempty"`
	SkipSummarization  bool               `json:"skip_summarization,omitempty"`
	StateDelta         map[string]any     `json:"state_delta,omitempty"`
	RequestedConfirms  map[string]any     `json:"requested_tool_confirmations,omitempty"`
	Parts              []fixturePart      `json:"parts,omitempty"`
	Usage              *fixtureUsageBlock `json:"usage,omitempty"`
}

type fixtureUsageBlock struct {
	PromptTokenCount int32 `json:"prompt_token_count,omitempty"`
}

type fixturePart struct {
	Kind     string         `json:"kind"`
	Name     string         `json:"name,omitempty"`
	ID       string         `json:"id,omitempty"`
	Text     string         `json:"text,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

// idMapper 把每次运行都会变化的 ID 换成稳定占位符，同时保留 ID 之间的相等/不等关系。
// 只有真正属于“已知 ID 集合”的字符串才会被替换，业务文本（路径、状态、摘要）保持原文，
// 这样 fixture 才能反映语义变化，而不只是随机噪声。
type idMapper struct {
	known map[string]bool
	index map[string]string
	next  int
}

func newIDMapper(eventSets ...[]*session.Event) *idMapper {
	known := map[string]bool{}
	for _, events := range eventSets {
		collectKnownIDs(events, known)
	}
	return &idMapper{known: known, index: map[string]string{}}
}

func (m *idMapper) mapID(id string) string {
	if id == "" || !m.known[id] {
		return id
	}
	if mapped, ok := m.index[id]; ok {
		return mapped
	}
	m.next++
	mapped := fmt.Sprintf("id#%d", m.next)
	m.index[id] = mapped
	return mapped
}

// collectKnownIDs 收集所有会随运行变化的标识符：事件、invocation、工具调用、
// 确认调用以及嵌套在参数里的原始调用 ID。
func collectKnownIDs(events []*session.Event, known map[string]bool) {
	for _, event := range events {
		if event == nil {
			continue
		}
		if event.ID != "" {
			known[event.ID] = true
		}
		if event.InvocationID != "" {
			known[event.InvocationID] = true
		}
		for _, id := range event.LongRunningToolIDs {
			known[id] = true
		}
		for callID := range event.Actions.RequestedToolConfirmations {
			known[callID] = true
		}
		if event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part == nil {
				continue
			}
			if part.FunctionCall != nil {
				if part.FunctionCall.ID != "" {
					known[part.FunctionCall.ID] = true
				}
				collectNestedIDs(part.FunctionCall.Args, known)
			}
			if part.FunctionResponse != nil && part.FunctionResponse.ID != "" {
				known[part.FunctionResponse.ID] = true
			}
		}
	}
}

func collectNestedIDs(value any, known map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == "id" {
				if text, ok := item.(string); ok && text != "" {
					known[text] = true
				}
			}
			collectNestedIDs(item, known)
		}
	case []any:
		for _, item := range typed {
			collectNestedIDs(item, known)
		}
	case *genai.FunctionCall:
		if typed != nil {
			if typed.ID != "" {
				known[typed.ID] = true
			}
			collectNestedIDs(typed.Args, known)
		}
	}
}

func (m *idMapper) normalize(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return m.mapID(typed)
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[m.mapID(key)] = m.normalize(item)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, m.normalize(item))
		}
		return result
	default:
		// 只有结构体/指针/容器才需要 JSON 往返（例如 *genai.FunctionCall、
		// toolconfirmation.ToolConfirmation）。数字和布尔必须原样返回，
		// 否则往返会把它重新解成 float64 并再次进入本分支，形成无限递归。
		switch reflect.TypeOf(typed).Kind() {
		case reflect.Struct, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Array:
		default:
			return typed
		}
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("<%T>", typed)
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return fmt.Sprintf("<%T>", typed)
		}
		return m.normalize(decoded)
	}
}

func buildFixtureEvents(events []*session.Event, mapper *idMapper) []fixtureEvent {
	result := make([]fixtureEvent, 0, len(events))
	for _, event := range events {
		if event == nil {
			continue
		}
		item := fixtureEvent{
			Author:            event.Author,
			InvocationID:      mapper.mapID(event.InvocationID),
			SkipSummarization: event.Actions.SkipSummarization,
			StateDelta:        normalizeMap(mapper, event.Actions.StateDelta),
			RequestedConfirms: normalizeMap(mapper, requestedConfirmations(event)),
		}
		for _, id := range event.LongRunningToolIDs {
			item.LongRunningToolIDs = append(item.LongRunningToolIDs, mapper.mapID(id))
		}
		if event.Content != nil {
			for _, part := range event.Content.Parts {
				if part == nil {
					continue
				}
				switch {
				case part.FunctionCall != nil:
					item.Parts = append(item.Parts, fixturePart{
						Kind: "function_call",
						Name: part.FunctionCall.Name,
						ID:   mapper.mapID(part.FunctionCall.ID),
						Args: normalizeMap(mapper, part.FunctionCall.Args),
					})
				case part.FunctionResponse != nil:
					item.Parts = append(item.Parts, fixturePart{
						Kind:     "function_response",
						Name:     part.FunctionResponse.Name,
						ID:       mapper.mapID(part.FunctionResponse.ID),
						Response: normalizeMap(mapper, part.FunctionResponse.Response),
					})
				case part.Text != "":
					item.Parts = append(item.Parts, fixturePart{Kind: "text", Text: part.Text})
				}
			}
		}
		result = append(result, item)
	}
	return result
}

func normalizeMap(mapper *idMapper, source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	normalized, _ := mapper.normalize(source).(map[string]any)
	return normalized
}

// requestedConfirmations 把 EventActions.RequestedToolConfirmations 转成可比较的 map，
// 它的键是原始工具 call ID，是暂停点必须落库的信息之一。
func requestedConfirmations(event *session.Event) map[string]any {
	if event == nil || len(event.Actions.RequestedToolConfirmations) == 0 {
		return nil
	}
	result := make(map[string]any, len(event.Actions.RequestedToolConfirmations))
	for callID, confirmation := range event.Actions.RequestedToolConfirmations {
		result[callID] = confirmation
	}
	return result
}

func persistedEvents(t *testing.T, ctx context.Context, item *scenario, from int) []*session.Event {
	t.Helper()
	stored := loadSession(t, ctx, item.service)
	events := make([]*session.Event, 0, stored.Events().Len())
	for index := from; index < stored.Events().Len(); index++ {
		events = append(events, stored.Events().At(index))
	}
	return events
}

func compareFixture(t *testing.T, path string, actual fixtureFile) {
	t.Helper()
	payload, err := json.MarshalIndent(actual, "", "  ")
	if err != nil {
		t.Fatalf("序列化 fixture 失败: %v", err)
	}
	payload = append(payload, '\n')
	if *updateFixture {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("创建 testdata 目录失败: %v", err)
		}
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatalf("写入 fixture 失败: %v", err)
		}
		t.Logf("已更新 fixture: %s", path)
		return
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 fixture 失败（首次可运行 -update-hitl-fixture 生成）: %v", err)
	}
	if string(expected) == string(payload) {
		return
	}
	expectedPath := filepath.Join(t.TempDir(), "expected.json")
	if err := os.WriteFile(expectedPath, expected, 0o644); err != nil {
		t.Fatalf("写入对比文件失败: %v", err)
	}
	t.Fatalf("事件 fixture 与基线不一致，说明 ADK 的确认/恢复协议可能已经变化。\n基线: %s\n实际: %s\n可用 -update-hitl-fixture 查看差异并决定是否更新基线。",
		path, expectedPath)
}

// TestApproveRoundTripFixture 冻结「暂停 + 批准恢复」的完整持久化事件形状。
// 依据 AGENT_APPROVAL_RESUME.md §25.6，ADK 升级后 fixture 差异应能被测试发现。
func TestApproveRoundTripFixture(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t, patchCall("notes.txt", "hello"), textResponse("已修改 notes.txt。"))

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}
	pauseEventCount := loadSession(t, ctx, item.service).Events().Len()
	pauseEvents := persistedEvents(t, ctx, item, 0)

	resume := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, true, nil))
	if resume.err != nil {
		t.Fatalf("批准恢复失败: %v", resume.err)
	}
	resumeEvents := persistedEvents(t, ctx, item, pauseEventCount)
	if len(resumeEvents) == 0 {
		t.Fatal("恢复回合没有持久化任何事件")
	}

	mapper := newIDMapper(pauseEvents, resumeEvents)
	fixture := fixtureFile{
		Scenario: "approve",
		Note:     "Phase 0 HITL 基线：暂停 + 批准恢复。ID 已归一化为 id#N，仅保留相等关系。",
		Turns: []fixtureTurn{
			{Name: "pause", Events: buildFixtureEvents(pauseEvents, mapper)},
			{Name: "approve_resume", Events: buildFixtureEvents(resumeEvents, mapper)},
		},
	}
	compareFixture(t, filepath.Join("testdata", "hitl_approve_roundtrip.json"), fixture)
}

// TestRejectRoundTripFixture 冻结「暂停 + 拒绝恢复」的事件形状，重点是框架产生的错误结果。
func TestRejectRoundTripFixture(t *testing.T) {
	ctx := context.Background()
	item := newScenario(t, patchCall("notes.txt", "hello"), textResponse("已按拒绝取消修改。"))

	pause := runTurn(ctx, item.runner, userText("请修改 notes.txt"), item.stateDelta(), adkrunner.WithYieldUserMessage())
	if pause.err != nil {
		t.Fatalf("暂停回合失败: %v", pause.err)
	}
	confirmationCall, _, ok := findConfirmationCall(pause.events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(pause.events))
	}
	pauseEventCount := loadSession(t, ctx, item.service).Events().Len()
	pauseEvents := persistedEvents(t, ctx, item, 0)

	resume := runTurn(ctx, item.runner, confirmationResponse(confirmationCall.ID, false, nil))
	if resume.err != nil {
		t.Fatalf("拒绝恢复失败: %v", resume.err)
	}
	resumeEvents := persistedEvents(t, ctx, item, pauseEventCount)

	mapper := newIDMapper(pauseEvents, resumeEvents)
	fixture := fixtureFile{
		Scenario: "reject",
		Note:     "Phase 0 HITL 基线：暂停 + 拒绝恢复。拒绝结果由 ADK 以错误 FunctionResponse 形式给出。",
		Turns: []fixtureTurn{
			{Name: "pause", Events: buildFixtureEvents(pauseEvents, mapper)},
			{Name: "reject_resume", Events: buildFixtureEvents(resumeEvents, mapper)},
		},
	}
	compareFixture(t, filepath.Join("testdata", "hitl_reject_roundtrip.json"), fixture)
}
