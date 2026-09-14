package contextspike

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	adksessiondb "google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	appName   = "abot-context-spike"
	userID    = "spike-user"
	sessionID = "spike-session"
	agentName = "abot_context_spike_agent"

	// summaryMarker 是确定性摘要的标记，用来判断某个请求是否已经吃过 compaction。
	summaryMarker = "【PHASE0-摘要】前文已被压缩"
)

// scriptedModel 与 hitlspike 中的实现同构：按脚本返回响应并记录每次请求。
type scriptedModel struct {
	name      string
	responses []*genai.Content

	mu         sync.Mutex
	index      int
	requests   []string
	toolCounts []int
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
		declarations := 0
		if request.Config != nil {
			for _, declared := range request.Config.Tools {
				if declared != nil {
					declarations += len(declared.FunctionDeclarations)
				}
			}
		}
		m.toolCounts = append(m.toolCounts, declarations)
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

// toolDeclarations 返回每次模型调用实际收到的工具声明数量。
func (m *scriptedModel) toolDeclarations() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.toolCounts...)
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

func textResponse(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleModel)
}

func userText(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleUser)
}

// recordingSummarizer 是确定性摘要器：不调用模型，直接返回固定标记，
// 因此 compaction 是否发生、覆盖了哪些事件都可以被精确断言。
type recordingSummarizer struct {
	mu      sync.Mutex
	calls   int
	covered []int
}

func (s *recordingSummarizer) SummarizeEvents(_ context.Context, events []*session.Event) (compaction.SummarizeResult, error) {
	s.mu.Lock()
	s.calls++
	s.covered = append(s.covered, len(events))
	s.mu.Unlock()
	return compaction.SummarizeResult{
		Content: genai.NewContentFromText(summaryMarker, genai.RoleModel),
		Usage: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     123,
			CandidatesTokenCount: 45,
		},
	}, nil
}

func (s *recordingSummarizer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// callbackProbe 复刻 Abot 现有 BeforeModel callback 的位置（tool budget 就在那里），
// 记录它看到的 request 形态，用于判断它与 compaction 的先后关系。
type callbackProbe struct {
	mu           sync.Mutex
	observations []probeObservation
	// stripToolsFromCall > 0 时，从第 N 次 callback 起清空工具声明，
	// 复刻 Abot 现有 tool budget 逻辑（kernel.go 的 BeforeModelCallbacks）。
	stripToolsFromCall int
}

type probeObservation struct {
	ContentCount    int
	ContainsSummary bool
	TotalChars      int
	ToolCount       int
	FunctionCalls   int
}

func (p *callbackProbe) beforeModel(_ adkagent.Context, request *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
	observation := probeObservation{ContentCount: len(request.Contents)}
	for _, content := range request.Contents {
		text := textOf(content)
		observation.TotalChars += len(text)
		if strings.Contains(text, summaryMarker) {
			observation.ContainsSummary = true
		}
		for _, part := range content.Parts {
			if part != nil && part.FunctionCall != nil {
				observation.FunctionCalls++
			}
		}
	}
	if request.Config != nil {
		for _, declared := range request.Config.Tools {
			if declared != nil {
				observation.ToolCount += len(declared.FunctionDeclarations)
			}
		}
	}
	p.mu.Lock()
	p.observations = append(p.observations, observation)
	callIndex := len(p.observations)
	p.mu.Unlock()

	if p.stripToolsFromCall > 0 && callIndex >= p.stripToolsFromCall && request.Config != nil {
		config := *request.Config
		config.Tools = nil
		config.ToolConfig = nil
		request.Config = &config
	}
	return nil, nil
}

func (p *callbackProbe) snapshot() []probeObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]probeObservation(nil), p.observations...)
}

