<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { NAlert, NButton, NCard, NEmpty, NForm, NFormItem, NInput, NInputNumber, NModal, NSelect, NSpace, NSwitch, NTag, useMessage } from 'naive-ui'
import { artifactContentURL, cancelCommandRun, commandOutputDownloadURL, readCommandRuns, readLocalDirectories, readOperations, readRemoteDirectories, readWorkspaceFiles, request } from '@/api'
import { useAppStore } from '@/stores/app'
import { useRoute, useRouter } from 'vue-router'
import type { ArtifactRef, CommandRun, Conversation, DirectoryListing, Operation, TestResult, Workspace } from '@/types'

const store = useAppStore()
const message = useMessage()
const route = useRoute()
const router = useRouter()
const showForm = ref(false)
const showDirectoryPicker = ref(false)
const showFiles = ref(false)
const editingID = ref('')
const directoryPath = ref('')
const directoryParent = ref('')
const directoryEntries = ref<{ name: string; path: string; is_dir: boolean }[]>([])
const selectedWorkspace = ref<Workspace | null>(null)
const files = ref<{ name: string; path: string; is_dir: boolean; size?: number }[]>([])
const filePath = ref('.')
const operations = ref<Operation[]>([])
const commandRuns = ref<CommandRun[]>([])
const saving = ref(false)
const directoryMode = ref<'local' | 'remote'>('local')
const fileContent = ref('')
const fileContentPath = ref('')
const showFileContent = ref(false)
const searchQuery = ref('')
const searchMode = ref<'literal' | 'regex'>('literal')
const searchCaseInsensitive = ref(false)
const searchGlob = ref('')
const searchContextLines = ref(0)
const searchMaxHits = ref(100)
const searchMatches = ref<{ path: string; line: number; preview: string; context_before?: { line: number; text: string }[]; context_after?: { line: number; text: string }[] }[]>([])
const gitOutput = ref('')
const gitDiffOutput = ref('')
const gitLogOutput = ref('')
const directoryLoading = ref(false)
const testResult = ref<TestResult | null>(null)
const showDiff = ref(false)
const selectedDiff = ref('')
const selectedDiffTitle = ref('')
const workspaceUserID = 'webui-user'

const form = reactive({
  id: '', name: '', type: 'local', root_path: '', remote_target_id: '', host: '', port: 22, user: '', auth_type: 'agent', key_path: '', password: '', host_key_fingerprint: '', enabled: true,
})

const remoteTargetOptions = computed(() => store.remoteTargets.map((target) => ({
  label: `${target.name} · ${target.user}@${target.host}:${target.port}${target.status === 'ready' ? ' · 已确认' : ' · 待确认'}`,
  value: target.id,
})))
const workspaceTypeOptions = computed(() => {
  const options = [{ label: '本地项目', value: 'local' }, { label: '远程项目（复用 SSH 主机）', value: 'remote' }]
  if (form.type === 'ssh') options.push({ label: '旧版内嵌 SSH（兼容）', value: 'ssh' })
  return options
})

function resetForm(workspace?: Workspace) {
  Object.assign(form, workspace ? {
    id: workspace.id, name: workspace.name, type: workspace.type, root_path: workspace.root_path, remote_target_id: workspace.remote_target_id || '', host: workspace.host || '', port: workspace.port || 22,
    user: workspace.user || '', auth_type: workspace.auth_type || 'agent', key_path: '', password: '', host_key_fingerprint: workspace.host_key_fingerprint || '', enabled: workspace.enabled !== false,
  } : { id: '', name: '', type: 'local', root_path: '', remote_target_id: '', host: '', port: 22, user: '', auth_type: 'agent', key_path: '', password: '', host_key_fingerprint: '', enabled: true })
  testResult.value = null
}

function openCreate() {
  editingID.value = ''
  resetForm()
  showForm.value = true
}

function openEdit(workspace: Workspace) {
  editingID.value = workspace.id
  resetForm(workspace)
  showForm.value = true
}

