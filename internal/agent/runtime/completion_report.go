package runtime

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	"Abot/internal/workspace"
)

// WorkflowResultStatus is the user-facing result state. It is deliberately
// separate from InvocationStatus: a completed invocation can still have a
// partial or blocked coding result when acceptance evidence is missing.
type WorkflowResultStatus string

const (
	WorkflowResultInProgress WorkflowResultStatus = "in_progress"
	WorkflowResultWaiting    WorkflowResultStatus = "waiting"
	WorkflowResultCompleted  WorkflowResultStatus = "completed"
	WorkflowResultPartial    WorkflowResultStatus = "partial"
	WorkflowResultBlocked    WorkflowResultStatus = "blocked"
	WorkflowResultFailed     WorkflowResultStatus = "failed"
)

type CriterionResultStatus string

const (
	CriterionSatisfied CriterionResultStatus = "satisfied"
	CriterionPending   CriterionResultStatus = "pending"
)

// CriterionResult is a conservative projection of TaskContract evidence. A
// criterion is satisfied only when an explicit durable evidence reference was
// attached; plan completion alone never turns a criterion into a claim.
type CriterionResult struct {
	ID          string                `json:"id"`
	Description string                `json:"description"`
	Status      CriterionResultStatus `json:"status"`
	Evidence    []EvidenceRef         `json:"evidence,omitempty"`
}

// AgentChangePath is the metadata-only attribution of one workspace-relative
// path. Operation IDs point back to the Workspace operation ledger; no file
// contents are copied into a completion report.
type AgentChangePath struct {
	Path           string   `json:"path"`
	OperationIDs   []string `json:"operation_ids,omitempty"`
	OperationTypes []string `json:"operation_types,omitempty"`
	BaselineStatus string   `json:"baseline_status,omitempty"`
	CurrentStatus  string   `json:"current_status,omitempty"`
}

// AgentChangeSetStatus describes how confidently Runtime can attribute the
// final path set. "known" requires a complete baseline, path-addressable
// mutation operations and (when configured) a complete terminal scan;
// "partial" records useful evidence but leaves command/scan gaps; "unknown"
// means a reliable attribution boundary was not available.
type AgentChangeSetStatus string

const (
	AgentChangeSetKnown   AgentChangeSetStatus = "known"
	AgentChangeSetPartial AgentChangeSetStatus = "partial"
	AgentChangeSetUnknown AgentChangeSetStatus = "unknown"
)

// AgentChangeSet is a derived, metadata-only final attribution view. It is
// deliberately not persisted as mutable state: reports can be recomputed from
// the immutable admission baseline, durable operations and a fresh read-only
// terminal scan.
type AgentChangeSet struct {
	Status                 AgentChangeSetStatus `json:"status"`
	Paths                  []AgentChangePath    `json:"paths,omitempty"`
	OverlapPaths           []string             `json:"overlap_paths,omitempty"`
	UnattributedPaths      []string             `json:"unattributed_paths,omitempty"`
	OperationIDs           []string             `json:"operation_ids,omitempty"`
	CurrentStatusDigest    string               `json:"current_status_digest,omitempty"`
	CurrentStatusKnown     bool                 `json:"current_status_known"`
	CurrentStatusTruncated bool                 `json:"current_status_truncated,omitempty"`
}

