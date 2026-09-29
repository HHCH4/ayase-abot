<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { NAlert, NButton, NCard, NDynamicTags, NEmpty, NForm, NFormItem, NInput, NInputNumber, NSelect, NSpace, NSwitch, NTag, useMessage } from 'naive-ui'
import { request } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import { useAppStore } from '@/stores/app'
import type { Bot, BotRuntimeConfig } from '@/types'

const store = useAppStore()
const message = useMessage()
const selectedID = ref('')
const editing = ref(false)
const saving = ref(false)
const testing = ref(false)
const runtimeLoading = ref(false)
const relationBusy = ref<string | null>(null)

type RuntimeOverview = {
  bot_id: string
  instance: { lifecycle_state: string; availability: string; current_load: number; active_sources: number; last_heartbeat_at?: string; last_platform_event_at?: string }
  sources: Array<{ source: string; chat_type: string; last_user_id: string; queued_turn_count: number; last_seen_at?: string }>
  decisions: Array<{ action: string; reason: string; reason_codes?: string[]; source: string; created_at?: string }>
  actions: Array<{ type: string; status: string; payload_summary?: string; platform_result?: string; updated_at?: string }>
  relations: Array<{
    bot_id: string
    scope_type: string
    scope_id: string
    preferred_name?: string
    stable_preferences?: string
    interaction_style?: string
    trust_level?: string
    recent_topics?: string
    commitments?: string
    last_interaction_at?: string
    revision: number
  }>
  follow_ups: Array<{ id: string; name: string; request: string; status: string; next_run_at?: string; last_error?: string; running?: boolean }>
}

const runtimeOverview = ref<RuntimeOverview | null>(null)

type BotForm = {
  id: string
  name: string
  type: string
  endpoint: string
  onebot_mode: string
  listen_host: string
  listen_port: number | null
  listen_path: string
  onebot_file_root: string
  group_trigger_mode: string
  admin_user_ids: string[]
  telegram_token: string
  onebot_access_token: string
  enabled: boolean
  config_profile_id: string
  runtime: RuntimeForm
}

type RuntimeForm = {
  runtime_enabled: BooleanOverride
  runtime_max_concurrency: number | null
  source_queue_limit: number | null
  turn_wait_ms: number | null
  group_turn_wait_ms: number | null
  attachment_wait_ms: number | null
  max_turn_messages: number | null
  private_mode: string
  group_participation_mode: string
  record_unaddressed_messages: BooleanOverride
  reaction_enabled: BooleanOverride
  group_context_enabled: BooleanOverride
  group_message_max_count: number | null
  group_image_caption: BooleanOverride
  group_image_caption_model: string
  proactive_enabled: BooleanOverride
  proactive_degree: string
  cooldown_seconds: number | null
  private_hourly_reply_limit: number | null
  group_hourly_reply_limit: number | null
  heartbeat_seconds: number | null
  quiet_hours_timezone: string
  quiet_hours_start: string
  quiet_hours_end: string
  emergency_bypass_quiet_hours: BooleanOverride
  relation_enabled: BooleanOverride
  relation_retention_seconds: number | null
  follow_up_enabled: BooleanOverride
  follow_up_max: number | null
  follow_up_max_retries: number | null
  follow_up_retry_delay_seconds: number | null
  follow_up_max_delay_seconds: number | null
  follow_up_allowed_sources: string[]
  expression_enabled: BooleanOverride
  expression_max_segments: number | null
  expression_long_threshold: number | null
  expression_delay_ms: number | null
  reply_mention: BooleanOverride
  reply_quote: BooleanOverride
  private_reply_quote: BooleanOverride
  agent_on_demand_enabled: BooleanOverride
  allowed_read_only_tools: string[]
  tool_budget: number | null
  subagent_enabled: BooleanOverride
  message_style: string
  disabled_actions: string[]
}

// 用字符串承载三态布尔值，避免 Naive UI 的选择器把 null/boolean 推断成不兼容的联合类型；
// 保存时再转换为“省略、true 或 false”，从而准确表达继承和显式关闭。
type BooleanOverride = 'inherit' | 'true' | 'false'

const form = reactive<BotForm>(emptyForm())
const profileOptions = computed(() => [
  { label: '使用系统默认配置', value: '' },
  ...store.configProfiles.map((item) => ({ label: `${item.name}${item.is_default ? ' · 默认' : ''}`, value: item.id })),
])
const selectedBot = computed(() => store.bots.find((item) => item.id === selectedID.value))
const statusMap: Record<string, { label: string; type: 'success' | 'warning' | 'error' | 'default' }> = {
  running: { label: '运行中', type: 'success' },
  ready: { label: '连接正常', type: 'success' },
  connecting: { label: '连接中', type: 'warning' },
  configured: { label: '待启动', type: 'warning' },
  stopped: { label: '已停止', type: 'default' },
  error: { label: '连接失败', type: 'error' },
}

