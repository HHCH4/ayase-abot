package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PlanStepStatus is the durable state of one user-visible task step.
type PlanStepStatus string

const (
	PlanStepPending    PlanStepStatus = "pending"
	PlanStepInProgress PlanStepStatus = "in_progress"
	PlanStepCompleted  PlanStepStatus = "completed"
	PlanStepBlocked    PlanStepStatus = "blocked"
	PlanStepSkipped    PlanStepStatus = "skipped"
)

// PlanStatus is derived from the step states unless a plan-level blocker is
// present. Keeping it explicit makes the current workflow state cheap to
// render without replaying every plan.updated event.
type PlanStatus string

const (
	PlanPending    PlanStatus = "pending"
	PlanInProgress PlanStatus = "in_progress"
	PlanCompleted  PlanStatus = "completed"
	PlanBlocked    PlanStatus = "blocked"
)

// EvidenceRef points at a durable event, tool call, verification or artifact.
// The referenced content stays in its owning store; a plan only keeps the
// small, reviewable pointer and optional summary.
type EvidenceRef struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Summary string `json:"summary,omitempty"`
}

// PlanStep is deliberately provider-neutral. A model may propose a plan, but
// Runtime validates the shape before it becomes durable state.
type PlanStep struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Status      PlanStepStatus `json:"status"`
	DependsOn   []string       `json:"depends_on,omitempty"`
	Evidence    []EvidenceRef  `json:"evidence,omitempty"`
	Blocker     string         `json:"blocker,omitempty"`
	// RepairAttempts counts bounded workflow repair loops for this step. It is
	// persisted inside the existing StepsJSON payload, so adding the field does
	// not require a database migration and old plans remain readable.
	RepairAttempts  int       `json:"repair_attempts,omitempty"`
	LastFailureCode string    `json:"last_failure_code,omitempty"`
	LastFailure     string    `json:"last_failure,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// TaskPlan is the durable structured plan attached to one Invocation. The
// revision is used as a compare-and-swap token so a stale WebUI update cannot
// overwrite a newer model/runtime update.
type TaskPlan struct {
	ID            string     `json:"id"`
	InvocationID  string     `json:"invocation_id"`
	Revision      int64      `json:"revision"`
	Status        PlanStatus `json:"status"`
	CurrentStepID string     `json:"current_step_id,omitempty"`
	Blocker       string     `json:"blocker,omitempty"`
	Steps         []PlanStep `json:"steps"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func (p TaskPlan) CurrentStep() *PlanStep {
	if strings.TrimSpace(p.CurrentStepID) == "" {
		return nil
	}
	for _, step := range p.Steps {
		if step.ID == p.CurrentStepID {
			copyStep := step
			copyStep.DependsOn = append([]string(nil), step.DependsOn...)
			copyStep.Evidence = append([]EvidenceRef(nil), step.Evidence...)
			return &copyStep
		}
	}
	return nil
}

var ErrInvalidPlan = errors.New("任务计划无效")

var (
	ErrPlanStepActive     = errors.New("任务计划已有执行中的步骤")
	ErrPlanNoRunnableStep = errors.New("任务计划当前没有可运行步骤")
	ErrPlanStepNotFound   = errors.New("任务计划步骤不存在")
	ErrPlanStepNotReady   = errors.New("任务计划步骤依赖尚未完成")
)

const MaxTaskPlanSteps = 100

