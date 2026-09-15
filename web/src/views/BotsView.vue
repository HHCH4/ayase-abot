<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { NAlert, NButton, NCard, NEmpty, NForm, NFormItem, NInput, NInputNumber, NSelect, NSpace, NSwitch, NTag, useMessage } from 'naive-ui'
import { request } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Bot } from '@/types'

const store = useAppStore()
const message = useMessage()
const selectedID = ref('')
const editing = ref(false)
const saving = ref(false)
const testing = ref(false)

type BotForm = {
  id: string
  name: string
  type: string
  endpoint: string
  onebot_mode: string
  listen_host: string
  listen_port: number | null
  listen_path: string
  group_trigger_mode: string
  admin_user_ids: string
  telegram_token: string
  onebot_access_token: string
  enabled: boolean
  config_profile_id: string
}

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
    listen_path: '/ws', group_trigger_mode: 'mention', admin_user_ids: '', telegram_token: '', onebot_access_token: '', enabled: true, config_profile_id: '',
  }
}

function statusOf(bot: Bot) {
  return statusMap[bot.status] || { label: bot.status || '未知', type: 'default' as const }
}

function fillForm(bot?: Bot) {
  Object.assign(form, bot ? {
    id: bot.id, name: bot.name, type: bot.type, endpoint: bot.endpoint || '', onebot_mode: bot.onebot_mode || (bot.endpoint ? 'client' : 'reverse-server'),
    listen_host: bot.listen_host || '0.0.0.0', listen_port: bot.listen_port || 6199, listen_path: bot.listen_path || '/ws',
    group_trigger_mode: bot.group_trigger_mode || 'mention',
    admin_user_ids: (bot.admin_user_ids || []).join(', '),
    telegram_token: '', onebot_access_token: '', enabled: bot.enabled !== false, config_profile_id: '',
  } : emptyForm())
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
}

function createBot() {
  selectedID.value = ''
  editing.value = false
  fillForm()
}

function payload() {
  const value: Record<string, unknown> = {
    id: form.id.trim(), name: form.name.trim(), type: form.type, endpoint: form.endpoint.trim(), onebot_mode: form.onebot_mode,
    listen_host: form.listen_host.trim(), listen_port: form.listen_port || 0, listen_path: form.listen_path.trim(), group_trigger_mode: form.group_trigger_mode, enabled: form.enabled,
  }
  // 全局管理员是聊天指令的根信任，只能在 WebUI 配置。
  value.admin_user_ids = form.admin_user_ids.split(/[,\s]+/).map((item) => item.trim()).filter(Boolean)
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
        <h2>机器人</h2>
        <p>管理 Telegram 和 OneBot 11 连接实例。NapCat 反向 WebSocket 默认连接到这里配置的监听地址。</p>
      </div>
      <NButton type="primary" size="large" @click="createBot">＋ 创建机器人</NButton>
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
          </template>
          <NFormItem v-else label="Telegram Bot Token"><NInput v-model:value="form.telegram_token" type="password" show-password-on="click" :placeholder="editing ? '留空表示保留旧 Token' : '请输入 Bot Token'" /></NFormItem>
          <NFormItem label="全局管理员（用户 ID，逗号分隔）">
            <NInput v-model:value="form.admin_user_ids" placeholder="例如 10001, 10002；留空表示没有管理员，管理指令将不可用" />
            <small class="form-help">管理员可以新建会话、切换模型与人格、绑定工作区，并在群里授权群管理员。</small>
          </NFormItem>
          <NFormItem label="群聊触发方式"><NSelect v-model:value="form.group_trigger_mode" :options="[{ label: '仅 @ 机器人时触发（推荐）', value: 'mention' }, { label: '群内所有消息都触发', value: 'all' }]" /></NFormItem>

          <div class="setting-section">
            <div class="section-heading-row"><div><h3>会话配置</h3><p>机器人创建的对话默认使用此配置；单个对话可以在 chat 页面单独绑定。</p></div></div>
            <NFormItem label="默认配置文件"><NSelect v-model:value="form.config_profile_id" :options="profileOptions" /></NFormItem>
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