function emptyForm(): BotForm {
  return {
    id: '', name: '', type: 'onebot11', endpoint: '', onebot_mode: 'reverse-server', listen_host: '0.0.0.0', listen_port: 6199,
    listen_path: '/ws', onebot_file_root: '', group_trigger_mode: 'mention', admin_user_ids: [], telegram_token: '', onebot_access_token: '', enabled: true, config_profile_id: '', runtime: emptyRuntime(),
  }
}

function emptyRuntime(): RuntimeForm {
  return {
    runtime_enabled: 'inherit', runtime_max_concurrency: null, source_queue_limit: null, turn_wait_ms: null, group_turn_wait_ms: null,
    attachment_wait_ms: null, max_turn_messages: null, private_mode: '', group_participation_mode: '', record_unaddressed_messages: 'inherit', reaction_enabled: 'inherit', group_context_enabled: 'inherit',
    group_message_max_count: null, group_image_caption: 'inherit', group_image_caption_model: '', proactive_enabled: 'inherit', proactive_degree: '',
    cooldown_seconds: null, private_hourly_reply_limit: null, group_hourly_reply_limit: null, heartbeat_seconds: null,
    quiet_hours_timezone: '', quiet_hours_start: '', quiet_hours_end: '', emergency_bypass_quiet_hours: 'inherit',
    relation_enabled: 'inherit', relation_retention_seconds: null, follow_up_enabled: 'inherit', follow_up_max: null, follow_up_max_retries: null, follow_up_retry_delay_seconds: null, follow_up_max_delay_seconds: null, follow_up_allowed_sources: [],
    expression_enabled: 'inherit', expression_max_segments: null, expression_long_threshold: null, expression_delay_ms: null,
    reply_mention: 'inherit', reply_quote: 'inherit', private_reply_quote: 'inherit', agent_on_demand_enabled: 'inherit', allowed_read_only_tools: [],
    tool_budget: null, subagent_enabled: 'inherit', message_style: '', disabled_actions: [],
  }
}

const booleanOptions = [
  { label: '继承配置', value: 'inherit' as BooleanOverride },
  { label: '开启', value: 'true' as BooleanOverride },
  { label: '关闭', value: 'false' as BooleanOverride },
]

function booleanOverride(value?: boolean): BooleanOverride {
  if (value === undefined || value === null) return 'inherit'
  return value ? 'true' : 'false'
}

function runtimeForm(value?: BotRuntimeConfig): RuntimeForm {
  return {
    runtime_enabled: booleanOverride(value?.runtime_enabled), runtime_max_concurrency: value?.runtime_max_concurrency ?? null,
    source_queue_limit: value?.source_queue_limit ?? null, turn_wait_ms: value?.turn_wait_ms ?? null, group_turn_wait_ms: value?.group_turn_wait_ms ?? null,
    attachment_wait_ms: value?.attachment_wait_ms ?? null, max_turn_messages: value?.max_turn_messages ?? null,
    private_mode: value?.private_mode || '', group_participation_mode: value?.group_participation_mode || '',
    record_unaddressed_messages: booleanOverride(value?.record_unaddressed_messages), reaction_enabled: booleanOverride(value?.reaction_enabled),
    group_context_enabled: booleanOverride(value?.group_context_enabled), group_message_max_count: value?.group_message_max_count ?? null,
    group_image_caption: booleanOverride(value?.group_image_caption), group_image_caption_model: value?.group_image_caption_model || '',
    proactive_enabled: booleanOverride(value?.proactive_enabled), proactive_degree: value?.proactive_degree || '', cooldown_seconds: value?.cooldown_seconds ?? null,
    private_hourly_reply_limit: value?.private_hourly_reply_limit ?? null, group_hourly_reply_limit: value?.group_hourly_reply_limit ?? null,
    heartbeat_seconds: value?.heartbeat_seconds ?? null, quiet_hours_timezone: value?.quiet_hours_timezone || '', quiet_hours_start: value?.quiet_hours_start || '', quiet_hours_end: value?.quiet_hours_end || '',
    emergency_bypass_quiet_hours: booleanOverride(value?.emergency_bypass_quiet_hours), relation_enabled: booleanOverride(value?.relation_enabled),
    relation_retention_seconds: value?.relation_retention_seconds ?? null, follow_up_enabled: booleanOverride(value?.follow_up_enabled), follow_up_max: value?.follow_up_max ?? null,
    follow_up_max_retries: value?.follow_up_max_retries ?? null, follow_up_retry_delay_seconds: value?.follow_up_retry_delay_seconds ?? null, follow_up_max_delay_seconds: value?.follow_up_max_delay_seconds ?? null,
    follow_up_allowed_sources: [...(value?.follow_up_allowed_sources || [])], expression_enabled: booleanOverride(value?.expression_enabled),
    expression_max_segments: value?.expression_max_segments ?? null, expression_long_threshold: value?.expression_long_threshold ?? null, expression_delay_ms: value?.expression_delay_ms ?? null,
    reply_mention: booleanOverride(value?.reply_mention), reply_quote: booleanOverride(value?.reply_quote), private_reply_quote: booleanOverride(value?.private_reply_quote),
    agent_on_demand_enabled: booleanOverride(value?.agent_on_demand_enabled), allowed_read_only_tools: [...(value?.allowed_read_only_tools || [])],
    tool_budget: value?.tool_budget ?? null, subagent_enabled: booleanOverride(value?.subagent_enabled), message_style: value?.message_style || '',
    disabled_actions: Object.entries(value?.action_permissions || {}).filter(([, enabled]) => enabled === false).map(([action]) => action),
  }
}

