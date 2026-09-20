package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WorkflowFailureDomain is the bounded classification used by the plan
// controller. It intentionally mirrors the Runtime failure domains without
// accepting arbitrary model-provided retry instructions.
type WorkflowFailureDomain string

const (
	WorkflowFailureRuntime      WorkflowFailureDomain = "runtime"
	WorkflowFailureVerification WorkflowFailureDomain = "verification"
	WorkflowFailureModel        WorkflowFailureDomain = "model"
	WorkflowFailureTool         WorkflowFailureDomain = "tool"
	WorkflowFailureWorkspace    WorkflowFailureDomain = "workspace"
	WorkflowFailureEnvironment  WorkflowFailureDomain = "environment"
	WorkflowFailureUser         WorkflowFailureDomain = "user_input"
	WorkflowFailureUnknown      WorkflowFailureDomain = "unknown"
)

// WorkflowPhase is a UI/recovery projection, not a replacement for the
// Invocation lifecycle. It is derived from durable plan, approval,
// verification and event facts so a reconnecting client never has to infer
// progress from assistant prose.
type WorkflowPhase string

const (
	WorkflowPhaseUnderstanding           WorkflowPhase = "understanding"
	WorkflowPhaseDiscoveringInstructions WorkflowPhase = "discovering_instructions"
	WorkflowPhaseInspecting              WorkflowPhase = "inspecting"
	WorkflowPhasePlanning                WorkflowPhase = "planning"
	WorkflowPhaseEditing                 WorkflowPhase = "editing"
	WorkflowPhaseWaitingApproval         WorkflowPhase = "waiting_approval"
	WorkflowPhaseWaitingTool             WorkflowPhase = "waiting_tool"
	WorkflowPhaseWaitingUser             WorkflowPhase = "waiting_user"
	WorkflowPhaseVerifying               WorkflowPhase = "verifying"
	WorkflowPhaseReviewing               WorkflowPhase = "reviewing"
	WorkflowPhaseReporting               WorkflowPhase = "reporting"
	WorkflowPhaseBlocked                 WorkflowPhase = "blocked"
)

func validWorkflowPhase(phase WorkflowPhase) bool {
	switch phase {
	case WorkflowPhaseUnderstanding, WorkflowPhaseDiscoveringInstructions, WorkflowPhaseInspecting, WorkflowPhasePlanning, WorkflowPhaseEditing, WorkflowPhaseWaitingApproval, WorkflowPhaseWaitingTool, WorkflowPhaseWaitingUser, WorkflowPhaseVerifying, WorkflowPhaseReviewing, WorkflowPhaseReporting, WorkflowPhaseBlocked:
		return true
	default:
		return false
	}
}

func deriveWorkflowPhase(invocation Invocation, plan *TaskPlan, pendingApprovals []ApprovalRef, verifications []VerificationRun, events []AgentEvent) WorkflowPhase {
	if invocation.Status.Terminal() {
		return WorkflowPhaseReporting
	}
	if len(pendingApprovals) > 0 || invocation.Status == InvocationWaitingApproval {
		return WorkflowPhaseWaitingApproval
	}
	if invocation.Status == InvocationWaitingTool {
		return WorkflowPhaseWaitingTool
	}
	if invocation.Status == InvocationWaitingUser {
		return WorkflowPhaseWaitingUser
	}
	if plan != nil {
		if plan.Status == PlanBlocked {
			return WorkflowPhaseBlocked
		}
		if step := plan.CurrentStep(); step != nil {
			return workflowPhaseForStep(*step)
		}
		if len(plan.Steps) > 0 && plan.Status == PlanPending {
			return WorkflowPhasePlanning
		}
	}
	for _, verification := range verifications {
		if verification.Status == VerificationPending || verification.Status == VerificationRunning {
			return WorkflowPhaseVerifying
		}
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		switch event.Type {
		case EventInstructionConflict, EventInstructionReconfirmed:
			return WorkflowPhaseDiscoveringInstructions
		case EventApprovalRequested:
			return WorkflowPhaseWaitingApproval
		case EventInvocationWaiting:
			reason, _ := event.Data["reason"].(string)
			if strings.EqualFold(strings.TrimSpace(reason), "approval") {
				return WorkflowPhaseWaitingApproval
			}
			if strings.EqualFold(strings.TrimSpace(reason), "user") || strings.EqualFold(strings.TrimSpace(reason), "input") {
				return WorkflowPhaseWaitingUser
			}
			return WorkflowPhaseWaitingTool
		case EventUserInputRequested:
			return WorkflowPhaseWaitingUser
		case EventVerificationUpdated:
			return WorkflowPhaseVerifying
		case EventToolRequested, EventToolStarted, EventToolOutput, EventToolCompleted, EventToolFailed, EventCommandOutput:
			return WorkflowPhaseEditing
		case EventPlanUpdated:
			return WorkflowPhasePlanning
		case EventContextManifest, EventContextCompacted, EventWorkingSetUpdated:
			return WorkflowPhaseInspecting
		}
	}
	if invocation.Status == InvocationQueued || invocation.Status == InvocationRunning {
		return WorkflowPhaseUnderstanding
	}
	return WorkflowPhaseReporting
}

