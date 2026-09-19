<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { NButton, NForm, NFormItem, NInput, NModal, NSpace, useMessage } from 'naive-ui'
import { request } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Workspace } from '@/types'

// 项目创建留在 chat 内完成，避免为了建一个项目跳回管理台。
const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ 'update:show': [value: boolean]; created: [workspace: Workspace] }>()

const store = useAppStore()
const message = useMessage()
const name = ref('')
const rootPath = ref('')
const saving = ref(false)

const show = computed({ get: () => props.show, set: (value: boolean) => emit('update:show', value) })

watch(() => props.show, (value) => {
  if (!value) return
  name.value = ''
  rootPath.value = ''
})

async function submit() {
  const trimmedName = name.value.trim()
  const trimmedPath = rootPath.value.trim()
  if (!trimmedName || !trimmedPath) {
    message.warning('请填写项目名称和源文件夹')
    return
  }
  saving.value = true
  try {
    const workspace = await request<Workspace>('/api/v1/workspaces', {
      method: 'POST',
      body: JSON.stringify({ name: trimmedName, type: 'local', root_path: trimmedPath, enabled: true }),
    })
    await store.reloadWorkspaces()
    emit('created', workspace)
    show.value = false
    message.success('项目已创建')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '创建项目失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <NModal v-model:show="show" preset="card" title="新建项目" class="chat-dialog" :style="{ width: '460px' }">
    <NForm label-placement="top" :show-feedback="false">
      <NFormItem label="项目名称" required>
        <NInput v-model:value="name" placeholder="例如 Abot" />
      </NFormItem>
      <NFormItem label="源文件夹" required>
        <NInput v-model:value="rootPath" placeholder="必须是已存在的绝对路径，例如 /Users/me/code/abot" />
      </NFormItem>
    </NForm>
    <p class="chat-dialog-hint">项目目录不会自动创建，也不会生成默认项目。远程目录请到管理台的项目页面配置。</p>
    <template #footer>
      <NSpace justify="end">
        <NButton secondary :disabled="saving" @click="show = false">取消</NButton>
        <NButton type="primary" :loading="saving" @click="submit">创建</NButton>
      </NSpace>
    </template>
  </NModal>
</template>
