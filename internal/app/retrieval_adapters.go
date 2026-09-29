package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"Abot/internal/agent"
	"Abot/internal/artifact"
	"Abot/internal/conversation"
	"Abot/internal/document"
	memorysvc "Abot/internal/memory"
	"Abot/internal/websearch"
	"Abot/internal/workspace"
)

// memoryRetrievalAdapter 把长期记忆服务投影为带来源的证据；用户隔离由已有
// memory.Service 再次执行，检索编排器不接受模型传入的任意 user_id。
type memoryRetrievalAdapter struct{ service *memorysvc.Service }

func (a memoryRetrievalAdapter) Kind() agent.RetrievalSourceKind { return agent.RetrievalSourceMemory }

func (a memoryRetrievalAdapter) ValidateAccess(ctx context.Context, request agent.RetrievalRequest) error {
	if a.service == nil || strings.TrimSpace(request.UserID) == "" {
		return errors.New("长期记忆检索范围无效")
	}
	_, err := a.service.List(ctx, request.UserID, "", 1)
	return err
}

func (a memoryRetrievalAdapter) Search(ctx context.Context, request agent.RetrievalRequest) ([]agent.EvidenceItem, error) {
	if a.service == nil {
		return nil, errors.New("长期记忆服务未装配")
	}
	items, err := a.service.List(ctx, request.UserID, request.Query, request.TopK)
	if err != nil {
		return nil, err
	}
	result := make([]agent.EvidenceItem, 0, len(items))
	for _, item := range items {
		result = append(result, agent.EvidenceItem{
			EvidenceID: "memory:" + item.ID, SourceKind: string(agent.RetrievalSourceMemory), SourceID: item.ID,
			SourceName: "长期记忆", Locator: item.ID, Title: item.Tags, Excerpt: item.Content,
			RetrievedAt: time.Now().UTC(), PublishedAt: item.UpdatedAt, TrustLevel: "user_owned",
			Citation: "长期记忆 " + item.ID,
			Metadata: map[string]string{"conversation_id": item.ConversationID, "source": item.Source},
		})
	}
	return result, nil
}

// conversationRetrievalAdapter 只允许当前用户读取指定会话，返回消息 ID 级别
// 的定位摘要；未指定会话时拒绝跨会话搜索，避免私聊和群聊混入。
type conversationRetrievalAdapter struct{ service *conversation.Service }

func (a conversationRetrievalAdapter) Kind() agent.RetrievalSourceKind {
	return agent.RetrievalSourceConversation
}

func (a conversationRetrievalAdapter) ValidateAccess(ctx context.Context, request agent.RetrievalRequest) error {
	if a.service == nil || strings.TrimSpace(request.UserID) == "" || strings.TrimSpace(request.ConversationID) == "" {
		return errors.New("对话检索权限范围无效")
	}
	_, err := a.service.Get(ctx, request.UserID, request.ConversationID)
	return err
}

func (a conversationRetrievalAdapter) Search(ctx context.Context, request agent.RetrievalRequest) ([]agent.EvidenceItem, error) {
	if a.service == nil {
		return nil, errors.New("对话服务未装配")
	}
	conversationID := strings.TrimSpace(request.ConversationID)
	if conversationID == "" {
		return nil, errors.New("会话检索必须指定 conversation_id")
	}
	messages, err := a.service.Messages(ctx, request.UserID, conversationID)
	if err != nil {
		return nil, err
	}
	result := make([]agent.EvidenceItem, 0, len(messages))
	for index, message := range messages {
		if strings.TrimSpace(message.Text) == "" {
			continue
		}
		if !retrievalTextMatches(request.Query, message.Text, message.Role) {
			continue
		}
		result = append(result, agent.EvidenceItem{
			EvidenceID: fmt.Sprintf("conversation:%s:%d", conversationID, index), SourceKind: string(agent.RetrievalSourceConversation), SourceID: conversationID,
			SourceName: "当前会话", Locator: fmt.Sprintf("message:%d", index), Title: message.Role, Excerpt: message.Text,
			RetrievedAt: time.Now().UTC(), PublishedAt: message.Timestamp, TrustLevel: "conversation_scope",
			Citation: fmt.Sprintf("当前会话消息 #%d", index+1),
			Metadata: map[string]string{"role": message.Role, "attachment_count": strconv.Itoa(message.AttachmentCount)},
		})
	}
	return result, nil
}

