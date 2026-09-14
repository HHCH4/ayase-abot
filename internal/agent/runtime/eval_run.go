package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EvalRunStatus is the durable lifecycle of a deterministic evaluation run.
// Deterministic runs are currently evaluated synchronously, but the explicit
// status keeps the persisted contract forward-compatible with queued suites.
type EvalRunStatus string

const (
	EvalRunQueued    EvalRunStatus = "queued"
	EvalRunRunning   EvalRunStatus = "running"
	EvalRunCompleted EvalRunStatus = "completed"
	EvalRunFailed    EvalRunStatus = "failed"
)

// EvalRun stores an immutable, metadata-only evaluation result. Definition
// and Result are JSON snapshots so a later code change cannot reinterpret an
// old run using a mutable case registry. They contain no model prompt,
// response, file content, command output or hidden reasoning.
type EvalRun struct {
	ID                   string          `json:"id"`
	InvocationID         string          `json:"invocation_id"`
	CaseID               string          `json:"case_id"`
	CaseVersion          string          `json:"case_version"`
	Kind                 string          `json:"kind"`
	Status               EvalRunStatus   `json:"status"`
	Passed               bool            `json:"passed"`
	HardFailure          bool            `json:"hard_failure"`
	AssertionCount       int             `json:"assertion_count"`
	FailedAssertions     int             `json:"failed_assertions"`
	HardFailedAssertions int             `json:"hard_failed_assertions"`
	Definition           json.RawMessage `json:"definition,omitempty"`
	Result               json.RawMessage `json:"result,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	FinishedAt           *time.Time      `json:"finished_at,omitempty"`
}

var ErrInvalidEvalRun = errors.New("EvalRun 无效")

func (r EvalRun) Validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.InvocationID) == "" {
		return fmt.Errorf("%w: id 和 invocation_id 不能为空", ErrInvalidEvalRun)
	}
	if strings.TrimSpace(r.CaseID) == "" || strings.TrimSpace(r.CaseVersion) == "" {
		return fmt.Errorf("%w: case_id 和 case_version 不能为空", ErrInvalidEvalRun)
	}
	if strings.TrimSpace(r.Kind) == "" {
		return fmt.Errorf("%w: kind 不能为空", ErrInvalidEvalRun)
	}
	switch r.Status {
	case EvalRunQueued, EvalRunRunning, EvalRunCompleted, EvalRunFailed:
	default:
		return fmt.Errorf("%w: status %q 不受支持", ErrInvalidEvalRun, r.Status)
	}
	if r.AssertionCount < 0 || r.FailedAssertions < 0 || r.HardFailedAssertions < 0 || r.FailedAssertions > r.AssertionCount || r.HardFailedAssertions > r.FailedAssertions {
		return fmt.Errorf("%w: assertion 计数无效", ErrInvalidEvalRun)
	}
	if len(r.Definition) > 2*1024*1024 || len(r.Result) > 2*1024*1024 {
		return fmt.Errorf("%w: JSON 快照过大", ErrInvalidEvalRun)
	}
	if len(r.Definition) > 0 && !json.Valid(r.Definition) {
		return fmt.Errorf("%w: definition 不是合法 JSON", ErrInvalidEvalRun)
	}
	if len(r.Result) > 0 && !json.Valid(r.Result) {
		return fmt.Errorf("%w: result 不是合法 JSON", ErrInvalidEvalRun)
	}
	if r.Status == EvalRunCompleted && len(bytes.TrimSpace(r.Result)) == 0 {
		return fmt.Errorf("%w: completed run 缺少 result", ErrInvalidEvalRun)
	}
	return nil
}

// EvalInput is the metadata projection consumed by deterministic eval. It is
// intentionally copied out of the Coordinator so eval code cannot mutate
// runtime state or access model input.
type EvalInput struct {
	Invocation Invocation
	Events     []AgentEvent
	ToolCalls  []ToolCall
	Approvals  []Approval
}

// EvalRunRepository is optional for embedders. SQLite and MemoryRepository
// implement it; the core Runtime Repository interface remains source
// compatible with older integrations.
type EvalRunRepository interface {
	CreateEvalRun(context.Context, EvalRun) error
	GetEvalRun(context.Context, string) (EvalRun, error)
	ListEvalRuns(context.Context, string) ([]EvalRun, error)
}
