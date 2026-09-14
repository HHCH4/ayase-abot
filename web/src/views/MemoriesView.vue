<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NEmpty, NForm, NFormItem, NInput, NModal, NSpace, NTag, useMessage } from 'naive-ui'
import { clearMemories, createMemory, deleteMemory, readMemories, updateMemory } from '@/api'
import { useAppStore } from '@/stores/app'
import { useRouter } from 'vue-router'
import type { MemoryItem } from '@/types'

const userID = 'webui-user'
const store = useAppStore()
const router = useRouter()
const message = useMessage()
const items = ref<MemoryItem[]>([])
const query = ref('')
const loading = ref(false)
const saving = ref(false)
const showEditor = ref(false)
const editingID = ref('')
const form = reactive({ content: '', tags: '', conversation_id: '' })

const sortedItems = computed(() => [...items.value].sort((a, b) => String(b.updated_at || b.created_at).localeCompare(String(a.updated_at || a.created_at))))

function resetForm(item?: MemoryItem) {
  editingID.value = item?.id || ''
  form.content = item?.content || ''
  form.tags = item?.tags || ''
  form.conversation_id = item?.conversation_id || ''
}

function openCreate() {
  resetForm()
  showEditor.value = true
}

function openEdit(item: MemoryItem) {
  resetForm(item)
  showEditor.value = true
}

async function load() {
  loading.value = true
  try {
    items.value = await readMemories(userID, query.value)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取长期记忆失败')
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!form.content.trim()) {
    message.warning('记忆内容不能为空')
    return
  }
  saving.value = true
  try {
    if (editingID.value) {
      await updateMemory(editingID.value, { user_id: userID, content: form.content, tags: form.tags })
    } else {
      await createMemory({ user_id: userID, conversation_id: form.conversation_id.trim() || undefined, content: form.content, tags: form.tags })
    }
    showEditor.value = false
    await load()
    message.success(editingID.value ? '长期记忆已更新' : '长期记忆已保存')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存长期记忆失败')
  } finally {
    saving.value = false
  }
}

async function remove(item: MemoryItem) {
  if (!window.confirm('确认彻底删除这条长期记忆？删除后不可恢复。')) return
  try {
    await deleteMemory(item.id, userID)
    await load()
    message.success('长期记忆已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除长期记忆失败')
  }
}

async function clearAll() {
  if (!items.value.length || !window.confirm('确认彻底删除当前用户的全部长期记忆？删除后不可恢复。')) return
  try {
    await clearMemories(userID)
    items.value = []
    message.success('长期记忆已全部删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '清空长期记忆失败')
  }
}

function sourceLabel(source?: string) {
  return source === 'agent' ? 'Agent 明确保存' : source === 'conversation' ? '对话导入' : '手动保存'
}

function conversationTitle(id?: string) {
  if (!id) return ''
  return store.conversations.find((item) => item.id === id)?.title || id
}

function formatTime(value?: string) {
  return value ? new Date(value).toLocaleString() : '未知时间'
}

onMounted(async () => {
  await store.loadAll()
  await load()
})
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">LONG-TERM MEMORY</p>
        <h2>长期记忆</h2>
        <p>管理跨对话保留的信息。默认不自动收集，Agent 只有在你明确要求时才会调用保存工具。</p>
      </div>
      <NSpace>
        <NButton secondary :loading="loading" @click="load">刷新</NButton>
        <NButton type="primary" @click="openCreate">＋ 添加记忆</NButton>
      </NSpace>
    </div>

    <NAlert type="info" :show-icon="false" class="empty-panel">
      先在“配置中心 → 长期记忆”中开启 Agent 能力。自动检索是可选项；关闭自动检索时，Agent 仍可按需调用 <code>load_memory</code>。
    </NAlert>

    <NCard class="memory-card" :bordered="false">
      <div class="memory-toolbar">
        <NInput v-model:value="query" clearable placeholder="搜索记忆内容或标签" @keyup.enter="load" />
        <NSpace><NButton secondary :loading="loading" @click="load">搜索</NButton><NButton tertiary type="error" :disabled="!items.length" @click="clearAll">清空全部</NButton></NSpace>
      </div>
      <NEmpty v-if="!sortedItems.length" description="还没有长期记忆" class="memory-empty" />
      <div v-else class="memory-list">
        <article v-for="item in sortedItems" :key="item.id" class="memory-item">
          <div class="memory-item-header">
            <NSpace size="small"><NTag size="small" :bordered="false" type="success">{{ sourceLabel(item.source) }}</NTag><NTag v-if="item.tags" size="small" :bordered="false">{{ item.tags }}</NTag></NSpace>
            <span>{{ formatTime(item.updated_at || item.created_at) }}</span>
          </div>
          <p class="memory-content">{{ item.content }}</p>
          <div class="memory-item-footer">
            <button v-if="item.conversation_id" type="button" class="memory-source-link" @click="router.push({ name: 'chat', query: { conversation: item.conversation_id } })">来源：{{ conversationTitle(item.conversation_id) }}</button>
            <span v-else class="muted">用户 {{ item.user_id }}</span>
            <NSpace size="small"><NButton size="small" secondary @click="openEdit(item)">编辑</NButton><NButton size="small" tertiary type="error" @click="remove(item)">删除</NButton></NSpace>
          </div>
        </article>
      </div>
    </NCard>

    <NModal v-model:show="showEditor" preset="card" :mask-closable="false" style="width: min(680px, calc(100vw - 32px))" :title="editingID ? '编辑长期记忆' : '添加长期记忆'">
      <NForm label-placement="top" :show-feedback="false">
        <NFormItem label="记忆内容" required><NInput v-model:value="form.content" type="textarea" :autosize="{ minRows: 5, maxRows: 12 }" placeholder="例如：用户偏好使用中文，并希望代码包含中文注释。" /></NFormItem>
        <NFormItem v-if="!editingID" label="关联对话（可选）"><NInput v-model:value="form.conversation_id" placeholder="填写对话 ID，删除对话时会同步清理关联记忆" /></NFormItem>
        <NFormItem label="标签（可选）"><NInput v-model:value="form.tags" placeholder="例如 偏好、项目、工作方式" /></NFormItem>
      </NForm>
      <template #footer><div class="modal-footer"><NButton @click="showEditor = false">取消</NButton><NButton type="primary" :loading="saving" @click="save">保存</NButton></div></template>
    </NModal>
  </div>
</template>

