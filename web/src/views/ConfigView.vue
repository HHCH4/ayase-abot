<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NButton, NCard, NCheckbox, NDynamicTags, NEmpty, NInput, NInputNumber, NSelect, NSpace, NTabPane, NTag, NTabs, useMessage } from 'naive-ui'
import { readConfigRevisions, readPersonas, request } from '@/api'
import { configuredModelOptions, makeModelReference, parseModelReference } from '@/model-catalog'
import { useAppStore } from '@/stores/app'
import type { ConfigField, ConfigProfile, ConfigRevision, SubAgentProfileDescriptor, SubAgentProfileSettings } from '@/types'

const store = useAppStore()
const message = useMessage()
const mode = ref<'profile' | 'system'>('profile')
const view = ref<'visual' | 'json'>('visual')
const selectedID = ref('')
const group = ref('ai')
const search = ref('')
const profileName = ref('')
const draft = reactive<Record<string, unknown>>({})
const systemDraft = reactive<Record<string, unknown>>({
  log_level: 'info', request_timeout_seconds: 300,
  artifact_quota_bytes: 4 * 1024 * 1024 * 1024, artifact_stale_upload_seconds: 86400, artifact_input_retention_seconds: 7 * 86400,
  modal_fallback_enabled: false, modal_fallback_provider_id: '', modal_fallback_vision_model: '', modal_fallback_audio_model: '',
  subagent_profiles: {},
})
const revisions = ref<ConfigRevision[]>([])
const dirty = ref(false)
const jsonText = ref('{}')
const saving = ref(false)
const showHistory = ref(false)
const importInput = ref<HTMLInputElement | null>(null)
const personas = ref<{ id: string; name: string; enabled: boolean }[]>([])

const groupItems = [
  { key: 'ai', label: 'AI 与模型' },
  { key: 'capabilities', label: '能力' },
  { key: 'persona', label: '人格' },
  { key: 'context', label: '上下文管理' },
  { key: 'agent', label: 'Agent 执行' },
  { key: 'workspace', label: '项目能力' },
  { key: 'message', label: '消息输出' },
  { key: 'memory', label: '长期记忆' },
  { key: 'plugins', label: '插件配置' },
  { key: 'platform', label: '平台配置' },
  { key: 'extensions', label: '扩展功能' },
]

const currentProfile = computed(() => store.configProfiles.find((item) => item.id === selectedID.value) || store.defaultProfile)
const profileOptions = computed(() => store.configProfiles.map((item) => ({ label: `${item.name}${item.is_default ? ' · 默认' : ''}`, value: item.id })))
const currentFields = computed(() => (store.configSchema.fields || []).filter((field) => {
  if (!search.value.trim() && field.group !== group.value) return false
  if (!displayMatches(field)) return false
  if (!search.value.trim()) return true
  const text = [field.label, field.key, field.help].filter(Boolean).join(' ').toLocaleLowerCase()
  return text.includes(search.value.trim().toLocaleLowerCase())
}))

// 平台字段按基本、白名单、限速和安全分区展示；搜索结果仍保持扁平。
function platformSectionStart(key: string) {
  if (group.value !== 'platform' || search.value.trim()) return ''
  const sections: Record<string, string> = {
    'platform.admin_ids': '基本设置',
    'platform.whitelist_enabled': '白名单',
    'platform.rate_limit_seconds': '速率限制',
    'platform.ignore_bot_self_message': '其他行为',
    'platform.block_patterns': '内容安全',
    'platform.telegram_pre_ack_enabled': 'Telegram',
  }
  return sections[key] || ''
}

// 这些字段保存的都是供应商模型 ID，统一改用已配置目录选择，避免同一个模型在不同页面重复手写。
// 群图片转述与普通多模态降级一样，从现有模型目录选择，避免再次手写模型 ID。
const modelFieldKeys = new Set(['ai.default_model_id', 'image.caption_model', 'voice.stt_model', 'voice.tts_model', 'extensions.group_image_caption_model'])

function clone(value: unknown): unknown {
  return JSON.parse(JSON.stringify(value ?? {}))
}

function fieldValue(field: ConfigField) {
  return Object.prototype.hasOwnProperty.call(draft, field.key) ? draft[field.key] : field.default
}