// webRetrievalAdapter 复用已接入的 Tavily/LangSearch 管理器，因此优先级、额度、
// 失败回退和日志仍由同一服务执行，不会为检索编排器另开供应商客户端。
type webRetrievalAdapter struct{ manager *websearch.Manager }

func (a webRetrievalAdapter) Kind() agent.RetrievalSourceKind { return agent.RetrievalSourceWeb }

func (a webRetrievalAdapter) ValidateAccess(_ context.Context, _ agent.RetrievalRequest) error {
	if a.manager == nil {
		return errors.New("网页搜索服务未装配")
	}
	return nil
}

func (a webRetrievalAdapter) Search(ctx context.Context, request agent.RetrievalRequest) ([]agent.EvidenceItem, error) {
	if a.manager == nil {
		return nil, errors.New("网页搜索服务未装配")
	}
	maxResults := request.TopK
	if maxResults <= 0 || maxResults > websearch.MaxSearchResults {
		maxResults = 5
	}
	dailyLimit, invocationLimit, alertPercent := request.WebSearchDailyCallLimit, request.WebSearchMaxCallsPerInvocation, request.WebSearchAlertPercent
	if dailyLimit <= 0 {
		dailyLimit = 100
	}
	if invocationLimit <= 0 {
		invocationLimit = 8
	}
	if alertPercent <= 0 {
		alertPercent = 80
	}
	response, err := a.manager.SearchWithBudget(ctx, request.SourceIDs, request.InvocationID, request.ConversationID, websearch.SearchRequest{Query: request.Query, MaxResults: maxResults, Freshness: request.Freshness}, websearch.Budget{DailyCallLimit: int64(dailyLimit), MaxCallsPerInvocation: invocationLimit, AlertPercent: alertPercent})
	if err != nil {
		return nil, err
	}
	result := make([]agent.EvidenceItem, 0, len(response.Results))
	for index, item := range response.Results {
		excerpt := strings.TrimSpace(item.Text)
		if excerpt == "" {
			excerpt = strings.TrimSpace(item.Snippet)
		}
		if excerpt == "" || strings.TrimSpace(item.URL) == "" {
			continue
		}
		result = append(result, agent.EvidenceItem{
			EvidenceID: fmt.Sprintf("web:%s:%d", response.ServiceID, index), SourceKind: string(agent.RetrievalSourceWeb), SourceID: response.ServiceID,
			SourceName: item.Title, Locator: item.URL, Title: item.Title, Excerpt: excerpt,
			RetrievalScore: item.Score, RetrievedAt: time.Now().UTC(), TrustLevel: "external_untrusted",
			Citation: item.Title + ": " + item.URL, Metadata: map[string]string{"provider": response.Provider, "request_id": response.RequestID},
		})
	}
	return result, nil
}

// workspaceRetrievalAdapter 把工作区已有的只读搜索投影为证据。workspace ID 必须
// 由已绑定的会话或受信 Runtime scope 提供，relative 只作为工作区内路径。
type workspaceRetrievalAdapter struct{ service *workspace.Service }

func (a workspaceRetrievalAdapter) Kind() agent.RetrievalSourceKind {
	return agent.RetrievalSourceWorkspace
}

func (a workspaceRetrievalAdapter) ValidateAccess(ctx context.Context, request agent.RetrievalRequest) error {
	if a.service == nil {
		return errors.New("工作区服务未装配")
	}
	workspaceID := ""
	if len(request.Scope) > 0 {
		workspaceID = strings.TrimSpace(request.Scope[0])
	}
	if workspaceID == "" && len(request.SourceIDs) > 0 {
		workspaceID = strings.TrimSpace(request.SourceIDs[0])
	}
	if workspaceID == "" {
		return errors.New("工作区检索缺少已授权 workspace scope")
	}
	_, err := a.service.Get(ctx, workspaceID)
	return err
}

