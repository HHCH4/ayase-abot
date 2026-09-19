<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { NButton, NCard, NEmpty, NInput, NSelect, NSpace, NTabPane, NTag, NTabs, useMessage } from 'naive-ui'
import { readDataConversations, readDataLogs, readDataTraces, readDashboardStats, readInvocationTrace } from '@/api'
import type { Conversation, DashboardStats, DataLogEntry, InvocationTrace } from '@/types'

const message = useMessage()
const activeTab = ref('overview')
const range = ref('1d')
const loading = ref(false)
const stats = ref<DashboardStats | null>(null)
const conversations = ref<Conversation[]>([])
const traces = ref<Record<string, unknown>[]>([])
const logs = ref<DataLogEntry[]>([])
const conversationQuery = ref('')
const conversationStatus = ref('')
const traceQuery = ref('')
const logLevel = ref('')
const selectedTrace = ref<InvocationTrace | null>(null)
const traceLoading = ref(false)
let logTimer: number | undefined

const trendMax = computed(() => Math.max(1, ...(stats.value?.message_trend || []).map((item) => item.messages)))
const statusOptions = [
  { label: '全部状态', value: '' },
  { label: '进行中', value: 'active' },
  { label: '已归档', value: 'archived' },
]
const logLevelOptions = [
  { label: '全部日志', value: '' },
  { label: 'INFO', value: 'INFO' },
  { label: 'WARN', value: 'WARN' },
  { label: 'ERROR', value: 'ERROR' },
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

async function loadTraces() {
  try {
    const result = await readDataTraces({ q: traceQuery.value, page: '1', page_size: '50' })
    traces.value = result.traces || []
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取执行追踪失败')
  }
}

async function loadLogs() {
  try {
    logs.value = await readDataLogs(logLevel.value)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取日志失败')
  }
}

async function load() {
  loading.value = true
  await Promise.all([loadStats(), loadConversations(), loadTraces(), loadLogs()])
  loading.value = false
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
  return value ? new Date(value).toLocaleString() : '未知时间'
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
watch(logLevel, loadLogs)

onMounted(async () => {
  await load()
  // 日志页需要实时感知新事件，其他统计仍由手动刷新或筛选变化触发。
  logTimer = window.setInterval(loadLogs, 5000)
})

onUnmounted(() => {
  if (logTimer !== undefined) window.clearInterval(logTimer)
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
            <div class="section-heading-row"><div><h3>消息趋势</h3><p>按 UTC 日期统计已创建的内置 Agent 调用。</p></div><NTag size="small" :bordered="false">{{ stats.range_days }} 天</NTag></div>
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
        <div class="data-toolbar"><NInput v-model:value="conversationQuery" clearable placeholder="搜索标题或对话 ID" /><NSelect v-model:value="conversationStatus" :options="statusOptions" style="width: 140px" /></div>
        <div v-if="conversations.length" class="data-table-wrap"><table class="data-table"><thead><tr><th>标题 / 来源</th><th>用户</th><th>状态</th><th>更新时间</th></tr></thead><tbody><tr v-for="item in conversations" :key="item.id"><td><strong>{{ item.title || '未命名对话' }}</strong><code>{{ item.source_name || item.source || item.id }}</code></td><td>{{ item.user_id }}</td><td><NTag size="small" :bordered="false">{{ item.status === 'active' ? '进行中' : '已归档' }}</NTag></td><td>{{ formatTime(item.updated_at || item.created_at) }}</td></tr></tbody></table></div>
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
        <div class="data-toolbar"><NSelect v-model:value="logLevel" :options="logLevelOptions" style="width: 140px" /><span class="muted">每 5 秒刷新一次 · 保留原始请求与响应字段</span></div>
        <div v-if="logs.length" class="log-list"><div v-for="(item, index) in logs" :key="`${item.time}-${index}`" class="log-row"><time>{{ formatTime(item.time) }}</time><NTag size="small" :bordered="false" :type="item.level === 'ERROR' ? 'error' : item.level === 'WARN' ? 'warning' : 'info'">{{ item.level }}</NTag><span>{{ item.message }}</span><code v-if="item.attributes">{{ JSON.stringify(item.attributes) }}</code></div></div>
        <NEmpty v-else description="暂无日志" />
      </NTabPane>
    </NTabs>
  </div>
</template>