function textValue(field: ConfigField) {
  const value = fieldValue(field)
  return typeof value === 'string' ? value : value == null ? '' : String(value)
}

function numberValue(field: ConfigField) {
  const value = fieldValue(field)
  return typeof value === 'number' ? value : Number(value || 0)
}

// listValue 把新旧两种列表形状都转换为标签编辑器需要的字符串数组。
function listValue(field: ConfigField): string[] {
  const value = fieldValue(field)
  if (Array.isArray(value)) return value.map((item) => String(item).trim()).filter(Boolean)
  if (typeof value === 'string') {
    try {
      const parsed = JSON.parse(value) as unknown
      return Array.isArray(parsed) ? parsed.map((item) => String(item).trim()).filter(Boolean) : []
    } catch {
      return value.trim() ? [value.trim()] : []
    }
  }
  return []
}

// setListValue 统一去空白和去重，保证标签编辑器不会产生无效重复项。
function setListValue(field: ConfigField, value: unknown) {
  const values = Array.isArray(value) ? value.map((item) => String(item).trim()).filter(Boolean) : []
  draft[field.key] = [...new Set(values)]
  dirty.value = true
}

// normalizeListDraft 让旧配置在第一次打开或保存其他字段时自动升级为数组。
function normalizeListDraft() {
  for (const field of store.configSchema.fields || []) {
    if (field.type === 'list' && Object.prototype.hasOwnProperty.call(draft, field.key)) {
      draft[field.key] = listValue(field)
    }
  }
}

function systemFieldValue(field: ConfigField) {
  return Object.prototype.hasOwnProperty.call(systemDraft, field.key) ? systemDraft[field.key] : field.default
}

function systemTextValue(field: ConfigField) {
  const value = systemFieldValue(field)
  return typeof value === 'string' ? value : value == null ? '' : String(value)
}

function systemNumberValue(field: ConfigField) {
  const value = systemFieldValue(field)
  return typeof value === 'number' ? value : Number(value || 0)
}

function setSystemValue(field: ConfigField, value: unknown) {
  systemDraft[field.key] = value
  // 打开降级开关时优先选择目录中已经确认支持图片的模型，避免用户还要重复输入已配置的模型 ID。
  if (field.key === 'modal_fallback_enabled' && value === true && !String(systemDraft.modal_fallback_vision_model || '').trim()) {
    const providerID = fallbackPrimaryProviderID()
    const provider = store.providers.find((item) => item.id === providerID)
    const candidate = (provider?.models || []).find((item) => {
      if (item.enabled === false) return false
      const state = item.capabilities?.images?.state
      return state === 'supported' || state === 'degraded'
    })
    if (candidate) {
      systemDraft.modal_fallback_provider_id = providerID
      systemDraft.modal_fallback_vision_model = candidate.id
    }
  }
}

function isModelField(field: ConfigField) {
  return modelFieldKeys.has(field.key)
}

function profileModelOptions(field: ConfigField) {
  const current = textValue(field).trim()
  const preferredProvider = field.key === 'ai.default_model_id' ? String(draft['ai.default_provider_id'] || '').trim() : ''
  const options = configuredModelOptions(store.providers).filter((item) => !preferredProvider || item.providerID === preferredProvider)
  const result = [
    { label: field.key === 'ai.default_model_id' ? '沿用供应商默认模型' : '不指定模型', value: '' },
    ...options.map((item) => ({ label: item.label, value: item.value })),
  ]
  if (current && !options.some((item) => item.modelID === current)) {
    // 旧配置可能来自目录外模型；保留它的显示和运行能力，但不再要求用户重新输入。
    result.push({ label: `当前值：${current}（目录外）`, value: makeModelReference('current', current) })
  }
  return result
}

function profileModelControlValue(field: ConfigField) {
  const current = textValue(field).trim()
  if (!current) return ''
  const options = profileModelOptions(field)
  return options.find((item) => parseModelReference(item.value)?.modelID === current)?.value || makeModelReference('current', current)
}

function setProfileModelValue(field: ConfigField, value: unknown) {
  const raw = String(value || '')
  const reference = parseModelReference(raw)
  if (!reference || reference.providerID === 'current') {
    setValue(field, reference?.modelID || '')
    return
  }
  setValue(field, reference.modelID)
  // 默认模型同时绑定供应商，防止选择了 A 供应商的模型却继续沿用 B 供应商。
  if (field.key === 'ai.default_model_id') {
    draft['ai.default_provider_id'] = reference.providerID
  }
}

