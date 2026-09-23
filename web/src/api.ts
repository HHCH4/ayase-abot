import type {
  Bot,
  Artifact,
  ChatAttachment,
  ConfigProfile,
  ConfigRevision,
  ConfigSchema,
  ConversationContextStatus,
  Conversation,
  ConversationMessage,
  CompletionReport,
  Operation,
  CommandRun,
  CommandOutputPage,
  MemoryItem,
  Provider,
  DirectoryListing,
  RemoteTarget,
  SystemSettingsResponse,
  Workspace,
  InvocationTrace,
  InvocationUsage,
  RuntimeSnapshot,
  TaskPlan,
  VerificationRun,
  ToolSetSnapshot,
  ModelCapabilitySnapshot,
  WorktreeBaseline,
  Persona,
  PersonaRevision,
  SessionRule,
  SessionRuleGroup,
  SessionSource,
  ScheduledTask,
  DashboardStats,
  DataLogEntry,
  WebSearchService,
  WebSearchServiceInput,
  WebSearchUsageSummary,
  WebSearchTestResult,
} from './types'

export class ApiError extends Error {
  status: number

  constructor(message: string, status = 0) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

// 所有前端请求集中经过这里，保证错误信息和无响应正文时的处理一致。
export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(path, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      ...(options.headers || {}),
    },
  })
  if (response.status === 204) return undefined as T
  const body = await response.json().catch(() => ({})) as { error?: string }
  if (!response.ok) throw new ApiError(body.error || `请求失败（${response.status}）`, response.status)
  return body as T
}

export function jsonBody(value: unknown): RequestInit {
  return { method: 'POST', body: JSON.stringify(value) }
}

export async function readConversationMessages(userID: string, conversationID: string): Promise<ConversationMessage[]> {
  const result = await request<{ messages: ConversationMessage[] }>(`/api/v1/conversations/${encodeURIComponent(conversationID)}/messages?user_id=${encodeURIComponent(userID)}`)
  return result.messages || []
}

// 管理台对话操作仍复用正式会话接口，确保归档、恢复和物理删除与聊天页保持同一套生命周期规则。
export async function archiveStoredConversation(userID: string, conversationID: string): Promise<Conversation> {
  return request<Conversation>(`/api/v1/conversations/${encodeURIComponent(conversationID)}/archive?user_id=${encodeURIComponent(userID)}`, { method: 'POST', body: '{}' })
}

export async function unarchiveStoredConversation(userID: string, conversationID: string): Promise<Conversation> {
  return request<Conversation>(`/api/v1/conversations/${encodeURIComponent(conversationID)}/unarchive?user_id=${encodeURIComponent(userID)}`, { method: 'POST', body: '{}' })
}

export async function deleteStoredConversation(userID: string, conversationID: string): Promise<void> {
  await request<void>(`/api/v1/conversations/${encodeURIComponent(conversationID)}?user_id=${encodeURIComponent(userID)}`, { method: 'DELETE' })
}

export async function readConversationContext(userID: string, conversationID: string): Promise<ConversationContextStatus> {
  return request<ConversationContextStatus>(`/api/v1/conversations/${encodeURIComponent(conversationID)}/context?user_id=${encodeURIComponent(userID)}`)
}

export async function readInvocationTrace(invocationID: string): Promise<InvocationTrace> {
  return request<InvocationTrace>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/trace`)
}

export async function readInvocationUsage(invocationID: string): Promise<InvocationUsage> {
  return request<InvocationUsage>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/usage`)
}

export async function readInvocationRuntimeSnapshot(invocationID: string): Promise<RuntimeSnapshot> {
  return request<RuntimeSnapshot>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/runtime-snapshot`)
}

export async function readInvocationPlan(invocationID: string): Promise<TaskPlan> {
  return request<TaskPlan>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/plan`)
}

export async function readInvocationResult(invocationID: string): Promise<CompletionReport> {
  return request<CompletionReport>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/result`)
}

export async function readInvocationBaseline(invocationID: string): Promise<WorktreeBaseline> {
  return request<WorktreeBaseline>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/baseline`)
}

