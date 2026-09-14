<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { NAlert, NButton, NCard, NTag, useMessage } from 'naive-ui'
import { request } from '@/api'
import type { StatusSummary } from '@/types'
import { RouterLink } from 'vue-router'

const message = useMessage()
const loading = ref(false)
const summary = ref<StatusSummary | null>(null)

async function refresh() {
  loading.value = true
  try {
    summary.value = await request<StatusSummary>('/api/v1/status')
  } catch (error) {
    message.error(error instanceof Error ? error.message : '读取系统状态失败')
  } finally {
    loading.value = false
  }
}

onMounted(() => void refresh())
</script>

<template>
  <div class="page-view">
    <div class="page-intro">
      <div>
        <p class="eyebrow">SYSTEM OVERVIEW</p>
        <h2>总览</h2>
        <p>从这里查看供应商、机器人、项目和远程主机的当前状态，具体配置仍在各自页面维护。</p>
      </div>
      <NButton secondary :loading="loading" @click="refresh">刷新状态</NButton>
    </div>

    <NAlert v-if="!summary" type="info" :show-icon="false" class="empty-panel">正在读取系统状态。</NAlert>
    <div v-else class="status-grid">
      <NCard class="status-card" :bordered="false"><span class="status-card-label">模型供应商</span><strong>{{ summary.ready_providers }} / {{ summary.providers }}</strong><span>连接正常</span><NTag size="small" :type="summary.ready_providers ? 'success' : 'warning'">{{ summary.ready_providers ? '可用' : '待配置' }}</NTag></NCard>
      <NCard class="status-card" :bordered="false"><span class="status-card-label">机器人</span><strong>{{ summary.running_bots }} / {{ summary.bots }}</strong><span>运行中</span><NTag size="small" :type="summary.running_bots ? 'success' : 'warning'">{{ summary.running_bots ? '运行中' : '未启动' }}</NTag></NCard>
      <NCard class="status-card" :bordered="false"><span class="status-card-label">项目工作区</span><strong>{{ summary.ready_workspaces }} / {{ summary.workspaces }}</strong><span>目录可用</span><NTag size="small" :type="summary.ready_workspaces ? 'success' : 'warning'">{{ summary.ready_workspaces ? '可使用' : '待检查' }}</NTag></NCard>
      <NCard class="status-card" :bordered="false"><span class="status-card-label">远程主机</span><strong>{{ summary.ready_remote_targets }} / {{ summary.remote_targets }}</strong><span>指纹已确认</span><NTag size="small" :type="summary.ready_remote_targets ? 'success' : 'warning'">{{ summary.ready_remote_targets ? '可连接' : '待确认' }}</NTag></NCard>
      <NCard class="status-card" :bordered="false"><span class="status-card-label">会话</span><strong>{{ summary.active_conversations ?? 0 }} / {{ summary.conversations ?? 0 }}</strong><span>活跃 / 全部保留</span><NTag size="small" type="info">可管理</NTag></NCard>
    </div>

    <NCard class="overview-guide" :bordered="false">
      <template #header><div class="section-heading-row"><div><h3>下一步</h3><p>按依赖顺序完成基础配置，Agent 才能稳定工作。</p></div></div></template>
      <div class="overview-guide-grid">
        <RouterLink to="/providers" class="overview-guide-item"><strong>1. 配置供应商</strong><span>添加兼容 OpenAI Chat / Responses 的模型接口。</span></RouterLink>
        <RouterLink to="/config" class="overview-guide-item"><strong>2. 选择配置</strong><span>设置默认模型、人格、上下文和项目能力。</span></RouterLink>
        <RouterLink to="/workspaces" class="overview-guide-item"><strong>3. 创建项目</strong><span>明确指定本机目录或已确认远程主机上的目录。</span></RouterLink>
        <RouterLink to="/chat" class="overview-guide-item"><strong>4. 开始 chat</strong><span>从全局右上角进入 chat，按需绑定项目工作区。</span></RouterLink>
      </div>
    </NCard>
  </div>
</template>