func (p *callbackProbe) last() (probeObservation, bool) {
	items := p.snapshot()
	if len(items) == 0 {
		return probeObservation{}, false
	}
	return items[len(items)-1], true
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

// ============================================================
// 场景装配
// ============================================================

type scenario struct {
	t          *testing.T
	dbPath     string
	service    session.Service
	model      *scriptedModel
	probe      *callbackProbe
	summarizer *recordingSummarizer
	runner     *adkrunner.Runner
	compaction *compaction.Config
}

func newScenario(t *testing.T, responses ...*genai.Content) *scenario {
	t.Helper()
	return newScenarioWithTools(t, nil, responses...)
}

func newScenarioWithTools(t *testing.T, tools []tool.Tool, responses ...*genai.Content) *scenario {
	t.Helper()
	dir := t.TempDir()
	item := &scenario{
		t:          t,
		dbPath:     filepath.Join(dir, "sessions.db"),
		probe:      &callbackProbe{},
		summarizer: &recordingSummarizer{},
		model:      newScriptedModel("scripted-flash", responses...),
	}
	item.service = openSessionService(t, item.dbPath)
	if err := ensureSession(context.Background(), item.service, sessionID); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	// TokenThreshold=1 保证每次模型调用前都会尝试 tail retention；
	// EventRetentionSize=2 保留最近两条事件原文，其余进入摘要。
	item.compaction = &compaction.Config{
		TokenThreshold:     1,
		EventRetentionSize: 2,
		Summarizer:         item.summarizer,
	}
	item.runner = item.buildRunner(tools)
	return item
}

func (s *scenario) buildRunner(tools []tool.Tool) *adkrunner.Runner {
	root, err := llmagent.New(llmagent.Config{
		Name:        agentName,
		Description: "Phase 0 context spike agent",
		Model:       s.model,
		Instruction: "你是上下文验证用的确定性 Agent。",
		Tools:       tools,
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			s.probe.beforeModel,
		},
	})
	if err != nil {
		s.t.Fatalf("创建 Agent 失败: %v", err)
	}
	runner, err := adkrunner.New(adkrunner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    s.service,
		AutoCreateSession: false,
		Compaction:        s.compaction,
	})
	if err != nil {
		s.t.Fatalf("创建 Runner 失败: %v", err)
	}
	return runner
}

func openSessionService(t *testing.T, dbPath string) session.Service {
	t.Helper()
	service, err := adksessiondb.NewSessionService(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("打开 SQLite 会话服务失败: %v", err)
	}
	if err := adksessiondb.AutoMigrate(service); err != nil {
		t.Fatalf("迁移 ADK 会话表失败: %v", err)
	}
	return service
}

func ensureSession(ctx context.Context, service session.Service, id string) error {
	if _, err := service.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: id}); err == nil {
		return nil
	}
	_, err := service.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: id})
	return err
}

type turnResult struct {
	events []*session.Event
	err    error
}

func (s *scenario) run(content *genai.Content) turnResult {
	s.t.Helper()
	result := turnResult{}
	for event, err := range s.runner.Run(context.Background(), userID, sessionID, content,
		adkagent.RunConfig{StreamingMode: adkagent.StreamingModeNone}) {
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

func (s *scenario) stored() session.Session {
	s.t.Helper()
	response, err := s.service.Get(context.Background(), &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		s.t.Fatalf("读取持久化会话失败: %v", err)
	}
	return response.Session
}

// compactionEvents 返回所有携带 EventCompaction 的事件，用于映射 AgentEvent。
func compactionEvents(stored session.Session) []*session.Event {
	var result []*session.Event
	if stored == nil {
		return result
	}
	for index := 0; index < stored.Events().Len(); index++ {
		event := stored.Events().At(index)
		if event != nil && event.Actions.Compaction != nil {
			result = append(result, event)
		}
	}
	return result
}

func summarizeText(event *session.Event) string {
	if event == nil || event.Actions.Compaction == nil {
		return ""
	}
	return textOf(event.Actions.Compaction.CompactedContent)
}

func eventTexts(stored session.Session) []string {
	var result []string
	for index := 0; index < stored.Events().Len(); index++ {
		event := stored.Events().At(index)
		if event == nil {
			continue
		}
		if text := textOf(event.Content); text != "" {
			result = append(result, text)
		}
	}
	return result
}

// findConfirmationCall 定位 adk_request_confirmation 调用（暂停点标记）。
func findConfirmationCall(events []*session.Event) (*genai.FunctionCall, bool) {
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
					return part.FunctionCall, true
				}
			}
		}
	}
	return nil, false
}

// confirmationResponse 构造批准恢复输入。
func confirmationResponse(t *testing.T, events []*session.Event) *genai.Content {
	t.Helper()
	call, ok := findConfirmationCall(events)
	if !ok {
		t.Fatalf("暂停回合没有产生确认请求；事件如下:\n%s", describeEvents(events))
	}
	return &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       call.ID,
				Name:     toolconfirmation.FunctionCallName,
				Response: map[string]any{"confirmed": true},
			},
		}},
	}
}

func lastText(events []*session.Event) string {
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

func describeEvents(events []*session.Event) string {
	var builder strings.Builder
	for index, event := range events {
		if event == nil {
			continue
		}
		fmt.Fprintf(&builder, "  [%02d] author=%s long_running=%v\n", index, event.Author, event.LongRunningToolIDs)
		if event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			switch {
			case part == nil:
			case part.FunctionCall != nil:
				fmt.Fprintf(&builder, "        call name=%s id=%s\n", part.FunctionCall.Name, part.FunctionCall.ID)
			case part.FunctionResponse != nil:
				fmt.Fprintf(&builder, "        response name=%s id=%s\n", part.FunctionResponse.Name, part.FunctionResponse.ID)
			case part.Text != "":
				fmt.Fprintf(&builder, "        text=%s\n", part.Text)
			}
		}
	}
	return builder.String()
}

func waitFor(t *testing.T, timeout time.Duration, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
