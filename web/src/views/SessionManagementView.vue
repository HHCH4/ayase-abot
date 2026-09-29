<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NButton, NCard, NEmpty, NForm, NFormItem, NInput, NInputNumber, NModal, NSelect, NSpace, NTag, useMessage } from 'naive-ui'
import { batchSessionRules, createSessionRule, deleteSessionRule, deleteSessionRuleGroup, readPersonas, readSessionRuleGroups, readSessionRules, readSessionSources, resetSessionRuleField, saveSessionRule, saveSessionRuleGroup } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import { useAppStore } from '@/stores/app'
import type { Persona, SessionRule, SessionRuleGroup, SessionSource } from '@/types'

const store = useAppStore()
const message = useMessage()
const rules = ref<SessionRule[]>([])
const sources = ref<SessionSource[]>([])
const groups = ref<SessionRuleGroup[]>([])
const personas = ref<Persona[]>([])
const selectedSources = ref<string[]>([])
const loading = ref(false)
const saving = ref(false)
const showEditor = ref(false)
const showGroupEditor = ref(false)
const editingSource = ref('')
const editingGroupID = ref('')
const batchScope = ref('selected')
const batchGroupID = ref('')
const batchLLM = ref('keep')
const batchProcess = ref('keep')
const batchTTS = ref('keep')
const batchModel = ref('')
const sourceQuery = ref('')
const resetFieldKey = ref('')
type BooleanOverride = 'inherit' | 'true' | 'false'

const booleanOverrideOptions = [
  { label: '继承全局', value: 'inherit' },
  { label: '启用', value: 'true' },
  { label: '停用', value: 'false' },
]

const runtimeActionOptions = [
  { label: '发送文本', value: 'send_text' },
  { label: '发送图片', value: 'send_image' },
  { label: '发送音频', value: 'send_audio' },
  { label: '发送文件', value: 'send_file' },
  { label: '表情回应', value: 'add_reaction' },
  { label: '戳一戳', value: 'poke' },
  { label: '撤回消息', value: 'recall_message' },
  { label: '调用 Agent', value: 'start_agent' },
  { label: '创建 Follow-up', value: 'create_follow_up' },
  { label: '更新关系', value: 'update_relation' },
  { label: '更新来源状态', value: 'update_source_state' },
]

const form = reactive({
  source: '', process_enabled: 'inherit' as BooleanOverride, llm_enabled: 'inherit' as BooleanOverride, tts_enabled: 'inherit' as BooleanOverride, note: '', chat_model: '', stt_model: '', tts_model: '',
  follow_profile: 'inherit' as BooleanOverride, profile_id: '', persona_id: '', disabled_plugins: '', knowledge_bases: '', knowledge_top_k: null as number | null, knowledge_rerank: 'inherit' as BooleanOverride,
  private_mode: '', group_participation_mode: '', record_unaddressed_messages: 'inherit' as BooleanOverride, reaction_enabled: 'inherit' as BooleanOverride, group_context_enabled: 'inherit' as BooleanOverride,
  proactive_enabled: 'inherit' as BooleanOverride, private_hourly_reply_limit: null as number | null, group_hourly_reply_limit: null as number | null, quiet_hours_timezone: '', quiet_hours_start: '', quiet_hours_end: '',
  emergency_bypass_quiet_hours: 'inherit' as BooleanOverride, reply_quote: 'inherit' as BooleanOverride, private_reply_quote: 'inherit' as BooleanOverride, follow_up_enabled: 'inherit' as BooleanOverride, relation_enabled: 'inherit' as BooleanOverride,
  runtime_enabled: 'inherit' as BooleanOverride, runtime_max_concurrency: null as number | null, source_queue_limit: null as number | null, turn_wait_ms: null as number | null,
  group_turn_wait_ms: null as number | null, attachment_wait_ms: null as number | null, max_turn_messages: null as number | null, group_message_max_count: null as number | null,
  group_image_caption: 'inherit' as BooleanOverride, group_image_caption_model: '', proactive_degree: '', cooldown_seconds: null as number | null, heartbeat_seconds: null as number | null,
  relation_retention_seconds: null as number | null, follow_up_max: null as number | null, follow_up_max_retries: null as number | null, follow_up_retry_delay_seconds: null as number | null,
  follow_up_max_delay_seconds: null as number | null, follow_up_allowed_sources: '', expression_enabled: 'inherit' as BooleanOverride, expression_max_segments: null as number | null,
  expression_long_threshold: null as number | null, expression_delay_ms: null as number | null, reply_mention: 'inherit' as BooleanOverride,
  agent_on_demand_enabled: 'inherit' as BooleanOverride, allowed_read_only_tools: '', tool_budget: null as number | null, subagent_enabled: 'inherit' as BooleanOverride,
  message_style: '', disabled_actions: [] as string[],
})
const groupForm = reactive({ name: '', description: '', members: '' })

