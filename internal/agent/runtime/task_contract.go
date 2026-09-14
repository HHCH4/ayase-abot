package runtime

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// TaskType describes the requested work without granting permissions.
type TaskType string

const (
	TaskAnswer   TaskType = "answer"
	TaskInspect  TaskType = "inspect"
	TaskDiagnose TaskType = "diagnose"
	TaskChange   TaskType = "change"
	TaskBuild    TaskType = "build"
	TaskReview   TaskType = "review"
	TaskOperate  TaskType = "operate"
)

// Criterion is an explicit acceptance condition. Evidence is attached only
// when a criterion has actually been checked.
type Criterion struct {
	ID          string        `json:"id"`
	Description string        `json:"description"`
	Evidence    []EvidenceRef `json:"evidence,omitempty"`
}

type Constraint struct {
	Description string `json:"description"`
	Source      string `json:"source,omitempty"`
}

type ExternalActionRule struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

type TaskContract struct {
	InvocationID       string       `json:"invocation_id"`
	Version            int64        `json:"version"`
	TaskType           TaskType     `json:"task_type"`
	Goal               string       `json:"goal"`
	RequestedOutcome   string       `json:"requested_outcome,omitempty"`
	AcceptanceCriteria []Criterion  `json:"acceptance_criteria,omitempty"`
	Constraints        []Constraint `json:"constraints,omitempty"`
	NonGoals           []string     `json:"non_goals,omitempty"`
	MutationAllowed    bool         `json:"mutation_allowed"`
	// ValidationRequired is true when the requested outcome is not complete
	// until the Runtime has recorded an appropriate verification result.
	ValidationRequired bool                 `json:"validation_required"`
	ExternalActions    []ExternalActionRule `json:"external_actions,omitempty"`
	SourceMessageIDs   []string             `json:"source_message_ids"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

var ErrInvalidTaskContract = errors.New("任务契约无效")

func (c TaskContract) Validate() error {
	if strings.TrimSpace(c.InvocationID) == "" {
		return fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidTaskContract)
	}
	if c.Version <= 0 {
		return fmt.Errorf("%w: version 必须为正数", ErrInvalidTaskContract)
	}
	if !validTaskType(c.TaskType) {
		return fmt.Errorf("%w: task_type %q 不受支持", ErrInvalidTaskContract, c.TaskType)
	}
	if strings.TrimSpace(c.Goal) == "" {
		return fmt.Errorf("%w: goal 不能为空", ErrInvalidTaskContract)
	}
	if len(c.SourceMessageIDs) == 0 {
		return fmt.Errorf("%w: source_message_ids 不能为空", ErrInvalidTaskContract)
	}
	for _, id := range c.SourceMessageIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%w: source_message_ids 不能包含空值", ErrInvalidTaskContract)
		}
	}
	for _, criterion := range c.AcceptanceCriteria {
		if strings.TrimSpace(criterion.ID) == "" || strings.TrimSpace(criterion.Description) == "" {
			return fmt.Errorf("%w: acceptance criterion 必须包含 id 和 description", ErrInvalidTaskContract)
		}
		for _, evidence := range criterion.Evidence {
			if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Ref) == "" {
				return fmt.Errorf("%w: criterion %q 的 evidence 不完整", ErrInvalidTaskContract, criterion.ID)
			}
		}
	}
	criterionIDs := make(map[string]struct{}, len(c.AcceptanceCriteria))
	for _, criterion := range c.AcceptanceCriteria {
		id := strings.TrimSpace(criterion.ID)
		if _, exists := criterionIDs[id]; exists {
			return fmt.Errorf("%w: acceptance criteria id %q 重复", ErrInvalidTaskContract, id)
		}
		criterionIDs[id] = struct{}{}
	}
	for _, constraint := range c.Constraints {
		if strings.TrimSpace(constraint.Description) == "" {
			return fmt.Errorf("%w: constraint description 不能为空", ErrInvalidTaskContract)
		}
	}
	for _, nonGoal := range c.NonGoals {
		if strings.TrimSpace(nonGoal) == "" {
			return fmt.Errorf("%w: non_goals 不能包含空值", ErrInvalidTaskContract)
		}
	}
	for _, action := range c.ExternalActions {
		if strings.TrimSpace(action.Action) == "" {
			return fmt.Errorf("%w: external action 不能为空", ErrInvalidTaskContract)
		}
	}
	if !c.MutationAllowed && (c.TaskType == TaskChange || c.TaskType == TaskBuild || c.TaskType == TaskOperate) {
		return fmt.Errorf("%w: %s 任务必须明确 mutation_allowed", ErrInvalidTaskContract, c.TaskType)
	}
	if c.MutationAllowed && (c.TaskType == TaskAnswer || c.TaskType == TaskInspect || c.TaskType == TaskDiagnose || c.TaskType == TaskReview) {
		return fmt.Errorf("%w: %s 任务默认不能允许修改", ErrInvalidTaskContract, c.TaskType)
	}
	return nil
}

func validTaskType(value TaskType) bool {
	switch value {
	case TaskAnswer, TaskInspect, TaskDiagnose, TaskChange, TaskBuild, TaskReview, TaskOperate:
		return true
	default:
		return false
	}
}

// ClassifyTask is intentionally deterministic and conservative. It only
// infers a task class from explicit verbs; callers may replace it with a
// user-confirmed contract later.
func ClassifyTask(message string) TaskType {
	value := strings.ToLower(strings.TrimSpace(message))
	for _, marker := range []string{"修复", "修改", "实现", "添加", "删除", "重构", "更新", "改造", "fix", "implement", "add", "delete", "refactor", "change", "build"} {
		if strings.Contains(value, marker) {
			if strings.Contains(value, "实现") || strings.Contains(value, "build") || strings.Contains(value, "implement") {
				return TaskBuild
			}
			return TaskChange
		}
	}
	for _, marker := range []string{"评审", "审查", "review"} {
		if strings.Contains(value, marker) {
			return TaskReview
		}
	}
	for _, marker := range []string{"诊断", "原因", "排查", "diagnose", "why"} {
		if strings.Contains(value, marker) {
			return TaskDiagnose
		}
	}
	for _, marker := range []string{"检查", "查看", "看看", "inspect", "status", "diff"} {
		if strings.Contains(value, marker) {
			return TaskInspect
		}
	}
	return TaskAnswer
}

func InitialTaskContract(invocationID, message string, now time.Time) TaskContract {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	taskType := ClassifyTask(message)
	mutationAllowed := taskType == TaskChange || taskType == TaskBuild || taskType == TaskOperate
	goal := strings.TrimSpace(message)
	if goal == "" {
		goal = "处理用户提供的附件"
	}
	return TaskContract{InvocationID: strings.TrimSpace(invocationID), Version: 1, TaskType: taskType, Goal: goal, RequestedOutcome: goal, MutationAllowed: mutationAllowed, ValidationRequired: mutationAllowed, SourceMessageIDs: []string{"invocation:" + strings.TrimSpace(invocationID)}, UpdatedAt: now}
}
