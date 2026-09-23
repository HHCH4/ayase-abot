import { defineStore } from 'pinia'
import { request } from '@/api'
import type { ConfigProfile, ConfigSchema, Conversation, Provider, SystemSettingsResponse, Workspace, Bot, RemoteTarget } from '@/types'

let pendingLoad: Promise<void> | null = null

export const useAppStore = defineStore('app', {
  state: () => ({
    providers: [] as Provider[],
    bots: [] as Bot[],
    workspaces: [] as Workspace[],
    remoteTargets: [] as RemoteTarget[],
    conversations: [] as Conversation[],
    defaults: {} as { provider_id?: string; model_id?: string },
    configSchema: { version: 1, fields: [] } as ConfigSchema,
    configProfiles: [] as ConfigProfile[],
    defaultProfileID: '',
    systemSettings: {
      log_level: 'info', request_timeout_seconds: 300,
      artifact_quota_bytes: 4 * 1024 * 1024 * 1024, artifact_stale_upload_seconds: 86400, artifact_input_retention_seconds: 7 * 86400,
      web_search_daily_call_limit: 100, web_search_max_calls_per_invocation: 8, web_search_alert_percent: 80,
      modal_fallback_enabled: false, modal_fallback_provider_id: '', modal_fallback_vision_model: '', modal_fallback_audio_model: '',
      subagent_enabled: true,
      subagent: {},
    } as SystemSettingsResponse,
    loading: false,
    lastError: '',
    initialized: false,
  }),
  getters: {
    defaultProfile: (state) => state.configProfiles.find((item) => item.id === state.defaultProfileID) || state.configProfiles.find((item) => item.is_default),
    enabledProviders: (state) => state.providers.filter((item) => item.status !== 'error'),
  },
  actions: {
    // 首屏加载只组织领域数据，页面组件不直接互相依赖，便于后续继续拆分管理页。
    async loadAll() {
      if (this.initialized) return
      if (pendingLoad) return pendingLoad
      pendingLoad = (async () => {
        this.loading = true
        this.lastError = ''
        try {
          const [providers, settings, bots, workspaces, remoteTargets, conversations] = await Promise.all([
            request<{ providers: Provider[]; default: { provider_id?: string; model_id?: string } }>('/api/v1/providers'),
            request<{ default?: { provider_id?: string; model_id?: string } }>('/api/v1/settings'),
            request<{ bots: Bot[] }>('/api/v1/bots'),
            request<{ workspaces: Workspace[] }>('/api/v1/workspaces'),
            request<{ remote_targets: RemoteTarget[] }>('/api/v1/remote-targets'),
            request<{ conversations: Conversation[] }>('/api/v1/conversations?user_id=webui-user&include_archived=true'),
          ])
          this.providers = providers.providers || []
          this.defaults = providers.default || settings.default || {}
          this.bots = bots.bots || []
          this.workspaces = workspaces.workspaces || []
          this.remoteTargets = remoteTargets.remote_targets || []
          this.conversations = conversations.conversations || []
          try {
            await this.loadConfig()
          } catch (error) {
            // 配置中心兼容旧服务，配置接口暂不可用时不阻塞供应商和聊天页面。
            this.lastError = error instanceof Error ? error.message : '配置中心加载失败'
          }
          this.initialized = true
        } catch (error) {
          // 首屏数据失败时保留空状态和可诊断错误，避免页面因单个接口异常白屏。
          this.lastError = error instanceof Error ? error.message : 'API 加载失败'
        } finally {
          this.loading = false
        }
      })()
      try {
        await pendingLoad
      } finally {
        pendingLoad = null
      }
    },
    async loadConfig() {
      const [schema, profiles, system] = await Promise.all([
        request<ConfigSchema>('/api/v1/config/schema'),
        request<{ profiles: ConfigProfile[]; default_profile_id: string }>('/api/v1/config-profiles'),
        request<SystemSettingsResponse>('/api/v1/system-settings'),
      ])
      this.configSchema = schema
      this.configProfiles = profiles.profiles || []
      this.defaultProfileID = profiles.default_profile_id || this.configProfiles.find((item) => item.is_default)?.id || ''
      this.systemSettings = system
    },
    async reloadConversations(userID = 'webui-user') {
      const result = await request<{ conversations: Conversation[] }>(`/api/v1/conversations?user_id=${encodeURIComponent(userID)}&include_archived=true`)
      this.conversations = result.conversations || []
    },
    async reloadProviders() {
      const result = await request<{ providers: Provider[]; default: { provider_id?: string; model_id?: string } }>('/api/v1/providers')
      this.providers = result.providers || []
      this.defaults = result.default || {}
    },
    async reloadBots() {
      const result = await request<{ bots: Bot[] }>('/api/v1/bots')
      this.bots = result.bots || []
    },
    async reloadWorkspaces() {
      const result = await request<{ workspaces: Workspace[] }>('/api/v1/workspaces')
      this.workspaces = result.workspaces || []
    },
    async reloadRemoteTargets() {
      const result = await request<{ remote_targets: RemoteTarget[] }>('/api/v1/remote-targets')
      this.remoteTargets = result.remote_targets || []
    },
  },
})