const profileOptions = computed(() => store.configProfiles.map((item) => ({ label: `${item.name}${item.is_default ? ' · 默认' : ''}`, value: item.id })))
const personaOptions = computed(() => personas.value.filter((item) => item.enabled).map((item) => ({ label: item.name, value: item.id })))
const modelOptions = computed(() => {
  const values: { label: string; value: string }[] = []
  for (const provider of store.providers) {
    for (const model of provider.models || []) {
      if (model.enabled === false) continue
      values.push({ label: `${provider.name} / ${model.display_name || model.id}`, value: model.id })
    }
  }
  // 旧规则可能仍引用已移出目录的模型，保留当前值显示，保存时不强迫用户重新手写。
  for (const current of [form.chat_model, form.stt_model, form.tts_model, batchModel.value]) {
    if (current.trim() && !values.some((item) => item.value === current.trim())) values.push({ label: `当前值：${current.trim()}（目录外）`, value: current.trim() })
  }
  return values
})
const selectedCount = computed(() => selectedSources.value.length)
const sourceOptions = computed(() => {
  const options = new Map<string, { label: string; value: string }>()
  // 优先展示来源目录中的全部 UMO，让用户直接从已知会话选择。
  for (const item of sources.value) {
    const detail = [item.platform, messageTypeLabel(item.message_type), item.session_id].filter(Boolean).join(' · ')
    const name = item.source_name?.trim() || item.auto_name?.trim()
    options.set(item.source, { label: name ? `${name} · ${item.source}${detail ? `（${detail}）` : ''}` : `${item.source}${detail ? `（${detail}）` : ''}`, value: item.source })
  }
  // 保留没有最近消息但已经存在规则的来源，避免编辑旧规则时选项消失。
  for (const item of rules.value) {
    if (!options.has(item.source)) options.set(item.source, { label: `${item.source}（已有规则）`, value: item.source })
  }
  return [...options.values()]
})

const sourceRule = (source: string) => rules.value.find((item) => item.source === source)
const resetFieldOptions = [
  { label: '处理消息', value: 'process_enabled' },
  { label: '内置 AI', value: 'llm_enabled' },
  { label: 'TTS', value: 'tts_enabled' },
  { label: '聊天模型', value: 'chat_model' },
  { label: 'STT 模型', value: 'stt_model' },
  { label: 'TTS 模型', value: 'tts_model' },
  { label: '配置文件跟随', value: 'follow_profile' },
  { label: '配置文件', value: 'profile_id' },
  { label: '人格', value: 'persona_id' },
  { label: '停用插件记录', value: 'disabled_plugins' },
  { label: '知识库', value: 'knowledge_bases' },
  { label: '知识库 Top K', value: 'knowledge_top_k' },
  { label: '知识库重排', value: 'knowledge_rerank' },
  { label: '私聊行为', value: 'private_mode' },
  { label: '群聊参与', value: 'group_participation_mode' },
  { label: '记录未唤醒消息', value: 'record_unaddressed_messages' },
  { label: '允许表情回应', value: 'reaction_enabled' },
  { label: '群聊上下文', value: 'group_context_enabled' },
  { label: '主动参与', value: 'proactive_enabled' },
  { label: '私聊每小时主动上限', value: 'private_hourly_reply_limit' },
  { label: '群聊每小时主动上限', value: 'group_hourly_reply_limit' },
  { label: '安静时区', value: 'quiet_hours_timezone' },
  { label: '安静开始', value: 'quiet_hours_start' },
  { label: '安静结束', value: 'quiet_hours_end' },
  { label: '紧急提醒绕过安静时段', value: 'emergency_bypass_quiet_hours' },
  { label: '群聊回复引用', value: 'reply_quote' },
  { label: '私聊回复引用', value: 'private_reply_quote' },
  { label: 'Follow-up', value: 'follow_up_enabled' },
  { label: '关系状态', value: 'relation_enabled' },
  { label: 'Runtime 开关', value: 'runtime_enabled' },
  { label: 'Runtime 并发', value: 'runtime_max_concurrency' },
  { label: '来源队列上限', value: 'source_queue_limit' },
  { label: '私聊聚合等待', value: 'turn_wait_ms' },
  { label: '群聊聚合等待', value: 'group_turn_wait_ms' },
  { label: '附件等待', value: 'attachment_wait_ms' },
  { label: '最大聚合消息数', value: 'max_turn_messages' },
  { label: '群聊上下文条数', value: 'group_message_max_count' },
  { label: '群图片转述', value: 'group_image_caption' },
  { label: '群图片转述模型', value: 'group_image_caption_model' },
  { label: '主动程度', value: 'proactive_degree' },
  { label: '主动冷却', value: 'cooldown_seconds' },
  { label: '心跳周期', value: 'heartbeat_seconds' },
  { label: '关系保留时间', value: 'relation_retention_seconds' },
  { label: 'Follow-up 上限', value: 'follow_up_max' },
  { label: 'Follow-up 最大重试', value: 'follow_up_max_retries' },
  { label: 'Follow-up 重试等待', value: 'follow_up_retry_delay_seconds' },
  { label: 'Follow-up 最大延迟', value: 'follow_up_max_delay_seconds' },
  { label: 'Follow-up 来源白名单', value: 'follow_up_allowed_sources' },
  { label: '聊天表达开关', value: 'expression_enabled' },
  { label: '最大表达段数', value: 'expression_max_segments' },
  { label: '长文阈值', value: 'expression_long_threshold' },
  { label: '表达间隔', value: 'expression_delay_ms' },
  { label: '回复 @', value: 'reply_mention' },
  { label: '按需调用 Agent', value: 'agent_on_demand_enabled' },
  { label: '只读工具', value: 'allowed_read_only_tools' },
  { label: '工具预算', value: 'tool_budget' },
  { label: '子 Agent', value: 'subagent_enabled' },
  { label: 'Runtime 动作权限', value: 'action_permissions' },
  { label: '消息表达风格', value: 'message_style' },
  { label: '备注', value: 'note' },
]

function hasOverride(item: SessionRule | undefined, key: string) {
  return Boolean(item?.configured_fields?.includes(key))
}

function booleanOverride(item: SessionRule | undefined, key: string, value?: boolean): BooleanOverride {
  if (!item || !hasOverride(item, key)) return 'inherit'
  return value ? 'true' : 'false'
}

function numberOverride(item: SessionRule | undefined, key: string, value?: number) {
  if (!item || !hasOverride(item, key) || value === undefined) return null
  return value
}

