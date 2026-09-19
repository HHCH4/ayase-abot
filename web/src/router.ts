import { createRouter, createWebHistory } from 'vue-router'
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/status' },
    { path: '/status', name: 'status', component: () => import('@/views/StatusView.vue'), meta: { title: '总览', eyebrow: 'SYSTEM OVERVIEW' } },
    { path: '/bots', name: 'bots', component: () => import('@/views/BotsView.vue'), meta: { title: '机器人平台', eyebrow: 'PLATFORM ADAPTERS' } },
    { path: '/bot-commands', name: 'bot-commands', component: () => import('@/views/BotCommandsView.vue'), meta: { title: '指令', eyebrow: 'COMMAND CENTER' } },
    { path: '/providers', name: 'providers', component: () => import('@/views/ProvidersView.vue'), meta: { title: '模型供应商', eyebrow: 'MODEL POOL' } },
    { path: '/config', name: 'config', component: () => import('@/views/ConfigView.vue'), meta: { title: '配置文件', eyebrow: 'CONFIG CENTER' } },
    { path: '/personas', name: 'personas', component: () => import('@/views/PersonasView.vue'), meta: { title: '人格设定', eyebrow: 'PERSONA CATALOG' } },
    { path: '/persona', redirect: '/personas' },
    { path: '/data', name: 'data', component: () => import('@/views/DataView.vue'), meta: { title: '数据与日志', eyebrow: 'DATA & OBSERVABILITY' } },
    { path: '/session-management', name: 'session-management', component: () => import('@/views/SessionManagementView.vue'), meta: { title: '自定义规则', eyebrow: 'SESSION RULES' } },
    { path: '/cron', name: 'cron', component: () => import('@/views/CronView.vue'), meta: { title: '未来任务', eyebrow: 'SCHEDULED TASKS' } },
    { path: '/memories', name: 'memories', component: () => import('@/views/MemoriesView.vue'), meta: { title: '长期记忆', eyebrow: 'LONG-TERM MEMORY' } },
    { path: '/workspaces', name: 'workspaces', component: () => import('@/views/WorkspacesView.vue'), meta: { title: '项目', eyebrow: 'PROJECTS' } },
    { path: '/remote-targets', name: 'remote-targets', component: () => import('@/views/RemoteTargetsView.vue'), meta: { title: '远程主机', eyebrow: 'REMOTE TARGETS' } },
    { path: '/chat', name: 'chat', component: () => import('@/views/ChatView.vue'), meta: { title: 'chat', eyebrow: 'AGENT CORE' } },
  ],
})
