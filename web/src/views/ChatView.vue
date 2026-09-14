<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { NAlert, NButton, NEmpty, NForm, NFormItem, NInput, NSelect, NSpace, NSpin, NTag, useMessage } from 'naive-ui'
import { readConversationContext, readConversationMessages, readInvocationCapabilities, readInvocationPlan, readInvocationResult, readInvocationRuntimeSnapshot, readInvocationToolSet, readInvocationTrace, readInvocationVerifications, request, streamChat, uploadArtifact } from '@/api'
import { useAppStore } from '@/stores/app'
import { useRoute, useRouter } from 'vue-router'
import type { ChatAttachment, CompletionReport, ConversationContextStatus, ConversationMessage, InvocationTrace, ModelCapabilitySnapshot, RuntimeSnapshot, TaskPlan, ToolSetSnapshot, VerificationRun } from '@/types'

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
const manualModelID = ref('')
const selectedModel = ref('')
const providerID = ref('')
const profileID = ref('')
const sending = ref(false)
const loadingMessages = ref(false)
const statusText = ref('')
const showSettings = ref(false)
type RuntimeTimelineItem = { type: string; text: string; timestamp: string; data?: Record<string, unknown> }
type PendingApproval = { id: string; toolName: string; hint: string; args: Record<string, unknown> }
type InstructionSnapshotMeta = { path?: string; scope_path?: string; source?: string; priority?: number; content_digest?: string }
type InstructionConflict = { previous?: InstructionSnapshotMeta[]; current?: InstructionSnapshotMeta[]; reason?: string }
const runtimeTimeline = ref<RuntimeTimelineItem[]>([])
const pendingApproval = ref<PendingApproval | null>(null)
const instructionConflict = ref<InstructionConflict | null>(null)
const resolvingApproval = ref(false)
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
} | null>(null)
const loadingRuntimeDetails = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)
const messagePanel = ref<HTMLElement | null>(null)

const selectedConversation = computed(() => store.conversations.find((item) => item.id === conversationID.value))
const activeConversations = computed(() => store.conversations)
const profileOptions = computed(() => [{ label: '使用系统默认配置', value: '' }, ...store.configProfiles.map((item) => ({ label: `${item.name}${item.is_default ? ' · 默认' : ''}`, value: item.id }))])
const providerOptions = computed(() => store.providers.map((item) => ({ label: item.name, value: item.id })))
const modelOptions = computed(() => {
  const provider = store.providers.find((item) => item.id === providerID.value)
  return [...(provider?.models || []).filter((item) => item.enabled).map((item) => ({ label: item.display_name || item.id, value: item.id })), { label: '手动输入目录外模型…', value: '__manual__' }]
})
const modelSelectValue = computed({
  get: () => modelOptions.value.some((item) => item.value === selectedModel.value) ? selectedModel.value : '__manual__',
  set: (value: string) => {
    selectedModel.value = value === '__manual__' ? manualModelID.value : value
  },
})
const currentModelLabel = computed(() => {
  const provider = store.providers.find((item) => item.id === providerID.value)
  return provider?.models.find((item) => item.id === selectedModel.value)?.display_name || selectedModel.value || '未选择模型'
})
const displayStatus = computed(() => {
  if (pendingApproval.value) return '等待审批'
  if (sending.value) return statusText.value || '运行中'
  if (statusText.value) return statusText.value
  if (!selectedConversation.value) return '选择一个对话'
  return selectedConversation.value.status === 'active' ? '就绪' : '已归档'
})
const statusTagType = computed<'default' | 'success' | 'warning' | 'error' | 'info'>(() => {
  if (pendingApproval.value) return 'warning'
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
  const [trace, snapshot, plan, result, verifications, toolset, capabilities] = await Promise.all([
    read(() => readInvocationTrace(invocationID)),
    read(() => readInvocationRuntimeSnapshot(invocationID)),
    read(() => readInvocationPlan(invocationID)),
    read(() => readInvocationResult(invocationID)),
    read(() => readInvocationVerifications(invocationID)),
    read(() => readInvocationToolSet(invocationID)),
    read(() => readInvocationCapabilities(invocationID)),
  ])
  runtimeDetails.value = { trace, snapshot, plan, result, verifications, toolset, capabilities }
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
    manualModelID.value = nextModel
  } else {
    // 切换到没有默认模型的配置时清掉旧值，避免把上一份配置的模型带入当前对话。
    selectedModel.value = ''
    manualModelID.value = ''
  }
}

