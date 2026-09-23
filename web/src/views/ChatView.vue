<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { NAlert, NButton, NEmpty, NForm, NFormItem, NInput, NScrollbar, NSelect, NSpace, NSpin, NTag, useMessage } from 'naive-ui'
import { readConversationContext, readConversationMessages, readInvocationCapabilities, readInvocationPlan, readInvocationResult, readInvocationRuntimeSnapshot, readInvocationSubAgents, readInvocationToolSet, readInvocationTrace, readInvocationVerifications, readSubAgentGroup, request, streamChat, uploadArtifact } from '@/api'
import ConversationActions from '@/components/ConversationActions.vue'
import AppIcon from '@/components/AppIcon.vue'
import ProjectCreateDialog from '@/components/ProjectCreateDialog.vue'
import ProviderDialog from '@/components/ProviderDialog.vue'
import { useAppStore } from '@/stores/app'
import { useRoute, useRouter } from 'vue-router'
import type { ChatAttachment, CompletionReport, Conversation, ConversationContextStatus, ConversationMessage,
  Provider, InvocationTrace, ModelCapabilitySnapshot, RuntimeSnapshot, SubAgentGroup, SubAgentGroupDetail, TaskPlan, ToolSetSnapshot, VerificationRun, Workspace } from '@/types'

const store = useAppStore()
const route = useRoute()
const router = useRouter()
const message = useMessage()
const userID = ref('webui-user')
const conversationID = ref('')
const conversationMessages = ref<ConversationMessage[]>([])
const contextStatus = ref<ConversationContextStatus>({ compressed: false, compaction_count: 0 })
const inputText = ref('')
const attachments = ref<ChatAttachment[]>([])
const selectedModel = ref('')
const providerID = ref('')
const profileID = ref('')
const sending = ref(false)
const preparingConversation = ref(false)
const loadingMessages = ref(false)
const statusText = ref('')
const showSettings = ref(false)
// 思考强度是请求级覆盖，不写回会话；空值表示沿用配置中心默认。
const reasoningEffort = ref('')
// 项目与供应商都在 chat 内就地管理，不跳回管理台。
const showProjectDialog = ref(false)
const showProviderDialog = ref(false)
const editingProvider = ref<Provider | null>(null)

function openProviderDialog(provider: Provider | null) {
  editingProvider.value = provider
  showProviderDialog.value = true
}

async function removeProvider(provider: Provider) {
  if (!window.confirm(`确认删除供应商“${provider.name}”？使用它的对话需要重新选择模型。`)) return
  try {
    await request(`/api/v1/providers/${encodeURIComponent(provider.id)}`, { method: 'DELETE' })
    await store.reloadProviders()
    message.success('供应商已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除供应商失败')
  }
}

const reasoningEffortOptions = [
  { label: '思考：默认', value: '' },
  { label: '思考：最少', value: 'minimal' },
  { label: '思考：低', value: 'low' },
  { label: '思考：中', value: 'medium' },
  { label: '思考：高', value: 'high' },
]
type RuntimeTimelineItem = { type: string; text: string; timestamp: string; data?: Record<string, unknown> }
type PendingApprovalChoice = { id: string; label: string; approved: boolean }
type PendingApproval = { id: string; toolName: string; hint: string; args: Record<string, unknown>; choices: PendingApprovalChoice[] }
type PendingUserInputOption = { id: string; label: string; description?: string; recommended?: boolean }
type PendingUserInput = {
  id: string; invocationID: string; title: string; question: string; kind: string; options: PendingUserInputOption[]
  allowMultiple: boolean; allowFreeform: boolean; allowSkip: boolean; minSelections: number; maxSelections: number
  step: number; totalSteps: number; placeholder: string; selectedIDs: string[]; freeform: string
}
type InstructionSnapshotMeta = { path?: string; scope_path?: string; source?: string; priority?: number; content_digest?: string }
type InstructionConflict = { previous?: InstructionSnapshotMeta[]; current?: InstructionSnapshotMeta[]; reason?: string }
type RuntimeSubAgentDetails = { groups: SubAgentGroup[]; details: Record<string, SubAgentGroupDetail> }
const runtimeTimeline = ref<RuntimeTimelineItem[]>([])
const pendingApproval = ref<PendingApproval | null>(null)
const pendingUserInput = ref<PendingUserInput | null>(null)
const instructionConflict = ref<InstructionConflict | null>(null)
const resolvingApproval = ref(false)
const resolvingUserInput = ref(false)
const reconfirmingInstructions = ref(false)
const currentInvocationID = ref('')
const runtimeDetails = ref<{
  trace?: InvocationTrace
  snapshot?: RuntimeSnapshot
  plan?: TaskPlan
  result?: CompletionReport
  verifications?: VerificationRun[]
  toolset?: ToolSetSnapshot
  capabilities?: ModelCapabilitySnapshot
  subagents?: RuntimeSubAgentDetails
} | null>(null)
const loadingRuntimeDetails = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)
const messagePanel = ref<HTMLElement | null>(null)

const selectedConversation = computed(() => store.conversations.find((item) => item.id === conversationID.value))
const activeConversations = computed(() => store.conversations)
const subAgentEffectiveEnabled = computed(() => {
  const override = selectedConversation.value?.subagent_enabled
  return typeof override === 'boolean' ? override : store.systemSettings.subagent_enabled !== false
})
const subAgentMode = computed(() => {
  const override = selectedConversation.value?.subagent_enabled
  if (typeof override !== 'boolean') return 'inherit'
  return override ? 'on' : 'off'
})
const subAgentModeOptions = [
  { label: '跟随系统默认', value: 'inherit' },
  { label: '当前会话启用', value: 'on' },
  { label: '当前会话停用', value: 'off' },
]
const profileOptions = computed(() => [{ label: '使用系统默认配置', value: '' }, ...store.configProfiles.map((item) => ({ label: `${item.name}${item.is_default ? ' · 默认' : ''}`, value: item.id }))])
const providerOptions = computed(() => store.providers.map((item) => ({ label: item.name, value: item.id })))
const modelOptions = computed(() => {
  const provider = store.providers.find((item) => item.id === providerID.value)
  const options = (provider?.models || []).filter((item) => item.enabled).map((item) => ({ label: item.display_name || item.id, value: item.id }))
  if (selectedModel.value && !options.some((item) => item.value === selectedModel.value)) {
    // 兼容旧配置中的目录外模型，但不再把用户引导到另一个手动输入框。
    options.unshift({ label: `当前配置模型 · ${selectedModel.value}`, value: selectedModel.value })
  }
  return options
})
const modelSelectValue = computed({
  get: () => selectedModel.value,
  set: (value: string) => { selectedModel.value = value || '' },
})
// 顶栏只做模型切换：首项是"跟随默认"，另外把目录外的当前模型也列进来，避免显示成占位文案。
const defaultModelLabel = computed(() => {
  const id = store.defaults.model_id
  if (!id) return '默认模型'
  const provider = store.providers.find((item) => item.id === store.defaults.provider_id)
  const model = provider?.models.find((item) => item.id === id)
  return `默认模型 · ${model?.display_name || id}`
})
const topbarModelOptions = computed(() => {
  const options = modelOptions.value
  const current = selectedModel.value
  const followDefault = { label: defaultModelLabel.value, value: '' }
  if (current && !options.some((item) => item.value === current)) return [followDefault, { label: current, value: current }, ...options]
  return [followDefault, ...options]
})
const topbarModelValue = computed<string | null>({
  // 空字符串在 naive-ui 里算"已选择"，所以必须有一个 value 为空串的选项来承接它。
  get: () => selectedModel.value || '',
  set: (value: string | null) => { selectedModel.value = value || '' },
})
const currentModelLabel = computed(() => {
  const provider = store.providers.find((item) => item.id === providerID.value)
  return provider?.models.find((item) => item.id === selectedModel.value)?.display_name || selectedModel.value || '未选择模型'
})
const displayStatus = computed(() => {
  if (pendingApproval.value) return '等待审批'
  if (pendingUserInput.value) return '等待回答'
  if (sending.value) return statusText.value || '运行中'
  if (statusText.value) return statusText.value
  if (!selectedConversation.value) return '发送消息后自动创建对话'
  return selectedConversation.value.status === 'active' ? '就绪' : '已归档'
})
const statusTagType = computed<'default' | 'success' | 'warning' | 'error' | 'info'>(() => {
  if (pendingApproval.value) return 'warning'
  if (pendingUserInput.value) return 'info'
  if (statusText.value === '运行失败') return 'error'
  if (statusText.value === '完成' || (!sending.value && selectedConversation.value?.status === 'active')) return 'success'
  if (sending.value) return 'info'
  return 'default'
})
function resultTagType(status?: string): 'default' | 'success' | 'warning' | 'error' | 'info' {
  if (status === 'completed') return 'success'
  if (status === 'failed' || status === 'blocked') return 'error'
  if (status === 'partial') return 'warning'
  if (status === 'in_progress' || status === 'waiting') return 'info'
  return 'default'
}

