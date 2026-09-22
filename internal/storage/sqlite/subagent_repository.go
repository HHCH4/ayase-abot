package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentruntime "Abot/internal/agent/runtime"
	"gorm.io/gorm"
)

// 子 Agent 表只保存有界文本和元数据；附件字节、模型上下文以及授权材料均不进入这些表。
type subAgentGroupRow struct {
	ID                 string    `gorm:"primaryKey;size:100"`
	InvocationID       string    `gorm:"index;size:100"`
	ConversationID     string    `gorm:"index;size:100"`
	RootInvocationID   string    `gorm:"index;size:100"`
	ParentInvocationID string    `gorm:"index;size:100"`
	ParentNodeID       string    `gorm:"index;size:100"`
	Profile            string    `gorm:"index;size:80;not null"`
	Purpose            string    `gorm:"size:1000"`
	Status             string    `gorm:"index;size:40;not null"`
	FailurePolicy      string    `gorm:"size:40;not null"`
	ExpectedCount      int       `gorm:"not null"`
	QueuedCount        int       `gorm:"not null"`
	RunningCount       int       `gorm:"not null"`
	CompletedCount     int       `gorm:"not null"`
	FailedCount        int       `gorm:"not null"`
	CancelledCount     int       `gorm:"not null"`
	MaxConcurrency     int       `gorm:"not null"`
	TimeoutSeconds     int       `gorm:"not null"`
	OutputBudgetBytes  int       `gorm:"not null"`
	InputBudgetBytes   int       `gorm:"not null"`
	ImageBudgetBytes   int       `gorm:"not null"`
	DeadlineAt         time.Time `gorm:"index"`
	SourceKindsJSON    string    `gorm:"type:text"`
	QueryDigest        string    `gorm:"size:128"`
	ParentPlanID       string    `gorm:"size:100"`
	ResultDigest       string    `gorm:"size:128"`
	ErrorSummary       string    `gorm:"type:text"`
	CreatedAt          time.Time `gorm:"index"`
	StartedAt          time.Time
	FinishedAt         time.Time
	UpdatedAt          time.Time
}

func (subAgentGroupRow) TableName() string { return "abot_agent_subagent_groups" }

