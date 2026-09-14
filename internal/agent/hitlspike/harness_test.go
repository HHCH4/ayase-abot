package hitlspike

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	adksessiondb "google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	appName   = "abot-hitl-spike"
	userID    = "spike-user"
	sessionID = "spike-session"
	agentName = "abot_spike_agent"

	// stateAbotInvocationID 是 AGENT_APPROVAL_RESUME.md §8.5 建议的稳定 Invocation
	// 上下文注入键；spike 用它验证“工具能否在首次进入和恢复进入时都读到同一个值”。
	stateAbotInvocationID = "abot_invocation_id"

	kindPrepare          = "prepare"
	kindExecute          = "execute"
	kindReject           = "reject"
	kindDuplicateBlocked = "duplicate_blocked"

	toolNamePatch = "workspace_patch"
)

// ============================================================
// 脚本化模型
// ============================================================

// scriptedModel 是确定性的 model.LLM：按脚本顺序返回响应，并记录每次请求的原文快照。
// 脚本耗尽后再被调用会返回错误，避免测试在没有断言的情况下静默多跑一轮模型。
type scriptedModel struct {
	name      string
	responses []*genai.Content

	mu       sync.Mutex
	index    int
	requests []string
}

func newScriptedModel(name string, responses ...*genai.Content) *scriptedModel {
	return &scriptedModel{name: name, responses: responses}
}

func (m *scriptedModel) Name() string { return m.name }

func (m *scriptedModel) GenerateContent(_ context.Context, request *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		snapshot, err := json.Marshal(request.Contents)
		if err != nil {
			m.mu.Unlock()
			yield(nil, fmt.Errorf("快照模型请求失败: %w", err))
			return
		}
		m.requests = append(m.requests, string(snapshot))
		callIndex := m.index
		m.index++
		var scripted *genai.Content
		if callIndex < len(m.responses) {
			scripted = cloneContent(m.responses[callIndex])
		}
		m.mu.Unlock()

		if scripted == nil {
			yield(nil, fmt.Errorf("脚本化模型第 %d 次被调用，但脚本只有 %d 个响应", callIndex+1, len(m.responses)))
			return
		}
		yield(&adkmodel.LLMResponse{Content: scripted}, nil)
	}
}

func (m *scriptedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.index
}

// requestSnapshots 返回每次模型调用看到的对话内容（JSON），用于验证模型确实收到了真实工具结果。
func (m *scriptedModel) requestSnapshots() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}

func (m *scriptedModel) lastRequest() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		return ""
	}
	return m.requests[len(m.requests)-1]
}

func cloneContent(content *genai.Content) *genai.Content {
	if content == nil {
		return nil
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return content
	}
	cloned := &genai.Content{}
	if err := json.Unmarshal(raw, cloned); err != nil {
		return content
	}
	return cloned
}

// patchCall 构造模型发起的 workspace_patch 调用。ID 故意留空，由 ADK 自行分配，
// 这样测试必须从事件里把真实 call ID 找出来，而不是自己假定。
func patchCall(path, content string) *genai.Content {
	return genai.NewContentFromFunctionCall(toolNamePatch, map[string]any{
		"path":    path,
		"content": content,
	}, genai.RoleModel)
}

func textResponse(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleModel)
}

// ============================================================
// 无副作用账本
// ============================================================

// ledgerEntry 是 spike 的“持久化审计记录”，同时承载 §8.5 关心的两种 Invocation ID 来源：
// 闭包绑定值与 ADK session state 值。二者都记录下来，测试才能判断哪种注入方式真实可用。
type ledgerEntry struct {
	Kind                  string `json:"kind"`
	At                    string `json:"at"`
	Sequence              int    `json:"sequence"`
	BoundAbotInvocationID string `json:"bound_abot_invocation_id,omitempty"`
	StateAbotInvocationID string `json:"state_abot_invocation_id,omitempty"`
	AdkInvocationID       string `json:"adk_invocation_id,omitempty"`
	FunctionCallID        string `json:"function_call_id,omitempty"`
	Path                  string `json:"path,omitempty"`
	Digest                string `json:"digest,omitempty"`
	Confirmed             *bool  `json:"confirmed,omitempty"`
}

type ledger struct {
	path string
	mu   sync.Mutex
}

