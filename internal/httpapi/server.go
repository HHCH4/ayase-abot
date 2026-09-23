package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/artifact"
	"Abot/internal/bot"
	configsvc "Abot/internal/config"
	"Abot/internal/conversation"
	"Abot/internal/logging"
	memorysvc "Abot/internal/memory"
	"Abot/internal/provider"
	"Abot/internal/schedule"
	"Abot/internal/sessionrule"
	"Abot/internal/websearch"
	"Abot/internal/webui"
	"Abot/internal/workspace"
	"google.golang.org/adk/v2/session"
)

// Server 是独立于 WebUI 的 REST/SSE API 服务。
type Server struct {
	providers                  *provider.Registry
	kernel                     *agent.Kernel
	bots                       *bot.Manager
	sourceRegistry             bot.MessageSourceRegistry
	workspaces                 *workspace.Service
	remoteTargets              *workspace.RemoteTargetService
	conversations              *conversation.Service
	config                     *configsvc.Service
	personas                   *configsvc.PersonaService
	memories                   *memorysvc.Service
	artifacts                  *artifact.Service
	sessionRules               *sessionrule.Service
	schedules                  *schedule.Service
	webSearch                  *websearch.Manager
	logs                       *logging.Store
	runtime                    *agentruntime.Coordinator
	toolRegistry               *agent.ToolRegistry
	eventDelivery              http.Handler
	eventStatus                http.Handler
	eventPrepare               http.Handler
	eventCommit                http.Handler
	eventAbort                 http.Handler
	eventSource                http.Handler
	checkpointDelivery         http.Handler
	checkpointStatus           http.Handler
	checkpointSource           http.Handler
	checkpointPrepare          http.Handler
	checkpointCommit           http.Handler
	checkpointAbort            http.Handler
	configDelivery             http.Handler
	configStatus               http.Handler
	configPrepare              http.Handler
	configCommit               http.Handler
	configAbort                http.Handler
	configDirectory            http.Handler
	configDirectoryStatus      http.Handler
	configDirectoryMaterialize http.Handler
	rebindApply                http.Handler
	rebindApplyStatus          http.Handler
	rejectionDelivery          http.Handler
	rejectionStatus            http.Handler
	rejectionPrepare           http.Handler
	rejectionCommit            http.Handler
	rejectionAbort             http.Handler
	deliveryGroupStatus        http.Handler
	deliveryGroupStatusBatch   http.Handler
	deliveryGroupPrepare       http.Handler
	deliveryGroupCommit        http.Handler
	deliveryGroupAbort         http.Handler
	toolStatus                 http.Handler
}

// NewServer 创建 API 服务；静态页面通过同一个 Handler 挂载，因此生产环境无需 CORS。
func NewServer(providers *provider.Registry, kernel *agent.Kernel, workspaceServices ...*workspace.Service) *Server {
	server := &Server{providers: providers, kernel: kernel}
	if len(workspaceServices) > 0 {
		server.workspaces = workspaceServices[0]
	}
	return server
}

// NewServerWithServices 创建完整 API 服务，包含工作区、对话和机器人生命周期管理。
// bots 使用可选参数以保持已有测试和嵌入方的构造方式兼容。
func NewServerWithServices(providers *provider.Registry, kernel *agent.Kernel, workspaceService *workspace.Service, conversationService *conversation.Service, botServices ...*bot.Manager) *Server {
	server := &Server{providers: providers, kernel: kernel, workspaces: workspaceService, conversations: conversationService}
	if len(botServices) > 0 {
		server.bots = botServices[0]
	}
	return server
}

// SetConfigService 装配配置中心；单独提供设置器以保持已有嵌入方的构造函数兼容。
func (s *Server) SetConfigService(service *configsvc.Service) {
	s.config = service
}

// SetPersonaService 装配独立人格目录和选择服务。
func (s *Server) SetPersonaService(service *configsvc.PersonaService) {
	s.personas = service
}

// SetRemoteTargetService 装配可复用远程主机服务，保留旧版构造函数兼容性。
func (s *Server) SetRemoteTargetService(service *workspace.RemoteTargetService) {
	s.remoteTargets = service
}

// SetMemoryService 装配长期记忆管理和 Agent 使用的同一份服务。
func (s *Server) SetMemoryService(service *memorysvc.Service) {
	s.memories = service
}

// SetArtifactService 装配大内容的内容寻址存储和元数据访问服务。
func (s *Server) SetArtifactService(service *artifact.Service) {
	s.artifacts = service
}

// SetSessionRuleService 装配按消息会话来源覆盖配置的规则服务。
func (s *Server) SetSessionRuleService(service *sessionrule.Service) {
	s.sessionRules = service
}

// SetMessageSourceRegistry 装配独立的 UMO 来源目录；它记录所有入站消息，
// 不要求消息已经创建对话或进入内置 AI。
func (s *Server) SetMessageSourceRegistry(registry bot.MessageSourceRegistry) {
	s.sourceRegistry = registry
}

// SetScheduleService 装配未来任务和本地调度服务。
func (s *Server) SetScheduleService(service *schedule.Service) {
	s.schedules = service
}

// SetWebSearchManager 设置网页搜索管理器；管理接口不会返回密钥明文。
func (s *Server) SetWebSearchManager(manager *websearch.Manager) {
	s.webSearch = manager
}

// SetLogStore 装配管理台日志的有界内存快照。
func (s *Server) SetLogStore(store *logging.Store) {
	s.logs = store
}

// SetRuntimeCoordinator 装配独立于 HTTP 请求生命周期的 Agent Runtime。
func (s *Server) SetRuntimeCoordinator(coordinator *agentruntime.Coordinator) {
	s.runtime = coordinator
}

// SetToolRegistry exposes the public, metadata-only catalog used by the
// Runtime. Tool implementations and credentials never leave the process.
func (s *Server) SetToolRegistry(registry *agent.ToolRegistry) {
	s.toolRegistry = registry
}

// SetRuntimeEventDeliveryReceiver mounts an explicitly configured, signed
// event receiver. It is opt-in so an API server never exposes an unauthenticated
// cross-service ingestion endpoint by default.
func (s *Server) SetRuntimeEventDeliveryReceiver(receiver *agentruntime.RuntimeEventDeliveryReceiver) {
	if receiver == nil {
		s.eventDelivery = nil
		s.eventStatus = nil
		return
	}
	s.eventDelivery = receiver.Handler()
	s.eventStatus = receiver.StatusHandler()
}

// SetRuntimeEventDeliveryTransactionReceiver mounts the optional
// authenticated prepare/commit endpoints. They are separate from the legacy
// one-phase receiver so an existing transport cannot silently change its
// delivery semantics.
func (s *Server) SetRuntimeEventDeliveryTransactionReceiver(receiver *agentruntime.RuntimeEventDeliveryReceiver) {
	if receiver == nil {
		s.eventPrepare = nil
		s.eventCommit = nil
		s.eventAbort = nil
		return
	}
	s.eventPrepare = receiver.PrepareHandler()
	s.eventCommit = receiver.CommitHandler()
	s.eventAbort = receiver.AbortHandler()
}

// SetRuntimeEventDeliverySource mounts an explicitly configured, signed
// source lookup endpoint. It is opt-in: event bodies remain local unless the
// host deliberately exposes this route to an authenticated destination.
func (s *Server) SetRuntimeEventDeliverySource(source *agentruntime.RuntimeEventDeliverySource) {
	if source == nil {
		s.eventSource = nil
		return
	}
	s.eventSource = source.Handler()
}

// SetRuntimeCheckpointDeliveryReceiver mounts an explicitly configured,
// authenticated metadata-only checkpoint receiver. It is opt-in so a normal
// API server never accepts cross-service recovery state by default.
func (s *Server) SetRuntimeCheckpointDeliveryReceiver(receiver *agentruntime.RuntimeCheckpointDeliveryReceiver) {
	if receiver == nil {
		s.checkpointDelivery = nil
		s.checkpointStatus = nil
		return
	}
	s.checkpointDelivery = receiver.Handler()
	s.checkpointStatus = receiver.StatusHandler()
}

// SetRuntimeCheckpointDeliveryTransactionReceiver mounts the optional
// authenticated prepare/commit endpoints. They are separate from the legacy
// one-phase receiver so a deployment cannot accidentally upgrade its delivery
// semantics without explicitly opting in.
func (s *Server) SetRuntimeCheckpointDeliveryTransactionReceiver(receiver *agentruntime.RuntimeCheckpointDeliveryReceiver) {
	if receiver == nil {
		s.checkpointPrepare = nil
		s.checkpointCommit = nil
		s.checkpointAbort = nil
		return
	}
	s.checkpointPrepare = receiver.PrepareHandler()
	s.checkpointCommit = receiver.CommitHandler()
	s.checkpointAbort = receiver.AbortHandler()
}

// SetRuntimeCheckpointDeliverySource mounts an explicitly configured source
// for exact metadata-only checkpoint revisions. It is separate from the event
// source route because a checkpoint carries a different signed contract.
func (s *Server) SetRuntimeCheckpointDeliverySource(source *agentruntime.RuntimeCheckpointDeliverySource) {
	if source == nil {
		s.checkpointSource = nil
		return
	}
	s.checkpointSource = source.Handler()
}

// SetRuntimeConfigDeliveryReceiver mounts the metadata-only cross-Runtime
// configuration lock receiver. It is opt-in and remains absent until the host
// supplies a route-bound, HMAC-authenticated receiver.
func (s *Server) SetRuntimeConfigDeliveryReceiver(receiver *agentruntime.RuntimeConfigDeliveryReceiver) {
	if receiver == nil {
		s.configDelivery = nil
		s.configStatus = nil
		return
	}
	s.configDelivery = receiver.Handler()
	s.configStatus = receiver.StatusHandler()
}

// SetRuntimeConfigDeliveryTransactionReceiver mounts the optional prepare and
// commit endpoints for the configuration lock two-phase protocol.
func (s *Server) SetRuntimeConfigDeliveryTransactionReceiver(receiver *agentruntime.RuntimeConfigDeliveryReceiver) {
	if receiver == nil {
		s.configPrepare = nil
		s.configCommit = nil
		s.configAbort = nil
		return
	}
	s.configPrepare = receiver.PrepareHandler()
	s.configCommit = receiver.CommitHandler()
	s.configAbort = receiver.AbortHandler()
}

