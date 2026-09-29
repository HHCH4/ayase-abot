package sqlite

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/artifact"
	"Abot/internal/bot"
	"Abot/internal/config"
	"Abot/internal/conversation"
	"Abot/internal/memory"
	"Abot/internal/provider"
	"Abot/internal/schedule"
	"Abot/internal/sessionrule"
	"Abot/internal/websearch"
	"Abot/internal/workspace"
	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	adksessiondb "google.golang.org/adk/v2/session/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store 持有 Abot 业务表和 ADK 会话服务使用的 SQLite 数据库。
type Store struct {
	db             *gorm.DB
	sessionService session.Service
}

// Open 创建数据目录、迁移供应商表和 ADK 会话表。
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = "./data"
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	dbPath := filepath.Join(dataDir, "abot.db")
	// 业务仓储与 ADK 会话共用同一个连接池，避免同一进程的独立写连接互相争锁。
	// 锁等待参数必须写入 DSN，才能覆盖连接池以后新建的每条 SQLite 连接。
	dsn := dbPath + "?_pragma=busy_timeout(15000)&_txlock=immediate"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: newDatabaseLogger()})
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("取得 SQLite 连接池失败: %w", err)
	}
	// 树莓派内存有限；单连接既限制连接数，也让业务写入和会话事件顺序执行。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	// WAL 允许其他进程的读取与写入并行，并在所有迁移开始前统一数据库日志模式。
	var journalMode string
	if err := db.Raw("PRAGMA journal_mode=WAL").Scan(&journalMode).Error; err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("启用 SQLite WAL 失败: %w", err)
	}
	if journalMode != "wal" {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("启用 SQLite WAL 失败: 当前模式 %q", journalMode)
	}
	backupCreated, err := backupRuntimeMigrationStateIfNeeded(db, dbPath)
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	// 这次破坏性架构切换删除独立子 Agent 编排表及其历史；新执行记录只属于父 Invocation。
	if err := db.Migrator().DropTable("abot_agent_subagent_evidence", "abot_agent_subagent_runs", "abot_agent_subagent_groups"); err != nil {
		return nil, fmt.Errorf("删除旧子 Agent 历史表失败: %w", err)
	}
	// 所有新增的管理台数据都纳入同一次迁移，保证旧数据目录升级后仍可直接启动。
	if err := db.AutoMigrate(&providerRow{}, &modelRow{}, &capabilityObservationRow{}, &settingRow{}, &workspaceRow{}, &workspaceOperationRow{}, &workspaceCommandRunRow{}, &workspaceCommandOutputChunkRow{}, &remoteTargetRow{}, &conversationRow{}, &botRow{}, &botSourceNameRow{}, &botMessageSourceRow{}, &configProfileRow{}, &configRevisionRow{}, &configBindingRow{}, &systemSettingsRow{}, &personaRow{}, &personaRevisionRow{}, &personaBindingRow{}, &memoryRow{}, &invocationRow{}, &invocationResumeRow{}, &invocationResumeOutboxRow{}, &worktreeBaselineRow{}, &agentEventRow{}, &runtimeEventOutboxRow{}, &runtimeEventDeliveryInboxRow{}, &runtimeEventDeliveryTransactionRow{}, &runtimeCheckpointDeliveryInboxRow{}, &runtimeCheckpointDeliveryOutboxRow{}, &runtimeCheckpointDeliveryTransactionRow{}, &runtimeApprovalRejectionDeliveryOutboxRow{}, &runtimeApprovalRejectionDeliveryInboxRow{}, &runtimeApprovalRejectionDeliveryTransactionRow{}, &runtimeConfigDeliveryInboxRow{}, &runtimeConfigDeliveryOutboxRow{}, &runtimeConfigDeliveryTransactionRow{}, &runtimeConfigDirectoryRow{}, &runtimeConfigDirectoryOutboxRow{}, &runtimeConfigDirectoryFanoutRow{}, &runtimeConfigDirectoryRebindPlanRow{}, &runtimeConfigDirectoryRebindConfirmationRow{}, &runtimeConfigDirectoryRebindApplyRow{}, &runtimeConfigDirectoryRebindMultiConfirmationRow{}, &runtimeConfigDirectoryRebindMultiApplyRow{}, &runtimeConfigDirectoryRebindInboxRow{}, &runtimeDeliveryAttemptRow{}, &runtimeDeliveryGroupRow{}, &runtimeDeliveryGroupTransactionRow{}, &runtimeDeliveryGroupFenceRow{}, &runtimeDeliveryGroupSettlementRow{}, &runtimeDeliveryGroupSagaRow{}, &runtimeDeliveryCompensationRow{}, &approvalRow{}, &toolCallRow{}, &taskPlanRow{}, &taskContractRow{}, &instructionSnapshotSetRow{}, &contextManifestRow{}, &workingSetRow{}, &verificationRunRow{}, &runtimeSnapshotRow{}, &toolSetSnapshotRow{}, &modelCapabilitySnapshotRow{}, &evalRunRow{}, &artifactRow{}, &artifactObjectDeletionRow{}, &botGroupAdminRow{}, &botCommandPolicyRow{}, &botCommandAuditRow{}, &botSourceNameRow{}, &botMessageSourceRow{}, &botRuntimeInboxRow{}, &botRuntimeContextRow{}, &botAdminRouteRow{}, &botDeleteConfirmationRow{}, &botApprovalMessageBindingRow{}, &sessionRuleRow{}, &sessionRuleGroupRow{}, &scheduledTaskRow{}, &webSearchServiceRow{}, &webSearchUsageRow{}); err != nil {
		return nil, fmt.Errorf("迁移 Abot 表失败: %w", err)
	}
	if err := db.AutoMigrate(&botInstanceStateRow{}, &botRuntimeSourceRow{}, &botRelationRow{}, &botRuntimeTurnRow{}, &botRuntimeTurnEventRow{}, &botRuntimeDecisionRow{}, &botPlatformEventRow{}, &botActionPlanRow{}, &botActionRow{}, &botApprovalDeliveryRow{}, &subAgentGroupRow{}, &subAgentRunRow{}); err != nil {
		return nil, fmt.Errorf("迁移 Bot Runtime 状态表失败: %w", err)
	}
	if !backupCreated {
		backupCreated, err = backupRuntimeMigrationStateIfNeeded(db, dbPath)
		if err != nil {
			_ = sqlDB.Close()
			return nil, err
		}
	}
	// 审批引用必须按 Bot、平台和聊天联合唯一；旧版本仅按 message_id
	// 唯一会导致不同 Bot 的相同平台消息 ID 无法建立映射。
	if db.Migrator().HasIndex(&botApprovalMessageBindingRow{}, "idx_bot_approval_message_ref") {
		if err := db.Migrator().DropIndex(&botApprovalMessageBindingRow{}, "idx_bot_approval_message_ref"); err != nil {
			return nil, fmt.Errorf("迁移审批引用联合索引失败: %w", err)
		}
	}
	if err := migrateLegacyScheduledTasks(db); err != nil {
		return nil, err
	}
	// 已经存在于 Follow-up 表中的旧 active 状态也一次性转换，避免升级后
	// 同一张表同时出现两套生命周期枚举。
	if err := db.Model(&scheduledTaskRow{}).Where("status = ?", "active").Update("status", string(schedule.StatusScheduled)).Error; err != nil {
		return nil, fmt.Errorf("迁移 Follow-up active 状态失败: %w", err)
	}
	// Inbox 正文消费后已经清空；定期删除旧占位行和行为决策，避免常驻树莓派数据库无限增长。
	if err := db.Where("status = ? AND updated_at < ?", "processed", time.Now().UTC().AddDate(0, 0, -7)).Delete(&botRuntimeInboxRow{}).Error; err != nil {
		return nil, fmt.Errorf("清理 Bot Runtime Inbox 失败: %w", err)
	}
	if err := db.Where("created_at < ?", time.Now().UTC().AddDate(0, 0, -90)).Delete(&botRuntimeDecisionRow{}).Error; err != nil {
		return nil, fmt.Errorf("清理 Bot Runtime 行为决策失败: %w", err)
	}
	if err := db.Where("received_at < ?", time.Now().UTC().AddDate(0, 0, -90)).Delete(&botPlatformEventRow{}).Error; err != nil {
		return nil, fmt.Errorf("清理 Bot Runtime 平台事件失败: %w", err)
	}
	if err := db.Where("created_at < ?", time.Now().UTC().AddDate(0, 0, -90)).Delete(&webSearchUsageRow{}).Error; err != nil {
		return nil, fmt.Errorf("清理过期网页搜索用量失败: %w", err)
	}
	// 清除旧编排器写入的子任务/检索事件，避免新运行时把不再支持的历史类型显示成悬空事件。
	if err := db.Where("type LIKE ? OR type LIKE ?", "subagent.%", "retrieval.%").Delete(&runtimeEventOutboxRow{}).Error; err != nil {
		return nil, fmt.Errorf("删除旧子 Agent 事件投递记录失败: %w", err)
	}
	if err := db.Where("type LIKE ? OR type LIKE ?", "subagent.%", "retrieval.%").Delete(&agentEventRow{}).Error; err != nil {
		return nil, fmt.Errorf("删除旧子 Agent 事件历史失败: %w", err)
	}
	// 尚处于旧子 Agent 等待状态的 Invocation 没有可移植 checkpoint，升级时终结为失败，避免永久占用会话队列。
	if err := db.Model(&invocationRow{}).Where("status = ?", "waiting_subagents").Updates(map[string]any{"status": "failed", "error": "子 Agent 架构已切换；旧任务无可恢复 checkpoint"}).Error; err != nil {
		return nil, fmt.Errorf("终结旧子 Agent 等待任务失败: %w", err)
	}

	// ADK 保留自己的 GORM 实例，但复用业务仓储的连接池，避免重新打开独立写连接。
	// 继续传入同一个日志配置，防止正常的 "record not found" 被记录成错误。
	sessionService, err := adksessiondb.NewSessionService(sqlite.Dialector{Conn: sqlDB}, &gorm.Config{Logger: newDatabaseLogger()})
	if err != nil {
		return nil, fmt.Errorf("创建 ADK 会话服务失败: %w", err)
	}
	if err := adksessiondb.AutoMigrate(sessionService); err != nil {
		return nil, fmt.Errorf("迁移 ADK 会话表失败: %w", err)
	}
	return &Store{db: db, sessionService: sessionService}, nil
}