func workflowPhaseForStep(step PlanStep) WorkflowPhase {
	value := strings.ToLower(strings.TrimSpace(step.ID) + " " + strings.TrimSpace(step.Title) + " " + strings.TrimSpace(step.Description))
	switch {
	case strings.Contains(value, "verif"), strings.Contains(value, "test"), strings.Contains(value, "lint"), strings.Contains(value, "验证"), strings.Contains(value, "测试"):
		return WorkflowPhaseVerifying
	case strings.Contains(value, "inspect"), strings.Contains(value, "read"), strings.Contains(value, "search"), strings.Contains(value, "分析"), strings.Contains(value, "检查代码"):
		return WorkflowPhaseInspecting
	case strings.Contains(value, "plan"), strings.Contains(value, "计划"):
		return WorkflowPhasePlanning
	default:
		return WorkflowPhaseEditing
	}
}

// defaultWorkflowPlan derives the smallest durable coding workflow from the
// already-recorded TaskContract. It is intentionally conservative: mutation
// work must pass an inspecting step before editing and a verifying step after
// editing. The plan only establishes evidence boundaries; it never invokes a
// tool, command, or file mutation by itself.
func defaultWorkflowPlan(invocation Invocation, contract TaskContract) (TaskPlan, bool) {
	plan := initialTaskPlan(invocation.ID, invocation.CreatedAt)
	var steps []PlanStep
	switch contract.TaskType {
	case TaskChange, TaskBuild, TaskOperate:
		steps = []PlanStep{
			{ID: "inspect", Title: "检查工作区与相关文件", Description: "先读取必要上下文并确认现有状态", Status: PlanStepPending},
			{ID: "edit", Title: "修改目标实现", Description: "仅通过已审批的 Workspace 操作应用变更", DependsOn: []string{"inspect"}, Status: PlanStepPending},
			{ID: "verify", Title: "运行目标验证", Description: "记录格式化、测试或其他必要验证的结构化结果", DependsOn: []string{"edit"}, Status: PlanStepPending},
		}
	case TaskInspect, TaskDiagnose, TaskReview:
		if strings.TrimSpace(invocation.WorkspaceID) == "" {
			return plan, false
		}
		steps = []PlanStep{{ID: "inspect", Title: "检查工作区与相关文件", Description: "读取并收集可核验的工作区证据", Status: PlanStepPending}}
	default:
		return plan, false
	}
	plan.Steps = steps
	plan.Status = PlanPending
	return plan, true
}

// WorkflowFailure is returned by a step executor when a step did not produce
// completion evidence. RetrySafe must be explicitly true: Retryable alone is
// not enough to repeat a potentially non-idempotent side effect.
type WorkflowFailure struct {
	Code      string                `json:"code"`
	Domain    WorkflowFailureDomain `json:"domain"`
	Message   string                `json:"message"`
	Retryable bool                  `json:"retryable"`
	RetrySafe bool                  `json:"retry_safe"`
}