// SetRuntimeConfigDirectoryReceiver mounts the opt-in, HMAC-authenticated
// configuration-directory body receiver. It is separate from the metadata-only
// config-delivery lock route and never changes defaults or bindings.
func (s *Server) SetRuntimeConfigDirectoryReceiver(receiver *agentruntime.RuntimeConfigDirectoryReceiver) {
	if receiver == nil {
		s.configDirectory = nil
		s.configDirectoryStatus = nil
		s.configDirectoryMaterialize = nil
		return
	}
	s.configDirectory = receiver.Handler()
	s.configDirectoryStatus = receiver.StatusHandler()
	s.configDirectoryMaterialize = receiver.MaterializeHandler()
}

// SetRuntimeConfigDirectoryRebindApplyReceiver mounts the explicit,
// receipt-backed single-target rebind apply endpoint. It is opt-in and only
// persists the bounded checkpoint projection in the supplied inbox.
func (s *Server) SetRuntimeConfigDirectoryRebindApplyReceiver(receiver *agentruntime.RuntimeConfigDirectoryRebindApplyReceiver) {
	if receiver == nil {
		s.rebindApply = nil
		s.rebindApplyStatus = nil
		return
	}
	s.rebindApply = receiver.Handler()
	s.rebindApplyStatus = receiver.StatusHandler()
}

// SetRuntimeApprovalRejectionDeliveryReceiver mounts the opt-in,
// metadata-only cross-service rejection receiver. It is never exposed unless
// the host explicitly supplies an authenticated receiver.
func (s *Server) SetRuntimeApprovalRejectionDeliveryReceiver(receiver *agentruntime.RuntimeApprovalRejectionDeliveryReceiver) {
	if receiver == nil {
		s.rejectionDelivery = nil
		s.rejectionStatus = nil
		return
	}
	s.rejectionDelivery = receiver.Handler()
	s.rejectionStatus = receiver.StatusHandler()
}

// SetRuntimeApprovalRejectionDeliveryTransactionReceiver mounts the explicit
// prepare/commit endpoints for the rejection-intent protocol.
func (s *Server) SetRuntimeApprovalRejectionDeliveryTransactionReceiver(receiver *agentruntime.RuntimeApprovalRejectionDeliveryReceiver) {
	if receiver == nil {
		s.rejectionPrepare = nil
		s.rejectionCommit = nil
		s.rejectionAbort = nil
		return
	}
	s.rejectionPrepare = receiver.PrepareHandler()
	s.rejectionCommit = receiver.CommitHandler()
	s.rejectionAbort = receiver.AbortHandler()
}

// SetRuntimeDeliveryGroupTransactionReceiver mounts the opt-in, signed
// metadata-only group settlement endpoints. It records only group/member
// identities; family payloads continue to use their own delivery routes.
func (s *Server) SetRuntimeDeliveryGroupTransactionReceiver(receiver *agentruntime.RuntimeDeliveryGroupReceiver) {
	if receiver == nil {
		s.deliveryGroupStatus = nil
		s.deliveryGroupStatusBatch = nil
		s.deliveryGroupPrepare = nil
		s.deliveryGroupCommit = nil
		s.deliveryGroupAbort = nil
		return
	}
	s.deliveryGroupStatus = receiver.StatusHandler()
	s.deliveryGroupStatusBatch = receiver.BatchStatusHandler()
	s.deliveryGroupPrepare = receiver.PrepareHandler()
	s.deliveryGroupCommit = receiver.CommitHandler()
	s.deliveryGroupAbort = receiver.AbortHandler()
}

// SetRuntimeLongRunningToolStatusReceiver mounts the explicit, authenticated
// status endpoint used by an external Tool Runtime. It is opt-in and never
// executes or resumes a tool.
func (s *Server) SetRuntimeLongRunningToolStatusReceiver(receiver *agentruntime.RuntimeLongRunningToolStatusReceiver) {
	if receiver == nil {
		s.toolStatus = nil
		return
	}
	s.toolStatus = receiver.Handler()
}