// artifactDocumentRetrievalAdapter 只从显式授权的 ArtifactRef 读取结构化文件。
// 它不接受任意本地路径，也不把文件全文写入索引；每次查询重新解析有界副本，
// 因而会话删除或权限变化后不会留下可继续读取的隐藏正文。
type artifactDocumentRetrievalAdapter struct {
	service *artifact.Service
	kind    agent.RetrievalSourceKind
}

func (a artifactDocumentRetrievalAdapter) Kind() agent.RetrievalSourceKind { return a.kind }

func (a artifactDocumentRetrievalAdapter) ValidateAccess(ctx context.Context, request agent.RetrievalRequest) error {
	if a.service == nil || strings.TrimSpace(request.UserID) == "" || len(request.SourceIDs) == 0 {
		return &agent.SubAgentRuntimeError{Code: "subagent_artifact_scope_denied", HumanMessage: "Artifact 不属于当前授权范围", Retryable: false}
	}
	for _, sourceID := range request.SourceIDs {
		item, err := a.service.Get(ctx, request.UserID, strings.TrimSpace(sourceID))
		if err != nil {
			return &agent.SubAgentRuntimeError{Code: "subagent_artifact_scope_denied", HumanMessage: "Artifact 不属于当前授权范围", Retryable: false, Cause: err}
		}
		if item.Status != artifact.StatusReady {
			return &agent.SubAgentRuntimeError{Code: "subagent_artifact_scope_denied", HumanMessage: "Artifact 当前不可读取", Retryable: false}
		}
		if request.ConversationID != "" && strings.TrimSpace(item.ConversationID) != request.ConversationID {
			return &agent.SubAgentRuntimeError{Code: "subagent_artifact_scope_denied", HumanMessage: "Artifact 不属于当前会话", Retryable: false}
		}
	}
	return nil
}

func (a artifactDocumentRetrievalAdapter) Search(ctx context.Context, request agent.RetrievalRequest) ([]agent.EvidenceItem, error) {
	if a.service == nil {
		return nil, errors.New("Artifact 文档服务未装配")
	}
	if len(request.SourceIDs) == 0 || len(request.SourceIDs) > 8 {
		return nil, errors.New("结构化检索必须指定 1 至 8 个 Artifact ID")
	}
	result := make([]agent.EvidenceItem, 0)
	for _, sourceID := range request.SourceIDs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		sourceID = strings.TrimSpace(sourceID)
		if sourceID == "" {
			continue
		}
		reader, _, item, err := a.service.Open(ctx, request.UserID, sourceID, artifact.ByteRange{})
		if err != nil {
			if errors.Is(err, artifact.ErrForbidden) || errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrExpired) || errors.Is(err, artifact.ErrQuarantined) {
				// 不向检索调用方泄露 Artifact 是否存在；权限、过期和不存在
				// 都统一成不可恢复的范围拒绝。
				return result, &agent.SubAgentRuntimeError{Code: "subagent_artifact_scope_denied", HumanMessage: "Artifact 不属于当前授权范围", Retryable: false, Cause: err}
			}
			return result, &agent.SubAgentRuntimeError{Code: "subagent_retrieval_adapter_unavailable", HumanMessage: "Artifact 当前不可读取", Retryable: true, Cause: err}
		}
		if request.ConversationID != "" && strings.TrimSpace(item.ConversationID) != "" && item.ConversationID != request.ConversationID {
			_ = reader.Close()
			return result, &agent.SubAgentRuntimeError{Code: "subagent_artifact_scope_denied", HumanMessage: "Artifact 不属于当前会话", Retryable: false}
		}
		const maxDocumentBytes = 10 << 20
		if item.Size > maxDocumentBytes {
			_ = reader.Close()
			return result, fmt.Errorf("Artifact %s 超过结构化检索大小上限", sourceID)
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxDocumentBytes+1))
		_ = reader.Close()
		if readErr != nil {
			return result, readErr
		}
		if int64(len(data)) > maxDocumentBytes {
			return result, fmt.Errorf("Artifact %s 读取结果超过结构化检索大小上限", sourceID)
		}
		parsed, parseErr := document.Parse(ctx, item.Name, item.MIMEType, data)
		if parseErr != nil {
			return result, fmt.Errorf("解析 Artifact %s 失败: %w", sourceID, parseErr)
		}
		artifactResultStart := len(result)
		for index, block := range parsed.Blocks {
			if index >= 64 || strings.TrimSpace(block.Text) == "" {
				break
			}
			if !retrievalTextMatches(request.Query, block.Text, item.Name) {
				continue
			}
			result = append(result, agent.EvidenceItem{
				EvidenceID: fmt.Sprintf("%s:%s:%d", a.kind, sourceID, index), SourceKind: string(a.kind), SourceID: sourceID,
				SourceName: item.Name, Locator: block.Locator, Title: item.Name, Excerpt: block.Text,
				RetrievedAt: time.Now().UTC(), PublishedAt: item.CreatedAt, TrustLevel: "artifact_scope",
				Citation: fmt.Sprintf("%s（%s）", item.Name, block.Locator),
				Metadata: map[string]string{"artifact_id": item.ID, "artifact_digest": item.Digest, "conversation_id": item.ConversationID},
			})
		}
		// 每个 Artifact 单独判断是否已有块结果，不能因为前一个文件命中
		// 就抑制当前文件的摘要证据。
		if len(result) == artifactResultStart {
			rendered, _ := parsed.Render(request.Query, document.DefaultRenderBytes)
			if strings.TrimSpace(rendered) != "" && retrievalTextMatches(request.Query, rendered, item.Name) {
				result = append(result, agent.EvidenceItem{
					EvidenceID: fmt.Sprintf("%s:%s:summary", a.kind, sourceID), SourceKind: string(a.kind), SourceID: sourceID,
					SourceName: item.Name, Locator: "document-summary", Title: item.Name, Excerpt: rendered,
					RetrievedAt: time.Now().UTC(), PublishedAt: item.CreatedAt, TrustLevel: "artifact_scope",
					Citation: item.Name, Metadata: map[string]string{"artifact_id": item.ID, "artifact_digest": item.Digest},
				})
			}
		}
	}
	return result, nil
}