function payload() {
  const value: Record<string, unknown> = {
    id: form.id.trim(), name: form.name.trim(), type: form.type, root_path: form.root_path.trim(), remote_target_id: form.remote_target_id, host: form.host.trim(), port: Number(form.port) || 22,
    user: form.user.trim(), auth_type: form.auth_type, key_path: form.key_path.trim(), host_key_fingerprint: form.host_key_fingerprint.trim(), enabled: form.enabled,
  }
  if (form.password) value.password = form.password
  return value
}

async function save() {
  saving.value = true
  try {
    const workspace = await request<Workspace>(`/api/v1/workspaces${editingID.value ? `/${encodeURIComponent(editingID.value)}` : ''}`, { method: editingID.value ? 'PUT' : 'POST', body: JSON.stringify(payload()) })
    await store.reloadWorkspaces()
    showForm.value = false
    message.success(editingID.value ? '项目已保存' : '项目已创建')
    if (!editingID.value) router.push({ name: 'workspaces', query: { project: workspace.id } })
  } catch (error) {
    message.error(error instanceof Error ? error.message : '保存项目失败')
  } finally {
    saving.value = false
  }
}

async function test(workspace?: Workspace) {
  try {
    const result = workspace
      ? await request<TestResult>(`/api/v1/workspaces/${encodeURIComponent(workspace.id)}/test`, { method: 'POST', body: '{}' })
      : await request<TestResult>('/api/v1/workspaces/preview/test', { method: 'POST', body: JSON.stringify(payload()) })
    testResult.value = result
    if (result.fingerprint) form.host_key_fingerprint = result.fingerprint
    if (result.ok) message.success(result.message || '目录/连接正常')
    else message.warning(result.message || '连接可达，但还需要确认主机指纹')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '目录/连接测试失败')
  }
}

async function remove(workspace: Workspace) {
  if (!window.confirm(`确认删除项目“${workspace.name}”？项目下仍有对话时不能删除。`)) return
  try {
    await request(`/api/v1/workspaces/${encodeURIComponent(workspace.id)}`, { method: 'DELETE' })
    await store.reloadWorkspaces()
    message.success('项目已删除')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '删除项目失败')
  }
}

async function createConversation(workspace: Workspace) {
  try {
    const conversation = await request<{ id: string }>(`/api/v1/workspaces/${encodeURIComponent(workspace.id)}/conversations`, { method: 'POST', body: JSON.stringify({ user_id: 'webui-user', title: `${workspace.name} 对话` }) })
    await store.reloadConversations()
    router.push({ name: 'chat', query: { conversation: conversation.id } })
  } catch (error) {
    message.error(error instanceof Error ? error.message : '创建项目对话失败')
  }
}

function workspaceConversations(workspaceID: string) {
  return store.conversations.filter((item) => item.workspace_id === workspaceID)
}

function openConversation(conversation: Conversation) {
  router.push({ name: 'chat', query: { conversation: conversation.id } })
}

async function openFiles(workspace: Workspace, path = '.') {
  selectedWorkspace.value = workspace
  filePath.value = path
  searchMatches.value = []
  gitOutput.value = ''
  gitDiffOutput.value = ''
  gitLogOutput.value = ''
  showFiles.value = true
  try {
    files.value = await readWorkspaceFiles(workspace.id, path) as { name: string; path: string; is_dir: boolean; size?: number }[]
  } catch (error) {
    files.value = []
    message.error(error instanceof Error ? error.message : '读取项目文件失败')
  }
}

async function readFile(file: { path: string; is_dir: boolean }) {
  if (!selectedWorkspace.value) return
  if (file.is_dir) {
    await openFiles(selectedWorkspace.value, file.path)
    return
  }
  try {
    const result = await request<{ content: string }>(`/api/v1/workspaces/${encodeURIComponent(selectedWorkspace.value.id)}/file?path=${encodeURIComponent(file.path)}`)
    fileContent.value = result.content
    fileContentPath.value = file.path
    showFileContent.value = true
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取文件失败')
  }
}