function sourceLabel(item: SessionSource) {
  return item.source_name?.trim() || item.auto_name?.trim() || item.source
}

function messageTypeLabel(value?: string) {
  if (value === 'FriendMessage') return '私聊'
  if (value === 'GroupMessage') return '群聊'
  if (value) return '其他会话'
  return '未知类型'
}

function sourceStatusLabel(status: string) {
  if (status === 'active') return '活跃'
  if (status === 'archived') return '已归档'
  return status || '未知'
}

function sourceStatusType(status: string) {
  return status === 'active' ? 'success' : 'warning'
}

function formatSourceTime(value?: string) {
  return value ? new Date(value).toLocaleString() : '—'
}

// 读取规则、分组和人格目录，编辑页只使用已存在的配置实体。
async function load() {
  loading.value = true
  try {
    await store.loadAll()
    const [ruleItems, groupItems, personaResult, sourceItems] = await Promise.all([readSessionRules(), readSessionRuleGroups(), readPersonas(), readSessionSources(sourceQuery.value)])
    rules.value = ruleItems
    groups.value = groupItems
    personas.value = personaResult.personas || []
    sources.value = sourceItems
    const known = new Set([...rules.value.map((item) => item.source), ...sources.value.map((item) => item.source)])
    selectedSources.value = selectedSources.value.filter((source) => known.has(source))
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取会话规则失败')
  } finally {
    loading.value = false
  }
}

function resetForm(item?: SessionRule) {
  editingSource.value = item?.source || ''
  form.source = item?.source || ''
  form.process_enabled = booleanOverride(item, 'process_enabled', item?.process_enabled)
  form.llm_enabled = booleanOverride(item, 'llm_enabled', item?.llm_enabled)
  form.tts_enabled = booleanOverride(item, 'tts_enabled', item?.tts_enabled)
  form.note = item && hasOverride(item, 'note') ? item.note || '' : ''
  form.chat_model = item && hasOverride(item, 'chat_model') ? item.chat_model || '' : ''
  form.stt_model = item && hasOverride(item, 'stt_model') ? item.stt_model || '' : ''
  form.tts_model = item && hasOverride(item, 'tts_model') ? item.tts_model || '' : ''
  form.follow_profile = booleanOverride(item, 'follow_profile', item?.follow_profile)
  form.profile_id = item && hasOverride(item, 'profile_id') ? item.profile_id || '' : ''
  form.persona_id = item && hasOverride(item, 'persona_id') ? item.persona_id || '' : ''
  form.disabled_plugins = item && hasOverride(item, 'disabled_plugins') ? (item.disabled_plugins || []).join(', ') : ''
  form.knowledge_bases = item && hasOverride(item, 'knowledge_bases') ? (item.knowledge_bases || []).join(', ') : ''
  form.knowledge_top_k = numberOverride(item, 'knowledge_top_k', item?.knowledge_top_k)
  form.knowledge_rerank = booleanOverride(item, 'knowledge_rerank', item?.knowledge_rerank)
  form.private_mode = item && hasOverride(item, 'private_mode') ? item.private_mode || '' : ''
  form.group_participation_mode = item && hasOverride(item, 'group_participation_mode') ? item.group_participation_mode || '' : ''
  form.record_unaddressed_messages = booleanOverride(item, 'record_unaddressed_messages', item?.record_unaddressed_messages)
  form.reaction_enabled = booleanOverride(item, 'reaction_enabled', item?.reaction_enabled)
  form.group_context_enabled = booleanOverride(item, 'group_context_enabled', item?.group_context_enabled)
  form.proactive_enabled = booleanOverride(item, 'proactive_enabled', item?.proactive_enabled)
  form.private_hourly_reply_limit = numberOverride(item, 'private_hourly_reply_limit', item?.private_hourly_reply_limit)
  form.group_hourly_reply_limit = numberOverride(item, 'group_hourly_reply_limit', item?.group_hourly_reply_limit)
  form.quiet_hours_timezone = item && hasOverride(item, 'quiet_hours_timezone') ? item.quiet_hours_timezone || '' : ''
  form.quiet_hours_start = item && hasOverride(item, 'quiet_hours_start') ? item.quiet_hours_start || '' : ''
  form.quiet_hours_end = item && hasOverride(item, 'quiet_hours_end') ? item.quiet_hours_end || '' : ''
  form.emergency_bypass_quiet_hours = booleanOverride(item, 'emergency_bypass_quiet_hours', item?.emergency_bypass_quiet_hours)
  form.reply_quote = booleanOverride(item, 'reply_quote', item?.reply_quote)
  form.private_reply_quote = booleanOverride(item, 'private_reply_quote', item?.private_reply_quote)
  form.follow_up_enabled = booleanOverride(item, 'follow_up_enabled', item?.follow_up_enabled)
  form.relation_enabled = booleanOverride(item, 'relation_enabled', item?.relation_enabled)
  form.runtime_enabled = booleanOverride(item, 'runtime_enabled', item?.runtime_enabled)
  form.runtime_max_concurrency = numberOverride(item, 'runtime_max_concurrency', item?.runtime_max_concurrency)
  form.source_queue_limit = numberOverride(item, 'source_queue_limit', item?.source_queue_limit)
  form.turn_wait_ms = numberOverride(item, 'turn_wait_ms', item?.turn_wait_ms)
  form.group_turn_wait_ms = numberOverride(item, 'group_turn_wait_ms', item?.group_turn_wait_ms)
  form.attachment_wait_ms = numberOverride(item, 'attachment_wait_ms', item?.attachment_wait_ms)
  form.max_turn_messages = numberOverride(item, 'max_turn_messages', item?.max_turn_messages)
  form.group_message_max_count = numberOverride(item, 'group_message_max_count', item?.group_message_max_count)
  form.group_image_caption = booleanOverride(item, 'group_image_caption', item?.group_image_caption)
  form.group_image_caption_model = item && hasOverride(item, 'group_image_caption_model') ? item.group_image_caption_model || '' : ''
  form.proactive_degree = item && hasOverride(item, 'proactive_degree') ? item.proactive_degree || '' : ''
  form.cooldown_seconds = numberOverride(item, 'cooldown_seconds', item?.cooldown_seconds)
  form.heartbeat_seconds = numberOverride(item, 'heartbeat_seconds', item?.heartbeat_seconds)
  form.relation_retention_seconds = numberOverride(item, 'relation_retention_seconds', item?.relation_retention_seconds)
  form.follow_up_max = numberOverride(item, 'follow_up_max', item?.follow_up_max)
  form.follow_up_max_retries = numberOverride(item, 'follow_up_max_retries', item?.follow_up_max_retries)
  form.follow_up_retry_delay_seconds = numberOverride(item, 'follow_up_retry_delay_seconds', item?.follow_up_retry_delay_seconds)
  form.follow_up_max_delay_seconds = numberOverride(item, 'follow_up_max_delay_seconds', item?.follow_up_max_delay_seconds)
  form.follow_up_allowed_sources = item && hasOverride(item, 'follow_up_allowed_sources') ? (item.follow_up_allowed_sources || []).join(', ') : ''
  form.expression_enabled = booleanOverride(item, 'expression_enabled', item?.expression_enabled)
  form.expression_max_segments = numberOverride(item, 'expression_max_segments', item?.expression_max_segments)
  form.expression_long_threshold = numberOverride(item, 'expression_long_threshold', item?.expression_long_threshold)
  form.expression_delay_ms = numberOverride(item, 'expression_delay_ms', item?.expression_delay_ms)
  form.reply_mention = booleanOverride(item, 'reply_mention', item?.reply_mention)
  form.agent_on_demand_enabled = booleanOverride(item, 'agent_on_demand_enabled', item?.agent_on_demand_enabled)
  form.allowed_read_only_tools = item && hasOverride(item, 'allowed_read_only_tools') ? (item.allowed_read_only_tools || []).join(', ') : ''
  form.tool_budget = numberOverride(item, 'tool_budget', item?.tool_budget)
  form.subagent_enabled = booleanOverride(item, 'subagent_enabled', item?.subagent_enabled)
  form.message_style = item && hasOverride(item, 'message_style') ? item.message_style || '' : ''
  form.disabled_actions = item && hasOverride(item, 'action_permissions') ? Object.entries(item.action_permissions || {}).filter(([, enabled]) => enabled === false).map(([key]) => key) : []
  resetFieldKey.value = ''
}