async function selectConversation(id: string, updateRoute = true) {
  if (id !== conversationID.value) {
    statusText.value = ''
    runtimeTimeline.value = []
    pendingApproval.value = null
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

async function archiveConversation() {
  const item = selectedConversation.value
  if (!item || item.status !== 'active') return
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(item.id)}/archive?user_id=${encodeURIComponent(userID.value)}`, { method: 'POST', body: '{}' })
    await store.reloadConversations(userID.value)
    await selectConversation(item.id, false)
    message.success('对话已归档')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '归档失败')
  }
}

async function unarchiveConversation() {
  const item = selectedConversation.value
  if (!item || item.status !== 'archived') return
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(item.id)}/unarchive?user_id=${encodeURIComponent(userID.value)}`, { method: 'POST', body: '{}' })
    await store.reloadConversations(userID.value)
    await selectConversation(item.id, false)
    message.success('对话已恢复')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '恢复失败')
  }
}

async function deleteConversation() {
  const item = selectedConversation.value
  if (!item || item.status !== 'archived') return
  if (!window.confirm(`彻底删除“${item.title}”及全部消息？此操作不可恢复。`)) return
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(item.id)}?user_id=${encodeURIComponent(userID.value)}`, { method: 'DELETE' })
    await store.reloadConversations(userID.value)
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
  if (type === 'runtime.notice' && data.code === 'tool_execution') return `工具执行：${String(data.outcome || '未知')}`
  return type
}

async function resolvePendingApproval(approved: boolean) {
  const approval = pendingApproval.value
  if (!approval || resolvingApproval.value) return
  resolvingApproval.value = true
  try {
    await request(`/api/v1/approvals/${encodeURIComponent(approval.id)}/resolve`, {
      method: 'POST', body: JSON.stringify({ approved, reason: approved ? 'WebUI 批准' : 'WebUI 拒绝' }),
    })
    appendRuntimeEvent('approval.resolved', approved ? '已批准，Agent 正在恢复' : '已拒绝，Agent 正在处理拒绝结果')
    pendingApproval.value = null
    statusText.value = approved ? 'Agent 恢复运行中…' : '已拒绝，Agent 处理中…'
    if (currentInvocationID.value) void loadRuntimeDetails(currentInvocationID.value)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '审批处理失败')
  } finally {
    resolvingApproval.value = false
  }
}

async function scrollToBottom() {
  await nextTick()
  if (messagePanel.value) messagePanel.value.scrollTop = messagePanel.value.scrollHeight
}

async function send() {
  const text = inputText.value.trim()
  const model = selectedModel.value === '__manual__' ? manualModelID.value.trim() : selectedModel.value.trim()
  const item = selectedConversation.value
  if ((!text && !attachments.value.length) || !item || item.status !== 'active' || !model || sending.value) return
  const outgoing = attachments.value.map((item) => ({ ...item }))
  conversationMessages.value.push({ role: 'user', text, attachment_count: outgoing.length, timestamp: new Date().toISOString() })
  conversationMessages.value.push({ role: 'assistant', text: '', timestamp: new Date().toISOString() })
  inputText.value = ''
  attachments.value = []
  sending.value = true
  statusText.value = 'Agent 运行中…'
  runtimeTimeline.value = []
  pendingApproval.value = null
  await scrollToBottom()
  try {
    await streamChat({ user_id: userID.value, conversation_id: item.id, provider_id: providerID.value, model_id: model, message: text, attachments: outgoing, stream: true }, (type, data) => {
      const last = conversationMessages.value[conversationMessages.value.length - 1]
      if (type === 'message' && last) last.text = `${last.text || ''}${String(data.delta || '')}`
      if (type === 'done' && last && !last.text) last.text = String(data.text || '')
      if (type === 'done' && data.invocation_id) currentInvocationID.value = String(data.invocation_id)
      if (type === 'approval') {
        if (data.invocation_id) currentInvocationID.value = String(data.invocation_id)
        pendingApproval.value = {
          id: String(data.approval_id || ''), toolName: String(data.tool_name || '工作区操作'),
          hint: String(data.hint || '该操作需要批准'), args: (data.args || {}) as Record<string, unknown>,
        }
        appendRuntimeEvent('approval.requested', pendingApproval.value.hint)
        statusText.value = '等待用户批准…'
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
  if (!modelOptions.value.some((item) => item.value === selectedModel.value)) selectedModel.value = modelOptions.value[0]?.value || ''
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
  manualModelID.value = selectedModel.value
  profileID.value = ''
  applyProfile('')
  await ensureConversation()
})
</script>

<template>
  <div class="chat-page-view">
    <header class="chat-header">
      <div class="chat-header-main">
        <div class="chat-agent-avatar">A</div>
        <div class="chat-title-block">
          <div class="chat-title">{{ selectedConversation?.title || '新对话' }}</div>
          <div class="chat-subtitle">
            <span>{{ selectedConversation ? workspaceName(selectedConversation.workspace_id) : '选择一个对话开始' }}</span>
            <span class="chat-subtitle-dot">·</span>
            <span>{{ currentModelLabel }}</span>
          </div>
        </div>
      </div>
      <div class="chat-header-actions">
        <NTag round :type="statusTagType" :bordered="false">{{ displayStatus }}</NTag>
        <NButton quaternary circle aria-label="打开对话设置" @click="showSettings = !showSettings">⚙</NButton>
      </div>
    </header>

    <main ref="messagePanel" class="chat-thread">
      <NSpin v-if="loadingMessages" size="small" />
      <div v-else-if="!selectedConversation" class="chat-empty-state">
        <div class="chat-empty-avatar">A</div>
        <h1>准备好开始了吗？</h1>
        <p>创建或选择一个对话，把问题交给 Abot。</p>
        <NButton type="primary" size="large" @click="createConversation">＋ 新建对话</NButton>
      </div>
      <div v-else-if="!conversationMessages.length" class="chat-empty-state chat-empty-state-compact">
        <div class="chat-empty-avatar">✦</div>
        <h1>从这里开始</h1>
        <p>你可以直接提问，也可以让 Abot 读取和处理当前项目。</p>
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
              <div v-if="item.attachment_count" class="message-attachment-count">📎 {{ item.attachment_count }} 个附件</div>
            </div>
          </div>
        </div>
      </div>

      <section v-if="runtimeTimeline.length || pendingApproval" class="chat-activity">
        <div class="chat-activity-heading">
          <span><i class="activity-pulse" :class="{ active: sending }" />运行活动</span>
          <NTag v-if="pendingApproval" size="small" type="warning" round>需要你的确认</NTag>
          <span v-else class="chat-activity-count">{{ runtimeTimeline.length }} 条记录</span>
        </div>
        <div v-if="pendingApproval" class="chat-approval-card">
          <div class="approval-icon">!</div>
          <div class="approval-copy">
            <strong>{{ pendingApproval.toolName }}</strong>
            <p>{{ pendingApproval.hint }}</p>
            <code v-if="Object.keys(pendingApproval.args).length">{{ JSON.stringify(pendingApproval.args) }}</code>
          </div>
          <NSpace class="approval-actions">
            <NButton size="small" type="primary" :loading="resolvingApproval" @click="resolvePendingApproval(true)">批准</NButton>
            <NButton size="small" secondary :loading="resolvingApproval" @click="resolvePendingApproval(false)">拒绝</NButton>
          </NSpace>
        </div>
        <div v-if="instructionConflict" class="chat-conflict-card">
          <div class="conflict-icon">↻</div>
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
          <span>📎 {{ attachment.name }}</span><NButton text type="error" aria-label="移除附件" @click="removeAttachment(index)">×</NButton>
        </div>
      </div>
      <div class="chat-composer">
        <NInput v-model:value="inputText" class="chat-composer-input" type="textarea" :autosize="{ minRows: 1, maxRows: 6 }" placeholder="给 Abot 发消息…" :disabled="!selectedConversation || selectedConversation.status !== 'active' || sending" @keydown.ctrl.enter.prevent="send" @keydown.meta.enter.prevent="send" />
        <div class="chat-composer-toolbar">
          <div class="chat-composer-tools">
            <input ref="fileInput" type="file" multiple class="visually-hidden" accept="image/*,application/pdf,text/*,.json,.md,.csv,.doc,.docx,.xls,.xlsx" @change="chooseFiles" />
            <NButton quaternary circle aria-label="添加文件" :disabled="!selectedConversation || selectedConversation.status !== 'active' || sending" @click="fileInput?.click()">＋</NButton>
            <span>支持图片和文件</span>
          </div>
          <div class="chat-send-hint">⌘↵ 发送</div>
          <NButton class="chat-send-button" type="primary" circle :loading="sending" :disabled="!selectedConversation || selectedConversation.status !== 'active' || (!inputText.trim() && !attachments.length)" aria-label="发送消息" @click="send">↑</NButton>
        </div>
      </div>
      <p class="chat-disclaimer">Abot 可能会出错，请检查重要信息。</p>
    </footer>

    <transition name="chat-settings">
      <aside v-if="showSettings" class="chat-settings-panel">
        <div class="chat-settings-header"><div><span class="eyebrow">CONVERSATION</span><strong>对话设置</strong></div><NButton quaternary circle aria-label="关闭设置" @click="showSettings = false">×</NButton></div>
        <NForm label-placement="top" :show-feedback="false">
          <NFormItem label="对话"><NSelect :value="conversationID" :options="activeConversations.map((item) => ({ label: conversationLabel(item), value: item.id }))" placeholder="选择一个对话" @update:value="selectConversation" /></NFormItem>
          <NSpace wrap><NButton type="primary" @click="createConversation">＋ 新建对话</NButton><NButton secondary :disabled="selectedConversation?.status !== 'active'" @click="archiveConversation">归档</NButton><NButton secondary :disabled="selectedConversation?.status !== 'archived'" @click="unarchiveConversation">恢复</NButton><NButton tertiary type="error" :disabled="selectedConversation?.status !== 'archived'" @click="deleteConversation">删除</NButton></NSpace>
          <div class="chat-settings-divider" />
          <NFormItem label="配置文件"><NSelect v-model:value="profileID" :options="profileOptions" @update:value="bindProfile" /><small>系统默认 → 机器人 → 当前对话。</small></NFormItem>
          <NFormItem label="供应商"><NSelect v-model:value="providerID" :options="providerOptions" placeholder="选择供应商" /></NFormItem>
          <NFormItem label="模型"><NSelect v-model:value="modelSelectValue" :options="modelOptions" placeholder="选择或手动输入模型" /><NInput v-if="modelSelectValue === '__manual__'" v-model:value="manualModelID" class="manual-model-input" placeholder="目录外模型 ID" /></NFormItem>
          <NFormItem label="用户 ID"><NInput v-model:value="userID" /></NFormItem>
        </NForm>
        <NAlert v-if="statusText" :type="statusTagType" :show-icon="false">{{ statusText }}</NAlert>
      </aside>
    </transition>
  </div>
</template>
