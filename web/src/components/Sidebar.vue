<script setup lang="ts">
import { computed, reactive } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NScrollbar, useMessage } from 'naive-ui'
import { request } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Conversation, Workspace } from '@/types'

const store = useAppStore()
const route = useRoute()
const router = useRouter()
const message = useMessage()

const workspaces = computed(() => store.workspaces)
const normalConversations = computed(() => store.conversations.filter((item) => !item.workspace_id && item.status === 'active'))
const archivedConversations = computed(() => store.conversations.filter((item) => !item.workspace_id && item.status === 'archived'))
const expandedProjects = reactive<Record<string, boolean>>({})

function projectConversations(workspace: Workspace, archived = false) {
  return store.conversations.filter((item) => item.workspace_id === workspace.id && (archived ? item.status === 'archived' : item.status === 'active'))
}

function isExpanded(workspace: Workspace) {
  return expandedProjects[workspace.id] !== false
}

function toggleProject(workspace: Workspace) {
  expandedProjects[workspace.id] = !isExpanded(workspace)
}

async function createProjectConversation(workspace: Workspace) {
  try {
    const item = await request<Conversation>(`/api/v1/workspaces/${encodeURIComponent(workspace.id)}/conversations`, {
      method: 'POST',
      body: JSON.stringify({ user_id: 'webui-user', title: `${workspace.name} 对话` }),
    })
    await store.reloadConversations()
    router.push({ name: 'chat', query: { conversation: item.id } })
  } catch (error) {
    message.error(error instanceof Error ? error.message : '创建项目对话失败')
  }
}

function isCurrentConversation(item: Conversation) {
  return route.name === 'chat' && route.query.conversation === item.id
}

function openChat(item?: Conversation) {
  router.push({ name: 'chat', query: item ? { conversation: item.id } : undefined })
}

function openNewProject() {
  router.push({ name: 'workspaces', query: { create: '1' } })
}
</script>

<template>
  <div class="project-rail">
    <div class="project-rail-heading">
      <div>
        <span class="eyebrow">CONTEXT</span>
        <strong>项目空间</strong>
      </div>
      <NButton quaternary size="small" aria-label="创建项目" @click="openNewProject">＋</NButton>
    </div>
    <p class="project-rail-help">项目目录由你指定，对话在项目下独立保存。</p>

    <NScrollbar class="project-scroll">
      <!-- 项目树只展示用户已经创建的项目；不会生成默认项目或默认目录。 -->
      <div v-if="!workspaces.length" class="tree-empty">尚未创建项目</div>
      <div v-for="workspace in workspaces" :key="workspace.id" class="tree-project">
        <div class="tree-project-head">
          <button type="button" class="tree-project-toggle" :aria-label="`${isExpanded(workspace) ? '收起' : '展开'}${workspace.name}`" @click="toggleProject(workspace)">
            <span class="tree-chevron">{{ isExpanded(workspace) ? '⌄' : '›' }}</span>
          </button>
          <button type="button" class="tree-project-name" @click="router.push({ name: 'workspaces', query: { project: workspace.id } })">
            <span class="tree-folder">▱</span>
            <span class="tree-label">{{ workspace.name }}</span>
          </button>
          <button type="button" class="tree-project-add" :aria-label="`在${workspace.name}中新建对话`" @click="createProjectConversation(workspace)">＋</button>
        </div>
        <div v-if="isExpanded(workspace)" class="tree-children">
          <button
            v-for="conversation in projectConversations(workspace)"
            :key="conversation.id"
            type="button"
            class="tree-conversation"
            :class="{ active: isCurrentConversation(conversation), archived: conversation.status === 'archived' }"
            @click="openChat(conversation)"
          >
            <span class="tree-dot" />
            <span class="tree-label">{{ conversation.title || '新对话' }}</span>
          </button>
          <span v-if="!projectConversations(workspace).length" class="tree-child-empty">暂无进行中的对话</span>
          <span v-if="projectConversations(workspace, true).length" class="tree-group-label tree-archived-label">已归档</span>
          <button
            v-for="conversation in projectConversations(workspace, true)"
            :key="conversation.id"
            type="button"
            class="tree-conversation archived"
            :class="{ active: isCurrentConversation(conversation) }"
            @click="openChat(conversation)"
          >
            <span class="tree-dot" />
            <span class="tree-label">{{ conversation.title || '新对话' }}</span>
          </button>
        </div>
      </div>

      <div v-if="normalConversations.length" class="tree-normal">
        <span class="tree-group-label">未归属项目</span>
        <button
          v-for="conversation in normalConversations"
          :key="conversation.id"
          type="button"
          class="tree-conversation"
          :class="{ active: isCurrentConversation(conversation), archived: conversation.status === 'archived' }"
          @click="openChat(conversation)"
        >
          <span class="tree-dot" />
          <span class="tree-label">{{ conversation.title || '新对话' }}</span>
        </button>
      </div>
      <div v-if="archivedConversations.length" class="tree-normal tree-archived-normal">
        <span class="tree-group-label">已归档</span>
        <button
          v-for="conversation in archivedConversations"
          :key="conversation.id"
          type="button"
          class="tree-conversation archived"
          :class="{ active: isCurrentConversation(conversation) }"
          @click="openChat(conversation)"
        >
          <span class="tree-dot" />
          <span class="tree-label">{{ conversation.title || '新对话' }}</span>
        </button>
      </div>
    </NScrollbar>

    <div class="project-rail-footer">
      <NButton text size="small" @click="router.push({ name: 'workspaces' })">管理全部项目 →</NButton>
    </div>
  </div>
</template>