function openCreate(source = '') {
  if (!sourceOptions.value.length) {
    message.info('还没有可配置的消息会话来源，请先让机器人收到一条消息')
    return
  }
  resetForm()
  form.source = source
  showEditor.value = true
}

function openSource(item: SessionSource) {
  const rule = sourceRule(item.source)
  if (rule) openEdit(rule)
  else openCreate(item.source)
}

function openEdit(item: SessionRule) {
  resetForm(item)
  showEditor.value = true
}

function splitValues(value: string) {
  return value.split(/[,\n]/).map((item) => item.trim()).filter(Boolean)
}

async function save() {
  if (!form.source.trim()) {
    message.warning('消息会话来源不能为空')
    return
  }
  saving.value = true
  try {
    // 表单只提交明确的覆盖项；“继承全局”不能序列化成默认值，否则会把全局和机器人的配置误改掉。
    const payload: Record<string, unknown> & { source: string } = { source: form.source.trim() }
    const setBooleanOverride = (key: string, value: BooleanOverride) => {
      if (value !== 'inherit') payload[key] = value === 'true'
    }
    const setTextOverride = (key: string, value: string) => {
      const text = value.trim()
      if (text) payload[key] = text
    }
    setBooleanOverride('process_enabled', form.process_enabled)
    setBooleanOverride('llm_enabled', form.llm_enabled)
    setBooleanOverride('tts_enabled', form.tts_enabled)
    setTextOverride('note', form.note)
    setTextOverride('chat_model', form.chat_model)
    setTextOverride('stt_model', form.stt_model)
    setTextOverride('tts_model', form.tts_model)
    setBooleanOverride('follow_profile', form.follow_profile)
    setTextOverride('profile_id', form.profile_id)
    setTextOverride('persona_id', form.persona_id)
    if (form.disabled_plugins.trim()) payload.disabled_plugins = splitValues(form.disabled_plugins)
    if (form.knowledge_bases.trim()) payload.knowledge_bases = splitValues(form.knowledge_bases)
    if (form.knowledge_top_k !== null) payload.knowledge_top_k = form.knowledge_top_k
    setBooleanOverride('knowledge_rerank', form.knowledge_rerank)
    if (form.private_mode) payload.private_mode = form.private_mode
    if (form.group_participation_mode) payload.group_participation_mode = form.group_participation_mode
    setBooleanOverride('record_unaddressed_messages', form.record_unaddressed_messages)
    setBooleanOverride('reaction_enabled', form.reaction_enabled)
    setBooleanOverride('group_context_enabled', form.group_context_enabled)
    setBooleanOverride('proactive_enabled', form.proactive_enabled)
    if (form.private_hourly_reply_limit !== null) payload.private_hourly_reply_limit = form.private_hourly_reply_limit
    if (form.group_hourly_reply_limit !== null) payload.group_hourly_reply_limit = form.group_hourly_reply_limit
    setTextOverride('quiet_hours_timezone', form.quiet_hours_timezone)
    setTextOverride('quiet_hours_start', form.quiet_hours_start)
    setTextOverride('quiet_hours_end', form.quiet_hours_end)
    setBooleanOverride('emergency_bypass_quiet_hours', form.emergency_bypass_quiet_hours)
    setBooleanOverride('reply_quote', form.reply_quote)
    setBooleanOverride('private_reply_quote', form.private_reply_quote)
    setBooleanOverride('follow_up_enabled', form.follow_up_enabled)
    setBooleanOverride('relation_enabled', form.relation_enabled)
    setBooleanOverride('runtime_enabled', form.runtime_enabled)
    const setNumber = (key: string, value: number | null) => { if (value !== null) payload[key] = value }
    setNumber('runtime_max_concurrency', form.runtime_max_concurrency)
    setNumber('source_queue_limit', form.source_queue_limit)
    setNumber('turn_wait_ms', form.turn_wait_ms)
    setNumber('group_turn_wait_ms', form.group_turn_wait_ms)
    setNumber('attachment_wait_ms', form.attachment_wait_ms)
    setNumber('max_turn_messages', form.max_turn_messages)
    setNumber('group_message_max_count', form.group_message_max_count)
    setBooleanOverride('group_image_caption', form.group_image_caption)
    setTextOverride('group_image_caption_model', form.group_image_caption_model)
    setTextOverride('proactive_degree', form.proactive_degree)
    setNumber('cooldown_seconds', form.cooldown_seconds)
    setNumber('heartbeat_seconds', form.heartbeat_seconds)
    setNumber('relation_retention_seconds', form.relation_retention_seconds)
    setNumber('follow_up_max', form.follow_up_max)
    setNumber('follow_up_max_retries', form.follow_up_max_retries)
    setNumber('follow_up_retry_delay_seconds', form.follow_up_retry_delay_seconds)
    setNumber('follow_up_max_delay_seconds', form.follow_up_max_delay_seconds)
    if (form.follow_up_allowed_sources.trim()) payload.follow_up_allowed_sources = splitValues(form.follow_up_allowed_sources)
    setBooleanOverride('expression_enabled', form.expression_enabled)
    setNumber('expression_max_segments', form.expression_max_segments)
    setNumber('expression_long_threshold', form.expression_long_threshold)
    setNumber('expression_delay_ms', form.expression_delay_ms)
    setBooleanOverride('reply_mention', form.reply_mention)
    setBooleanOverride('agent_on_demand_enabled', form.agent_on_demand_enabled)
    if (form.allowed_read_only_tools.trim()) payload.allowed_read_only_tools = splitValues(form.allowed_read_only_tools)
    setNumber('tool_budget', form.tool_budget)
    setBooleanOverride('subagent_enabled', form.subagent_enabled)
    setTextOverride('message_style', form.message_style)
    if (form.disabled_actions.length) payload.action_permissions = Object.fromEntries(form.disabled_actions.map((key) => [key, false]))
    const requestPayload = payload as Partial<SessionRule> & { source: string }
    if (editingSource.value) await saveSessionRule(requestPayload)
    else await createSessionRule(requestPayload)
    showEditor.value = false
    await load()
    message.success(editingSource.value ? '会话规则已更新' : '会话规则已创建')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存会话规则失败')
  } finally {
    saving.value = false
  }
}

