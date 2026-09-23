export type ConfigValues = Record<string, unknown>

export interface ProviderModel {
  id: string
  display_name: string
  enabled: boolean
  source?: string
  context_window?: number
  max_output_tokens?: number
  capabilities?: ModelCapabilityProfile
}

export interface ModelSupport {
  state: 'supported' | 'unsupported' | 'unknown' | 'degraded' | string
  source?: string
  confidence?: number
  observed_at?: string
  reason?: string
}

export interface TokenizerProfile {
  name: string
  version: string
  source: string
  quality: string
  known: boolean
  confidence: number
  calibration?: {
    samples: number
    estimated_tokens: number
    actual_tokens: number
    error_tokens: number
    absolute_error_tokens: number
    safety_multiplier: number
    source?: string
    updated_at?: string
  }
}

export interface ModelCapabilityProfile {
  provider_id: string
  model_id: string
  protocol: string
  route?: string
  tool_calling: ModelSupport
  parallel_tool_calls: ModelSupport
  structured_output: ModelSupport
  structured_output_schema: ModelSupport
  json_schema_dialect?: string
  streaming: ModelSupport
  images: ModelSupport
  input_files: ModelSupport
  audio: ModelSupport
  reasoning_effort: ModelSupport
  reasoning_summary: ModelSupport
  prompt_caching: ModelSupport
  native_compaction: ModelSupport
  tokenizer: TokenizerProfile
  usage_details?: string[]
  source_revision?: string
  updated_at?: string
}

export interface CapabilityObservation {
  feature: string
  state: string
  source?: string
  confidence?: number
  route?: string
  reason?: string
  observed_at?: string
}

export interface CapabilityObservationRecord extends CapabilityObservation {
  id: string
  provider_id: string
  model_id: string
  protocol: string
  created_at: string
}

export interface CapabilityProbeResult {
  profile: ModelCapabilityProfile
  observations?: CapabilityObservation[]
  message: string
  started_at: string
  completed_at: string
}

export interface Provider {
  id: string
  name: string
  base_url: string
  protocol: 'openai-compatible' | 'openai-completions' | 'gemini' | string
  openai_format?: 'auto' | 'chat' | 'responses' | string
  token_count_protocol?: 'anthropic' | string
  models: ProviderModel[]
  status: string
  status_message?: string
  api_key_configured?: boolean
  created_at?: string
  updated_at?: string
}

export interface Bot {
  id: string
  name: string
  type: 'telegram' | 'onebot11' | string
  endpoint?: string
  onebot_mode?: 'reverse-server' | 'client' | string
  listen_host?: string
  listen_port?: number
  listen_path?: string
  /** NapCat 容器附件目录映射到宿主机后的根路径。 */
  onebot_file_root?: string
  group_trigger_mode?: 'mention' | 'all' | string
  /** 聊天指令的全局管理员；只能在 WebUI 配置，聊天中无法授予。 */
  admin_user_ids?: string[]
  telegram_token_configured?: boolean
  onebot_access_token_configured?: boolean
  enabled: boolean
  status: string
  status_message?: string
  updated_at?: string
}

export interface Workspace {
  id: string
  name: string
  type: 'local' | 'remote' | 'ssh' | string
  root_path: string
  remote_target_id?: string
  host?: string
  port?: number
  user?: string
  auth_type?: string
  host_key_fingerprint?: string
  enabled: boolean
  status: string
  status_message?: string
  updated_at?: string
}

export interface RemoteTarget {
  id: string
  name: string
  transport: 'ssh' | string
  host: string
  port: number
  user: string
  auth_type: string
  key_path?: string
  password_configured: boolean
  host_key_fingerprint?: string
  enabled: boolean
  status: string
  status_message?: string
  last_checked_at?: string
  created_at?: string
  updated_at?: string
}

export interface TestResult {
  ok: boolean
  message: string
  fingerprint?: string
}

export interface DirectoryListing {
  path: string
  parent: string
  directories: { name: string; path: string; is_dir: boolean }[]
}

