<script setup lang="ts">
import { RouterLink, useRoute } from 'vue-router'

defineProps<{ collapsed?: boolean }>()

const route = useRoute()

// 图标沿用参考图的多彩风格；分组只承担视觉分层，不额外引入层级导航。
const navGroups = [
  {
    label: '',
    items: [
      { name: 'status', label: '总览', icon: '📊' },
      { name: 'bots', label: '机器人', icon: '🤖' },
      { name: 'bot-commands', label: '指令', icon: '⌨️' },
      { name: 'providers', label: '模型供应商', icon: '✨' },
    ],
  },
  {
    label: '资源',
    items: [
      { name: 'memories', label: '长期记忆', icon: '🧠' },
      { name: 'remote-targets', label: '远程主机', icon: '🖥️' },
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
          <span class="nav-item-icon" aria-hidden="true">{{ item.icon }}</span>
          <span class="nav-item-label">{{ item.label }}</span>
        </RouterLink>
      </div>
    </nav>

    <div class="app-nav-footer">
      <RouterLink
        to="/config"
        class="nav-item"
        :class="{ active: route.name === 'config' }"
        title="配置文件"
      >
        <span class="nav-item-icon" aria-hidden="true">⚙️</span>
        <span class="nav-item-label">配置文件</span>
      </RouterLink>
    </div>
  </aside>
</template>
