<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { NButton, NCard, NEmpty, NInput, NSelect, NSpace, NTabPane, NTabs, NTag, useMessage } from 'naive-ui'
import { request } from '@/api'
import { useAppStore } from '@/stores/app'
import type { BotCommand, BotCommandAudit } from '@/types'

const store = useAppStore()
const message = useMessage()
const selectedID = ref('')
const commands = ref<BotCommand[]>([])
const audits = ref<BotCommandAudit[]>([])
const loading = ref(false)
const saving = ref('')
const categoryFilter = ref<string | null>(null)
const permissionFilter = ref<string | null>(null)
const stateFilter = ref<string | null>(null)
const search = ref('')

const permissionOptions = [
  { label: '所有人', value: 'everyone' },
  { label: '群管理员', value: 'group_admin' },
  { label: '全局管理员', value: 'global_admin' },
]
const categoryLabels: Record<string, string> = { info: '信息', session: '会话', task: '任务', config: '配置', admin: '管理员' }
const categoryOptions = computed(() => Object.entries(categoryLabels).map(([value, label]) => ({ label, value })))
const stateOptions = [
  { label: '已启用', value: 'enabled' },
  { label: '已停用', value: 'disabled' },
]
const botOptions = computed(() => store.bots.map((item) => ({ label: `${item.name} · ${item.id}`, value: item.id })))

// 与后端保持一致的过滤，筛选条件不改变任何策略。
const visibleCommands = computed(() => commands.value.filter((item) => {
  if (categoryFilter.value && item.category !== categoryFilter.value) return false
  if (permissionFilter.value && item.effective_permission !== permissionFilter.value) return false
  if (stateFilter.value === 'enabled' && !item.effective_enabled) return false
  if (stateFilter.value === 'disabled' && item.effective_enabled) return false
  const needle = search.value.trim().toLowerCase()
  if (needle && !`${item.name} ${item.description} ${item.source_name}`.toLowerCase().includes(needle)) return false
  return true
}))
const disabledCount = computed(() => commands.value.filter((item) => !item.effective_enabled).length)

function formatTime(value: string) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function actionLabel(action: string) {
  const labels: Record<string, string> = {
    group_admin_add: '添加群管理员', group_admin_remove: '移除群管理员', group_admin_leave: '退出群管理员',
    model_switch: '切换模型', persona_switch: '切换人格', workspace_binding: '绑定工作区',
    command_policy: '调整指令策略', command_denied: '权限拒绝',
  }
  return labels[action] || action
}

function sourceTagType(command: BotCommand) {
  return command.source === 'builtin' ? 'default' : 'info'
}

async function loadCommands() {
  if (!selectedID.value) {
    commands.value = []
    return
  }
  loading.value = true
  try {
    const result = await request<{ commands: BotCommand[] }>(`/api/v1/bots/${encodeURIComponent(selectedID.value)}/commands`)
    commands.value = result.commands || []
  } catch (error) {
    commands.value = []
    message.error(error instanceof Error ? error.message : '读取指令列表失败')
  } finally {
    loading.value = false
  }
}

async function loadAudits() {
  if (!selectedID.value) {
    audits.value = []
    return
  }
  try {
    const result = await request<{ audits: BotCommandAudit[] }>(`/api/v1/bots/${encodeURIComponent(selectedID.value)}/commands/audit`)
    audits.value = result.audits || []
  } catch {
    audits.value = []
  }
}

async function updatePolicy(command: BotCommand, patch: { permission?: string; enabled?: boolean }) {
  saving.value = command.id
  try {
    const result = await request<{ commands: BotCommand[] }>(
      `/api/v1/bots/${encodeURIComponent(selectedID.value)}/commands/${encodeURIComponent(command.id)}`,
      {
        method: 'PUT',
        body: JSON.stringify({
          permission: patch.permission ?? command.effective_permission,
          enabled: patch.enabled ?? command.effective_enabled,
        }),
      },
    )
    commands.value = result.commands || commands.value
    await loadAudits()
    message.success('指令策略已更新')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '更新指令策略失败')
  } finally {
    saving.value = ''
  }
}

watch(selectedID, async () => {
  await loadCommands()
  await loadAudits()
})

onMounted(async () => {
  if (!store.bots.length) {
    await store.reloadBots()
  }
  if (!selectedID.value && store.bots.length) {
    selectedID.value = store.bots[0].id
  }
})
</script>