export interface StatusSummary {
  providers: number
  ready_providers: number
  workspaces: number
  ready_workspaces: number
  bots: number
  running_bots: number
	remote_targets: number
	ready_remote_targets: number
	conversations?: number
	active_conversations?: number
	default?: { provider_id?: string; model_id?: string }
}

export interface Conversation {
  id: string
  user_id: string
  source?: string
  source_name?: string
  platform?: string
  chat_type?: 'private' | 'group' | 'other' | string
  chat_id?: string
  workspace_id?: string
  subagent_enabled?: boolean | null
  title: string
  status: 'active' | 'archived' | string
  archived_at?: string
  created_at?: string
  updated_at?: string
}

export interface ConversationMessage {
  role: 'user' | 'assistant' | string
  text?: string
  attachment_count?: number
  timestamp?: string
}

export interface ConversationContextStatus {
  compressed: boolean
  compaction_count: number
  last_compaction_at?: string
  covered_from?: string
  covered_to?: string
}

export interface Invocation {
  id: string
  user_id: string
  conversation_id: string
  workspace_id?: string
  provider_id?: string
  model_id?: string
  config_snapshot_digest?: string
  status: string
  error?: string
  created_at?: string
  started_at?: string
  finished_at?: string
  updated_at?: string
}

export interface PlanStep {
  id: string
  title: string
  status: string
  evidence?: { kind: string; ref: string }[]
  blocker?: string
}

export interface TaskPlan {
  invocation_id: string
  revision: number
  status: string
  current_step_id?: string
  blocker?: string
  steps: PlanStep[]
  updated_at?: string
}

export interface CompletionCriterionResult {
  id: string
  description: string
  status: string
  evidence?: { kind: string; ref: string; summary?: string }[]
}

export interface WorktreePathStatus {
  path: string
  status: string
}

export interface WorktreeBaseline {
  invocation_id: string
  workspace_id?: string
  target_path?: string
  repository_type: string
  head_revision?: string
  branch?: string
  status_digest: string
  changed_paths?: WorktreePathStatus[]
  status_known: boolean
  truncated?: boolean
  revision: number
  captured_at?: string
}

export interface AgentChangePath {
  path: string
  operation_ids?: string[]
  operation_types?: string[]
  baseline_status?: string
  current_status?: string
}

export interface AgentChangeSet {
  status: string
  paths?: AgentChangePath[]
  overlap_paths?: string[]
  unattributed_paths?: string[]
  operation_ids?: string[]
  current_status_digest?: string
  current_status_known: boolean
  current_status_truncated?: boolean
}

export interface CompletionReport {
  invocation_id: string
  invocation_status: string
  status: string
  summary: string
  contract_version?: number
  plan_revision?: number
  criteria?: CompletionCriterionResult[]
  remaining_steps?: { id: string; title?: string }[]
  changed_operation_ids?: string[]
  baseline?: WorktreeBaseline
  preexisting_changes?: WorktreePathStatus[]
  agent_changes?: AgentChangeSet
  verification_runs?: { id: string; status?: string; summary?: string }[]
  pending_approvals?: { id: string; tool_name?: string; operation_id?: string }[]
  blockers?: { code?: string; message: string }[]
  unknown_states?: { code?: string; message: string }[]
  generated_at?: string
}

export interface RuntimeSnapshot {
  invocation_id: string
  revision: number
  phase: string
  workflow_phase?: string
  active_plan_step?: { id: string; title?: string }
  pending_approvals?: { id: string; tool_name?: string; operation_id?: string }[]
  verification_runs?: { id: string; status?: string; summary?: string }[]
  blockers?: { code?: string; message: string }[]
  unknown_states?: { code?: string; message: string }[]
  budget: { context_window?: number; estimated_input?: number; tool_calls_used?: number; tool_calls_limit?: number; budget_exhausted?: boolean }
  generated_at?: string
}

export interface VerificationRun {
  id: string
  invocation_id: string
  kind: string
  status: string
  summary?: string
  exit_code?: number
  output_digest?: string
  revision: number
  updated_at?: string
}

export interface TraceSpan {
  id: string
  parent_id?: string
  kind: string
  name: string
  start_at: string
  end_at?: string
  status?: string
  outcome?: string
  error_code?: string
  attributes?: Record<string, unknown>
}

