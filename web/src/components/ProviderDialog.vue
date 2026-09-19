<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { NButton, NForm, NFormItem, NInput, NModal, NSelect, NSpace, useMessage } from 'naive-ui'
import { request } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Provider } from '@/types'

// 供应商的新增与编辑放在 chat 内，避免为了改一个模型地址跳回管理台。
const props = defineProps<{ show: boolean; provider?: Provider | null }>()
const emit = defineEmits<{ 'update:show': [value: boolean] }>()

const store = useAppStore()
const message = useMessage()
const name = ref('')
const baseURL = ref('')
const protocol = ref('openai-compatible')
const apiKey = ref('')
const modelIDs = ref('')
const saving = ref(false)

const protocolOptions = [
  { label: 'OpenAI 兼容', value: 'openai-compatible' },
  { label: 'OpenAI Completions', value: 'openai-completions' },
  { label: 'Gemini', value: 'gemini' },
]

const editing = computed(() => Boolean(props.provider?.id))
const show = computed({ get: () => props.show, set: (value: boolean) => emit('update:show', value) })

watch(() => props.show, (value) => {
  if (!value) return
  const provider = props.provider
  name.value = provider?.name || ''
  baseURL.value = provider?.base_url || ''
  protocol.value = provider?.protocol || 'openai-compatible'
  apiKey.value = ''
  modelIDs.value = (provider?.models || []).map((item) => item.id).join('\n')
})

function parseModels() {
  return modelIDs.value
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .map((id) => ({ id, display_name: id, enabled: true }))
}

async function submit() {
  const trimmedName = name.value.trim()
  const trimmedURL = baseURL.value.trim()
  if (!trimmedName || !trimmedURL) {
    message.warning('请填写供应商名称和接口地址')
    return
  }
  const payload: Record<string, unknown> = {
    name: trimmedName,
    base_url: trimmedURL,
    protocol: protocol.value,
    models: parseModels(),
  }
  // 编辑时留空表示保留原有密钥。
  if (apiKey.value.trim()) payload.api_key = apiKey.value.trim()
  saving.value = true
  try {
    const path = editing.value ? `/api/v1/providers/${encodeURIComponent(props.provider!.id)}` : '/api/v1/providers'
    await request(path, { method: editing.value ? 'PUT' : 'POST', body: JSON.stringify(payload) })
    await store.reloadProviders()
    show.value = false
    message.success(editing.value ? '供应商已保存' : '供应商已添加')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存供应商失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <NModal v-model:show="show" preset="card" :title="editing ? '编辑供应商' : '添加供应商'" class="chat-dialog" :style="{ width: '520px' }">
    <NForm label-placement="top" :show-feedback="false">
      <NFormItem label="名称" required>
        <NInput v-model:value="name" placeholder="例如 DeepSeek" />
      </NFormItem>
      <NFormItem label="接口地址" required>
        <NInput v-model:value="baseURL" placeholder="例如 https://api.deepseek.com/v1" />
      </NFormItem>
      <NFormItem label="协议">
        <NSelect v-model:value="protocol" :options="protocolOptions" />
      </NFormItem>
      <NFormItem :label="editing ? 'API Key（留空表示不修改）' : 'API Key'">
        <NInput v-model:value="apiKey" type="password" show-password-on="click" placeholder="仅保存在本机" />
      </NFormItem>
      <NFormItem label="模型 ID">
        <NInput v-model:value="modelIDs" type="textarea" :autosize="{ minRows: 3, maxRows: 8 }" placeholder="每行一个模型 ID，例如&#10;deepseek-chat" />
      </NFormItem>
    </NForm>
    <template #footer>
      <NSpace justify="end">
        <NButton secondary :disabled="saving" @click="show = false">取消</NButton>
        <NButton type="primary" :loading="saving" @click="submit">保存</NButton>
      </NSpace>
    </template>
  </NModal>
</template>