<template>
  <div class="view-stack">
    <NCard class="config-card" :bordered="false">
      <template #header>
        <div class="section-heading-row">
          <div>
            <p class="eyebrow">COMMAND CENTER</p>
            <h2>指令</h2>
            <p>聊天指令的权限与启停按机器人分别配置；每次改动都会写入审计。</p>
          </div>
          <NSpace align="center">
            <NSelect v-model:value="selectedID" :options="botOptions" placeholder="选择机器人" style="width: 220px" />
            <NTag size="small">共 {{ commands.length }} 条 · 已停用 {{ disabledCount }}</NTag>
          </NSpace>
        </div>
      </template>
      <NSpace align="center" :wrap="true">
        <NSelect v-model:value="categoryFilter" :options="categoryOptions" placeholder="按分类筛选" clearable style="width: 150px" />
        <NSelect v-model:value="permissionFilter" :options="permissionOptions" placeholder="按权限筛选" clearable style="width: 160px" />
        <NSelect v-model:value="stateFilter" :options="stateOptions" placeholder="按状态筛选" clearable style="width: 140px" />
        <NInput v-model:value="search" placeholder="搜索指令…" style="width: 220px" />
      </NSpace>

      <NEmpty v-if="!selectedID" description="请选择机器人" style="margin-top: 18px" />
      <NEmpty v-else-if="!visibleCommands.length" description="没有符合条件的指令" style="margin-top: 18px" />
      <div v-else class="command-list" :class="{ 'is-loading': loading }">
        <div class="command-head">
          <span>指令</span><span>来源</span><span>分类</span><span>描述</span><span>权限</span><span>操作</span>
        </div>
        <div v-for="command in visibleCommands" :key="command.id" class="command-row">
          <code>/{{ command.name }}</code>
          <NTag size="small" :type="sourceTagType(command)">{{ command.source_name }}</NTag>
          <span class="muted">{{ categoryLabels[command.category] || command.category }}</span>
          <span class="command-desc">{{ command.description }}</span>
          <NSpace align="center" :size="6">
            <NTag v-if="command.overridden" size="tiny" type="warning">已覆盖</NTag>
            <NSelect
              :value="command.effective_permission"
              :options="permissionOptions"
              size="small"
              style="width: 132px"
              :disabled="saving === command.id"
              @update:value="(value: string) => updatePolicy(command, { permission: value })"
            />
          </NSpace>
          <NButton
            size="small"
            :type="command.effective_enabled ? 'default' : 'primary'"
            :loading="saving === command.id"
            @click="updatePolicy(command, { enabled: !command.effective_enabled })"
          >
            {{ command.effective_enabled ? '停用' : '启用' }}
          </NButton>
        </div>
      </div>
    </NCard>

    <NCard class="config-card" :bordered="false" style="margin-top: 16px">
      <template #header>
        <div class="section-heading-row">
          <div>
            <h3>审计</h3>
            <p>群管理员增删、模型与人格切换、工作区绑定以及权限拒绝。</p>
          </div>
          <NButton secondary @click="loadAudits">刷新</NButton>
        </div>
      </template>
      <NEmpty v-if="!audits.length" description="暂无审计记录" />
      <div v-else class="audit-list">
        <div v-for="entry in audits" :key="entry.id" class="audit-row">
          <span class="muted">{{ formatTime(entry.created_at) }}</span>
          <strong>{{ actionLabel(entry.action) }}</strong>
          <span>{{ entry.chat_id || '—' }}</span>
          <span>{{ entry.user_id || '—' }}</span>
          <span>{{ entry.target || '—' }}</span>
          <NTag size="small" :type="entry.result === 'ok' || entry.result === 'enabled' ? 'success' : entry.result === 'denied' ? 'error' : 'default'">
            {{ entry.result }}
          </NTag>
        </div>
      </div>
    </NCard>
  </div>
</template>

<style scoped>
.command-list { display: grid; gap: 6px; margin-top: 16px; }
.command-list.is-loading { opacity: .6; }
.command-head, .command-row { display: grid; grid-template-columns: 150px 120px 80px minmax(0, 1fr) 160px 80px; align-items: center; gap: 10px; }
.command-head { padding: 0 10px 6px; color: #98a1ad; font-size: 11px; letter-spacing: .04em; }
.command-row { padding: 9px 10px; border: 1px solid #eef1f5; border-radius: 10px; }
.command-row code { color: var(--brand); font-size: 12px; }
.command-desc { color: #55606e; font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.audit-list { display: grid; gap: 6px; }
.audit-row { display: grid; grid-template-columns: 170px 130px 130px 110px minmax(0, 1fr) 90px; align-items: center; gap: 10px; padding: 8px 10px; border: 1px solid #eef1f5; border-radius: 10px; font-size: 12px; }
</style>
