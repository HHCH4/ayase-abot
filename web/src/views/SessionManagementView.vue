<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NButton, NCard, NCheckbox, NEmpty, NForm, NFormItem, NInput, NInputNumber, NModal, NSelect, NSpace, NTag, useMessage } from 'naive-ui'
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
const form = reactive({
  source: '', process_enabled: true, llm_enabled: true, tts_enabled: false, note: '', chat_model: '', stt_model: '', tts_model: '',
  follow_profile: true, profile_id: '', persona_id: '', disabled_plugins: '', knowledge_bases: '', knowledge_top_k: 5, knowledge_rerank: false,
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
  { label: '备注', value: 'note' },
]

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
  form.process_enabled = item?.process_enabled ?? true
  form.llm_enabled = item?.llm_enabled ?? true
  form.tts_enabled = item?.tts_enabled ?? false
  form.note = item?.note || ''
  form.chat_model = item?.chat_model || ''
  form.stt_model = item?.stt_model || ''
  form.tts_model = item?.tts_model || ''
  form.follow_profile = item?.follow_profile ?? true
  form.profile_id = item?.profile_id || ''
  form.persona_id = item?.persona_id || ''
  form.disabled_plugins = (item?.disabled_plugins || []).join(', ')
  form.knowledge_bases = (item?.knowledge_bases || []).join(', ')
  form.knowledge_top_k = item?.knowledge_top_k || 5
  form.knowledge_rerank = item?.knowledge_rerank ?? false
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
    const payload = {
      source: form.source.trim(), process_enabled: form.process_enabled, llm_enabled: form.llm_enabled, tts_enabled: form.tts_enabled,
      note: form.note.trim(), chat_model: form.chat_model.trim(), stt_model: form.stt_model.trim(), tts_model: form.tts_model.trim(),
      follow_profile: form.follow_profile, profile_id: form.profile_id || '', persona_id: form.persona_id || '',
      disabled_plugins: splitValues(form.disabled_plugins), knowledge_bases: splitValues(form.knowledge_bases), knowledge_top_k: form.knowledge_top_k, knowledge_rerank: form.knowledge_rerank,
    }
    if (editingSource.value) await saveSessionRule(payload)
    else await createSessionRule(payload)
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
        <div class="form-grid-3"><NFormItem label="处理消息"><NCheckbox v-model:checked="form.process_enabled">启用</NCheckbox></NFormItem><NFormItem label="内置 AI"><NCheckbox v-model:checked="form.llm_enabled">允许调用</NCheckbox></NFormItem><NFormItem label="TTS"><NCheckbox v-model:checked="form.tts_enabled">启用</NCheckbox></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="覆盖聊天模型"><NSelect v-model:value="form.chat_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem><NFormItem label="人格"><NSelect v-model:value="form.persona_id" clearable :options="personaOptions" placeholder="沿用默认人格" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="配置文件"><NSelect v-model:value="form.profile_id" clearable :options="profileOptions" placeholder="沿用当前配置" /></NFormItem><NFormItem label="跟随配置文件"><NCheckbox v-model:checked="form.follow_profile">切换时应用配置文件</NCheckbox></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="STT 模型"><NSelect v-model:value="form.stt_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem><NFormItem label="TTS 模型"><NSelect v-model:value="form.tts_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="知识库（逗号分隔）"><NInput v-model:value="form.knowledge_bases" placeholder="可选知识库 ID" /></NFormItem><NFormItem label="知识库 Top K"><NInputNumber v-model:value="form.knowledge_top_k" :min="1" :max="100" style="width: 100%" /></NFormItem></div>
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
