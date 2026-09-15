<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NForm, NFormItem, NInput, NInputNumber, NModal, NSelect, NSpace, NTag, useMessage } from 'naive-ui'
import { request } from '@/api'
import { useAppStore } from '@/stores/app'
import type { CapabilityObservationRecord, CapabilityProbeResult, Provider, ProviderModel } from '@/types'

const store = useAppStore()
const message = useMessage()
const showEditor = ref(false)
const editingID = ref('')
const saving = ref(false)
const discovering = ref(false)
// 模型详情按行独立展开，默认不展示上下文和输出上限，避免目录列表过于拥挤。
const expandedModels = reactive<Record<number, boolean>>({})
const probingModels = reactive<Record<string, boolean>>({})
const capabilityObservationHistory = reactive<Record<string, CapabilityObservationRecord[]>>({})

type ProviderForm = {
  id: string
  name: string
  base_url: string
  protocol: string
  openai_format: string
  token_count_protocol: string
  api_key: string
  models: ProviderModel[]
}

const form = reactive<ProviderForm>(emptyForm())

function emptyForm(): ProviderForm {
  return { id: '', name: '', base_url: '', protocol: 'openai-compatible', openai_format: 'auto', token_count_protocol: '', api_key: '', models: [] }
}

const protocolOptions = [
  { label: 'OpenAI 兼容', value: 'openai-compatible' },
  { label: 'Gemini', value: 'gemini' },
]
const formatOptions = [
  { label: '自动（默认 Chat）', value: 'auto' },
  { label: 'Chat Completions', value: 'chat' },
  { label: 'Responses', value: 'responses' },
]
const tokenCountProtocolOptions = [
  { label: '跟随生成协议（默认）', value: '' },
  { label: 'Anthropic Messages token count（显式）', value: 'anthropic' },
]
const statusMap: Record<string, { label: string; type: 'success' | 'warning' | 'error' | 'default' }> = {
  ready: { label: '连接正常', type: 'success' },
  error: { label: '连接失败', type: 'error' },
  configured: { label: '待测试', type: 'warning' },
}
const sortedProviders = computed(() => [...store.providers].sort((a, b) => a.name.localeCompare(b.name)))

function statusOf(provider: Provider) {
  return statusMap[provider.status] || { label: provider.status || '未知', type: 'default' as const }
}

function replaceForm(value: ProviderForm) {
  Object.assign(form, value)
}

function resetExpandedModels() {
  Object.keys(expandedModels).forEach((key) => delete expandedModels[Number(key)])
}

function resetCapabilityObservationHistory() {
  Object.keys(capabilityObservationHistory).forEach((key) => delete capabilityObservationHistory[key])
}

function openEditor(provider?: Provider) {
  editingID.value = provider?.id || ''
  replaceForm(provider ? {
    id: provider.id,
    name: provider.name,
    base_url: provider.base_url || '',
    protocol: provider.protocol === 'openai-completions' ? 'openai-compatible' : provider.protocol,
    openai_format: provider.openai_format || 'auto',
    token_count_protocol: provider.token_count_protocol || '',
    api_key: '',
    models: (provider.models || []).map((item) => ({ ...item })),
  } : emptyForm())
  resetExpandedModels()
  resetCapabilityObservationHistory()
  showEditor.value = true
}

function addModel() {
  form.models.push({ id: '', display_name: '', enabled: true, source: 'manual' })
}

function removeModel(index: number) {
  form.models.splice(index, 1)
  resetExpandedModels()
}

function toggleModelDetails(index: number) {
  expandedModels[index] = !expandedModels[index]
  if (expandedModels[index] && editingID.value && form.models[index]?.id) {
    void loadCapabilityObservations(form.models[index].id)
  }
}

function probeKey(modelID: string) {
  return `${editingID.value}:${modelID}`
}

function supportLabel(value?: { state?: string }) {
  const labels: Record<string, string> = { supported: '支持', unsupported: '不支持', degraded: '部分', unknown: '未知' }
  return labels[value?.state || 'unknown'] || value?.state || '未知'
}

function supportType(value?: { state?: string }): 'success' | 'warning' | 'error' | 'default' {
  if (value?.state === 'supported') return 'success'
  if (value?.state === 'unsupported') return 'error'
  if (value?.state === 'degraded') return 'warning'
  return 'default'
}

function observationHistoryFor(modelID: string) {
  return capabilityObservationHistory[probeKey(modelID)] || []
}