const fallbackProviderOptions = computed(() => {
  const current = String(systemDraft.modal_fallback_provider_id || '').trim()
  const result = [{ label: '跟随主模型供应商', value: '' }, ...store.providers.map((item) => ({ label: item.name, value: item.id }))]
  if (current && !result.some((item) => item.value === current)) result.push({ label: `当前值：${current}（供应商不存在）`, value: current })
  return result
})

function fallbackPrimaryProviderID() {
  const configured = String(systemDraft.modal_fallback_provider_id || '').trim()
  if (configured) return configured
  const profileProvider = String(currentProfile.value?.values?.['ai.default_provider_id'] || '').trim()
  return store.defaults.provider_id || profileProvider || store.providers[0]?.id || ''
}

function isFallbackModelField(field: ConfigField) {
  return field.key === 'modal_fallback_vision_model' || field.key === 'modal_fallback_audio_model'
}

function fallbackModelOptions(field: ConfigField) {
  const current = systemTextValue(field).trim()
  const selectedProvider = String(systemDraft.modal_fallback_provider_id || '').trim()
  const primaryProvider = fallbackPrimaryProviderID()
  const providerIDs = selectedProvider ? [selectedProvider] : primaryProvider ? [primaryProvider] : store.providers.map((item) => item.id)
  const options = configuredModelOptions(store.providers).filter((item) => providerIDs.includes(item.providerID))
  const result = [{ label: '未指定（收到附件时只提示，不调用降级模型）', value: '' }, ...options.map((item) => ({ label: item.label, value: item.value }))]
  if (current && !options.some((item) => item.modelID === current)) {
    result.push({ label: `当前值：${current}（目录外）`, value: makeModelReference('current', current) })
  }
  return result
}

function fallbackModelControlValue(field: ConfigField) {
  const current = systemTextValue(field).trim()
  if (!current) return ''
  const options = fallbackModelOptions(field)
  return options.find((item) => parseModelReference(item.value)?.modelID === current)?.value || makeModelReference('current', current)
}

function setFallbackProvider(value: unknown) {
  const providerID = String(value || '')
  systemDraft.modal_fallback_provider_id = providerID
  // 切换供应商后清理不属于新目录的模型，避免保存一个看似有效但运行时找不到的组合。
  const targetProviderID = providerID || fallbackPrimaryProviderID()
  if (targetProviderID) {
    const provider = store.providers.find((item) => item.id === targetProviderID)
    const modelIDs = new Set((provider?.models || []).filter((item) => item.enabled !== false).map((item) => item.id))
    for (const key of ['modal_fallback_vision_model', 'modal_fallback_audio_model']) {
      const current = String(systemDraft[key] || '').trim()
      if (current && !modelIDs.has(current)) systemDraft[key] = ''
    }
  }
}

function setFallbackModel(field: ConfigField, value: unknown) {
  const reference = parseModelReference(String(value || ''))
  if (!reference || reference.providerID === 'current') {
    systemDraft[field.key] = reference?.modelID || ''
    return
  }
  systemDraft[field.key] = reference.modelID
  // 未指定供应商时，选择目录模型会自动锁定它所属的供应商，确保运行时路由一致。
  if (!String(systemDraft.modal_fallback_provider_id || '').trim()) systemDraft.modal_fallback_provider_id = reference.providerID
}

const subagentProfileSchema = computed<SubAgentProfileDescriptor[]>(() => store.systemSettings.subagent_profile_schema || [])

function subagentProfilesDraft(): Record<string, SubAgentProfileSettings> {
  const value = systemDraft.subagent_profiles
  if (!value || typeof value !== 'object' || Array.isArray(value)) return {}
  return value as Record<string, SubAgentProfileSettings>
}

function subagentProfileValue(profileID: string): SubAgentProfileSettings {
  return subagentProfilesDraft()[profileID] || {}
}