export async function readInvocationVerifications(invocationID: string): Promise<VerificationRun[]> {
  const result = await request<{ verifications: VerificationRun[] }>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/verifications`)
  return result.verifications || []
}

export async function readInvocationToolSet(invocationID: string): Promise<ToolSetSnapshot> {
  return request<ToolSetSnapshot>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/toolset`)
}

export async function readInvocationCapabilities(invocationID: string): Promise<ModelCapabilitySnapshot> {
  return request<ModelCapabilitySnapshot>(`/api/v1/invocations/${encodeURIComponent(invocationID)}/model-capabilities`)
}

export async function readConfigRevisions(profileID: string): Promise<ConfigRevision[]> {
  const result = await request<{ revisions: ConfigRevision[] }>(`/api/v1/config-profiles/${encodeURIComponent(profileID)}/revisions`)
  return result.revisions || []
}

export async function readPersonas(): Promise<{ personas: Persona[]; default_persona_id: string }> {
  return request<{ personas: Persona[]; default_persona_id: string }>('/api/v1/personas')
}

export async function readPersonaRevisions(personaID: string): Promise<PersonaRevision[]> {
  const result = await request<{ revisions: PersonaRevision[] }>(`/api/v1/personas/${encodeURIComponent(personaID)}/revisions`)
  return result.revisions || []
}

export async function savePersona(value: Partial<Persona> & { instruction: string; name: string }): Promise<Persona> {
  const path = value.id ? `/api/v1/personas/${encodeURIComponent(value.id)}` : '/api/v1/personas'
  return request<Persona>(path, { method: value.id ? 'PUT' : 'POST', body: JSON.stringify(value) })
}

export async function setDefaultPersona(personaID: string): Promise<void> {
  await request(`/api/v1/personas/${encodeURIComponent(personaID)}/default`, { method: 'POST', body: '{}' })
}

export async function deletePersona(personaID: string): Promise<void> {
  await request<void>(`/api/v1/personas/${encodeURIComponent(personaID)}`, { method: 'DELETE' })
}

export async function readSessionRules(query = ''): Promise<SessionRule[]> {
  const result = await request<{ rules: SessionRule[] }>(`/api/v1/session-rules${query.trim() ? `?q=${encodeURIComponent(query.trim())}` : ''}`)
  return result.rules || []
}

// 读取已经产生过平台消息的会话来源，规则页面据此提供可选 UMO。
export async function readSessionSources(query = ''): Promise<SessionSource[]> {
  const result = await request<{ sources: SessionSource[] }>(`/api/v1/session-sources${query.trim() ? `?q=${encodeURIComponent(query.trim())}` : ''}`)
  return result.sources || []
}

export async function saveSessionRule(value: Partial<SessionRule> & { source: string }): Promise<SessionRule> {
	return request<SessionRule>(`/api/v1/session-rules/${encodeURIComponent(value.source)}`, { method: 'PUT', body: JSON.stringify(value) })
}

// 新建规则单独走 POST，避免来源字段存在时误把新规则当成更新请求。
export async function createSessionRule(value: Partial<SessionRule> & { source: string }): Promise<SessionRule> {
	return request<SessionRule>('/api/v1/session-rules', { method: 'POST', body: JSON.stringify(value) })
}

export async function deleteSessionRule(source: string): Promise<void> {
  await request<void>(`/api/v1/session-rules/${encodeURIComponent(source)}`, { method: 'DELETE' })
}

// 只清除指定规则项，保留同一来源的其他覆盖配置和 UMO 来源记录。
export async function resetSessionRuleField(source: string, key: string): Promise<void> {
  await request<void>(`/api/v1/session-rules/${encodeURIComponent(source)}/reset`, { method: 'POST', body: JSON.stringify({ key }) })
}

export async function batchSessionRules(value: Record<string, unknown>): Promise<SessionRule[]> {
  const result = await request<{ rules: SessionRule[] }>('/api/v1/session-rules/batch', { method: 'POST', body: JSON.stringify(value) })
  return result.rules || []
}

