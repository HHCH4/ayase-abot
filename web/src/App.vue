<script setup lang="ts">
import { computed, onBeforeMount, ref } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'
import { NConfigProvider, NMessageProvider, lightTheme, type GlobalThemeOverrides } from 'naive-ui'
import GlobalNav from '@/components/GlobalNav.vue'
import { useAppStore } from '@/stores/app'

const route = useRoute()
const store = useAppStore()
const navCollapsed = ref(false)
const appVersion = __APP_VERSION__
// chat 是脱离管理台壳层的独立界面，只有它自己一套顶栏与侧栏。
const isChatRoute = computed(() => route.name === 'chat')

const themeOverrides: GlobalThemeOverrides = {
  common: {
    primaryColor: '#2f6fed',
    primaryColorHover: '#2861d8',
    primaryColorPressed: '#2559c9',
    primaryColorSuppl: '#2861d8',
    borderRadius: '9px',
    borderRadiusSmall: '7px',
    fontFamily: 'Inter, -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif',
    textColorBase: '#1f2329',
  },
  Button: {
    borderRadiusMedium: '9px',
    borderRadiusSmall: '8px',
    fontWeight: '600',
  },
  Card: {
    borderRadius: '14px',
  },
  Input: {
    borderRadius: '9px',
    color: '#ffffff',
  },
  Select: {
    peers: {
      InternalSelection: {
        borderRadius: '9px',
      },
    },
  },
}

const statusText = computed(() => {
  if (store.loading) return '同步中'
  if (store.lastError) return '配置需检查'
  return `${store.providers.length} 供应商 · ${store.workspaces.length} 项目`
})

onBeforeMount(() => {
  void store.loadAll()
})
</script>

<template>
  <NConfigProvider :theme="lightTheme" :theme-overrides="themeOverrides">
    <NMessageProvider>
      <div v-if="isChatRoute" class="chat-shell">
        <RouterView />
      </div>

      <div v-else class="app-shell">
        <header class="top-bar">
          <div class="top-bar-brand">
            <button
              type="button"
              class="top-bar-toggle"
              :aria-label="navCollapsed ? '展开导航' : '收起导航'"
              :aria-expanded="!navCollapsed"
              @click="navCollapsed = !navCollapsed"
            >☰</button>
            <RouterLink to="/status" class="top-brand">
              <span class="top-brand-name">Abot</span>
              <span class="top-brand-version">v{{ appVersion }}</span>
            </RouterLink>
          </div>
          <div class="top-bar-actions">
            <span class="top-bar-status" :class="{ warning: Boolean(store.lastError) }">{{ statusText }}</span>
            <RouterLink to="/chat" class="top-chat-entry">
              <span class="top-chat-entry-icon" aria-hidden="true">💬</span>
              <span>chat</span>
            </RouterLink>
          </div>
        </header>

        <div class="app-body">
          <GlobalNav :collapsed="navCollapsed" />
          <main class="app-content">
            <RouterView />
          </main>
        </div>
      </div>
    </NMessageProvider>
  </NConfigProvider>
</template>
