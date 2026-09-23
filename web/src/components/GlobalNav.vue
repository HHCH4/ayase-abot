<script setup lang="ts">
import { RouterLink, useRoute } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'

defineProps<{ collapsed?: boolean }>()

const route = useRoute()

// 使用统一 SVG 图标，避免 Emoji 在不同系统字体下出现尺寸和颜色不一致。
const navGroups = [
  {
    label: '',
    items: [
      { name: 'status', label: '总览', icon: 'dashboard' },
      { name: 'bots', label: '机器人', icon: 'bot' },
      { name: 'bot-commands', label: '指令', icon: 'terminal' },
      { name: 'providers', label: '模型供应商', icon: 'sparkles' },
      { name: 'web-search', label: '网页搜索', icon: 'search' },
      { name: 'config', label: '配置文件', icon: 'settings' },
    ],
  },
  {
    label: '资源',
    items: [
      { name: 'memories', label: '长期记忆', icon: 'book' },
      { name: 'remote-targets', label: '远程主机', icon: 'monitor' },
      { name: 'personas', label: '人格设定', icon: 'users' },
      { name: 'data', label: '数据与日志', icon: 'chart' },
      { name: 'session-management', label: '自定义规则', icon: 'rules' },
      { name: 'cron', label: '未来任务', icon: 'calendar' },
    ],
  },
]
</script>

<template>
  <aside class="app-nav" :class="{ collapsed: Boolean(collapsed) }">
    <nav class="app-nav-scroll" aria-label="主导航">
      <div v-for="(group, index) in navGroups" :key="index" class="nav-group">
        <span v-if="group.label" class="nav-group-label">{{ group.label }}</span>
        <RouterLink
          v-for="item in group.items"
          :key="item.name"
          :to="{ name: item.name }"
          class="nav-item"
          :class="{ active: route.name === item.name }"
          :title="item.label"
        >
          <span class="nav-item-icon"><AppIcon :name="item.icon" :size="17" /></span>
          <span class="nav-item-label">{{ item.label }}</span>
        </RouterLink>
      </div>
    </nav>
  </aside>
</template>