async function loadCapabilityObservations(modelID: string) {
  const providerID = editingID.value.trim()
  modelID = modelID.trim()
  if (!providerID || !modelID) return
  try {
    const result = await request<{ observations: CapabilityObservationRecord[] }>(`/api/v1/providers/${encodeURIComponent(providerID)}/models/${encodeURIComponent(modelID)}/capability-observations?limit=5`)
    capabilityObservationHistory[probeKey(modelID)] = result.observations || []
  } catch {
    // Older embedders may not expose observation history; the current profile
    // remains useful and the failure should not block provider editing.
    capabilityObservationHistory[probeKey(modelID)] = []
  }
}

async function probeModel(model: ProviderModel, deep = false) {
  const providerID = editingID.value.trim()
  const modelID = model.id.trim()
  if (!providerID || !modelID) {
    message.info('请先保存供应商和模型，再执行能力探测')
    return
  }
  const key = probeKey(modelID)
  probingModels[key] = true
  try {
    const body = deep ? JSON.stringify({
      include_streaming: true,
      include_tool_calling: true,
      include_structured_json: true,
      include_token_count: true,
      include_structured_schema: true,
      include_reasoning: true,
      include_images: true,
      include_audio: true,
      include_input_files: true,
    }) : '{}'
    const result = await request<CapabilityProbeResult>(`/api/v1/providers/${encodeURIComponent(providerID)}/models/${encodeURIComponent(modelID)}/probe`, { method: 'POST', body })
    model.capabilities = result.profile
    capabilityObservationHistory[key] = (result.observations || []).map((item, index) => ({
      ...item,
      id: `pending-${key}-${index}`,
      provider_id: providerID,
      model_id: modelID,
      protocol: result.profile.protocol,
      created_at: result.completed_at,
    }))
    await store.reloadProviders()
    await loadCapabilityObservations(modelID)
    message.success(result.message || '模型能力探测完成')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '模型能力探测失败')
  } finally {
    delete probingModels[key]
  }
}

async function save() {
  saving.value = true
  try {
    const payload: Record<string, unknown> = {
      id: form.id.trim(),
      name: form.name.trim(),
      base_url: form.base_url.trim(),
      protocol: form.protocol,
      openai_format: form.openai_format,
      token_count_protocol: form.token_count_protocol,
      models: form.models.filter((item) => item.id.trim()).map((item) => ({
        id: item.id.trim(),
        display_name: item.display_name?.trim() || item.id.trim(),
        enabled: item.enabled !== false,
        source: item.source || 'manual',
        context_window: item.context_window || 0,
        max_output_tokens: item.max_output_tokens || 0,
      })),
    }
    if (form.api_key.trim()) payload.api_key = form.api_key.trim()
    await request(`/api/v1/providers${editingID.value ? `/${encodeURIComponent(editingID.value)}` : ''}`, {
      method: editingID.value ? 'PUT' : 'POST',
      body: JSON.stringify(payload),
    })
    await store.reloadProviders()
    showEditor.value = false
    message.success('供应商配置已保存')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存供应商失败')
  } finally {
    saving.value = false
  }
}

async function discoverModels() {
  discovering.value = true
  try {
    const payload: Record<string, unknown> = {
      id: form.id.trim(), name: form.name.trim(), base_url: form.base_url.trim(), protocol: form.protocol, openai_format: form.openai_format, token_count_protocol: form.token_count_protocol,
    }
    if (form.api_key.trim()) payload.api_key = form.api_key.trim()
    const result = await request<{ models: ProviderModel[] }>('/api/v1/providers/preview/models/discover', { method: 'POST', body: JSON.stringify(payload) })
    form.models = (result.models || []).map((item) => ({ ...item, enabled: item.enabled !== false, source: item.source || 'discovered' }))
    resetExpandedModels()
    message.success(`已获取 ${form.models.length} 个模型`)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '获取模型失败')
  } finally {
    discovering.value = false
  }
}

async function test(provider: Provider) {
  try {
    const result = await request<{ message: string }>(`/api/v1/providers/${encodeURIComponent(provider.id)}/test`, { ...({ method: 'POST', body: '{}' }) })
    message.success(result.message || '连接正常')
    await store.reloadProviders()
  } catch (error) {
    message.error(error instanceof Error ? error.message : '连接测试失败')
    await store.reloadProviders()
  }
}

async function setDefault(provider: Provider) {
  const suggested = provider.models?.find((item) => item.enabled)?.id || ''
  const modelID = window.prompt('请输入该供应商的默认模型 ID', suggested)
  if (!modelID?.trim()) return
  try {
    await request('/api/v1/settings/default-model', { method: 'PUT', body: JSON.stringify({ provider_id: provider.id, model_id: modelID.trim() }) })
    await store.reloadProviders()
    message.success('默认模型已更新')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '设置默认模型失败')
  }
}

