<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { NButton, NCard, NEmpty, NInput, NModal, NSelect, NSpace, NTabPane, NTag, NTabs, useMessage } from 'naive-ui'
import { archiveStoredConversation, deleteStoredConversation, openDataLogStream, readConversationMessages, readDataConversations, readDataTraces, readDashboardStats, readInvocationTrace, unarchiveStoredConversation } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import type { Conversation, ConversationMessage, DashboardStats, DataLogEntry, InvocationTrace } from '@/types'

const message = useMessage()
const activeTab = ref('overview')
const range = ref('1d')
const loading = ref(false)
const stats = ref<DashboardStats | null>(null)
const conversations = ref<Conversation[]>([])
const selectedConversation = ref<Conversation | null>(null)
const conversationMessages = ref<ConversationMessage[]>([])
const conversationDetailVisible = ref(false)
const conversationDetailLoading = ref(false)
const traces = ref<Record<string, unknown>[]>([])
const logs = ref<DataLogEntry[]>([])
const conversationQuery = ref('')
const conversationStatus = ref('')
const traceQuery = ref('')
const selectedTrace = ref<InvocationTrace | null>(null)
const traceLoading = ref(false)
const logConnected = ref(false)
const autoScrollLogs = ref(true)
const logTerminal = ref<HTMLElement | null>(null)
let closeLogStream: (() => void) | undefined
let logStreamGeneration = 0
const logLevels = ['DEBUG', 'INFO', 'WARN', 'ERROR', 'CRITICAL']
const selectedLogLevels = ref([...logLevels])
const displayTimeZone = Intl.DateTimeFormat().resolvedOptions().timeZone || '本地时区'
const localDateTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  year: 'numeric', month: '2-digit', day: '2-digit',
  hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
})

const visibleLogs = computed(() => logs.value.filter((item) => selectedLogLevels.value.includes(item.level.toUpperCase())))

const trendMax = computed(() => Math.max(1, ...(stats.value?.message_trend || []).map((item) => item.messages)))
const statusOptions = [
  { label: '全部状态', value: '' },
  { label: '进行中', value: 'active' },
  { label: '已归档', value: 'archived' },
]
// 仪表盘分成独立请求，单个数据源异常时仍保留其他可用区域。
async function loadStats() {
  try {
    stats.value = await readDashboardStats(range.value)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取统计数据失败')
  }
}

async function loadConversations() {
  try {
    const result = await readDataConversations({ q: conversationQuery.value, status: conversationStatus.value, page: '1', page_size: '50' })
    conversations.value = result.conversations || []
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取对话数据失败')
  }
}

// 管理台点击会话时复用正式消息接口，确保机器人会话和 WebUI 会话展示同一份历史。
async function openConversation(item: Conversation) {
  selectedConversation.value = item
  conversationMessages.value = []
  conversationDetailVisible.value = true
  conversationDetailLoading.value = true
  try {
    conversationMessages.value = await readConversationMessages(item.user_id, item.id)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取会话消息失败')
  } finally {
    conversationDetailLoading.value = false
  }
}

// 归档只改变会话状态，保留对话、追踪和附件，便于后续恢复查看。
async function archiveDataConversation(item: Conversation) {
  if (item.status !== 'active') return
  try {
    const updated = await archiveStoredConversation(item.user_id, item.id)
    if (selectedConversation.value?.id === item.id) selectedConversation.value = updated
    await load()
    message.success('对话已归档')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '归档对话失败')
  }
}

// 恢复只允许归档会话执行，恢复后消息和关联追踪仍保持不变。
async function unarchiveDataConversation(item: Conversation) {
  if (item.status !== 'archived') return
  try {
    const updated = await unarchiveStoredConversation(item.user_id, item.id)
    if (selectedConversation.value?.id === item.id) selectedConversation.value = updated
    await load()
    message.success('对话已恢复')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '恢复对话失败')
  }
}