export interface UsageValue {
  value?: number
  known: boolean
}

export interface UsageTotals {
  prompt_tokens: UsageValue
  cached_input_tokens: UsageValue
  tool_use_prompt_tokens: UsageValue
  output_tokens: UsageValue
  reasoning_tokens: UsageValue
  total_tokens: UsageValue
  model_calls: number
  unknown_calls: number
  unknown_fields?: string[]
  sources?: string[]
}

export interface InvocationUsage {
  prompt_tokens: UsageValue
  cached_input_tokens: UsageValue
  tool_use_prompt_tokens: UsageValue
  output_tokens: UsageValue
  reasoning_tokens: UsageValue
  total_tokens: UsageValue
  model_calls: number
  unknown_calls: number
  unknown_fields?: string[]
  sources?: string[]
  compaction: UsageTotals
}

export interface InvocationTrace {
  invocation_id: string
  status: string
  generated_at: string
  spans: TraceSpan[]
  metrics: {
    queue_ms: number
    active_runtime_ms: number
    model_ms: number
    tool_ms: number
    approval_wait_ms: number
    command_run_ms: number
    context_build_ms: number
    total_wall_ms: number
    model_calls: number
    tool_calls: number
    approval_requests: number
    verification_runs: number
    unknown_usage_calls: number
    compaction_model_calls: number
    compaction_unknown_usage_calls: number
    context_limit_retries: number
    context_estimate_error_tokens: number
  }
  usage: InvocationUsage
}

export interface SubAgentGroup {
  id: string
  invocation_id?: string
  profile: string
  purpose?: string
  status: string
  failure_policy?: string
  expected_count: number
  queued_count: number
  running_count: number
  completed_count: number
  failed_count: number
  cancelled_count: number
  max_concurrency?: number
  timeout_seconds?: number
  output_budget_bytes?: number
  result_digest?: string
  error_summary?: string
  created_at?: string
  started_at?: string
  finished_at?: string
  updated_at?: string
}

export interface SubAgentRun {
  id: string
  group_id: string
  ordinal: number
  profile: string
  source_kind?: string
  status: string
  attempt?: number
  result_text?: string
  result_digest?: string
  error_code?: string
  error?: string
  started_at?: string
  finished_at?: string
}

export interface SubAgentEvidence {
  evidence_id: string
  source_kind: string
  source_id?: string
  locator?: string
  title?: string
  excerpt?: string
  retrieval_score?: number
  rerank_score?: number
  citation?: string
  truncated?: boolean
  stale?: boolean
}

export interface SubAgentGroupDetail {
  group: SubAgentGroup
  runs: SubAgentRun[]
  evidence?: SubAgentEvidence[]
}

export interface ToolSetSnapshot {
  invocation_id: string
  digest: string
  tools: { id: string; model_name: string; version: string }[]
  excluded?: { id: string; reason: string }[]
}

export interface ModelCapabilitySnapshot {
  invocation_id: string
  result: {
    profile_snapshot_id: string
    compatible: boolean
    warnings?: string[]
    disabled_features?: { feature: string; reason: string }[]
    request_plan: { streaming: boolean; capture_usage: boolean; parallel_tool_limit?: number }
  }
}

export interface MemoryItem {
  id: string
  app_name?: string
  user_id: string
  conversation_id?: string
  author?: string
  source?: string
  content: string
  tags?: string
  created_at?: string
  updated_at?: string
}

export interface ConfigOption {
  value: string
  label: string
}

export interface ConfigField {
  key: string
  group: string
  label: string
  type: 'boolean' | 'integer' | 'number' | 'string' | 'textarea' | 'list' | 'select' | string
  default?: unknown
  required?: boolean
  min?: number
  max?: number
  options?: ConfigOption[]
  option_source?: string
  display_if?: Record<string, unknown>
  secret?: boolean
  restart_required?: boolean
  help?: string
}

export interface ConfigSchema {
  version: number
  fields: ConfigField[]
}

export interface ConfigProfile {
  id: string
  name: string
  revision: number
  values: ConfigValues
  is_default: boolean
  created_at?: string
  updated_at?: string
}