func newLedger(path string) *ledger { return &ledger{path: path} }

func (l *ledger) append(entry ledgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.Sequence = len(l.readAllLocked()) + 1
	if entry.At == "" {
		entry.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(payload, '\n')); err != nil {
		return err
	}
	return nil
}

func (l *ledger) readAll() []ledgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readAllLocked()
}

func (l *ledger) readAllLocked() []ledgerEntry {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return nil
	}
	var entries []ledgerEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry ledgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

func (l *ledger) entriesOfKind(kind string) []ledgerEntry {
	var result []ledgerEntry
	for _, entry := range l.readAll() {
		if entry.Kind == kind {
			result = append(result, entry)
		}
	}
	return result
}

// findPrepare 模拟 §8.3 的“用 Abot Invocation ID + original call ID 回查准备记录”。
func (l *ledger) findPrepare(abotInvocationID, functionCallID string) (ledgerEntry, bool) {
	for _, entry := range l.entriesOfKind(kindPrepare) {
		if entry.FunctionCallID != functionCallID {
			continue
		}
		if abotInvocationID != "" && entry.BoundAbotInvocationID != "" && entry.BoundAbotInvocationID != abotInvocationID {
			continue
		}
		return entry, true
	}
	return ledgerEntry{}, false
}

// findExecute 模拟 §16.1 要求的“执行权”判定：某个 (Abot Invocation, 原始 call) 是否已经执行过。
func (l *ledger) findExecute(abotInvocationID, functionCallID string) (ledgerEntry, bool) {
	for _, entry := range l.entriesOfKind(kindExecute) {
		if entry.FunctionCallID != functionCallID {
			continue
		}
		if abotInvocationID != "" && entry.BoundAbotInvocationID != "" && entry.BoundAbotInvocationID != abotInvocationID {
			continue
		}
		return entry, true
	}
	return ledgerEntry{}, false
}

// ============================================================
// Guarded 工具（无副作用）
// ============================================================

type guardedArgs struct {
	Path    string `json:"path" jsonschema:"工作区根目录下的相对路径"`
	Content string `json:"content" jsonschema:"要写入文件的完整文本内容"`
}

