package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type retrievalToolArgs struct {
	Query         string   `json:"query" jsonschema:"要检索的问题或关键词"`
	SourceKinds   []string `json:"source_kinds" jsonschema:"检索来源：memory、knowledge、conversation、web、workspace、structured"`
	SourceIDs     []string `json:"source_ids,omitempty" jsonschema:"可选的来源或服务 ID"`
	Scope         []string `json:"scope,omitempty" jsonschema:"可选的已授权范围，例如会话或工作区 ID"`
	TopK          int      `json:"top_k,omitempty" jsonschema:"返回证据数量，最大 20"`
	RerankEnabled bool     `json:"rerank_enabled,omitempty" jsonschema:"是否按关键词进行有界重排"`
	Freshness     string   `json:"freshness,omitempty" jsonschema:"新鲜度约束"`
}

type retrievalToolResult struct {
	RequestID        string                   `json:"request_id"`
	Items            []EvidenceItem           `json:"items"`
	Failures         []RetrievalSourceFailure `json:"failures,omitempty"`
	Partial          bool                     `json:"partial"`
	QueryDigest      string                   `json:"query_digest"`
	PermissionDigest string                   `json:"permission_digest,omitempty"`
}

// newRetrievalTool 将统一检索编排器接入主 Agent。工具只返回有来源的 EvidenceItem，
// 不把适配器、凭据、原始网页响应或跨用户查询能力暴露给模型。
// NewRetrievalTool 创建一个带来源权限白名单的统一检索工具。允许列表为空时只
// 表示宿主尚未声明策略，调用仍会经过适配器自身的用户/会话边界校验。
func NewRetrievalTool(orchestrator *RetrievalOrchestrator, userID, conversationID, sessionID string, allowedKinds ...[]RetrievalSourceKind) (tool.Tool, error) {
	return newRetrievalTool(orchestrator, nil, "", userID, conversationID, sessionID, RuntimeOptions{}, allowedKinds...)
}

// NewManagedRetrievalTool 把每个独立来源放入统一 SubAgent Group/Run，
// 这样检索也拥有可取消、可追踪、可展示的生命周期；没有 Manager 时退回
// 同一个有界编排器的同步路径，方便最小嵌入环境使用。
func NewManagedRetrievalTool(orchestrator *RetrievalOrchestrator, manager SubAgentGroupRunner, invocationID, userID, conversationID, sessionID string, runtime RuntimeOptions, allowedKinds ...[]RetrievalSourceKind) (tool.Tool, error) {
	return newRetrievalTool(orchestrator, manager, invocationID, userID, conversationID, sessionID, runtime, allowedKinds...)
}

func newRetrievalTool(orchestrator *RetrievalOrchestrator, manager SubAgentGroupRunner, invocationID, userID, conversationID, sessionID string, runtime RuntimeOptions, allowedKinds ...[]RetrievalSourceKind) (tool.Tool, error) {
	if orchestrator == nil {
		return nil, &SubAgentRuntimeError{Code: "subagent_retrieval_adapter_unavailable", HumanMessage: "检索编排器不可用", Retryable: true}
	}
	allowed := map[RetrievalSourceKind]struct{}{}
	if len(allowedKinds) > 0 {
		for _, kind := range allowedKinds[0] {
			allowed[kind] = struct{}{}
		}
	}
	return functiontool.New(functiontool.Config{
		Name:        "retrieval_search",
		Description: "通过统一的只读检索编排器查询长期记忆、会话、网页、工作区或其他已授权来源。只把带来源的证据用于回答；没有证据时明确说明未检索到，不要把工具响应对象原样发送给用户。",
	}, func(ctx adkagent.Context, args retrievalToolArgs) (retrievalToolResult, error) {
		kinds := make([]RetrievalSourceKind, 0, len(args.SourceKinds))
		for _, value := range args.SourceKinds {
			value = strings.ToLower(strings.TrimSpace(value))
			if value != "" {
				kinds = append(kinds, RetrievalSourceKind(value))
			}
		}
		if len(allowed) > 0 {
			for _, kind := range kinds {
				if _, ok := allowed[kind]; !ok {
					return retrievalToolResult{}, &SubAgentRuntimeError{Code: "subagent_source_scope_denied", HumanMessage: "当前运行配置不允许该检索来源", Retryable: false}
				}
			}
		}
		topK := args.TopK
		if topK <= 0 {
			topK = 5
		}
		baseRequest := RetrievalRequest{
			Query: strings.TrimSpace(args.Query), InvocationID: strings.TrimSpace(invocationID), UserID: strings.TrimSpace(userID), ConversationID: strings.TrimSpace(conversationID), SessionID: strings.TrimSpace(sessionID),
			SourceKinds: kinds, SourceIDs: args.SourceIDs, Scope: args.Scope, TopK: topK, RerankEnabled: args.RerankEnabled, Freshness: args.Freshness,
			RequestedBy: "main_agent", MaxQueryCount: 24, MaxResultBytes: 128 << 10,
			WebSearchDailyCallLimit: runtime.WebSearchDailyCallLimit, WebSearchMaxCallsPerInvocation: runtime.WebSearchMaxCallsPerInvocation, WebSearchAlertPercent: runtime.WebSearchAlertPercent,
		}
		if manager != nil && strings.TrimSpace(invocationID) != "" && runtime.SubAgentsEnabled() {
			result, err := managedRetrieval(ctx, orchestrator, manager, invocationID, baseRequest, runtime)
			return result, err
		}
		result, err := orchestrator.Search(ctx, baseRequest)
		if err != nil && len(result.Items) == 0 {
			return retrievalToolResult{}, err
		}
		return retrievalToolResult{RequestID: result.RequestID, Items: result.Items, Failures: result.Failures, Partial: result.Partial, QueryDigest: result.QueryDigest, PermissionDigest: result.PermissionDigest}, err
	})
}