export interface ConfigRevision {
  profile_id: string
  revision: number
  values: ConfigValues
  created_at?: string
}

export interface Persona {
  id: string
  name: string
  description?: string
  instruction: string
  revision: number
  is_default: boolean
  enabled: boolean
  created_at?: string
  updated_at?: string
}

export interface PersonaRevision {
  persona_id: string
  revision: number
  name: string
  description?: string
  instruction: string
  enabled: boolean
  created_at?: string
}

export interface SessionRule {
  source: string
  process_enabled: boolean
  llm_enabled: boolean
  tts_enabled: boolean
  note?: string
  chat_model?: string
  stt_model?: string
  tts_model?: string
  follow_profile: boolean
  profile_id?: string
  persona_id?: string
  disabled_plugins?: string[]
  knowledge_bases?: string[]
  knowledge_top_k: number
  knowledge_rerank: boolean
  configured_fields?: string[]
  created_at?: string
  updated_at?: string
}

export interface SessionSource {
  source: string
  source_name?: string
  auto_name?: string
  user_id: string
  platform?: string
  message_type?: string
  session_id?: string
  status: string
  has_rule?: boolean
  first_seen_at?: string
  last_seen_at?: string
  updated_at?: string
}

export interface SessionRuleGroup {
  id: string
  name: string
  description?: string
  members: string[]
  created_at?: string
  updated_at?: string
}

export type ScheduledTaskMode = 'once' | 'interval' | 'daily' | 'weekly' | 'monthly' | 'custom' | string

export interface ScheduledTask {
  id: string
  name: string
  request: string
  mode: ScheduledTaskMode
  start_at?: string
  interval_seconds?: number
  weekday?: number
  month_day?: number
  time_of_day?: string
  cron?: string
  user_id: string
  conversation_id?: string
  adapter_id?: string
  chat_id?: string
  status: 'active' | 'paused' | 'completed' | string
  next_run_at?: string
  last_run_at?: string
  last_invocation_id?: string
  last_error?: string
  running: boolean
  created_at?: string
  updated_at?: string
}

export interface DashboardStats {
  range_days: number
  overview: {
    invocation_count: number
    message_count: number
    model_calls: number
    total_tokens: number
    success_count: number
    failed_count: number
    avg_response_ms: number
  }
  message_trend: { date: string; messages: number }[]
  model_ranking: { model: string; calls: number; tokens: number; success_rate: number }[]
  platform_instances: number
}

export interface DataLogEntry {
  time: string
  level: string
  message: string
  attributes?: Record<string, string>
}

export interface SystemSettings {
  log_level: string
  request_timeout_seconds: number
  artifact_quota_bytes: number
  artifact_stale_upload_seconds: number
  artifact_input_retention_seconds: number
  modal_fallback_enabled: boolean
  modal_fallback_provider_id: string
  modal_fallback_vision_model: string
  modal_fallback_audio_model: string
  subagent_enabled: boolean
  subagent_profiles: Record<string, SubAgentProfileSettings>
  subagent: SubAgentSettings
}

export interface SubAgentProfileSettings {
  provider_id?: string
  model_id?: string
  reasoning_effort?: string
  temperature?: number | null
  top_p?: number | null
  max_output_tokens?: number
  timeout_seconds?: number
  max_concurrency?: number
  output_budget_bytes?: number
  failure_policy?: string
}

export interface SubAgentSettings {
  provider_id?: string
  model_id?: string
  reasoning_effort?: string
  temperature?: number | null
  top_p?: number | null
  max_output_tokens?: number
  max_concurrency?: number
  input_budget_bytes?: number
  output_budget_bytes?: number
  allowed_tools?: string[]
}

export interface SubAgentProfileDescriptor {
  id: string
  label: string
  description: string
}

export interface SystemSettingsResponse extends SystemSettings {
  schema?: ConfigField[]
  subagent_profile_schema?: SubAgentProfileDescriptor[]
}

/**
 * ArtifactExtraction 是有界、仅元数据的提取结果。它不含对象正文，
 * warnings 会说明提取器做不到什么。
 */