async function loadDirectories(path = directoryPath.value) {
  directoryLoading.value = true
  try {
    const listing = directoryMode.value === 'remote'
      ? await readRemoteDirectories(form.remote_target_id, path)
      : await readLocalDirectories(path)
    const result = 'directories' in listing
      ? { path: listing.path, parent: listing.parent, entries: listing.directories }
      : listing
    directoryPath.value = result.path
    directoryParent.value = result.parent
    directoryEntries.value = result.entries
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取目录失败')
  } finally {
    directoryLoading.value = false
  }
}

function openPicker() {
  directoryMode.value = form.type === 'remote' ? 'remote' : 'local'
  directoryPath.value = form.root_path || (directoryMode.value === 'remote' ? '/' : '')
  if (directoryMode.value === 'remote' && !form.remote_target_id) {
    message.warning('请先选择远程主机')
    return
  }
  showDirectoryPicker.value = true
  void loadDirectories(directoryPath.value)
}

function chooseDirectory(path: string) {
  form.root_path = path
  showDirectoryPicker.value = false
}

async function loadOperations() {
  try {
    operations.value = await readOperations()
    commandRuns.value = await readCommandRuns()
  } catch {
    operations.value = []
    commandRuns.value = []
  }
}

function commandRunFor(operation: Operation) {
  if (!operation.command_run_id) return undefined
  return commandRuns.value.find((run) => run.id === operation.command_run_id)
}

function commandRunActive(operation: Operation) {
  const run = commandRunFor(operation)
  return run && (run.status === 'queued' || run.status === 'starting' || run.status === 'running')
}

function commandCapabilitySummary(operation: Operation) {
  const capabilities = commandRunFor(operation)?.capabilities
  if (!capabilities) return ''
  const isolation = capabilities.isolation_level === 'l0_host_process'
    ? 'L0 未隔离'
    : capabilities.isolation_level === 'ssh_account'
      ? 'SSH 账号边界'
      : capabilities.isolation_level
  const network = capabilities.network.state === 'unsupported' ? '网络未限制' : `网络 ${capabilities.network.state}`
  return `${isolation} · ${network}`
}

function commandCapabilityTooltip(operation: Operation) {
  const capabilities = commandRunFor(operation)?.capabilities
  if (!capabilities) return ''
  return [capabilities.filesystem.detail, capabilities.network.detail, capabilities.process_control.detail, capabilities.credentials.detail, capabilities.reattach.detail].filter(Boolean).join('；')
}

async function cancelCommand(operation: Operation) {
  const run = commandRunFor(operation)
  if (!run) return
  try {
    await cancelCommandRun(run.id)
    await loadOperations()
    message.success('取消请求已记录')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '取消命令失败')
  }
}

function downloadCommand(operation: Operation) {
  const run = commandRunFor(operation)
  if (!run) return
  if (run.output_retention_state === 'purged') {
    message.warning('完整日志已过保留期，仅保留命令摘要')
    return
  }
  const anchor = document.createElement('a')
  anchor.href = run.output_artifact?.id ? artifactContentURL(run.output_artifact.id, workspaceUserID) : commandOutputDownloadURL(run.id, 'text')
  anchor.download = run.output_artifact?.name || `command-${run.id}.log`
  anchor.rel = 'noopener'
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
}

function artifactDownloadURL(ref?: ArtifactRef) {
  return ref?.id ? artifactContentURL(ref.id, workspaceUserID) : ''
}

async function decide(operation: Operation, approve: boolean) {
  try {
    await request(`/api/v1/workspace-operations/${encodeURIComponent(operation.id)}/${approve ? 'approve' : 'reject'}`, { method: 'POST', body: '{}' })
    await loadOperations()
    message.success(approve ? '操作已执行' : '操作已拒绝')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '处理操作失败')
  }
}