function subagentModelOptions(profileID: string) {
  const current = subagentProfileValue(profileID)
  const currentProvider = String(current.provider_id || '').trim()
  const currentModel = String(current.model_id || '').trim()
  const result = [{ label: '继承主 Agent 模型', value: '' }, ...configuredModelOptions(store.providers).map((item) => ({ label: item.label, value: item.value }))]
  if (currentProvider && currentModel && !result.some((item) => parseModelReference(item.value)?.providerID === currentProvider && parseModelReference(item.value)?.modelID === currentModel)) {
    result.push({ label: `当前值：${currentProvider} / ${currentModel}（目录外）`, value: makeModelReference(currentProvider, currentModel) })
  }
  return result
}

function subagentModelControlValue(profileID: string) {
  const current = subagentProfileValue(profileID)
  const providerID = String(current.provider_id || '').trim()
  const modelID = String(current.model_id || '').trim()
  if (!providerID || !modelID) return ''
  return makeModelReference(providerID, modelID)
}

function setSubagentModel(profileID: string, value: unknown) {
  const profiles = subagentProfilesDraft()
  const current = { ...subagentProfileValue(profileID) }
  const reference = parseModelReference(String(value || ''))
  current.provider_id = reference?.providerID || ''
  current.model_id = reference?.modelID || ''
  if (Object.values(current).every((item) => item === undefined || item === null || item === '')) delete profiles[profileID]
  else profiles[profileID] = current
  systemDraft.subagent_profiles = profiles
}

function setSubagentValue(profileID: string, key: keyof SubAgentProfileSettings, value: unknown) {
  const profiles = subagentProfilesDraft()
  const current = { ...subagentProfileValue(profileID) }
  if (value === null || value === undefined || value === '') delete current[key]
  else current[key] = value as never
  // 空 profile 不落盘，保证“继承主 Agent”仍然是明确的默认语义。
  if (Object.values(current).every((item) => item === undefined || item === null || item === '')) delete profiles[profileID]
  else profiles[profileID] = current
  systemDraft.subagent_profiles = profiles
}

function subagentNumberValue(profileID: string, key: keyof SubAgentProfileSettings) {
  const value = subagentProfileValue(profileID)[key]
  return typeof value === 'number' ? value : null
}

const reasoningOptions = [
  { label: '继承主 Agent', value: '' },
  { label: '最少', value: 'minimal' },
  { label: '低', value: 'low' },
  { label: '中', value: 'medium' },
  { label: '高', value: 'high' },
]

const failurePolicyOptions = [
  { label: '继承默认（继续其它节点）', value: '' },
  { label: '继续其它节点', value: 'continue' },
  { label: '遇到失败立即中止', value: 'abort' },
]

function syncSystemDraft() {
  const value = store.systemSettings
  Object.keys(systemDraft).forEach((key) => { delete systemDraft[key] })
  Object.assign(systemDraft, {
    log_level: value.log_level, request_timeout_seconds: value.request_timeout_seconds,
    artifact_quota_bytes: value.artifact_quota_bytes, artifact_stale_upload_seconds: value.artifact_stale_upload_seconds, artifact_input_retention_seconds: value.artifact_input_retention_seconds,
    modal_fallback_enabled: value.modal_fallback_enabled, modal_fallback_provider_id: value.modal_fallback_provider_id,
    modal_fallback_vision_model: value.modal_fallback_vision_model, modal_fallback_audio_model: value.modal_fallback_audio_model,
    subagent_profiles: clone(value.subagent_profiles || {}),
  })
}

function displayMatches(field: ConfigField) {
  return Object.entries(field.display_if || {}).every(([key, expected]) => {
    const dependency = store.configSchema.fields.find((item) => item.key === key)
    const actual = dependency ? fieldValue(dependency) : draft[key]
    return JSON.stringify(actual) === JSON.stringify(expected)
  })
}

function fieldOptions(field: ConfigField) {
  if (field.option_source === 'providers') return [{ label: '沿用供应商注册表默认', value: '' }, ...store.providers.map((item) => ({ label: item.name, value: item.id }))]
  if (field.option_source === 'personas') return [{ label: '沿用默认人格', value: '' }, ...personas.value.filter((item) => item.enabled).map((item) => ({ label: item.name, value: item.id }))]
  return (field.options || []).map((item) => ({ label: item.label, value: item.value }))
}

function setValue(field: ConfigField, value: unknown) {
  draft[field.key] = value
  dirty.value = true
}