export interface ArtifactExtraction {
  kind: string
  extractor: string
  version: string
  source_digest: string
  source_size: number
  preview?: string
  truncated?: boolean
  image?: { format: string; width?: number; height?: number }
  pdf?: { version?: string; pages?: number; encrypted?: boolean }
  archive?: {
    format: string
    entries: number
    total_uncompressed?: number
    truncated?: boolean
    names?: string[]
    suspicious_paths?: string[]
  }
  warnings?: string[]
  extracted_at: string
}

export interface ArtifactRef {
  id: string
  version: number
  digest: string
  kind: string
  mime_type: string
  size: number
  name?: string
  preview?: string
}

export interface Artifact extends ArtifactRef {
  user_id: string
  conversation_id?: string
  invocation_id?: string
  producer_type?: string
  producer_id?: string
  storage_key?: string
  security_class?: string
  status: string
  metadata?: Record<string, unknown>
  error?: string
  expires_at?: string
  created_at?: string
  updated_at?: string
}

export interface Operation {
  id: string
  workspace_id: string
  conversation_id?: string
  invocation_id?: string
  tool_call_id?: string
  command_run_id?: string
  type: string
  path?: string
  command?: string
  cwd?: string
  preview?: string
  diff?: string
  result?: string
  error?: string
  exit_code?: number
  command_outcome?: string
  output_digest?: string
  output_truncated?: boolean
  diff_artifact?: ArtifactRef
  output_artifact?: ArtifactRef
  artifact_error?: string
  duration_ms?: number
  timed_out?: boolean
  unknown?: boolean
  /** 重试操作指向的源操作 ID；只有人工重试才会设置。 */
  retry_of?: string
  status: string
  created_at?: string
}

export interface ExecutionCapability {
  state: 'supported' | 'partial' | 'unsupported' | 'unknown' | string
  detail?: string
}

export interface CommandExecutionCapabilities {
  profile_id: string
  profile_version: string
  executor: string
  isolation_level: string
  filesystem: ExecutionCapability
  network: ExecutionCapability
  process_control: ExecutionCapability
  resources: ExecutionCapability
  credentials: ExecutionCapability
  pty: ExecutionCapability
  reattach: ExecutionCapability
}

export interface CommandRun {
  id: string
  workspace_id: string
  conversation_id?: string
  invocation_id?: string
  tool_call_id?: string
  operation_id: string
  executor: string
  mode: string
  capabilities?: CommandExecutionCapabilities
  command_preview?: string
  cwd?: string
  timeout_ms?: number
  status: string
  outcome?: string
  exit_code?: number
  stdout_bytes?: number
  stderr_bytes?: number
  stored_bytes?: number
  output_digest?: string
  output_truncated?: boolean
  output_artifact?: ArtifactRef
  artifact_error?: string
  output_retention_state?: 'pending' | 'retained' | 'purged' | string
  output_retained_until?: string
  output_purged_at?: string
  error?: string
  revision: number
  queued_at?: string
  started_at?: string
  finished_at?: string
  updated_at?: string
}

export interface CommandOutputChunk {
  command_run_id: string
  sequence: number
  stream: string
  offset: number
  data: string
  byte_length: number
  captured_at: string
  truncated?: boolean
}

export interface CommandOutputPage {
  chunks: CommandOutputChunk[]
  next_after: number
  has_more: boolean
  truncated?: boolean
  retention_state?: 'pending' | 'retained' | 'purged' | string
}

export interface ChatAttachment {
  name: string
  mime_type: string
  data?: string
  size?: number
  artifact_id?: string
  artifact_version?: number
  artifact_digest?: string
  artifact_size?: number
}

/** BotCommand 是一条聊天指令在某个机器人上的生效策略。 */
export interface BotCommand {
  id: string
  name: string
  aliases?: string[]
  source: string
  source_name: string
  category: string
  description: string
  default_permission: string
  scope: string
  default_enabled: boolean
  effective_permission: string
  effective_enabled: boolean
  overridden: boolean
}

/** BotCommandAudit 记录一次影响权限或配置的动作。 */
export interface BotCommandAudit {
  id: string
  adapter_id: string
  chat_id?: string
  user_id?: string
  command?: string
  action: string
  target?: string
  result: string
  created_at: string
}