const booleanRuntimeKeys = new Set<string>([
  'runtime_enabled', 'record_unaddressed_messages', 'reaction_enabled', 'group_context_enabled', 'group_image_caption', 'proactive_enabled', 'emergency_bypass_quiet_hours',
  'relation_enabled', 'follow_up_enabled', 'expression_enabled', 'reply_mention', 'reply_quote', 'private_reply_quote',
  'agent_on_demand_enabled', 'subagent_enabled',
])

function runtimePayload(value: RuntimeForm): Record<string, unknown> {
  const result: Record<string, unknown> = {}
  for (const [key, item] of Object.entries(value)) {
	if (key === 'disabled_actions') continue
    if (Array.isArray(item)) {
      if (item.length) result[key] = [...new Set(item.map((entry) => entry.trim()).filter(Boolean))]
    } else if (booleanRuntimeKeys.has(key)) {
      if (item === 'true' || item === 'false') result[key] = item === 'true'
    } else if (item !== null && item !== undefined && item !== '') {
      result[key] = item
    }
  }
  if (value.disabled_actions.length) {
    result.action_permissions = Object.fromEntries(value.disabled_actions.map((action) => [action, false]))
  }
  return result
}

const runtimeActionOptions = [
  { label: '发送文字', value: 'send_text' }, { label: '发送图片', value: 'send_image' },
  { label: '发送音频', value: 'send_audio' }, { label: '发送文件', value: 'send_file' },
  { label: '添加反应', value: 'add_reaction' }, { label: '戳一戳', value: 'poke' },
  { label: '撤回消息', value: 'recall_message' }, { label: '启动 Agent', value: 'start_agent' },
  { label: '创建 Follow-up', value: 'create_follow_up' }, { label: '更新关系状态', value: 'update_relation' },
  { label: '更新来源状态', value: 'update_source_state' },
]

function statusOf(bot: Bot) {
  return statusMap[bot.status] || { label: bot.status || '未知', type: 'default' as const }
}

function fillForm(bot?: Bot) {
  Object.assign(form, bot ? {
    id: bot.id, name: bot.name, type: bot.type, endpoint: bot.endpoint || '', onebot_mode: bot.onebot_mode || (bot.endpoint ? 'client' : 'reverse-server'),
    listen_host: bot.listen_host || '0.0.0.0', listen_port: bot.listen_port || 6199, listen_path: bot.listen_path || '/ws', onebot_file_root: bot.onebot_file_root || '',
    group_trigger_mode: bot.group_trigger_mode || 'mention',
    admin_user_ids: [...(bot.admin_user_ids || [])],
    telegram_token: '', onebot_access_token: '', enabled: bot.enabled !== false, config_profile_id: '', runtime: runtimeForm(bot.runtime_config),
  } : emptyForm())
}

function runtimeTime(value?: string) {
  return value ? new Date(value).toLocaleString() : '—'
}

async function loadRuntime(id = form.id) {
  if (!id) {
    runtimeOverview.value = null
    return
  }
  runtimeLoading.value = true
  try {
    const runtime = await request<RuntimeOverview>(`/api/v1/bots/${encodeURIComponent(id)}/runtime?limit=8`)
    const relationDirectory = await request<{ relations: RuntimeOverview['relations'] }>(`/api/v1/bots/${encodeURIComponent(id)}/runtime/relations?limit=500`)
    runtime.relations = relationDirectory.relations || []
    runtimeOverview.value = runtime
  } catch {
    runtimeOverview.value = null
  } finally {
    runtimeLoading.value = false
  }
}

function relationKey(relation: RuntimeOverview['relations'][number]) {
  return `${relation.scope_type}:${relation.scope_id}`
}

async function saveRelation(relation: RuntimeOverview['relations'][number]) {
  if (!form.id) return
  const key = relationKey(relation)
  relationBusy.value = key
  try {
    const saved = await request<RuntimeOverview['relations'][number]>(
      `/api/v1/bots/${encodeURIComponent(form.id)}/runtime/relations/${encodeURIComponent(relation.scope_type)}/${encodeURIComponent(relation.scope_id)}`,
      {
        method: 'PUT',
        body: JSON.stringify({
          preferred_name: relation.preferred_name || '',
          stable_preferences: relation.stable_preferences || '',
          interaction_style: relation.interaction_style || '',
          trust_level: relation.trust_level || '',
          recent_topics: relation.recent_topics || '',
          commitments: relation.commitments || '',
          last_interaction_at: relation.last_interaction_at,
          revision: relation.revision,
        }),
      },
    )
    Object.assign(relation, saved)
    message.success('关系状态已保存')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存关系状态失败；可能已被其他操作更新')
    await loadRuntime()
  } finally {
    relationBusy.value = null
  }
}