export async function readSessionRuleGroups(): Promise<SessionRuleGroup[]> {
  const result = await request<{ groups: SessionRuleGroup[] }>('/api/v1/session-rule-groups')
  return result.groups || []
}

export async function saveSessionRuleGroup(value: Partial<SessionRuleGroup> & { name: string; members: string[] }): Promise<SessionRuleGroup> {
  const path = value.id ? `/api/v1/session-rule-groups/${encodeURIComponent(value.id)}` : '/api/v1/session-rule-groups'
  return request<SessionRuleGroup>(path, { method: value.id ? 'PUT' : 'POST', body: JSON.stringify(value) })
}

export async function deleteSessionRuleGroup(id: string): Promise<void> {
  await request<void>(`/api/v1/session-rule-groups/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function readScheduledTasks(status = ''): Promise<ScheduledTask[]> {
  const query = status ? `?status=${encodeURIComponent(status)}` : ''
  const result = await request<{ tasks: ScheduledTask[] }>(`/api/v1/scheduled-tasks${query}`)
  return result.tasks || []
}

export async function saveScheduledTask(value: Partial<ScheduledTask> & { name: string; request: string; mode: string }): Promise<ScheduledTask> {
  const path = value.id ? `/api/v1/scheduled-tasks/${encodeURIComponent(value.id)}` : '/api/v1/scheduled-tasks'
  return request<ScheduledTask>(path, { method: value.id ? 'PUT' : 'POST', body: JSON.stringify(value) })
}

export async function setScheduledTaskStatus(id: string, action: 'pause' | 'resume'): Promise<ScheduledTask> {
  return request<ScheduledTask>(`/api/v1/scheduled-tasks/${encodeURIComponent(id)}/${action}`, { method: 'POST', body: '{}' })
}

export async function deleteScheduledTask(id: string): Promise<void> {
  await request<void>(`/api/v1/scheduled-tasks/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function readDashboardStats(range = '1d'): Promise<DashboardStats> {
  return request<DashboardStats>(`/api/v1/data/stats?range=${encodeURIComponent(range)}`)
}

export async function readDataConversations(query: Record<string, string> = {}): Promise<{ conversations: Conversation[]; total: number; page: number; page_size: number }> {
  const params = new URLSearchParams(query)
  return request<{ conversations: Conversation[]; total: number; page: number; page_size: number }>(`/api/v1/data/conversations?${params}`)
}

export async function readDataTraces(query: Record<string, string> = {}): Promise<{ traces: Record<string, unknown>[]; total: number; page: number; page_size: number }> {
  const params = new URLSearchParams(query)
  return request<{ traces: Record<string, unknown>[]; total: number; page: number; page_size: number }>(`/api/v1/data/traces?${params}`)
}

export async function readDataLogs(level = ''): Promise<DataLogEntry[]> {
  const query = level ? `?level=${encodeURIComponent(level)}` : ''
  const result = await request<{ logs: DataLogEntry[] }>(`/api/v1/data/logs${query}`)
  return result.logs || []
}

export async function readWebSearchServices(): Promise<WebSearchService[]> {
  const result = await request<{ services: WebSearchService[] }>('/api/v1/web-search/services')
  return result.services || []
}

export async function saveWebSearchService(value: WebSearchServiceInput): Promise<WebSearchService> {
  const path = value.id ? `/api/v1/web-search/services/${encodeURIComponent(value.id)}` : '/api/v1/web-search/services'
  return request<WebSearchService>(path, { method: value.id ? 'PUT' : 'POST', body: JSON.stringify(value) })
}

export async function deleteWebSearchService(id: string): Promise<void> {
  await request<void>(`/api/v1/web-search/services/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function testWebSearchService(id: string): Promise<WebSearchTestResult> {
  return request<WebSearchTestResult>(`/api/v1/web-search/services/${encodeURIComponent(id)}/test`, { method: 'POST', body: '{}' })
}

export async function readWebSearchUsage(): Promise<WebSearchUsageSummary> {
  return request<WebSearchUsageSummary>('/api/v1/web-search/usage')
}

// 打开日志页时建立 SSE 连接，关闭页面或切换筛选条件时由调用方关闭连接。
export function openDataLogStream(
  level: string,
  onSnapshot: (entries: DataLogEntry[]) => void,
  onEntry: (entry: DataLogEntry) => void,
  onError?: () => void,
): () => void {
  const query = level ? `?level=${encodeURIComponent(level)}` : ''
  const source = new EventSource(`/api/v1/data/logs/stream${query}`)
  source.addEventListener('snapshot', (event) => {
    try {
      const payload = JSON.parse((event as MessageEvent).data) as { logs?: DataLogEntry[] }
      onSnapshot(payload.logs || [])
    } catch {
      onError?.()
    }
  })
  source.addEventListener('log', (event) => {
    try {
      onEntry(JSON.parse((event as MessageEvent).data) as DataLogEntry)
    } catch {
      onError?.()
    }
  })
  source.onerror = () => onError?.()
  return () => source.close()
}

export async function readOperations(workspaceID = '', conversationID = ''): Promise<Operation[]> {
  const params = new URLSearchParams()
  if (workspaceID) params.set('workspace_id', workspaceID)
  if (conversationID) params.set('conversation_id', conversationID)
  const result = await request<{ operations: Operation[] }>(`/api/v1/workspace-operations${params.toString() ? `?${params}` : ''}`)
  return result.operations || []
}

export async function readCommandRuns(workspaceID = '', invocationID = '', operationID = ''): Promise<CommandRun[]> {
  const params = new URLSearchParams()
  if (workspaceID) params.set('workspace_id', workspaceID)
  if (invocationID) params.set('invocation_id', invocationID)
  if (operationID) params.set('operation_id', operationID)
  const result = await request<{ command_runs: CommandRun[] }>(`/api/v1/workspace-command-runs${params.toString() ? `?${params}` : ''}`)
  return result.command_runs || []
}

export async function readCommandRun(commandRunID: string): Promise<CommandRun> {
  return request<CommandRun>(`/api/v1/workspace-command-runs/${encodeURIComponent(commandRunID)}`)
}

export async function readCommandOutput(commandRunID: string, after = 0, limit = 200): Promise<CommandOutputPage> {
  return request<CommandOutputPage>(`/api/v1/workspace-command-runs/${encodeURIComponent(commandRunID)}/output?after=${after}&limit=${limit}`)
}

export async function uploadArtifact(file: Blob, userID: string, options: { name: string; mimeType?: string; conversationID?: string }): Promise<Artifact> {
  const params = new URLSearchParams({ name: options.name || 'artifact.bin', kind: 'input_attachment' })
  if (options.conversationID) params.set('conversation_id', options.conversationID)
  const response = await fetch(`/api/v1/artifacts?${params.toString()}`, {
    method: 'POST',
    headers: {
      'Content-Type': options.mimeType || file.type || 'application/octet-stream',
      'X-Abot-User-ID': userID,
    },
    body: file,
  })
  const body = await response.json().catch(() => ({})) as { error?: string }
  if (!response.ok) throw new ApiError(body.error || `附件上传失败（${response.status}）`, response.status)
  return body as Artifact
}

export function artifactContentURL(artifactID: string, userID: string): string {
  const params = new URLSearchParams({ user_id: userID })
  return `/api/v1/artifacts/${encodeURIComponent(artifactID)}/content?${params.toString()}`
}

export function commandOutputDownloadURL(commandRunID: string, format: 'text' | 'ndjson' = 'text'): string {
  return `/api/v1/workspace-command-runs/${encodeURIComponent(commandRunID)}/output/download?format=${encodeURIComponent(format)}`
}

export async function cancelCommandRun(commandRunID: string, reason = ''): Promise<CommandRun> {
  return request<CommandRun>(`/api/v1/workspace-command-runs/${encodeURIComponent(commandRunID)}/cancel`, { method: 'POST', body: JSON.stringify(reason ? { reason } : {}) })
}

export async function readLocalDirectories(path = ''): Promise<{ path: string; parent: string; entries: { name: string; path: string; is_dir: boolean }[] }> {
  const result = await request<DirectoryListing & { entries?: DirectoryListing['directories'] }>(`/api/v1/local/directories${path ? `?path=${encodeURIComponent(path)}` : ''}`)
  // 兼容早期前端使用 entries 的字段名，同时统一新接口的 directories 命名。
  return { path: result.path, parent: result.parent, entries: result.directories || result.entries || [] }
}

export async function readRemoteDirectories(targetID: string, path = ''): Promise<DirectoryListing> {
  const query = path ? `?path=${encodeURIComponent(path)}` : ''
  return request<DirectoryListing>(`/api/v1/remote-targets/${encodeURIComponent(targetID)}/directories${query}`)
}

export async function readWorkspaceFiles(workspaceID: string, path = ''): Promise<unknown[]> {
  const result = await request<{ files: unknown[] }>(`/api/v1/workspaces/${encodeURIComponent(workspaceID)}/files?path=${encodeURIComponent(path)}`)
  return result.files || []
}

export async function readMemories(userID: string, query = ''): Promise<MemoryItem[]> {
  const params = new URLSearchParams({ user_id: userID })
  if (query.trim()) params.set('q', query.trim())
  const result = await request<{ memories: MemoryItem[] }>(`/api/v1/memories?${params}`)
  return result.memories || []
}

export async function createMemory(value: { user_id: string; conversation_id?: string; content: string; tags?: string }): Promise<MemoryItem> {
  return request<MemoryItem>('/api/v1/memories', { method: 'POST', body: JSON.stringify(value) })
}

export async function updateMemory(id: string, value: { user_id: string; content: string; tags?: string }): Promise<MemoryItem> {
  return request<MemoryItem>(`/api/v1/memories/${encodeURIComponent(id)}?user_id=${encodeURIComponent(value.user_id)}`, {
    method: 'PUT', body: JSON.stringify({ content: value.content, tags: value.tags || '' }),
  })
}

export async function deleteMemory(id: string, userID: string): Promise<void> {
  await request<void>(`/api/v1/memories/${encodeURIComponent(id)}?user_id=${encodeURIComponent(userID)}`, { method: 'DELETE' })
}

export async function clearMemories(userID: string): Promise<void> {
  await request<void>(`/api/v1/memories?user_id=${encodeURIComponent(userID)}`, { method: 'DELETE' })
}

// 聊天使用 SSE，事件解析只保留协议层逻辑，页面负责展示和会话状态。
export async function streamChat(payload: {
  user_id: string
  conversation_id: string
  provider_id: string
  model_id: string
  message: string
  attachments: ChatAttachment[]
  stream: boolean
  /** 请求级思考强度覆盖；为空时由后端使用配置中心默认值。 */
  reasoning_effort?: string
}, onEvent: (type: string, data: Record<string, unknown>) => void): Promise<void> {
  const response = await fetch('/api/v1/chat', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  })
  if (!response.ok) {
    const body = await response.json().catch(() => ({})) as { error?: string }
    throw new ApiError(body.error || `聊天失败（${response.status}）`, response.status)
  }
  if (!response.body) throw new ApiError('服务器没有返回聊天流')
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  const consume = (line: string) => {
    if (!line.startsWith('data:')) return
    const content = line.slice(5).trim()
    if (!content) return
    const data = JSON.parse(content) as Record<string, unknown>
    onEvent(String(data.type || 'message'), data)
  }
  while (true) {
    const chunk = await reader.read()
    if (chunk.done) break
    buffer += decoder.decode(chunk.value, { stream: true })
    const lines = buffer.split('\n')
    buffer = lines.pop() || ''
    lines.forEach(consume)
  }
  // 服务端关闭连接时未必会补最后一个换行，不能丢掉尾部 SSE 事件。
  buffer += decoder.decode()
  consume(buffer)
}

export type CoreState = {
  providers: Provider[]
  bots: Bot[]
  workspaces: Workspace[]
  conversations: Conversation[]
  defaults: { provider_id?: string; model_id?: string }
  configSchema: ConfigSchema
  configProfiles: ConfigProfile[]
  defaultProfileID: string
  systemSettings: SystemSettingsResponse
}