// 删除会物理清理消息、附件、Runtime 追踪、工作区记录和长期记忆，执行前明确提示不可恢复。
async function deleteDataConversation(item: Conversation) {
  if (item.status !== 'archived') return
  if (!window.confirm(`彻底删除“${item.title || item.id}”及其全部关联数据？此操作不可恢复。`)) return
  try {
    await deleteStoredConversation(item.user_id, item.id)
    if (selectedConversation.value?.id === item.id) {
      selectedConversation.value = null
      conversationMessages.value = []
      conversationDetailVisible.value = false
    }
    await load()
    message.success('对话及关联数据已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除对话失败')
  }
}

async function loadTraces() {
  try {
    const result = await readDataTraces({ q: traceQuery.value, page: '1', page_size: '50' })
    traces.value = result.traces || []
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取执行追踪失败')
  }
}

async function load() {
  loading.value = true
  await Promise.all([loadStats(), loadConversations(), loadTraces()])
  loading.value = false
}

// 日志只在用户打开日志页时建立流；离开页面立即关闭连接并释放前端快照。
function stopLogStream() {
  logStreamGeneration += 1
  closeLogStream?.()
  closeLogStream = undefined
  logConnected.value = false
  logs.value = []
}

function scrollLogsToBottom() {
  if (!autoScrollLogs.value || !logTerminal.value) return
  logTerminal.value.scrollTop = logTerminal.value.scrollHeight
}

function appendLogEntries(entries: DataLogEntry[]) {
  logs.value = [...entries].reverse().slice(-500)
  void nextTick(scrollLogsToBottom)
}

function appendLogEntry(entry: DataLogEntry) {
  logs.value = [...logs.value, entry].slice(-500)
  void nextTick(scrollLogsToBottom)
}

function toggleLogLevel(level: string) {
  if (selectedLogLevels.value.includes(level)) {
    selectedLogLevels.value = selectedLogLevels.value.filter((item) => item !== level)
  } else {
    selectedLogLevels.value = [...selectedLogLevels.value, level]
  }
  void nextTick(scrollLogsToBottom)
}

function formatLocalDateTime(value?: string, milliseconds = false) {
  if (!value) return '未知时间'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  const text = localDateTimeFormatter.format(date).replaceAll('/', '-')
  if (!milliseconds) return text
  return `${text}.${String(date.getMilliseconds()).padStart(3, '0')}`
}

function formatLogTimestamp(value?: string) {
  return formatLocalDateTime(value, true)
}

function formatLogLine(item: DataLogEntry) {
  const attributes = item.attributes && Object.keys(item.attributes).length ? ` ${JSON.stringify(item.attributes)}` : ''
  return `[${formatLogTimestamp(item.time)}] [${item.level}] ${item.message}${attributes}`
}

function startLogStream() {
  if (activeTab.value !== 'logs' || closeLogStream) return
  const generation = ++logStreamGeneration
  closeLogStream = openDataLogStream(
    '',
    (entries) => {
      if (generation !== logStreamGeneration) return
      appendLogEntries(entries)
      logConnected.value = true
    },
    (entry) => {
      if (generation !== logStreamGeneration) return
      appendLogEntry(entry)
      logConnected.value = true
    },
    () => {
      if (generation === logStreamGeneration) logConnected.value = false
    },
  )
}

async function openTrace(item: Record<string, unknown>) {
  const id = String(item.invocation_id || item.id || '')
  if (!id) return
  traceLoading.value = true
  try {
    selectedTrace.value = await readInvocationTrace(id)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取追踪详情失败')
  } finally {
    traceLoading.value = false
  }
}

function formatTime(value?: string) {
  return formatLocalDateTime(value)
}