function subAgentTagType(status?: string): 'default' | 'success' | 'warning' | 'error' | 'info' {
  if (status === 'completed') return 'success'
  if (status === 'failed' || status === 'expired') return 'error'
  if (status === 'partial_failed' || status === 'cancelled') return 'warning'
  if (status === 'running' || status === 'queued') return 'info'
  return 'default'
}

function subAgentProfileLabel(profile?: string) {
  const labels: Record<string, string> = {
    generic: '通用子 Agent',
    document_image: '文档图片', memory_retrieval: '记忆检索', knowledge_retrieval: '知识检索',
    conversation_retrieval: '会话检索', web_research: '网络研究', workspace_search: '工作区检索',
    structured_query: '结构化查询', retrieval_aggregate: '检索汇总', research: '研究',
    workspace_review: '工作区审查', workspace_change: '工作区变更', verification: '验证',
  }
  return labels[profile || ''] || profile || '子 Agent'
}
const quickPrompts = ['总结当前对话', '帮我检查这个项目', '从这里开始一个任务']

function workspaceName(id?: string) {
  if (!id) return '普通对话'
  return store.workspaces.find((item) => item.id === id)?.name || id
}

function formatContextTime(value?: string) {
  if (!value) return ''
  const time = new Date(value)
  return Number.isNaN(time.getTime()) ? '' : time.toLocaleString()
}

function formatDuration(milliseconds?: number) {
  if (!milliseconds || milliseconds < 1) return '—'
  if (milliseconds < 1000) return `${milliseconds} ms`
  const seconds = milliseconds / 1000
  if (seconds < 60) return `${seconds.toFixed(1)} s`
  return `${Math.floor(seconds / 60)} 分 ${Math.round(seconds % 60)} 秒`
}

function formatUsageValue(value?: { value?: number; known: boolean }) {
  if (!value?.known || value.value === undefined) return '未知'
  return value.value.toLocaleString()
}

async function loadRuntimeDetails(invocationID: string) {
  if (!invocationID) return
  currentInvocationID.value = invocationID
  loadingRuntimeDetails.value = true
  const read = async <T>(loader: () => Promise<T>) => {
    try { return await loader() } catch { return undefined }
  }
  const [trace, snapshot, plan, result, verifications, toolset, capabilities, groups] = await Promise.all([
    read(() => readInvocationTrace(invocationID)),
    read(() => readInvocationRuntimeSnapshot(invocationID)),
    read(() => readInvocationPlan(invocationID)),
    read(() => readInvocationResult(invocationID)),
    read(() => readInvocationVerifications(invocationID)),
    read(() => readInvocationToolSet(invocationID)),
    read(() => readInvocationCapabilities(invocationID)),
    read(() => readInvocationSubAgents(invocationID)),
  ])
  const details: Record<string, SubAgentGroupDetail> = {}
  if (groups?.length) {
    await Promise.all(groups.map(async (group) => {
      try { details[group.id] = await readSubAgentGroup(group.id) } catch { /* 任务详情允许在子任务结束前暂时不可读 */ }
    }))
  }
  runtimeDetails.value = { trace, snapshot, plan, result, verifications, toolset, capabilities, subagents: groups ? { groups, details } : undefined }
  loadingRuntimeDetails.value = false
}

function conversationLabel(item: { title: string; workspace_id?: string; status: string }) {
  return `${item.title || '新对话'} · ${workspaceName(item.workspace_id)}${item.status === 'archived' ? ' · 已归档' : ''}`
}

function profileValues(id = profileID.value) {
  return store.configProfiles.find((item) => item.id === id)?.values || store.defaultProfile?.values || {}
}

function applyProfile(id = profileID.value) {
  const values = profileValues(id)
  const nextProvider = typeof values['ai.default_provider_id'] === 'string' ? values['ai.default_provider_id'] : store.defaults.provider_id || ''
  if (nextProvider && store.providers.some((item) => item.id === nextProvider)) providerID.value = nextProvider
  else if (!store.providers.some((item) => item.id === providerID.value)) providerID.value = store.defaults.provider_id || store.providers[0]?.id || ''
  const nextModel = typeof values['ai.default_model_id'] === 'string' ? values['ai.default_model_id'] : store.defaults.model_id || ''
  if (nextModel) {
    selectedModel.value = nextModel
  } else {
    // 切换到没有默认模型的配置时清掉旧值，避免把上一份配置的模型带入当前对话。
    selectedModel.value = ''
  }
}

async function selectConversation(id: string, updateRoute = true) {
  if (id !== conversationID.value) {
    // 切换会话立即清空旧消息，避免新会话加载期间继续显示或误认为继承了旧历史。
    conversationMessages.value = []
    contextStatus.value = { compressed: false, compaction_count: 0 }
    statusText.value = ''
    runtimeTimeline.value = []
    pendingApproval.value = null
    pendingUserInput.value = null
    instructionConflict.value = null
    currentInvocationID.value = ''
    runtimeDetails.value = null
  }
  conversationID.value = id
  showSettings.value = false
  if (updateRoute) await router.replace({ name: 'chat', query: id ? { conversation: id } : undefined })
  const item = selectedConversation.value
  if (!item) return
  loadingMessages.value = true
  try {
    const [messages, context] = await Promise.all([
      readConversationMessages(userID.value, id),
      readConversationContext(userID.value, id),
    ])
    conversationMessages.value = messages
    contextStatus.value = context
    try {
      const binding = await request<{ profile_id: string }>(`/api/v1/conversations/${encodeURIComponent(id)}/config-profile`)
      profileID.value = binding.profile_id || ''
      applyProfile(profileID.value)
    } catch {
      profileID.value = ''
      applyProfile('')
    }
    await scrollToBottom()
  } catch (error) {
    conversationMessages.value = []
    statusText.value = error instanceof Error ? error.message : '读取对话失败'
  } finally {
    loadingMessages.value = false
  }
}

// 会话侧栏：原先独立的全局项目树收进 chat 内部，改为按项目组织对话。
const expandedProjects = reactive<Record<string, boolean>>({})
const projectWorkspaces = computed(() => store.workspaces)
const looseConversations = computed(() => store.conversations.filter((item) => !item.workspace_id && item.status === 'active'))
const archivedConversations = computed(() => store.conversations.filter((item) => item.status === 'archived'))

function projectConversations(workspace: Workspace, archived = false) {
  return store.conversations.filter((item) => item.workspace_id === workspace.id && (archived ? item.status === 'archived' : item.status === 'active'))
}

function isProjectExpanded(workspace: Workspace) {
  return expandedProjects[workspace.id] !== false
}

