<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NEmpty, NForm, NFormItem, NInput, NInputNumber, NModal, NSelect, NSpace, NSwitch, NTag, useMessage } from 'naive-ui'
import { request } from '@/api'
import { useAppStore } from '@/stores/app'
import type { RemoteTarget, TestResult } from '@/types'

const store = useAppStore()
const message = useMessage()
const showEditor = ref(false)
const editingID = ref('')
const saving = ref(false)
const testing = ref(false)
const lastTest = ref<TestResult | null>(null)

type TargetForm = {
  id: string
  name: string
  transport: string
  host: string
  port: number | null
  user: string
  auth_type: string
  key_path: string
  password: string
  host_key_fingerprint: string
  enabled: boolean
}

const form = reactive<TargetForm>(emptyForm())
const sortedTargets = computed(() => [...store.remoteTargets].sort((a, b) => a.name.localeCompare(b.name)))
const targetStatus: Record<string, { label: string; type: 'success' | 'warning' | 'error' | 'default' }> = {
  ready: { label: '已确认', type: 'success' },
  configured: { label: '待确认', type: 'warning' },
  error: { label: '连接失败', type: 'error' },
}

function emptyForm(): TargetForm {
  return { id: '', name: '', transport: 'ssh', host: '', port: 22, user: '', auth_type: 'agent', key_path: '', password: '', host_key_fingerprint: '', enabled: true }
}

function statusOf(target: RemoteTarget) {
  return targetStatus[target.status] || { label: target.status || '未知', type: 'default' as const }
}

function openEditor(target?: RemoteTarget) {
  editingID.value = target?.id || ''
  lastTest.value = null
  Object.assign(form, target ? {
    id: target.id, name: target.name, transport: target.transport || 'ssh', host: target.host, port: target.port || 22,
    user: target.user, auth_type: target.auth_type || 'agent', key_path: target.key_path || '', password: '',
    host_key_fingerprint: target.host_key_fingerprint || '', enabled: target.enabled !== false,
  } : emptyForm())
  showEditor.value = true
}

function payload() {
  const value: Record<string, unknown> = {
    id: form.id.trim(), name: form.name.trim(), transport: form.transport, host: form.host.trim(), port: Number(form.port) || 22,
    user: form.user.trim(), auth_type: form.auth_type, key_path: form.key_path.trim(), host_key_fingerprint: form.host_key_fingerprint.trim(), enabled: form.enabled,
  }
  if (form.password.trim()) value.password = form.password.trim()
  return value
}

function acceptTest(result: TestResult, saved = false) {
  lastTest.value = result
  if (result.fingerprint) form.host_key_fingerprint = result.fingerprint
  if (result.ok) message.success(result.message || 'SSH 连接正常')
  else message.warning(result.message || '连接可达，但还需要确认主机指纹')
  if (saved) void store.reloadRemoteTargets()
}

async function testPreview() {
  testing.value = true
  try {
    const result = await request<TestResult>('/api/v1/remote-targets/preview/test', { method: 'POST', body: JSON.stringify(payload()) })
    acceptTest(result)
  } catch (error) {
    message.error(error instanceof Error ? error.message : 'SSH 连接测试失败')
  } finally {
    testing.value = false
  }
}

async function save() {
  saving.value = true
  try {
    const saved = await request<RemoteTarget>(`/api/v1/remote-targets${editingID.value ? `/${encodeURIComponent(editingID.value)}` : ''}`, {
      method: editingID.value ? 'PUT' : 'POST', body: JSON.stringify(payload()),
    })
    await store.reloadRemoteTargets()
    editingID.value = saved.id
    Object.assign(form, { id: saved.id })
    message.success('远程主机配置已保存，请测试并确认指纹')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存远程主机失败')
  } finally {
    saving.value = false
  }
}

async function testSaved(target: RemoteTarget) {
  try {
    const result = await request<TestResult>(`/api/v1/remote-targets/${encodeURIComponent(target.id)}/test`, { method: 'POST', body: '{}' })
    if (result.fingerprint && editingID.value === target.id) form.host_key_fingerprint = result.fingerprint
    acceptTest(result, true)
  } catch (error) {
    message.error(error instanceof Error ? error.message : 'SSH 连接测试失败')
    await store.reloadRemoteTargets()
  }
}

async function remove(target: RemoteTarget) {
  if (!window.confirm(`确认删除远程主机“${target.name}”？仍被项目引用时不能删除。`)) return
  try {
    await request(`/api/v1/remote-targets/${encodeURIComponent(target.id)}`, { method: 'DELETE' })
    await store.reloadRemoteTargets()
    if (editingID.value === target.id) {
      editingID.value = ''
      showEditor.value = false
    }
    message.success('远程主机已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除远程主机失败')
  }
}