// 按项清除会话覆盖，恢复继承全局配置；这和删除整条规则是两个不同操作。
async function resetOverride() {
  if (!editingSource.value || !resetFieldKey.value) {
    message.warning('请先选择要清除的规则项')
    return
  }
  try {
    await resetSessionRuleField(editingSource.value, resetFieldKey.value)
    await load()
    const updated = sourceRule(editingSource.value)
    if (updated) {
      resetForm(updated)
    } else {
      showEditor.value = false
    }
    message.success('该规则项已恢复继承全局配置')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '清除规则项失败')
  }
}

async function remove(item: SessionRule) {
  if (!window.confirm(`确认删除来源“${item.source}”的自定义规则？`)) return
  try {
    await deleteSessionRule(item.source)
    await load()
    message.success('会话规则已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除会话规则失败')
  }
}

async function applyBatch() {
  let sources = [...selectedSources.value]
  let scope = batchScope.value
  if (scope === 'group') {
    const group = groups.value.find((item) => item.id === batchGroupID.value)
    if (!group) {
      message.warning('请选择一个会话分组')
      return
    }
    scope = 'selected'
    sources = group.members
  }
  if (scope === 'selected' && !sources.length) {
    message.warning('请先选择至少一个会话来源')
    return
  }
  try {
    const result = await batchSessionRules({
      scope, sources,
      ...(batchLLM.value === 'keep' ? {} : { llm_enabled: batchLLM.value === 'enable' }),
      ...(batchProcess.value === 'keep' ? {} : { process_enabled: batchProcess.value === 'enable' }),
      ...(batchTTS.value === 'keep' ? {} : { tts_enabled: batchTTS.value === 'enable' }),
      ...(batchModel.value.trim() ? { chat_model: batchModel.value.trim() } : {}),
    })
    await load()
    message.success(`已更新 ${result.length} 条规则`)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '批量更新规则失败')
  }
}

function resetGroup(item?: SessionRuleGroup) {
  editingGroupID.value = item?.id || ''
  groupForm.name = item?.name || ''
  groupForm.description = item?.description || ''
  groupForm.members = (item?.members || []).join('\n')
}

function openGroupCreate() {
  resetGroup()
  showGroupEditor.value = true
}

function openGroupEdit(item: SessionRuleGroup) {
  resetGroup(item)
  showGroupEditor.value = true
}

