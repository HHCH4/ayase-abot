<script setup lang="ts">
import { RouterLink, useRoute } from 'vue-router'

const route = useRoute()

const navItems = [
  { name: 'status', label: '总览' },
  { name: 'bots', label: '机器人' },
  { name: 'providers', label: '模型供应商' },
  { name: 'config', label: '配置中心' },
  { name: 'memories', label: '长期记忆' },
  { name: 'workspaces', label: '项目工作区' },
  { name: 'remote-targets', label: '远程主机' },
]
</script>

<template>
  <div class="global-nav">
    <RouterLink to="/bots" class="global-brand" aria-label="返回机器人工作台">
      <span class="brand-mark">A</span>
      <span class="global-brand-copy">
        <strong>Abot</strong>
        <small>AI WORKSPACE</small>
      </span>
    </RouterLink>

    <nav class="global-nav-links" aria-label="主导航">
      <RouterLink
        v-for="item in navItems"
        :key="item.name"
        :to="{ name: item.name }"
        class="global-nav-link"
        :class="{ active: route.name === item.name }"
      >
        {{ item.label }}
      </RouterLink>
    </nav>
  </div>
</template>