// CompletionReport is computed from durable Runtime entities on demand. It is
// metadata-only and contains references, not command output, file contents or
// secrets owned by other stores.
type CompletionReport struct {
	InvocationID        string               `json:"invocation_id"`
	InvocationStatus    InvocationStatus     `json:"invocation_status"`
	Status              WorkflowResultStatus `json:"status"`
	Summary             string               `json:"summary"`
	ContractVersion     int64                `json:"contract_version,omitempty"`
	PlanRevision        int64                `json:"plan_revision,omitempty"`
	Criteria            []CriterionResult    `json:"criteria,omitempty"`
	RemainingSteps      []PlanStepRef        `json:"remaining_steps,omitempty"`
	ChangedOperationIDs []string             `json:"changed_operation_ids,omitempty"`
	Baseline            *WorktreeBaseline    `json:"baseline,omitempty"`
	PreexistingChanges  []PathStatus         `json:"preexisting_changes,omitempty"`
	AgentChanges        *AgentChangeSet      `json:"agent_changes,omitempty"`
	VerificationRuns    []VerificationRef    `json:"verification_runs,omitempty"`
	PendingApprovals    []ApprovalRef        `json:"pending_approvals,omitempty"`
	Blockers            []Blocker            `json:"blockers,omitempty"`
	UnknownStates       []UnknownState       `json:"unknown_states,omitempty"`
	GeneratedAt         time.Time            `json:"generated_at"`
}