// Handler 注册供应商、机器人、工作区、聊天、会话和内嵌 WebUI 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/healthz", s.health)
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/providers", s.listProviders)
	mux.HandleFunc("GET /api/v1/providers/capability-profiles", s.listProviderCapabilityProfiles)
	mux.HandleFunc("POST /api/v1/providers", s.createProvider)
	mux.HandleFunc("POST /api/v1/providers/capability-gate", s.evaluateProviderCapabilityGate)
	mux.HandleFunc("GET /api/v1/providers/{id}", s.getProvider)
	mux.HandleFunc("GET /api/v1/providers/{id}/models/{model}/capabilities", s.getProviderModelCapabilities)
	mux.HandleFunc("GET /api/v1/providers/{id}/models/{model}/capability-observations", s.getProviderModelCapabilityObservations)
	mux.HandleFunc("POST /api/v1/providers/{id}/models/{model}/probe", s.probeProviderModelCapabilities)
	mux.HandleFunc("PUT /api/v1/providers/{id}/models/{model}/capability-overrides", s.updateProviderModelCapabilityOverrides)
	mux.HandleFunc("GET /api/v1/tools", s.listTools)
	mux.HandleFunc("GET /api/v1/tools/{id}", s.getTool)
	mux.HandleFunc("PUT /api/v1/providers/{id}", s.updateProvider)
	mux.HandleFunc("DELETE /api/v1/providers/{id}", s.deleteProvider)
	mux.HandleFunc("POST /api/v1/providers/{id}/test", s.testProvider)
	mux.HandleFunc("POST /api/v1/providers/{id}/models/discover", s.discoverModels)
	mux.HandleFunc("POST /api/v1/providers/preview/models/discover", s.previewDiscoverModels)
	mux.HandleFunc("GET /api/v1/bot-types", s.listBotTypes)
	mux.HandleFunc("GET /api/v1/bots", s.listBots)
	mux.HandleFunc("POST /api/v1/bots", s.createBot)
	mux.HandleFunc("GET /api/v1/bots/{id}", s.getBot)
	mux.HandleFunc("PUT /api/v1/bots/{id}", s.updateBot)
	mux.HandleFunc("DELETE /api/v1/bots/{id}", s.deleteBot)
	mux.HandleFunc("POST /api/v1/bots/preview/test", s.previewBotTest)
	mux.HandleFunc("POST /api/v1/bots/{id}/test", s.testBot)
	mux.HandleFunc("POST /api/v1/bots/{id}/start", s.startBot)
	mux.HandleFunc("POST /api/v1/bots/{id}/stop", s.stopBot)
	mux.HandleFunc("POST /api/v1/bots/{id}/restart", s.restartBot)
	mux.HandleFunc("GET /api/v1/settings", s.getSettings)
	mux.HandleFunc("PUT /api/v1/settings/default-model", s.setDefaultModel)
	mux.HandleFunc("GET /api/v1/config/schema", s.configSchema)
	mux.HandleFunc("GET /api/v1/personas", s.listPersonas)
	mux.HandleFunc("POST /api/v1/personas", s.createPersona)
	mux.HandleFunc("GET /api/v1/personas/{id}", s.getPersona)
	mux.HandleFunc("GET /api/v1/personas/{id}/revisions", s.listPersonaRevisions)
	mux.HandleFunc("GET /api/v1/personas/{id}/export", s.exportPersona)
	mux.HandleFunc("PUT /api/v1/personas/{id}", s.updatePersona)
	mux.HandleFunc("DELETE /api/v1/personas/{id}", s.deletePersona)
	mux.HandleFunc("POST /api/v1/personas/{id}/default", s.setDefaultPersona)
	mux.HandleFunc("POST /api/v1/personas/import", s.importPersona)
	mux.HandleFunc("GET /api/v1/config-profiles", s.listConfigProfiles)
	mux.HandleFunc("POST /api/v1/config-profiles", s.createConfigProfile)
	mux.HandleFunc("POST /api/v1/config-profiles/import", s.importConfigProfile)
	mux.HandleFunc("GET /api/v1/config-profiles/{id}", s.getConfigProfile)
	mux.HandleFunc("GET /api/v1/config-profiles/{id}/revisions", s.listConfigRevisions)
	mux.HandleFunc("PUT /api/v1/config-profiles/{id}", s.updateConfigProfile)
	mux.HandleFunc("DELETE /api/v1/config-profiles/{id}", s.deleteConfigProfile)
	mux.HandleFunc("POST /api/v1/config-profiles/{id}/validate", s.validateConfigProfile)
	mux.HandleFunc("GET /api/v1/config-profiles/{id}/export", s.exportConfigProfile)
	mux.HandleFunc("POST /api/v1/config-profiles/{id}/copy", s.copyConfigProfile)
	mux.HandleFunc("POST /api/v1/config-profiles/{id}/default", s.setDefaultConfigProfile)
	mux.HandleFunc("GET /api/v1/system-settings", s.getSystemSettings)
	mux.HandleFunc("PUT /api/v1/system-settings", s.setSystemSettings)
	mux.HandleFunc("GET /api/v1/web-search/services", s.listWebSearchServices)
	mux.HandleFunc("POST /api/v1/web-search/services", s.saveWebSearchService)
	mux.HandleFunc("PUT /api/v1/web-search/services/{id}", s.saveWebSearchService)
	mux.HandleFunc("DELETE /api/v1/web-search/services/{id}", s.deleteWebSearchService)
	mux.HandleFunc("POST /api/v1/web-search/services/{id}/test", s.testWebSearchService)
	mux.HandleFunc("GET /api/v1/web-search/usage", s.getWebSearchUsage)
	mux.HandleFunc("GET /api/v1/memories", s.listMemories)
	mux.HandleFunc("POST /api/v1/memories", s.createMemory)
	mux.HandleFunc("DELETE /api/v1/memories", s.clearMemories)
	mux.HandleFunc("GET /api/v1/memories/{id}", s.getMemory)
	mux.HandleFunc("PUT /api/v1/memories/{id}", s.updateMemory)
	mux.HandleFunc("DELETE /api/v1/memories/{id}", s.deleteMemory)
	mux.HandleFunc("PUT /api/v1/bots/{id}/config-profile", s.bindBotConfigProfile)
	mux.HandleFunc("GET /api/v1/bots/{id}/config-profile", s.getBotConfigProfile)
	mux.HandleFunc("PUT /api/v1/conversations/{id}/config-profile", s.bindConversationConfigProfile)
	mux.HandleFunc("GET /api/v1/conversations/{id}/config-profile", s.getConversationConfigProfile)
	mux.HandleFunc("PUT /api/v1/bots/{id}/persona", s.bindBotPersona)
	mux.HandleFunc("GET /api/v1/bots/{id}/persona", s.getBotPersona)
	mux.HandleFunc("GET /api/v1/bots/{id}/commands", s.listBotCommands)
	mux.HandleFunc("PUT /api/v1/bots/{id}/commands/{commandID}", s.updateBotCommandPolicy)
	mux.HandleFunc("GET /api/v1/bots/{id}/commands/audit", s.listBotCommandAudits)
	mux.HandleFunc("GET /api/v1/bots/{id}/group-admins", s.listBotGroupAdmins)
	mux.HandleFunc("POST /api/v1/bots/{id}/group-admins", s.addBotGroupAdmin)
	mux.HandleFunc("DELETE /api/v1/bots/{id}/group-admins/{userID}", s.removeBotGroupAdmin)
	mux.HandleFunc("PUT /api/v1/conversations/{id}/persona", s.bindConversationPersona)
	mux.HandleFunc("GET /api/v1/conversations/{id}/persona", s.getConversationPersona)
	mux.HandleFunc("GET /api/v1/session-rules", s.listSessionRules)
	mux.HandleFunc("GET /api/v1/session-sources", s.listSessionSources)
	mux.HandleFunc("POST /api/v1/session-rules", s.createSessionRule)
	mux.HandleFunc("POST /api/v1/session-rules/batch", s.batchSessionRules)
	mux.HandleFunc("GET /api/v1/session-rules/{source}", s.getSessionRule)
	mux.HandleFunc("PUT /api/v1/session-rules/{source}", s.updateSessionRule)
	mux.HandleFunc("POST /api/v1/session-rules/{source}/reset", s.resetSessionRuleField)
	mux.HandleFunc("DELETE /api/v1/session-rules/{source}", s.deleteSessionRule)
	mux.HandleFunc("GET /api/v1/session-rule-groups", s.listSessionRuleGroups)
	mux.HandleFunc("POST /api/v1/session-rule-groups", s.createSessionRuleGroup)
	mux.HandleFunc("PUT /api/v1/session-rule-groups/{id}", s.updateSessionRuleGroup)
	mux.HandleFunc("DELETE /api/v1/session-rule-groups/{id}", s.deleteSessionRuleGroup)
	mux.HandleFunc("GET /api/v1/scheduled-tasks", s.listScheduledTasks)
	mux.HandleFunc("POST /api/v1/scheduled-tasks", s.createScheduledTask)
	mux.HandleFunc("GET /api/v1/scheduled-tasks/{id}", s.getScheduledTask)
	mux.HandleFunc("PUT /api/v1/scheduled-tasks/{id}", s.updateScheduledTask)
	mux.HandleFunc("DELETE /api/v1/scheduled-tasks/{id}", s.deleteScheduledTask)
	mux.HandleFunc("POST /api/v1/scheduled-tasks/{id}/pause", s.pauseScheduledTask)
	mux.HandleFunc("POST /api/v1/scheduled-tasks/{id}/resume", s.resumeScheduledTask)
	mux.HandleFunc("GET /api/v1/data/stats", s.dataStats)
	mux.HandleFunc("GET /api/v1/data/conversations", s.dataConversations)
	mux.HandleFunc("GET /api/v1/data/traces", s.dataTraces)
	mux.HandleFunc("GET /api/v1/data/logs", s.dataLogs)
	mux.HandleFunc("GET /api/v1/data/logs/stream", s.dataLogsStream)
	mux.HandleFunc("GET /api/v1/local/directories", s.listLocalDirectories)
	mux.HandleFunc("GET /api/v1/remote-targets", s.listRemoteTargets)
	mux.HandleFunc("POST /api/v1/remote-targets", s.createRemoteTarget)
	mux.HandleFunc("POST /api/v1/remote-targets/preview/test", s.previewTestRemoteTarget)
	mux.HandleFunc("GET /api/v1/remote-targets/{id}", s.getRemoteTarget)
	mux.HandleFunc("PUT /api/v1/remote-targets/{id}", s.updateRemoteTarget)
	mux.HandleFunc("DELETE /api/v1/remote-targets/{id}", s.deleteRemoteTarget)
	mux.HandleFunc("POST /api/v1/remote-targets/{id}/test", s.testRemoteTarget)
	mux.HandleFunc("GET /api/v1/remote-targets/{id}/directories", s.listRemoteDirectories)
	mux.HandleFunc("GET /api/v1/workspaces", s.listWorkspaces)
	mux.HandleFunc("POST /api/v1/workspaces", s.createWorkspace)
	mux.HandleFunc("POST /api/v1/workspaces/preview/test", s.previewTestWorkspace)
	// 保留方案中的 validate 别名，客户端可以用更明确的语义调用创建前校验。
	mux.HandleFunc("POST /api/v1/workspaces/preview/validate", s.previewTestWorkspace)
	mux.HandleFunc("GET /api/v1/workspaces/{id}", s.getWorkspace)
	mux.HandleFunc("PUT /api/v1/workspaces/{id}", s.updateWorkspace)
	mux.HandleFunc("DELETE /api/v1/workspaces/{id}", s.deleteWorkspace)
	mux.HandleFunc("POST /api/v1/workspaces/{id}/test", s.testWorkspace)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/files", s.listWorkspaceFiles)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/file", s.readWorkspaceFile)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/file-range", s.readWorkspaceRange)
	mux.HandleFunc("POST /api/v1/workspaces/{id}/files/read-many", s.readWorkspaceMany)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/glob", s.globWorkspace)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/search", s.searchWorkspace)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/stat", s.workspaceStat)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/git/status", s.workspaceGitStatus)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/git/diff", s.workspaceGitDiff)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/git/log", s.workspaceGitLog)
	mux.HandleFunc("POST /api/v1/workspaces/{id}/operations", s.createWorkspaceOperation)
	mux.HandleFunc("GET /api/v1/workspace-operations", s.listWorkspaceOperations)
	mux.HandleFunc("GET /api/v1/workspace-command-runs", s.listWorkspaceCommandRuns)
	mux.HandleFunc("GET /api/v1/workspace-command-runs/{id}", s.getWorkspaceCommandRun)
	mux.HandleFunc("GET /api/v1/workspace-command-runs/{id}/pty", s.attachWorkspaceCommandPTY)
	mux.HandleFunc("GET /api/v1/workspace-command-runs/{id}/output", s.getWorkspaceCommandOutput)
	mux.HandleFunc("GET /api/v1/workspace-command-runs/{id}/output/download", s.downloadWorkspaceCommandOutput)
	mux.HandleFunc("POST /api/v1/workspace-command-runs/{id}/cancel", s.cancelWorkspaceCommandRun)
	mux.HandleFunc("POST /api/v1/artifacts", s.createArtifact)
	mux.HandleFunc("GET /api/v1/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /api/v1/artifacts/usage", s.getArtifactUsage)
	mux.HandleFunc("POST /api/v1/artifacts/maintenance", s.runArtifactMaintenance)
	mux.HandleFunc("GET /api/v1/artifacts/{id}", s.getArtifact)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/content", s.getArtifactContent)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/preview", s.getArtifactPreview)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/extract", s.extractArtifact)
	mux.HandleFunc("DELETE /api/v1/artifacts/{id}", s.deleteArtifact)
	mux.HandleFunc("GET /api/v1/command-runs", s.listWorkspaceCommandRuns)
	mux.HandleFunc("GET /api/v1/command-runs/{id}", s.getWorkspaceCommandRun)
	mux.HandleFunc("GET /api/v1/command-runs/{id}/pty", s.attachWorkspaceCommandPTY)
	mux.HandleFunc("GET /api/v1/command-runs/{id}/output", s.getWorkspaceCommandOutput)
	mux.HandleFunc("GET /api/v1/command-runs/{id}/output/download", s.downloadWorkspaceCommandOutput)
	mux.HandleFunc("POST /api/v1/command-runs/{id}/cancel", s.cancelWorkspaceCommandRun)
	mux.HandleFunc("POST /api/v1/workspace-operations/{id}/approve", s.approveWorkspaceOperation)
	mux.HandleFunc("POST /api/v1/workspace-operations/{id}/reject", s.rejectWorkspaceOperation)
	mux.HandleFunc("POST /api/v1/workspace-operations/{id}/retry", s.retryWorkspaceOperation)
	mux.HandleFunc("GET /api/v1/conversations", s.listConversations)
	mux.HandleFunc("POST /api/v1/conversations", s.createConversation)
	mux.HandleFunc("GET /api/v1/conversations/{id}", s.getConversation)
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages", s.listConversationMessages)
	mux.HandleFunc("GET /api/v1/conversations/{id}/context", s.getConversationContext)
	mux.HandleFunc("PUT /api/v1/conversations/{id}/subagent", s.setConversationSubAgent)
	mux.HandleFunc("POST /api/v1/conversations/{id}/archive", s.archiveConversation)
	mux.HandleFunc("POST /api/v1/conversations/{id}/unarchive", s.unarchiveConversation)
	mux.HandleFunc("DELETE /api/v1/conversations/{id}", s.deleteConversation)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/conversations", s.listWorkspaceConversations)
	mux.HandleFunc("POST /api/v1/workspaces/{id}/conversations", s.createWorkspaceConversation)
	mux.HandleFunc("POST /api/v1/chat", s.chat)
	mux.HandleFunc("POST /api/v1/invocations", s.createInvocation)
	mux.HandleFunc("GET /api/v1/invocations", s.listInvocations)
	mux.HandleFunc("GET /api/v1/invocations/{id}", s.getInvocation)
	mux.HandleFunc("GET /api/v1/invocations/{id}/plan", s.getInvocationPlan)
	mux.HandleFunc("PUT /api/v1/invocations/{id}/plan", s.updateInvocationPlan)
	mux.HandleFunc("POST /api/v1/invocations/{id}/continuations", s.continueInvocation)
	mux.HandleFunc("GET /api/v1/invocations/{id}/result", s.getInvocationResult)
	mux.HandleFunc("GET /api/v1/invocations/{id}/completion-report", s.getInvocationResult)
	mux.HandleFunc("GET /api/v1/invocations/{id}/baseline", s.getInvocationBaseline)
	mux.HandleFunc("GET /api/v1/invocations/{id}/worktree-baseline", s.getInvocationBaseline)
	mux.HandleFunc("GET /api/v1/invocations/{id}/contract", s.getInvocationContract)
	mux.HandleFunc("GET /api/v1/invocations/{id}/contracts", s.listInvocationContracts)
	mux.HandleFunc("PUT /api/v1/invocations/{id}/contract", s.updateInvocationContract)
	mux.HandleFunc("GET /api/v1/invocations/{id}/events", s.listInvocationEvents)
	mux.HandleFunc("GET /api/v1/invocations/{id}/trace", s.getInvocationTrace)
	mux.HandleFunc("GET /api/v1/invocations/{id}/usage", s.getInvocationUsage)
	mux.HandleFunc("POST /api/v1/evals/runs", s.createEvalRun)
	mux.HandleFunc("GET /api/v1/evals/runs", s.listEvalRuns)
	mux.HandleFunc("GET /api/v1/evals/runs/{id}", s.getEvalRun)
	mux.HandleFunc("POST /api/v1/evals/suites", s.runEvalSuite)
	mux.HandleFunc("GET /api/v1/evals/compare", s.compareEvalRuns)
	mux.HandleFunc("POST /api/v1/evals/gate", s.evaluateEvalGate)
	mux.HandleFunc("GET /api/v1/invocations/{id}/tool-calls", s.listInvocationToolCalls)
	mux.HandleFunc("GET /api/v1/invocations/{id}/context-manifests", s.listInvocationContextManifests)
	mux.HandleFunc("GET /api/v1/invocations/{id}/toolset", s.getInvocationToolSet)
	mux.HandleFunc("GET /api/v1/invocations/{id}/model-capabilities", s.getInvocationModelCapabilities)
	mux.HandleFunc("POST /api/v1/invocations/{id}/instructions/reconfirm", s.reconfirmInvocationInstructions)
	mux.HandleFunc("GET /api/v1/invocations/{id}/runtime-snapshot", s.getInvocationRuntimeSnapshot)
	mux.HandleFunc("GET /api/v1/invocations/{id}/delivery-attempts", s.listInvocationDeliveryAttempts)
	mux.HandleFunc("GET /api/v1/invocations/{id}/delivery-compensations", s.listInvocationDeliveryCompensations)
	mux.HandleFunc("GET /api/v1/invocations/{id}/delivery-groups", s.listInvocationDeliveryGroups)
	mux.HandleFunc("GET /api/v1/invocations/{id}/delivery-group-settlements", s.listInvocationDeliveryGroupSettlements)
	mux.HandleFunc("GET /api/v1/invocations/{id}/delivery-group-sagas", s.listInvocationDeliveryGroupSagas)
	mux.HandleFunc("GET /api/v1/invocations/{id}/working-set", s.getInvocationWorkingSet)
	mux.HandleFunc("PUT /api/v1/invocations/{id}/working-set", s.updateInvocationWorkingSet)
	mux.HandleFunc("GET /api/v1/invocations/{id}/verifications", s.listInvocationVerifications)
	mux.HandleFunc("POST /api/v1/invocations/{id}/verifications", s.createInvocationVerification)
	mux.HandleFunc("PUT /api/v1/invocations/{id}/verifications/{verification_id}", s.updateInvocationVerification)
	mux.HandleFunc("GET /api/v1/invocations/{id}/stream", s.streamInvocation)
	mux.HandleFunc("POST /api/v1/invocations/{id}/resume", s.resumeInvocation)
	mux.HandleFunc("GET /api/v1/invocations/{id}/tool-status", s.queryInvocationToolStatus)
	mux.HandleFunc("POST /api/v1/invocations/{id}/tool-status", s.queryInvocationToolStatus)
	mux.HandleFunc("POST /api/v1/invocations/{id}/tool-status/reconcile", s.reconcileInvocationToolStatus)
	mux.HandleFunc("POST /api/v1/invocations/{id}/config-migration", s.migrateInvocationConfig)
	mux.HandleFunc("POST /api/v1/invocations/{id}/cancel", s.cancelInvocation)
	mux.HandleFunc("GET /api/v1/runtime/delivery-groups/{id}", s.getRuntimeDeliveryGroup)
	mux.HandleFunc("GET /api/v1/runtime/config-directory/fanouts", s.listRuntimeConfigDirectoryFanouts)
	mux.HandleFunc("GET /api/v1/runtime/config-directory/fanouts/{id}", s.getRuntimeConfigDirectoryFanout)
	mux.HandleFunc("POST /api/v1/runtime/config-directory/rebind-plans", s.createRuntimeConfigDirectoryRebindPlan)
	mux.HandleFunc("GET /api/v1/runtime/config-directory/rebind-plans", s.listRuntimeConfigDirectoryRebindPlans)
	mux.HandleFunc("GET /api/v1/runtime/config-directory/rebind-plans/{id}", s.getRuntimeConfigDirectoryRebindPlan)
	mux.HandleFunc("POST /api/v1/runtime/config-directory/rebind-plans/{id}/confirm", s.confirmRuntimeConfigDirectoryRebindPlan)
	mux.HandleFunc("POST /api/v1/runtime/config-directory/rebind-plans/{id}/apply", s.applyRuntimeConfigDirectoryRebindPlan)
	mux.HandleFunc("GET /api/v1/runtime/config-directory/rebind-plans/{id}/applies", s.listRuntimeConfigDirectoryRebindApplies)
	mux.HandleFunc("POST /api/v1/runtime/config-directory/rebind-plans/{id}/multi-confirm", s.confirmRuntimeConfigDirectoryRebindMultiPlan)
	mux.HandleFunc("POST /api/v1/runtime/config-directory/rebind-plans/{id}/multi-apply", s.applyRuntimeConfigDirectoryRebindMultiPlan)
	mux.HandleFunc("GET /api/v1/runtime/config-directory/rebind-plans/{id}/multi-applies", s.listRuntimeConfigDirectoryRebindMultiApplies)
	mux.HandleFunc("GET /api/v1/runtime/config-directory/rebind-multi-applies/{id}", s.getRuntimeConfigDirectoryRebindMultiApply)
	if s.eventDelivery != nil {
		mux.Handle("/api/v1/runtime/event-delivery", s.eventDelivery)
	}
	if s.eventStatus != nil {
		mux.Handle("/api/v1/runtime/event-delivery/status", s.eventStatus)
	}
	if s.eventPrepare != nil {
		mux.Handle("/api/v1/runtime/event-delivery/prepare", s.eventPrepare)
	}
	if s.eventCommit != nil {
		mux.Handle("/api/v1/runtime/event-delivery/commit", s.eventCommit)
	}
	if s.eventAbort != nil {
		mux.Handle("/api/v1/runtime/event-delivery/abort", s.eventAbort)
	}
	if s.eventSource != nil {
		mux.Handle("/api/v1/runtime/event-delivery/source", s.eventSource)
	}
	if s.checkpointDelivery != nil {
		mux.Handle("/api/v1/runtime/checkpoint-delivery", s.checkpointDelivery)
	}
	if s.checkpointStatus != nil {
		mux.Handle("/api/v1/runtime/checkpoint-delivery/status", s.checkpointStatus)
	}
	if s.checkpointSource != nil {
		mux.Handle("/api/v1/runtime/checkpoint-delivery/source", s.checkpointSource)
	}
	if s.checkpointPrepare != nil {
		mux.Handle("/api/v1/runtime/checkpoint-delivery/prepare", s.checkpointPrepare)
	}
	if s.checkpointCommit != nil {
		mux.Handle("/api/v1/runtime/checkpoint-delivery/commit", s.checkpointCommit)
	}
	if s.checkpointAbort != nil {
		mux.Handle("/api/v1/runtime/checkpoint-delivery/abort", s.checkpointAbort)
	}
	if s.configDelivery != nil {
		mux.Handle("/api/v1/runtime/config-delivery", s.configDelivery)
	}
	if s.configStatus != nil {
		mux.Handle("/api/v1/runtime/config-delivery/status", s.configStatus)
	}
	if s.configPrepare != nil {
		mux.Handle("/api/v1/runtime/config-delivery/prepare", s.configPrepare)
	}
	if s.configCommit != nil {
		mux.Handle("/api/v1/runtime/config-delivery/commit", s.configCommit)
	}
	if s.configAbort != nil {
		mux.Handle("/api/v1/runtime/config-delivery/abort", s.configAbort)
	}
	if s.configDirectory != nil {
		mux.Handle("/api/v1/runtime/config-directory", s.configDirectory)
	}
	if s.configDirectoryStatus != nil {
		mux.Handle("/api/v1/runtime/config-directory/status", s.configDirectoryStatus)
	}
	if s.configDirectoryMaterialize != nil {
		mux.Handle("/api/v1/runtime/config-directory/materialize", s.configDirectoryMaterialize)
	}
	if s.rebindApply != nil {
		mux.Handle("/api/v1/runtime/config-directory/rebind-apply", s.rebindApply)
	}
	if s.rebindApplyStatus != nil {
		mux.Handle("/api/v1/runtime/config-directory/rebind-apply/status", s.rebindApplyStatus)
	}
	if s.rejectionDelivery != nil {
		mux.Handle("/api/v1/runtime/approval-rejection-delivery", s.rejectionDelivery)
	}
	if s.rejectionStatus != nil {
		mux.Handle("/api/v1/runtime/approval-rejection-delivery/status", s.rejectionStatus)
	}
	if s.rejectionPrepare != nil {
		mux.Handle("/api/v1/runtime/approval-rejection-delivery/prepare", s.rejectionPrepare)
	}
	if s.rejectionCommit != nil {
		mux.Handle("/api/v1/runtime/approval-rejection-delivery/commit", s.rejectionCommit)
	}
	if s.rejectionAbort != nil {
		mux.Handle("/api/v1/runtime/approval-rejection-delivery/abort", s.rejectionAbort)
	}
	if s.deliveryGroupStatus != nil {
		mux.Handle("/api/v1/runtime/delivery-group/status", s.deliveryGroupStatus)
	}
	if s.deliveryGroupStatusBatch != nil {
		mux.Handle("/api/v1/runtime/delivery-group/status/batch", s.deliveryGroupStatusBatch)
	}
	if s.deliveryGroupPrepare != nil {
		mux.Handle("/api/v1/runtime/delivery-group/prepare", s.deliveryGroupPrepare)
	}
	if s.deliveryGroupCommit != nil {
		mux.Handle("/api/v1/runtime/delivery-group/commit", s.deliveryGroupCommit)
	}
	if s.deliveryGroupAbort != nil {
		mux.Handle("/api/v1/runtime/delivery-group/abort", s.deliveryGroupAbort)
	}
	if s.toolStatus != nil {
		mux.Handle("/api/v1/runtime/tool-status", s.toolStatus)
	}
	mux.HandleFunc("GET /api/v1/approvals", s.listApprovals)
	mux.HandleFunc("POST /api/v1/approvals/{id}/resolve", s.resolveApproval)
	mux.HandleFunc("GET /api/v1/sessions", s.listSessions)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.deleteSession)
	mux.Handle("/", webui.Handler())
	return requestLogger(mux)
}