function resetDraft(profile?: ConfigProfile) {
  Object.keys(draft).forEach((key) => delete draft[key])
  Object.assign(draft, clone(profile?.values || {}) as Record<string, unknown>)
  normalizeListDraft()
  profileName.value = profile?.name || ''
  jsonText.value = JSON.stringify(draft, null, 2)
  dirty.value = false
}

async function chooseProfile(id: string) {
  if (dirty.value && !window.confirm('当前配置有未保存修改，确认放弃吗？')) return
  selectedID.value = id
  resetDraft(store.configProfiles.find((item) => item.id === id))
  await loadHistory()
}

async function loadHistory() {
  revisions.value = []
  if (!selectedID.value) return
  try {
    revisions.value = await readConfigRevisions(selectedID.value)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取修订历史失败')
  }
}

async function reloadConfig(preferredID = selectedID.value) {
  await store.loadConfig()
  selectedID.value = preferredID || store.defaultProfileID || store.configProfiles[0]?.id || ''
  resetDraft(store.configProfiles.find((item) => item.id === selectedID.value))
  syncSystemDraft()
  await loadHistory()
}

async function saveProfile() {
  if (!selectedID.value) return
  if (view.value === 'json') {
    try {
      const parsed = JSON.parse(jsonText.value) as Record<string, unknown>
      Object.keys(draft).forEach((key) => delete draft[key])
      Object.assign(draft, parsed)
      normalizeListDraft()
    } catch (error) {
      message.error(`JSON 格式无效：${error instanceof Error ? error.message : '无法解析'}`)
      return
    }
  }
  saving.value = true
  try {
    const body = { id: selectedID.value, name: profileName.value.trim(), values: draft }
    const validation = await request<{ valid: boolean; error?: string }>(`/api/v1/config-profiles/${encodeURIComponent(selectedID.value)}/validate`, { method: 'POST', body: JSON.stringify(body) })
    if (!validation.valid) throw new Error(validation.error || '配置校验失败')
    await request(`/api/v1/config-profiles/${encodeURIComponent(selectedID.value)}`, { method: 'PUT', body: JSON.stringify(body) })
    await reloadConfig(selectedID.value)
    message.success('配置已保存并立即生效')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存配置失败')
  } finally {
    saving.value = false
  }
}

async function createProfile() {
  const name = window.prompt('请输入配置文件名称', '新配置')
  if (!name?.trim()) return
  try {
    const profile = await request<ConfigProfile>('/api/v1/config-profiles', { method: 'POST', body: JSON.stringify({ name: name.trim(), values: {} }) })
    await reloadConfig(profile.id)
    message.success('配置文件已创建')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '创建配置文件失败')
  }
}

async function copyProfile() {
  if (!selectedID.value) return
  const name = window.prompt('请输入副本名称', `${currentProfile.value?.name || '配置'} 副本`)
  if (!name?.trim()) return
  try {
    const profile = await request<ConfigProfile>(`/api/v1/config-profiles/${encodeURIComponent(selectedID.value)}/copy`, { method: 'POST', body: JSON.stringify({ name: name.trim() }) })
    await reloadConfig(profile.id)
    message.success('配置副本已创建')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '复制配置失败')
  }
}

async function setDefault() {
  if (!selectedID.value) return
  try {
    await request(`/api/v1/config-profiles/${encodeURIComponent(selectedID.value)}/default`, { method: 'POST', body: '{}' })
    await reloadConfig(selectedID.value)
    message.success('已切换系统默认配置')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '切换默认配置失败')
  }
}

async function deleteProfile() {
  if (!currentProfile.value || currentProfile.value.is_default) return
  if (!window.confirm(`彻底删除“${currentProfile.value.name}”及全部修订历史？`)) return
  try {
    await request(`/api/v1/config-profiles/${encodeURIComponent(currentProfile.value.id)}`, { method: 'DELETE' })
    await reloadConfig()
    message.success('配置文件已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除配置失败')
  }
}

async function exportProfile() {
  if (!selectedID.value) return
  try {
    const value = await request<Record<string, unknown>>(`/api/v1/config-profiles/${encodeURIComponent(selectedID.value)}/export`)
    const link = document.createElement('a')
    link.href = URL.createObjectURL(new Blob([JSON.stringify(value, null, 2)], { type: 'application/json' }))
    link.download = `${selectedID.value}.json`
    link.click()
    URL.revokeObjectURL(link.href)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '导出配置失败')
  }
}

