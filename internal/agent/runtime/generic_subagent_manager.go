package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"Abot/internal/agent"
	"google.golang.org/adk/v2/tool"
)

// RunSubAgent 负责通用子 Agent 的 durable 编排。这里故意不派生总超时 Context；
// 只有父 Invocation 的取消、截止时间或 Runtime 关闭可以结束子 Agent。
func (m *SubAgentManager) RunSubAgent(ctx context.Context, request agent.SubAgentRequest) (string, error) {
	if m == nil {
		return "", errors.New("子 Agent 管理器不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !request.Runtime.SubAgentsEnabled() {
		return "", agent.ErrSubAgentsDisabled
	}
	m.mu.Lock()
	runner := m.genericRunner
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return "", ErrConflict
	}
	if runner == nil {
		return "", errors.New("通用子 Agent 未装配")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	request.UserID = strings.TrimSpace(request.UserID)
	request.InvocationID = strings.TrimSpace(request.InvocationID)
	request.ParentNodeID = strings.TrimSpace(request.ParentNodeID)
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.UserID == "" {
		return "", errors.New("通用子 Agent user_id 不能为空")
	}
	if request.Prompt == "" {
		return "", errors.New("通用子 Agent prompt 不能为空")
	}
	if len(request.Images) > maxSubAgentRuns {
		return "", fmt.Errorf("通用子 Agent 图片数量不能超过 %d", maxSubAgentRuns)
	}
	if err := m.validateInvocationScope(ctx, request.InvocationID, request.UserID, request.ConversationID, ""); err != nil {
		return "", err
	}
	releaseQuota, err := m.reserveInvocationGroup(ctx, request.InvocationID)
	if err != nil {
		return "", err
	}
	defer releaseQuota()

	options := request.Runtime.SubAgent
	maxConcurrency := options.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	if maxConcurrency > maxSubAgentConcurrency {
		return "", fmt.Errorf("通用子 Agent 并发数不能超过 %d", maxSubAgentConcurrency)
	}
	outputBudget := options.OutputBudgetBytes
	if outputBudget <= 0 {
		outputBudget = maxSubAgentOutputBytes
	}
	if outputBudget > maxSubAgentOutputBytes {
		return "", fmt.Errorf("通用子 Agent 输出预算不能超过 %d 字节", maxSubAgentOutputBytes)
	}
	inputBytes := len([]byte(request.Prompt))
	var imageBytes int
	for _, image := range request.Images {
		imageBytes += len(image.Data)
	}
	inputBudget := options.InputBudgetBytes
	if inputBudget <= 0 {
		// 默认允许完整的有界文档提取指令；图片二进制另按 image budget 校验，
		// 不把一个正常的 PDF 图片任务误判成文本超限。
		inputBudget = maxBuiltInSubAgentPromptTotalBytes
	}
	if inputBytes > inputBudget {
		return "", fmt.Errorf("通用子 Agent prompt 超出 Runtime 输入预算（上限 %d 字节）", inputBudget)
	}
	if imageBytes > maxDocumentImageTotal {
		return "", errors.New("通用子 Agent 图片输入超出 Runtime 输入预算")
	}

	now := time.Now().UTC()
	group := SubAgentGroup{
		ID: newID("subagent-group"), InvocationID: request.InvocationID, ConversationID: request.ConversationID,
		ParentInvocationID: request.InvocationID, ParentNodeID: request.ParentNodeID,
		Profile: SubAgentProfileGeneric, Purpose: strings.TrimSpace(request.Purpose),
		FailurePolicy: SubAgentFailureContinue, ExpectedCount: 1, MaxConcurrency: maxConcurrency,
		// 通用子 Agent 不设置 TimeoutSeconds；0 是“继承父 Context”的明确状态。
		TimeoutSeconds: 0, OutputBudgetBytes: outputBudget, InputBudgetBytes: inputBytes, ImageBudgetBytes: imageBytes,
	}
	if group.Purpose == "" {
		group.Purpose = "执行通用只读子 Agent 任务"
	}
	group, err = normalizeSubAgentGroup(group, now)
	if err != nil {
		return "", err
	}
	providerID := strings.TrimSpace(request.Runtime.SubAgent.ProviderID)
	if providerID == "" {
		providerID = strings.TrimSpace(request.Runtime.ProviderID)
	}
	modelID := strings.TrimSpace(request.Runtime.SubAgent.ModelID)
	if modelID == "" {
		modelID = strings.TrimSpace(request.Runtime.ModelID)
	}
	run, err := normalizeSubAgentRun(SubAgentRun{
		ID: newID("subagent-run"), GroupID: group.ID, InvocationID: request.InvocationID,
		ParentNodeID: request.ParentNodeID, Ordinal: 0, Stage: 0, Profile: SubAgentProfileGeneric,
		ProviderID: providerID, ModelID: modelID,
		InputMetadata: map[string]any{
			"purpose":        group.Purpose,
			"prompt_digest":  digestSubAgentText(request.Prompt),
			"tool_names":     genericSubAgentToolNames(request.Tools),
			"image_count":    len(request.Images),
			"image_bytes":    imageBytes,
			"parent_node_id": request.ParentNodeID,
		},
	}, now)
	if err != nil {
		return "", err
	}
	if err := m.subRepo.CreateSubAgentGroup(ctx, group, []SubAgentRun{run}); err != nil {
		return "", err
	}
	group.Status = SubAgentGroupRunning
	group.StartedAt = now
	group.QueuedCount = 1
	group.UpdatedAt = now
	if err := m.subRepo.UpdateSubAgentGroup(ctx, group); err != nil {
		return "", err
	}

	// startGroup 的 timeout 参数为 0，保证子 Agent 不会被 Runtime 再套一层短超时。
	groupCtx, release := m.startGroup(group.ID, ctx, 0)
	defer release()
	m.emit(ctx, group.InvocationID, EventSubAgentRequested, map[string]any{"group_id": group.ID, "profile": group.Profile, "count": 1, "purpose": group.Purpose})
	m.emit(ctx, group.InvocationID, EventSubAgentQueued, map[string]any{"group_id": group.ID, "profile": group.Profile, "count": 1})

	text, runErr := m.runGenericSubAgent(groupCtx, group, run, request, runner)
	m.finishGenericSubAgentGroup(ctx, group, run.ID, text, runErr)
	if runErr != nil {
		return "", runErr
	}
	return text, nil
}

func (m *SubAgentManager) runGenericSubAgent(ctx context.Context, group SubAgentGroup, run SubAgentRun, request agent.SubAgentRequest, runner agent.SubAgentRunner) (string, error) {
	if err := ctx.Err(); err != nil {
		status := subAgentContextTerminalStatus(err)
		_, _ = m.subRepo.TransitionSubAgentRun(context.WithoutCancel(ctx), run.ID, SubAgentRunQueued, status, err.Error(), time.Now().UTC())
		m.emitContextTerminal(group.InvocationID, group.ID, run.ID, status, boundedSubAgentError(err))
		return "", err
	}
	owner := newID("subagent-worker")
	claimed, ok, err := m.subRepo.ClaimSubAgentRun(ctx, run.ID, owner, time.Now().UTC(), maxSubAgentLease)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("通用子 Agent run 未能取得执行租约")
	}
	stopRenew := m.startSubAgentLeaseRenewal(ctx, claimed.ID, owner)
	defer stopRenew()
	m.emit(ctx, group.InvocationID, EventSubAgentStarted, map[string]any{"group_id": group.ID, "run_id": claimed.ID, "profile": claimed.Profile, "parent_node_id": request.ParentNodeID})
	slog.Info("通用子Agent运行已开始", "invocation_id", group.InvocationID, "group_id", group.ID, "run_id", claimed.ID, "purpose", group.Purpose, "tools", genericSubAgentToolNames(request.Tools), "images", len(request.Images))

	text, runErr := runner.RunSubAgent(ctx, request)
	text = strings.TrimSpace(text)
	perRunBudget := request.Runtime.SubAgent.OutputBudgetBytes
	if perRunBudget <= 0 || perRunBudget > maxSubAgentOutputBytes {
		perRunBudget = maxSubAgentOutputBytes
	}
	if len([]byte(text)) > perRunBudget {
		text = limitSubAgentText(text, perRunBudget)
	}
	status := SubAgentRunCompleted
	errorCode := ""
	message := ""
	if runErr != nil {
		status = SubAgentRunFailed
		errorCode = "subagent_failed"
		message = boundedSubAgentError(runErr)
		if ctx.Err() != nil {
			status = subAgentContextTerminalStatus(ctx.Err())
			errorCode = string(status)
			message = boundedSubAgentError(ctx.Err())
		}
	} else if text == "" {
		status = SubAgentRunFailed
		errorCode = "empty_result"
		message = "通用子 Agent 未返回摘要"
	}
	completed, completeErr := m.subRepo.CompleteSubAgentRun(context.WithoutCancel(ctx), claimed.ID, owner, status, text, errorCode, message, time.Now().UTC())
	if completeErr != nil {
		return "", completeErr
	}
	if !completed {
		return "", errors.New("通用子 Agent run 完成确认失败")
	}
	if status != SubAgentRunCompleted {
		m.emitContextTerminal(group.InvocationID, group.ID, claimed.ID, status, message)
		slog.Warn("通用子Agent运行失败", "invocation_id", group.InvocationID, "group_id", group.ID, "run_id", claimed.ID, "status", status, "error", message)
		if runErr != nil {
			return "", runErr
		}
		return "", errors.New(message)
	}
	m.emit(ctx, group.InvocationID, EventSubAgentCompleted, map[string]any{"group_id": group.ID, "run_id": claimed.ID, "result_digest": digestSubAgentText(text), "result_bytes": len([]byte(text))})
	slog.Info("通用子Agent运行已完成", "invocation_id", group.InvocationID, "group_id", group.ID, "run_id", claimed.ID, "result_bytes", len([]byte(text)))
	return text, nil
}