// managedRetrieval 为每个数据源创建一个只读 Run，再由 Group 结果屏障
// 聚合证据。来源失败不会吞掉其他来源，但所有失败都会保留在工具结果中。
func managedRetrieval(ctx context.Context, orchestrator *RetrievalOrchestrator, manager SubAgentGroupRunner, invocationID string, request RetrievalRequest, runtime RuntimeOptions) (retrievalToolResult, error) {
	runs := make([]SubAgentRunRequest, 0, len(request.SourceKinds))
	for index, sourceKind := range request.SourceKinds {
		kind := sourceKind
		runs = append(runs, SubAgentRunRequest{
			ID: fmt.Sprintf("source-%d-%s", index+1, kind), Prompt: fmt.Sprintf("只读检索来源 %s，返回带来源的 EvidenceItem，不执行任何副作用。", kind),
			Stage: "source", InputMetadata: map[string]string{"source_kind": string(kind)},
			Execute: func(runCtx context.Context, _ SubAgentRequest) (SubAgentExecutionResult, error) {
				sourceRequest := request
				sourceRequest.ParentRunID = request.ParentRunID
				sourceRequest.SourceKinds = []RetrievalSourceKind{kind}
				sourceRequest.RequestedBy = "retrieval_subagent"
				result, err := orchestrator.Search(runCtx, sourceRequest)
				if err != nil && len(result.Items) == 0 {
					return SubAgentExecutionResult{}, err
				}
				metadata := map[string]string{"source_kind": string(kind), "request_id": result.RequestID}
				if err != nil {
					// 保留“有证据但来源不完整”的 best-effort 结果；错误摘要进入
					// Run 元数据，不能因为一个来源的尾部失败丢掉已经取得的证据。
					metadata["failure_code"] = retrievalErrorCode(err)
					metadata["failure_message"] = limitSubAgentError(err.Error())
				}
				return SubAgentExecutionResult{
					Text:     fmt.Sprintf("来源 %s 返回 %d 条证据。", kind, len(result.Items)),
					Evidence: result.Items,
					Metadata: metadata,
				}, nil
			},
		})
	}
	group, err := manager.RunSubAgentGroup(ctx, SubAgentGroupRequest{
		Parent:  SubAgentRequest{InvocationID: strings.TrimSpace(invocationID), UserID: request.UserID, ConversationID: request.ConversationID, Runtime: runtime, Purpose: "多来源只读检索"},
		Profile: SubAgentProfileResearch, Purpose: "多来源只读检索", FailurePolicy: SubAgentFailureBestEffort,
		MaxConcurrency: runtime.SubAgent.MaxConcurrency, SourceScope: append([]string(nil), request.Scope...), QueryDigest: digestSubAgentText(request.Query), Runs: runs,
		Budget: SubAgentBudgetSnapshot{MaxChildren: len(runs), MaxConcurrency: runtime.SubAgent.MaxConcurrency, NetworkRequests: request.MaxQueryCount, ResultBytes: request.MaxResultBytes},
	})
	output := retrievalToolResult{RequestID: "subagent-group:" + group.Group.ID, Items: group.Evidence, Partial: len(group.Failures) > 0, QueryDigest: digestSubAgentText(request.Query), PermissionDigest: request.PermissionDigest}
	for _, failure := range group.Failures {
		output.Failures = append(output.Failures, RetrievalSourceFailure{SourceKind: failure.InputMetadata["source_kind"], Code: failure.ErrorCode, Message: failure.ErrorMessage, Retryable: failure.ErrorRetryable})
	}
	for _, run := range group.Runs {
		if code := strings.TrimSpace(run.ResultMetadata["failure_code"]); code != "" {
			output.Partial = true
			output.Failures = append(output.Failures, RetrievalSourceFailure{SourceKind: run.InputMetadata["source_kind"], Code: code, Message: run.ResultMetadata["failure_message"], Retryable: code == "subagent_retrieval_adapter_unavailable"})
		}
	}
	if err != nil && len(output.Items) == 0 {
		return output, err
	}
	if len(output.Items) == 0 {
		return output, &SubAgentRuntimeError{Code: "subagent_no_evidence", HumanMessage: "未检索到可用证据", Retryable: false, GroupID: group.Group.ID}
	}
	return output, nil
}

func retrievalErrorCode(err error) string {
	var runtimeErr *SubAgentRuntimeError
	if errors.As(err, &runtimeErr) && strings.TrimSpace(runtimeErr.Code) != "" {
		return runtimeErr.Code
	}
	return retrievalSourceFailure(RetrievalSourceKind("unknown"), err).Code
}