function conversationContext(item: Conversation) {
  const parts = (item.source || '').split(':')
  const sourceType = parts[1] || ''
  let chatType = item.chat_type || ''
  if (!chatType) {
    if (sourceType === 'FriendMessage') chatType = 'private'
    else if (sourceType === 'GroupMessage') chatType = 'group'
    else if (sourceType) chatType = 'other'
  }
  return {
    platform: item.platform || parts[0] || 'WebUI',
    chatType,
    chatID: item.chat_id || parts.slice(2).join(':') || '',
  }
}

function conversationChatTypeLabel(item: Conversation) {
  const value = conversationContext(item).chatType
  if (value === 'private') return '私聊'
  if (value === 'group') return '群聊'
  if (value === 'other') return '其他会话'
  return 'WebUI 对话'
}

function conversationChatTypeTag(item: Conversation) {
  const value = conversationContext(item).chatType
  if (value === 'group') return 'info'
  if (value === 'private') return 'success'
  return 'default'
}

function formatNumber(value?: number) {
  return new Intl.NumberFormat('zh-CN').format(value || 0)
}

function formatRate(value?: number) {
  return `${Math.round((value || 0) * 100)}%`
}

function traceStatus(value: unknown) {
  const status = String(value || '')
  if (status === 'completed') return '已完成'
  if (status === 'failed') return '失败'
  if (status === 'running') return '运行中'
  return status || '未知'
}

watch(range, loadStats)
watch([conversationQuery, conversationStatus], loadConversations)
watch(traceQuery, loadTraces)
watch(activeTab, (tab, previous) => {
  if (previous === 'logs') stopLogStream()
  if (tab === 'logs') startLogStream()
})

onMounted(async () => {
  await load()
})

onUnmounted(() => {
  stopLogStream()
})