type subAgentRunRow struct {
	ID             string     `gorm:"primaryKey;size:100"`
	GroupID        string     `gorm:"index:idx_subagent_runs_group_ordinal,priority:1;size:100;not null"`
	InvocationID   string     `gorm:"index;size:100"`
	ParentNodeID   string     `gorm:"size:100"`
	Ordinal        int        `gorm:"index:idx_subagent_runs_group_ordinal,priority:2;not null"`
	Stage          int        `gorm:"not null"`
	DependsOnJSON  string     `gorm:"type:text"`
	Profile        string     `gorm:"index;size:80;not null"`
	SourceKind     string     `gorm:"index;size:80"`
	Status         string     `gorm:"index;size:40;not null"`
	Attempt        int        `gorm:"not null"`
	ProviderID     string     `gorm:"size:100"`
	ModelID        string     `gorm:"size:300"`
	InputDigest    string     `gorm:"size:128"`
	InputJSON      string     `gorm:"type:text"`
	ResultText     string     `gorm:"type:text"`
	ResultJSON     string     `gorm:"type:text"`
	ResultDigest   string     `gorm:"size:128"`
	ErrorCode      string     `gorm:"size:80"`
	Error          string     `gorm:"type:text"`
	ErrorRetryable bool       `gorm:"not null"`
	LeaseOwner     string     `gorm:"index;size:160"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	CreatedAt      time.Time  `gorm:"index"`
	QueuedAt       time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
	UpdatedAt      time.Time
}

func (subAgentRunRow) TableName() string { return "abot_agent_subagent_runs" }

type subAgentEvidenceRow struct {
	EvidenceID     string `gorm:"primaryKey;size:160"`
	GroupID        string `gorm:"index;size:100;not null"`
	SourceKind     string `gorm:"index;size:80;not null"`
	SourceID       string `gorm:"size:300"`
	SourceName     string `gorm:"size:500"`
	Locator        string `gorm:"size:1000"`
	Title          string `gorm:"size:1000"`
	Excerpt        string `gorm:"type:text;not null"`
	ContentDigest  string `gorm:"index;size:128;not null"`
	RetrievalScore float64
	RerankScore    float64
	RetrievedAt    time.Time `gorm:"index"`
	PublishedAt    *time.Time
	Freshness      string `gorm:"size:80"`
	TrustLevel     string `gorm:"size:80"`
	Citation       string `gorm:"size:1000"`
	MetadataJSON   string `gorm:"type:text"`
	Truncated      bool
	Stale          bool
}

func (subAgentEvidenceRow) TableName() string { return "abot_agent_subagent_evidence" }

func subAgentGroupToRow(item agentruntime.SubAgentGroup) (*subAgentGroupRow, error) {
	sources, err := json.Marshal(item.SourceKinds)
	if err != nil {
		return nil, err
	}
	return &subAgentGroupRow{
		ID: item.ID, InvocationID: item.InvocationID, ConversationID: item.ConversationID, RootInvocationID: item.RootInvocationID, ParentInvocationID: item.ParentInvocationID,
		ParentNodeID: item.ParentNodeID, Profile: string(item.Profile), Purpose: item.Purpose, Status: string(item.Status), FailurePolicy: string(item.FailurePolicy),
		ExpectedCount: item.ExpectedCount, QueuedCount: item.QueuedCount, RunningCount: item.RunningCount, CompletedCount: item.CompletedCount, FailedCount: item.FailedCount, CancelledCount: item.CancelledCount,
		MaxConcurrency: item.MaxConcurrency, TimeoutSeconds: item.TimeoutSeconds, OutputBudgetBytes: item.OutputBudgetBytes, InputBudgetBytes: item.InputBudgetBytes, ImageBudgetBytes: item.ImageBudgetBytes, DeadlineAt: item.DeadlineAt, SourceKindsJSON: string(sources), QueryDigest: item.QueryDigest,
		ParentPlanID: item.ParentPlanID, ResultDigest: item.ResultDigest, ErrorSummary: item.ErrorSummary, CreatedAt: item.CreatedAt, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

func subAgentGroupFromRow(row subAgentGroupRow) agentruntime.SubAgentGroup {
	var sourceKinds []string
	_ = json.Unmarshal([]byte(row.SourceKindsJSON), &sourceKinds)
	return agentruntime.SubAgentGroup{
		ID: row.ID, InvocationID: row.InvocationID, ConversationID: row.ConversationID, RootInvocationID: row.RootInvocationID, ParentInvocationID: row.ParentInvocationID, ParentNodeID: row.ParentNodeID,
		Profile: agentruntime.SubAgentProfile(row.Profile), Purpose: row.Purpose, Status: agentruntime.SubAgentGroupStatus(row.Status), FailurePolicy: agentruntime.SubAgentFailurePolicy(row.FailurePolicy),
		ExpectedCount: row.ExpectedCount, QueuedCount: row.QueuedCount, RunningCount: row.RunningCount, CompletedCount: row.CompletedCount, FailedCount: row.FailedCount, CancelledCount: row.CancelledCount,
		MaxConcurrency: row.MaxConcurrency, TimeoutSeconds: row.TimeoutSeconds, OutputBudgetBytes: row.OutputBudgetBytes, InputBudgetBytes: row.InputBudgetBytes, ImageBudgetBytes: row.ImageBudgetBytes, DeadlineAt: row.DeadlineAt, SourceKinds: sourceKinds, QueryDigest: row.QueryDigest,
		ParentPlanID: row.ParentPlanID, ResultDigest: row.ResultDigest, ErrorSummary: row.ErrorSummary, CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, UpdatedAt: row.UpdatedAt,
	}
}

func subAgentRunToRow(item agentruntime.SubAgentRun) (*subAgentRunRow, error) {
	depends, err := json.Marshal(item.DependsOn)
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(item.InputMetadata)
	if err != nil {
		return nil, err
	}
	result, err := json.Marshal(item.ResultMetadata)
	if err != nil {
		return nil, err
	}
	var lease *time.Time
	if !item.LeaseExpiresAt.IsZero() {
		value := item.LeaseExpiresAt.UTC()
		lease = &value
	}
	return &subAgentRunRow{
		ID: item.ID, GroupID: item.GroupID, InvocationID: item.InvocationID, ParentNodeID: item.ParentNodeID, Ordinal: item.Ordinal, Stage: item.Stage,
		DependsOnJSON: string(depends), Profile: string(item.Profile), SourceKind: item.SourceKind, Status: string(item.Status), Attempt: item.Attempt, ProviderID: item.ProviderID, ModelID: item.ModelID,
		InputDigest: item.InputDigest, InputJSON: string(input), ResultText: item.ResultText, ResultJSON: string(result), ResultDigest: item.ResultDigest, ErrorCode: item.ErrorCode, Error: item.Error, ErrorRetryable: item.ErrorRetryable,
		LeaseOwner: item.LeaseOwner, LeaseExpiresAt: lease, CreatedAt: item.CreatedAt, QueuedAt: item.QueuedAt, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

func subAgentRunFromRow(row subAgentRunRow) agentruntime.SubAgentRun {
	var depends []string
	var input, result map[string]any
	_ = json.Unmarshal([]byte(row.DependsOnJSON), &depends)
	_ = json.Unmarshal([]byte(row.InputJSON), &input)
	_ = json.Unmarshal([]byte(row.ResultJSON), &result)
	var lease time.Time
	if row.LeaseExpiresAt != nil {
		lease = row.LeaseExpiresAt.UTC()
	}
	return agentruntime.SubAgentRun{
		ID: row.ID, GroupID: row.GroupID, InvocationID: row.InvocationID, ParentNodeID: row.ParentNodeID, Ordinal: row.Ordinal, Stage: row.Stage, DependsOn: depends,
		Profile: agentruntime.SubAgentProfile(row.Profile), SourceKind: row.SourceKind, Status: agentruntime.SubAgentRunStatus(row.Status), Attempt: row.Attempt, ProviderID: row.ProviderID, ModelID: row.ModelID,
		InputDigest: row.InputDigest, InputMetadata: input, ResultText: row.ResultText, ResultMetadata: result, ResultDigest: row.ResultDigest, ErrorCode: row.ErrorCode, Error: row.Error, ErrorRetryable: row.ErrorRetryable,
		LeaseOwner: row.LeaseOwner, LeaseExpiresAt: lease, CreatedAt: row.CreatedAt, QueuedAt: row.QueuedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, UpdatedAt: row.UpdatedAt,
	}
}

func subAgentEvidenceToRow(item agentruntime.EvidenceItem, groupID string) (*subAgentEvidenceRow, error) {
	item.EvidenceID = strings.TrimSpace(item.EvidenceID)
	item.SourceKind = strings.TrimSpace(item.SourceKind)
	item.Excerpt = strings.TrimSpace(item.Excerpt)
	if item.EvidenceID == "" || item.SourceKind == "" || item.Excerpt == "" {
		return nil, agentruntime.ErrConflict
	}
	if len(item.Excerpt) > 32<<10 {
		item.Excerpt = agentruntime.LimitSubAgentText(item.Excerpt, 32<<10)
		item.Truncated = true
	}
	if item.ContentDigest == "" {
		item.ContentDigest = digestSQLiteText(item.SourceKind + "\x00" + item.SourceID + "\x00" + item.Locator + "\x00" + item.Excerpt)
	}
	if item.RetrievedAt.IsZero() {
		item.RetrievedAt = time.Now().UTC()
	}
	metadata, err := json.Marshal(item.Metadata)
	if err != nil {
		return nil, err
	}
	if len(metadata) > 16<<10 {
		return nil, agentruntime.ErrConflict
	}
	return &subAgentEvidenceRow{EvidenceID: item.EvidenceID, GroupID: groupID, SourceKind: item.SourceKind, SourceID: item.SourceID, SourceName: item.SourceName, Locator: item.Locator, Title: item.Title, Excerpt: item.Excerpt, ContentDigest: item.ContentDigest, RetrievalScore: item.RetrievalScore, RerankScore: item.RerankScore, RetrievedAt: item.RetrievedAt, PublishedAt: item.PublishedAt, Freshness: item.Freshness, TrustLevel: item.TrustLevel, Citation: item.Citation, MetadataJSON: string(metadata), Truncated: item.Truncated, Stale: item.Stale}, nil
}

func subAgentEvidenceFromRow(row subAgentEvidenceRow) agentruntime.EvidenceItem {
	var metadata map[string]any
	_ = json.Unmarshal([]byte(row.MetadataJSON), &metadata)
	return agentruntime.EvidenceItem{EvidenceID: row.EvidenceID, SourceKind: row.SourceKind, SourceID: row.SourceID, SourceName: row.SourceName, Locator: row.Locator, Title: row.Title, Excerpt: row.Excerpt, ContentDigest: row.ContentDigest, RetrievalScore: row.RetrievalScore, RerankScore: row.RerankScore, RetrievedAt: row.RetrievedAt, PublishedAt: row.PublishedAt, Freshness: row.Freshness, TrustLevel: row.TrustLevel, Citation: row.Citation, Metadata: metadata, Truncated: row.Truncated, Stale: row.Stale}
}

func (r *runtimeRepository) CreateSubAgentGroup(ctx context.Context, group agentruntime.SubAgentGroup, runs []agentruntime.SubAgentRun) error {
	now := time.Now().UTC()
	group, err := normalizeSubAgentGroupForSQLite(group, now)
	if err != nil {
		return err
	}
	if len(runs) != group.ExpectedCount {
		return agentruntime.ErrConflict
	}
	rows := make([]*subAgentRunRow, 0, len(runs))
	normalizedRuns := make([]agentruntime.SubAgentRun, 0, len(runs))
	for _, run := range runs {
		normalized, normalizeErr := normalizeSubAgentRunForSQLite(run, now)
		if normalizeErr != nil {
			return normalizeErr
		}
		if normalized.GroupID != group.ID || normalized.InvocationID != group.InvocationID {
			return agentruntime.ErrConflict
		}
		row, rowErr := subAgentRunToRow(normalized)
		if rowErr != nil {
			return rowErr
		}
		rows = append(rows, row)
		normalizedRuns = append(normalizedRuns, normalized)
	}
	if err := agentruntime.ValidateSubAgentRunDependencies(normalizedRuns); err != nil {
		return err
	}
	groupRow, err := subAgentGroupToRow(group)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(groupRow).Error; err != nil {
			return mapSubAgentSQLiteError(err)
		}
		for _, row := range rows {
			if err := tx.Create(row).Error; err != nil {
				return mapSubAgentSQLiteError(err)
			}
		}
		return nil
	})
}

func normalizeSubAgentGroupForSQLite(item agentruntime.SubAgentGroup, now time.Time) (agentruntime.SubAgentGroup, error) {
	item.ID = strings.TrimSpace(item.ID)
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	item.Profile = agentruntime.SubAgentProfile(strings.TrimSpace(string(item.Profile)))
	item.Purpose = strings.TrimSpace(item.Purpose)
	if item.ID == "" || !agentruntime.ValidSubAgentProfile(item.Profile) || item.ExpectedCount <= 0 || item.ExpectedCount > 32 {
		return agentruntime.SubAgentGroup{}, agentruntime.ErrConflict
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = now
	}
	if item.Status == "" {
		item.Status = agentruntime.SubAgentGroupQueued
	}
	if !agentruntime.ValidSubAgentGroupStatus(item.Status) {
		return agentruntime.SubAgentGroup{}, agentruntime.ErrConflict
	}
	if item.FailurePolicy == "" {
		item.FailurePolicy = agentruntime.SubAgentFailureContinue
	}
	if item.MaxConcurrency <= 0 {
		item.MaxConcurrency = 1
	}
	if item.MaxConcurrency > 4 {
		return agentruntime.SubAgentGroup{}, agentruntime.ErrConflict
	}
	if item.TimeoutSeconds <= 0 {
		item.TimeoutSeconds = 120
	}
	if item.TimeoutSeconds > 300 {
		return agentruntime.SubAgentGroup{}, agentruntime.ErrConflict
	}
	if item.OutputBudgetBytes <= 0 {
		item.OutputBudgetBytes = 128 << 10
	}
	if item.OutputBudgetBytes > 128<<10 {
		return agentruntime.SubAgentGroup{}, agentruntime.ErrConflict
	}
	if item.OutputBudgetBytes < item.ExpectedCount {
		return agentruntime.SubAgentGroup{}, agentruntime.ErrConflict
	}
	if item.InputBudgetBytes < 0 || item.InputBudgetBytes > 512<<10 || item.ImageBudgetBytes < 0 || item.ImageBudgetBytes > 24<<20 {
		return agentruntime.SubAgentGroup{}, agentruntime.ErrConflict
	}
	if item.DeadlineAt.IsZero() && item.Status == agentruntime.SubAgentGroupRunning {
		item.DeadlineAt = now.Add(time.Duration(item.TimeoutSeconds) * time.Second)
	}
	return item, nil
}

func normalizeSubAgentRunForSQLite(item agentruntime.SubAgentRun, now time.Time) (agentruntime.SubAgentRun, error) {
	item.ID = strings.TrimSpace(item.ID)
	item.GroupID = strings.TrimSpace(item.GroupID)
	item.InvocationID = strings.TrimSpace(item.InvocationID)
	item.ParentNodeID = strings.TrimSpace(item.ParentNodeID)
	item.Profile = agentruntime.SubAgentProfile(strings.TrimSpace(string(item.Profile)))
	item.SourceKind = strings.TrimSpace(item.SourceKind)
	if item.ID == "" || item.GroupID == "" || item.Profile == "" || item.Ordinal < 0 || item.Ordinal >= 32 || item.Stage < 0 || item.Attempt < 0 || item.Attempt > 16 {
		return agentruntime.SubAgentRun{}, agentruntime.ErrConflict
	}
	if item.Status == "" {
		item.Status = agentruntime.SubAgentRunQueued
	}
	if !agentruntime.ValidSubAgentRunStatus(item.Status) || !agentruntime.ValidSubAgentProfile(item.Profile) {
		return agentruntime.SubAgentRun{}, agentruntime.ErrConflict
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	if item.QueuedAt.IsZero() {
		item.QueuedAt = now
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = now
	}
	input, err := json.Marshal(item.InputMetadata)
	if err != nil || len(input) > 16<<10 {
		return agentruntime.SubAgentRun{}, agentruntime.ErrConflict
	}
	if len([]byte(item.ResultText)) > 32<<10 {
		item.ResultText = agentruntime.LimitSubAgentText(item.ResultText, 32<<10)
	}
	if len(item.Error) > 4096 {
		item.Error = item.Error[:4096]
	}
	return item, nil
}

func mapSubAgentSQLiteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return agentruntime.ErrConflict
	}
	return err
}

func (r *runtimeRepository) GetSubAgentGroup(ctx context.Context, id string) (agentruntime.SubAgentGroup, error) {
	var row subAgentGroupRow
	err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return agentruntime.SubAgentGroup{}, agentruntime.ErrNotFound
	}
	if err != nil {
		return agentruntime.SubAgentGroup{}, err
	}
	return subAgentGroupFromRow(row), nil
}

func (r *runtimeRepository) ListSubAgentGroups(ctx context.Context, invocationID string, status agentruntime.SubAgentGroupStatus, limit int) ([]agentruntime.SubAgentGroup, error) {
	if limit <= 0 || limit > agentruntime.MaxSubAgentListLimit {
		limit = agentruntime.MaxSubAgentListLimit
	}
	query := r.db.WithContext(ctx).Order("created_at ASC").Order("id ASC").Limit(limit)
	if strings.TrimSpace(invocationID) != "" {
		query = query.Where("invocation_id = ?", strings.TrimSpace(invocationID))
	}
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []subAgentGroupRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.SubAgentGroup, 0, len(rows))
	for _, row := range rows {
		result = append(result, subAgentGroupFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) UpdateSubAgentGroup(ctx context.Context, item agentruntime.SubAgentGroup) error {
	row, err := subAgentGroupToRow(item)
	if err != nil {
		return err
	}
	result := r.db.WithContext(ctx).Model(&subAgentGroupRow{}).Where("id = ?", row.ID).Updates(map[string]any{
		"invocation_id": row.InvocationID, "conversation_id": row.ConversationID, "root_invocation_id": row.RootInvocationID, "parent_invocation_id": row.ParentInvocationID, "parent_node_id": row.ParentNodeID,
		"profile": row.Profile, "purpose": row.Purpose, "status": row.Status, "failure_policy": row.FailurePolicy, "expected_count": row.ExpectedCount, "queued_count": row.QueuedCount,
		"running_count": row.RunningCount, "completed_count": row.CompletedCount, "failed_count": row.FailedCount, "cancelled_count": row.CancelledCount, "max_concurrency": row.MaxConcurrency,
		"timeout_seconds": row.TimeoutSeconds, "output_budget_bytes": row.OutputBudgetBytes, "input_budget_bytes": row.InputBudgetBytes, "image_budget_bytes": row.ImageBudgetBytes, "deadline_at": row.DeadlineAt, "source_kinds_json": row.SourceKindsJSON, "query_digest": row.QueryDigest, "parent_plan_id": row.ParentPlanID,
		"result_digest": row.ResultDigest, "error_summary": row.ErrorSummary, "started_at": row.StartedAt, "finished_at": row.FinishedAt, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return agentruntime.ErrNotFound
	}
	return nil
}

func (r *runtimeRepository) GetSubAgentRun(ctx context.Context, id string) (agentruntime.SubAgentRun, error) {
	var row subAgentRunRow
	err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(id)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return agentruntime.SubAgentRun{}, agentruntime.ErrNotFound
	}
	if err != nil {
		return agentruntime.SubAgentRun{}, err
	}
	return subAgentRunFromRow(row), nil
}

func (r *runtimeRepository) ListSubAgentRuns(ctx context.Context, groupID string, status agentruntime.SubAgentRunStatus, limit int) ([]agentruntime.SubAgentRun, error) {
	if limit <= 0 || limit > agentruntime.MaxSubAgentListLimit {
		limit = agentruntime.MaxSubAgentListLimit
	}
	query := r.db.WithContext(ctx).Where("group_id = ?", strings.TrimSpace(groupID)).Order("ordinal ASC").Order("id ASC").Limit(limit)
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	var rows []subAgentRunRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.SubAgentRun, 0, len(rows))
	for _, row := range rows {
		result = append(result, subAgentRunFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) ClaimSubAgentRun(ctx context.Context, id, owner string, now time.Time, ttl time.Duration) (agentruntime.SubAgentRun, bool, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(owner) == "" || ttl <= 0 || ttl > 10*time.Minute {
		return agentruntime.SubAgentRun{}, false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var result agentruntime.SubAgentRun
	claimed := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row subAgentRunRow
		if err := tx.Where("id = ?", strings.TrimSpace(id)).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		if agentruntime.SubAgentRunStatus(row.Status) == agentruntime.SubAgentRunCompleted || agentruntime.SubAgentRunStatus(row.Status) == agentruntime.SubAgentRunFailed || agentruntime.SubAgentRunStatus(row.Status) == agentruntime.SubAgentRunCancelled || agentruntime.SubAgentRunStatus(row.Status) == agentruntime.SubAgentRunExpired {
			result = subAgentRunFromRow(row)
			return nil
		}
		if row.Status == string(agentruntime.SubAgentRunRunning) && row.LeaseExpiresAt != nil && row.LeaseExpiresAt.After(now) {
			result = subAgentRunFromRow(row)
			return nil
		}
		expires := now.Add(ttl)
		updates := map[string]any{"status": string(agentruntime.SubAgentRunRunning), "attempt": row.Attempt + 1, "lease_owner": strings.TrimSpace(owner), "lease_expires_at": expires, "started_at": now, "updated_at": now}
		update := tx.Model(&subAgentRunRow{}).Where("id = ? AND (status = ? OR (status = ? AND lease_expires_at <= ?))", row.ID, string(agentruntime.SubAgentRunQueued), string(agentruntime.SubAgentRunRunning), now).Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected == 0 {
			if err := tx.Where("id = ?", row.ID).First(&row).Error; err != nil {
				return err
			}
			result = subAgentRunFromRow(row)
			return nil
		}
		if err := tx.Where("id = ?", row.ID).First(&row).Error; err != nil {
			return err
		}
		result = subAgentRunFromRow(row)
		claimed = true
		return nil
	})
	return result, claimed, err
}

func (r *runtimeRepository) CompleteSubAgentRun(ctx context.Context, id, owner string, status agentruntime.SubAgentRunStatus, resultText, errorCode, message string, now time.Time) (bool, error) {
	if !statusTerminal(status) || strings.TrimSpace(owner) == "" {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if len([]byte(resultText)) > 32<<10 {
		resultText = agentruntime.LimitSubAgentText(resultText, 32<<10)
	}
	updates := map[string]any{"status": string(status), "result_text": resultText, "result_digest": digestSQLiteText(resultText), "error_code": strings.TrimSpace(errorCode), "error": boundSQLiteSubAgentError(message), "lease_owner": "", "lease_expires_at": nil, "finished_at": now, "updated_at": now}
	result := r.db.WithContext(ctx).Model(&subAgentRunRow{}).Where("id = ? AND status = ? AND lease_owner = ? AND lease_expires_at > ?", strings.TrimSpace(id), string(agentruntime.SubAgentRunRunning), strings.TrimSpace(owner), now).Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	return true, nil
}

func (r *runtimeRepository) TransitionSubAgentRun(ctx context.Context, id string, from, to agentruntime.SubAgentRunStatus, message string, now time.Time) (bool, error) {
	if !subAgentRunTransitionValid(from, to) {
		return false, agentruntime.ErrConflict
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	updates := map[string]any{"status": string(to), "error": boundSQLiteSubAgentError(message), "updated_at": now}
	if statusTerminal(to) {
		updates["finished_at"] = now
		updates["lease_owner"] = ""
		updates["lease_expires_at"] = nil
	}
	result := r.db.WithContext(ctx).Model(&subAgentRunRow{}).Where("id = ? AND status = ?", strings.TrimSpace(id), string(from)).Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	return true, nil
}

func statusTerminal(status agentruntime.SubAgentRunStatus) bool {
	return status == agentruntime.SubAgentRunCompleted || status == agentruntime.SubAgentRunFailed || status == agentruntime.SubAgentRunCancelled || status == agentruntime.SubAgentRunExpired
}

func subAgentRunTransitionValid(from, to agentruntime.SubAgentRunStatus) bool {
	if from == to {
		return false
	}
	if from == agentruntime.SubAgentRunQueued {
		return to == agentruntime.SubAgentRunRunning || to == agentruntime.SubAgentRunFailed || to == agentruntime.SubAgentRunCancelled || to == agentruntime.SubAgentRunExpired
	}
	if from == agentruntime.SubAgentRunRunning {
		return statusTerminal(to)
	}
	return false
}

func digestSQLiteText(value string) string {
	return agentruntimeDigest(value)
}

func agentruntimeDigest(value string) string {
	// 通过 Runtime 暴露的摘要字段保持格式一致；这里不引入附件正文。
	return "sha256:" + hexDigest(value)
}

func hexDigest(value string) string {
	// 该小函数避免把哈希实现散落到 SQL 行转换代码中。
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func boundSQLiteSubAgentError(message string) string {
	message = strings.TrimSpace(message)
	return agentruntime.LimitSubAgentText(message, 4096)
}

func (r *runtimeRepository) SaveSubAgentEvidence(ctx context.Context, groupID string, items []agentruntime.EvidenceItem) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return agentruntime.ErrConflict
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var group subAgentGroupRow
		if err := tx.Where("id = ?", groupID).First(&group).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentruntime.ErrNotFound
			}
			return err
		}
		if err := tx.Where("group_id = ?", groupID).Delete(&subAgentEvidenceRow{}).Error; err != nil {
			return err
		}
		for _, item := range items {
			row, err := subAgentEvidenceToRow(item, groupID)
			if err != nil {
				return err
			}
			if err := tx.Create(row).Error; err != nil {
				return mapSubAgentSQLiteError(err)
			}
		}
		return nil
	})
}

func (r *runtimeRepository) ListSubAgentEvidence(ctx context.Context, groupID string, limit int, after string) ([]agentruntime.EvidenceItem, error) {
	var group subAgentGroupRow
	if err := r.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(groupID)).First(&group).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, agentruntime.ErrNotFound
		}
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	query := r.db.WithContext(ctx).Where("group_id = ?", strings.TrimSpace(groupID)).Order("evidence_id ASC").Limit(limit)
	if strings.TrimSpace(after) != "" {
		query = query.Where("evidence_id > ?", strings.TrimSpace(after))
	}
	var rows []subAgentEvidenceRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]agentruntime.EvidenceItem, 0, len(rows))
	for _, row := range rows {
		result = append(result, subAgentEvidenceFromRow(row))
	}
	return result, nil
}

func (r *runtimeRepository) PruneSubAgentRecords(ctx context.Context, before time.Time, limit int) (int, error) {
	if before.IsZero() {
		before = time.Now().UTC()
	} else {
		before = before.UTC()
	}
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	var groups []subAgentGroupRow
	if err := r.db.WithContext(ctx).Where("status IN ? AND finished_at > ? AND finished_at < ?", []string{
		string(agentruntime.SubAgentGroupCompleted), string(agentruntime.SubAgentGroupPartial), string(agentruntime.SubAgentGroupFailed), string(agentruntime.SubAgentGroupCancelled), string(agentruntime.SubAgentGroupExpired),
	}, time.Time{}, before).Order("finished_at ASC").Limit(limit).Find(&groups).Error; err != nil {
		return 0, err
	}
	if len(groups) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_id IN ?", ids).Delete(&subAgentEvidenceRow{}).Error; err != nil {
			return err
		}
		if err := tx.Where("group_id IN ?", ids).Delete(&subAgentRunRow{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&subAgentGroupRow{}).Error
	})
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}