async function saveGroup() {
  if (!groupForm.name.trim()) {
    message.warning('分组名称不能为空')
    return
  }
  try {
    await saveSessionRuleGroup({ id: editingGroupID.value || undefined, name: groupForm.name.trim(), description: groupForm.description.trim(), members: splitValues(groupForm.members) })
    showGroupEditor.value = false
    await load()
    message.success(editingGroupID.value ? '分组已更新' : '分组已创建')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存会话分组失败')
  }
}

async function removeGroup(item: SessionRuleGroup) {
  if (!window.confirm(`确认删除分组“${item.name}”？规则本身不会被删除。`)) return
  try {
    await deleteSessionRuleGroup(item.id)
    await load()
    message.success('会话分组已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除会话分组失败')
  }
}
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">SESSION RULES</p>
        <h2>自定义规则</h2>
        <p>来源从已经产生过消息的会话中选择，按 UMO 覆盖处理、内置 AI、模型、人格和知识库选项；所有规则都在本地内置 Agent 边界内执行。</p>
      </div>
      <NSpace><NButton secondary :loading="loading" @click="load">刷新</NButton><NButton type="primary" @click="openCreate()"><AppIcon name="plus" :size="14" />新建规则</NButton></NSpace>
    </div>

    <NCard class="detail-card" :bordered="false">
      <div class="section-heading-row"><div><h3>可配置会话</h3><p>来源目录记录所有收到过消息的 UMO，即使消息没有唤醒 AI 或只执行了内置指令，也可以直接配置。</p></div><NSpace><NInput v-model:value="sourceQuery" clearable placeholder="搜索名称、平台、Session ID" style="width: 250px" @keyup.enter="load" /><NButton secondary :loading="loading" @click="load">刷新会话</NButton></NSpace></div>
      <div v-if="sources.length" class="data-table-wrap"><table class="data-table"><thead><tr><th class="check-column">选</th><th>会话来源</th><th>平台 / 类型</th><th>Session ID</th><th>状态</th><th>规则</th><th>最近活动</th><th>操作</th></tr></thead><tbody><tr v-for="item in sources" :key="item.source"><td class="check-column"><input v-model="selectedSources" type="checkbox" :value="item.source"></td><td><strong>{{ sourceLabel(item) }}</strong><code>{{ item.source }}</code><code v-if="item.source_name && item.auto_name">自动名称：{{ item.auto_name }}</code></td><td><span>{{ item.platform || '—' }}</span><NTag size="small" :bordered="false" :type="item.message_type === 'GroupMessage' ? 'info' : item.message_type === 'FriendMessage' ? 'success' : 'default'">{{ messageTypeLabel(item.message_type) }}</NTag></td><td><code>{{ item.session_id || '—' }}</code></td><td><NTag size="small" :bordered="false" :type="sourceStatusType(item.status)">{{ sourceStatusLabel(item.status) }}</NTag></td><td><NTag v-if="sourceRule(item.source) || item.has_rule" size="small" :bordered="false" type="info">已配置</NTag><span v-else class="muted">未配置</span></td><td>{{ formatSourceTime(item.last_seen_at || item.updated_at) }}</td><td><NButton size="small" secondary @click="openSource(item)">{{ sourceRule(item.source) ? '编辑规则' : '配置规则' }}</NButton></td></tr></tbody></table></div>
      <NEmpty v-else description="还没有可配置的会话；请先让机器人收到一条平台消息，再点击刷新" />
    </NCard>

    <NCard class="detail-card" :bordered="false">
      <div class="section-heading-row"><div><h3>会话来源规则</h3><p>已选 {{ selectedCount }} 条；可用来源 {{ sources.length }} 个。批量修改会作用于所有已知 UMO，没有旧规则的来源会自动创建默认规则。</p></div><NButton secondary @click="openGroupCreate">管理分组</NButton></div>
      <div class="batch-toolbar">
        <NSelect v-model:value="batchScope" :options="[{ label: '选中会话', value: 'selected' }, { label: '全部会话', value: 'all' }, { label: '群聊来源', value: 'groups' }, { label: '私聊来源', value: 'private' }, { label: '指定分组', value: 'group' }]" style="width: 140px" />
        <NSelect v-if="batchScope === 'group'" v-model:value="batchGroupID" :options="groups.map((item) => ({ label: item.name, value: item.id }))" placeholder="选择分组" style="width: 180px" />
        <NSelect v-model:value="batchLLM" :options="[{ label: 'AI：不修改', value: 'keep' }, { label: 'AI：启用', value: 'enable' }, { label: 'AI：停用', value: 'disable' }]" style="width: 150px" />
        <NSelect v-model:value="batchProcess" :options="[{ label: '处理：不修改', value: 'keep' }, { label: '处理：启用', value: 'enable' }, { label: '处理：停用', value: 'disable' }]" style="width: 160px" />
        <NSelect v-model:value="batchModel" clearable filterable :options="modelOptions" placeholder="批量覆盖模型（可选）" style="width: 260px" />
        <NButton type="primary" secondary @click="applyBatch">应用批量修改</NButton>
      </div>
      <div v-if="rules.length" class="data-table-wrap"><table class="data-table"><thead><tr><th class="check-column">选</th><th>来源</th><th>处理 / AI</th><th>模型 / 人格</th><th>更新时间</th><th>操作</th></tr></thead><tbody><tr v-for="item in rules" :key="item.source"><td class="check-column"><input v-model="selectedSources" type="checkbox" :value="item.source"></td><td><strong>{{ item.source }}</strong><code>{{ item.note || '未填写备注' }}</code></td><td><NTag size="small" :bordered="false" :type="item.process_enabled ? 'success' : 'warning'">{{ item.process_enabled ? '处理' : '跳过' }}</NTag><NTag size="small" :bordered="false" :type="item.llm_enabled ? 'info' : 'warning'">{{ item.llm_enabled ? '内置 AI' : 'AI 关闭' }}</NTag></td><td><span>{{ item.chat_model || '沿用全局模型' }}</span><code>{{ personas.find((persona) => persona.id === item.persona_id)?.name || '沿用默认人格' }}</code></td><td>{{ item.updated_at ? new Date(item.updated_at).toLocaleString() : '—' }}</td><td><NSpace size="small"><NButton size="small" secondary @click="openEdit(item)">编辑</NButton><NButton size="small" tertiary type="error" @click="remove(item)">删除</NButton></NSpace></td></tr></tbody></table></div>
      <NEmpty v-else description="还没有会话规则" />
    </NCard>

    <NCard class="detail-card group-card" :bordered="false">
      <div class="section-heading-row"><div><h3>会话分组</h3><p>分组只保存来源集合，批量修改时会展开为精确来源。</p></div><NButton secondary @click="openGroupCreate"><AppIcon name="plus" :size="14" />新建分组</NButton></div>
      <div v-if="groups.length" class="group-list"><div v-for="item in groups" :key="item.id" class="group-row"><div><strong>{{ item.name }}</strong><span>{{ item.description || '暂无描述' }}</span></div><NTag size="small" :bordered="false">{{ item.members.length }} 个来源</NTag><NSpace size="small"><NButton size="small" secondary @click="openGroupEdit(item)">编辑</NButton><NButton size="small" tertiary type="error" @click="removeGroup(item)">删除</NButton></NSpace></div></div>
      <NEmpty v-else description="还没有分组" />
    </NCard>

    <NModal v-model:show="showEditor" preset="card" :mask-closable="false" style="width: min(820px, calc(100vw - 32px))" :title="editingSource ? '编辑会话规则' : '新建会话规则'">
      <NForm label-placement="top" :show-feedback="false">
        <NFormItem label="消息会话来源" required><NSelect v-model:value="form.source" :options="sourceOptions" filterable :disabled="Boolean(editingSource)" placeholder="从已产生消息的会话中选择来源" /></NFormItem>
        <div class="form-grid-3"><NFormItem label="处理消息"><NSelect v-model:value="form.process_enabled" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="内置 AI"><NSelect v-model:value="form.llm_enabled" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="TTS"><NSelect v-model:value="form.tts_enabled" :options="booleanOverrideOptions" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="覆盖聊天模型"><NSelect v-model:value="form.chat_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem><NFormItem label="人格"><NSelect v-model:value="form.persona_id" clearable :options="personaOptions" placeholder="沿用默认人格" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="配置文件"><NSelect v-model:value="form.profile_id" clearable :options="profileOptions" placeholder="继承当前配置" /></NFormItem><NFormItem label="跟随配置文件"><NSelect v-model:value="form.follow_profile" :options="booleanOverrideOptions" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="STT 模型"><NSelect v-model:value="form.stt_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem><NFormItem label="TTS 模型"><NSelect v-model:value="form.tts_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="私聊行为"><NSelect v-model:value="form.private_mode" :options="[{ label: '继承全局', value: '' }, { label: '正常响应', value: 'responsive' }, { label: '只记录不回复', value: 'observe_only' }]" /></NFormItem><NFormItem label="群聊参与"><NSelect v-model:value="form.group_participation_mode" :options="[{ label: '继承全局', value: '' }, { label: '仅被点名时响应', value: 'addressed_only' }, { label: '只观察', value: 'observe_only' }]" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="记录未唤醒消息"><NSelect v-model:value="form.record_unaddressed_messages" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="允许表情回应"><NSelect v-model:value="form.reaction_enabled" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="群聊上下文"><NSelect v-model:value="form.group_context_enabled" :options="booleanOverrideOptions" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="主动参与"><NSelect v-model:value="form.proactive_enabled" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="Follow-up"><NSelect v-model:value="form.follow_up_enabled" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="关系状态"><NSelect v-model:value="form.relation_enabled" :options="booleanOverrideOptions" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="私聊每小时主动上限"><NInputNumber v-model:value="form.private_hourly_reply_limit" :min="0" :max="100000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="群聊每小时主动上限"><NInputNumber v-model:value="form.group_hourly_reply_limit" :min="0" :max="100000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="安静时区"><NInput v-model:value="form.quiet_hours_timezone" placeholder="例如 Asia/Shanghai" /></NFormItem><NFormItem label="安静开始"><NInput v-model:value="form.quiet_hours_start" placeholder="例如 23:00" /></NFormItem><NFormItem label="安静结束"><NInput v-model:value="form.quiet_hours_end" placeholder="例如 08:00" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="紧急提醒绕过安静时段"><NSelect v-model:value="form.emergency_bypass_quiet_hours" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="群聊回复引用"><NSelect v-model:value="form.reply_quote" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="私聊回复引用"><NSelect v-model:value="form.private_reply_quote" :options="booleanOverrideOptions" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="Runtime 开关"><NSelect v-model:value="form.runtime_enabled" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="Runtime 并发"><NInputNumber v-model:value="form.runtime_max_concurrency" :min="1" :max="64" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="来源队列上限"><NInputNumber v-model:value="form.source_queue_limit" :min="1" :max="10000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="私聊聚合等待（毫秒）"><NInputNumber v-model:value="form.turn_wait_ms" :min="0" :max="120000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="群聊聚合等待（毫秒）"><NInputNumber v-model:value="form.group_turn_wait_ms" :min="0" :max="120000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="附件等待（毫秒）"><NInputNumber v-model:value="form.attachment_wait_ms" :min="0" :max="180000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="最大聚合消息数"><NInputNumber v-model:value="form.max_turn_messages" :min="1" :max="100" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="群聊上下文条数"><NInputNumber v-model:value="form.group_message_max_count" :min="1" :max="300" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="群图片转述"><NSelect v-model:value="form.group_image_caption" :options="booleanOverrideOptions" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="群图片转述模型"><NInput v-model:value="form.group_image_caption_model" placeholder="继承全局；留空使用主模型" /></NFormItem><NFormItem label="主动程度"><NSelect v-model:value="form.proactive_degree" :options="[{ label: '继承全局', value: '' }, { label: '关闭', value: 'off' }, { label: '低', value: 'low' }, { label: '正常', value: 'normal' }, { label: '高', value: 'high' }]" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="主动冷却（秒）"><NInputNumber v-model:value="form.cooldown_seconds" :min="0" :max="604800" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="心跳周期（秒）"><NInputNumber v-model:value="form.heartbeat_seconds" :min="5" :max="86400" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="关系保留（秒）"><NInputNumber v-model:value="form.relation_retention_seconds" :min="3600" :max="31536000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="Follow-up 上限"><NInputNumber v-model:value="form.follow_up_max" :min="1" :max="10000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="最大重试"><NInputNumber v-model:value="form.follow_up_max_retries" :min="0" :max="10" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="重试等待（秒）"><NInputNumber v-model:value="form.follow_up_retry_delay_seconds" :min="0" :max="86400" clearable placeholder="继承全局" style="width: 100%" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="Follow-up 最大延迟（秒）"><NInputNumber v-model:value="form.follow_up_max_delay_seconds" :min="0" :max="604800" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="允许主动联系的来源"><NInput v-model:value="form.follow_up_allowed_sources" placeholder="逗号分隔 UMO；留空继承全局" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="聊天表达开关"><NSelect v-model:value="form.expression_enabled" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="最大表达段数"><NInputNumber v-model:value="form.expression_max_segments" :min="1" :max="8" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="长文阈值"><NInputNumber v-model:value="form.expression_long_threshold" :min="100" :max="10000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="段间隔（毫秒）"><NInputNumber v-model:value="form.expression_delay_ms" :min="0" :max="5000" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="回复时 @ 用户"><NSelect v-model:value="form.reply_mention" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="按需调用 Agent"><NSelect v-model:value="form.agent_on_demand_enabled" :options="booleanOverrideOptions" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="允许的只读工具"><NInput v-model:value="form.allowed_read_only_tools" placeholder="逗号分隔工具名；留空继承全局" /></NFormItem><NFormItem label="工具预算"><NInputNumber v-model:value="form.tool_budget" :min="0" :max="100" clearable placeholder="继承全局" style="width: 100%" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="子 Agent"><NSelect v-model:value="form.subagent_enabled" :options="booleanOverrideOptions" /></NFormItem><NFormItem label="消息表达风格"><NSelect v-model:value="form.message_style" :options="[{ label: '继承全局', value: '' }, { label: '自然聊天', value: 'natural' }, { label: '结构化说明', value: 'structured' }]" /></NFormItem></div>
        <NFormItem label="禁用 Runtime 动作"><NSelect v-model:value="form.disabled_actions" multiple clearable :options="runtimeActionOptions" placeholder="留空表示继承全局权限" /></NFormItem>
        <div class="form-grid-3"><NFormItem label="知识库（逗号分隔）"><NInput v-model:value="form.knowledge_bases" placeholder="可选知识库 ID" /></NFormItem><NFormItem label="知识库 Top K"><NInputNumber v-model:value="form.knowledge_top_k" :min="1" :max="100" clearable placeholder="继承全局" style="width: 100%" /></NFormItem><NFormItem label="知识库重排"><NSelect v-model:value="form.knowledge_rerank" :options="booleanOverrideOptions" /></NFormItem></div>
        <NFormItem label="停用插件（逗号分隔）"><NInput v-model:value="form.disabled_plugins" placeholder="仅记录规则，实际插件由内置工具目录决定" /></NFormItem>
        <NFormItem label="备注"><NInput v-model:value="form.note" type="textarea" :autosize="{ minRows: 2, maxRows: 5 }" /></NFormItem>
        <NFormItem v-if="editingSource" label="按项清除覆盖"><NSpace><NSelect v-model:value="resetFieldKey" :options="resetFieldOptions" clearable placeholder="选择后恢复继承全局配置" style="width: 230px" /><NButton secondary type="warning" @click="resetOverride">清除此项</NButton></NSpace><p class="muted">只清除所选字段，不影响同一来源的其他规则；全部清除后规则行会移除，但会话来源仍保留。</p></NFormItem>
      </NForm>
      <template #footer><div class="modal-footer"><NButton @click="showEditor = false">取消</NButton><NButton type="primary" :loading="saving" @click="save">保存</NButton></div></template>
    </NModal>

    <NModal v-model:show="showGroupEditor" preset="card" :mask-closable="false" style="width: min(620px, calc(100vw - 32px))" :title="editingGroupID ? '编辑会话分组' : '新建会话分组'">
      <NForm label-placement="top" :show-feedback="false"><NFormItem label="名称" required><NInput v-model:value="groupForm.name" /></NFormItem><NFormItem label="描述"><NInput v-model:value="groupForm.description" /></NFormItem><NFormItem label="来源集合" required><NInput v-model:value="groupForm.members" type="textarea" :autosize="{ minRows: 5, maxRows: 10 }" placeholder="每行一个来源，也支持逗号分隔" /></NFormItem></NForm>
      <template #footer><div class="modal-footer"><NButton @click="showGroupEditor = false">取消</NButton><NButton type="primary" @click="saveGroup">保存</NButton></div></template>
    </NModal>
  </div>
</template>
