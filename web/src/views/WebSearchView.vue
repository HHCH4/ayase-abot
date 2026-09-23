<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NCheckbox, NEmpty, NForm, NFormItem, NInput, NInputNumber, NModal, NProgress, NSelect, NSpace, NTag, useMessage } from 'naive-ui'
import { deleteWebSearchService, readWebSearchServices, readWebSearchUsage, saveWebSearchService, testWebSearchService } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import type { WebSearchService, WebSearchServiceInput, WebSearchTestResult, WebSearchUsageRecord, WebSearchUsageSummary } from '@/types'

const message = useMessage()
const services = ref<WebSearchService[]>([])
const usage = ref<WebSearchUsageSummary | null>(null)
const loading = ref(false)
const saving = ref(false)
const editorVisible = ref(false)
const editingID = ref('')
const clearAPIKey = ref(false)
const testResultVisible = ref(false)
const testingIDs = reactive<Record<string, boolean>>({})
const lastTest = ref<WebSearchTestResult | null>(null)

const providerOptions = [
  { label: 'Tavily', value: 'tavily' },
  { label: 'LangSearch', value: 'langsearch' },
]

type ServiceForm = {
  name: string
  provider: 'tavily' | 'langsearch'
  account_group: string
  api_key: string
  enabled: boolean
  priority: number
}

const form = reactive<ServiceForm>(emptyForm())
const sortedServices = computed(() => [...services.value].sort((a, b) => a.priority - b.priority || a.name.localeCompare(b.name)))
const dailyProgress = computed(() => {
  const limit = usage.value?.daily_call_limit || 0
  if (!limit) return 0
  return Math.max(0, Math.min(100, Math.round((usage.value?.daily_calls || 0) * 100 / limit)))
})
const uniqueAccountCount = computed(() => new Set(services.value.map((item) => `${item.provider}:${item.account_group || item.id}`)).size)

function emptyForm(): ServiceForm {
  return { name: '', provider: 'tavily', account_group: '', api_key: '', enabled: true, priority: 100 }
}

async function refresh() {
  loading.value = true
  try {
    const [serviceItems, usageSummary] = await Promise.all([readWebSearchServices(), readWebSearchUsage()])
    services.value = serviceItems
    usage.value = usageSummary
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取网页搜索配置失败')
  } finally {
    loading.value = false
  }
}

function openEditor(service?: WebSearchService) {
  editingID.value = service?.id || ''
  clearAPIKey.value = false
  Object.assign(form, service ? {
    name: service.name,
    provider: service.provider === 'langsearch' ? 'langsearch' : 'tavily',
    account_group: service.account_group || '',
    api_key: '',
    enabled: service.enabled,
    priority: service.priority,
  } : emptyForm())
  editorVisible.value = true
}

async function save() {
  const name = form.name.trim()
  if (!name) {
    message.warning('请填写服务名称')
    return
  }
  if (!editingID.value && !form.api_key.trim()) {
    message.warning('新服务必须填写 API Key')
    return
  }
  if (clearAPIKey.value && form.enabled) {
    message.warning('清除 API Key 前请先停用该服务')
    return
  }
  saving.value = true
  try {
    const payload: WebSearchServiceInput = {
      id: editingID.value || undefined,
      name,
      provider: form.provider,
      account_group: form.account_group.trim(),
      enabled: form.enabled,
      priority: form.priority || 0,
    }
    if (form.api_key.trim()) payload.api_key = form.api_key.trim()
    else if (clearAPIKey.value) payload.api_key = ''
    await saveWebSearchService(payload)
    editorVisible.value = false
    message.success('网页搜索服务已保存')
    await refresh()
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存网页搜索服务失败')
  } finally {
    saving.value = false
  }
}

async function remove(service: WebSearchService) {
  if (!window.confirm(`删除“${service.name}”？历史用量会保留；配置档正在引用时需要先移除引用。`)) return
  try {
    await deleteWebSearchService(service.id)
    message.success('网页搜索服务已删除')
    await refresh()
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除网页搜索服务失败')
  }
}