// Validate checks invariants that must hold regardless of who proposed the
// plan. It intentionally does not validate whether an evidence Ref exists;
// that belongs to the owning event/tool/verification repository.
func (p TaskPlan) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("%w: id 不能为空", ErrInvalidPlan)
	}
	if strings.TrimSpace(p.InvocationID) == "" {
		return fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidPlan)
	}
	if p.Revision <= 0 {
		return fmt.Errorf("%w: revision 必须为正数", ErrInvalidPlan)
	}
	if p.Status != "" && !validPlanStatus(p.Status) {
		return fmt.Errorf("%w: status %q 不受支持", ErrInvalidPlan, p.Status)
	}
	if len(p.Steps) > MaxTaskPlanSteps {
		return fmt.Errorf("%w: steps 不能超过 %d 个", ErrInvalidPlan, MaxTaskPlanSteps)
	}
	seen := make(map[string]struct{}, len(p.Steps))
	inProgress := 0
	for _, step := range p.Steps {
		stepID := strings.TrimSpace(step.ID)
		if stepID == "" {
			return fmt.Errorf("%w: step id 不能为空", ErrInvalidPlan)
		}
		if strings.TrimSpace(step.Title) == "" {
			return fmt.Errorf("%w: step %q 的 title 不能为空", ErrInvalidPlan, stepID)
		}
		if _, exists := seen[stepID]; exists {
			return fmt.Errorf("%w: step id %q 重复", ErrInvalidPlan, stepID)
		}
		seen[stepID] = struct{}{}
		if !validPlanStepStatus(step.Status) {
			return fmt.Errorf("%w: step %q 的 status %q 不受支持", ErrInvalidPlan, stepID, step.Status)
		}
		if step.Status == PlanStepInProgress {
			inProgress++
		}
		if step.Status == PlanStepCompleted && len(step.Evidence) == 0 {
			return fmt.Errorf("%w: 已完成步骤 %q 必须包含 evidence", ErrInvalidPlan, stepID)
		}
		for _, evidence := range step.Evidence {
			if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Ref) == "" {
				return fmt.Errorf("%w: 步骤 %q 的 evidence 必须包含 kind 和 ref", ErrInvalidPlan, stepID)
			}
		}
		if step.Status == PlanStepBlocked && strings.TrimSpace(step.Blocker) == "" {
			return fmt.Errorf("%w: blocked 步骤 %q 必须包含 blocker", ErrInvalidPlan, stepID)
		}
		if step.RepairAttempts < 0 {
			return fmt.Errorf("%w: 步骤 %q 的 repair_attempts 不能为负数", ErrInvalidPlan, stepID)
		}
		if len(step.LastFailureCode) > 160 || len(step.LastFailure) > 4096 {
			return fmt.Errorf("%w: 步骤 %q 的失败信息过长", ErrInvalidPlan, stepID)
		}
	}
	if inProgress > 1 {
		return fmt.Errorf("%w: 同时只能有一个 in_progress 步骤", ErrInvalidPlan)
	}
	for _, step := range p.Steps {
		for _, dependency := range step.DependsOn {
			dependency = strings.TrimSpace(dependency)
			if dependency == "" {
				return fmt.Errorf("%w: step %q 存在空依赖", ErrInvalidPlan, step.ID)
			}
			if dependency == step.ID {
				return fmt.Errorf("%w: step %q 不能依赖自身", ErrInvalidPlan, step.ID)
			}
			if _, exists := seen[dependency]; !exists {
				return fmt.Errorf("%w: step %q 依赖不存在的步骤 %q", ErrInvalidPlan, step.ID, dependency)
			}
		}
	}
	// A step can only claim progress or completion after all dependencies have
	// reached a terminal-success state. Pending descendants remain valid, but
	// an in-progress/completed/skipped step with an unfinished dependency would
	// make recovery ambiguous and is rejected before persistence.
	for _, step := range p.Steps {
		if step.Status != PlanStepInProgress && step.Status != PlanStepCompleted && step.Status != PlanStepSkipped {
			continue
		}
		for _, dependency := range step.DependsOn {
			dependency = strings.TrimSpace(dependency)
			for _, candidate := range p.Steps {
				if candidate.ID != dependency {
					continue
				}
				if candidate.Status != PlanStepCompleted && candidate.Status != PlanStepSkipped {
					return fmt.Errorf("%w: 步骤 %q 的依赖 %q 当前为 %s", ErrInvalidPlan, step.ID, dependency, candidate.Status)
				}
				break
			}
		}
	}
	states := make(map[string]uint8, len(p.Steps))
	var visit func(string) error
	visit = func(stepID string) error {
		switch states[stepID] {
		case 1:
			return fmt.Errorf("%w: 计划依赖存在循环 (%s)", ErrInvalidPlan, stepID)
		case 2:
			return nil
		}
		states[stepID] = 1
		for _, step := range p.Steps {
			if step.ID != stepID {
				continue
			}
			for _, dependency := range step.DependsOn {
				if err := visit(strings.TrimSpace(dependency)); err != nil {
					return err
				}
			}
			break
		}
		states[stepID] = 2
		return nil
	}
	for _, step := range p.Steps {
		if err := visit(step.ID); err != nil {
			return err
		}
	}
	if p.CurrentStepID != "" {
		if _, ok := seen[strings.TrimSpace(p.CurrentStepID)]; !ok {
			return fmt.Errorf("%w: current_step_id %q 不存在", ErrInvalidPlan, p.CurrentStepID)
		}
		for _, step := range p.Steps {
			if step.ID == p.CurrentStepID && step.Status != PlanStepInProgress {
				return fmt.Errorf("%w: current_step_id %q 不是 in_progress", ErrInvalidPlan, p.CurrentStepID)
			}
		}
	}
	if p.Status == PlanCompleted {
		for _, step := range p.Steps {
			if step.Status != PlanStepCompleted && step.Status != PlanStepSkipped {
				return fmt.Errorf("%w: plan completed 但步骤 %q 仍为 %s", ErrInvalidPlan, step.ID, step.Status)
			}
		}
	}
	if p.Status == PlanBlocked && strings.TrimSpace(p.Blocker) == "" {
		blockedStep := false
		for _, step := range p.Steps {
			if step.Status == PlanStepBlocked {
				blockedStep = true
				break
			}
		}
		if !blockedStep {
			return fmt.Errorf("%w: plan blocked 必须包含 blocker 或 blocked 步骤", ErrInvalidPlan)
		}
	}
	return nil
}