function toggleProject(workspace: Workspace) {
  expandedProjects[workspace.id] = !isProjectExpanded(workspace)
}

function isCurrentConversation(item: { id: string }) {
  return item.id === conversationID.value
}

async function createProjectConversation(workspace: Workspace) {
  try {
    const conversation = await request<{ id: string }>(`/api/v1/workspaces/${encodeURIComponent(workspace.id)}/conversations`, { method: 'POST', body: JSON.stringify({ user_id: userID.value, title: `${workspace.name} 对话` }) })
    await store.reloadConversations(userID.value)
    await selectConversation(conversation.id)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '创建项目对话失败')
  }
}

async function ensureConversation() {
  const fromRoute = typeof route.query.conversation === 'string' ? route.query.conversation : ''
  const preferred = fromRoute && store.conversations.some((item) => item.id === fromRoute) ? fromRoute : store.conversations[0]?.id || ''
  if (preferred) {
    // 删除当前对话后路由可能还带着旧 ID，此时同步改到新的对话，避免刷新页面再次访问已删除记录。
    await selectConversation(preferred, preferred !== fromRoute)
    return
  }
  conversationID.value = ''
  conversationMessages.value = []
  contextStatus.value = { compressed: false, compaction_count: 0 }
  if (fromRoute) await router.replace({ name: 'chat' })
}

// createAutomaticConversation 为没有当前会话的首次消息准备普通对话。
// 创建成功后立即刷新列表并选中，保证后续附件和消息都绑定到同一个会话。
async function createAutomaticConversation(title = '新对话'): Promise<Conversation | null> {
  if (preparingConversation.value) return null
  preparingConversation.value = true
  try {
    const created = await request<Conversation>('/api/v1/conversations', {
      method: 'POST',
      body: JSON.stringify({ user_id: userID.value, title: title.trim() || '新对话' }),
    })
    await store.reloadConversations(userID.value)
    await selectConversation(created.id)
    return store.conversations.find((item) => item.id === created.id) || created
  } catch (error) {
    message.error(error instanceof Error ? error.message : '自动创建对话失败')
    return null
  } finally {
    preparingConversation.value = false
  }
}

async function createConversation() {
  const title = window.prompt('请输入对话名称', '新对话')
  if (title === null) return
  try {
    const conversation = await request<{ id: string }>('/api/v1/conversations', { method: 'POST', body: JSON.stringify({ user_id: userID.value, title: title.trim() || '新对话' }) })
    await store.reloadConversations(userID.value)
    await selectConversation(conversation.id)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '创建对话失败')
  }
}

async function archiveConversation(target?: Conversation) {
  const item = target || selectedConversation.value
  if (!item || item.status !== 'active') return
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(item.id)}/archive?user_id=${encodeURIComponent(userID.value)}`, { method: 'POST', body: '{}' })
    await store.reloadConversations(userID.value)
    if (item.id === conversationID.value) await selectConversation(item.id, false)
    message.success('对话已归档')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '归档失败')
  }
}

async function unarchiveConversation(target?: Conversation) {
  const item = target || selectedConversation.value
  if (!item || item.status !== 'archived') return
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(item.id)}/unarchive?user_id=${encodeURIComponent(userID.value)}`, { method: 'POST', body: '{}' })
    await store.reloadConversations(userID.value)
    if (item.id === conversationID.value) await selectConversation(item.id, false)
    message.success('对话已恢复')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '恢复失败')
  }
}

async function deleteConversation(target?: Conversation) {
  const item = target || selectedConversation.value
  if (!item || item.status !== 'archived') return
  if (!window.confirm(`彻底删除“${item.title}”及全部消息？此操作不可恢复。`)) return
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(item.id)}?user_id=${encodeURIComponent(userID.value)}`, { method: 'DELETE' })
    await store.reloadConversations(userID.value)
    if (item.id !== conversationID.value) {
      message.success('对话已彻底删除')
      return
    }
    conversationID.value = ''
    conversationMessages.value = []
    contextStatus.value = { compressed: false, compaction_count: 0 }
    await ensureConversation()
    message.success('对话已彻底删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除对话失败')
  }
}

async function bindProfile() {
  if (!conversationID.value) return
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(conversationID.value)}/config-profile`, { method: 'PUT', body: JSON.stringify({ profile_id: profileID.value }) })
    applyProfile(profileID.value)
    message.success('当前对话配置已更新')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '绑定配置失败')
  }
}

