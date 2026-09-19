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
