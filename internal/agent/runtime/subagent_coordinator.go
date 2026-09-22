package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"Abot/internal/agent"
)

// SetRetrievalAdapter 将领域检索服务装配到通用 Runtime。适配器只负责只读检索，
// 子 Agent 的预算、证据边界和持久化仍由 Coordinator 统一管理。
func (c *Coordinator) SetRetrievalAdapter(adapter RetrievalAdapter) error {
	if c == nil || c.subagents == nil {
		return errors.New("子 Agent 管理器未装配")
	}
	return c.subagents.SetRetrievalAdapter(adapter)
}

// RunRetrieval 在应用侧需要主动检索时启动一次受限的多来源汇总。
func (c *Coordinator) RunRetrieval(ctx context.Context, request RetrievalRequest) (RetrievalSummary, error) {
	if c == nil || c.subagents == nil {
		return RetrievalSummary{}, errors.New("子 Agent 管理器未装配")
	}
	return c.subagents.RunRetrieval(ctx, request)
}

// RunBuiltInSubAgentGroup 在应用侧启动受限的通用文本子 Agent 组。执行只
// 使用 Kernel 已注册的内置 Provider，不开放工具、网络或第二套 Agent 服务。
func (c *Coordinator) RunBuiltInSubAgentGroup(ctx context.Context, request BuiltInSubAgentGroupRequest) ([]BuiltInSubAgentTaskResult, error) {
	if c == nil || c.subagents == nil {
		return nil, errors.New("子 Agent 管理器未装配")
	}
	request, err := c.prepareBuiltInSubAgentRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	return c.subagents.RunBuiltInSubAgentGroup(ctx, request)
}

// prepareBuiltInSubAgentRequest 让子 Agent 默认继承父 Invocation 的模型路由
// 和会话配置。显式传入不同路由时拒绝请求，避免子任务绕过父任务的配置快照。
func (c *Coordinator) prepareBuiltInSubAgentRequest(ctx context.Context, request BuiltInSubAgentGroupRequest) (BuiltInSubAgentGroupRequest, error) {
	if c == nil || c.kernel == nil {
		return request, errors.New("Agent Kernel 未装配")
	}
	request.InvocationID = strings.TrimSpace(request.InvocationID)
	if request.InvocationID != "" {
		invocation, err := c.repo.GetInvocation(ctx, request.InvocationID)
		if err != nil {
			return request, err
		}
		if request.UserID != "" && strings.TrimSpace(request.UserID) != strings.TrimSpace(invocation.UserID) {
			return request, fmt.Errorf("子 Agent 无权访问该 invocation")
		}
		if request.ConversationID == "" {
			request.ConversationID = invocation.ConversationID
		}
		if request.ProviderID != "" && invocation.ProviderID != "" && strings.TrimSpace(request.ProviderID) != strings.TrimSpace(invocation.ProviderID) {
			return request, fmt.Errorf("子 Agent 不能覆盖父 invocation 的 provider")
		}
		if request.ModelID != "" && invocation.ModelID != "" && strings.TrimSpace(request.ModelID) != strings.TrimSpace(invocation.ModelID) {
			return request, fmt.Errorf("子 Agent 不能覆盖父 invocation 的 model")
		}
		if request.ProviderID == "" {
			request.ProviderID = invocation.ProviderID
		}
		if request.ModelID == "" {
			request.ModelID = invocation.ModelID
		}
	}
	if !runtimeOptionsSpecified(request.Runtime) {
		resolved, err := c.kernel.ResolveRuntimeOptions(ctx, agent.ChatRequest{
			UserID: request.UserID, ConversationID: request.ConversationID, SessionID: request.ConversationID,
			ProviderID: request.ProviderID, ModelID: request.ModelID,
		})
		if err != nil {
			return request, fmt.Errorf("解析子 Agent 内置 AI 配置失败: %w", err)
		}
		request.Runtime = resolved
	}
	return request, nil
}

func runtimeOptionsSpecified(runtime agent.RuntimeOptions) bool {
	return runtime.AIEnabled || strings.TrimSpace(runtime.ProviderID) != "" || strings.TrimSpace(runtime.ModelID) != "" ||
		runtime.AIMaxOutputTokens != 0 || runtime.AITemperature != 0 || runtime.AITopP != 0 || strings.TrimSpace(runtime.AIReasoningEffort) != "" || len(runtime.SubAgentProfiles) > 0
}

// ListSubAgentGroups 返回指定 Invocation 的子 Agent 组，供管理台展示生命周期。
func (c *Coordinator) ListSubAgentGroups(ctx context.Context, invocationID string, status SubAgentGroupStatus, limit int) ([]SubAgentGroup, error) {
	if c == nil || c.subagents == nil {
		return nil, errors.New("子 Agent 管理器未装配")
	}
	return c.subagents.ListSubAgentGroups(ctx, invocationID, status, limit)
}

func (c *Coordinator) GetSubAgentGroup(ctx context.Context, groupID string) (SubAgentGroup, error) {
	if c == nil || c.subagents == nil {
		return SubAgentGroup{}, errors.New("子 Agent 管理器未装配")
	}
	return c.subagents.GetSubAgentGroup(ctx, groupID)
}

func (c *Coordinator) ListSubAgentRuns(ctx context.Context, groupID string, status SubAgentRunStatus, limit int) ([]SubAgentRun, error) {
	if c == nil || c.subagents == nil {
		return nil, errors.New("子 Agent 管理器未装配")
	}
	return c.subagents.ListSubAgentRuns(ctx, groupID, status, limit)
}

func (c *Coordinator) ListSubAgentEvidence(ctx context.Context, groupID string, limit int, after string) ([]EvidenceItem, error) {
	if c == nil || c.subagents == nil {
		return nil, errors.New("子 Agent 管理器未装配")
	}
	return c.subagents.ListSubAgentEvidence(ctx, groupID, limit, after)
}

// InvalidateRetrievalCache 清理会话、用户或工作区变更后不能继续复用的
// 进程内证据缓存。缓存本身不落盘，删除后不会影响源数据。
func (c *Coordinator) InvalidateRetrievalCache(userID, conversationID, workspaceID string) int {
	if c == nil || c.subagents == nil {
		return 0
	}
	return c.subagents.InvalidateRetrievalCache(userID, conversationID, workspaceID)
}

// PruneSubAgentRecords 清理超过保留期的终态子 Agent 记录，供应用维护循环调用。
func (c *Coordinator) PruneSubAgentRecords(ctx context.Context, before time.Time, limit int) (int, error) {
	if c == nil || c.subagents == nil {
		return 0, errors.New("子 Agent 管理器未装配")
	}
	return c.subagents.Prune(ctx, before, limit)
}