var (
	ErrInvalidWorkflowPolicy = errors.New("工作流修正策略无效")
	ErrWorkflowStep          = errors.New("工作流步骤执行失败")
	ErrWorkflowBlocked       = errors.New("工作流已阻塞")
	ErrWorkflowBudget        = errors.New("工作流执行预算耗尽")
	ErrWorkflowActiveStep    = errors.New("工作流存在未确认的执行中步骤")
)

// PlanExecutionPolicy bounds both progress and repair. The defaults encode
// the documented guardrails: a transient runtime issue may be retried twice,
// while a verification target may receive three evidence-backed repairs.
type PlanExecutionPolicy struct {
	MaxStepExecutions      int `json:"max_step_executions"`
	MaxRuntimeRetries      int `json:"max_runtime_retries"`
	MaxVerificationRepairs int `json:"max_verification_repairs"`
}

const (
	DefaultMaxStepExecutions      = 100
	DefaultMaxRuntimeRetries      = 2
	DefaultMaxVerificationRepairs = 3
	MaxPlanStepExecutions         = 1000
	MaxPlanRetries                = 20
)

func DefaultPlanExecutionPolicy() PlanExecutionPolicy {
	return PlanExecutionPolicy{
		MaxStepExecutions:      DefaultMaxStepExecutions,
		MaxRuntimeRetries:      DefaultMaxRuntimeRetries,
		MaxVerificationRepairs: DefaultMaxVerificationRepairs,
	}
}

func (p PlanExecutionPolicy) Normalize() (PlanExecutionPolicy, error) {
	defaults := DefaultPlanExecutionPolicy()
	// An entirely zero value means "use defaults". Once a caller sets any
	// field, zero retry budgets remain meaningful and explicitly disable that
	// class of repair.
	if p.MaxStepExecutions == 0 && p.MaxRuntimeRetries == 0 && p.MaxVerificationRepairs == 0 {
		return defaults, nil
	}
	if p.MaxStepExecutions == 0 {
		p.MaxStepExecutions = defaults.MaxStepExecutions
	}
	if p.MaxStepExecutions < 1 || p.MaxStepExecutions > MaxPlanStepExecutions {
		return PlanExecutionPolicy{}, fmt.Errorf("%w: max_step_executions 必须在 1-%d 之间", ErrInvalidWorkflowPolicy, MaxPlanStepExecutions)
	}
	if p.MaxRuntimeRetries < 0 || p.MaxRuntimeRetries > MaxPlanRetries {
		return PlanExecutionPolicy{}, fmt.Errorf("%w: max_runtime_retries 必须在 0-%d 之间", ErrInvalidWorkflowPolicy, MaxPlanRetries)
	}
	if p.MaxVerificationRepairs < 0 || p.MaxVerificationRepairs > MaxPlanRetries {
		return PlanExecutionPolicy{}, fmt.Errorf("%w: max_verification_repairs 必须在 0-%d 之间", ErrInvalidWorkflowPolicy, MaxPlanRetries)
	}
	return p, nil
}

func (p PlanExecutionPolicy) retryLimit(domain WorkflowFailureDomain) int {
	switch normalizeWorkflowFailureDomain(domain) {
	case WorkflowFailureVerification:
		return p.MaxVerificationRepairs
	case WorkflowFailureRuntime, WorkflowFailureModel, WorkflowFailureTool:
		return p.MaxRuntimeRetries
	default:
		return 0
	}
}

func normalizeWorkflowFailureDomain(domain WorkflowFailureDomain) WorkflowFailureDomain {
	switch WorkflowFailureDomain(strings.ToLower(strings.TrimSpace(string(domain)))) {
	case WorkflowFailureVerification:
		return WorkflowFailureVerification
	case WorkflowFailureRuntime:
		return WorkflowFailureRuntime
	case WorkflowFailureModel:
		return WorkflowFailureModel
	case WorkflowFailureTool:
		return WorkflowFailureTool
	case WorkflowFailureWorkspace:
		return WorkflowFailureWorkspace
	case WorkflowFailureEnvironment:
		return WorkflowFailureEnvironment
	case WorkflowFailureUser:
		return WorkflowFailureUser
	default:
		return WorkflowFailureUnknown
	}
}