async function importProfile(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  try {
    const value = JSON.parse(await file.text()) as { version?: number; id?: string; name?: string; values?: Record<string, unknown> }
    const body = { version: value.version, id: value.id, name: value.name, values: value.values || {}, overwrite: false }
    let profile: ConfigProfile
    try {
      profile = await request<ConfigProfile>('/api/v1/config-profiles/import', { method: 'POST', body: JSON.stringify(body) })
    } catch (error) {
      if (!(error instanceof Error) || !/已存在|冲突/.test(error.message) || !window.confirm('配置文件已存在，确认覆盖同 ID 配置吗？')) throw error
      body.overwrite = true
      profile = await request<ConfigProfile>('/api/v1/config-profiles/import', { method: 'POST', body: JSON.stringify(body) })
    }
    await reloadConfig(profile.id)
    message.success('配置已导入')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '导入配置失败')
  }
}

async function saveSystem() {
  try {
    const value = await request<typeof systemDraft>('/api/v1/system-settings', { method: 'PUT', body: JSON.stringify(systemDraft) })
    Object.assign(store.systemSettings, value)
    syncSystemDraft()
    message.success('系统设置已保存并立即生效')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存系统设置失败')
  }
}

onMounted(async () => {
  await store.loadAll()
  // 人格选择器复用独立目录，配置文件本身只保存稳定 ID。
  try {
    personas.value = (await readPersonas()).personas || []
  } catch {
    personas.value = []
  }
  selectedID.value = store.defaultProfileID || store.configProfiles[0]?.id || ''
  resetDraft(currentProfile.value)
  syncSystemDraft()
  await loadHistory()
})
</script>

