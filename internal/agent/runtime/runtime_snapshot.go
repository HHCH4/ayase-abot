package runtime

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type PlanStepRef struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

type ApprovalRef struct {
	ID          string `json:"id"`
	ToolName    string `json:"tool_name,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

type ChangeSetRef struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision,omitempty"`
	Status   string `json:"status,omitempty"`
}

type VerificationRef struct {
	ID      string `json:"id"`
	Status  string `json:"status,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type Question struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status,omitempty"`
}

type Blocker struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type UnknownState struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type BudgetStatus struct {
	ContextWindow   int    `json:"context_window,omitempty"`
	OutputReserve   int    `json:"output_reserve,omitempty"`
	SafetyReserve   int    `json:"safety_reserve,omitempty"`
	EstimatedInput  int    `json:"estimated_input,omitempty"`
	ToolCallsUsed   int    `json:"tool_calls_used,omitempty"`
	ToolCallsLimit  int    `json:"tool_calls_limit,omitempty"`
	LastManifestID  string `json:"last_manifest_id,omitempty"`
	BudgetExhausted bool   `json:"budget_exhausted,omitempty"`
}

// RuntimeSnapshot is a deterministic, compact projection of durable Runtime
// entities. It is rebuilt from Invocation/Contract/Plan/Approval/ToolCall
// records rather than accepted as arbitrary model-authored JSON.
type RuntimeSnapshot struct {
	InvocationID       string             `json:"invocation_id"`
	Revision           int64              `json:"revision"`
	ContractVersion    int64              `json:"contract_version,omitempty"`
	PlanRevision       int64              `json:"plan_revision,omitempty"`
	Phase              string             `json:"phase"`
	WorkflowPhase      WorkflowPhase      `json:"workflow_phase,omitempty"`
	Workflow           WorkflowCheckpoint `json:"workflow,omitempty"`
	ActivePlanStep     *PlanStepRef       `json:"active_plan_step,omitempty"`
	PendingApprovals   []ApprovalRef      `json:"pending_approvals,omitempty"`
	AppliedChangeSets  []ChangeSetRef     `json:"applied_change_sets,omitempty"`
	VerificationRuns   []VerificationRef  `json:"verification_runs,omitempty"`
	OpenQuestions      []Question         `json:"open_questions,omitempty"`
	Blockers           []Blocker          `json:"blockers,omitempty"`
	UnknownStates      []UnknownState     `json:"unknown_states,omitempty"`
	WorkingSetRevision int64              `json:"working_set_revision,omitempty"`
	Budget             BudgetStatus       `json:"budget"`
	GeneratedAt        time.Time          `json:"generated_at"`
}

var ErrInvalidRuntimeSnapshot = errors.New("Runtime Snapshot 无效")

func (s RuntimeSnapshot) Validate() error {
	if strings.TrimSpace(s.InvocationID) == "" {
		return fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidRuntimeSnapshot)
	}
	if s.Revision <= 0 {
		return fmt.Errorf("%w: revision 必须为正数", ErrInvalidRuntimeSnapshot)
	}
	if strings.TrimSpace(s.Phase) == "" {
		return fmt.Errorf("%w: phase 不能为空", ErrInvalidRuntimeSnapshot)
	}
	if s.WorkflowPhase != "" && !validWorkflowPhase(s.WorkflowPhase) {
		return fmt.Errorf("%w: workflow_phase %q 不受支持", ErrInvalidRuntimeSnapshot, s.WorkflowPhase)
	}
	if err := s.Workflow.Validate(); err != nil {
		return fmt.Errorf("%w: workflow: %v", ErrInvalidRuntimeSnapshot, err)
	}
	if s.Budget.ContextWindow < 0 || s.Budget.OutputReserve < 0 || s.Budget.SafetyReserve < 0 || s.Budget.EstimatedInput < 0 || s.Budget.ToolCallsUsed < 0 || s.Budget.ToolCallsLimit < 0 {
		return fmt.Errorf("%w: budget 不能包含负数", ErrInvalidRuntimeSnapshot)
	}
	return nil
}