func normalizeWorkflowFailure(failure *WorkflowFailure) WorkflowFailure {
	if failure == nil {
		return WorkflowFailure{Code: "step_execution_failed", Domain: WorkflowFailureRuntime, Message: "步骤执行器未返回完成结果"}
	}
	copyFailure := *failure
	copyFailure.Code = strings.TrimSpace(copyFailure.Code)
	if copyFailure.Code == "" {
		copyFailure.Code = "step_execution_failed"
	}
	copyFailure.Domain = normalizeWorkflowFailureDomain(copyFailure.Domain)
	copyFailure.Message = strings.TrimSpace(copyFailure.Message)
	if copyFailure.Message == "" {
		copyFailure.Message = copyFailure.Code
	}
	// Step failures are persisted in plan metadata and runtime notices. Keep
	// their size bounded and redact common credentials before crossing either
	// boundary.
	copyFailure.Code = sanitizeRuntimeString(copyFailure.Code)
	copyFailure.Message = sanitizeRuntimeString(copyFailure.Message)
	if len(copyFailure.Code) > 160 {
		copyFailure.Code = copyFailure.Code[:160]
	}
	if len(copyFailure.Message) > 4096 {
		copyFailure.Message = copyFailure.Message[:4096]
	}
	return copyFailure
}

// PlanStepExecutionResult is the only output accepted from an executor. A
// successful step must provide at least one evidence reference; this keeps
// completion claims auditable after a restart.
type PlanStepExecutionResult struct {
	Evidence []EvidenceRef    `json:"evidence,omitempty"`
	Failure  *WorkflowFailure `json:"failure,omitempty"`
}

// PlanStepExecutor is deliberately injected. It may call approved tools and
// Verification services, but the controller itself never executes a command
// or writes a file, so retries cannot bypass the normal side-effect policy.
type PlanStepExecutor func(context.Context, PlanStep) (PlanStepExecutionResult, error)

type PlanExecutionResult struct {
	Plan           TaskPlan         `json:"plan"`
	StepExecutions int              `json:"step_executions"`
	Repairs        int              `json:"repairs"`
	Failure        *WorkflowFailure `json:"failure,omitempty"`
}

// WorkflowBlockedError exposes a stable error while retaining the plan's
// durable blocker as the source of truth.
type WorkflowBlockedError struct {
	StepID  string
	Failure WorkflowFailure
	Reason  string
}

func (e *WorkflowBlockedError) Error() string {
	if e == nil {
		return ErrWorkflowBlocked.Error()
	}
	return fmt.Sprintf("%s: step=%s code=%s reason=%s", ErrWorkflowBlocked, e.StepID, e.Failure.Code, e.Reason)
}

func (e *WorkflowBlockedError) Unwrap() error { return ErrWorkflowBlocked }

// WorkflowBudgetError reports a finite execution guard rather than silently
// looping. The current plan remains durable and can be inspected or resumed
// by a new explicit invocation.
type WorkflowBudgetError struct {
	Executions int
	Limit      int
}

func (e *WorkflowBudgetError) Error() string {
	if e == nil {
		return ErrWorkflowBudget.Error()
	}
	return fmt.Sprintf("%s: executions=%d limit=%d", ErrWorkflowBudget, e.Executions, e.Limit)
}

func (e *WorkflowBudgetError) Unwrap() error { return ErrWorkflowBudget }

func workflowEvidenceValid(evidence []EvidenceRef) bool {
	if len(evidence) == 0 {
		return false
	}
	for _, item := range evidence {
		if strings.TrimSpace(item.Kind) == "" || strings.TrimSpace(item.Ref) == "" {
			return false
		}
	}
	return true
}