// GetCompletionReport builds a deterministic report from the latest durable
// state. It never changes Invocation/Plan state and is safe to call while an
// invocation is running or being replayed.
func (c *Coordinator) GetCompletionReport(ctx context.Context, invocationID string) (CompletionReport, error) {
	if c == nil || c.repo == nil {
		return CompletionReport{}, errors.New("Runtime Coordinator 不能为空")
	}
	invocationID = strings.TrimSpace(invocationID)
	invocation, err := c.repo.GetInvocation(ctx, invocationID)
	if err != nil {
		return CompletionReport{}, err
	}
	report := CompletionReport{
		InvocationID: invocation.ID, InvocationStatus: invocation.Status,
		Status: WorkflowResultInProgress, Summary: "任务仍在运行", GeneratedAt: time.Now().UTC(),
		Criteria: make([]CriterionResult, 0), RemainingSteps: make([]PlanStepRef, 0),
		ChangedOperationIDs: make([]string, 0), VerificationRuns: make([]VerificationRef, 0),
		PreexistingChanges: make([]PathStatus, 0),
		PendingApprovals:   make([]ApprovalRef, 0), Blockers: make([]Blocker, 0),
		UnknownStates: make([]UnknownState, 0),
	}

	if contractRepo, ok := c.repo.(TaskContractRepository); ok {
		if contract, contractErr := contractRepo.GetTaskContract(ctx, invocationID); contractErr == nil {
			report.ContractVersion = contract.Version
			for _, criterion := range contract.AcceptanceCriteria {
				item := CriterionResult{ID: strings.TrimSpace(criterion.ID), Description: strings.TrimSpace(criterion.Description), Status: CriterionPending, Evidence: cloneEvidence(criterion.Evidence)}
				if len(item.Evidence) > 0 {
					item.Status = CriterionSatisfied
				}
				report.Criteria = append(report.Criteria, item)
			}
		} else if !errors.Is(contractErr, ErrNotFound) {
			report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "contract_unavailable", Message: "任务契约读取失败，无法确认验收边界"})
		}
	}

	var plan *TaskPlan
	if planRepo, ok := c.repo.(TaskPlanRepository); ok {
		if current, planErr := planRepo.GetTaskPlan(ctx, invocationID); planErr == nil {
			plan = &current
			report.PlanRevision = current.Revision
			for _, step := range current.Steps {
				switch step.Status {
				case PlanStepPending, PlanStepInProgress, PlanStepBlocked:
					report.RemainingSteps = append(report.RemainingSteps, PlanStepRef{ID: step.ID, Title: step.Title})
				}
				for _, evidence := range step.Evidence {
					if evidence.Kind == "operation" && strings.TrimSpace(evidence.Ref) != "" && !containsString(report.ChangedOperationIDs, evidence.Ref) {
						report.ChangedOperationIDs = append(report.ChangedOperationIDs, evidence.Ref)
					}
				}
				if step.Status == PlanStepBlocked && strings.TrimSpace(step.Blocker) != "" {
					report.Blockers = append(report.Blockers, Blocker{Code: "plan_step_blocked", Message: step.ID + ": " + strings.TrimSpace(step.Blocker)})
				}
			}
			if strings.TrimSpace(current.Blocker) != "" {
				report.Blockers = append(report.Blockers, Blocker{Code: "plan_blocked", Message: strings.TrimSpace(current.Blocker)})
			}
		} else if !errors.Is(planErr, ErrNotFound) {
			report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "plan_unavailable", Message: "任务计划读取失败，无法确认剩余步骤"})
		}
	}

	if verificationRepo, ok := c.repo.(VerificationRepository); ok {
		if verifications, verificationErr := verificationRepo.ListVerificationRuns(ctx, invocationID); verificationErr == nil {
			for _, verification := range verifications {
				report.VerificationRuns = append(report.VerificationRuns, VerificationRef{ID: verification.ID, Status: string(verification.Status), Summary: verification.Summary})
				if verification.Status == VerificationPending || verification.Status == VerificationRunning {
					report.Blockers = append(report.Blockers, Blocker{Code: "verification_incomplete", Message: verification.ID + ": 验证尚未完成"})
				}
				if verification.Status == VerificationUnknown {
					report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "verification_unknown", Message: verification.ID + ": " + firstNonEmpty(verification.Summary, "验证结果未知")})
				}
				if verification.Status == VerificationFailed || verification.Status == VerificationBlocked {
					report.Blockers = append(report.Blockers, Blocker{Code: "verification_" + string(verification.Status), Message: verification.ID + ": " + firstNonEmpty(verification.Summary, "验证未通过")})
				}
			}
		} else {
			report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "verification_unavailable", Message: "验证记录读取失败，无法确认验证状态"})
		}
	}

	if approvals, approvalErr := c.repo.ListApprovals(ctx, invocationID, ApprovalPending); approvalErr == nil {
		for _, approval := range approvals {
			report.PendingApprovals = append(report.PendingApprovals, ApprovalRef{ID: approval.ID, ToolName: approval.ToolName, OperationID: approval.OperationID})
		}
	} else {
		report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "approval_unavailable", Message: "审批记录读取失败，无法确认是否仍有待处理审批"})
	}
	if len(report.PendingApprovals) > 0 {
		report.Blockers = append(report.Blockers, Blocker{Code: "pending_approval", Message: "仍有待处理审批"})
	}
	if baselineRepo, ok := c.repo.(WorktreeBaselineRepository); ok {
		baseline, baselineErr := baselineRepo.GetWorkspaceBaseline(ctx, invocationID)
		if baselineErr == nil {
			report.Baseline = &baseline
			report.PreexistingChanges = append(report.PreexistingChanges, baseline.ChangedPaths...)
			if !baseline.StatusKnown || baseline.Truncated {
				report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "baseline_status_unknown", Message: "工作区基线状态不完整，无法完成已有修改归因"})
			}
		} else if errors.Is(baselineErr, ErrNotFound) {
			if strings.TrimSpace(invocation.WorkspaceID) != "" {
				report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "baseline_missing", Message: "缺少 Invocation 创建时的工作区基线"})
			}
		} else {
			return CompletionReport{}, baselineErr
		}
	} else if strings.TrimSpace(invocation.WorkspaceID) != "" {
		report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "baseline_unavailable", Message: "当前 Runtime 存储未提供工作区基线"})
	}
	if changes, changeErr := c.buildAgentChangeSet(ctx, invocation, report.Baseline); changeErr != nil {
		return CompletionReport{}, changeErr
	} else if changes != nil {
		report.AgentChanges = changes
		if changes.Status == AgentChangeSetUnknown {
			report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "agent_changes_unknown", Message: "无法完整归因本次任务的工作区变更"})
		} else if changes.Status == AgentChangeSetPartial {
			report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "agent_changes_partial", Message: "工作区变更仅能按已有 operation/部分扫描结果归因"})
		}
	}
	if toolRepo, ok := c.repo.(ToolCallRepository); ok {
		if toolCalls, toolErr := toolRepo.ListToolCalls(ctx, invocationID); toolErr == nil {
			for _, call := range toolCalls {
				if call.Status == ToolCallFailed {
					report.Blockers = append(report.Blockers, Blocker{Code: "tool_failed", Message: call.ToolName + ": " + firstNonEmpty(call.Error, "工具调用失败")})
				}
			}
		} else {
			report.UnknownStates = append(report.UnknownStates, UnknownState{Code: "tool_calls_unavailable", Message: "工具调用记录读取失败，无法确认副作用状态"})
		}
	}
	if invocation.Status == InvocationFailed {
		report.Blockers = append(report.Blockers, Blocker{Code: "invocation_failed", Message: firstNonEmpty(invocation.Error, "Invocation 运行失败")})
	}

	sortCompletionReport(&report)
	report.Status, report.Summary = completionStatus(invocation, plan, report)
	return report, nil
}

