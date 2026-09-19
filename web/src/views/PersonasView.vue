<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NCheckbox, NEmpty, NForm, NFormItem, NInput, NModal, NSpace, NTag, useMessage } from 'naive-ui'
import { deletePersona, readPersonaRevisions, readPersonas, request, savePersona, setDefaultPersona } from '@/api'
import type { Persona, PersonaRevision } from '@/types'

const message = useMessage()
const personas = ref<Persona[]>([])
const revisions = ref<PersonaRevision[]>([])
const selectedID = ref('')
const loading = ref(false)
const saving = ref(false)
const showEditor = ref(false)
const showHistory = ref(false)
const importInput = ref<HTMLInputElement | null>(null)
const form = reactive({ id: '', name: '', description: '', instruction: '', enabled: true })

const selected = computed(() => personas.value.find((item) => item.id === selectedID.value) || personas.value[0])

// 统一重置编辑草稿，避免在新建和编辑之间残留旧人格内容。
function resetForm(item?: Persona) {
  form.id = item?.id || ''
  form.name = item?.name || ''
  form.description = item?.description || ''
  form.instruction = item?.instruction || ''
  form.enabled = item?.enabled ?? true
}

function openCreate() {
  resetForm()
  showEditor.value = true
}

function openEdit(item: Persona) {
  selectedID.value = item.id
  resetForm(item)
  showEditor.value = true
}

async function load() {
  loading.value = true
  try {
    const result = await readPersonas()
    personas.value = result.personas || []
    selectedID.value = selectedID.value && personas.value.some((item) => item.id === selectedID.value)
      ? selectedID.value
      : result.default_persona_id || personas.value[0]?.id || ''
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取人格目录失败')
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!form.name.trim() || !form.instruction.trim()) {
    message.warning('人格名称和指令不能为空')
    return
  }
  saving.value = true
  try {
    const saved = await savePersona({
      id: form.id || undefined,
      name: form.name.trim(),
      description: form.description.trim(),
      instruction: form.instruction.trim(),
      enabled: form.enabled,
    })
    showEditor.value = false
    selectedID.value = saved.id
    await load()
    message.success(form.id ? '人格已更新' : '人格已创建')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存人格失败')
  } finally {
    saving.value = false
  }
}

async function makeDefault(item: Persona) {
  try {
    await setDefaultPersona(item.id)
    await load()
    message.success(`已将“${item.name}”设为默认人格`)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '切换默认人格失败')
  }
}

async function remove(item: Persona) {
  if (item.is_default) {
    message.warning('默认人格不能直接删除，请先切换默认人格')
    return
  }
  if (!window.confirm(`确认删除人格“${item.name}”及其修订历史？`)) return
  try {
    await deletePersona(item.id)
    await load()
    message.success('人格已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除人格失败')
  }
}

async function showRevisions(item: Persona) {
  selectedID.value = item.id
  try {
    revisions.value = await readPersonaRevisions(item.id)
    showHistory.value = true
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取人格修订历史失败')
  }
}

async function exportPersona(item: Persona) {
  try {
    const value = await request<Record<string, unknown>>(`/api/v1/personas/${encodeURIComponent(item.id)}/export`)
    const link = document.createElement('a')
    link.href = URL.createObjectURL(new Blob([JSON.stringify(value, null, 2)], { type: 'application/json' }))
    link.download = `${item.id}.persona.json`
    link.click()
    URL.revokeObjectURL(link.href)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '导出人格失败')
  }
}

async function importPersona(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  try {
    const value = JSON.parse(await file.text()) as Record<string, unknown>
    const imported = await request<Persona>('/api/v1/personas/import', { method: 'POST', body: JSON.stringify(value) })
    selectedID.value = imported.id
    await load()
    message.success('人格已导入')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '导入人格失败')
  }
}

function formatTime(value?: string) {
  return value ? new Date(value).toLocaleString() : '未知时间'
}