<template>
  <div class="page-view">
    <div class="page-intro config-page-intro">
      <div><p class="eyebrow">CONFIG CENTER</p><h2>配置文件</h2><p>普通配置和系统设置共用同一套 Schema 表单；每次保存都会先校验，成功后立即应用到下一轮请求。</p></div>
      <NSpace><NButton secondary @click="exportProfile">导出配置</NButton><NButton secondary @click="importInput?.click()">导入配置</NButton><input ref="importInput" type="file" accept="application/json,.json" class="visually-hidden" @change="importProfile" /></NSpace>
    </div>

    <NCard class="config-card" :bordered="false">
      <NTabs v-model:value="mode" type="segment" animated>
        <NTabPane name="profile" tab="普通配置">
          <div class="config-toolbar">
            <NSelect class="profile-select" :value="selectedID" :options="profileOptions" @update:value="chooseProfile" />
            <NInput v-model:value="search" clearable placeholder="搜索设置名称、说明或配置键" class="config-search" />
            <NSpace><NButton secondary @click="createProfile">新建</NButton><NButton secondary :disabled="!currentProfile" @click="copyProfile">复制</NButton><NButton secondary :disabled="!currentProfile || currentProfile.is_default" @click="setDefault">设为默认</NButton><NButton tertiary type="error" :disabled="!currentProfile || currentProfile.is_default" @click="deleteProfile">删除</NButton></NSpace>
          </div>
          <div class="config-meta-row"><div><NInput v-model:value="profileName" placeholder="配置名称" @update:value="dirty = true" /><span v-if="currentProfile" class="muted">当前修订 v{{ currentProfile.revision }}{{ currentProfile.is_default ? ' · 系统默认' : '' }}</span></div><NButton text @click="showHistory = !showHistory">{{ showHistory ? '收起修订历史' : '查看修订历史' }}</NButton></div>
          <div v-if="showHistory" class="revision-panel"><div v-for="revision in revisions" :key="`${revision.profile_id}-${revision.revision}`" class="revision-row"><strong>v{{ revision.revision }}</strong><span>{{ revision.created_at ? new Date(revision.created_at).toLocaleString() : '未知时间' }}</span><NTag v-if="revision.revision === currentProfile?.revision" size="small" type="success">当前</NTag></div><span v-if="!revisions.length" class="muted">暂无修订历史</span></div>
          <div class="config-editor-layout">
            <aside class="config-groups"><button v-for="item in groupItems" :key="item.key" type="button" :class="{ active: group === item.key }" @click="group = item.key">{{ item.label }}</button></aside>
            <section class="config-editor">
              <div class="editor-view-tabs"><NButton size="small" :type="view === 'visual' ? 'primary' : 'default'" @click="view = 'visual'">可视化</NButton><NButton size="small" :type="view === 'json' ? 'primary' : 'default'" @click="jsonText = JSON.stringify(draft, null, 2); view = 'json'">JSON（高级）</NButton></div>
              <div v-if="view === 'visual'" class="field-list">
                <template v-for="field in currentFields" :key="field.key">
                <h3 v-if="platformSectionStart(field.key)" class="config-field-section">{{ platformSectionStart(field.key) }}</h3>
                <div class="config-field-row">
                  <div class="field-copy"><strong>{{ field.label }}</strong><code>{{ field.key }}</code><span v-if="field.help">{{ field.help }}</span><NTag v-if="field.restart_required" size="small" type="warning">需重启</NTag></div>
                  <div class="field-control">
                    <NCheckbox v-if="field.type === 'boolean'" :checked="fieldValue(field) === true" @update:checked="setValue(field, $event)" />
                    <NSelect v-else-if="isModelField(field)" :value="profileModelControlValue(field)" :options="profileModelOptions(field)" filterable clearable @update:value="setProfileModelValue(field, $event)" />
                    <NSelect v-else-if="field.type === 'select'" :value="String(fieldValue(field) ?? '')" :options="fieldOptions(field)" @update:value="setValue(field, $event)" />
                    <NInputNumber v-else-if="field.type === 'integer' || field.type === 'number'" :value="numberValue(field)" :min="field.min" :max="field.max" :step="field.type === 'number' ? 0.01 : 1" :show-button="false" @update:value="setValue(field, $event)" />
                    <NDynamicTags v-else-if="field.type === 'list'" :value="listValue(field)" :closable="true" :input-props="{ placeholder: '输入后按 Enter 添加' }" @update:value="setListValue(field, $event)" />
                    <NInput v-else-if="field.type === 'textarea'" type="textarea" :value="textValue(field)" :autosize="{ minRows: 3, maxRows: 8 }" @update:value="setValue(field, $event)" />
                    <NInput v-else :value="textValue(field)" :type="field.secret ? 'password' : 'text'" @update:value="setValue(field, $event)" />
                  </div>
                </div>
                </template>
                <NEmpty v-if="!currentFields.length" description="当前没有匹配的设置" />
              </div>
              <NInput v-else v-model:value="jsonText" type="textarea" :autosize="{ minRows: 18, maxRows: 30 }" class="json-editor" @update:value="dirty = true" />
              <div class="save-bar"><span :class="{ dirty }">{{ dirty ? '有未保存修改' : '配置已同步' }}</span><NButton type="primary" :loading="saving" :disabled="!currentProfile" @click="saveProfile">保存当前修订</NButton></div>
            </section>
          </div>
        </NTabPane>
        <NTabPane name="system" tab="系统设置">
          <div class="system-settings-panel">
            <div class="section-heading-row"><div><h3>系统设置</h3><p>日志等级、全局请求超时、Artifact 维护和多模态降级保存后立即生效。</p></div></div>
            <div v-for="field in (store.systemSettings.schema || [])" :key="field.key" class="config-field-row">
              <div class="field-copy"><strong>{{ field.label }}</strong><code>system.{{ field.key }}</code><span>{{ field.help }}</span><NTag v-if="field.restart_required" size="small" type="warning">需重启</NTag></div>
              <div class="field-control">
                <NCheckbox v-if="field.type === 'boolean'" :checked="systemFieldValue(field) === true" @update:checked="setSystemValue(field, $event)" />
                <NSelect v-else-if="field.key === 'modal_fallback_provider_id'" :value="String(systemFieldValue(field) ?? '')" :options="fallbackProviderOptions" @update:value="setFallbackProvider" />
                <NSelect v-else-if="isFallbackModelField(field)" :value="fallbackModelControlValue(field)" :options="fallbackModelOptions(field)" filterable clearable @update:value="setFallbackModel(field, $event)" />
                <NSelect v-else-if="field.type === 'select'" :value="String(systemFieldValue(field) ?? '')" :options="field.options || []" @update:value="setSystemValue(field, $event)" />
                <NInputNumber v-else-if="field.type === 'integer' || field.type === 'number'" :value="systemNumberValue(field)" :min="field.min" :max="field.max" :step="field.type === 'number' ? 0.01 : 1" :show-button="false" @update:value="setSystemValue(field, $event)" />
                <NInput v-else :value="systemTextValue(field)" :type="field.secret ? 'password' : 'text'" @update:value="setSystemValue(field, $event)" />
              </div>
            </div>
            <div v-if="subagentProfileSchema.length" class="subagent-settings-panel">
              <div class="section-heading-row">
                <div><h3>子 Agent 配置</h3><p>每种受控子 Agent 可以单独选择已配置的内置模型、思考强度和执行预算。留空表示继承主 Agent；模型引用只使用本机 Provider 目录，不会启用外部 Agent 服务。</p></div>
              </div>
              <div v-for="profile in subagentProfileSchema" :key="profile.id" class="subagent-profile-card">
                <div class="subagent-profile-heading"><div><strong>{{ profile.label }}</strong><code>subagent_profiles.{{ profile.id }}</code><span>{{ profile.description }}</span></div></div>
                <div class="subagent-profile-grid">
                  <label class="subagent-control"><span>模型</span><NSelect :value="subagentModelControlValue(profile.id)" :options="subagentModelOptions(profile.id)" filterable clearable @update:value="setSubagentModel(profile.id, $event)" /></label>
                  <label class="subagent-control"><span>思考强度</span><NSelect :value="String(subagentProfileValue(profile.id).reasoning_effort || '')" :options="reasoningOptions" @update:value="setSubagentValue(profile.id, 'reasoning_effort', $event)" /></label>
                  <label class="subagent-control"><span>温度（留空继承）</span><NInputNumber :value="subagentNumberValue(profile.id, 'temperature')" :min="0" :max="2" :step="0.01" clearable :show-button="false" @update:value="setSubagentValue(profile.id, 'temperature', $event)" /></label>
                  <label class="subagent-control"><span>Top P（留空继承）</span><NInputNumber :value="subagentNumberValue(profile.id, 'top_p')" :min="0.01" :max="1" :step="0.01" clearable :show-button="false" @update:value="setSubagentValue(profile.id, 'top_p', $event)" /></label>
                  <label class="subagent-control"><span>最大输出 token（0 为默认）</span><NInputNumber :value="subagentNumberValue(profile.id, 'max_output_tokens')" :min="0" :max="1536" :step="1" clearable :show-button="false" @update:value="setSubagentValue(profile.id, 'max_output_tokens', $event)" /></label>
                  <label class="subagent-control"><span>超时秒数（0 为默认）</span><NInputNumber :value="subagentNumberValue(profile.id, 'timeout_seconds')" :min="0" :max="300" :step="1" clearable :show-button="false" @update:value="setSubagentValue(profile.id, 'timeout_seconds', $event)" /></label>
                  <label class="subagent-control"><span>最大并发（0 为默认）</span><NInputNumber :value="subagentNumberValue(profile.id, 'max_concurrency')" :min="0" :max="4" :step="1" clearable :show-button="false" @update:value="setSubagentValue(profile.id, 'max_concurrency', $event)" /></label>
                  <label class="subagent-control"><span>输出预算字节（0 为默认）</span><NInputNumber :value="subagentNumberValue(profile.id, 'output_budget_bytes')" :min="0" :max="131072" :step="1024" clearable :show-button="false" @update:value="setSubagentValue(profile.id, 'output_budget_bytes', $event)" /></label>
                  <label class="subagent-control"><span>失败策略</span><NSelect :value="String(subagentProfileValue(profile.id).failure_policy || '')" :options="failurePolicyOptions" @update:value="setSubagentValue(profile.id, 'failure_policy', $event)" /></label>
                </div>
              </div>
            </div>
            <div class="save-bar"><span>保存后立即应用</span><NButton type="primary" @click="saveSystem">保存系统设置</NButton></div>
          </div>
        </NTabPane>
      </NTabs>
    </NCard>
  </div>
</template>