// retrievalTextMatches 是来源适配器的第二层相关性过滤。长期记忆服务已经
// 自带查询能力，但会话和 Artifact 是本地有界扫描，不能把整段历史无条件交给
// 编排器；同时保留整句匹配，兼容中文等没有空格分词的查询。
func retrievalTextMatches(query string, values ...string) bool {
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" {
		return true
	}
	text := strings.ToLower(strings.Join(values, " "))
	if strings.Contains(text, query) {
		return true
	}
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		if term != "" && strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func (a workspaceRetrievalAdapter) Search(ctx context.Context, request agent.RetrievalRequest) ([]agent.EvidenceItem, error) {
	if a.service == nil {
		return nil, errors.New("工作区服务未装配")
	}
	workspaceID := ""
	if len(request.Scope) > 0 {
		workspaceID = strings.TrimSpace(request.Scope[0])
	}
	if workspaceID == "" && len(request.SourceIDs) > 0 {
		workspaceID = strings.TrimSpace(request.SourceIDs[0])
	}
	if workspaceID == "" {
		return nil, errors.New("工作区检索缺少已授权 workspace scope")
	}
	relative := ""
	if request.Filters != nil {
		relative = strings.TrimSpace(request.Filters["relative"])
	}
	matches, err := a.service.Search(ctx, workspaceID, relative, request.Query)
	if err != nil {
		return nil, err
	}
	result := make([]agent.EvidenceItem, 0, len(matches))
	for index, item := range matches {
		result = append(result, agent.EvidenceItem{
			EvidenceID: fmt.Sprintf("workspace:%s:%d:%d", workspaceID, item.Line, index), SourceKind: string(agent.RetrievalSourceWorkspace), SourceID: workspaceID,
			SourceName: workspaceID, Locator: fmt.Sprintf("%s:%d", item.Path, item.Line), Title: item.Path, Excerpt: item.Preview,
			RetrievedAt: time.Now().UTC(), TrustLevel: "workspace_scope", Citation: fmt.Sprintf("%s:%d", item.Path, item.Line),
		})
	}
	return result, nil
}