onMounted(load)
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">PERSONA CATALOG</p>
        <h2>人格设定</h2>
        <p>管理可复用的系统指令、启停状态、默认人格和修订历史。人格只影响内置 AI 的提示词，不引入外部编排执行方式。</p>
      </div>
      <NSpace>
        <NButton secondary :loading="loading" @click="load">刷新</NButton>
        <NButton secondary @click="importInput?.click()">导入</NButton>
        <input ref="importInput" class="visually-hidden" type="file" accept="application/json,.json" @change="importPersona">
        <NButton type="primary" @click="openCreate">＋ 新建人格</NButton>
      </NSpace>
    </div>

    <div v-if="personas.length" class="persona-grid">
      <NCard v-for="item in personas" :key="item.id" class="persona-card" :bordered="false" :class="{ selected: selected?.id === item.id }" @click="selectedID = item.id">
        <div class="persona-card-heading">
          <div class="detail-avatar">🎭</div>
          <div class="persona-card-copy">
            <strong>{{ item.name }}</strong>
            <span>{{ item.id }}</span>
          </div>
          <NTag v-if="item.is_default" size="small" type="success" :bordered="false">默认</NTag>
        </div>
        <p class="persona-description">{{ item.description || '暂无描述' }}</p>
        <div class="persona-card-meta">
          <NTag size="small" :bordered="false" :type="item.enabled ? 'info' : 'warning'">{{ item.enabled ? '已启用' : '已停用' }}</NTag>
          <span>修订 {{ item.revision }}</span>
        </div>
        <div class="persona-card-actions" @click.stop>
          <NButton size="small" secondary @click="openEdit(item)">编辑</NButton>
          <NButton size="small" tertiary @click="showRevisions(item)">历史</NButton>
          <NButton size="small" tertiary @click="exportPersona(item)">导出</NButton>
          <NButton v-if="!item.is_default" size="small" tertiary @click="makeDefault(item)">设为默认</NButton>
          <NButton v-if="!item.is_default" size="small" tertiary type="error" @click="remove(item)">删除</NButton>
        </div>
      </NCard>
    </div>
    <NEmpty v-else description="还没有人格设定" />

    <NAlert v-if="selected" type="info" :show-icon="false" class="persona-note">
      当前人格的工具和技能仍由 Abot 的内置工具目录统一管理；此页面只管理人格指令和选择关系，避免把未实现的能力伪装成可配置项。
    </NAlert>

    <NModal v-model:show="showEditor" preset="card" :mask-closable="false" style="width: min(760px, calc(100vw - 32px))" :title="form.id ? '编辑人格' : '新建人格'">
      <NForm label-placement="top" :show-feedback="false">
        <div class="form-grid-2">
          <NFormItem label="名称" required><NInput v-model:value="form.name" placeholder="例如：代码审查助手" /></NFormItem>
          <NFormItem label="状态"><NCheckbox v-model:checked="form.enabled">允许被选择</NCheckbox></NFormItem>
        </div>
        <NFormItem label="描述"><NInput v-model:value="form.description" placeholder="简短说明适用场景" /></NFormItem>
        <NFormItem label="系统指令" required><NInput v-model:value="form.instruction" type="textarea" :autosize="{ minRows: 8, maxRows: 18 }" placeholder="写入该人格要遵循的行为边界和表达方式" /></NFormItem>
      </NForm>
      <template #footer><div class="modal-footer"><NButton @click="showEditor = false">取消</NButton><NButton type="primary" :loading="saving" @click="save">保存</NButton></div></template>
    </NModal>

    <NModal v-model:show="showHistory" preset="card" style="width: min(760px, calc(100vw - 32px))" title="人格修订历史">
      <div v-if="revisions.length" class="revision-panel">
        <div v-for="item in revisions" :key="`${item.persona_id}-${item.revision}`" class="revision-row persona-revision-row">
          <strong>v{{ item.revision }}</strong><span>{{ item.name }}</span><span>{{ item.enabled ? '启用' : '停用' }}</span><span class="muted">{{ formatTime(item.created_at) }}</span>
        </div>
      </div>
      <NEmpty v-else description="没有修订记录" />
    </NModal>
  </div>
</template>