async function test(service: WebSearchService) {
  testingIDs[service.id] = true
  try {
    lastTest.value = await testWebSearchService(service.id)
    testResultVisible.value = true
    const units = usageLabel(service.provider, lastTest.value.usage)
    message.success(`连接成功，返回 ${lastTest.value.results.length} 条结果；本次用量：${units}`)
    await refresh()
  } catch (error) {
    message.error(error instanceof Error ? error.message : '搜索服务测试失败')
    await refresh()
  } finally {
    delete testingIDs[service.id]
  }
}

function serviceUsage(id: string) {
  return usage.value?.by_service.find((item) => item.service_id === id)
}

function usageLabel(provider: string, amount?: { credits: number; input_tokens: number; output_tokens: number; known: boolean }) {
  if (!amount?.known) return '服务商未返回可计量用量'
  if (provider === 'tavily') return `${amount.credits} credits`
  return `${amount.input_tokens} 输入 / ${amount.output_tokens} 输出 tokens`
}

function serviceUnitUsage(service: WebSearchService) {
  const amount = serviceUsage(service.id)
  if (!amount) return '暂无记录'
  if (service.provider === 'tavily') return `${amount.credits} credits`
  return `${amount.input_tokens + amount.output_tokens} tokens`
}

function recordUsageLabel(record: WebSearchUsageRecord) {
  return usageLabel(record.provider, record.usage)
}

function accountLabel(service: WebSearchService) {
  return service.account_group || '独立额度组'
}

function formatTime(value?: string) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function statusType(status: string): 'success' | 'warning' | 'error' | 'default' {
  if (status === 'success') return 'success'
  if (status === 'pending') return 'warning'
  if (status === 'failed') return 'error'
  return 'default'
}

