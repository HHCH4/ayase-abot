<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NButton, NCard, NCheckbox, NEmpty, NForm, NFormItem, NInput, NInputNumber, NModal, NSelect, NSpace, NTag, useMessage } from 'naive-ui'
import { batchSessionRules, createSessionRule, deleteSessionRule, deleteSessionRuleGroup, readPersonas, readSessionRuleGroups, readSessionRules, saveSessionRule, saveSessionRuleGroup } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Persona, SessionRule, SessionRuleGroup } from '@/types'

const store = useAppStore()
const message = useMessage()
const rules = ref<SessionRule[]>([])
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

// 读取规则、分组和人格目录，编辑页只使用已存在的配置实体。
async function load() {
  loading.value = true
  try {
    await store.loadAll()
    const [ruleItems, groupItems, personaResult] = await Promise.all([readSessionRules(), readSessionRuleGroups(), readPersonas()])
    rules.value = ruleItems
    groups.value = groupItems
    personas.value = personaResult.personas || []
    selectedSources.value = selectedSources.value.filter((source) => rules.value.some((item) => item.source === source))
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
}

function openCreate() {
  resetForm()
  showEditor.value = true
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
        <p>按 UMO 或 /sid 返回的来源标识覆盖处理、内置 AI、模型、人格和知识库选项；所有规则都在本地内置 Agent 边界内执行。</p>
      </div>
      <NSpace><NButton secondary :loading="loading" @click="load">刷新</NButton><NButton type="primary" @click="openCreate">＋ 新建规则</NButton></NSpace>
    </div>

    <NCard class="detail-card" :bordered="false">
      <div class="section-heading-row"><div><h3>会话来源规则</h3><p>已选 {{ selectedCount }} 条；来源值建议直接复制聊天中的 /sid 结果。</p></div><NButton secondary @click="openGroupCreate">管理分组</NButton></div>
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
      <div class="section-heading-row"><div><h3>会话分组</h3><p>分组只保存来源集合，批量修改时会展开为精确来源。</p></div><NButton secondary @click="openGroupCreate">＋ 新建分组</NButton></div>
      <div v-if="groups.length" class="group-list"><div v-for="item in groups" :key="item.id" class="group-row"><div><strong>{{ item.name }}</strong><span>{{ item.description || '暂无描述' }}</span></div><NTag size="small" :bordered="false">{{ item.members.length }} 个来源</NTag><NSpace size="small"><NButton size="small" secondary @click="openGroupEdit(item)">编辑</NButton><NButton size="small" tertiary type="error" @click="removeGroup(item)">删除</NButton></NSpace></div></div>
      <NEmpty v-else description="还没有分组" />
    </NCard>

    <NModal v-model:show="showEditor" preset="card" :mask-closable="false" style="width: min(820px, calc(100vw - 32px))" :title="editingSource ? '编辑会话规则' : '新建会话规则'">
      <NForm label-placement="top" :show-feedback="false">
        <NFormItem label="消息会话来源" required><NInput v-model:value="form.source" :disabled="Boolean(editingSource)" placeholder="例如：telegram:123456 或 onebot:group:987" /></NFormItem>
        <div class="form-grid-3"><NFormItem label="处理消息"><NCheckbox v-model:checked="form.process_enabled">启用</NCheckbox></NFormItem><NFormItem label="内置 AI"><NCheckbox v-model:checked="form.llm_enabled">允许调用</NCheckbox></NFormItem><NFormItem label="TTS"><NCheckbox v-model:checked="form.tts_enabled">启用</NCheckbox></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="覆盖聊天模型"><NSelect v-model:value="form.chat_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem><NFormItem label="人格"><NSelect v-model:value="form.persona_id" clearable :options="personaOptions" placeholder="沿用默认人格" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="配置文件"><NSelect v-model:value="form.profile_id" clearable :options="profileOptions" placeholder="沿用当前配置" /></NFormItem><NFormItem label="跟随配置文件"><NCheckbox v-model:checked="form.follow_profile">切换时应用配置文件</NCheckbox></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="STT 模型"><NSelect v-model:value="form.stt_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem><NFormItem label="TTS 模型"><NSelect v-model:value="form.tts_model" clearable filterable :options="modelOptions" placeholder="沿用配置文件模型" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="知识库（逗号分隔）"><NInput v-model:value="form.knowledge_bases" placeholder="可选知识库 ID" /></NFormItem><NFormItem label="知识库 Top K"><NInputNumber v-model:value="form.knowledge_top_k" :min="1" :max="100" style="width: 100%" /></NFormItem></div>
        <NFormItem label="停用插件（逗号分隔）"><NInput v-model:value="form.disabled_plugins" placeholder="仅记录规则，实际插件由内置工具目录决定" /></NFormItem>
        <NFormItem label="备注"><NInput v-model:value="form.note" type="textarea" :autosize="{ minRows: 2, maxRows: 5 }" /></NFormItem>
      </NForm>
      <template #footer><div class="modal-footer"><NButton @click="showEditor = false">取消</NButton><NButton type="primary" :loading="saving" @click="save">保存</NButton></div></template>
    </NModal>

    <NModal v-model:show="showGroupEditor" preset="card" :mask-closable="false" style="width: min(620px, calc(100vw - 32px))" :title="editingGroupID ? '编辑会话分组' : '新建会话分组'">
      <NForm label-placement="top" :show-feedback="false"><NFormItem label="名称" required><NInput v-model:value="groupForm.name" /></NFormItem><NFormItem label="描述"><NInput v-model:value="groupForm.description" /></NFormItem><NFormItem label="来源集合" required><NInput v-model:value="groupForm.members" type="textarea" :autosize="{ minRows: 5, maxRows: 10 }" placeholder="每行一个来源，也支持逗号分隔" /></NFormItem></NForm>
      <template #footer><div class="modal-footer"><NButton @click="showGroupEditor = false">取消</NButton><NButton type="primary" @click="saveGroup">保存</NButton></div></template>
    </NModal>
  </div>
</template>