async function deleteRelation(relation: RuntimeOverview['relations'][number]) {
  if (!form.id || !window.confirm(`确认删除关系“${relation.scope_type}:${relation.scope_id}”？`)) return
  const key = relationKey(relation)
  relationBusy.value = key
  try {
    await request<void>(
      `/api/v1/bots/${encodeURIComponent(form.id)}/runtime/relations/${encodeURIComponent(relation.scope_type)}/${encodeURIComponent(relation.scope_id)}?revision=${encodeURIComponent(String(relation.revision))}`,
      { method: 'DELETE' },
    )
    const index = runtimeOverview.value?.relations.indexOf(relation) ?? -1
    if (index >= 0) runtimeOverview.value?.relations.splice(index, 1)
    message.success('关系状态已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除关系状态失败；可能已被其他操作更新')
    await loadRuntime()
  } finally {
    relationBusy.value = null
  }
}

async function selectBot(id: string) {
  selectedID.value = id
  editing.value = true
  fillForm(store.bots.find((item) => item.id === id))
  try {
    const binding = await request<{ profile_id: string }>(`/api/v1/bots/${encodeURIComponent(id)}/config-profile`)
    form.config_profile_id = binding.profile_id || ''
  } catch {
    form.config_profile_id = ''
  }
  await loadRuntime(id)
}

function createBot() {
  selectedID.value = ''
  editing.value = false
  fillForm()
}

function payload() {
  const value: Record<string, unknown> = {
    id: form.id.trim(), name: form.name.trim(), type: form.type, endpoint: form.endpoint.trim(), onebot_mode: form.onebot_mode,
    listen_host: form.listen_host.trim(), listen_port: form.listen_port || 0, listen_path: form.listen_path.trim(), onebot_file_root: form.onebot_file_root.trim(), group_trigger_mode: form.group_trigger_mode,
    admin_user_ids: [...new Set(form.admin_user_ids.map((item) => item.trim()).filter(Boolean))], enabled: form.enabled,
    runtime_config: runtimePayload(form.runtime),
  }
  if (form.telegram_token.trim()) value.telegram_token = form.telegram_token.trim()
  if (form.onebot_access_token.trim()) value.onebot_access_token = form.onebot_access_token.trim()
  return value
}

async function save() {
  saving.value = true
  try {
    const saved = await request<Bot>(`/api/v1/bots${editing.value ? `/${encodeURIComponent(form.id)}` : ''}`, {
      method: editing.value ? 'PUT' : 'POST', body: JSON.stringify(payload()),
    })
    await store.reloadBots()
    selectedID.value = saved.id
    editing.value = true
    await bindProfile(saved.id, true)
    await selectBot(saved.id)
    message.success('机器人配置已保存')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存机器人失败')
  } finally {
    saving.value = false
  }
}

async function bindProfile(id: string, quiet = false) {
  try {
    await request(`/api/v1/bots/${encodeURIComponent(id)}/config-profile`, { method: 'PUT', body: JSON.stringify({ profile_id: form.config_profile_id }) })
    if (!quiet) message.success('机器人默认配置已更新')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '绑定配置失败')
  }
}

async function testCurrent() {
  testing.value = true
  try {
    const result = await request<{ message: string }>(editing.value ? `/api/v1/bots/${encodeURIComponent(form.id)}/test` : '/api/v1/bots/preview/test', {
      method: 'POST', body: JSON.stringify(payload()),
    })
    message.success(result.message || '连接正常')
    if (editing.value) await store.reloadBots()
  } catch (error) {
    message.error(error instanceof Error ? error.message : '连接测试失败')
    if (editing.value) await store.reloadBots()
  } finally {
    testing.value = false
  }
}

async function toggle(bot: Bot) {
  try {
    await request(`/api/v1/bots/${encodeURIComponent(bot.id)}/${bot.status === 'running' || bot.status === 'connecting' ? 'stop' : 'start'}`, { method: 'POST', body: '{}' })
    await store.reloadBots()
    await selectBot(bot.id)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '操作机器人失败')
  }
}

async function restart(bot: Bot) {
  try {
    await request(`/api/v1/bots/${encodeURIComponent(bot.id)}/restart`, { method: 'POST', body: '{}' })
    await store.reloadBots()
    await selectBot(bot.id)
    message.success('机器人已重连')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '重连机器人失败')
  }
}