onMounted(() => void refresh())
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">WEB SEARCH</p>
        <h2>网页搜索</h2>
        <p>为内置 Agent 配置 Tavily、LangSearch 搜索服务。密钥只保存在服务端；按服务优先级回退，并记录每次请求与服务商返回的用量。</p>
      </div>
      <NSpace>
        <NButton secondary :loading="loading" @click="refresh"><AppIcon name="refresh" :size="14" />刷新用量</NButton>
        <NButton type="primary" @click="openEditor()"><AppIcon name="plus" :size="14" />添加服务</NButton>
      </NSpace>
    </div>

    <NAlert v-if="usage?.alert" type="warning" :show-icon="true" class="web-search-alert">
      已达到每日搜索上限的 {{ usage.alert_percent }}%（{{ usage.daily_calls }} / {{ usage.daily_call_limit }} 次）。Abot 会在硬上限处停止向搜索服务发请求。
    </NAlert>

    <div v-if="usage" class="stats-grid web-search-stats">
      <NCard class="stat-card" :bordered="false"><span>今日请求</span><strong>{{ usage.daily_calls }} / {{ usage.daily_call_limit }}</strong><span>按 UTC 日期统计，每次服务尝试计一次</span></NCard>
      <NCard class="stat-card" :bordered="false"><span>本月已配置服务</span><strong>{{ services.length }}</strong><span>{{ uniqueAccountCount }} 个服务商额度组</span></NCard>
      <NCard class="stat-card" :bordered="false"><span>本月成功 / 失败</span><strong>{{ usage.by_service.reduce((sum, item) => sum + item.successes, 0) }} / {{ usage.by_service.reduce((sum, item) => sum + item.failures, 0) }}</strong><span>包括优先级回退尝试</span></NCard>
      <NCard class="stat-card web-search-progress-card" :bordered="false"><span>每日预算使用</span><NProgress type="line" :percentage="dailyProgress" :status="usage.alert ? 'warning' : 'success'" :show-indicator="true" /></NCard>
    </div>

    <NCard class="web-search-card" :bordered="false">
      <div class="section-heading-row">
        <div><h3>搜索服务</h3><p>相同服务商且额度组相同的 Key 会作为同一账号处理；收到限额错误后暂时跳过该组的其他 Key，再尝试下一组。</p></div>
        <NTag size="small" :bordered="false">{{ services.length }} 个实例</NTag>
      </div>
      <NEmpty v-if="!services.length" description="还没有搜索服务。先添加 Tavily 或 LangSearch API Key，再到配置文件中启用网页搜索。" />
      <div v-else class="web-search-service-list">
        <article v-for="service in sortedServices" :key="service.id" class="web-search-service-row">
          <div class="web-search-service-main">
            <div class="web-search-service-title">
              <strong>{{ service.name }}</strong>
              <NTag size="small" :bordered="false" :type="service.provider === 'tavily' ? 'info' : 'success'">{{ service.provider }}</NTag>
              <NTag size="small" :bordered="false" :type="service.enabled ? 'success' : 'default'">{{ service.enabled ? '已启用' : '已停用' }}</NTag>
              <NTag size="small" :bordered="false" :type="service.api_key_configured ? 'default' : 'warning'">{{ service.api_key_configured ? 'Key 已保存' : '缺少 Key' }}</NTag>
            </div>
            <code>{{ service.id }}</code>
            <span>额度组：{{ accountLabel(service) }} · 服务优先级 {{ service.priority }}</span>
            <span>本月 {{ serviceUsage(service.id)?.calls || 0 }} 次请求 · {{ serviceUnitUsage(service) }}</span>
          </div>
          <NSpace class="web-search-service-actions" :size="6">
            <NButton size="small" secondary :disabled="!service.enabled || !service.api_key_configured" :loading="testingIDs[service.id] === true" @click="test(service)">测试</NButton>
            <NButton size="small" secondary @click="openEditor(service)">编辑</NButton>
            <NButton size="small" tertiary type="error" @click="remove(service)"><AppIcon name="trash" :size="14" />删除</NButton>
          </NSpace>
        </article>
      </div>
    </NCard>

    <NCard class="web-search-card" :bordered="false">
      <div class="section-heading-row"><div><h3>本月账号额度组汇总</h3><p>相同服务商、相同额度组的多个 Key 合并统计。这里只计 Abot 发起的请求；同一账号经 MCP 或其他客户端的调用也会占上游额度，但不在 Abot 账本内。</p></div><NTag size="small" :bordered="false">{{ usage?.period_start ? new Date(usage.period_start).toLocaleDateString() : '' }} 起</NTag></div>
      <div v-if="usage?.by_account.length" class="data-table-wrap web-search-table-wrap">
        <table class="data-table web-search-usage-table">
          <thead><tr><th>服务商 / 额度组</th><th>请求</th><th>成功 / 失败</th><th>Tavily credits</th><th>LangSearch tokens</th><th>用量未知</th></tr></thead>
          <tbody>
            <tr v-for="item in usage.by_account" :key="`${item.provider}:${item.account_group}`">
              <td><strong>{{ item.provider }}</strong><code>{{ item.account_group }}</code></td>
              <td>{{ item.calls }}</td><td>{{ item.successes }} / {{ item.failures }}</td><td>{{ item.credits }}</td><td>{{ item.input_tokens }} / {{ item.output_tokens }}</td><td>{{ item.unknown_usage }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <NEmpty v-else description="尚无已配置账号额度组" />
    </NCard>

    <NCard class="web-search-card" :bordered="false">
      <div class="section-heading-row"><div><h3>按 API Key 查看本月用量</h3><p>Tavily 记录服务返回的 credits；LangSearch 记录返回的输入、输出 tokens。服务商不返回用量时会标记为未知，不作估算。</p></div></div>
      <div v-if="usage?.by_service.length" class="data-table-wrap web-search-table-wrap">
        <table class="data-table web-search-usage-table">
          <thead><tr><th>服务 / 额度组</th><th>请求</th><th>成功 / 失败</th><th>Tavily credits</th><th>LangSearch tokens</th><th>用量未知</th></tr></thead>
          <tbody>
            <tr v-for="item in usage.by_service" :key="item.service_id">
              <td><strong>{{ item.service_name }}</strong><code>{{ item.provider }} · {{ item.account_group || '独立额度组' }}</code></td>
              <td>{{ item.calls }}</td><td>{{ item.successes }} / {{ item.failures }}</td><td>{{ item.credits }}</td><td>{{ item.input_tokens }} / {{ item.output_tokens }}</td><td>{{ item.unknown_usage }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <NEmpty v-else description="本月尚无网页搜索请求" />
    </NCard>

    <NCard class="web-search-card" :bordered="false">
      <div class="section-heading-row"><div><h3>最近请求</h3><p>展示近 90 天记录；删除会话会清空查询文本、任务 ID 和会话关联，但匿名用量仍计入预算。</p></div></div>
      <div v-if="usage?.recent.length" class="data-table-wrap web-search-table-wrap">
        <table class="data-table web-search-table">
          <thead><tr><th>时间</th><th>查询</th><th>服务</th><th>状态</th><th>结果数</th><th>服务商用量</th></tr></thead>
          <tbody>
            <tr v-for="record in usage.recent" :key="record.id">
              <td>{{ formatTime(record.created_at) }}</td>
              <td class="web-search-query">{{ record.query }}</td>
              <td><strong>{{ record.service_name }}</strong><code>{{ record.provider }} · {{ record.account_group || '独立额度组' }}</code></td>
              <td><NTag size="small" :bordered="false" :type="statusType(record.status)">{{ record.status === 'success' ? '成功' : record.status === 'pending' ? '处理中' : '失败' }}</NTag><code v-if="record.http_status">HTTP {{ record.http_status }}</code></td>
              <td>{{ record.result_count }}</td><td>{{ recordUsageLabel(record) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <NEmpty v-else description="暂无网页搜索记录" />
    </NCard>

    <NModal v-model:show="editorVisible" preset="card" :title="editingID ? '编辑搜索服务' : '添加搜索服务'" class="web-search-editor-modal" :mask-closable="false">
      <NForm label-placement="top">
        <NFormItem label="服务名称" required><NInput v-model:value="form.name" placeholder="例如：Tavily 主账号" /></NFormItem>
        <NFormItem label="服务商" required><NSelect v-model:value="form.provider" :options="providerOptions" /></NFormItem>
        <NFormItem label="API Key" required>
          <NInput v-model:value="form.api_key" type="password" show-password-on="click" :placeholder="editingID ? '留空表示保留当前 Key' : '请输入 API Key'" />
          <template #feedback>{{ editingID ? 'Key 不会回传到浏览器；留空保留已保存的 Key。' : 'Key 只保存于 Abot 服务端，不会出现在配置档或 API 响应中。' }}</template>
        </NFormItem>
        <NCheckbox v-if="editingID" v-model:checked="clearAPIKey">清除已保存的 API Key（需同时停用服务）</NCheckbox>
        <NFormItem label="账号额度组">
          <NInput v-model:value="form.account_group" placeholder="例如 tavily-account-main" />
          <template #feedback>同一个服务商的相同登录账号请填写同一组名；不同账号使用不同组名。留空时视为独立额度组。</template>
        </NFormItem>
        <div class="form-grid-2">
          <NFormItem label="调用优先级"><NInputNumber v-model:value="form.priority" :min="0" :max="10000" :step="1" :show-button="false" /></NFormItem>
          <NFormItem label="状态"><NCheckbox v-model:checked="form.enabled">启用</NCheckbox></NFormItem>
        </div>
      </NForm>
      <NAlert type="info" :show-icon="false">发生限额或临时服务错误时，Abot 会按优先级尝试其他账号组；同一额度组中的其他 Key 会暂时跳过。</NAlert>
      <template #footer><div class="modal-footer"><NButton @click="editorVisible = false">取消</NButton><NButton type="primary" :loading="saving" @click="save">保存服务</NButton></div></template>
    </NModal>

    <NModal v-model:show="testResultVisible" preset="card" title="网页搜索测试结果" class="web-search-test-modal">
      <div v-if="lastTest" class="web-search-test-content">
        <NAlert type="success" :show-icon="false">{{ lastTest.provider }} · 用量：{{ usageLabel(lastTest.provider, lastTest.usage) }}</NAlert>
        <article v-for="result in lastTest.results" :key="result.url" class="web-search-result">
          <strong>{{ result.title || result.url }}</strong><a :href="result.url" target="_blank" rel="noreferrer">{{ result.url }}</a><p>{{ result.snippet }}</p>
        </article>
        <p v-if="!lastTest.results.length" class="muted">请求成功，但服务商没有返回搜索结果。</p>
      </div>
    </NModal>
  </div>
</template>