async function searchWorkspace() {
  if (!selectedWorkspace.value || !searchQuery.value.trim()) return
  try {
    const params = new URLSearchParams({ path: filePath.value, q: searchQuery.value.trim(), mode: searchMode.value, case_insensitive: searchCaseInsensitive.value ? 'true' : 'false', max_hits: String(Math.max(1, Math.min(100, Number(searchMaxHits.value) || 100))) })
    if (searchGlob.value.trim()) params.set('glob', searchGlob.value.trim())
    if (searchContextLines.value > 0) params.set('context_lines', String(Math.min(5, Math.max(0, Number(searchContextLines.value) || 0))))
    const result = await request<{ matches: { path: string; line: number; preview: string; context_before?: { line: number; text: string }[]; context_after?: { line: number; text: string }[] }[] }>(`/api/v1/workspaces/${encodeURIComponent(selectedWorkspace.value.id)}/search?${params.toString()}`)
    searchMatches.value = result.matches || []
    if (!searchMatches.value.length) message.info('没有找到匹配内容')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '搜索项目失败')
  }
}

async function loadGitStatus() {
  if (!selectedWorkspace.value) return
  try {
    const result = await request<{ output: string }>(`/api/v1/workspaces/${encodeURIComponent(selectedWorkspace.value.id)}/git/status`)
    gitOutput.value = result.output || '工作区干净'
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取 Git 状态失败')
  }
}

async function loadGitDiff() {
  if (!selectedWorkspace.value) return
  try {
    const result = await request<{ output: string }>(`/api/v1/workspaces/${encodeURIComponent(selectedWorkspace.value.id)}/git/diff`)
    gitDiffOutput.value = result.output || '当前没有未提交差异'
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取 Git diff 失败')
  }
}

async function loadGitLog() {
  if (!selectedWorkspace.value) return
  try {
    const result = await request<{ output: string }>(`/api/v1/workspaces/${encodeURIComponent(selectedWorkspace.value.id)}/git/log`)
    gitLogOutput.value = result.output || '当前项目还没有提交记录'
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取 Git 日志失败')
  }
}

function openOperationDiff(operation: Operation) {
  if (!operation.diff) return
  selectedDiff.value = operation.diff
  selectedDiffTitle.value = `${operationLabel(operation.type)} · ${operation.path || operation.cwd || operation.id}`
  showDiff.value = true
}

function operationLabel(type: string) {
  const labels: Record<string, string> = {
    write_file: '写入文件',
    make_directory: '新建目录',
    patch_file: '应用补丁',
    delete_path: '删除路径',
    execute_command: '执行命令',
  }
  return labels[type] || '工作区操作'
}

function openSearchMatch(match: { path: string }) {
  if (selectedWorkspace.value) void readFile({ path: match.path, is_dir: false })
}

watch(() => form.type, (type) => {
  if (type === 'remote' && form.root_path && !form.root_path.startsWith('/')) form.root_path = ''
})

watch(() => route.query.create, (value) => {
  if (value === '1') openCreate()
}, { immediate: true })