// backupRuntimeMigrationStateIfNeeded 在删除旧表、旧事件或旧状态前创建可直接
// 恢复的 SQLite 快照。只在确实存在破坏性迁移目标时执行，避免每次普通启动都
// 留下一份无意义的备份文件；备份失败则阻止启动，不能带着不可恢复的迁移继续。
func backupRuntimeMigrationStateIfNeeded(db *gorm.DB, dbPath string) (bool, error) {
	needed, err := runtimeMigrationNeedsBackup(db)
	if err != nil {
		return false, fmt.Errorf("检查 SQLite 破坏性迁移目标失败: %w", err)
	}
	if !needed {
		return false, nil
	}
	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		return false, fmt.Errorf("解析 SQLite 路径失败: %w", err)
	}
	backupPath := fmt.Sprintf("%s.pre-runtime-migration-%d.db", absDBPath, time.Now().UTC().UnixNano())
	quotedPath := strings.ReplaceAll(backupPath, "'", "''")
	if err := db.Exec("VACUUM INTO '" + quotedPath + "'").Error; err != nil {
		return false, fmt.Errorf("创建 SQLite 迁移备份失败: %w", err)
	}
	if err := os.Chmod(backupPath, 0o600); err != nil {
		return false, fmt.Errorf("设置 SQLite 迁移备份权限失败: %w", err)
	}
	stat, err := os.Stat(backupPath)
	if err != nil {
		return false, fmt.Errorf("检查 SQLite 迁移备份失败: %w", err)
	}
	if stat.Size() == 0 {
		return false, fmt.Errorf("SQLite 迁移备份为空: %s", backupPath)
	}
	log.Printf("已创建 SQLite 破坏性迁移备份: %s", backupPath)
	return true, nil
}