// buildAgentChangeSet joins immutable admission facts with durable Workspace
// operations. The optional resolver is deliberately kept outside Runtime's
// Repository interface for backwards compatibility with embedders.
func (c *Coordinator) buildAgentChangeSet(ctx context.Context, invocation Invocation, baseline *WorktreeBaseline) (*AgentChangeSet, error) {
	if strings.TrimSpace(invocation.WorkspaceID) == "" {
		return nil, nil
	}
	c.mu.Lock()
	operationResolver := c.operationResolver
	baselineResolver := c.baselineResolver
	c.mu.Unlock()
	if operationResolver == nil {
		// The resolver is optional for embedders that only implement the original
		// Runtime Repository. Do not manufacture an empty or unknown AgentChangeSet
		// that would make otherwise valid legacy reports appear blocked.
		return nil, nil
	}
	operations, err := operationResolver(ctx, invocation.WorkspaceID, invocation.ID)
	if err != nil {
		return &AgentChangeSet{Status: AgentChangeSetUnknown, Paths: []AgentChangePath{}, OperationIDs: []string{}}, nil
	}
	changes := &AgentChangeSet{Status: AgentChangeSetKnown, Paths: make([]AgentChangePath, 0), OverlapPaths: make([]string, 0), UnattributedPaths: make([]string, 0), OperationIDs: make([]string, 0)}
	paths := make(map[string]*AgentChangePath)
	baselinePaths := make(map[string]string)
	if baseline != nil {
		for _, item := range baseline.ChangedPaths {
			if cleaned := normalizeAttributionPath(item.Path); cleaned != "" {
				baselinePaths[cleaned] = item.Status
			}
		}
		if !baseline.StatusKnown || baseline.Truncated {
			changes.Status = AgentChangeSetUnknown
		}
	} else {
		changes.Status = AgentChangeSetUnknown
	}
	commandMutation := false
	for _, operation := range operations {
		if strings.TrimSpace(operation.InvocationID) != strings.TrimSpace(invocation.ID) {
			continue
		}
		if operation.Status == workspace.OperationCompleted {
			if operation.Type == workspace.OperationCommand {
				// A successful shell command may have changed arbitrary paths;
				// it is evidence of activity, not a path-level attribution.
				commandMutation = true
				continue
			}
			if !isAttributionMutation(operation.Type) {
				continue
			}
			for _, rawPath := range attributionOperationPaths(operation) {
				cleaned := normalizeAttributionPath(rawPath)
				if cleaned == "" {
					changes.Status = AgentChangeSetUnknown
					continue
				}
				entry := paths[cleaned]
				if entry == nil {
					entry = &AgentChangePath{Path: cleaned, OperationIDs: make([]string, 0), OperationTypes: make([]string, 0)}
					if status, ok := baselinePaths[cleaned]; ok {
						entry.BaselineStatus = status
						changes.OverlapPaths = append(changes.OverlapPaths, cleaned)
					}
					paths[cleaned] = entry
				}
				if !containsString(entry.OperationIDs, operation.ID) {
					entry.OperationIDs = append(entry.OperationIDs, operation.ID)
				}
				if !containsString(entry.OperationTypes, string(operation.Type)) {
					entry.OperationTypes = append(entry.OperationTypes, string(operation.Type))
				}
				if !containsString(changes.OperationIDs, operation.ID) {
					changes.OperationIDs = append(changes.OperationIDs, operation.ID)
				}
			}
		}
	}
	if commandMutation {
		changes.Status = worseAgentChangeSetStatus(changes.Status, AgentChangeSetPartial)
	}
	// A terminal scan is useful for formatter edits, user edits after an Agent
	// operation, and deletion/rename evidence not represented by Operations.
	// It is intentionally skipped for live reports to avoid turning polling
	// into repeated SSH/Git calls.
	if invocation.Status.Terminal() {
		if baselineResolver == nil {
			changes.Status = worseAgentChangeSetStatus(changes.Status, AgentChangeSetPartial)
		} else {
			current, scanErr := baselineResolver(ctx, invocation.WorkspaceID, invocation.TargetPath)
			if scanErr != nil {
				changes.Status = worseAgentChangeSetStatus(changes.Status, AgentChangeSetPartial)
			} else {
				changes.CurrentStatusDigest = current.StatusDigest
				changes.CurrentStatusKnown = current.StatusKnown
				changes.CurrentStatusTruncated = current.Truncated
				currentPaths := make(map[string]string, len(current.ChangedPaths))
				for _, item := range current.ChangedPaths {
					if cleaned := normalizeAttributionPath(item.Path); cleaned != "" {
						currentPaths[cleaned] = item.Status
					}
				}
				for cleaned, entry := range paths {
					entry.CurrentStatus = currentPaths[cleaned]
				}
				for cleaned := range currentPaths {
					if _, operationPath := paths[cleaned]; operationPath {
						continue
					}
					if _, baselinePath := baselinePaths[cleaned]; baselinePath {
						continue
					}
					changes.UnattributedPaths = append(changes.UnattributedPaths, cleaned)
				}
				if len(changes.UnattributedPaths) > 0 {
					changes.Status = worseAgentChangeSetStatus(changes.Status, AgentChangeSetPartial)
				}
				if !current.StatusKnown || current.Truncated {
					changes.Status = worseAgentChangeSetStatus(changes.Status, AgentChangeSetUnknown)
				}
			}
		}
	}
	for _, entry := range paths {
		sort.Strings(entry.OperationIDs)
		sort.Strings(entry.OperationTypes)
		changes.Paths = append(changes.Paths, *entry)
	}
	sort.Slice(changes.Paths, func(left, right int) bool { return changes.Paths[left].Path < changes.Paths[right].Path })
	sort.Strings(changes.OverlapPaths)
	sort.Strings(changes.UnattributedPaths)
	sort.Strings(changes.OperationIDs)
	return changes, nil
}