type guardedResult struct {
	Status      string `json:"status"`
	OperationID string `json:"operation_id,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`
	Digest      string `json:"digest,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
}

// newGuardedPatchTool 复刻 AGENT_APPROVAL_RESUME.md §8.2 / §8.3 的 guarded handler 形状：
// 首次进入只做可逆准备并显式 RequestConfirmation；恢复进入才执行，且执行前回查准备记录
// 并比对 digest。它不写工作区，只写 spike 自己的账本。
//
// 与生产不同的是 Operation/Approval ID 由 digest 派生而非随机生成，目的是让事件 fixture
// 在多次运行之间保持稳定；生产实现必须使用不可预测 ID（§5.4）。
func newGuardedPatchTool(book *ledger, boundAbotInvocationID string) (tool.Tool, error) {
	return newGuardedPatchToolWith(book, boundAbotInvocationID, false)
}

// newGuardedPatchToolWith 的 oneShot 为 true 时启用“执行权”CAS：
// 已经执行过的 (Abot Invocation, 原始 call) 拒绝第二次执行。
// ADK 不会挡住重复的 confirmation response（实测见 TestDuplicateApprovalReExecutesTool），
// 因此这个判定必须由 Runtime/工具自己承担（§16.1）。
func newGuardedPatchToolWith(book *ledger, boundAbotInvocationID string, oneShot bool) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        toolNamePatch,
		Description: "对工作区文件执行一次精确补丁；写入前必须获得用户批准。",
	}, func(ctx adkagent.Context, args guardedArgs) (guardedResult, error) {
		digest := digestOf(args.Path, args.Content)
		entry := ledgerEntry{
			BoundAbotInvocationID: boundAbotInvocationID,
			StateAbotInvocationID: readStateString(ctx, stateAbotInvocationID),
			AdkInvocationID:       ctx.InvocationID(),
			FunctionCallID:        ctx.FunctionCallID(),
			Path:                  args.Path,
			Digest:                digest,
		}

		confirmation := ctx.ToolConfirmation()
		if confirmation == nil {
			// 首次进入：先持久化准备记录，再请求确认（§10 的提交点顺序）。
			entry.Kind = kindPrepare
			if err := book.append(entry); err != nil {
				return guardedResult{}, err
			}
			hint := fmt.Sprintf("请审批对 %s 的写入（digest %s）", args.Path, digest)
			payload := map[string]any{
				"approval_id":  "apv_" + digest[:12],
				"operation_id": "op_" + digest[:12],
				"digest":       digest,
			}
			if err := ctx.RequestConfirmation(hint, payload); err != nil {
				return guardedResult{}, err
			}
			return guardedResult{
				Status:      "waiting_approval",
				OperationID: "op_" + digest[:12],
				ApprovalID:  "apv_" + digest[:12],
				Digest:      digest,
			}, nil
		}

		confirmed := confirmation.Confirmed
		entry.Confirmed = &confirmed
		if !confirmed {
			// 恢复进入且被拒绝。注意：functiontool 包装下这条分支不可达（见 reject_test.go），
			// 只有自实现 tool.Tool 时才会走到这里。
			entry.Kind = kindReject
			if err := book.append(entry); err != nil {
				return guardedResult{}, err
			}
			return guardedResult{Status: "rejected", Digest: digest}, nil
		}

		// §3.7：ADK 的 confirmed 只负责恢复控制流，副作用是否允许由 Abot 自己的持久化记录决定。
		prepared, ok := book.findPrepare(boundAbotInvocationID, ctx.FunctionCallID())
		if !ok {
			return guardedResult{}, errors.New("找不到匹配的 prepare 记录，拒绝执行")
		}
		if prepared.Digest != digest {
			return guardedResult{}, errors.New("参数 digest 与准备阶段不一致，拒绝执行")
		}
		if oneShot {
			if _, executed := book.findExecute(boundAbotInvocationID, ctx.FunctionCallID()); executed {
				entry.Kind = kindDuplicateBlocked
				if err := book.append(entry); err != nil {
					return guardedResult{}, err
				}
				return guardedResult{Status: "already_applied", Digest: digest}, nil
			}
		}

		entry.Kind = kindExecute
		if err := book.append(entry); err != nil {
			return guardedResult{}, err
		}
		return guardedResult{
			Status:      "applied",
			OperationID: "op_" + digest[:12],
			ApprovalID:  "apv_" + digest[:12],
			Digest:      digest,
			Bytes:       len(args.Content),
		}, nil
	})
}

// newStaticConfirmationTool 使用 ADK 自带的静态 RequireConfirmation 路径，
// 用于对比“框架自动确认”和“handler 内显式确认”在拒绝时的差异（§8.1 的取舍依据）。
func newStaticConfirmationTool(book *ledger) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:                toolNamePatch,
		Description:         "静态 RequireConfirmation 对照工具；handler 只在确认通过后执行。",
		RequireConfirmation: true,
	}, func(ctx adkagent.Context, args guardedArgs) (guardedResult, error) {
		confirmed := false
		if confirmation := ctx.ToolConfirmation(); confirmation != nil {
			confirmed = confirmation.Confirmed
		}
		entry := ledgerEntry{
			Kind:            kindExecute,
			Confirmed:       &confirmed,
			AdkInvocationID: ctx.InvocationID(),
			FunctionCallID:  ctx.FunctionCallID(),
			Path:            args.Path,
			Digest:          digestOf(args.Path, args.Content),
		}
		if err := book.append(entry); err != nil {
			return guardedResult{}, err
		}
		return guardedResult{Status: "applied", Bytes: len(args.Content), Digest: entry.Digest}, nil
	})
}

func digestOf(path, content string) string {
	sum := sha256.Sum256([]byte(path + "\x00" + content))
	return hex.EncodeToString(sum[:])[:16]
}

func readStateString(ctx adkagent.Context, key string) string {
	if ctx == nil {
		return ""
	}
	value, err := ctx.State().Get(key)
	if err != nil {
		return ""
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// ============================================================
// 会话与 Runner
// ============================================================

func openSessionService(t interface {
	Helper()
	Fatalf(string, ...any)
}, dbPath string) session.Service {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("创建 spike 数据目录失败: %v", err)
	}
	service, err := adksessiondb.NewSessionService(sqlite.Open(dbPath), &gorm.Config{
		// spike 关心的是事件形状；GORM 的 record not found 日志会淹没测试输出。
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("打开 ADK SQLite 会话服务失败: %v", err)
	}
	if err := adksessiondb.AutoMigrate(service); err != nil {
		t.Fatalf("迁移 ADK 会话表失败: %v", err)
	}
	return service
}

func ensureSession(ctx context.Context, service session.Service, id string) error {
	_, err := service.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: id})
	if err == nil {
		return nil
	}
	_, createErr := service.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: id})
	return createErr
}

func newRunner(t interface {
	Helper()
	Fatalf(string, ...any)
}, service session.Service, model adkmodel.LLM, tools []tool.Tool) *adkrunner.Runner {
	t.Helper()
	root, err := llmagent.New(llmagent.Config{
		Name:        agentName,
		Description: "Phase 0 HITL spike agent",
		Model:       model,
		Instruction: "你是 Phase 0 技术验证用的确定性 Agent，只按工具结果作答。",
		Tools:       tools,
	})
	if err != nil {
		t.Fatalf("创建 spike Agent 失败: %v", err)
	}
	runner, err := adkrunner.New(adkrunner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    service,
		AutoCreateSession: false,
	})
	if err != nil {
		t.Fatalf("创建 spike Runner 失败: %v", err)
	}
	return runner
}

type turnResult struct {
	events []*session.Event
	err    error
}

// runTurn 排空一次 Runner 激活的事件流，保留错误而不是直接失败，因为拒绝路径本身会产生错误事件。
func runTurn(ctx context.Context, runner *adkrunner.Runner, content *genai.Content, opts ...adkrunner.RunOption) turnResult {
	result := turnResult{}
	for event, err := range runner.Run(ctx, userID, sessionID, content, adkagent.RunConfig{StreamingMode: adkagent.StreamingModeNone}, opts...) {
		if err != nil {
			result.err = err
			continue
		}
		if event != nil {
			result.events = append(result.events, event)
		}
	}
	return result
}

// ============================================================
// 事件读取辅助
// ============================================================

// findConfirmationCall 定位 ADK 生成的 adk_request_confirmation FunctionCall。
// 依据 ADK v2.3.0 internal/llminternal/functions.go，该 call 的 ID 同时出现在
// 事件的 LongRunningToolIDs 中，这是“暂停点”的可判定标记。
func findConfirmationCall(events []*session.Event) (*genai.FunctionCall, *session.Event, bool) {
	for _, event := range events {
		if event == nil || len(event.LongRunningToolIDs) == 0 || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part == nil || part.FunctionCall == nil {
				continue
			}
			if part.FunctionCall.Name != toolconfirmation.FunctionCallName {
				continue
			}
			for _, id := range event.LongRunningToolIDs {
				if id == part.FunctionCall.ID {
					return part.FunctionCall, event, true
				}
			}
		}
	}
	return nil, nil, false
}

func findFunctionCall(events []*session.Event, name string) (*genai.FunctionCall, bool) {
	for _, event := range events {
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part != nil && part.FunctionCall != nil && part.FunctionCall.Name == name {
				return part.FunctionCall, true
			}
		}
	}
	return nil, false
}

// confirmationPair 把 ADK 生成的确认调用和它包装的原始业务调用配成一对，
// 这是 Runtime 在暂停点必须落库的两个 call ID（AGENT_APPROVAL_RESUME.md §3.4）。
type confirmationPair struct {
	ConfirmationCall *genai.FunctionCall
	OriginalCall     *genai.FunctionCall
	Event            *session.Event
}

// findAllConfirmations 支持一个暂停事件里包含多个确认调用的情形（§23 第 9 问）。
func findAllConfirmations(events []*session.Event) []confirmationPair {
	var pairs []confirmationPair
	for _, event := range events {
		if event == nil || len(event.LongRunningToolIDs) == 0 || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part == nil || part.FunctionCall == nil {
				continue
			}
			if part.FunctionCall.Name != toolconfirmation.FunctionCallName {
				continue
			}
			if !slices.Contains(event.LongRunningToolIDs, part.FunctionCall.ID) {
				continue
			}
			original, err := toolconfirmation.OriginalCallFrom(part.FunctionCall)
			if err != nil {
				continue
			}
			pairs = append(pairs, confirmationPair{
				ConfirmationCall: part.FunctionCall,
				OriginalCall:     original,
				Event:            event,
			})
		}
	}
	return pairs
}

// multiConfirmationResponse 在一个用户事件里携带多个确认响应（§23 第 9 问的输入形态）。
func multiConfirmationResponse(decisions ...confirmationDecision) *genai.Content {
	parts := make([]*genai.Part, 0, len(decisions))
	for _, decision := range decisions {
		parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
			ID:       decision.CallID,
			Name:     toolconfirmation.FunctionCallName,
			Response: map[string]any{"confirmed": decision.Confirmed},
		}})
	}
	return &genai.Content{Role: genai.RoleUser, Parts: parts}
}

type confirmationDecision struct {
	CallID    string
	Confirmed bool
}

func findFunctionResponse(events []*session.Event, name string) (*genai.FunctionResponse, bool) {
	for _, event := range events {
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == name {
				return part.FunctionResponse, true
			}
		}
	}
	return nil, false
}

// allFunctionResponseIDs 按事件/分片顺序返回某个工具的全部 FunctionResponse ID，
// 用于验证模型看到的结果顺序（ADK 组装顺序），而不是副作用完成顺序。
func allFunctionResponseIDs(events []*session.Event, name string) []string {
	var ids []string
	for _, event := range events {
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == name {
				ids = append(ids, part.FunctionResponse.ID)
			}
		}
	}
	return ids
}

func finalText(events []*session.Event) string {
	text := ""
	for _, event := range events {
		if event == nil || event.Author == "user" {
			continue
		}
		if value := textOf(event.Content); value != "" {
			text = value
		}
	}
	return text
}

func textOf(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range content.Parts {
		if part != nil && part.Text != "" {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// describeEvents 用于失败输出：把事件形状渲染成可读文本，避免只看“断言失败”而无从判断。
func describeEvents(events []*session.Event) string {
	var builder strings.Builder
	for index, event := range events {
		if event == nil {
			fmt.Fprintf(&builder, "  [%02d] <nil>\n", index)
			continue
		}
		fmt.Fprintf(&builder, "  [%02d] author=%s invocation=%s long_running=%v\n",
			index, event.Author, event.InvocationID, event.LongRunningToolIDs)
		if event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			switch {
			case part == nil:
			case part.FunctionCall != nil:
				fmt.Fprintf(&builder, "        function_call name=%s id=%s args=%v\n",
					part.FunctionCall.Name, part.FunctionCall.ID, part.FunctionCall.Args)
			case part.FunctionResponse != nil:
				fmt.Fprintf(&builder, "        function_response name=%s id=%s response=%v\n",
					part.FunctionResponse.Name, part.FunctionResponse.ID, part.FunctionResponse.Response)
			case part.Text != "":
				fmt.Fprintf(&builder, "        text=%s\n", part.Text)
			}
		}
	}
	return builder.String()
}

func eventInvocationIDs(events []*session.Event) []string {
	var ids []string
	for _, event := range events {
		if event == nil || event.InvocationID == "" {
			continue
		}
		if len(ids) > 0 && ids[len(ids)-1] == event.InvocationID {
			continue
		}
		ids = append(ids, event.InvocationID)
	}
	return ids
}

// confirmationResponse 构造用户决定产生的恢复输入。ID 必须是 confirmation call 的 ID，
// 名字必须是 adk_request_confirmation（见 toolconfirmation 包注释与 ADK 例子）。
func confirmationResponse(callID string, confirmed bool, payload map[string]any) *genai.Content {
	response := map[string]any{"confirmed": confirmed}
	if payload != nil {
		response["payload"] = payload
	}
	return &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       callID,
				Name:     toolconfirmation.FunctionCallName,
				Response: response,
			},
		}},
	}
}

func userText(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleUser)
}

// loadSession 用于在测试里直接检查 SQLite 中的持久化状态。
func loadSession(t interface {
	Helper()
	Fatalf(string, ...any)
}, ctx context.Context, service session.Service) session.Session {
	t.Helper()
	response, err := service.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("读取持久化会话失败: %v", err)
	}
	return response.Session
}