// RunnableSteps returns a stable copy of pending steps whose dependencies are
// completed or skipped. It never mutates the plan and sorts by step ID so
// callers do not depend on model-provided slice order.
func (p TaskPlan) RunnableSteps() ([]PlanStep, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Status == PlanBlocked {
		return nil, fmt.Errorf("%w: plan 已 blocked", ErrPlanNoRunnableStep)
	}
	if p.Status == PlanCompleted {
		return nil, fmt.Errorf("%w: plan 已 completed", ErrPlanNoRunnableStep)
	}
	if p.CurrentStepID != "" {
		return nil, ErrPlanStepActive
	}
	stepsByID := make(map[string]PlanStep, len(p.Steps))
	for _, step := range p.Steps {
		stepsByID[step.ID] = step
	}
	result := make([]PlanStep, 0)
	for _, step := range p.Steps {
		if step.Status != PlanStepPending {
			continue
		}
		ready := true
		for _, dependency := range step.DependsOn {
			dependencyStep := stepsByID[strings.TrimSpace(dependency)]
			if dependencyStep.Status != PlanStepCompleted && dependencyStep.Status != PlanStepSkipped {
				ready = false
				break
			}
		}
		if ready {
			step.DependsOn = append([]string(nil), step.DependsOn...)
			step.Evidence = append([]EvidenceRef(nil), step.Evidence...)
			result = append(result, step)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	if len(result) == 0 {
		return nil, ErrPlanNoRunnableStep
	}
	return result, nil
}

// NextRunnableStep returns the deterministic next step in a plan.
func (p TaskPlan) NextRunnableStep() (PlanStep, error) {
	steps, err := p.RunnableSteps()
	if err != nil {
		return PlanStep{}, err
	}
	return steps[0], nil
}

// StartNextStep marks the deterministic next pending step as in progress.
// Revision changes are intentionally left to Coordinator's CAS boundary.
func (p TaskPlan) StartNextStep(now time.Time) (TaskPlan, error) {
	step, err := p.NextRunnableStep()
	if err != nil {
		return TaskPlan{}, err
	}
	return p.StartStep(step.ID, now)
}

// StartStep marks one runnable pending step as in progress.
func (p TaskPlan) StartStep(stepID string, now time.Time) (TaskPlan, error) {
	if err := p.Validate(); err != nil {
		return TaskPlan{}, err
	}
	if p.CurrentStepID != "" {
		return TaskPlan{}, ErrPlanStepActive
	}
	stepID = strings.TrimSpace(stepID)
	if stepID == "" {
		return TaskPlan{}, fmt.Errorf("%w: step id 不能为空", ErrPlanStepNotFound)
	}
	steps, err := p.RunnableSteps()
	if err != nil {
		return TaskPlan{}, err
	}
	index := -1
	for candidate := range steps {
		if steps[candidate].ID == stepID {
			index = candidate
			break
		}
	}
	if index < 0 {
		found := false
		for _, step := range p.Steps {
			if step.ID == stepID {
				found = true
				break
			}
		}
		if !found {
			return TaskPlan{}, fmt.Errorf("%w: %s", ErrPlanStepNotFound, stepID)
		}
		return TaskPlan{}, fmt.Errorf("%w: %s", ErrPlanStepNotReady, stepID)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for index := range p.Steps {
		if p.Steps[index].ID == stepID {
			p.Steps[index].Status = PlanStepInProgress
			p.Steps[index].UpdatedAt = now
			p.CurrentStepID = stepID
			break
		}
	}
	p.Status = PlanInProgress
	p.UpdatedAt = now
	return p, nil
}

// CompleteStep marks the active step complete and requires durable evidence.
func (p TaskPlan) CompleteStep(stepID string, evidence []EvidenceRef, now time.Time) (TaskPlan, error) {
	if err := p.Validate(); err != nil {
		return TaskPlan{}, err
	}
	stepID = strings.TrimSpace(stepID)
	if stepID == "" {
		return TaskPlan{}, fmt.Errorf("%w: step id 不能为空", ErrPlanStepNotFound)
	}
	if p.CurrentStepID != stepID {
		return TaskPlan{}, fmt.Errorf("%w: current=%q requested=%q", ErrPlanStepActive, p.CurrentStepID, stepID)
	}
	if len(evidence) == 0 {
		return TaskPlan{}, fmt.Errorf("%w: 完成步骤 %q 必须包含 evidence", ErrInvalidPlan, stepID)
	}
	for index := range evidence {
		evidence[index].Kind = strings.TrimSpace(evidence[index].Kind)
		evidence[index].Ref = strings.TrimSpace(evidence[index].Ref)
		evidence[index].Summary = strings.TrimSpace(evidence[index].Summary)
		if evidence[index].Kind == "" || evidence[index].Ref == "" {
			return TaskPlan{}, fmt.Errorf("%w: 步骤 %q 的 evidence 必须包含 kind 和 ref", ErrInvalidPlan, stepID)
		}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for index := range p.Steps {
		if p.Steps[index].ID != stepID {
			continue
		}
		p.Steps[index].Status = PlanStepCompleted
		p.Steps[index].Evidence = append([]EvidenceRef(nil), evidence...)
		p.Steps[index].Blocker = ""
		p.Steps[index].LastFailureCode = ""
		p.Steps[index].LastFailure = ""
		p.Steps[index].UpdatedAt = now
		break
	}
	p.CurrentStepID = ""
	p.UpdatedAt = now
	p.Status = derivePlanStatus(p)
	return p, p.Validate()
}

// RetryStep returns an in-memory plan with the active step pending again.
// The caller must have made a policy decision before invoking this helper.
func (p TaskPlan) RetryStep(stepID, failureCode, failure string, now time.Time) (TaskPlan, error) {
	if err := p.Validate(); err != nil {
		return TaskPlan{}, err
	}
	stepID = strings.TrimSpace(stepID)
	if p.CurrentStepID != stepID {
		return TaskPlan{}, fmt.Errorf("%w: current=%q requested=%q", ErrPlanStepActive, p.CurrentStepID, stepID)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	failureCode = strings.TrimSpace(failureCode)
	failure = strings.TrimSpace(failure)
	if len(failureCode) > 160 || len(failure) > 4096 {
		return TaskPlan{}, fmt.Errorf("%w: 失败信息过长", ErrInvalidPlan)
	}
	for index := range p.Steps {
		if p.Steps[index].ID != stepID {
			continue
		}
		p.Steps[index].Status = PlanStepPending
		p.Steps[index].Blocker = ""
		p.Steps[index].RepairAttempts++
		p.Steps[index].LastFailureCode = failureCode
		p.Steps[index].LastFailure = failure
		p.Steps[index].UpdatedAt = now
		break
	}
	p.CurrentStepID = ""
	p.Status = PlanPending
	p.UpdatedAt = now
	return p, p.Validate()
}

// BlockStep records a terminal workflow blocker without pretending the step
// completed or retrying an unsafe operation.
func (p TaskPlan) BlockStep(stepID, blocker string, now time.Time) (TaskPlan, error) {
	if err := p.Validate(); err != nil {
		return TaskPlan{}, err
	}
	stepID = strings.TrimSpace(stepID)
	blocker = strings.TrimSpace(blocker)
	if stepID == "" {
		return TaskPlan{}, fmt.Errorf("%w: step id 不能为空", ErrPlanStepNotFound)
	}
	if blocker == "" {
		return TaskPlan{}, fmt.Errorf("%w: blocker 不能为空", ErrInvalidPlan)
	}
	if len(blocker) > 4096 {
		return TaskPlan{}, fmt.Errorf("%w: blocker 过长", ErrInvalidPlan)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	found := false
	for index := range p.Steps {
		if p.Steps[index].ID != stepID {
			continue
		}
		found = true
		p.Steps[index].Status = PlanStepBlocked
		p.Steps[index].Blocker = blocker
		p.Steps[index].UpdatedAt = now
		break
	}
	if !found {
		return TaskPlan{}, fmt.Errorf("%w: %s", ErrPlanStepNotFound, stepID)
	}
	p.CurrentStepID = ""
	p.Blocker = blocker
	p.Status = PlanBlocked
	p.UpdatedAt = now
	return p, p.Validate()
}

// Normalize fills derived fields and trims externally supplied identifiers.
// The caller supplies now so tests and persistence paths can remain
// deterministic.
func (p TaskPlan) Normalize(now time.Time) (TaskPlan, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	p.ID = strings.TrimSpace(p.ID)
	p.InvocationID = strings.TrimSpace(p.InvocationID)
	p.CurrentStepID = strings.TrimSpace(p.CurrentStepID)
	p.Blocker = strings.TrimSpace(p.Blocker)
	if p.Revision <= 0 {
		p.Revision = 1
	}
	for index := range p.Steps {
		step := &p.Steps[index]
		step.ID = strings.TrimSpace(step.ID)
		step.Title = strings.TrimSpace(step.Title)
		step.Description = strings.TrimSpace(step.Description)
		step.Blocker = strings.TrimSpace(step.Blocker)
		step.LastFailureCode = strings.TrimSpace(step.LastFailureCode)
		step.LastFailure = strings.TrimSpace(step.LastFailure)
		if step.Status == "" {
			step.Status = PlanStepPending
		}
		if step.UpdatedAt.IsZero() {
			step.UpdatedAt = now
		} else {
			step.UpdatedAt = step.UpdatedAt.UTC()
		}
		for depIndex := range step.DependsOn {
			step.DependsOn[depIndex] = strings.TrimSpace(step.DependsOn[depIndex])
		}
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	} else {
		p.CreatedAt = p.CreatedAt.UTC()
	}
	p.UpdatedAt = now
	if p.CurrentStepID == "" {
		for _, step := range p.Steps {
			if step.Status == PlanStepInProgress {
				p.CurrentStepID = step.ID
				break
			}
		}
	}
	derivedStatus := derivePlanStatus(p)
	if p.Status == "" {
		p.Status = derivedStatus
	} else if p.Status != derivedStatus {
		return TaskPlan{}, fmt.Errorf("%w: status=%s 与步骤推导状态=%s 不一致", ErrInvalidPlan, p.Status, derivedStatus)
	}
	if err := p.Validate(); err != nil {
		return TaskPlan{}, err
	}
	return p, nil
}

func validPlanStatus(status PlanStatus) bool {
	switch status {
	case PlanPending, PlanInProgress, PlanCompleted, PlanBlocked:
		return true
	default:
		return false
	}
}

func validPlanStepStatus(status PlanStepStatus) bool {
	switch status {
	case PlanStepPending, PlanStepInProgress, PlanStepCompleted, PlanStepBlocked, PlanStepSkipped:
		return true
	default:
		return false
	}
}

func derivePlanStatus(plan TaskPlan) PlanStatus {
	if strings.TrimSpace(plan.Blocker) != "" {
		return PlanBlocked
	}
	allDone := len(plan.Steps) > 0
	for _, step := range plan.Steps {
		switch step.Status {
		case PlanStepBlocked:
			return PlanBlocked
		case PlanStepInProgress:
			return PlanInProgress
		case PlanStepPending:
			allDone = false
		}
	}
	if allDone {
		return PlanCompleted
	}
	return PlanPending
}

func initialTaskPlan(invocationID string, now time.Time) TaskPlan {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return TaskPlan{
		ID:           newID("plan"),
		InvocationID: strings.TrimSpace(invocationID),
		Revision:     1,
		Status:       PlanPending,
		Steps:        []PlanStep{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}