func (m *SubAgentManager) startSubAgentLeaseRenewal(ctx context.Context, runID, owner string) func() {
	repository, ok := m.subRepo.(SubAgentLeaseRepository)
	if !ok || repository == nil {
		return func() {}
	}
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case now := <-ticker.C:
				renewed, err := repository.RenewSubAgentRunLease(context.WithoutCancel(ctx), runID, owner, now.UTC(), maxSubAgentLease)
				if err != nil || !renewed {
					slog.Warn("通用子Agent执行租约续期失败", "run_id", runID, "error", err, "renewed", renewed)
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

func (m *SubAgentManager) finishGenericSubAgentGroup(ctx context.Context, group SubAgentGroup, runID, text string, runErr error) {
	persistCtx := context.WithoutCancel(ctx)
	runs, err := m.subRepo.ListSubAgentRuns(persistCtx, group.ID, "", maxSubAgentRuns)
	if err != nil {
		return
	}
	group = summarizeSubAgentGroup(group, runs)
	if runErr != nil {
		group.ErrorSummary = boundedSubAgentError(runErr)
	}
	if strings.TrimSpace(text) != "" {
		group.ResultDigest = digestSubAgentText(text)
	}
	group.UpdatedAt = time.Now().UTC()
	group.FinishedAt = group.UpdatedAt
	if err := m.subRepo.UpdateSubAgentGroup(persistCtx, group); err != nil {
		slog.Warn("通用子Agent group 收口失败", "group_id", group.ID, "run_id", runID, "error", err)
		return
	}
	m.emit(ctx, group.InvocationID, EventSubAgentGroupCompleted, map[string]any{"group_id": group.ID, "status": group.Status, "completed": group.CompletedCount, "failed": group.FailedCount, "cancelled": group.CancelledCount})
}

func genericSubAgentToolNames(values []tool.Tool) []string {
	result := make([]string, 0, len(values))
	for _, item := range values {
		if item != nil && strings.TrimSpace(item.Name()) != "" {
			result = append(result, item.Name())
		}
	}
	return result
}