onMounted(async () => {
  await store.loadAll()
})
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">REMOTE TARGETS</p>
        <h2>远程主机</h2>
        <p>连接参数独立于项目保存，同一台主机可以被多个项目复用。只有测试成功并确认主机指纹后，远程项目才会启用。</p>
      </div>
      <NButton type="primary" size="large" @click="openEditor()">＋ 添加远程主机</NButton>
    </div>

    <NAlert v-if="!sortedTargets.length" type="info" :show-icon="false" class="empty-panel">还没有远程主机。添加时不需要先保存，直接点击“测试连接”即可获取指纹；确认无误后再保存指纹。</NAlert>
    <div v-else class="remote-target-grid">
      <NCard v-for="target in sortedTargets" :key="target.id" class="remote-target-card" hoverable>
        <template #header><div class="workspace-card-title"><span class="workspace-icon">⌁</span><div><strong>{{ target.name }}</strong><span>{{ target.user }}@{{ target.host }}:{{ target.port }}</span></div></div></template>
        <template #header-extra><NTag round size="small" :type="statusOf(target).type">{{ statusOf(target).label }}</NTag></template>
        <div class="remote-target-meta"><span>SSH</span><span>{{ target.auth_type === 'key-file' ? '私钥认证' : target.auth_type === 'password' ? '密码认证' : 'ssh-agent' }}</span><span>项目可复用</span></div>
        <p class="muted">{{ target.status_message || (target.host_key_fingerprint ? '已记录主机指纹' : '尚未确认主机指纹') }}</p>
        <template #footer><NSpace wrap :size="8"><NButton size="small" secondary @click="testSaved(target)">测试连接</NButton><NButton size="small" secondary @click="openEditor(target)">编辑</NButton><NButton size="small" tertiary type="error" @click="remove(target)">删除</NButton></NSpace></template>
      </NCard>
    </div>

    <NModal v-model:show="showEditor" preset="card" :mask-closable="false" style="width: min(760px, calc(100vw - 32px))" :title="editingID ? '编辑远程主机' : '添加远程主机'">
      <NAlert type="info" :show-icon="false" class="form-tip">主机指纹用于防止连接到错误的 SSH 主机。第一次测试只会读取指纹，不会自动信任；请核对后再保存。</NAlert>
      <NForm label-placement="top" :show-feedback="false">
        <div class="form-grid-2"><NFormItem label="名称" required><NInput v-model:value="form.name" placeholder="例如 家庭开发机" /></NFormItem><NFormItem v-if="!editingID" label="内部 ID"><NInput v-model:value="form.id" placeholder="可留空，系统自动生成" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="主机地址" required><NInput v-model:value="form.host" placeholder="192.168.1.20" /></NFormItem><NFormItem label="端口"><NInputNumber v-model:value="form.port" :min="1" :max="65535" :show-button="false" /></NFormItem><NFormItem label="SSH 用户" required><NInput v-model:value="form.user" placeholder="例如 amginlily" /></NFormItem></div>
        <div class="form-grid-2"><NFormItem label="认证方式"><NSelect v-model:value="form.auth_type" :options="[{ label: 'ssh-agent', value: 'agent' }, { label: '本机私钥文件', value: 'key-file' }, { label: 'SSH 密码', value: 'password' }]" /></NFormItem><NFormItem label="启用主机"><NSwitch v-model:value="form.enabled" /></NFormItem></div>
        <NFormItem v-if="form.auth_type === 'key-file'" label="本机私钥路径"><NInput v-model:value="form.key_path" placeholder="例如 /Users/me/.ssh/id_ed25519" /></NFormItem>
        <NFormItem v-if="form.auth_type === 'password'" label="SSH 密码"><NInput v-model:value="form.password" type="password" show-password-on="click" placeholder="编辑时留空表示保留原密码" /></NFormItem>
        <NFormItem label="主机指纹"><NInput v-model:value="form.host_key_fingerprint" placeholder="点击测试连接后核对并保存" /></NFormItem>
      </NForm>
      <NAlert v-if="lastTest && !lastTest.ok" type="warning" :show-icon="false" class="form-tip">{{ lastTest.message }}<template v-if="lastTest.fingerprint"> 实际指纹：<code>{{ lastTest.fingerprint }}</code></template></NAlert>
      <template #footer><div class="modal-footer"><NButton @click="showEditor = false">关闭</NButton><NSpace><NButton secondary :loading="testing" @click="testPreview">测试连接</NButton><NButton type="primary" :loading="saving" @click="save">保存配置</NButton></NSpace></div></template>
    </NModal>
  </div>
</template>
