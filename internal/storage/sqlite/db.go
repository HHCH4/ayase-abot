package sqlite

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/artifact"
	"Abot/internal/bot"
	"Abot/internal/config"
	"Abot/internal/conversation"
	"Abot/internal/memory"
	"Abot/internal/provider"
	"Abot/internal/schedule"
	"Abot/internal/sessionrule"
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
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: newDatabaseLogger()})
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// 这次破坏性架构切换删除独立子 Agent 编排表及其历史；新执行记录只属于父 Invocation。
	if err := db.Migrator().DropTable("abot_agent_subagent_evidence", "abot_agent_subagent_runs", "abot_agent_subagent_groups"); err != nil {
		return nil, fmt.Errorf("删除旧子 Agent 历史表失败: %w", err)
	}
	// 所有新增的管理台数据都纳入同一次迁移，保证旧数据目录升级后仍可直接启动。
	if err := db.AutoMigrate(&providerRow{}, &modelRow{}, &capabilityObservationRow{}, &settingRow{}, &workspaceRow{}, &workspaceOperationRow{}, &workspaceCommandRunRow{}, &workspaceCommandOutputChunkRow{}, &remoteTargetRow{}, &conversationRow{}, &botRow{}, &botSourceNameRow{}, &botMessageSourceRow{}, &configProfileRow{}, &configRevisionRow{}, &configBindingRow{}, &systemSettingsRow{}, &personaRow{}, &personaRevisionRow{}, &personaBindingRow{}, &memoryRow{}, &invocationRow{}, &invocationResumeRow{}, &invocationResumeOutboxRow{}, &worktreeBaselineRow{}, &agentEventRow{}, &runtimeEventOutboxRow{}, &runtimeEventDeliveryInboxRow{}, &runtimeEventDeliveryTransactionRow{}, &runtimeCheckpointDeliveryInboxRow{}, &runtimeCheckpointDeliveryOutboxRow{}, &runtimeCheckpointDeliveryTransactionRow{}, &runtimeApprovalRejectionDeliveryOutboxRow{}, &runtimeApprovalRejectionDeliveryInboxRow{}, &runtimeApprovalRejectionDeliveryTransactionRow{}, &runtimeConfigDeliveryInboxRow{}, &runtimeConfigDeliveryOutboxRow{}, &runtimeConfigDeliveryTransactionRow{}, &runtimeConfigDirectoryRow{}, &runtimeConfigDirectoryOutboxRow{}, &runtimeConfigDirectoryFanoutRow{}, &runtimeConfigDirectoryRebindPlanRow{}, &runtimeConfigDirectoryRebindConfirmationRow{}, &runtimeConfigDirectoryRebindApplyRow{}, &runtimeConfigDirectoryRebindMultiConfirmationRow{}, &runtimeConfigDirectoryRebindMultiApplyRow{}, &runtimeConfigDirectoryRebindInboxRow{}, &runtimeDeliveryAttemptRow{}, &runtimeDeliveryGroupRow{}, &runtimeDeliveryGroupTransactionRow{}, &runtimeDeliveryGroupFenceRow{}, &runtimeDeliveryGroupSettlementRow{}, &runtimeDeliveryGroupSagaRow{}, &runtimeDeliveryCompensationRow{}, &approvalRow{}, &toolCallRow{}, &taskPlanRow{}, &taskContractRow{}, &instructionSnapshotSetRow{}, &contextManifestRow{}, &workingSetRow{}, &verificationRunRow{}, &runtimeSnapshotRow{}, &toolSetSnapshotRow{}, &modelCapabilitySnapshotRow{}, &evalRunRow{}, &artifactRow{}, &artifactObjectDeletionRow{}, &botGroupAdminRow{}, &botCommandPolicyRow{}, &botCommandAuditRow{}, &botSourceNameRow{}, &botMessageSourceRow{}, &sessionRuleRow{}, &sessionRuleGroupRow{}, &scheduledTaskRow{}); err != nil {
		return nil, fmt.Errorf("迁移 Abot 表失败: %w", err)
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

	// ADK 自带的 database.Service 负责会话事件、State 和 EventCompaction 的持久化。
	// ADK 自己创建 gorm 实例；把同一个 logger 传进去，否则它的
	// "record not found" 仍会以错误级别刷屏。
	sessionService, err := adksessiondb.NewSessionService(sqlite.Open(dbPath), &gorm.Config{Logger: newDatabaseLogger()})
	if err != nil {
		return nil, fmt.Errorf("创建 ADK 会话服务失败: %w", err)
	}
	if err := adksessiondb.AutoMigrate(sessionService); err != nil {
		return nil, fmt.Errorf("迁移 ADK 会话表失败: %w", err)
	}
	return &Store{db: db, sessionService: sessionService}, nil
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

// Close 关闭业务数据库连接；ADK 服务随进程退出释放其连接。
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