func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) status(writer http.ResponseWriter, _ *http.Request) {
	providers := []provider.Provider(nil)
	defaultProvider := provider.DefaultRef{}
	if s.providers != nil {
		providers = s.providers.List()
		defaultProvider = s.providers.Default()
	}
	ready := 0
	for _, item := range providers {
		if item.Status == provider.StatusReady {
			ready++
		}
	}
	workspaceCount := 0
	readyWorkspaces := 0
	if s.workspaces != nil {
		if items, err := s.workspaces.List(context.Background()); err == nil {
			workspaceCount = len(items)
			for _, item := range items {
				if item.Status == workspace.StatusReady {
					readyWorkspaces++
				}
			}
		}
	}
	botCount := 0
	runningBots := 0
	if s.bots != nil {
		items := s.bots.List()
		botCount = len(items)
		for _, item := range items {
			if item.Status == bot.StatusRunning || item.Status == bot.StatusConnecting {
				runningBots++
			}
		}
	}
	remoteTargetCount := 0
	readyRemoteTargets := 0
	if s.remoteTargets != nil {
		if items, err := s.remoteTargets.List(context.Background()); err == nil {
			remoteTargetCount = len(items)
			for _, item := range items {
				if item.Status == workspace.RemoteTargetReady && item.Enabled {
					readyRemoteTargets++
				}
			}
		}
	}
	conversationCount := 0
	activeConversationCount := 0
	if s.conversations != nil {
		if count, err := s.conversations.CountAll(context.Background(), true); err == nil {
			conversationCount = count
		}
		if count, err := s.conversations.CountAll(context.Background(), false); err == nil {
			activeConversationCount = count
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"providers":            len(providers),
		"ready_providers":      ready,
		"workspaces":           workspaceCount,
		"ready_workspaces":     readyWorkspaces,
		"bots":                 botCount,
		"running_bots":         runningBots,
		"remote_targets":       remoteTargetCount,
		"ready_remote_targets": readyRemoteTargets,
		"conversations":        conversationCount,
		"active_conversations": activeConversationCount,
		"default":              defaultProvider,
	})
}

