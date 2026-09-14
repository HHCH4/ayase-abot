<script setup lang="ts">
import { computed, onBeforeMount } from 'vue'
import { RouterView, useRouter } from 'vue-router'
import { NButton, NConfigProvider, NLayout, NLayoutContent, NLayoutHeader, NLayoutSider, NMessageProvider, NSpace, NTag, lightTheme, type GlobalThemeOverrides } from 'naive-ui'
import GlobalNav from '@/components/GlobalNav.vue'
import Sidebar from '@/components/Sidebar.vue'
import { useAppStore } from '@/stores/app'

const router = useRouter()
const store = useAppStore()

const themeOverrides: GlobalThemeOverrides = {
  common: {
    primaryColor: '#147d6f',
    primaryColorHover: '#0f6b60',
    primaryColorPressed: '#0b574e',
    borderRadius: '12px',
    borderRadiusSmall: '9px',
    fontFamily: 'Inter, -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif',
  },
  Button: {
    borderRadiusMedium: '10px',
    borderRadiusSmall: '9px',
    fontWeight: '600',
  },
  Card: {
    borderRadius: '18px',
  },
  Input: {
    borderRadius: '10px',
    color: '#ffffff',
  },
  Select: {
    peers: {
      InternalSelection: {
        borderRadius: '10px',
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
      <NLayout class="app-layout">
        <NLayoutHeader bordered class="global-header">
          <GlobalNav />
          <NSpace align="center" :size="14">
            <NButton v-if="router.currentRoute.value.name !== 'chat'" text class="chat-entry" aria-label="chat" @click="router.push({ name: 'chat' })">chat</NButton>
            <NTag round :bordered="false" :type="store.lastError ? 'warning' : 'info'" class="status-tag">{{ statusText }}</NTag>
          </NSpace>
        </NLayoutHeader>
        <NLayout has-sider class="body-layout">
          <NLayoutSider bordered :width="248" :collapsed-width="0" class="context-sider">
            <Sidebar />
          </NLayoutSider>
          <NLayoutContent class="app-content">
            <RouterView />
          </NLayoutContent>
        </NLayout>
      </NLayout>
    </NMessageProvider>
  </NConfigProvider>
</template>
