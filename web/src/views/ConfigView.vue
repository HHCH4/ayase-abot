<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NButton, NCard, NCheckbox, NEmpty, NInput, NInputNumber, NSelect, NSpace, NTabPane, NTag, NTabs, useMessage } from 'naive-ui'
import { readConfigRevisions, request } from '@/api'
import { useAppStore } from '@/stores/app'
import type { ConfigField, ConfigProfile, ConfigRevision } from '@/types'

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
  artifact_quota_bytes: 4 * 1024 * 1024 * 1024, artifact_stale_upload_seconds: 86400,
  modal_fallback_enabled: false, modal_fallback_provider_id: '', modal_fallback_vision_model: '', modal_fallback_audio_model: '',
})
const revisions = ref<ConfigRevision[]>([])
const dirty = ref(false)
const jsonText = ref('{}')
const saving = ref(false)
const showHistory = ref(false)
const importInput = ref<HTMLInputElement | null>(null)

const groupItems = [
  { key: 'ai', label: 'AI 与模型' },
  { key: 'persona', label: '人格' },
  { key: 'context', label: '上下文管理' },
  { key: 'agent', label: 'Agent 执行' },
  { key: 'workspace', label: '项目能力' },
  { key: 'message', label: '消息输出' },
  { key: 'memory', label: '长期记忆' },
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
}

function syncSystemDraft() {
  const value = store.systemSettings
  Object.keys(systemDraft).forEach((key) => { delete systemDraft[key] })
  Object.assign(systemDraft, {
    log_level: value.log_level, request_timeout_seconds: value.request_timeout_seconds,
    artifact_quota_bytes: value.artifact_quota_bytes, artifact_stale_upload_seconds: value.artifact_stale_upload_seconds,
    modal_fallback_enabled: value.modal_fallback_enabled, modal_fallback_provider_id: value.modal_fallback_provider_id,
    modal_fallback_vision_model: value.modal_fallback_vision_model, modal_fallback_audio_model: value.modal_fallback_audio_model,
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
  return (field.options || []).map((item) => ({ label: item.label, value: item.value }))
}

function setValue(field: ConfigField, value: unknown) {
  draft[field.key] = value
  dirty.value = true
}

function resetDraft(profile?: ConfigProfile) {
  Object.keys(draft).forEach((key) => delete draft[key])
  Object.assign(draft, clone(profile?.values || {}) as Record<string, unknown>)
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
              <div class="editor-view-tabs"><NButton size="small" :type="view === 'visual' ? 'primary' : 'default'" @click="view = 'visual'">可视化</NButton><NButton size="small" :type="view === 'json' ? 'primary' : 'default'" @click="jsonText = JSON.stringify(draft, null, 2); view = 'json'">JSON</NButton></div>
              <div v-if="view === 'visual'" class="field-list">
                <div v-for="field in currentFields" :key="field.key" class="config-field-row">
                  <div class="field-copy"><strong>{{ field.label }}</strong><code>{{ field.key }}</code><span v-if="field.help">{{ field.help }}</span><NTag v-if="field.restart_required" size="small" type="warning">需重启</NTag></div>
                  <div class="field-control">
                    <NCheckbox v-if="field.type === 'boolean'" :checked="fieldValue(field) === true" @update:checked="setValue(field, $event)" />
                    <NSelect v-else-if="field.type === 'select'" :value="String(fieldValue(field) ?? '')" :options="fieldOptions(field)" @update:value="setValue(field, $event)" />
                    <NInputNumber v-else-if="field.type === 'integer' || field.type === 'number'" :value="numberValue(field)" :min="field.min" :max="field.max" :step="field.type === 'number' ? 0.01 : 1" :show-button="false" @update:value="setValue(field, $event)" />
                    <NInput v-else-if="field.type === 'textarea'" type="textarea" :value="textValue(field)" :autosize="{ minRows: 3, maxRows: 8 }" @update:value="setValue(field, $event)" />
                    <NInput v-else :value="textValue(field)" :type="field.secret ? 'password' : 'text'" @update:value="setValue(field, $event)" />
                  </div>
                </div>
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
                <NSelect v-else-if="field.type === 'select'" :value="String(systemFieldValue(field) ?? '')" :options="field.options || []" @update:value="setSystemValue(field, $event)" />
                <NInputNumber v-else-if="field.type === 'integer' || field.type === 'number'" :value="systemNumberValue(field)" :min="field.min" :max="field.max" :step="field.type === 'number' ? 0.01 : 1" :show-button="false" @update:value="setSystemValue(field, $event)" />
                <NInput v-else :value="systemTextValue(field)" :type="field.secret ? 'password' : 'text'" @update:value="setSystemValue(field, $event)" />
              </div>
            </div>
            <div class="save-bar"><span>保存后立即应用</span><NButton type="primary" @click="saveSystem">保存系统设置</NButton></div>
          </div>
        </NTabPane>
      </NTabs>
    </NCard>
  </div>
</template>