func worseAgentChangeSetStatus(current, candidate AgentChangeSetStatus) AgentChangeSetStatus {
	rank := func(status AgentChangeSetStatus) int {
		switch status {
		case AgentChangeSetUnknown:
			return 2
		case AgentChangeSetPartial:
			return 1
		default:
			return 0
		}
	}
	if rank(candidate) > rank(current) {
		return candidate
	}
	return current
}

func isAttributionMutation(operationType workspace.OperationType) bool {
	switch operationType {
	case workspace.OperationWriteFile, workspace.OperationMakeDirectory, workspace.OperationPatchFile, workspace.OperationPatchSet, workspace.OperationDeletePath:
		return true
	default:
		return false
	}
}

func attributionOperationPaths(operation workspace.Operation) []string {
	if operation.Type == workspace.OperationPatchSet {
		paths := make([]string, 0, len(operation.Patches))
		for _, patch := range operation.Patches {
			paths = append(paths, patch.Path)
		}
		return paths
	}
	return []string{operation.Path}
}

func normalizeAttributionPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.ContainsRune(value, '\x00') {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || path.IsAbs(cleaned) || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}

func completionStatus(invocation Invocation, plan *TaskPlan, report CompletionReport) (WorkflowResultStatus, string) {
	switch invocation.Status {
	case InvocationQueued, InvocationRunning, InvocationCancelling:
		return WorkflowResultInProgress, "任务仍在运行"
	case InvocationWaitingApproval, InvocationWaitingTool, InvocationWaitingUser:
		return WorkflowResultWaiting, "任务等待审批或外部输入"
	case InvocationFailed:
		return WorkflowResultFailed, "任务运行失败"
	case InvocationCancelled, InvocationExpired:
		if len(report.UnknownStates) > 0 || len(report.Blockers) > 0 {
			return WorkflowResultBlocked, "任务已停止，仍存在阻塞或未知状态"
		}
		return WorkflowResultPartial, "任务未完整完成"
	case InvocationCompleted:
		if len(report.UnknownStates) > 0 {
			return WorkflowResultBlocked, "任务完成事件已记录，但仍存在未知副作用"
		}
		if plan != nil && plan.Status == PlanBlocked {
			return WorkflowResultBlocked, "任务计划被阻塞"
		}
		if len(report.Blockers) > 0 {
			return WorkflowResultBlocked, "任务仍存在阻塞项"
		}
		if len(report.PendingApprovals) > 0 {
			return WorkflowResultBlocked, "任务完成事件已记录，但仍有待处理审批"
		}
		for _, verification := range report.VerificationRuns {
			if verification.Status == string(VerificationPending) || verification.Status == string(VerificationRunning) {
				return WorkflowResultBlocked, "任务完成事件已记录，但仍有验证未结束"
			}
		}
		for _, criterion := range report.Criteria {
			if criterion.Status != CriterionSatisfied {
				return WorkflowResultPartial, "任务已完成运行，但仍缺少验收证据"
			}
		}
		if plan != nil && len(plan.Steps) > 0 && plan.Status != PlanCompleted {
			return WorkflowResultPartial, "任务已完成运行，但仍有计划步骤未完成"
		}
		return WorkflowResultCompleted, "任务已完成"
	default:
		return WorkflowResultInProgress, "任务状态未知"
	}
}