type modelPayload struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	Enabled         *bool  `json:"enabled"`
	Source          string `json:"source"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

type providerPayload struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	BaseURL            string         `json:"base_url"`
	Protocol           string         `json:"protocol"`
	OpenAIFormat       string         `json:"openai_format"`
	TokenCountProtocol string         `json:"token_count_protocol"`
	APIKey             *string        `json:"api_key"`
	Models             []modelPayload `json:"models"`
}

type providerView struct {
	ID                 string                      `json:"id"`
	Name               string                      `json:"name"`
	BaseURL            string                      `json:"base_url"`
	Protocol           provider.Protocol           `json:"protocol"`
	OpenAIFormat       provider.OpenAIFormat       `json:"openai_format,omitempty"`
	TokenCountProtocol provider.TokenCountProtocol `json:"token_count_protocol,omitempty"`
	APIKeyConfigured   bool                        `json:"api_key_configured"`
	IsDefault          bool                        `json:"is_default"`
	Models             []provider.Model            `json:"models"`
	Status             provider.Status             `json:"status"`
	StatusMessage      string                      `json:"status_message,omitempty"`
	LastCheckedAt      any                         `json:"last_checked_at,omitempty"`
	CreatedAt          any                         `json:"created_at,omitempty"`
	UpdatedAt          any                         `json:"updated_at,omitempty"`
}

func (s *Server) listProviders(writer http.ResponseWriter, _ *http.Request) {
	items := s.providers.List()
	defaults := s.providers.Default()
	views := make([]providerView, 0, len(items))
	for _, item := range items {
		views = append(views, publicProvider(item, defaults))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"providers": views, "default": defaults})
}

// listProviderCapabilityProfiles exports only model capability metadata so a
// release job can build candidate/baseline documents without handling API
// keys or provider configuration. The registry projection is deterministic
// and fail-closed when a persisted profile is malformed.
func (s *Server) listProviderCapabilityProfiles(writer http.ResponseWriter, _ *http.Request) {
	if s.providers == nil {
		writeError(writer, provider.ErrInvalidRequest)
		return
	}
	profiles, err := s.providers.CapabilityProfiles()
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"profiles": profiles})
}

func (s *Server) getProvider(writer http.ResponseWriter, request *http.Request) {
	item, err := s.providers.Get(request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicProvider(item, s.providers.Default()))
}

func (s *Server) getProviderModelCapabilities(writer http.ResponseWriter, request *http.Request) {
	item, err := s.providers.Get(request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	modelID := strings.TrimSpace(request.PathValue("model"))
	for _, model := range item.Models {
		if model.ID != modelID {
			continue
		}
		profile := model.Capabilities
		if profile == nil {
			value := provider.DefaultCapabilities(item, model)
			profile = &value
		} else {
			value := provider.NormalizeCapabilityProfile(*profile)
			profile = &value
		}
		writeJSON(writer, http.StatusOK, profile)
		return
	}
	writeError(writer, provider.ErrNoModel)
}

func (s *Server) createProvider(writer http.ResponseWriter, request *http.Request) {
	s.saveProvider(writer, request, "")
}

func (s *Server) updateProvider(writer http.ResponseWriter, request *http.Request) {
	s.saveProvider(writer, request, request.PathValue("id"))
}

func (s *Server) saveProvider(writer http.ResponseWriter, request *http.Request, pathID string) {
	var payload providerPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if pathID != "" {
		if payload.ID != "" && payload.ID != pathID {
			writeError(writer, fmt.Errorf("路径中的供应商 ID 与请求体不一致"))
			return
		}
		payload.ID = pathID
	}
	models := make([]provider.Model, 0, len(payload.Models))
	for _, item := range payload.Models {
		enabled := true
		if item.Enabled != nil {
			enabled = *item.Enabled
		}
		models = append(models, provider.Model{
			ID:              item.ID,
			DisplayName:     item.DisplayName,
			Enabled:         enabled,
			Source:          item.Source,
			ContextWindow:   item.ContextWindow,
			MaxOutputTokens: item.MaxOutputTokens,
		})
	}
	item, err := s.providers.Save(request.Context(), provider.SaveRequest{
		Provider: provider.Provider{
			ID:                 payload.ID,
			Name:               payload.Name,
			BaseURL:            payload.BaseURL,
			Protocol:           provider.Protocol(payload.Protocol),
			OpenAIFormat:       provider.OpenAIFormat(payload.OpenAIFormat),
			TokenCountProtocol: provider.TokenCountProtocol(payload.TokenCountProtocol),
			Models:             models,
		},
		APIKey: payload.APIKey,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, publicProvider(item, s.providers.Default()))
}

func (s *Server) deleteProvider(writer http.ResponseWriter, request *http.Request) {
	if err := s.providers.Delete(request.Context(), request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) testProvider(writer http.ResponseWriter, request *http.Request) {
	result, err := s.providers.TestConnection(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) discoverModels(writer http.ResponseWriter, request *http.Request) {
	models, err := s.providers.DiscoverModels(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"models": models})
}

// previewDiscoverModels 探测未保存的表单配置，不把临时密钥或模型目录写入数据库。
func (s *Server) previewDiscoverModels(writer http.ResponseWriter, request *http.Request) {
	var payload providerPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}

	apiKey := ""
	if payload.APIKey != nil {
		apiKey = *payload.APIKey
	} else if id := strings.TrimSpace(payload.ID); id != "" {
		// 编辑已有供应商且表单未填写新密钥时，临时复用进程内的旧密钥。
		stored, err := s.providers.Get(id)
		if err == nil {
			apiKey = stored.APIKey
		} else if !errors.Is(err, provider.ErrNotFound) {
			writeError(writer, err)
			return
		}
	}

	models, err := s.providers.DiscoverModelsForProvider(request.Context(), provider.Provider{
		ID:                 payload.ID,
		Name:               payload.Name,
		BaseURL:            payload.BaseURL,
		Protocol:           provider.Protocol(payload.Protocol),
		OpenAIFormat:       provider.OpenAIFormat(payload.OpenAIFormat),
		TokenCountProtocol: provider.TokenCountProtocol(payload.TokenCountProtocol),
		APIKey:             apiKey,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) getSettings(writer http.ResponseWriter, request *http.Request) {
	result := map[string]any{
		"default": s.providers.Default(),
		"compaction": map[string]any{
			"enabled":       true,
			"trigger_ratio": 0.8,
			"source":        "agent-default",
		},
	}
	if s.config != nil {
		if profileID, err := s.config.DefaultProfileID(request.Context()); err == nil {
			result["config"] = map[string]any{"default_profile_id": profileID, "schema_version": configsvc.SchemaVersion}
		}
		// 兼容旧设置端点也读取配置中心的有效值，避免旧客户端继续显示固定的压缩默认值。
		if runtime, err := s.config.Resolve(request.Context(), "", ""); err == nil {
			result["compaction"] = map[string]any{
				"enabled":          runtime.CompactionEnabled,
				"trigger_ratio":    runtime.CompactionRatio,
				"safety_tokens":    runtime.CompactionSafetyTokens,
				"retention_events": runtime.CompactionRetentionEvents,
				"sliding_interval": runtime.CompactionInterval,
				"sliding_overlap":  runtime.CompactionOverlap,
				"source":           "config-profile",
			}
			if runtime.ProviderID != "" || runtime.ModelID != "" {
				result["config_default"] = provider.DefaultRef{ProviderID: runtime.ProviderID, ModelID: runtime.ModelID}
			}
		}
		if settings, err := s.config.GetSystemSettings(request.Context()); err == nil {
			result["system"] = settings
		}
	}
	writeJSON(writer, http.StatusOK, result)
}

func (s *Server) setDefaultModel(writer http.ResponseWriter, request *http.Request) {
	var ref provider.DefaultRef
	if err := decodeJSON(writer, request, &ref); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if err := s.providers.SetDefault(request.Context(), ref); err != nil {
		writeError(writer, err)
		return
	}
	if s.config != nil {
		// 兼容旧端点：新配置中心也同步保存默认模型，使两个入口的运行期语义一致。
		profileID, profileErr := s.config.DefaultProfileID(request.Context())
		if profileErr == nil {
			profile, getErr := s.config.GetProfile(request.Context(), profileID)
			if getErr != nil {
				writeError(writer, getErr)
				return
			}
			values := profile.Values
			if values == nil {
				values = configsvc.Values{}
			}
			values["ai.default_provider_id"] = ref.ProviderID
			values["ai.default_model_id"] = ref.ModelID
			if _, saveErr := s.config.SaveProfile(request.Context(), profile.ID, profile.Name, values); saveErr != nil {
				writeError(writer, saveErr)
				return
			}
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"default": s.providers.Default()})
}

type chatPayload struct {
	UserID         string `json:"user_id"`
	IdempotencyKey string `json:"idempotency_key"`
	BotID          string `json:"bot_id"`
	ConversationID string `json:"conversation_id"`
	// SessionID 兼容旧版客户端；新客户端只使用 ConversationID。
	SessionID   string                  `json:"session_id"`
	ProviderID  string                  `json:"provider_id"`
	ModelID     string                  `json:"model_id"`
	Message     string                  `json:"message"`
	Attachments []chatAttachmentPayload `json:"attachments"`
	Stream      bool                    `json:"stream"`
	// ReasoningEffort 是可选的请求级思考强度覆盖；为空时用配置中心的默认值。
	ReasoningEffort string `json:"reasoning_effort"`
}

// reasoningEffortValues 是允许下发的思考强度；空值表示不指定，交给配置中心或模型默认。
var reasoningEffortValues = map[string]struct{}{"minimal": {}, "low": {}, "medium": {}, "high": {}}

// normalizeReasoningEffort 校验并规范化请求级思考强度，避免把任意字符串透传给上游模型。
func normalizeReasoningEffort(value string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(value))
	if effort == "" {
		return "", nil
	}
	if _, ok := reasoningEffortValues[effort]; !ok {
		return "", fmt.Errorf("思考强度 %q 不受支持", value)
	}
	return effort, nil
}

// chatAttachmentPayload 是聊天 API 的上传格式；data 由 encoding/json 按 Base64 字符串解码。
type chatAttachmentPayload struct {
	Name            string `json:"name"`
	MIMEType        string `json:"mime_type"`
	Data            []byte `json:"data"`
	ArtifactID      string `json:"artifact_id,omitempty"`
	ArtifactVersion int    `json:"artifact_version,omitempty"`
	ArtifactDigest  string `json:"artifact_digest,omitempty"`
	ArtifactSize    int64  `json:"artifact_size,omitempty"`
}

const (
	// 附件会膨胀为 Base64，因此请求体上限要高于附件原始字节上限。
	maxChatRequestBytes         int64 = 32 << 20
	maxChatAttachments                = 5
	maxChatAttachmentBytes      int64 = 10 << 20
	maxChatAttachmentTotalBytes int64 = 20 << 20
)

func (s *Server) chat(writer http.ResponseWriter, request *http.Request) {
	var payload chatPayload
	if err := decodeJSONWithLimit(writer, request, &payload, maxChatRequestBytes); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	reasoningEffort, err := normalizeReasoningEffort(payload.ReasoningEffort)
	if err != nil {
		writeError(writer, err)
		return
	}
	conversationID := strings.TrimSpace(payload.ConversationID)
	// 先校验附件，再按需创建对话，避免无效上传留下没有消息的孤立对话。
	attachments, err := normalizeChatAttachments(payload.Attachments)
	if err != nil {
		writeError(writer, fmt.Errorf("请求附件无效: %w", err))
		return
	}
	if conversationID == "" {
		// 旧版请求使用 session_id；只把它作为对话 ID 兼容，不再允许携带 workspace_id。
		conversationID = strings.TrimSpace(payload.SessionID)
	}
	if err := s.validateChatAttachmentRefs(request.Context(), payload.UserID, conversationID, attachments); err != nil {
		writeError(writer, fmt.Errorf("请求附件 ref 无效: %w", err))
		return
	}
	if s.conversations != nil && conversationID != "" {
		item, getErr := s.conversations.Get(request.Context(), payload.UserID, conversationID)
		if getErr != nil {
			writeError(writer, getErr)
			return
		}
		// 归档会话只能查看和删除，不能再接收新任务，避免删除流程与新请求并发。
		if item.Status != conversation.StatusActive {
			writeError(writer, fmt.Errorf("会话当前状态为 %s，不能继续聊天", item.Status))
			return
		}
	}
	if s.conversations != nil && conversationID == "" {
		created, createErr := s.conversations.Create(request.Context(), conversation.CreateRequest{UserID: payload.UserID})
		if createErr != nil {
			writeError(writer, createErr)
			return
		}
		conversationID = created.ID
	}
	if conversationID == "" {
		conversationID = agent.NewSessionID()
	}
	attachments, err = s.materializeChatAttachments(request.Context(), payload.UserID, conversationID, invocationIdempotencyKey(request, payload.IdempotencyKey), attachments)
	if err != nil {
		writeError(writer, err)
		return
	}
	chatRequest := agent.ChatRequest{
		UserID: payload.UserID, IdempotencyKey: invocationIdempotencyKey(request, payload.IdempotencyKey), BotID: payload.BotID, ConversationID: conversationID, SessionID: conversationID, ProviderID: payload.ProviderID,
		ModelID: payload.ModelID, Message: payload.Message, Attachments: attachments, Stream: payload.Stream, ReasoningEffort: reasoningEffort,
	}
	// The legacy endpoint remains a compatibility projection, but production
	// requests must run through the durable Coordinator so a browser disconnect
	// does not lose the task. Tests and embedders that do not install a Runtime
	// Coordinator continue to use the original synchronous path below.
	if s.runtime != nil {
		invocation, startErr := s.runtime.StartInvocation(request.Context(), chatRequest)
		if startErr != nil {
			writeError(writer, startErr)
			return
		}
		if payload.Stream {
			s.streamRuntimeChat(writer, request, invocation)
			return
		}
		s.waitRuntimeChat(writer, request, invocation)
		return
	}
	runContext, cancel, err := s.chatContext(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	defer cancel()
	if payload.Stream {
		s.streamChat(writer, runContext, chatRequest)
		return
	}
	var final string
	for event, err := range s.kernel.Run(runContext, chatRequest) {
		if err != nil {
			writeError(writer, err)
			return
		}
		if isAssistantEvent(event) {
			if text := agent.TextFromContent(event.Content); text != "" {
				final = text
			}
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"text": final, "conversation_id": conversationID, "session_id": conversationID})
}

// chatContext 将系统设置中的全局请求超时真正施加到 WebUI/API 聊天请求。
func (s *Server) chatContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if s.config == nil {
		return parent, func() {}, nil
	}
	settings, err := s.config.GetSystemSettings(parent)
	if err != nil {
		return nil, nil, fmt.Errorf("读取系统设置失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(settings.RequestTimeoutSeconds)*time.Second)
	return ctx, cancel, nil
}

func (s *Server) streamChat(writer http.ResponseWriter, ctx context.Context, request agent.ChatRequest) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, errors.New("当前 HTTP 服务不支持 SSE"))
		return
	}
	streamLog := newSSEStreamLog("chat", "conversation_id", request.ConversationID, "session_id", request.SessionID)
	streamStatus := "closed"
	defer func() {
		streamLog.finish(streamStatus)
	}()
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	finalText := ""
	for event, err := range s.kernel.Run(ctx, request) {
		if err != nil {
			streamStatus = "failed"
			_ = writeSSE(writer, "error", map[string]any{"error": err.Error()}, streamLog)
			flusher.Flush()
			return
		}
		if !isAssistantEvent(event) {
			continue
		}
		text := agent.TextFromContent(event.Content)
		if event.Partial {
			if text != "" {
				if err := writeSSE(writer, "message", map[string]any{"type": "message", "delta": text}, streamLog); err != nil {
					streamStatus = "write_failed"
					return
				}
				flusher.Flush()
			}
			continue
		}
		if text != "" {
			finalText = text
		}
	}
	if err := writeSSE(writer, "done", map[string]any{"type": "done", "text": finalText, "conversation_id": request.ConversationID, "session_id": request.SessionID}, streamLog); err != nil {
		streamStatus = "write_failed"
		return
	}
	streamStatus = "completed"
	flusher.Flush()
}

// normalizeChatAttachments 校验上传数量和大小，并把浏览器 MIME 信息规范成 IANA 类型。
func normalizeChatAttachments(items []chatAttachmentPayload) ([]agent.Attachment, error) {
	if len(items) > maxChatAttachments {
		return nil, fmt.Errorf("附件数量不能超过 %d 个", maxChatAttachments)
	}
	result := make([]agent.Attachment, 0, len(items))
	var total int64
	for index, item := range items {
		name := cleanAttachmentName(item.Name)
		if name == "" {
			name = fmt.Sprintf("附件-%d", index+1)
		}
		if strings.TrimSpace(item.ArtifactID) != "" {
			if len(item.Data) > 0 || item.ArtifactVersion <= 0 || item.ArtifactSize < 0 {
				return nil, fmt.Errorf("第 %d 个附件 ref 无效：需要 artifact_id、正版本且不能带 inline data", index+1)
			}
			mimeType := normalizeAttachmentMIME(name, item.MIMEType)
			result = append(result, agent.Attachment{
				Name: name, MIMEType: mimeType,
				Ref: &agent.AttachmentRef{ID: strings.TrimSpace(item.ArtifactID), Version: item.ArtifactVersion, Digest: strings.TrimSpace(item.ArtifactDigest), Size: item.ArtifactSize, Name: name, MIMEType: mimeType},
			})
			continue
		}
		if item.ArtifactVersion != 0 || strings.TrimSpace(item.ArtifactDigest) != "" || item.ArtifactSize != 0 {
			return nil, fmt.Errorf("第 %d 个附件 artifact ref 不完整", index+1)
		}
		if len(item.Data) == 0 {
			return nil, fmt.Errorf("第 %d 个附件内容不能为空", index+1)
		}
		if int64(len(item.Data)) > maxChatAttachmentBytes {
			return nil, fmt.Errorf("第 %d 个附件不能超过 %d MB", index+1, maxChatAttachmentBytes>>20)
		}
		total += int64(len(item.Data))
		if total > maxChatAttachmentTotalBytes {
			return nil, fmt.Errorf("附件总大小不能超过 %d MB", maxChatAttachmentTotalBytes>>20)
		}
		result = append(result, agent.Attachment{
			Name:     name,
			MIMEType: normalizeAttachmentMIME(name, item.MIMEType),
			Data:     append([]byte(nil), item.Data...),
		})
	}
	return result, nil
}

// materializeChatAttachments moves inline browser bytes to the Artifact CAS
// before a durable Invocation is accepted. The Invocation then stores only a
// versioned, digest-bound ref; legacy inline behavior remains available when
// an embedder has not installed Artifact service.
func (s *Server) materializeChatAttachments(ctx context.Context, userID, conversationID, producerKey string, attachments []agent.Attachment) ([]agent.Attachment, error) {
	if s.artifacts == nil || len(attachments) == 0 {
		return attachments, nil
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id 不能为空", artifact.ErrInvalidRequest)
	}
	result := make([]agent.Attachment, len(attachments))
	for index, attachment := range attachments {
		result[index] = attachment
		if attachment.Ref != nil {
			ref := *attachment.Ref
			if ref.Version <= 0 || strings.TrimSpace(ref.ID) == "" {
				return nil, fmt.Errorf("第 %d 个附件 ref 无效", index+1)
			}
			item, err := s.artifacts.Get(ctx, userID, ref.ID)
			if err != nil {
				return nil, fmt.Errorf("第 %d 个附件不可访问: %w", index+1, err)
			}
			if item.Status != artifact.StatusReady || item.Version != ref.Version {
				return nil, fmt.Errorf("第 %d 个附件版本或状态已变化: %w", index+1, artifact.ErrConflict)
			}
			if item.ConversationID != "" && item.ConversationID != strings.TrimSpace(conversationID) {
				return nil, fmt.Errorf("第 %d 个附件不属于当前对话: %w", index+1, artifact.ErrForbidden)
			}
			if ref.Digest != "" && ref.Digest != item.Digest || ref.Size > 0 && ref.Size != item.Size {
				return nil, fmt.Errorf("第 %d 个附件 digest/size 不一致: %w", index+1, artifact.ErrConflict)
			}
			canonical := item.Ref()
			result[index] = agent.Attachment{Name: canonical.Name, MIMEType: canonical.MIMEType, Ref: &agent.AttachmentRef{ID: canonical.ID, Version: canonical.Version, Digest: canonical.Digest, Kind: string(canonical.Kind), MIMEType: canonical.MIMEType, Size: canonical.Size, Name: canonical.Name}}
			continue
		}
		if len(attachment.Data) == 0 {
			return nil, fmt.Errorf("第 %d 个附件内容不能为空", index+1)
		}
		producerID := ""
		if strings.TrimSpace(producerKey) != "" {
			producerID = strings.TrimSpace(producerKey) + ":" + strconv.Itoa(index)
		}
		item, err := s.artifacts.Put(ctx, artifact.PutRequest{
			UserID: userID, ConversationID: strings.TrimSpace(conversationID), ProducerType: "chat_attachment", ProducerID: producerID,
			Kind: artifact.KindInputAttachment, Name: attachment.Name, MIMEType: attachment.MIMEType,
		}, bytes.NewReader(attachment.Data))
		if err != nil {
			return nil, fmt.Errorf("第 %d 个附件写入 Artifact 失败: %w", index+1, err)
		}
		canonical := item.Ref()
		result[index] = agent.Attachment{Name: canonical.Name, MIMEType: canonical.MIMEType, Ref: &agent.AttachmentRef{ID: canonical.ID, Version: canonical.Version, Digest: canonical.Digest, Kind: string(canonical.Kind), MIMEType: canonical.MIMEType, Size: canonical.Size, Name: canonical.Name}}
	}
	return result, nil
}

func (s *Server) validateChatAttachmentRefs(ctx context.Context, userID, conversationID string, attachments []agent.Attachment) error {
	if s.artifacts == nil {
		return nil
	}
	for index, attachment := range attachments {
		if attachment.Ref == nil {
			continue
		}
		if attachment.Ref.Version <= 0 || strings.TrimSpace(attachment.Ref.ID) == "" {
			return fmt.Errorf("第 %d 个附件 ref 无效", index+1)
		}
		item, err := s.artifacts.Get(ctx, userID, attachment.Ref.ID)
		if err != nil {
			return fmt.Errorf("第 %d 个附件不可访问: %w", index+1, err)
		}
		if item.ConversationID != "" && item.ConversationID != strings.TrimSpace(conversationID) {
			return fmt.Errorf("第 %d 个附件不属于当前对话: %w", index+1, artifact.ErrForbidden)
		}
		if item.Status != artifact.StatusReady || item.Version != attachment.Ref.Version {
			return fmt.Errorf("第 %d 个附件版本或状态已变化: %w", index+1, artifact.ErrConflict)
		}
	}
	return nil
}

// cleanAttachmentName 只保留文件名部分，避免把客户端传入的路径带入模型上下文。
func cleanAttachmentName(name string) string {
	name = strings.TrimSpace(strings.Map(func(value rune) rune {
		if value < 0x20 || value == 0x7f {
			return -1
		}
		return value
	}, name))
	name = strings.ReplaceAll(name, "\\", "/")
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	return strings.TrimSpace(name)
}

// normalizeAttachmentMIME 优先相信客户端声明，缺失时再根据扩展名推断。
func normalizeAttachmentMIME(name, value string) string {
	if mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value)); err == nil && mediaType != "" {
		return strings.ToLower(mediaType)
	}
	if guessed := mime.TypeByExtension(filepath.Ext(name)); guessed != "" {
		if mediaType, _, err := mime.ParseMediaType(guessed); err == nil && mediaType != "" {
			return strings.ToLower(mediaType)
		}
	}
	return "application/octet-stream"
}

func (s *Server) listSessions(writer http.ResponseWriter, request *http.Request) {
	items, err := s.kernel.ListSessions(request.Context(), request.URL.Query().Get("user_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"sessions": items})
}

func (s *Server) deleteSession(writer http.ResponseWriter, request *http.Request) {
	userID := request.URL.Query().Get("user_id")
	if strings.TrimSpace(userID) == "" {
		writeError(writer, errors.New("user_id 不能为空"))
		return
	}
	if err := s.kernel.DeleteSession(request.Context(), userID, request.PathValue("id")); err != nil {
		writeError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func publicProvider(item provider.Provider, defaults provider.DefaultRef) providerView {
	return providerView{
		ID: item.ID, Name: item.Name, BaseURL: item.BaseURL, Protocol: item.Protocol,
		OpenAIFormat: item.OpenAIFormat, TokenCountProtocol: item.TokenCountProtocol, APIKeyConfigured: strings.TrimSpace(item.APIKey) != "",
		IsDefault: defaults.ProviderID == item.ID,
		Models:    item.Models, Status: item.Status, StatusMessage: item.StatusMessage,
		LastCheckedAt: item.LastCheckedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func isAssistantEvent(event *session.Event) bool {
	if event == nil || event.Author == "user" || event.Content == nil {
		return false
	}
	return event.Content.Role == "model" || event.Content.Role == "assistant" || event.Author != ""
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	return decodeJSONWithLimit(writer, request, target, 2*1024*1024)
}

// decodeJSONWithLimit 统一处理 JSON 请求，并允许聊天附件使用更大的专用上限。
func decodeJSONWithLimit(writer http.ResponseWriter, request *http.Request, target any, limit int64) error {
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

// decodeSingleJSONWithLimit is used by state-changing commands whose body is
// part of an idempotent decision. It rejects a second JSON document (and any
// non-whitespace trailing bytes) so a proxy and Runtime cannot disagree about
// which command was accepted.
func decodeSingleJSONWithLimit(writer http.ResponseWriter, request *http.Request, target any, limit int64) error {
	if request == nil || request.Body == nil {
		return fmt.Errorf("请求体不能为空")
	}
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("请求包含多个 JSON 文档")
		}
		return fmt.Errorf("请求包含额外内容: %w", err)
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writeJSONInternal(writer, status, value, true)
}

// writeJSONWithoutBodyLog 返回结构化 JSON 但不把响应正文再次写入日志，适用于日志查询等自包含接口。
func writeJSONWithoutBodyLog(writer http.ResponseWriter, status int, value any) {
	writeJSONInternal(writer, status, value, false)
}

// writeJSONInternal 统一编码和写回逻辑；日志正文开关用于阻断日志查询接口的自引用。
func writeJSONInternal(writer http.ResponseWriter, status int, value any, logBody bool) {
	data, err := json.Marshal(value)
	if err != nil {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(status)
		slog.Error("编码 JSON 响应失败", "status", status, "error", err)
		return
	}
	data = append(data, '\n')
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if logBody {
		// 保留普通 API 的完整响应日志，方便查看机器人和模型请求链路；自包含日志接口会显式关闭此项。
		slog.Info("HTTP JSON 响应正文", "status", status, "body", string(data))
	}
	if _, err := writer.Write(data); err != nil {
		slog.Debug("写入 JSON 响应失败", "error", err)
	}
}

func writeSSE(writer http.ResponseWriter, event string, value any, streamLog *sseStreamLog) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if streamLog != nil {
		streamLog.record(event, data)
	} else {
		// 非流式聚合调用保留兼容日志，避免未来新增调用点完全丢失正文。
		slog.Info("HTTP SSE 响应正文", "event", event, "body", string(data))
	}
	_, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case isHTTPMaxBytesError(err):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, bot.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, bot.ErrInvalid), errors.Is(err, bot.ErrUnsupported):
		status = http.StatusBadRequest
	case errors.Is(err, bot.ErrNotRunning):
		status = http.StatusConflict
	case errors.Is(err, bot.ErrClosed):
		status = http.StatusConflict
	case errors.Is(err, provider.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, conversation.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, memorysvc.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, workspace.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, workspace.ErrRemoteTargetNotFound):
		status = http.StatusNotFound
	case errors.Is(err, provider.ErrNoDefault), errors.Is(err, provider.ErrNoModel):
		status = http.StatusConflict
	case errors.Is(err, provider.ErrInvalidRequest), errors.Is(err, provider.ErrInvalidCapabilityOverride):
		status = http.StatusBadRequest
	case errors.Is(err, provider.ErrCapabilityProbeUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, provider.ErrCapabilityObservationUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, provider.ErrInvalidCapabilityProfile):
		status = http.StatusBadRequest
	case errors.Is(err, provider.ErrInvalidCapabilityGate):
		status = http.StatusBadRequest
	case errors.Is(err, provider.ErrCapabilitySnapshotConflict):
		status = http.StatusConflict
	case errors.Is(err, configsvc.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, configsvc.ErrInvalidRequest), errors.Is(err, configsvc.ErrConflict):
		status = http.StatusBadRequest
	case errors.Is(err, configsvc.ErrDefaultProfile), errors.Is(err, configsvc.ErrProfileInUse):
		status = http.StatusConflict
	case errors.Is(err, configsvc.ErrDefaultPersona), errors.Is(err, configsvc.ErrPersonaInUse):
		status = http.StatusConflict
	case errors.Is(err, conversation.ErrInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, agentruntime.ErrInvalidPlan):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrInvalidWorkflowContinuation):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrWorkflowContinuationUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrWorkflowContinuationSource), errors.Is(err, agentruntime.ErrWorkflowContinuationConflict):
		status = http.StatusConflict
	case errors.Is(err, agentruntime.ErrInvalidTaskContract):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrInvalidEvalRun):
		status = http.StatusBadRequest
	case errors.Is(err, agent.ErrToolNotFound):
		status = http.StatusNotFound
	case errors.Is(err, agent.ErrInvalidToolDescriptor), errors.Is(err, agent.ErrRequiredToolUnavailable):
		status = http.StatusBadRequest
	case errors.Is(err, agent.ErrToolConflict), errors.Is(err, agent.ErrToolSnapshotConflict):
		status = http.StatusConflict
	case errors.Is(err, agentruntime.ErrInvalidResume):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrInvalidRuntimeLongRunningToolStatus):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrRuntimeLongRunningToolStatusUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrRuntimeLongRunningToolStatusAuth):
		status = http.StatusUnauthorized
	case errors.Is(err, agentruntime.ErrRuntimeLongRunningToolStatusTargetMismatch):
		status = http.StatusConflict
	case errors.Is(err, agentruntime.ErrInvalidRuntimeDeliveryAttempt):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrRuntimeDeliveryAttemptUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrInvalidRuntimeDeliveryCompensation):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrRuntimeDeliveryCompensationUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrInvalidRuntimeDeliveryGroupTransaction):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrRuntimeDeliveryGroupTransactionUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrRuntimeDeliveryGroupTransactionAuth):
		status = http.StatusUnauthorized
	case errors.Is(err, agentruntime.ErrRuntimeDeliveryGroupTransactionStale):
		status = http.StatusUnauthorized
	case errors.Is(err, agentruntime.ErrInvalidRuntimeDeliveryGroupSettlement):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrRuntimeDeliveryGroupSettlementUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrInvalidRuntimeConfigDirectory):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrInvalidRuntimeConfigDirectoryRebind):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindNotEligible), errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindCapability), errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindSnapshot), errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindConflict):
		status = http.StatusConflict
	case errors.Is(err, agentruntime.ErrInvalidRuntimeConfigDirectoryRebindConfirmation), errors.Is(err, agentruntime.ErrInvalidRuntimeConfigDirectoryRebindApply):
		status = http.StatusBadRequest
	case errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindApplyUnavailable):
		status = http.StatusNotImplemented
	case errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationExpired), errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindConfirmationConflict), errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindApplyConflict), errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindApplyStale):
		status = http.StatusConflict
	case errors.Is(err, agentruntime.ErrRuntimeConfigDirectoryRebindApplyAuth):
		status = http.StatusUnauthorized
	case errors.Is(err, agentruntime.ErrResumeMismatch), errors.Is(err, agentruntime.ErrNotStarted), errors.Is(err, agentruntime.ErrConflict), errors.Is(err, agentruntime.ErrAlreadyResolved), errors.Is(err, agentruntime.ErrApprovalExpired):
		status = http.StatusConflict
	case errors.Is(err, memorysvc.ErrInvalidRequest), errors.Is(err, memorysvc.ErrConflict):
		status = http.StatusBadRequest
	case errors.Is(err, artifact.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, artifact.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, artifact.ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, artifact.ErrQuotaExceeded):
		status = http.StatusInsufficientStorage
	case errors.Is(err, artifact.ErrExpired), errors.Is(err, artifact.ErrObjectMissing):
		status = http.StatusGone
	case errors.Is(err, artifact.ErrQuarantined), errors.Is(err, artifact.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, artifact.ErrInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, workspace.ErrInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, workspace.ErrCommandOutputExpired):
		status = http.StatusGone
	case errors.Is(err, workspace.ErrCommandOutputDownloadTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, workspace.ErrPTYUnavailable):
		status = http.StatusConflict
	case errors.Is(err, workspace.ErrPTYWriterHeld):
		status = http.StatusConflict
	case errors.Is(err, workspace.ErrPTYWriterDenied):
		status = http.StatusForbidden
	case errors.Is(err, workspace.ErrPTYInvalidSignal):
		status = http.StatusBadRequest
	case errors.Is(err, workspace.ErrRemoteTargetInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, workspace.ErrDisabled), errors.Is(err, workspace.ErrUnsupported), errors.Is(err, workspace.ErrOperationState), errors.Is(err, workspace.ErrCommandRunConflict):
		status = http.StatusConflict
	case errors.Is(err, workspace.ErrRemoteTargetDisabled), errors.Is(err, workspace.ErrRemoteTargetInUse), errors.Is(err, workspace.ErrRemoteTargetUnconfirmed):
		status = http.StatusConflict
	case errors.Is(err, workspace.ErrHasConversations), errors.Is(err, conversation.ErrArchived), errors.Is(err, conversation.ErrDeleteRequiresArchive):
		status = http.StatusConflict
	case strings.Contains(err.Error(), "上游 API"), strings.Contains(err.Error(), "调用 OpenAI"), strings.Contains(err.Error(), "Gemini API"):
		status = http.StatusBadGateway
	default:
		if status == http.StatusBadRequest && !strings.Contains(err.Error(), "不能为空") && !strings.Contains(err.Error(), "无效") {
			status = http.StatusInternalServerError
		}
	}
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}

func isHTTPMaxBytesError(err error) bool {
	var target *http.MaxBytesError
	return errors.As(err, &target)
}

// 聊天请求允许携带较大的 Base64 图片；日志上限与聊天 JSON 请求上限一致，避免大图片日志被过早截断。
const requestLogBodyLimit int64 = 32 << 20

// requestLogger 记录完整 HTTP 请求正文和响应状态；正文只设置有界上限，避免日志本身耗尽进程内存。
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		startedAt := time.Now()
		body, truncated, readErr := replayRequestBody(request)
		if isWebSearchCredentialRequest(request) {
			body = redactWebSearchCredentialBody(body, truncated)
		}
		query := request.URL.RawQuery
		requestURI := request.URL.RequestURI()
		if isWebSearchCredentialRequest(request) && query != "" {
			query = "[REDACTED]"
			requestURI = request.URL.Path + "?[REDACTED]"
		}
		requestAttrs := []any{
			"method", request.Method,
			"path", request.URL.Path,
			"query", query,
			"url", requestURI,
			"content_type", request.Header.Get("Content-Type"),
			"content_length", request.ContentLength,
		}
		if body != "" || request.ContentLength > 0 {
			requestAttrs = append(requestAttrs, "body", body, "body_truncated", truncated)
		}
		if readErr != nil {
			requestAttrs = append(requestAttrs, "body_read_error", readErr)
		}
		// 请求日志放在 Handler 前，确保即使业务返回错误也能看到原始输入。
		slog.Info("HTTP 请求", requestAttrs...)

		recorder := &responseRecorder{ResponseWriter: writer}
		defer func() {
			// 用 defer 记录响应，覆盖正常返回和 Handler 抛出异常前的已写状态。
			slog.Info("HTTP 响应", "method", request.Method, "path", request.URL.Path, "status", recorder.statusCode(), "bytes", recorder.bytes, "duration", time.Since(startedAt))
		}()
		next.ServeHTTP(recorder, request)
	})
}

func isWebSearchCredentialRequest(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	if request.Method == http.MethodPost && request.URL.Path == "/api/v1/web-search/services" {
		return true
	}
	if request.Method != http.MethodPut {
		return false
	}
	const prefix = "/api/v1/web-search/services/"
	path := strings.TrimPrefix(request.URL.Path, prefix)
	return request.URL.Path != path && path != "" && !strings.Contains(path, "/")
}

func redactWebSearchCredentialBody(body string, truncated bool) string {
	if truncated {
		return "[REDACTED: truncated web search service body]"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return "[REDACTED: invalid web search service body]"
	}
	for key := range fields {
		if strings.EqualFold(strings.TrimSpace(key), "api_key") {
			fields[key] = json.RawMessage(`"[REDACTED]"`)
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return "[REDACTED: web search service body]"
	}
	return string(encoded)
}

// replayRequestBody 读取请求正文用于日志后再拼回原 Body，保证业务解码逻辑看到的内容完全不变。
func replayRequestBody(request *http.Request) (string, bool, error) {
	if request == nil || request.Body == nil || request.Body == http.NoBody {
		return "", false, nil
	}
	original := request.Body
	data, err := io.ReadAll(io.LimitReader(original, requestLogBodyLimit+1))
	truncated := int64(len(data)) > requestLogBodyLimit
	// LimitReader 为判断截断多读取了一个字节；回放时必须把已读取的全部字节放回去，不能丢掉这个字节。
	replayData := data
	if truncated {
		data = data[:requestLogBodyLimit]
	}
	request.Body = &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(replayData), original), closer: original}
	return string(data), truncated, err
}

// replayReadCloser 在重放日志前缀的同时保留原始 Body 的关闭语义。
type replayReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *replayReadCloser) Close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// responseRecorder 只旁路记录状态和字节数，不缓存响应正文，因此不破坏 SSE、文件下载和 WebSocket。
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *responseRecorder) statusCode() int {
	if r == nil || !r.wrote {
		return http.StatusOK
	}
	return r.status
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.status = status
	r.wrote = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	count, err := r.ResponseWriter.Write(data)
	r.bytes += int64(count)
	return count, err
}

func (r *responseRecorder) Flush() {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("底层响应写入器不支持 WebSocket hijack")
}

func (r *responseRecorder) Push(target string, options *http.PushOptions) error {
	if pusher, ok := r.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
