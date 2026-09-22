<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { NButton, NCard, NEmpty, NForm, NFormItem, NInput, NInputNumber, NModal, NSelect, NSpace, NTag, useMessage } from 'naive-ui'
import { deleteScheduledTask, readScheduledTasks, saveScheduledTask, setScheduledTaskStatus } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import { useAppStore } from '@/stores/app'
import type { ScheduledTask, ScheduledTaskMode } from '@/types'

const store = useAppStore()
const message = useMessage()
const tasks = ref<ScheduledTask[]>([])
const loading = ref(false)
const saving = ref(false)
const showEditor = ref(false)
const editingID = ref('')
const form = reactive({
  name: '', request: '', mode: 'once' as ScheduledTaskMode, start_at: '', interval_seconds: 3600, weekday: 1, month_day: 1, time_of_day: '09:00', cron: '0 9 * * *',
  user_id: 'scheduler', conversation_id: '', adapter_id: '', chat_id: '',
})

const modeOptions = [
  { label: '一次性', value: 'once' }, { label: '间隔循环', value: 'interval' }, { label: '每天', value: 'daily' },
  { label: '每周', value: 'weekly' }, { label: '每月', value: 'monthly' }, { label: '自定义 Cron', value: 'custom' },
]
const weekdayOptions = [{ label: '周日', value: 0 }, { label: '周一', value: 1 }, { label: '周二', value: 2 }, { label: '周三', value: 3 }, { label: '周四', value: 4 }, { label: '周五', value: 5 }, { label: '周六', value: 6 }]
const adapterOptions = () => store.bots.map((item) => ({ label: `${item.name} (${item.id})`, value: item.id }))

// 读取任务和平台列表，任务执行器始终绑定到本地内置 Coordinator。
async function load() {
  loading.value = true
  try {
    await store.loadAll()
    tasks.value = await readScheduledTasks()
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取未来任务失败')
  } finally {
    loading.value = false
  }
}

function localDateTime(value?: string) {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const pad = (number: number) => String(number).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

function resetForm(item?: ScheduledTask) {
  editingID.value = item?.id || ''
  form.name = item?.name || ''
  form.request = item?.request || ''
  form.mode = item?.mode || 'once'
  form.start_at = localDateTime(item?.start_at)
  form.interval_seconds = item?.interval_seconds || 3600
  form.weekday = item?.weekday ?? 1
  form.month_day = item?.month_day || 1
  form.time_of_day = item?.time_of_day || '09:00'
  form.cron = item?.cron || '0 9 * * *'
  form.user_id = item?.user_id || 'scheduler'
  form.conversation_id = item?.conversation_id || ''
  form.adapter_id = item?.adapter_id || ''
  form.chat_id = item?.chat_id || ''
}

function openCreate() {
  resetForm()
  showEditor.value = true
}

function openEdit(item: ScheduledTask) {
  resetForm(item)
  showEditor.value = true
}

function toISO(value: string) {
  if (!value) return undefined
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString()
}

async function save() {
  if (!form.name.trim() || !form.request.trim()) {
    message.warning('任务名称和请求内容不能为空')
    return
  }
  if (form.mode === 'once' && !form.start_at) {
    message.warning('一次性任务需要指定执行时间')
    return
  }
  saving.value = true
  try {
    const payload = {
      id: editingID.value || undefined, name: form.name.trim(), request: form.request.trim(), mode: form.mode,
      start_at: form.mode === 'once' ? toISO(form.start_at) : undefined, interval_seconds: form.mode === 'interval' ? form.interval_seconds : undefined,
      weekday: form.mode === 'weekly' ? form.weekday : undefined, month_day: form.mode === 'monthly' ? form.month_day : undefined,
      time_of_day: ['daily', 'weekly', 'monthly'].includes(form.mode) ? form.time_of_day : undefined, cron: form.mode === 'custom' ? form.cron.trim() : undefined,
      user_id: form.user_id.trim() || 'scheduler', conversation_id: form.conversation_id.trim() || undefined, adapter_id: form.adapter_id || undefined, chat_id: form.chat_id.trim() || undefined,
    }
    const saved = await saveScheduledTask(payload)
    showEditor.value = false
    await load()
    message.success(editingID.value ? '未来任务已更新' : `未来任务已创建（${saved.id}）`)
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存未来任务失败')
  } finally {
    saving.value = false
  }
}

async function toggle(item: ScheduledTask) {
  try {
    await setScheduledTaskStatus(item.id, item.status === 'active' ? 'pause' : 'resume')
    await load()
    message.success(item.status === 'active' ? '任务已暂停' : '任务已恢复')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '更新任务状态失败')
  }
}

async function remove(item: ScheduledTask) {
  if (!window.confirm(`确认删除未来任务“${item.name}”？`)) return
  try {
    await deleteScheduledTask(item.id)
    await load()
    message.success('未来任务已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除未来任务失败')
  }
}

function modeLabel(mode: string) {
  return modeOptions.find((item) => item.value === mode)?.label || mode
}

function statusLabel(status: string) {
  return status === 'active' ? '运行中' : status === 'paused' ? '已暂停' : status === 'completed' ? '已完成' : status
}

