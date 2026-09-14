import { createRouter, createWebHistory } from 'vue-router'
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/status' },
    { path: '/status', name: 'status', component: () => import('@/views/StatusView.vue'), meta: { title: '总览', eyebrow: 'SYSTEM OVERVIEW' } },
    { path: '/bots', name: 'bots', component: () => import('@/views/BotsView.vue'), meta: { title: '机器人平台', eyebrow: 'PLATFORM ADAPTERS' } },
    { path: '/providers', name: 'providers', component: () => import('@/views/ProvidersView.vue'), meta: { title: '模型供应商', eyebrow: 'MODEL POOL' } },
    { path: '/config', name: 'config', component: () => import('@/views/ConfigView.vue'), meta: { title: '配置文件', eyebrow: 'CONFIG CENTER' } },
    { path: '/memories', name: 'memories', component: () => import('@/views/MemoriesView.vue'), meta: { title: '长期记忆', eyebrow: 'LONG-TERM MEMORY' } },
    { path: '/workspaces', name: 'workspaces', component: () => import('@/views/WorkspacesView.vue'), meta: { title: '项目', eyebrow: 'PROJECTS' } },
    { path: '/remote-targets', name: 'remote-targets', component: () => import('@/views/RemoteTargetsView.vue'), meta: { title: '远程主机', eyebrow: 'REMOTE TARGETS' } },
    { path: '/chat', name: 'chat', component: () => import('@/views/ChatView.vue'), meta: { title: 'chat', eyebrow: 'AGENT CORE' } },
  ],
})
