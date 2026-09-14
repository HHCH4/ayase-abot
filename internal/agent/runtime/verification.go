package runtime

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type VerificationStatus string

const (
	VerificationPending    VerificationStatus = "pending"
	VerificationRunning    VerificationStatus = "running"
	VerificationPassed     VerificationStatus = "passed"
	VerificationFailed     VerificationStatus = "failed"
	VerificationBlocked    VerificationStatus = "blocked"
	VerificationCancelled  VerificationStatus = "cancelled"
	VerificationUnknown    VerificationStatus = "unknown"
	VerificationSuperseded VerificationStatus = "superseded"
)

func (s VerificationStatus) Terminal() bool {
	return s == VerificationPassed || s == VerificationFailed || s == VerificationBlocked || s == VerificationCancelled || s == VerificationUnknown || s == VerificationSuperseded
}

// VerificationRun is the durable evidence record for one validation attempt.
// Output is referenced by digest/ref; large command logs remain in Artifact or
// CommandRun storage rather than being copied into the Runtime snapshot.
type VerificationRun struct {
	ID           string             `json:"id"`
	InvocationID string             `json:"invocation_id"`
	PlanStepID   string             `json:"plan_step_id,omitempty"`
	Kind         string             `json:"kind"`
	Command      string             `json:"command,omitempty"`
	Status       VerificationStatus `json:"status"`
	ExitCode     *int               `json:"exit_code,omitempty"`
	Summary      string             `json:"summary,omitempty"`
	OutputRef    string             `json:"output_ref,omitempty"`
	OutputDigest string             `json:"output_digest,omitempty"`
	Revision     int64              `json:"revision"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
	StartedAt    *time.Time         `json:"started_at,omitempty"`
	FinishedAt   *time.Time         `json:"finished_at,omitempty"`
}

var (
	ErrInvalidVerification  = errors.New("VerificationRun 无效")
	ErrVerificationPlanLink = errors.New("VerificationRun 无法关联任务计划步骤")
)

func (v VerificationRun) Validate() error {
	if strings.TrimSpace(v.ID) == "" || strings.TrimSpace(v.InvocationID) == "" {
		return fmt.Errorf("%w: id 和 invocation_id 不能为空", ErrInvalidVerification)
	}
	if strings.TrimSpace(v.Kind) == "" {
		return fmt.Errorf("%w: kind 不能为空", ErrInvalidVerification)
	}
	if v.Revision <= 0 {
		return fmt.Errorf("%w: revision 必须为正数", ErrInvalidVerification)
	}
	if !validVerificationStatus(v.Status) {
		return fmt.Errorf("%w: status %q 不受支持", ErrInvalidVerification, v.Status)
	}
	if v.ExitCode != nil && (*v.ExitCode < -1 || *v.ExitCode > 255) {
		return fmt.Errorf("%w: exit_code 超出范围", ErrInvalidVerification)
	}
	if len(v.Summary) > 4096 || len(v.Command) > 4096 || len(v.OutputRef) > 4096 || len(v.OutputDigest) > 256 {
		return fmt.Errorf("%w: 文本字段过长", ErrInvalidVerification)
	}
	return nil
}

func validVerificationStatus(status VerificationStatus) bool {
	switch status {
	case VerificationPending, VerificationRunning, VerificationPassed, VerificationFailed, VerificationBlocked, VerificationCancelled, VerificationUnknown, VerificationSuperseded:
		return true
	default:
		return false
	}
}