watch(autoScrollLogs, () => {
  void nextTick(scrollLogsToBottom)
})
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">DATA &amp; OBSERVABILITY</p>
        <h2>数据与日志</h2>
        <p>查看内置 Agent 的对话、模型调用、执行追踪和原始日志。统计来自本地运行时持久化数据，不依赖外部分析平台。</p>
      </div>
      <NSpace>
        <NSelect v-model:value="range" :options="[{ label: '近 1 天', value: '1d' }, { label: '近 3 天', value: '3d' }, { label: '近 7 天', value: '7d' }]" style="width: 118px" />
        <NButton secondary :loading="loading" @click="load">刷新</NButton>
      </NSpace>
    </div>

    <NTabs v-model:value="activeTab" type="line" animated>
      <NTabPane name="overview" tab="总览">
        <div v-if="stats" class="stats-grid">
          <NCard v-for="item in [
            { label: '任务调用', value: stats.overview.invocation_count },
            { label: '消息数', value: stats.overview.message_count },
            { label: '模型调用', value: stats.overview.model_calls },
            { label: '总 Token', value: stats.overview.total_tokens },
            { label: '成功率', value: stats.overview.invocation_count ? `${Math.round(stats.overview.success_count / stats.overview.invocation_count * 100)}%` : '0%' },
            { label: '平均响应', value: `${Math.round(stats.overview.avg_response_ms)} ms` },
          ]" :key="item.label" class="stat-card" :bordered="false">
            <span>{{ item.label }}</span><strong>{{ typeof item.value === 'number' ? formatNumber(item.value) : item.value }}</strong>
          </NCard>
        </div>
        <div v-if="stats" class="data-two-column">
            <NCard class="detail-card" :bordered="false">
            <div class="section-heading-row"><div><h3>消息趋势</h3><p>统计按 UTC 日期归档，明细时间按浏览器本地时区展示。</p></div><NTag size="small" :bordered="false">{{ stats.range_days }} 天</NTag></div>
            <div class="trend-list">
              <div v-for="item in stats.message_trend" :key="item.date" class="trend-row">
                <span>{{ item.date }}</span><div class="trend-track"><i :style="{ width: `${item.messages / trendMax * 100}%` }" /></div><strong>{{ item.messages }}</strong>
              </div>
            </div>
          </NCard>
          <NCard class="detail-card" :bordered="false">
            <div class="section-heading-row"><div><h3>模型使用排行</h3><p>仅展示本地追踪中已知的模型调用。</p></div><NTag size="small" :bordered="false">{{ stats.platform_instances }} 个平台</NTag></div>
            <div v-if="stats.model_ranking.length" class="ranking-list">
              <div v-for="item in stats.model_ranking" :key="item.model" class="ranking-row"><span>{{ item.model }}</span><strong>{{ item.calls }} 次</strong><small>{{ formatNumber(item.tokens) }} tokens · {{ formatRate(item.success_rate) }}</small></div>
            </div>
            <NEmpty v-else description="暂无模型调用数据" />
          </NCard>
        </div>
        <NEmpty v-else description="暂无统计数据" />
      </NTabPane>

      <NTabPane name="conversations" tab="对话">
        <div class="data-toolbar"><NInput v-model:value="conversationQuery" clearable placeholder="搜索标题、来源或对话 ID" /><NSelect v-model:value="conversationStatus" :options="statusOptions" style="width: 140px" /></div>
        <div v-if="conversations.length" class="data-table-wrap"><table class="data-table"><thead><tr><th>标题 / 来源</th><th>平台 / 会话类型</th><th>用户</th><th>状态</th><th>更新时间</th><th>操作</th></tr></thead><tbody><tr v-for="item in conversations" :key="item.id"><td><strong>{{ item.title || '未命名对话' }}</strong><code>{{ item.source_name || item.source || item.id }}</code></td><td><NSpace size="small" align="center"><NTag size="small" :bordered="false" :type="conversationChatTypeTag(item)">{{ conversationChatTypeLabel(item) }}</NTag><span>{{ conversationContext(item).platform }}</span></NSpace><code v-if="conversationContext(item).chatID">会话：{{ conversationContext(item).chatID }}</code></td><td>{{ item.user_id }}</td><td><NTag size="small" :bordered="false">{{ item.status === 'active' ? '进行中' : '已归档' }}</NTag></td><td>{{ formatTime(item.updated_at || item.created_at) }}</td><td><NSpace size="small"><NButton size="small" secondary @click="openConversation(item)">查看对话</NButton><NButton v-if="item.status === 'active'" size="small" secondary @click="archiveDataConversation(item)">归档</NButton><NButton v-else size="small" secondary @click="unarchiveDataConversation(item)">恢复</NButton><NButton v-if="item.status === 'archived'" size="small" tertiary type="error" @click="deleteDataConversation(item)">删除</NButton></NSpace></td></tr></tbody></table></div>
        <NEmpty v-else description="暂无匹配对话" />
      </NTabPane>

      <NTabPane name="traces" tab="执行追踪">
        <div class="data-toolbar"><NInput v-model:value="traceQuery" clearable placeholder="搜索 Invocation、对话或消息" /><span class="muted">点击记录查看完整 Trace</span></div>
        <div class="data-two-column trace-layout">
          <NCard class="detail-card" :bordered="false"><div v-if="traces.length" class="trace-list"><button v-for="item in traces" :key="String(item.id)" type="button" class="trace-list-item" :class="{ active: selectedTrace?.invocation_id === item.id }" @click="openTrace(item)"><span><strong>{{ traceStatus(item.status) }}</strong><code>{{ item.id }}</code></span><small>{{ formatTime(String(item.created_at || '')) }}</small></button></div><NEmpty v-else description="暂无执行追踪" /></NCard>
          <NCard class="detail-card" :bordered="false"><div v-if="selectedTrace" class="trace-detail"><div class="section-heading-row"><div><h3>Trace 详情</h3><p>{{ selectedTrace.invocation_id }}</p></div><NTag size="small" :bordered="false">{{ traceStatus(selectedTrace.status) }}</NTag></div><pre>{{ JSON.stringify(selectedTrace, null, 2) }}</pre></div><NEmpty v-else :description="traceLoading ? '正在读取 Trace…' : '选择左侧记录查看详情'" /></NCard>
        </div>
      </NTabPane>

      <NTabPane name="logs" tab="实时日志">
        <div class="log-terminal-shell">
          <div class="log-terminal-toolbar">
            <div class="log-level-filters">
              <button v-for="level in logLevels" :key="level" type="button" class="log-filter-chip" :class="{ active: selectedLogLevels.includes(level) }" @click="toggleLogLevel(level)">
                <AppIcon v-if="selectedLogLevels.includes(level)" name="check" :size="13" />{{ level }}
              </button>
            </div>
            <div class="log-terminal-actions">
              <button type="button" class="log-toggle" :class="{ active: autoScrollLogs }" @click="autoScrollLogs = !autoScrollLogs">自动滚动</button>
              <span class="log-stream-state" :class="{ connected: logConnected }">{{ logConnected ? 'LIVE' : 'CONNECTING' }}</span>
            </div>
          </div>
          <div ref="logTerminal" class="log-terminal">
            <div v-if="visibleLogs.length" class="log-terminal-lines">
              <div v-for="(item, index) in visibleLogs" :key="`${item.time}-${index}`" class="log-terminal-line" :class="`level-${item.level.toUpperCase()}`">{{ formatLogLine(item) }}</div>
            </div>
            <div v-else class="log-terminal-empty">{{ logConnected ? '暂无匹配日志' : '正在连接实时日志流…' }}</div>
          </div>
        </div>
      </NTabPane>
    </NTabs>

    <NModal v-model:show="conversationDetailVisible" preset="card" :mask-closable="false" style="width: min(860px, calc(100vw - 32px))" :title="selectedConversation?.title || '对话详情'">
      <div class="conversation-detail-meta">
        <code>{{ selectedConversation?.source_name || selectedConversation?.source || selectedConversation?.id }}</code>
        <NTag v-if="selectedConversation" size="small" :bordered="false" :type="conversationChatTypeTag(selectedConversation)">{{ conversationChatTypeLabel(selectedConversation) }}</NTag>
        <span v-if="selectedConversation">平台：{{ conversationContext(selectedConversation).platform }}</span>
        <span v-if="selectedConversation && conversationContext(selectedConversation).chatID">会话：{{ conversationContext(selectedConversation).chatID }}</span>
        <span>{{ selectedConversation?.user_id }}</span>
        <span class="muted">时间：{{ displayTimeZone }}</span>
      </div>
      <div v-if="conversationDetailLoading" class="conversation-detail-loading">正在读取会话消息…</div>
      <div v-else-if="conversationMessages.length" class="message-panel conversation-detail-panel">
        <div v-for="(item, index) in conversationMessages" :key="`${item.timestamp || 'message'}-${index}`" class="message-line" :class="{ 'from-user': item.role === 'user' }">
          <div class="message-avatar">{{ item.role === 'user' ? '你' : 'AI' }}</div>
          <div class="message-bubble">
            <pre>{{ item.text || (item.attachment_count ? `附件 ${item.attachment_count} 个` : '（无文本内容）') }}</pre>
            <div v-if="item.attachment_count" class="message-attachment-count">附件 {{ item.attachment_count }} 个</div>
            <small>{{ formatTime(item.timestamp) }}</small>
          </div>
        </div>
      </div>
      <NEmpty v-else description="这个会话暂无可展示消息" />
      <NSpace v-if="selectedConversation" justify="end" style="margin-top: 18px">
        <NButton v-if="selectedConversation.status === 'active'" secondary @click="archiveDataConversation(selectedConversation)">归档</NButton>
        <NButton v-else secondary @click="unarchiveDataConversation(selectedConversation)">恢复</NButton>
        <NButton v-if="selectedConversation.status === 'archived'" tertiary type="error" @click="deleteDataConversation(selectedConversation)">删除对话及关联数据</NButton>
      </NSpace>
    </NModal>
  </div>
</template>