func sortCompletionReport(report *CompletionReport) {
	sort.Slice(report.Criteria, func(left, right int) bool { return report.Criteria[left].ID < report.Criteria[right].ID })
	sort.Slice(report.RemainingSteps, func(left, right int) bool { return report.RemainingSteps[left].ID < report.RemainingSteps[right].ID })
	sort.Slice(report.VerificationRuns, func(left, right int) bool {
		return report.VerificationRuns[left].ID < report.VerificationRuns[right].ID
	})
	sort.Slice(report.PendingApprovals, func(left, right int) bool {
		return report.PendingApprovals[left].ID < report.PendingApprovals[right].ID
	})
	sort.Slice(report.Blockers, func(left, right int) bool {
		if report.Blockers[left].Code == report.Blockers[right].Code {
			return report.Blockers[left].Message < report.Blockers[right].Message
		}
		return report.Blockers[left].Code < report.Blockers[right].Code
	})
	sort.Slice(report.UnknownStates, func(left, right int) bool {
		if report.UnknownStates[left].Code == report.UnknownStates[right].Code {
			return report.UnknownStates[left].Message < report.UnknownStates[right].Message
		}
		return report.UnknownStates[left].Code < report.UnknownStates[right].Code
	})
	sort.Strings(report.ChangedOperationIDs)
	sort.Slice(report.PreexistingChanges, func(left, right int) bool {
		if report.PreexistingChanges[left].Path == report.PreexistingChanges[right].Path {
			return report.PreexistingChanges[left].Status < report.PreexistingChanges[right].Status
		}
		return report.PreexistingChanges[left].Path < report.PreexistingChanges[right].Path
	})
}

func cloneEvidence(evidence []EvidenceRef) []EvidenceRef {
	if len(evidence) == 0 {
		return nil
	}
	return append([]EvidenceRef(nil), evidence...)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