function formatTime(value?: string) {
  return value ? new Date(value).toLocaleString() : '未计算'
}

onMounted(load)
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">SCHEDULED TASKS</p>
        <h2>未来任务</h2>
        <p>创建一次性、周期性或 Cron 任务。到期后由本地调度器启动内置 AI，外部 Dify、Coze、百炼、DeerFlow 等执行方式不会被启用。</p>
      </div>
      <NSpace><NButton secondary :loading="loading" @click="load">刷新</NButton><NButton type="primary" @click="openCreate"><AppIcon name="plus" :size="14" />新建任务</NButton></NSpace>
    </div>

    <div v-if="tasks.length" class="task-grid">
      <NCard v-for="item in tasks" :key="item.id" class="task-card" :bordered="false">
        <div class="task-card-heading"><div><strong>{{ item.name }}</strong><code>{{ item.id }}</code></div><NTag size="small" :bordered="false" :type="item.status === 'active' ? 'success' : item.status === 'paused' ? 'warning' : 'default'">{{ statusLabel(item.status) }}</NTag></div>
        <p class="task-request">{{ item.request }}</p>
        <div class="task-meta"><span>{{ modeLabel(item.mode) }}</span><span>下次：{{ formatTime(item.next_run_at) }}</span><span v-if="item.adapter_id">投递：{{ item.adapter_id }} / {{ item.chat_id || '未指定会话' }}</span></div>
        <p v-if="item.last_error" class="error-text">上次错误：{{ item.last_error }}</p>
        <div class="persona-card-actions"><NButton size="small" secondary @click="openEdit(item)">编辑</NButton><NButton size="small" tertiary @click="toggle(item)">{{ item.status === 'active' ? '暂停' : '恢复' }}</NButton><NButton size="small" tertiary type="error" @click="remove(item)">删除</NButton></div>
      </NCard>
    </div>
    <NEmpty v-else description="还没有未来任务" />

    <NModal v-model:show="showEditor" preset="card" :mask-closable="false" style="width: min(820px, calc(100vw - 32px))" :title="editingID ? '编辑未来任务' : '新建未来任务'">
      <NForm label-placement="top" :show-feedback="false">
        <div class="form-grid-2"><NFormItem label="名称" required><NInput v-model:value="form.name" placeholder="例如：每天早上汇总项目进展" /></NFormItem><NFormItem label="执行方式"><NSelect v-model:value="form.mode" :options="modeOptions" /></NFormItem></div>
        <NFormItem label="请求内容" required><NInput v-model:value="form.request" type="textarea" :autosize="{ minRows: 4, maxRows: 10 }" placeholder="到期后交给内置 Agent 执行的自然语言请求" /></NFormItem>
        <div v-if="form.mode === 'once'" class="form-grid-2"><NFormItem label="执行时间" required><input v-model="form.start_at" class="native-control" type="datetime-local"></NFormItem><NFormItem label="用户 ID"><NInput v-model:value="form.user_id" placeholder="默认 scheduler" /></NFormItem></div>
        <div v-else-if="form.mode === 'interval'" class="form-grid-2"><NFormItem label="间隔秒数" required><NInputNumber v-model:value="form.interval_seconds" :min="1" style="width: 100%" /></NFormItem><NFormItem label="用户 ID"><NInput v-model:value="form.user_id" placeholder="默认 scheduler" /></NFormItem></div>
        <div v-else-if="['daily', 'weekly', 'monthly'].includes(form.mode)" class="form-grid-3"><NFormItem label="执行时间" required><NInput v-model:value="form.time_of_day" placeholder="HH:MM" /></NFormItem><NFormItem v-if="form.mode === 'weekly'" label="星期"><NSelect v-model:value="form.weekday" :options="weekdayOptions" /></NFormItem><NFormItem v-if="form.mode === 'monthly'" label="每月几日"><NInputNumber v-model:value="form.month_day" :min="1" :max="31" style="width: 100%" /></NFormItem><NFormItem label="用户 ID"><NInput v-model:value="form.user_id" placeholder="默认 scheduler" /></NFormItem></div>
        <div v-else-if="form.mode === 'custom'" class="form-grid-2"><NFormItem label="五段 Cron" required><NInput v-model:value="form.cron" placeholder="分钟 小时 日 月 星期" /></NFormItem><NFormItem label="用户 ID"><NInput v-model:value="form.user_id" placeholder="默认 scheduler" /></NFormItem></div>
        <div class="form-grid-3"><NFormItem label="复用对话（可选）"><NInput v-model:value="form.conversation_id" placeholder="为空则自动创建" /></NFormItem><NFormItem label="投递平台（可选）"><NSelect v-model:value="form.adapter_id" clearable :options="adapterOptions()" placeholder="只执行不投递" /></NFormItem><NFormItem label="投递 Chat ID"><NInput v-model:value="form.chat_id" placeholder="填写后投递结果" /></NFormItem></div>
      </NForm>
      <template #footer><div class="modal-footer"><NButton @click="showEditor = false">取消</NButton><NButton type="primary" :loading="saving" @click="save">保存</NButton></div></template>
    </NModal>
  </div>
</template>