async function setSubAgentMode(value: string) {
  if (!conversationID.value || selectedConversation.value?.status !== 'active') return
  const enabled = value === 'inherit' ? null : value === 'on'
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(conversationID.value)}/subagent?user_id=${encodeURIComponent(userID.value)}`, {
      method: 'PUT', body: JSON.stringify({ enabled }),
    })
    await store.reloadConversations(userID.value)
    message.success(enabled === null ? '当前会话已恢复跟随系统默认' : `当前会话子 Agent 已${enabled ? '启用' : '停用'}`)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '更新子 Agent 开关失败')
  }
}

function guessMime(name: string) {
  const extension = name.toLowerCase().split('.').pop() || ''
  const values: Record<string, string> = { txt: 'text/plain', md: 'text/markdown', csv: 'text/csv', json: 'application/json', pdf: 'application/pdf', doc: 'application/msword', docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document', xls: 'application/vnd.ms-excel', xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' }
  return values[extension] || 'application/octet-stream'
}

async function uploadFile(file: File): Promise<ChatAttachment> {
  const mimeType = file.type || guessMime(file.name)
  const item = await uploadArtifact(file, userID.value, { name: file.name, mimeType, conversationID: conversationID.value })
  return {
    name: item.name || file.name,
    mime_type: item.mime_type || mimeType,
    size: item.size,
    artifact_id: item.id,
    artifact_version: item.version,
    artifact_digest: item.digest,
    artifact_size: item.size,
  }
}

async function chooseFiles(event: Event) {
  const input = event.target as HTMLInputElement
  const selected = Array.from(input.files || [])
  input.value = ''
  if (attachments.value.length + selected.length > 5) {
    message.error('最多添加 5 个附件')
    return
  }
  const total = attachments.value.reduce((sum, item) => sum + (item.size || 0), 0)
  let currentTotal = total
  const accepted: File[] = []
  for (const file of selected) {
    if (file.size > 10 * 1024 * 1024) {
      message.error(`文件“${file.name}”超过 10 MB`)
      continue
    }
    if (currentTotal + file.size > 20 * 1024 * 1024) {
      message.error('附件总大小不能超过 20 MB')
      break
    }
    currentTotal += file.size
    accepted.push(file)
  }
  // 没有会话时，选择附件也要先建立会话，避免附件成为没有归属的孤立对象。
  if (accepted.length && !selectedConversation.value) {
    const created = await createAutomaticConversation()
    if (!created) return
  }
  const loaded = await Promise.allSettled(accepted.map(uploadFile))
  loaded.forEach((item) => item.status === 'fulfilled' ? attachments.value.push(item.value) : message.error(item.reason?.message || '上传附件失败'))
}

function removeAttachment(index: number) {
  attachments.value.splice(index, 1)
}

function appendRuntimeEvent(type: string, text: string, data?: Record<string, unknown>) {
  runtimeTimeline.value.push({ type, text, timestamp: new Date().toISOString(), data })
  // 命令输出可能非常密集；页面只保留最近事件，完整内容仍可通过 Invocation API 回放。
  if (runtimeTimeline.value.length > 120) runtimeTimeline.value.splice(0, runtimeTimeline.value.length - 120)
}

async function reconfirmInstructions() {
  if (!currentInvocationID.value || reconfirmingInstructions.value) return
  reconfirmingInstructions.value = true
  try {
    await request(`/api/v1/invocations/${encodeURIComponent(currentInvocationID.value)}/instructions/reconfirm`, { method: 'POST', body: '{}' })
    instructionConflict.value = null
    appendRuntimeEvent('instruction.reconfirmed', '已重新确认当前项目指令快照')
    message.success('项目指令已重新确认，请再次提交审批')
    if (currentInvocationID.value) void loadRuntimeDetails(currentInvocationID.value)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '重新确认项目指令失败')
  } finally {
    reconfirmingInstructions.value = false
  }
}

function runtimeEventText(type: string, data: Record<string, unknown>) {
  if (type === 'command.output') {
    const stream = String(data.stream || 'output')
    return `[${stream}] ${String(data.data || '')}`.trim()
  }
  if (type === 'tool.requested') return `调用工具：${String(data.name || '未知工具')}`
  if (type === 'tool.completed') return `工具完成：${String(data.name || '未知工具')}`
  if (type === 'tool.failed') return `工具失败：${String(data.name || '未知工具')}`
  if (type === 'context.compacted') return '上下文已压缩'
  if (type === 'instruction.conflict') return '项目指令发生变化，审批需要重新确认'
  if (type === 'instruction.reconfirmed') return '项目指令快照已重新确认'
  if (type === 'model.migration_required') return '模型能力已变化，需要迁移或重新发起任务'
  if (type === 'toolset.snapshot_invalidated') return '工具目录已变化，需要重新选择工具'
  if (type === 'subagent.requested') return `请求子 Agent：${String(data.profile || '未知职责')}`
  if (type === 'subagent.started') return `子 Agent 已开始：${String(data.node_id || data.source_kind || data.run_id || '')}`
  if (type === 'subagent.completed') return `子 Agent 已完成：${String(data.node_id || data.source_kind || data.run_id || '')}`
  if (type === 'subagent.failed') return `子 Agent 失败：${String(data.error || data.run_id || '')}`
  if (type === 'subagent.cancelled') return `子 Agent 已取消：${String(data.run_id || '')}`
  if (type === 'subagent.expired') return `子 Agent 已超时：${String(data.run_id || '')}`
  if (type === 'subagent.group_completed') return `子 Agent 组完成：${String(data.status || '')}`
  if (type === 'retrieval.requested') return '开始并行检索'
  if (type === 'retrieval.source_completed') return `检索来源完成：${String(data.source_kind || '')}`
  if (type === 'retrieval.deduplicated') return `证据去重：${String(data.before || 0)} → ${String(data.after || 0)}`
  if (type === 'retrieval.completed') return `检索完成：${String(data.evidence_count || 0)} 条证据`
  if (type === 'runtime.notice' && data.code === 'tool_execution') return `工具执行：${String(data.outcome || '未知')}`
  return type
}

async function resolvePendingApproval(choice: PendingApprovalChoice) {
	const approval = pendingApproval.value
	if (!approval || resolvingApproval.value) return
	resolvingApproval.value = true
	try {
		await request(`/api/v1/approvals/${encodeURIComponent(approval.id)}/resolve`, {
			method: 'POST', body: JSON.stringify({ choice_id: choice.id, reason: `WebUI 选择：${choice.label}` }),
		})
		appendRuntimeEvent('approval.resolved', choice.approved ? `已选择“${choice.label}”，Agent 正在恢复` : `已选择“${choice.label}”，Agent 正在处理拒绝结果`)
		pendingApproval.value = null
		statusText.value = choice.approved ? 'Agent 恢复运行中…' : '已拒绝，Agent 处理中…'
    if (currentInvocationID.value) void loadRuntimeDetails(currentInvocationID.value)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '审批处理失败')
  } finally {
    resolvingApproval.value = false
  }
}

// togglePendingUserInputOption 只修改当前问题卡片的本地选择，不把选项文案
// 直接当作恢复值；提交时会同时发送稳定 ID 和展示标签。
function togglePendingUserInputOption(optionID: string) {
  const input = pendingUserInput.value
  if (!input || input.kind === 'text') return
  if (!input.allowMultiple) {
    input.selectedIDs = [optionID]
    return
  }
  const index = input.selectedIDs.indexOf(optionID)
  if (index >= 0) {
    input.selectedIDs.splice(index, 1)
    return
  }
  if (input.maxSelections > 0 && input.selectedIDs.length >= input.maxSelections) {
    message.warning(`本题最多选择 ${input.maxSelections} 项`)
    return
  }
  input.selectedIDs.push(optionID)
}

// resolvePendingUserInput 将 UI 选择转换为 ADK RequestInput 的结构化
// FunctionResponse。审批按钮不经过这里，二者保持完全独立的恢复协议。
async function resolvePendingUserInput(skip = false) {
  const input = pendingUserInput.value
  if (!input || resolvingUserInput.value) return
  let response: Record<string, unknown>
  if (skip) {
    response = { skipped: true }
  } else if (input.kind === 'text') {
    const text = input.freeform.trim()
    if (!text) {
      message.warning('请先输入答案')
      return
    }
    response = { text }
  } else {
    const selected = input.options.filter((option) => input.selectedIDs.includes(option.id))
    const customText = input.freeform.trim()
    if (customText && input.allowFreeform && selected.length === 0) {
      response = { text: customText }
    } else {
      if (!selected.length) {
        message.warning('请至少选择一项')
        return
      }
      if (!input.allowMultiple && selected.length !== 1) {
        message.warning('本题只能选择一个选项')
        return
      }
      if (input.minSelections > 0 && selected.length < input.minSelections) {
        message.warning(`本题至少选择 ${input.minSelections} 项`)
        return
      }
      if (input.maxSelections > 0 && selected.length > input.maxSelections) {
        message.warning(`本题最多选择 ${input.maxSelections} 项`)
        return
      }
      response = {
        selected_ids: selected.map((option) => option.id),
        selected_labels: selected.map((option) => option.label),
      }
      if (customText && input.allowFreeform) response.text = customText
    }
  }
  resolvingUserInput.value = true
  try {
    await request(`/api/v1/invocations/${encodeURIComponent(input.invocationID)}/resume`, {
      method: 'POST',
      body: JSON.stringify({ wait_id: input.id, name: 'adk_request_input', response }),
    })
    appendRuntimeEvent('user_input.resolved', skip ? '已跳过当前问题' : '已提交当前问题答案')
    pendingUserInput.value = null
    statusText.value = input.step > 0 && input.totalSteps > input.step ? '进入下一题…' : 'Agent 恢复运行中…'
    if (currentInvocationID.value) void loadRuntimeDetails(currentInvocationID.value)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '提交回答失败')
  } finally {
    resolvingUserInput.value = false
  }
}

async function scrollToBottom() {
  await nextTick()
  if (messagePanel.value) messagePanel.value.scrollTop = messagePanel.value.scrollHeight
}

async function send() {
  const text = inputText.value.trim()
  const model = selectedModel.value.trim()
  if ((!text && !attachments.value.length) || !model || sending.value || preparingConversation.value) return
  let item: Conversation | undefined = selectedConversation.value
  if (!item) {
    // 删除全部会话后仍允许直接发送，首次消息会自动创建新的普通会话。
    item = (await createAutomaticConversation()) || undefined
  }
  if (!item || item.status !== 'active') return
  const outgoing = attachments.value.map((item) => ({ ...item }))
  conversationMessages.value.push({ role: 'user', text, attachment_count: outgoing.length, timestamp: new Date().toISOString() })
  conversationMessages.value.push({ role: 'assistant', text: '', timestamp: new Date().toISOString() })
  inputText.value = ''
  attachments.value = []
  sending.value = true
  statusText.value = 'Agent 运行中…'
  runtimeTimeline.value = []
  pendingApproval.value = null
  pendingUserInput.value = null
  await scrollToBottom()
  try {
    await streamChat({ user_id: userID.value, conversation_id: item.id, provider_id: providerID.value, model_id: model, message: text, attachments: outgoing, stream: true, reasoning_effort: reasoningEffort.value || undefined }, (type, data) => {
      const last = conversationMessages.value[conversationMessages.value.length - 1]
      if (type === 'message' && last) last.text = `${last.text || ''}${String(data.delta || '')}`
      if (type === 'done' && last && !last.text) last.text = String(data.text || '')
      if (type === 'done' && data.invocation_id) currentInvocationID.value = String(data.invocation_id)
      if (type === 'approval') {
        if (data.invocation_id) currentInvocationID.value = String(data.invocation_id)
        pendingUserInput.value = null
        const rawChoices = (Array.isArray(data.choices) ? data.choices : [])
          .map((item: any) => ({ id: String(item?.id || ''), label: String(item?.label || ''), approved: Boolean(item?.approved) }))
          .filter((item: PendingApprovalChoice) => (item.id === 'approve' || item.id === 'reject') && item.label)
        const approvalDefaults = [{ id: 'approve', label: '允许一次', approved: true }, { id: 'reject', label: '拒绝', approved: false }]
        // 审批永远是二元权限决策；普通多选题由 input 事件单独渲染。
        const choices = approvalDefaults.map((fallback) => rawChoices.find((item: PendingApprovalChoice) => item.id === fallback.id) || fallback)
        pendingApproval.value = {
          id: String(data.approval_id || ''), toolName: String(data.tool_name || '工作区操作'),
          hint: String(data.hint || '该操作需要确认'), args: (data.args || {}) as Record<string, unknown>, choices,
        }
        appendRuntimeEvent('approval.requested', pendingApproval.value.hint)
        statusText.value = '等待用户批准…'
      }
      if (type === 'input') {
        if (data.invocation_id) currentInvocationID.value = String(data.invocation_id)
        pendingApproval.value = null
        const options = (Array.isArray(data.options) ? data.options : [])
          .map((item: any) => ({ id: String(item?.id || ''), label: String(item?.label || ''), description: String(item?.description || ''), recommended: Boolean(item?.recommended) }))
          .filter((item: PendingUserInputOption) => item.id && item.label)
        pendingUserInput.value = {
          id: String(data.request_id || data.wait_id || ''), invocationID: String(data.invocation_id || currentInvocationID.value || ''),
          title: String(data.title || ''), question: String(data.question || data.message || '请提供输入'),
          kind: String(data.kind || 'text'), options, allowMultiple: Boolean(data.allow_multiple),
          allowFreeform: Boolean(data.allow_freeform), allowSkip: Boolean(data.allow_skip),
          minSelections: Number(data.min_selections || 0), maxSelections: Number(data.max_selections || 0),
          step: Number(data.step || 0), totalSteps: Number(data.total_steps || 0), placeholder: String(data.placeholder || '输入你的答案'),
          selectedIDs: [], freeform: '',
        }
        appendRuntimeEvent('user_input.requested', pendingUserInput.value.question, data)
        statusText.value = '等待你的回答…'
      }
      if (type === 'runtime') {
        const eventName = String(data.event || 'runtime')
        const eventData = (data.data || {}) as Record<string, unknown>
        appendRuntimeEvent(eventName, runtimeEventText(eventName, eventData), eventData)
        if (eventName === 'instruction.conflict') {
          instructionConflict.value = {
            previous: Array.isArray(eventData.previous) ? eventData.previous as InstructionSnapshotMeta[] : [],
            current: Array.isArray(eventData.current) ? eventData.current as InstructionSnapshotMeta[] : [],
            reason: String(eventData.reason || ''),
          }
          statusText.value = '项目指令已变化，等待重新确认'
        }
        if (eventName === 'instruction.reconfirmed') instructionConflict.value = null
      }
      if (type === 'error') throw new Error(String(data.error || 'Agent 运行失败'))
      void scrollToBottom()
    })
    statusText.value = '完成'
    await store.reloadConversations(userID.value)
    contextStatus.value = await readConversationContext(userID.value, item.id)
    if (currentInvocationID.value) await loadRuntimeDetails(currentInvocationID.value)
  } catch (error) {
    const last = conversationMessages.value[conversationMessages.value.length - 1]
    if (last) last.text = error instanceof Error ? error.message : 'Agent 运行失败'
    statusText.value = '运行失败'
    if (currentInvocationID.value) void loadRuntimeDetails(currentInvocationID.value)
  } finally {
    sending.value = false
  }
}

watch(() => route.query.conversation, (value) => {
  if (typeof value === 'string' && value !== conversationID.value && store.conversations.some((item) => item.id === value)) {
    void selectConversation(value, false)
  } else if (!value && conversationID.value) {
    conversationID.value = ''
    conversationMessages.value = []
    contextStatus.value = { compressed: false, compaction_count: 0 }
  }
})
watch(providerID, () => {
  // 切换供应商后只回落到该供应商已启用的模型目录，避免把旧供应商的 ID 带到新路由。
  const catalog = (store.providers.find((item) => item.id === providerID.value)?.models || []).filter((item) => item.enabled)
  if (!catalog.some((item) => item.id === selectedModel.value)) selectedModel.value = catalog[0]?.id || ''
})
watch(userID, async () => {
  await store.reloadConversations(userID.value)
  await ensureConversation()
})

onMounted(async () => {
  await store.loadAll()
  if (!store.conversations.length) await store.reloadConversations(userID.value)
  providerID.value = store.defaults.provider_id || store.providers[0]?.id || ''
  selectedModel.value = store.defaults.model_id || ''
  profileID.value = ''
  applyProfile('')
  await ensureConversation()
})
</script>

<template>
  <div class="chat-page-view" :class="{ 'is-empty': !conversationMessages.length }">
    <!-- 左栏：按项目组织的对话列表，对应参考图的会话侧栏。 -->
    <aside class="chat-rail">
      <div class="chat-rail-brand">
        <span class="chat-rail-brand-mark" aria-hidden="true">A</span>
        <span class="chat-rail-brand-copy"><strong>Abot</strong><small>Chat</small></span>
      </div>
      <button type="button" class="rail-nav-item" @click="createConversation">
        <span class="rail-nav-icon"><AppIcon name="plus" :size="15" /></span>创建对话
      </button>
      <NScrollbar class="chat-rail-scroll">
        <div class="rail-section-head">
          <span>项目</span>
          <button type="button" class="rail-section-add" title="新建项目" aria-label="新建项目" @click="showProjectDialog = true"><AppIcon name="plus" :size="14" /></button>
        </div>
        <div v-for="workspace in projectWorkspaces" :key="workspace.id" class="rail-project">
          <div class="rail-project-head">
            <button type="button" class="rail-project-toggle" :aria-label="`${isProjectExpanded(workspace) ? '收起' : '展开'}${workspace.name}`" @click="toggleProject(workspace)">
              <span class="rail-chevron">{{ isProjectExpanded(workspace) ? '⌄' : '›' }}</span>
            </button>
            <span class="rail-project-name" :title="workspace.root_path"><span class="rail-folder"><AppIcon name="folder" :size="14" /></span><span class="rail-label">{{ workspace.name }}</span></span>
            <button type="button" class="rail-project-add" :aria-label="`在${workspace.name}中新建对话`" @click="createProjectConversation(workspace)"><AppIcon name="plus" :size="14" /></button>
          </div>
          <div v-if="isProjectExpanded(workspace)" class="rail-children">
            <div v-for="item in projectConversations(workspace)" :key="item.id" class="rail-conversation-row">
              <button type="button" class="rail-conversation" :class="{ active: isCurrentConversation(item) }" @click="selectConversation(item.id)">
                <span class="rail-dot" /><span class="rail-label">{{ item.title || '新对话' }}</span>
              </button>
              <span class="rail-item-actions"><ConversationActions :item="item" @archive="archiveConversation" @unarchive="unarchiveConversation" @remove="deleteConversation" /></span>
            </div>
            <span v-if="!projectConversations(workspace).length" class="rail-empty">暂无对话</span>
          </div>
        </div>

        <div v-if="looseConversations.length" class="rail-group">
          <span class="rail-group-label">未归属项目</span>
          <div v-for="item in looseConversations" :key="item.id" class="rail-conversation-row">
            <button type="button" class="rail-conversation" :class="{ active: isCurrentConversation(item) }" @click="selectConversation(item.id)">
              <span class="rail-dot" /><span class="rail-label">{{ item.title || '新对话' }}</span>
            </button>
            <span class="rail-item-actions"><ConversationActions :item="item" @archive="archiveConversation" @unarchive="unarchiveConversation" @remove="deleteConversation" /></span>
          </div>
        </div>

        <div v-if="archivedConversations.length" class="rail-group">
          <span class="rail-group-label">已归档</span>
          <div v-for="item in archivedConversations" :key="item.id" class="rail-conversation-row">
            <button type="button" class="rail-conversation archived" :class="{ active: isCurrentConversation(item) }" @click="selectConversation(item.id)">
              <span class="rail-dot" /><span class="rail-label">{{ item.title || '新对话' }}</span>
            </button>
            <span class="rail-item-actions"><ConversationActions :item="item" @archive="archiveConversation" @unarchive="unarchiveConversation" @remove="deleteConversation" /></span>
          </div>
        </div>

        <span v-if="!projectWorkspaces.length && !looseConversations.length && !archivedConversations.length" class="rail-empty">还没有对话，先新建一个。</span>
      </NScrollbar>
      <div class="chat-rail-footer">
        <button type="button" class="rail-nav-item" @click="showSettings = true">
        <span class="rail-nav-icon"><AppIcon name="settings" :size="15" /></span>设置
        </button>
      </div>
    </aside>

    <section class="chat-main">
      <header class="chat-topbar">
        <div class="chat-topbar-main">
          <strong class="chat-topbar-title">{{ selectedConversation?.title || '新对话' }}</strong>
          <span class="chat-topbar-sub">{{ selectedConversation ? workspaceName(selectedConversation.workspace_id) : '尚未选择对话' }}</span>
        </div>
        <div class="chat-topbar-actions">
          <span class="chat-topbar-status">{{ displayStatus }}</span>
          <button type="button" class="chat-switch-button" title="返回 Bot 管理台" @click="router.push({ name: 'status' })">
            <AppIcon name="bot" :size="16" />Bot
          </button>
        </div>
      </header>

      <main ref="messagePanel" class="chat-thread">
      <NSpin v-if="loadingMessages" size="small" />
      <div v-else-if="!selectedConversation" class="chat-empty-state">
        <h1>今天想聊点什么？</h1>
        <p>从左侧新建或选择一个对话，把问题交给 Abot。</p>
      </div>
      <div v-else-if="!conversationMessages.length" class="chat-empty-state">
        <h1>今天想聊点什么？</h1>
        <div class="quick-prompts">
          <button v-for="prompt in quickPrompts" :key="prompt" type="button" @click="inputText = prompt">{{ prompt }}</button>
        </div>
      </div>
      <div v-else class="message-list">
        <div v-for="(item, index) in conversationMessages" :key="`${item.timestamp || ''}-${index}`" class="message-line" :class="item.role === 'user' ? 'from-user' : 'from-assistant'">
          <div class="message-avatar">{{ item.role === 'user' ? '我' : 'A' }}</div>
          <div class="message-content">
            <div class="message-meta">{{ item.role === 'user' ? '你' : 'Abot' }}<span v-if="item.timestamp">{{ formatContextTime(item.timestamp) }}</span></div>
            <div class="message-bubble">
              <pre v-if="item.text">{{ item.text }}</pre>
              <span v-if="!item.text && item.role === 'assistant' && sending && index === conversationMessages.length - 1" class="typing-indicator"><i /><i /><i /></span>
              <div v-if="item.attachment_count" class="message-attachment-count"><AppIcon name="paperclip" :size="13" /> {{ item.attachment_count }} 个附件</div>
            </div>
          </div>
        </div>
      </div>

      <section v-if="runtimeTimeline.length || pendingApproval || pendingUserInput" class="chat-activity">
        <div class="chat-activity-heading">
          <span><i class="activity-pulse" :class="{ active: sending }" />运行活动</span>
          <NTag v-if="pendingApproval" size="small" type="warning" round>需要你的确认</NTag>
          <NTag v-else-if="pendingUserInput" size="small" type="info" round>等待回答</NTag>
          <span v-else class="chat-activity-count">{{ runtimeTimeline.length }} 条记录</span>
        </div>
        <div v-if="pendingApproval" class="chat-approval-card">
          <div class="approval-icon"><AppIcon name="shield" :size="16" /></div>
          <div class="approval-copy">
            <strong>{{ pendingApproval.toolName }}</strong>
            <p>{{ pendingApproval.hint }}</p>
            <code v-if="Object.keys(pendingApproval.args).length">{{ JSON.stringify(pendingApproval.args) }}</code>
          </div>
          <NSpace class="approval-actions">
            <NButton v-for="(choice, index) in pendingApproval.choices" :key="choice.id" size="small" :type="index === 0 && choice.approved ? 'primary' : 'default'" :secondary="index !== 0 || !choice.approved" :loading="resolvingApproval" @click="resolvePendingApproval(choice)">{{ choice.label }}</NButton>
          </NSpace>
        </div>
        <div v-if="pendingUserInput" class="chat-user-input-card">
          <div class="user-input-heading">
            <div>
              <span v-if="pendingUserInput.title" class="user-input-title">{{ pendingUserInput.title }}</span>
              <strong>{{ pendingUserInput.question }}</strong>
            </div>
            <span v-if="pendingUserInput.step && pendingUserInput.totalSteps" class="user-input-step">{{ pendingUserInput.step }}/{{ pendingUserInput.totalSteps }}</span>
          </div>
          <div v-if="pendingUserInput.options.length" class="user-input-options">
            <button v-for="(option, index) in pendingUserInput.options" :key="option.id" type="button" class="user-input-option" :class="{ selected: pendingUserInput.selectedIDs.includes(option.id) }" @click="togglePendingUserInputOption(option.id)">
              <span class="user-input-option-index"><AppIcon v-if="pendingUserInput.allowMultiple && pendingUserInput.selectedIDs.includes(option.id)" name="check" :size="14" /><template v-else-if="!pendingUserInput.allowMultiple">{{ index + 1 }}</template></span>
              <span class="user-input-option-copy"><strong>{{ option.label }}</strong><small v-if="option.description">{{ option.description }}</small></span>
              <NTag v-if="option.recommended" size="small" type="info" :bordered="false">推荐</NTag>
            </button>
          </div>
          <NInput v-if="pendingUserInput.kind === 'text' || pendingUserInput.allowFreeform" v-model:value="pendingUserInput.freeform" type="textarea" :autosize="{ minRows: 1, maxRows: 4 }" :placeholder="pendingUserInput.placeholder" class="user-input-freeform" />
          <div class="user-input-actions">
            <NButton v-if="pendingUserInput.allowSkip" secondary :loading="resolvingUserInput" @click="resolvePendingUserInput(true)">跳过本题</NButton>
            <NButton type="primary" :loading="resolvingUserInput" @click="resolvePendingUserInput()">{{ pendingUserInput.step && pendingUserInput.totalSteps && pendingUserInput.step < pendingUserInput.totalSteps ? '下一题' : '提交' }}</NButton>
          </div>
        </div>
        <div v-if="instructionConflict" class="chat-conflict-card">
          <div class="conflict-icon"><AppIcon name="refresh" :size="16" /></div>
          <div class="conflict-copy">
            <strong>项目指令已变化</strong>
            <p>为避免把新规则静默带入副作用，请先确认当前指令快照，再重新提交审批。</p>
            <div class="conflict-snapshot-columns">
              <div>
                <span class="conflict-label">审批时快照</span>
                <code v-for="item in instructionConflict.previous || []" :key="`old-${item.path}-${item.content_digest}`">{{ item.path || '未知路径' }} · {{ item.content_digest || '无 digest' }}</code>
                <code v-if="!(instructionConflict.previous || []).length">（无项目指令）</code>
              </div>
              <div>
                <span class="conflict-label">当前发现</span>
                <code v-for="item in instructionConflict.current || []" :key="`new-${item.path}-${item.content_digest}`">{{ item.path || '未知路径' }} · {{ item.content_digest || '无 digest' }}</code>
                <code v-if="!(instructionConflict.current || []).length">（无项目指令）</code>
              </div>
            </div>
          </div>
          <NButton size="small" type="warning" :loading="reconfirmingInstructions" @click="reconfirmInstructions">重新确认当前指令</NButton>
        </div>
        <div v-if="runtimeTimeline.length" class="runtime-log-list">
          <div v-for="(item, index) in runtimeTimeline" :key="`${item.timestamp}-${index}`" class="runtime-log-line">
            <NTag size="small" :bordered="false">{{ item.type }}</NTag><code>{{ item.text }}</code>
          </div>
        </div>
        <div v-if="runtimeDetails || loadingRuntimeDetails" class="runtime-detail-card">
          <div class="runtime-detail-heading">
            <span>任务详情</span>
            <NButton text size="tiny" :loading="loadingRuntimeDetails" @click="currentInvocationID && loadRuntimeDetails(currentInvocationID)">刷新</NButton>
          </div>
          <div v-if="runtimeDetails?.trace" class="runtime-detail-grid">
            <span v-if="runtimeDetails?.snapshot?.workflow_phase">工作阶段<strong>{{ runtimeDetails.snapshot.workflow_phase }}</strong></span>
            <span>总耗时<strong>{{ formatDuration(runtimeDetails.trace.metrics.total_wall_ms) }}</strong></span>
            <span>实际运行<strong>{{ formatDuration(runtimeDetails.trace.metrics.active_runtime_ms) }}</strong></span>
            <span>模型调用<strong>{{ runtimeDetails.trace.metrics.model_calls }}</strong></span>
            <span>工具调用<strong>{{ runtimeDetails.trace.metrics.tool_calls }}</strong></span>
            <span>审批等待<strong>{{ formatDuration(runtimeDetails.trace.metrics.approval_wait_ms) }}</strong></span>
            <span>Token<strong>{{ formatUsageValue(runtimeDetails.trace.usage.total_tokens) }}</strong></span>
            <span v-if="runtimeDetails.trace.usage.tool_use_prompt_tokens?.known">工具输入 Token<strong>{{ formatUsageValue(runtimeDetails.trace.usage.tool_use_prompt_tokens) }}</strong></span>
            <span v-if="runtimeDetails.trace.usage.compaction?.model_calls">上下文压缩<strong>{{ runtimeDetails.trace.usage.compaction.model_calls }} 次 · {{ formatUsageValue(runtimeDetails.trace.usage.compaction.total_tokens) }} Token</strong></span>
          </div>
          <div v-if="runtimeDetails?.snapshot?.budget?.context_window" class="runtime-budget-line">
            上下文 {{ runtimeDetails.snapshot.budget.estimated_input || 0 }} / {{ runtimeDetails.snapshot.budget.context_window }} token
            <span v-if="runtimeDetails.snapshot.budget.budget_exhausted"> · 预算已触顶</span>
          </div>
          <div v-if="runtimeDetails?.subagents?.groups?.length" class="runtime-subagent-list">
            <div class="runtime-subagent-heading">子 Agent 编排</div>
            <div v-for="group in runtimeDetails.subagents.groups" :key="group.id" class="runtime-subagent-group">
              <div class="runtime-subagent-group-heading">
                <span><NTag size="small" :type="subAgentTagType(group.status)">{{ group.status }}</NTag> {{ subAgentProfileLabel(group.profile) }}</span>
                <span>{{ group.completed_count }}/{{ group.expected_count }} 完成<span v-if="group.failed_count"> · {{ group.failed_count }} 失败</span></span>
              </div>
              <p v-if="group.purpose">{{ group.purpose }}</p>
              <p v-if="group.error_summary" class="runtime-subagent-error">{{ group.error_summary }}</p>
              <div v-if="runtimeDetails.subagents.details[group.id]?.runs?.length" class="runtime-subagent-runs">
                <span v-for="run in runtimeDetails.subagents.details[group.id].runs" :key="run.id">
                  <NTag size="tiny" :type="subAgentTagType(run.status)">{{ run.status }}</NTag>
                  {{ run.source_kind || `节点 ${run.ordinal + 1}` }}<template v-if="run.error">：{{ run.error }}</template>
                </span>
              </div>
              <div v-if="runtimeDetails.subagents.details[group.id]?.evidence?.length" class="runtime-subagent-evidence">
                证据 {{ runtimeDetails.subagents.details[group.id].evidence?.length }} 条：{{ runtimeDetails.subagents.details[group.id].evidence?.slice(0, 3).map((item) => item.title || item.locator || item.source_kind).join(' · ') }}
              </div>
            </div>
          </div>
          <div v-if="runtimeDetails?.plan?.steps?.length" class="runtime-plan-list">
            <div v-for="step in runtimeDetails.plan.steps" :key="step.id" class="runtime-plan-step">
              <NTag size="small" :type="step.status === 'completed' ? 'success' : step.status === 'blocked' ? 'error' : step.status === 'in_progress' ? 'info' : 'default'">{{ step.status }}</NTag>
              <span>{{ step.title }}</span>
            </div>
          </div>
          <div v-if="runtimeDetails?.verifications?.length" class="runtime-verification-list">
            <span v-for="item in runtimeDetails.verifications" :key="item.id"><NTag size="small" :type="item.status === 'passed' ? 'success' : item.status === 'failed' ? 'error' : 'warning'">{{ item.status }}</NTag> {{ item.kind }}</span>
          </div>
          <div v-if="runtimeDetails?.capabilities?.result?.disabled_features?.length" class="runtime-capability-warning">
            模型已降级：{{ runtimeDetails.capabilities.result.disabled_features.map((item) => item.feature).join('、') }}
          </div>
          <div v-if="runtimeDetails?.result" class="runtime-result-card">
            <div class="runtime-result-heading">
              <span>结果报告</span>
              <NTag size="small" :type="resultTagType(runtimeDetails.result.status)">{{ runtimeDetails.result.status }}</NTag>
            </div>
            <p>{{ runtimeDetails.result.summary }}</p>
            <div v-if="runtimeDetails.result.criteria?.length" class="runtime-result-items">
              <span v-for="criterion in runtimeDetails.result.criteria" :key="criterion.id">
                <NTag size="small" :type="criterion.status === 'satisfied' ? 'success' : 'warning'">{{ criterion.status }}</NTag>
                {{ criterion.description }}
              </span>
            </div>
            <div v-if="runtimeDetails.result.baseline" class="runtime-result-baseline">
              <span>任务开始前工作区：{{ runtimeDetails.result.baseline.repository_type }} · {{ runtimeDetails.result.baseline.status_known ? '状态已捕获' : '状态未知' }}</span>
              <span v-if="runtimeDetails.result.preexisting_changes?.length">已有 {{ runtimeDetails.result.preexisting_changes.length }} 项修改</span>
              <span v-if="runtimeDetails.result.preexisting_changes?.length" class="runtime-result-baseline-paths">
                {{ runtimeDetails.result.preexisting_changes.slice(0, 5).map((item) => `${item.status} ${item.path}`).join(' · ') }}<template v-if="runtimeDetails.result.preexisting_changes.length > 5"> · …</template>
              </span>
              <span v-if="runtimeDetails.result.baseline.truncated || !runtimeDetails.result.baseline.status_known" class="runtime-result-baseline-warning">无法完整归因所有已有修改</span>
            </div>
            <div v-if="runtimeDetails.result.agent_changes" class="runtime-result-changes">
              <span>本次变更归因：{{ runtimeDetails.result.agent_changes.status }} · {{ runtimeDetails.result.agent_changes.paths?.length || 0 }} 个路径</span>
              <span v-if="runtimeDetails.result.agent_changes.paths?.length" class="runtime-result-changes-paths">
                {{ runtimeDetails.result.agent_changes.paths.slice(0, 5).map((item) => item.path).join(' · ') }}<template v-if="runtimeDetails.result.agent_changes.paths.length > 5"> · …</template>
              </span>
              <span v-if="runtimeDetails.result.agent_changes.overlap_paths?.length" class="runtime-result-overlap">与任务开始前已有修改重叠 {{ runtimeDetails.result.agent_changes.overlap_paths.length }} 个路径：{{ runtimeDetails.result.agent_changes.overlap_paths.slice(0, 5).join(' · ') }}<template v-if="runtimeDetails.result.agent_changes.overlap_paths.length > 5"> · …</template></span>
              <span v-if="runtimeDetails.result.agent_changes.unattributed_paths?.length" class="runtime-result-overlap">终态发现未归因路径 {{ runtimeDetails.result.agent_changes.unattributed_paths.length }} 个：{{ runtimeDetails.result.agent_changes.unattributed_paths.slice(0, 5).join(' · ') }}<template v-if="runtimeDetails.result.agent_changes.unattributed_paths.length > 5"> · …</template></span>
              <span v-if="runtimeDetails.result.agent_changes.status !== 'known'" class="runtime-result-baseline-warning">部分路径可能来自命令或未确认的工作区扫描</span>
            </div>
            <div v-if="runtimeDetails.result.blockers?.length || runtimeDetails.result.unknown_states?.length" class="runtime-result-warning">
              仍有 {{ (runtimeDetails.result.blockers?.length || 0) + (runtimeDetails.result.unknown_states?.length || 0) }} 项阻塞或未知状态
            </div>
          </div>
        </div>
      </section>
    </main>

    <footer class="chat-composer-shell">
      <div v-if="attachments.length" class="attachment-strip">
        <div v-for="(attachment, index) in attachments" :key="`${attachment.name}-${index}`" class="attachment-chip">
          <span><AppIcon name="paperclip" :size="13" /> {{ attachment.name }}</span><NButton text type="error" aria-label="移除附件" @click="removeAttachment(index)"><AppIcon name="close" :size="15" /></NButton>
        </div>
      </div>
      <div class="chat-composer">
        <NInput v-model:value="inputText" class="chat-composer-input" type="textarea" :autosize="{ minRows: 1, maxRows: 6 }" placeholder="给 Abot 发消息…" :disabled="(selectedConversation && selectedConversation.status !== 'active') || sending || preparingConversation" @keydown.ctrl.enter.prevent="send" @keydown.meta.enter.prevent="send" />
        <div class="chat-composer-toolbar">
          <div class="chat-composer-tools">
            <input ref="fileInput" type="file" multiple class="visually-hidden" accept="image/*,application/pdf,text/*,.json,.md,.csv,.doc,.docx,.xls,.xlsx" @change="chooseFiles" />
            <NButton quaternary circle aria-label="添加文件" :disabled="(selectedConversation && selectedConversation.status !== 'active') || sending || preparingConversation" @click="fileInput?.click()"><AppIcon name="paperclip" :size="16" /></NButton>
            <NSelect v-model:value="topbarModelValue" :options="topbarModelOptions" size="tiny" class="composer-select composer-model-select" placeholder="选择模型" />
            <NSelect v-model:value="reasoningEffort" :options="reasoningEffortOptions" size="tiny" class="composer-select composer-effort-select" />
          </div>
          <div class="chat-send-hint">⌘↵ 发送</div>
          <NButton class="chat-send-button" type="primary" circle :loading="sending || preparingConversation" :disabled="(selectedConversation && selectedConversation.status !== 'active') || sending || preparingConversation || (!inputText.trim() && !attachments.length)" aria-label="发送消息" @click="send"><AppIcon name="arrow-up" :size="16" /></NButton>
        </div>
      </div>
      <p class="chat-disclaimer">Abot 可能会出错，请检查重要信息。</p>
    </footer>
    </section>

    <ProjectCreateDialog v-model:show="showProjectDialog" />
    <ProviderDialog v-model:show="showProviderDialog" :provider="editingProvider" />

    <transition name="chat-settings">
      <aside v-if="showSettings" class="chat-settings-panel">
        <div class="chat-settings-header"><div><span class="eyebrow">CONVERSATION</span><strong>对话设置</strong></div><NButton quaternary circle aria-label="关闭设置" @click="showSettings = false"><AppIcon name="close" :size="16" /></NButton></div>
        <NForm label-placement="top" :show-feedback="false">
          <NFormItem label="对话"><NSelect :value="conversationID" :options="activeConversations.map((item) => ({ label: conversationLabel(item), value: item.id }))" placeholder="选择一个对话" @update:value="selectConversation" /></NFormItem>
          <NSpace wrap><NButton type="primary" @click="createConversation"><AppIcon name="plus" :size="14" />新建对话</NButton><NButton secondary :disabled="selectedConversation?.status !== 'active'" @click="archiveConversation()">归档</NButton><NButton secondary :disabled="selectedConversation?.status !== 'archived'" @click="unarchiveConversation()">恢复</NButton><NButton tertiary type="error" :disabled="selectedConversation?.status !== 'archived'" @click="deleteConversation()">删除</NButton></NSpace>
          <div class="chat-settings-divider" />
          <NFormItem label="配置文件"><NSelect v-model:value="profileID" :options="profileOptions" @update:value="bindProfile" /><small>系统默认 → 机器人 → 当前对话。</small></NFormItem>
          <NFormItem label="子 Agent"><NSelect :value="subAgentMode" :options="subAgentModeOptions" :disabled="selectedConversation?.status !== 'active'" @update:value="setSubAgentMode" /><small>当前：{{ subAgentEffectiveEnabled ? '已启用' : '已停用' }}。也可以在聊天中使用 /subagent on、/subagent off 或 /subagent inherit。</small></NFormItem>
          <NFormItem label="供应商"><NSelect v-model:value="providerID" :options="providerOptions" placeholder="选择供应商" /></NFormItem>
          <div class="settings-provider-list">
            <div v-for="item in store.providers" :key="item.id" class="settings-provider-row">
              <span class="settings-provider-name">{{ item.name }}</span>
              <span class="rail-item-actions always">
                <button type="button" class="rail-item-action" title="编辑" aria-label="编辑供应商" @click="openProviderDialog(item)"><AppIcon name="edit" :size="14" /></button>
                <button type="button" class="rail-item-action danger" title="删除" aria-label="删除供应商" @click="removeProvider(item)"><AppIcon name="trash" :size="14" /></button>
              </span>
            </div>
            <button type="button" class="settings-provider-add" @click="openProviderDialog(null)"><AppIcon name="plus" :size="14" />添加供应商</button>
          </div>
          <NFormItem label="模型"><NSelect v-model:value="modelSelectValue" :options="modelOptions" placeholder="选择已配置模型" /></NFormItem>
          <NFormItem label="用户 ID"><NInput v-model:value="userID" /></NFormItem>
        </NForm>
        <NAlert v-if="statusText" :type="statusTagType" :show-icon="false">{{ statusText }}</NAlert>
      </aside>
    </transition>
  </div>
</template>