// ensurePlanStepStarted admits the deterministic next plan step without
// running a tool. It is the small default-Kernel bridge between a structured
// model plan and the existing ADK/Workspace execution path: the active step
// is durable before any subsequent tool/approval event is processed. A CAS
// conflict means another actor already advanced or edited the plan; reload
// on the next event instead of failing an otherwise valid Invocation.
func (c *Coordinator) ensurePlanStepStarted(ctx context.Context, invocationID, reason string) error {
	if c == nil || strings.TrimSpace(invocationID) == "" {
		return nil
	}
	if _, ok := c.repo.(TaskPlanRepository); !ok {
		return nil
	}
	invocation, err := c.repo.GetInvocation(ctx, strings.TrimSpace(invocationID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	// A late verification/operation notification must never reopen a terminal
	// Invocation by admitting a fresh active step. The plan remains durable for
	// audit, but a new user invocation is required to continue work.
	if invocation.Status.Terminal() {
		return nil
	}
	plan, err := c.GetTaskPlan(ctx, invocationID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if plan.Status != PlanPending || plan.CurrentStepID != "" || len(plan.Steps) == 0 {
		return nil
	}
	step, err := plan.NextRunnableStep()
	if err != nil {
		if errors.Is(err, ErrPlanNoRunnableStep) {
			// Dependencies may be waiting for an explicit completion update;
			// do not invent a plan-level blocker from a transient proposal.
			return nil
		}
		return err
	}
	started, err := plan.StartStep(step.ID, time.Now().UTC())
	if err != nil {
		return err
	}
	if _, err := c.UpdateTaskPlan(ctx, invocationID, started, reason); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil
		}
		return err
	}
	return nil
}

// ExecuteTaskPlan runs a bounded, dependency-aware plan. It persists every
// start/repair/completion transition through Coordinator's CAS API. An
// executor failure is retried only when its domain policy, Retryable flag and
// explicit RetrySafe flag all allow it; otherwise the active step is blocked.
func (c *Coordinator) ExecuteTaskPlan(ctx context.Context, invocationID string, policy PlanExecutionPolicy, execute PlanStepExecutor) (PlanExecutionResult, error) {
	if c == nil {
		return PlanExecutionResult{}, errors.New("Runtime Coordinator 不能为空")
	}
	if execute == nil {
		return PlanExecutionResult{}, fmt.Errorf("%w: step executor 不能为空", ErrWorkflowStep)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.ensureStarted(); err != nil {
		return PlanExecutionResult{}, err
	}
	policy, err := policy.Normalize()
	if err != nil {
		return PlanExecutionResult{}, err
	}
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return PlanExecutionResult{}, fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidPlan)
	}
	if _, err := c.repo.GetInvocation(ctx, invocationID); err != nil {
		return PlanExecutionResult{}, err
	}
	result := PlanExecutionResult{}
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		plan, err := c.GetTaskPlan(ctx, invocationID)
		if err != nil {
			return result, err
		}
		result.Plan = plan
		if plan.Status == PlanCompleted {
			return result, nil
		}
		if plan.Status == PlanBlocked {
			failure := WorkflowFailure{Code: "plan_blocked", Domain: WorkflowFailureUnknown, Message: plan.Blocker}
			result.Failure = &failure
			return result, &WorkflowBlockedError{StepID: plan.CurrentStepID, Failure: failure, Reason: plan.Blocker}
		}
		if plan.CurrentStepID != "" {
			// A previous worker may have died after persisting start and before
			// the executor returned. Re-running it could duplicate a side effect;
			// leave the step for explicit recovery instead of guessing.
			failure := WorkflowFailure{Code: "active_step_unknown", Domain: WorkflowFailureRuntime, Message: "已有 in_progress 步骤，无法确认上次执行是否产生副作用"}
			result.Failure = &failure
			return result, fmt.Errorf("%w: step=%s: %s", ErrWorkflowActiveStep, plan.CurrentStepID, failure.Message)
		}
		step, nextErr := plan.NextRunnableStep()
		if nextErr != nil {
			failure := WorkflowFailure{Code: "plan_no_runnable_step", Domain: WorkflowFailureRuntime, Message: "计划存在未完成步骤但没有满足依赖的可运行步骤"}
			// A dependency deadlock is represented at plan level, preserving
			// every step state for diagnosis instead of inventing a blocked step.
			blocked := plan
			blocked.Blocker = failure.Message
			blocked.Status = PlanBlocked
			blocked.UpdatedAt = time.Now().UTC()
			blocked.Revision = plan.Revision
			updated, updateErr := c.UpdateTaskPlan(ctx, invocationID, blocked, "workflow blocked: no runnable step")
			if updateErr != nil {
				return result, updateErr
			}
			result.Plan = updated
			result.Failure = &failure
			return result, &WorkflowBlockedError{Failure: failure, Reason: failure.Message}
		}
		if result.StepExecutions >= policy.MaxStepExecutions {
			return result, &WorkflowBudgetError{Executions: result.StepExecutions, Limit: policy.MaxStepExecutions}
		}
		started, err := plan.StartStep(step.ID, time.Now().UTC())
		if err != nil {
			return result, err
		}
		started, err = c.UpdateTaskPlan(ctx, invocationID, started, "workflow step started")
		if err != nil {
			return result, err
		}
		result.Plan = started
		result.StepExecutions++
		execution, executeErr := execute(ctx, step)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		var failure WorkflowFailure
		if executeErr != nil {
			failure = normalizeWorkflowFailure(&WorkflowFailure{Code: "step_executor_error", Domain: WorkflowFailureRuntime, Message: executeErr.Error()})
		} else if execution.Failure != nil {
			failure = normalizeWorkflowFailure(execution.Failure)
		} else if !workflowEvidenceValid(execution.Evidence) {
			failure = normalizeWorkflowFailure(&WorkflowFailure{Code: "missing_completion_evidence", Domain: WorkflowFailureVerification, Message: "步骤执行器未返回有效完成证据"})
		} else {
			completed, completeErr := started.CompleteStep(step.ID, execution.Evidence, time.Now().UTC())
			if completeErr != nil {
				return result, completeErr
			}
			completed, completeErr = c.UpdateTaskPlan(ctx, invocationID, completed, "workflow step completed")
			if completeErr != nil {
				return result, completeErr
			}
			result.Plan = completed
			continue
		}

		failureLimit := policy.retryLimit(failure.Domain)
		canRetry := failure.Retryable && failure.RetrySafe && started.CurrentStepID == step.ID && step.RepairAttempts < failureLimit
		if canRetry {
			retry, retryErr := started.RetryStep(step.ID, failure.Code, failure.Message, time.Now().UTC())
			if retryErr != nil {
				return result, retryErr
			}
			retry, retryErr = c.UpdateTaskPlan(ctx, invocationID, retry, "workflow repair scheduled")
			if retryErr != nil {
				return result, retryErr
			}
			result.Plan = retry
			result.Repairs++
			repairAttempt := 0
			if retryIndex := stepIndex(retry, step.ID); retryIndex >= 0 {
				repairAttempt = retry.Steps[retryIndex].RepairAttempts
			}
			_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventRuntimeNotice, Timestamp: time.Now().UTC(), Data: map[string]any{
				"code": "workflow_repair_scheduled", "step_id": step.ID, "failure_code": failure.Code, "domain": failure.Domain,
				"repair_attempt": repairAttempt, "repair_limit": failureLimit,
			}})
			continue
		}
		blocker := failure.Code + ": " + failure.Message
		if len(blocker) > 4096 {
			blocker = blocker[:4096]
		}
		blocked, blockErr := started.BlockStep(step.ID, blocker, time.Now().UTC())
		if blockErr != nil {
			return result, blockErr
		}
		blocked, blockErr = c.UpdateTaskPlan(ctx, invocationID, blocked, "workflow step blocked")
		if blockErr != nil {
			return result, blockErr
		}
		result.Plan = blocked
		result.Failure = &failure
		_, _ = c.appendAndPublish(context.WithoutCancel(ctx), AgentEvent{ID: newID("event"), InvocationID: invocationID, Type: EventRuntimeNotice, Timestamp: time.Now().UTC(), Data: map[string]any{
			"code": "workflow_blocked", "step_id": step.ID, "failure_code": failure.Code, "domain": failure.Domain, "retryable": failure.Retryable, "retry_safe": failure.RetrySafe,
		}})
		return result, &WorkflowBlockedError{StepID: step.ID, Failure: failure, Reason: blocker}
	}
}

func stepIndex(plan TaskPlan, stepID string) int {
	for index, step := range plan.Steps {
		if step.ID == stepID {
			return index
		}
	}
	return -1
}