async function remove(bot: Bot) {
  if (!window.confirm(`确认删除机器人“${bot.name}”？`)) return
  try {
    await request(`/api/v1/bots/${encodeURIComponent(bot.id)}`, { method: 'DELETE' })
    await store.reloadBots()
    selectedID.value = ''
    editing.value = false
    runtimeOverview.value = null
    fillForm()
    message.success('机器人已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除机器人失败')
  }
}

watch(() => form.config_profile_id, (value, oldValue) => {
  if (editing.value && value !== oldValue && form.id) void bindProfile(form.id)
})

onMounted(async () => {
  await store.loadAll()
  if (store.bots.length && !selectedID.value) void selectBot(store.bots[0].id)
})
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">PLATFORM ADAPTERS</p>
        <p>管理 Telegram 和 OneBot 11 连接实例。NapCat 反向 WebSocket 默认连接到这里配置的监听地址。</p>
      </div>
      <NButton type="primary" size="large" @click="createBot"><AppIcon name="plus" :size="14" />创建机器人</NButton>
    </div>

    <div class="bot-workbench">
      <NCard class="catalog-card" :bordered="false">
        <template #header>
          <div class="section-heading-row"><div><h3>连接实例</h3><p>{{ store.bots.length }} 个机器人</p></div><NTag size="small" round :bordered="false">平台</NTag></div>
        </template>
        <NEmpty v-if="!store.bots.length" description="还没有机器人" class="catalog-empty" />
        <div v-else class="bot-list">
          <button v-for="bot in store.bots" :key="bot.id" type="button" class="bot-list-item" :class="{ active: bot.id === selectedID }" @click="selectBot(bot.id)">
            <span class="bot-avatar">◉</span>
            <span class="bot-list-copy"><strong>{{ bot.name }}</strong><small><i :class="['status-dot', statusOf(bot).type]" />{{ statusOf(bot).label }} · {{ bot.type === 'onebot11' ? 'OneBot 11' : 'Telegram' }}</small></span>
            <span class="list-arrow">›</span>
          </button>
        </div>
      </NCard>

      <NCard class="detail-card" :bordered="false">
        <template #header>
          <div class="detail-heading"><div class="detail-avatar">◉</div><div><span class="eyebrow">PLATFORM ADAPTER</span><h3>{{ editing ? (selectedBot?.name || '机器人') : '添加机器人' }}</h3><p>{{ editing && selectedBot ? (selectedBot.status_message || statusOf(selectedBot).label) : '填写连接信息并保存' }}</p></div></div>
        </template>
        <template #header-extra><NButton v-if="editing && selectedBot" quaternary type="error" @click="remove(selectedBot)">删除</NButton></template>

        <div v-if="editing" class="runtime-overview-card">
          <div class="section-heading-row">
            <div><h3>Bot Runtime</h3><p>持久状态、来源积压和最近决策</p></div>
            <NButton quaternary size="small" :loading="runtimeLoading" @click="loadRuntime()"><AppIcon name="refresh" :size="14" />刷新</NButton>
          </div>
          <NEmpty v-if="!runtimeOverview && !runtimeLoading" description="暂无运行状态" />
          <template v-else-if="runtimeOverview">
            <div class="runtime-overview-grid">
              <span><small>生命周期</small><strong>{{ runtimeOverview.instance.lifecycle_state || 'unknown' }}</strong></span>
              <span><small>可用性</small><strong>{{ runtimeOverview.instance.availability || 'unknown' }}</strong></span>
              <span><small>当前负载</small><strong>{{ runtimeOverview.instance.current_load }}</strong></span>
              <span><small>活跃来源</small><strong>{{ runtimeOverview.instance.active_sources }}</strong></span>
            </div>
            <div v-if="runtimeOverview.sources.length" class="runtime-overview-list">
              <strong>来源队列</strong>
              <div v-for="source in runtimeOverview.sources" :key="source.source" class="runtime-overview-row">
                <code>{{ source.source }}</code><NTag size="small" :bordered="false">{{ source.queued_turn_count }} 条待处理</NTag>
              </div>
            </div>
            <div v-if="runtimeOverview.decisions.length" class="runtime-overview-list">
              <strong>最近决策</strong>
              <div v-for="(decision, index) in runtimeOverview.decisions" :key="`${decision.source}-${decision.created_at}-${index}`" class="runtime-overview-row">
                <span>{{ decision.action }} · {{ decision.reason || '—' }}<em v-if="decision.reason_codes?.length"> · {{ decision.reason_codes.join('、') }}</em></span><small>{{ runtimeTime(decision.created_at) }}</small>
              </div>
            </div>
            <div v-if="runtimeOverview.relations.length" class="runtime-overview-list">
              <div class="section-heading-row"><strong>关系状态</strong><small>revision CAS</small></div>
              <div v-for="(relation, index) in runtimeOverview.relations" :key="`${relation.scope_type}-${relation.scope_id}-${index}`" class="runtime-overview-row">
                <div class="runtime-relation-editor">
                  <div class="runtime-relation-meta"><code>{{ relation.scope_type }}:{{ relation.scope_id }}</code><small>修订 {{ relation.revision }}</small></div>
                  <div class="form-grid-3">
                    <NInput v-model:value="relation.preferred_name" size="small" placeholder="称呼" />
                    <NInput v-model:value="relation.interaction_style" size="small" placeholder="互动风格" />
                    <NInput v-model:value="relation.trust_level" size="small" placeholder="信任状态（事实）" />
                    <NInput v-model:value="relation.stable_preferences" size="small" placeholder="稳定偏好" />
                    <NInput v-model:value="relation.recent_topics" size="small" placeholder="近期话题" />
                    <NInput v-model:value="relation.commitments" size="small" placeholder="承诺/待跟进" />
                  </div>
                  <div class="runtime-relation-actions">
                    <small>{{ runtimeTime(relation.last_interaction_at) }}</small>
                    <NSpace size="small"><NButton size="small" type="primary" :loading="relationBusy === relationKey(relation)" @click="saveRelation(relation)">保存</NButton><NButton size="small" type="error" secondary :loading="relationBusy === relationKey(relation)" @click="deleteRelation(relation)">删除</NButton></NSpace>
                  </div>
                </div>
              </div>
            </div>
            <div v-if="runtimeOverview.actions.length" class="runtime-overview-list">
              <strong>最近动作</strong>
              <div v-for="(action, index) in runtimeOverview.actions" :key="`${action.type}-${action.updated_at}-${index}`" class="runtime-overview-row">
                <span>{{ action.type }} · {{ action.status }}</span><small>{{ runtimeTime(action.updated_at) }}</small>
              </div>
            </div>
            <div v-if="runtimeOverview.follow_ups.length" class="runtime-overview-list">
              <strong>后续任务</strong>
              <div v-for="followUp in runtimeOverview.follow_ups" :key="followUp.id" class="runtime-overview-row">
                <span>{{ followUp.name || followUp.request }} · {{ followUp.status }}<em v-if="followUp.running"> · 执行中</em></span>
                <small>{{ followUp.last_error || (followUp.next_run_at ? `下次 ${runtimeTime(followUp.next_run_at)}` : '—') }}</small>
              </div>
            </div>
          </template>
        </div>

        <NForm label-placement="top" :show-feedback="false">
          <div class="form-grid-2">
            <NFormItem label="机器人名称" required><NInput v-model:value="form.name" placeholder="例如 NapCat" /></NFormItem>
            <NFormItem label="平台类型" required><NSelect v-model:value="form.type" :options="[{ label: 'OneBot 11', value: 'onebot11' }, { label: 'Telegram', value: 'telegram' }]" /></NFormItem>
          </div>
          <NFormItem label="启用此机器人"><NSwitch v-model:value="form.enabled" /></NFormItem>
          <template v-if="form.type === 'onebot11'">
            <NFormItem label="连接模式"><NSelect v-model:value="form.onebot_mode" :options="[{ label: '反向 WebSocket 服务端（推荐 NapCat 客户端）', value: 'reverse-server' }, { label: '正向 WebSocket 客户端', value: 'client' }]" /></NFormItem>
            <template v-if="form.onebot_mode === 'reverse-server'">
              <div class="form-grid-3">
                <NFormItem label="监听主机"><NInput v-model:value="form.listen_host" placeholder="0.0.0.0" /></NFormItem>
                <NFormItem label="监听端口"><NInputNumber v-model:value="form.listen_port" :min="1" :max="65535" :show-button="false" /></NFormItem>
                <NFormItem label="监听路径"><NInput v-model:value="form.listen_path" placeholder="/ws" /></NFormItem>
              </div>
              <NAlert type="info" :show-icon="false" class="form-tip">NapCat 的反向 WebSocket 地址应填写：<code>ws://Abot主机局域网IP:端口/路径</code>。两台机器不在同一 Docker 网络时，不要填写容器名。</NAlert>
            </template>
            <NFormItem v-else label="正向 WebSocket 地址"><NInput v-model:value="form.endpoint" placeholder="例如 ws://192.168.1.20:3001" /></NFormItem>
            <NFormItem label="OneBot Access Token"><NInput v-model:value="form.onebot_access_token" type="password" show-password-on="click" :placeholder="editing ? '留空表示保留旧 Token' : '可选'" /></NFormItem>
            <NFormItem label="附件宿主机根目录">
              <NInput v-model:value="form.onebot_file_root" placeholder="例如 /home/amginlily/napcat/ntqq" />
              <span class="form-help">NapCat 容器中的 <code>/app/.config/QQ</code> 对应的宿主机目录；每个机器人独立保存，留空时仅尝试本机约定目录。</span>
            </NFormItem>
          </template>
          <NFormItem v-else label="Telegram Bot Token"><NInput v-model:value="form.telegram_token" type="password" show-password-on="click" :placeholder="editing ? '留空表示保留旧 Token' : '请输入 Bot Token'" /></NFormItem>
          <NFormItem label="群聊触发方式"><NSelect v-model:value="form.group_trigger_mode" :options="[{ label: '仅 @ 机器人时触发（推荐）', value: 'mention' }, { label: '群内所有消息都触发', value: 'all' }]" /></NFormItem>

          <NFormItem label="此 Bot 的全局管理员 ID">
            <NDynamicTags v-model:value="form.admin_user_ids" :closable="true" :input-props="{ placeholder: '输入 UID 后按 Enter 添加' }" />
            <span class="form-help">输入平台消息中的用户 UID；一个一个回车添加，点击标签右侧 × 删除，不需要填写 JSON。OneBot 和 Telegram 的 UID 要分别配置。</span>
          </NFormItem>

          <div class="setting-section">
            <div class="section-heading-row"><div><h3>会话配置</h3><p>机器人创建的对话默认使用此配置；单个对话可以在 chat 页面单独绑定。</p></div></div>
            <NFormItem label="默认配置文件"><NSelect v-model:value="form.config_profile_id" :options="profileOptions" /></NFormItem>
          </div>

          <div class="setting-section runtime-config-section">
            <div class="section-heading-row"><div><h3>Bot Runtime 配置</h3><p>这里的“继承配置”表示继续使用绑定配置文件；保存后会直接作用于这个机器人。</p></div></div>
            <div class="form-grid-2">
              <NFormItem label="Runtime 总开关"><NSelect v-model:value="form.runtime.runtime_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="最大 Agent 并发"><NInputNumber v-model:value="form.runtime.runtime_max_concurrency" :min="1" :max="32" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="每来源队列上限"><NInputNumber v-model:value="form.runtime.source_queue_limit" :min="1" :max="500" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="Runtime 心跳（秒）"><NInputNumber v-model:value="form.runtime.heartbeat_seconds" :min="5" :max="3600" :show-button="false" placeholder="继承" /></NFormItem>
            </div>
            <div class="form-grid-3">
              <NFormItem label="私聊聚合等待（毫秒）"><NInputNumber v-model:value="form.runtime.turn_wait_ms" :min="0" :max="30000" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="群聊聚合等待（毫秒）"><NInputNumber v-model:value="form.runtime.group_turn_wait_ms" :min="0" :max="30000" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="附件补充等待（毫秒）"><NInputNumber v-model:value="form.runtime.attachment_wait_ms" :min="0" :max="60000" :show-button="false" placeholder="继承" /></NFormItem>
            </div>
            <div class="form-grid-2">
              <NFormItem label="私聊行为"><NSelect v-model:value="form.runtime.private_mode" :options="[{ label: '继承配置', value: '' }, { label: '正常响应', value: 'responsive' }, { label: '只记录不回复', value: 'observe_only' }]" /></NFormItem>
              <NFormItem label="群聊参与"><NSelect v-model:value="form.runtime.group_participation_mode" :options="[{ label: '继承配置', value: '' }, { label: '仅被点名时响应', value: 'addressed_only' }, { label: '只观察', value: 'observe_only' }]" /></NFormItem>
              <NFormItem label="记录未唤醒群消息"><NSelect v-model:value="form.runtime.record_unaddressed_messages" :options="booleanOptions" /></NFormItem>
              <NFormItem label="允许表情回应"><NSelect v-model:value="form.runtime.reaction_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="群聊上下文"><NSelect v-model:value="form.runtime.group_context_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="最大聚合消息数"><NInputNumber v-model:value="form.runtime.max_turn_messages" :min="1" :max="100" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="最多注入群消息数"><NInputNumber v-model:value="form.runtime.group_message_max_count" :min="1" :max="300" :show-button="false" placeholder="继承" /></NFormItem>
            </div>
            <div class="form-grid-3">
              <NFormItem label="群图片转述"><NSelect v-model:value="form.runtime.group_image_caption" :options="booleanOptions" /></NFormItem>
              <NFormItem label="群图片转述模型"><NInput v-model:value="form.runtime.group_image_caption_model" placeholder="继承；留空使用主模型" /></NFormItem>
            </div>
            <div class="form-grid-3">
              <NFormItem label="主动行为开关"><NSelect v-model:value="form.runtime.proactive_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="主动参与程度"><NSelect v-model:value="form.runtime.proactive_degree" :options="[{ label: '继承配置', value: '' }, { label: '关闭', value: 'off' }, { label: '低', value: 'low' }, { label: '正常', value: 'normal' }, { label: '高', value: 'high' }]" /></NFormItem>
              <NFormItem label="Follow-up 开关"><NSelect v-model:value="form.runtime.follow_up_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="Follow-up 上限"><NInputNumber v-model:value="form.runtime.follow_up_max" :min="1" :max="10000" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="最大重试次数"><NInputNumber v-model:value="form.runtime.follow_up_max_retries" :min="0" :max="10" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="首次重试等待（秒）"><NInputNumber v-model:value="form.runtime.follow_up_retry_delay_seconds" :min="0" :max="86400" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="最大重试延迟（秒）"><NInputNumber v-model:value="form.runtime.follow_up_max_delay_seconds" :min="0" :max="604800" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="回复冷却（秒）"><NInputNumber v-model:value="form.runtime.cooldown_seconds" :min="0" :max="86400" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="私聊每小时上限"><NInputNumber v-model:value="form.runtime.private_hourly_reply_limit" :min="0" :max="10000" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="群聊每小时上限"><NInputNumber v-model:value="form.runtime.group_hourly_reply_limit" :min="0" :max="10000" :show-button="false" placeholder="继承" /></NFormItem>
            </div>
            <NFormItem label="允许主动联系的来源（UMO）"><NDynamicTags v-model:value="form.runtime.follow_up_allowed_sources" :closable="true" :input-props="{ placeholder: '输入 UMO 后按 Enter 添加' }" /></NFormItem>
            <div class="form-grid-3">
              <NFormItem label="关系状态"><NSelect v-model:value="form.runtime.relation_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="关系保留（秒）"><NInputNumber v-model:value="form.runtime.relation_retention_seconds" :min="86400" :max="31536000" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="只读工具预算"><NInputNumber v-model:value="form.runtime.tool_budget" :min="0" :max="100" :show-button="false" placeholder="继承" /></NFormItem>
            </div>
            <NFormItem label="允许的只读工具"><NDynamicTags v-model:value="form.runtime.allowed_read_only_tools" :closable="true" :input-props="{ placeholder: '输入工具名后按 Enter 添加' }" /></NFormItem>
            <NFormItem label="禁用的 Runtime 动作"><NSelect v-model:value="form.runtime.disabled_actions" multiple clearable :options="runtimeActionOptions" placeholder="留空表示按平台能力和默认权限执行" /></NFormItem>
            <div class="form-grid-2">
              <NFormItem label="按需调用 Agent"><NSelect v-model:value="form.runtime.agent_on_demand_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="主 Agent 启用子 Agent"><NSelect v-model:value="form.runtime.subagent_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="聊天表达层"><NSelect v-model:value="form.runtime.expression_enabled" :options="booleanOptions" /></NFormItem>
              <NFormItem label="最大语义段数"><NInputNumber v-model:value="form.runtime.expression_max_segments" :min="1" :max="8" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="长文阈值（字符）"><NInputNumber v-model:value="form.runtime.expression_long_threshold" :min="100" :max="10000" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="段间隔（毫秒）"><NInputNumber v-model:value="form.runtime.expression_delay_ms" :min="0" :max="5000" :show-button="false" placeholder="继承" /></NFormItem>
              <NFormItem label="消息表达风格"><NSelect v-model:value="form.runtime.message_style" :options="[{ label: '继承配置', value: '' }, { label: '自然聊天', value: 'natural' }, { label: '结构化说明', value: 'structured' }]" /></NFormItem>
            </div>
            <div class="form-grid-3">
              <NFormItem label="群聊引用"><NSelect v-model:value="form.runtime.reply_quote" :options="booleanOptions" /></NFormItem>
              <NFormItem label="群聊 @ 用户"><NSelect v-model:value="form.runtime.reply_mention" :options="booleanOptions" /></NFormItem>
              <NFormItem label="私聊默认引用"><NSelect v-model:value="form.runtime.private_reply_quote" :options="booleanOptions" /></NFormItem>
            </div>
            <div class="form-grid-3">
              <NFormItem label="安静时区"><NInput v-model:value="form.runtime.quiet_hours_timezone" placeholder="继承，例如 Asia/Shanghai" /></NFormItem>
              <NFormItem label="安静开始"><NInput v-model:value="form.runtime.quiet_hours_start" placeholder="例如 23:00" /></NFormItem>
              <NFormItem label="安静结束"><NInput v-model:value="form.runtime.quiet_hours_end" placeholder="例如 08:00" /></NFormItem>
            </div>
            <NFormItem label="紧急 Follow-up 绕过安静时段"><NSelect v-model:value="form.runtime.emergency_bypass_quiet_hours" :options="booleanOptions" /></NFormItem>
          </div>
          <NFormItem v-if="!editing" label="Adapter ID" required><NInput v-model:value="form.id" placeholder="例如 napcat" /></NFormItem>
          <div class="form-actions-row">
            <NSpace><NButton secondary :loading="testing" @click="testCurrent">测试连接</NButton><NButton v-if="editing && selectedBot" secondary @click="restart(selectedBot)">重连</NButton><NButton v-if="editing && selectedBot" secondary @click="toggle(selectedBot)">{{ selectedBot.status === 'running' || selectedBot.status === 'connecting' ? '停止' : '启动' }}</NButton></NSpace>
            <NButton type="primary" :loading="saving" @click="save">保存配置</NButton>
          </div>
        </NForm>
      </NCard>
    </div>
  </div>
</template>