onMounted(() => {
  void loadOperations()
})
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div><p class="eyebrow">PROJECTS</p><h2>项目</h2><p>每个项目绑定一个由你明确选择的本机或远程目录，并可以包含多个独立对话。</p></div>
      <NButton type="primary" size="large" @click="openCreate">＋ 创建项目</NButton>
    </div>

    <NAlert v-if="!store.workspaces.length" type="info" :show-icon="false" class="empty-panel">尚未创建项目。项目目录不会自动生成，创建时必须由你明确指定源文件夹。</NAlert>
    <div class="workspace-grid">
      <NCard v-for="workspace in store.workspaces" :key="workspace.id" class="workspace-card" hoverable>
        <template #header><div class="workspace-card-title"><span class="workspace-icon">▱</span><div><strong>{{ workspace.name }}</strong><span>{{ workspace.id }}</span></div></div></template>
        <template #header-extra><NTag round size="small" :type="workspace.status === 'ready' ? 'success' : workspace.status === 'error' ? 'error' : 'warning'">{{ workspace.status === 'ready' ? '已连接' : workspace.status === 'error' ? '连接失败' : '待检查' }}</NTag></template>
        <div class="workspace-root"><span>{{ workspace.type === 'remote' ? `远程 · ${store.remoteTargets.find((item) => item.id === workspace.remote_target_id)?.name || 'SSH'}` : workspace.type === 'ssh' ? '旧版 SSH · ' : '本地 · ' }}</span>{{ workspace.root_path }}</div>
        <p v-if="workspace.status_message" class="muted">{{ workspace.status_message }}</p>
        <div class="workspace-conversations">
          <div class="workspace-conversations-heading"><span>对话</span><NTag size="small" :bordered="false">{{ workspaceConversations(workspace.id).length }}</NTag></div>
          <button v-for="conversation in workspaceConversations(workspace.id)" :key="conversation.id" type="button" class="workspace-conversation-link" :class="{ archived: conversation.status === 'archived' }" @click="openConversation(conversation)">
            <span class="tree-dot" /><span>{{ conversation.title || '新对话' }}</span>
          </button>
          <span v-if="!workspaceConversations(workspace.id).length" class="muted">还没有对话，点击“新建对话”开始。</span>
        </div>
        <template #footer><NSpace wrap :size="8"><NButton size="small" secondary @click="createConversation(workspace)">＋ 新建对话</NButton><NButton size="small" secondary @click="openFiles(workspace)">文件</NButton><NButton size="small" secondary @click="test(workspace)">测试</NButton><NButton size="small" secondary @click="openEdit(workspace)">编辑</NButton><NButton size="small" tertiary type="error" @click="remove(workspace)">删除</NButton></NSpace></template>
      </NCard>
    </div>

    <NCard class="operations-card" :bordered="false">
      <template #header><div class="section-heading-row"><div><h3>待审批与最近操作</h3><p>Agent 的文件变更和命令操作仍需用户明确批准。</p></div><NButton secondary @click="loadOperations">刷新</NButton></div></template>
      <NEmpty v-if="!operations.length" description="暂无操作" />
      <div v-for="operation in operations" :key="operation.id" class="operation-row"><div><strong>{{ operationLabel(operation.type) }}</strong><span>{{ operation.path || operation.cwd || '.' }}</span><code>{{ operation.command || operation.preview || operation.path || '' }}</code><p v-if="operation.error" class="error-text">{{ operation.error }}</p><p v-if="operation.artifact_error" class="error-text">{{ operation.artifact_error }}</p></div><NSpace align="center"><NTag size="small">{{ operation.status }}</NTag><NTag v-if="commandRunFor(operation)" size="small" :type="commandRunFor(operation)?.status === 'exited' && commandRunFor(operation)?.outcome === 'success' ? 'success' : commandRunFor(operation)?.status === 'unknown' ? 'error' : 'warning'">命令 {{ commandRunFor(operation)?.status }}<template v-if="commandRunFor(operation)?.outcome"> · {{ commandRunFor(operation)?.outcome }}</template></NTag><NTag v-if="commandCapabilitySummary(operation)" size="small" type="warning" :title="commandCapabilityTooltip(operation)">{{ commandCapabilitySummary(operation) }}</NTag><NTag v-if="commandRunFor(operation)?.output_retention_state === 'purged'" size="small" type="default">完整日志已过期</NTag><NButton v-if="commandRunFor(operation)" size="small" tertiary :disabled="commandRunFor(operation)?.output_retention_state === 'purged' && !commandRunFor(operation)?.output_artifact" @click="downloadCommand(operation)">{{ commandRunFor(operation)?.output_artifact ? '下载 Artifact 日志' : commandRunFor(operation)?.output_retention_state === 'purged' ? '日志已过期' : '下载日志' }}</NButton><NButton v-if="commandRunActive(operation)" size="small" tertiary type="warning" @click="cancelCommand(operation)">取消命令</NButton><NButton v-if="operation.diff" size="small" secondary @click="openOperationDiff(operation)">查看 diff</NButton><NButton v-if="operation.diff_artifact" tag="a" size="small" secondary :href="artifactDownloadURL(operation.diff_artifact)" :download="operation.diff_artifact.name || `diff-${operation.id}.patch`" target="_blank" rel="noopener">下载 Artifact diff</NButton><template v-if="operation.status === 'pending' || operation.status === 'prepared'"><NButton size="small" type="primary" @click="decide(operation, true)">批准</NButton><NButton size="small" tertiary type="error" @click="decide(operation, false)">拒绝</NButton></template></NSpace></div>
    </NCard>

    <NModal v-model:show="showForm" preset="card" style="width: min(760px, calc(100vw - 32px))" :title="editingID ? '编辑项目' : '创建项目'" :mask-closable="false">
      <NForm label-placement="top" :show-feedback="false">
        <NFormItem label="项目名称" required><NInput v-model:value="form.name" placeholder="例如 Abot" /></NFormItem>
        <NFormItem label="源文件夹" required><NSpace class="full-width" :wrap="false"><NInput v-model:value="form.root_path" placeholder="必须由你明确指定绝对路径" /><NButton secondary @click="openPicker">选择目录</NButton></NSpace></NFormItem>
        <div class="form-grid-2"><NFormItem label="项目类型"><NSelect v-model:value="form.type" :options="workspaceTypeOptions" /><small class="form-help">远程项目引用已确认的远程主机；不会把凭据复制到项目中。</small></NFormItem><NFormItem label="启用项目"><NSwitch v-model:value="form.enabled" /></NFormItem></div>
        <template v-if="form.type === 'remote'"><NFormItem label="远程主机" required><NSelect v-model:value="form.remote_target_id" :options="remoteTargetOptions" placeholder="先在远程主机页面测试并确认指纹" /><small class="form-help">远程主机可以被多个项目复用。若列表为空，请先添加远程主机。</small></NFormItem></template>
        <template v-if="form.type === 'ssh'"><div class="form-grid-2"><NFormItem label="SSH 主机"><NInput v-model:value="form.host" placeholder="例如 192.168.1.20" /></NFormItem><NFormItem label="端口"><NInputNumber v-model:value="form.port" :min="1" :max="65535" :show-button="false" /></NFormItem><NFormItem label="SSH 用户"><NInput v-model:value="form.user" placeholder="例如 dev" /></NFormItem><NFormItem label="认证方式"><NSelect v-model:value="form.auth_type" :options="[{ label: 'ssh-agent', value: 'agent' }, { label: '私钥文件', value: 'key-file' }, { label: '密码', value: 'password' }]" /></NFormItem></div><NFormItem v-if="form.auth_type === 'key-file'" label="本机私钥路径"><NInput v-model:value="form.key_path" placeholder="例如 /Users/me/.ssh/id_ed25519" /></NFormItem><NFormItem v-if="form.auth_type === 'password'" label="SSH 密码"><NInput v-model:value="form.password" type="password" show-password-on="click" placeholder="编辑时留空表示保留原密码" /></NFormItem><NFormItem label="主机指纹"><NInput v-model:value="form.host_key_fingerprint" placeholder="测试连接后核对并保存" /></NFormItem></template>
      </NForm>
      <NAlert v-if="testResult && !testResult.ok" type="warning" :show-icon="false" class="form-tip">{{ testResult.message }}<template v-if="testResult.fingerprint"> 实际指纹：<code>{{ testResult.fingerprint }}</code></template></NAlert>
      <template #footer><div class="modal-footer"><NButton @click="showForm = false">取消</NButton><NSpace><NButton secondary @click="test()">测试目录/连接</NButton><NButton type="primary" :loading="saving" @click="save">{{ editingID ? '保存项目' : '创建项目' }}</NButton></NSpace></div></template>
    </NModal>

    <NModal v-model:show="showDirectoryPicker" preset="card" style="width: min(760px, calc(100vw - 32px))" title="选择项目文件夹">
      <NSpace align="center" :wrap="false" class="directory-toolbar"><NInput v-model:value="directoryPath" placeholder="输入绝对路径" @keyup.enter="loadDirectories()" /><NButton secondary :loading="directoryLoading" @click="loadDirectories()">前往</NButton><NButton secondary :disabled="!directoryParent || directoryLoading" @click="loadDirectories(directoryParent)">上级</NButton></NSpace>
      <div class="directory-list"><button v-for="entry in directoryEntries" :key="entry.path" type="button" class="directory-entry" @click="entry.is_dir ? loadDirectories(entry.path) : undefined"><span>▱</span><span>{{ entry.name }}</span><NButton v-if="entry.is_dir" size="tiny" type="primary" @click.stop="chooseDirectory(entry.path)">选择</NButton></button><span v-if="!directoryEntries.length" class="muted">当前目录没有可进入的子目录。</span></div>
      <template #footer><div class="modal-footer"><NButton @click="showDirectoryPicker = false">取消</NButton><NButton type="primary" :disabled="!directoryPath" @click="chooseDirectory(directoryPath)">选择当前目录</NButton></div></template>
    </NModal>

    <NModal v-model:show="showFiles" preset="card" style="width: min(900px, calc(100vw - 32px))" :title="`${selectedWorkspace?.name || '项目'} · 文件`">
      <div class="file-browser-toolbar"><code>{{ filePath }}</code><NSpace><NInput v-model:value="searchQuery" size="small" :placeholder="searchMode === 'regex' ? '正则搜索项目内容' : '搜索项目内容'" @keyup.enter="searchWorkspace" /><NSelect v-model:value="searchMode" size="small" :options="[{ label: '原文', value: 'literal' }, { label: '正则', value: 'regex' }]" style="width: 76px" /><NInput v-model:value="searchGlob" size="small" placeholder="glob（可选）" style="width: 150px" /><NSwitch v-model:value="searchCaseInsensitive" size="small" /><span class="muted">忽略大小写</span><NInputNumber v-model:value="searchContextLines" size="small" :min="0" :max="5" :show-button="false" placeholder="上下文" style="width: 76px" /><NInputNumber v-model:value="searchMaxHits" size="small" :min="1" :max="100" :show-button="false" style="width: 76px" /><NButton size="small" secondary @click="searchWorkspace">搜索</NButton><NButton size="small" secondary @click="loadGitStatus">Git 状态</NButton><NButton size="small" secondary @click="loadGitDiff">Git diff</NButton><NButton size="small" secondary @click="loadGitLog">Git log</NButton><NButton size="small" secondary @click="selectedWorkspace && openFiles(selectedWorkspace, '.')">回到根目录</NButton></NSpace></div>
      <div class="file-browser-list"><button v-for="file in files" :key="file.path" type="button" class="file-entry" @click="readFile(file)"><span>{{ file.is_dir ? '▱' : '·' }}</span><strong>{{ file.name }}</strong><small>{{ file.is_dir ? '目录' : `${file.size || 0} bytes` }}</small></button><NEmpty v-if="!files.length" description="目录为空" /></div>
      <div v-if="searchMatches.length" class="workspace-result-panel"><strong>搜索结果（{{ searchMatches.length }}）</strong><button v-for="match in searchMatches" :key="`${match.path}:${match.line}`" type="button" class="workspace-result-row" @click="openSearchMatch(match)"><code>{{ match.path }}:{{ match.line }}</code><span>{{ match.preview }}</span></button></div>
      <div v-if="gitOutput" class="workspace-result-panel"><strong>Git 状态</strong><pre>{{ gitOutput }}</pre></div>
      <div v-if="gitDiffOutput" class="workspace-result-panel"><strong>Git diff</strong><pre>{{ gitDiffOutput }}</pre></div>
      <div v-if="gitLogOutput" class="workspace-result-panel"><strong>Git log</strong><pre>{{ gitLogOutput }}</pre></div>
    </NModal>

    <NModal v-model:show="showFileContent" preset="card" style="width: min(900px, calc(100vw - 32px))" :title="fileContentPath || '文件内容'">
      <NInput :value="fileContent" type="textarea" readonly :autosize="{ minRows: 16, maxRows: 30 }" class="file-content-viewer" />
    </NModal>

    <NModal v-model:show="showDiff" preset="card" style="width: min(980px, calc(100vw - 32px))" :title="selectedDiffTitle || '变更 diff'">
      <pre class="diff-viewer">{{ selectedDiff }}</pre>
    </NModal>
  </div>
</template>