// runtimeMigrationNeedsBackup 汇总所有会丢弃历史或改变生命周期的入口。
// 查询只读取存在的表，兼容从旧版本直接升级到当前版本的数据目录。
func runtimeMigrationNeedsBackup(db *gorm.DB) (bool, error) {
	for _, table := range []string{
		"abot_agent_subagent_evidence",
		"abot_agent_subagent_runs",
		"abot_agent_subagent_groups",
		"abot_scheduled_tasks",
	} {
		if db.Migrator().HasTable(table) {
			return true, nil
		}
	}

	cutoffInbox := time.Now().UTC().AddDate(0, 0, -7)
	cutoffHistory := time.Now().UTC().AddDate(0, 0, -90)
	checks := []struct {
		table string
		query string
		args  []any
	}{
		{"abot_bot_follow_ups", "SELECT EXISTS (SELECT 1 FROM abot_bot_follow_ups WHERE status = 'active')", nil},
		{"abot_agent_event_outbox", "SELECT EXISTS (SELECT 1 FROM abot_agent_event_outbox WHERE type LIKE ? OR type LIKE ?)", []any{"subagent.%", "retrieval.%"}},
		{"abot_agent_events", "SELECT EXISTS (SELECT 1 FROM abot_agent_events WHERE type LIKE ? OR type LIKE ?)", []any{"subagent.%", "retrieval.%"}},
		{"abot_agent_invocations", "SELECT EXISTS (SELECT 1 FROM abot_agent_invocations WHERE status = 'waiting_subagents')", nil},
		{"abot_bot_runtime_inbox", "SELECT EXISTS (SELECT 1 FROM abot_bot_runtime_inbox WHERE status = 'processed' AND updated_at < ?)", []any{cutoffInbox}},
		{"abot_bot_runtime_decisions", "SELECT EXISTS (SELECT 1 FROM abot_bot_runtime_decisions WHERE created_at < ?)", []any{cutoffHistory}},
		{"abot_bot_platform_events", "SELECT EXISTS (SELECT 1 FROM abot_bot_platform_events WHERE received_at < ?)", []any{cutoffHistory}},
		{"abot_web_search_usage", "SELECT EXISTS (SELECT 1 FROM abot_web_search_usage WHERE created_at < ?)", []any{cutoffHistory}},
	}
	for _, check := range checks {
		if !db.Migrator().HasTable(check.table) {
			continue
		}
		var exists bool
		if err := db.Raw(check.query, check.args...).Scan(&exists).Error; err != nil {
			return false, fmt.Errorf("检查表 %s 失败: %w", check.table, err)
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

// RuntimeRepository 返回 Agent Runtime 的 Invocation/Event/Approval/TaskPlan 仓储。
// 保留在 Store 上是为了让应用装配继续通过同一 SQLite 数据库完成迁移。
var _ agentruntime.Repository = (*runtimeRepository)(nil)
var _ agentruntime.InvocationResumeRepository = (*runtimeRepository)(nil)
var _ agentruntime.AgentEventRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeEventOutboxRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeEventOutboxConsistencyReader = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeEventDeliveryInboxRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeEventDeliveryInboxReader = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeEventDeliveryTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeCheckpointDeliveryInbox = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeCheckpointDeliveryOutboxRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeCheckpointDeliveryOutboxDeferrer = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeCheckpointDeliveryTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeApprovalRejectionDeliveryOutboxRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeApprovalRejectionDeliveryInbox = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeApprovalRejectionDeliveryTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeApprovalRejectionDeliveryApplyRepository = (*runtimeRepository)(nil)
var _ agentruntime.ApprovalRejectionDeliveryCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigMigrationCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigMigrationDeliveryCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDeliveryInbox = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDeliveryTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDeliveryOutboxRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryOutboxRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryFanoutRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindPlanRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindConfirmationRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindMultiConfirmationRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindApplyRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindMultiApplyCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindMultiApplyRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindApplyReleaseRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindApplyCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryRebindInbox = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeConfigDirectoryMaterializer = (*configRepository)(nil)
var _ agentruntime.RuntimeDeliveryAttemptRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupByIDClaimer = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupDeferrer = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupFenceIssuer = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupFenceBinder = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupSettlementCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupTransactionRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupMemberPhaseResolver = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupSettlementRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryGroupSagaRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeDeliveryCompensationRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeSnapshotCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.RuntimeSnapshotCheckpointCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.InterruptedInvocationCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.ApprovalRejectionCommitRepository = (*runtimeRepository)(nil)
var _ agentruntime.WorkflowContinuationCommitRepository = (*runtimeRepository)(nil)

// ProviderRepository 返回供应商领域仓储接口。
func (s *Store) ProviderRepository() provider.Repository {
	return &providerRepository{db: s.db}
}

// BotRepository 返回平台机器人连接仓储。
func (s *Store) BotRepository() bot.Repository {
	return &botRepository{db: s.db}
}

// BotRuntimeRepository 返回平台机器人持久 Inbox、来源上下文和管理员路由仓储。
func (s *Store) BotRuntimeRepository() bot.RuntimeStateRepository {
	return &botRuntimeRepository{db: s.db}
}

// SubAgentRuntimeRepository 返回统一子 Agent Group/Run 的 SQLite 仓储，供
// Manager 保存队列、结果屏障和重启后的查询状态。
func (s *Store) SubAgentRuntimeRepository() agent.SubAgentRuntimeStore {
	return &botRuntimeRepository{db: s.db}
}

// WorkspaceRepository 返回工作区配置和操作审计仓储。
func (s *Store) WorkspaceRepository() workspace.Repository {
	return &workspaceRepository{db: s.db}
}

// RemoteTargetRepository 返回可复用远程主机配置仓储。
func (s *Store) RemoteTargetRepository() workspace.RemoteTargetRepository {
	return &remoteTargetRepository{db: s.db}
}

// ConversationRepository 返回对话元数据仓储。
func (s *Store) ConversationRepository() conversation.Repository {
	return &conversationRepository{db: s.db}
}

// ConfigRepository 返回配置文件、修订、绑定和系统设置仓储。
func (s *Store) ConfigRepository() config.Repository {
	return &configRepository{db: s.db}
}

// RuntimeConfigDirectoryMaterializer returns the explicit destination
// capability that applies an already accepted directory record to the real
// profile/persona catalog. It never changes defaults or bindings.
func (s *Store) RuntimeConfigDirectoryMaterializer() agentruntime.RuntimeConfigDirectoryMaterializer {
	if s == nil || s.db == nil {
		return nil
	}
	return &configRepository{db: s.db}
}

// PersonaRepository 返回独立人格目录、修订和选择绑定仓储。
func (s *Store) PersonaRepository() config.PersonaRepository {
	return &configRepository{db: s.db}
}

// SessionRuleRepository 返回按消息会话来源覆盖配置的规则仓储。
func (s *Store) SessionRuleRepository() sessionrule.Repository {
	return &sessionRuleRepository{db: s.db}
}

// ScheduleRepository 返回未来任务的持久化仓储。
func (s *Store) ScheduleRepository() schedule.Repository {
	return &scheduleRepository{db: s.db}
}

// WebSearchRepository 持久化搜索凭据和有界用量记录。
func (s *Store) WebSearchRepository() websearch.Repository {
	return &webSearchRepository{db: s.db}
}

// MemoryRepository 返回长期记忆仓储。
func (s *Store) MemoryRepository() memory.Repository {
	return &memoryRepository{db: s.db}
}

// ArtifactRepository 返回 Artifact 元数据仓储；内容本身由应用按数据目录装配对象存储。
// newDatabaseLogger keeps expected "record not found" lookups out of the log.
// Those lookups are normal control flow (first read of an optional row), and
// printing them as errors buries the failures that matter on an unattended
// single-board deployment. Slow queries and real driver errors are still shown.
func newDatabaseLogger() logger.Interface {
	return logger.New(log.New(os.Stdout, "", log.LstdFlags), logger.Config{
		SlowThreshold:             500 * time.Millisecond,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
		Colorful:                  false,
	})
}

func (s *Store) ArtifactRepository() artifact.Repository {
	return &artifactRepository{db: s.db}
}

// SessionService 返回共享的 ADK 持久化会话服务。
func (s *Store) SessionService() session.Service {
	return s.sessionService
}

// Close 关闭业务仓储和 ADK 会话服务共同使用的数据库连接池。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