async function remove(provider: Provider) {
  if (!window.confirm(`确认删除供应商“${provider.name}”？历史会话不会被删除。`)) return
  try {
    await request(`/api/v1/providers/${encodeURIComponent(provider.id)}`, { method: 'DELETE' })
    await store.reloadProviders()
    message.success('供应商已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除供应商失败')
  }
}
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">MODEL POOL</p>
        <h2>供应商与模型目录</h2>
        <p>集中管理 OpenAI Chat / Responses 兼容接口和 Gemini 供应商，保存后立即加入 Agent 模型池。</p>
      </div>
      <NButton type="primary" size="large" @click="openEditor()">＋ 添加供应商</NButton>
    </div>

    <NAlert v-if="store.providers.length === 0" type="info" :show-icon="false" class="empty-panel">
      还没有模型供应商。添加后可以从右上角 <code>chat</code> 进入对话测试，也可以先创建项目和配置文件。
    </NAlert>

    <div class="provider-grid">
      <NCard v-for="provider in sortedProviders" :key="provider.id" class="provider-card" hoverable>
        <template #header>
          <div class="provider-card-title">
            <div class="provider-avatar">✦</div>
            <div class="provider-title-copy">
              <strong>{{ provider.name }}</strong>
              <span>{{ provider.id }}</span>
            </div>
          </div>
        </template>
        <template #header-extra>
          <NTag round size="small" :type="statusOf(provider).type">{{ statusOf(provider).label }}</NTag>
        </template>
        <div class="provider-meta-row">
          <NTag size="small" :bordered="false">{{ provider.protocol === 'gemini' ? 'Gemini' : 'OpenAI 兼容' }}</NTag>
          <NTag v-if="provider.protocol !== 'gemini'" size="small" :bordered="false">{{ provider.openai_format || 'auto' }}</NTag>
          <NTag v-if="provider.token_count_protocol" size="small" type="info" :bordered="false">计数 {{ provider.token_count_protocol }}</NTag>
          <NTag size="small" :bordered="false">密钥 {{ provider.api_key_configured ? '已配置' : '未配置' }}</NTag>
        </div>
        <p class="provider-url" :title="provider.base_url">{{ provider.base_url || '官方默认地址' }}</p>
        <div class="provider-model-summary">
          <span class="summary-label">模型目录</span>
          <span>{{ provider.models?.length || 0 }} 个可用模型</span>
        </div>
        <template #footer>
          <NSpace :size="8" wrap>
            <NButton size="small" secondary @click="test(provider)">测试连接</NButton>
            <NButton size="small" secondary @click="openEditor(provider)">编辑</NButton>
            <NButton size="small" secondary @click="setDefault(provider)">设为默认</NButton>
            <NButton size="small" tertiary type="error" @click="remove(provider)">删除</NButton>
          </NSpace>
        </template>
      </NCard>
    </div>

    <NModal v-model:show="showEditor" preset="card" style="width: min(900px, calc(100vw - 32px))" :title="editingID ? '编辑供应商' : '添加供应商'" :mask-closable="false">
      <NForm label-placement="top" :show-feedback="false">
        <div class="form-grid-2">
          <NFormItem label="Provider ID" required>
            <NInput v-model:value="form.id" :disabled="Boolean(editingID)" placeholder="例如 deepseek" />
          </NFormItem>
          <NFormItem label="显示名称" required>
            <NInput v-model:value="form.name" placeholder="例如 DeepSeek" />
          </NFormItem>
        </div>
        <NFormItem label="API 地址" required>
          <NInput v-model:value="form.base_url" placeholder="例如 https://api.deepseek.com/v1" />
        </NFormItem>
        <div class="form-grid-2">
          <NFormItem label="协议">
            <NSelect v-model:value="form.protocol" :options="protocolOptions" />
          </NFormItem>
          <NFormItem v-if="form.protocol !== 'gemini'" label="OpenAI 线路">
            <NSelect v-model:value="form.openai_format" :options="formatOptions" />
          </NFormItem>
        </div>
        <NFormItem label="Token 计数线路">
          <NSelect v-model:value="form.token_count_protocol" :options="tokenCountProtocolOptions" />
          <template #feedback>仅在明确开启模型探测时调用；Anthropic 线路不会改变生成协议，失败仍回退为启发式估算。</template>
        </NFormItem>
        <NFormItem label="API 密钥">
          <NInput v-model:value="form.api_key" type="password" show-password-on="click" :placeholder="editingID ? '留空表示保留旧密钥' : '请输入 API 密钥'" />
        </NFormItem>

        <div class="model-directory-panel">
          <div class="section-heading-row">
            <div>
              <h3>模型目录</h3>
              <p>可以在线获取，也可以手动维护目录外模型的 ID。</p>
            </div>
            <NButton secondary :loading="discovering" @click="discoverModels">获取可用模型</NButton>
          </div>
          <div v-if="!form.models.length" class="inline-empty">还没有模型目录，保存后仍可在 chat 中手动输入模型 ID。</div>
          <div v-for="(model, index) in form.models" :key="`${model.id}-${index}`" class="model-row">
            <div class="model-row-main">
              <NInput v-model:value="model.id" placeholder="模型 ID" />
              <NInput v-model:value="model.display_name" placeholder="显示名称" />
              <NButton text class="model-expand-button" :aria-expanded="expandedModels[index] === true" :aria-label="`${expandedModels[index] ? '收起' : '展开'}${model.id || '模型'}详情`" @click="toggleModelDetails(index)">{{ expandedModels[index] ? '⌃' : '⌄' }}</NButton>
              <NButton quaternary type="error" aria-label="删除模型" @click="removeModel(index)">删除</NButton>
            </div>
            <div v-if="expandedModels[index]" class="model-row-details">
              <label class="model-detail-field"><span>上下文窗口</span><NInputNumber :value="model.context_window || null" :min="0" :show-button="false" placeholder="例如 256K" @update:value="(value) => { model.context_window = value || undefined }" /></label>
              <label class="model-detail-field"><span>最大输出 token</span><NInputNumber :value="model.max_output_tokens || null" :min="0" :show-button="false" placeholder="例如 32K" @update:value="(value) => { model.max_output_tokens = value || undefined }" /></label>
              <div class="model-capability-box">
                <div class="model-capability-heading">
                  <span>能力证据</span>
                  <NSpace v-if="editingID && model.id" :size="6">
                    <NButton size="tiny" secondary :loading="probingModels[probeKey(model.id)] === true" @click="probeModel(model)">基础探测</NButton>
                    <NButton size="tiny" tertiary :loading="probingModels[probeKey(model.id)] === true" @click="probeModel(model, true)">深度探测</NButton>
                  </NSpace>
                </div>
                <div v-if="model.capabilities" class="model-capability-tags">
                  <NTag size="small" :type="supportType(model.capabilities.tool_calling)">工具 {{ supportLabel(model.capabilities.tool_calling) }}</NTag>
                  <NTag size="small" :type="supportType(model.capabilities.streaming)">流式 {{ supportLabel(model.capabilities.streaming) }}</NTag>
                  <NTag size="small" :type="supportType(model.capabilities.structured_output)">JSON {{ supportLabel(model.capabilities.structured_output) }}</NTag>
                  <NTag size="small" :type="supportType(model.capabilities.structured_output_schema)">Schema {{ supportLabel(model.capabilities.structured_output_schema) }}</NTag>
                  <NTag size="small" :type="supportType(model.capabilities.images)">图片 {{ supportLabel(model.capabilities.images) }}</NTag>
                  <NTag size="small" :type="supportType(model.capabilities.audio)">音频 {{ supportLabel(model.capabilities.audio) }}</NTag>
                  <NTag size="small" :type="supportType(model.capabilities.input_files)">文件 {{ supportLabel(model.capabilities.input_files) }}</NTag>
                  <NTag size="small" :type="supportType(model.capabilities.reasoning_effort)">推理 {{ supportLabel(model.capabilities.reasoning_effort) }}</NTag>
                  <NTag size="small" :type="model.capabilities.tokenizer?.known ? 'success' : 'default'">Token 计数 {{ model.capabilities.tokenizer?.known ? '精确' : '启发式' }}</NTag>
                  <span class="model-capability-source">{{ model.capabilities.route || model.capabilities.protocol }} · {{ model.capabilities.tool_calling.source || 'unknown source' }}</span>
                </div>
                <span v-else class="model-capability-empty">尚无探测结果，运行一次只读能力探测即可建立证据。</span>
                <div v-if="observationHistoryFor(model.id).length" class="model-capability-history">
                  <span class="model-capability-history-title">最近证据</span>
                  <span v-for="observation in observationHistoryFor(model.id).slice(0, 3)" :key="observation.id" class="model-capability-history-item">{{ observation.feature }} · {{ supportLabel(observation) }}</span>
                </div>
              </div>
            </div>
          </div>
          <NButton dashed block @click="addModel">＋ 添加模型</NButton>
        </div>
      </NForm>
      <template #footer>
        <div class="modal-footer">
          <NButton @click="showEditor = false">取消</NButton>
          <NButton type="primary" :loading="saving" @click="save">保存配置</NButton>
        </div>
      </template>
    </NModal>
  </div>
</template>
