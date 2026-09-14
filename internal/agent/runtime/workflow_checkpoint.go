package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// WorkflowCheckpointStatus is the durable, provider-neutral status of the
// orchestration state reconstructed from Invocation and AgentEvent facts.
// It deliberately does not replace InvocationStatus: an invocation can be
// queued or running while its workflow is waiting on an explicit boundary.
type WorkflowCheckpointStatus string

const (
	WorkflowCheckpointActive    WorkflowCheckpointStatus = "active"
	WorkflowCheckpointWaiting   WorkflowCheckpointStatus = "waiting"
	WorkflowCheckpointCompleted WorkflowCheckpointStatus = "completed"
	WorkflowCheckpointFailed    WorkflowCheckpointStatus = "failed"
	WorkflowCheckpointCancelled WorkflowCheckpointStatus = "cancelled"
	WorkflowCheckpointBlocked   WorkflowCheckpointStatus = "blocked"
)

// WorkflowBoundaryKind identifies the external continuation contract. A
// boundary is not an instruction to execute anything; it is a durable marker
// that says which part of a workflow still needs an external answer.
type WorkflowBoundaryKind string

const (
	WorkflowBoundaryTool     WorkflowBoundaryKind = "tool"
	WorkflowBoundaryApproval WorkflowBoundaryKind = "approval"
	WorkflowBoundaryUser     WorkflowBoundaryKind = "user"
)

type WorkflowBoundaryStatus string

const (
	WorkflowBoundaryWaiting   WorkflowBoundaryStatus = "waiting"
	WorkflowBoundaryResolved  WorkflowBoundaryStatus = "resolved"
	WorkflowBoundaryCompleted WorkflowBoundaryStatus = "completed"
	WorkflowBoundaryFailed    WorkflowBoundaryStatus = "failed"
	WorkflowBoundaryCancelled WorkflowBoundaryStatus = "cancelled"
)

type WorkflowBranchStatus string

const (
	WorkflowBranchActive    WorkflowBranchStatus = "active"
	WorkflowBranchWaiting   WorkflowBranchStatus = "waiting"
	WorkflowBranchCompleted WorkflowBranchStatus = "completed"
	WorkflowBranchFailed    WorkflowBranchStatus = "failed"
	WorkflowBranchCancelled WorkflowBranchStatus = "cancelled"
	WorkflowBranchBlocked   WorkflowBranchStatus = "blocked"
)

// Limits keep a replayed event stream from turning a recovery snapshot into
// an unbounded document. Older resolved boundaries are compacted after
// replay; all currently waiting boundaries are retained while they fit.
const (
	MaxWorkflowBranches        = 32
	MaxWorkflowBoundaries      = 64
	MaxWorkflowBoundaryWaitIDs = 16
	MaxWorkflowIDBytes         = 160
)

type WorkflowBoundary struct {
	ID               string                 `json:"id"`
	Sequence         int64                  `json:"sequence"`
	Kind             WorkflowBoundaryKind   `json:"kind"`
	Status           WorkflowBoundaryStatus `json:"status"`
	BranchID         string                 `json:"branch_id"`
	ParentBoundaryID string                 `json:"parent_boundary_id,omitempty"`
	StepID           string                 `json:"step_id,omitempty"`
	WaitIDs          []string               `json:"wait_ids,omitempty"`
	PendingWaitIDs   []string               `json:"pending_wait_ids,omitempty"`
	RequestDigest    string                 `json:"request_digest,omitempty"`
	Outcome          string                 `json:"outcome,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	ResolvedAt       *time.Time             `json:"resolved_at,omitempty"`
}

type WorkflowBranch struct {
	ID              string               `json:"id"`
	ParentBranchID  string               `json:"parent_branch_id,omitempty"`
	HeadBoundaryID  string               `json:"head_boundary_id,omitempty"`
	Status          WorkflowBranchStatus `json:"status"`
	StartedSequence int64                `json:"started_sequence"`
	UpdatedSequence int64                `json:"updated_sequence"`
}

// WorkflowCheckpoint is a compact state machine projection. It is persisted
// inside RuntimeSnapshot JSON, so adding this projection does not require a
// second database table or a cross-table transaction.
type WorkflowCheckpoint struct {
	Status            WorkflowCheckpointStatus `json:"status,omitempty"`
	CurrentBranchID   string                   `json:"current_branch_id,omitempty"`
	ActiveBoundaryIDs []string                 `json:"active_boundary_ids,omitempty"`
	Branches          []WorkflowBranch         `json:"branches,omitempty"`
	Boundaries        []WorkflowBoundary       `json:"boundaries,omitempty"`
}

var ErrInvalidWorkflowCheckpoint = errors.New("工作流恢复检查点无效")

func validWorkflowCheckpointStatus(status WorkflowCheckpointStatus) bool {
	switch status {
	case WorkflowCheckpointActive, WorkflowCheckpointWaiting, WorkflowCheckpointCompleted, WorkflowCheckpointFailed, WorkflowCheckpointCancelled, WorkflowCheckpointBlocked:
		return true
	default:
		return false
	}
}

func validWorkflowBoundaryKind(kind WorkflowBoundaryKind) bool {
	return kind == WorkflowBoundaryTool || kind == WorkflowBoundaryApproval || kind == WorkflowBoundaryUser
}

func validWorkflowBoundaryStatus(status WorkflowBoundaryStatus) bool {
	switch status {
	case WorkflowBoundaryWaiting, WorkflowBoundaryResolved, WorkflowBoundaryCompleted, WorkflowBoundaryFailed, WorkflowBoundaryCancelled:
		return true
	default:
		return false
	}
}

func validWorkflowBranchStatus(status WorkflowBranchStatus) bool {
	switch status {
	case WorkflowBranchActive, WorkflowBranchWaiting, WorkflowBranchCompleted, WorkflowBranchFailed, WorkflowBranchCancelled, WorkflowBranchBlocked:
		return true
	default:
		return false
	}
}

func validateWorkflowID(value, field string, required bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		if required {
			return fmt.Errorf("%w: %s 不能为空", ErrInvalidWorkflowCheckpoint, field)
		}
		return nil
	}
	if len(value) > MaxWorkflowIDBytes {
		return fmt.Errorf("%w: %s 过长", ErrInvalidWorkflowCheckpoint, field)
	}
	return nil
}

// Validate checks all references that must remain true after a process
// restart. It intentionally does not require a particular branch topology:
// future fan-out implementations can add parent/child branches without
// changing the snapshot format.
func (c WorkflowCheckpoint) Validate() error {
	if c.Status != "" && !validWorkflowCheckpointStatus(c.Status) {
		return fmt.Errorf("%w: status %q 不受支持", ErrInvalidWorkflowCheckpoint, c.Status)
	}
	if len(c.Branches) > MaxWorkflowBranches {
		return fmt.Errorf("%w: branches 不能超过 %d 个", ErrInvalidWorkflowCheckpoint, MaxWorkflowBranches)
	}
	if len(c.Boundaries) > MaxWorkflowBoundaries {
		return fmt.Errorf("%w: boundaries 不能超过 %d 个", ErrInvalidWorkflowCheckpoint, MaxWorkflowBoundaries)
	}
	if len(c.ActiveBoundaryIDs) > MaxWorkflowBoundaries {
		return fmt.Errorf("%w: active_boundary_ids 不能超过 %d 个", ErrInvalidWorkflowCheckpoint, MaxWorkflowBoundaries)
	}
	if err := validateWorkflowID(c.CurrentBranchID, "current_branch_id", false); err != nil {
		return err
	}
	branches := make(map[string]WorkflowBranch, len(c.Branches))
	for _, branch := range c.Branches {
		if err := validateWorkflowID(branch.ID, "branch.id", true); err != nil {
			return err
		}
		if !validWorkflowBranchStatus(branch.Status) {
			return fmt.Errorf("%w: branch %q status %q 不受支持", ErrInvalidWorkflowCheckpoint, branch.ID, branch.Status)
		}
		if branch.ID == strings.TrimSpace(branch.ParentBranchID) {
			return fmt.Errorf("%w: branch %q 不能以自身为 parent", ErrInvalidWorkflowCheckpoint, branch.ID)
		}
		if err := validateWorkflowID(branch.ParentBranchID, "branch.parent_branch_id", false); err != nil {
			return err
		}
		if err := validateWorkflowID(branch.HeadBoundaryID, "branch.head_boundary_id", false); err != nil {
			return err
		}
		if branch.StartedSequence < 0 || branch.UpdatedSequence < 0 {
			return fmt.Errorf("%w: branch %q sequence 不能为负数", ErrInvalidWorkflowCheckpoint, branch.ID)
		}
		if _, exists := branches[strings.TrimSpace(branch.ID)]; exists {
			return fmt.Errorf("%w: branch %q 重复", ErrInvalidWorkflowCheckpoint, branch.ID)
		}
		branches[strings.TrimSpace(branch.ID)] = branch
	}
	if c.CurrentBranchID != "" {
		if _, ok := branches[strings.TrimSpace(c.CurrentBranchID)]; !ok {
			return fmt.Errorf("%w: current_branch_id %q 不存在", ErrInvalidWorkflowCheckpoint, c.CurrentBranchID)
		}
	}
	boundaries := make(map[string]WorkflowBoundary, len(c.Boundaries))
	for _, boundary := range c.Boundaries {
		if err := validateWorkflowID(boundary.ID, "boundary.id", true); err != nil {
			return err
		}
		if boundary.Sequence < 0 {
			return fmt.Errorf("%w: boundary %q sequence 不能为负数", ErrInvalidWorkflowCheckpoint, boundary.ID)
		}
		if !validWorkflowBoundaryKind(boundary.Kind) {
			return fmt.Errorf("%w: boundary %q kind %q 不受支持", ErrInvalidWorkflowCheckpoint, boundary.ID, boundary.Kind)
		}
		if !validWorkflowBoundaryStatus(boundary.Status) {
			return fmt.Errorf("%w: boundary %q status %q 不受支持", ErrInvalidWorkflowCheckpoint, boundary.ID, boundary.Status)
		}
		if err := validateWorkflowID(boundary.BranchID, "boundary.branch_id", true); err != nil {
			return err
		}
		if _, ok := branches[strings.TrimSpace(boundary.BranchID)]; !ok {
			return fmt.Errorf("%w: boundary %q 的 branch %q 不存在", ErrInvalidWorkflowCheckpoint, boundary.ID, boundary.BranchID)
		}
		if err := validateWorkflowID(boundary.ParentBoundaryID, "boundary.parent_boundary_id", false); err != nil {
			return err
		}
		if err := validateWorkflowID(boundary.StepID, "boundary.step_id", false); err != nil {
			return err
		}
		if err := validateWorkflowID(boundary.RequestDigest, "boundary.request_digest", false); err != nil {
			return err
		}
		if len(boundary.WaitIDs) > MaxWorkflowBoundaryWaitIDs || len(boundary.PendingWaitIDs) > MaxWorkflowBoundaryWaitIDs {
			return fmt.Errorf("%w: boundary %q wait IDs 不能超过 %d 个", ErrInvalidWorkflowCheckpoint, boundary.ID, MaxWorkflowBoundaryWaitIDs)
		}
		waits := make(map[string]struct{}, len(boundary.WaitIDs))
		for _, waitID := range boundary.WaitIDs {
			if err := validateWorkflowID(waitID, "boundary.wait_id", true); err != nil {
				return err
			}
			waitID = strings.TrimSpace(waitID)
			if _, exists := waits[waitID]; exists {
				return fmt.Errorf("%w: boundary %q wait_id %q 重复", ErrInvalidWorkflowCheckpoint, boundary.ID, waitID)
			}
			waits[waitID] = struct{}{}
		}
		pending := make(map[string]struct{}, len(boundary.PendingWaitIDs))
		for _, waitID := range boundary.PendingWaitIDs {
			if err := validateWorkflowID(waitID, "boundary.pending_wait_id", true); err != nil {
				return err
			}
			waitID = strings.TrimSpace(waitID)
			if _, exists := pending[waitID]; exists {
				return fmt.Errorf("%w: boundary %q pending wait_id %q 重复", ErrInvalidWorkflowCheckpoint, boundary.ID, waitID)
			}
			if _, exists := waits[waitID]; !exists {
				return fmt.Errorf("%w: boundary %q pending wait_id %q 不在 wait_ids 中", ErrInvalidWorkflowCheckpoint, boundary.ID, waitID)
			}
			pending[waitID] = struct{}{}
		}
		if boundary.Status != WorkflowBoundaryWaiting && len(boundary.PendingWaitIDs) > 0 {
			return fmt.Errorf("%w: boundary %q 非 waiting 状态不能保留 pending wait IDs", ErrInvalidWorkflowCheckpoint, boundary.ID)
		}
		if len(boundary.Outcome) > 4096 {
			return fmt.Errorf("%w: boundary %q outcome 过长", ErrInvalidWorkflowCheckpoint, boundary.ID)
		}
		if _, exists := boundaries[strings.TrimSpace(boundary.ID)]; exists {
			return fmt.Errorf("%w: boundary %q 重复", ErrInvalidWorkflowCheckpoint, boundary.ID)
		}
		boundaries[strings.TrimSpace(boundary.ID)] = boundary
	}
	for _, branch := range c.Branches {
		if branch.ParentBranchID != "" {
			if _, ok := branches[strings.TrimSpace(branch.ParentBranchID)]; !ok {
				return fmt.Errorf("%w: branch %q 的 parent %q 不存在", ErrInvalidWorkflowCheckpoint, branch.ID, branch.ParentBranchID)
			}
		}
		if branch.HeadBoundaryID != "" {
			boundary, ok := boundaries[strings.TrimSpace(branch.HeadBoundaryID)]
			if !ok {
				return fmt.Errorf("%w: branch %q 的 head boundary %q 不存在", ErrInvalidWorkflowCheckpoint, branch.ID, branch.HeadBoundaryID)
			}
			if boundary.BranchID != branch.ID {
				return fmt.Errorf("%w: branch %q 的 head boundary 不属于本分支", ErrInvalidWorkflowCheckpoint, branch.ID)
			}
		}
	}
	active := make(map[string]struct{}, len(c.ActiveBoundaryIDs))
	for _, id := range c.ActiveBoundaryIDs {
		if err := validateWorkflowID(id, "active_boundary_id", true); err != nil {
			return err
		}
		id = strings.TrimSpace(id)
		if _, exists := active[id]; exists {
			return fmt.Errorf("%w: active boundary %q 重复", ErrInvalidWorkflowCheckpoint, id)
		}
		boundary, ok := boundaries[id]
		if !ok {
			return fmt.Errorf("%w: active boundary %q 不存在", ErrInvalidWorkflowCheckpoint, id)
		}
		if boundary.Status != WorkflowBoundaryWaiting {
			return fmt.Errorf("%w: active boundary %q 当前状态为 %s", ErrInvalidWorkflowCheckpoint, id, boundary.Status)
		}
		active[id] = struct{}{}
	}
	for _, boundary := range c.Boundaries {
		if boundary.Status != WorkflowBoundaryWaiting {
			continue
		}
		if _, ok := active[boundary.ID]; !ok {
			return fmt.Errorf("%w: waiting boundary %q 未出现在 active_boundary_ids", ErrInvalidWorkflowCheckpoint, boundary.ID)
		}
	}
	return nil
}

func workflowEventTimestamp(event AgentEvent, fallback time.Time) time.Time {
	if !event.Timestamp.IsZero() {
		return event.Timestamp.UTC()
	}
	if !fallback.IsZero() {
		return fallback.UTC()
	}
	return time.Unix(0, 0).UTC()
}

func workflowEventString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				if len(value) > MaxWorkflowIDBytes {
					return value[:MaxWorkflowIDBytes]
				}
				return value
			}
		}
	}
	return ""
}

func workflowEventBool(data map[string]any, key string) (bool, bool) {
	value, ok := data[key].(bool)
	return value, ok
}

func workflowBoundaryKindForReason(reason string) WorkflowBoundaryKind {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool", "tools", "waiting_tool":
		return WorkflowBoundaryTool
	case "approval", "approvals", "waiting_approval":
		return WorkflowBoundaryApproval
	case "user", "input", "waiting_user":
		return WorkflowBoundaryUser
	default:
		return ""
	}
}

func workflowBoundaryIDs(data map[string]any, kind WorkflowBoundaryKind, approvalIDs []string) []string {
	ids := make([]string, 0, MaxWorkflowBoundaryWaitIDs)
	appendID := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || len(ids) >= MaxWorkflowBoundaryWaitIDs {
			return
		}
		for _, existing := range ids {
			if existing == value {
				return
			}
		}
		ids = append(ids, value)
	}
	if kind == WorkflowBoundaryTool {
		for _, value := range resumeStringList(data["tool_call_ids"]) {
			appendID(value)
		}
		for _, value := range resumeStringList(data["wait_ids"]) {
			appendID(value)
		}
		appendID(workflowEventString(data, "wait_id"))
	} else if kind == WorkflowBoundaryApproval {
		appendID(workflowEventString(data, "approval_id", "confirmation_call_id", "approval_call_id"))
		if len(approvalIDs) > 0 {
			// Runtime currently admits one approval resume boundary per
			// Invocation. Keep the newest requested approval as the fallback
			// when the waiting marker itself only carries reason=approval.
			appendID(approvalIDs[len(approvalIDs)-1])
		}
	} else {
		appendID(workflowEventString(data, "wait_id", "question_id", "request_id"))
	}
	return ids
}

func workflowRemoveID(values []string, target string) []string {
	target = strings.TrimSpace(target)
	if target == "" {
		return values
	}
	filtered := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value) != target {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func workflowBoundaryID(invocationID string, sequence int64, eventID string, existing map[string]WorkflowBoundary) string {
	core := "boundary:" + strings.TrimSpace(invocationID)
	sequenceSuffix := fmt.Sprintf(":%d", sequence)
	if len(core)+len(sequenceSuffix) > MaxWorkflowIDBytes {
		core = core[:MaxWorkflowIDBytes-len(sequenceSuffix)]
	}
	base := core + sequenceSuffix
	if _, ok := existing[base]; !ok {
		return base
	}
	eventID = strings.TrimSpace(eventID)
	if eventID != "" {
		candidateSuffix := ":" + eventID
		if len(candidateSuffix) >= MaxWorkflowIDBytes {
			candidateSuffix = candidateSuffix[:MaxWorkflowIDBytes-1]
		}
		candidateCore := base
		if len(candidateCore)+len(candidateSuffix) > MaxWorkflowIDBytes {
			candidateCore = candidateCore[:MaxWorkflowIDBytes-len(candidateSuffix)]
		}
		candidate := candidateCore + candidateSuffix
		if _, ok := existing[candidate]; !ok {
			return candidate
		}
	}
	for index := 2; ; index++ {
		candidateSuffix := fmt.Sprintf(":%d", index)
		candidateCore := base
		if len(candidateCore)+len(candidateSuffix) > MaxWorkflowIDBytes {
			candidateCore = candidateCore[:MaxWorkflowIDBytes-len(candidateSuffix)]
		}
		candidate := candidateCore + candidateSuffix
		if _, ok := existing[candidate]; !ok {
			return candidate
		}
	}
}

func workflowOrderedEvents(events []AgentEvent) []AgentEvent {
	ordered := append([]AgentEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.Sequence > 0 && right.Sequence > 0 && left.Sequence != right.Sequence {
			return left.Sequence < right.Sequence
		}
		if left.Sequence > 0 && right.Sequence <= 0 {
			return true
		}
		if left.Sequence <= 0 && right.Sequence > 0 {
			return false
		}
		if !left.Timestamp.Equal(right.Timestamp) {
			return left.Timestamp.Before(right.Timestamp)
		}
		return left.ID < right.ID
	})
	return ordered
}

func workflowBoundaryPendingContains(boundary WorkflowBoundary, ids []string) bool {
	if len(ids) == 0 {
		return true
	}
	pending := make(map[string]struct{}, len(boundary.PendingWaitIDs))
	for _, id := range boundary.PendingWaitIDs {
		pending[id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := pending[id]; !ok {
			return false
		}
	}
	return true
}

func workflowFindWaitingBoundary(boundaries map[string]WorkflowBoundary, kind WorkflowBoundaryKind, ids []string, boundaryID string) (string, bool) {
	if boundaryID = strings.TrimSpace(boundaryID); boundaryID != "" {
		boundary, ok := boundaries[boundaryID]
		return boundaryID, ok && boundary.Status == WorkflowBoundaryWaiting && boundary.Kind == kind
	}
	candidates := make([]WorkflowBoundary, 0)
	for _, boundary := range boundaries {
		if boundary.Status != WorkflowBoundaryWaiting || boundary.Kind != kind || !workflowBoundaryPendingContains(boundary, ids) {
			continue
		}
		candidates = append(candidates, boundary)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Sequence != candidates[j].Sequence {
			return candidates[i].Sequence > candidates[j].Sequence
		}
		return candidates[i].ID > candidates[j].ID
	})
	if len(candidates) == 0 {
		return "", false
	}
	return candidates[0].ID, true
}

func workflowResolveBoundary(boundaries map[string]WorkflowBoundary, kind WorkflowBoundaryKind, ids []string, boundaryID, requestDigest, outcome string, timestamp time.Time) {
	matchedID, ok := workflowFindWaitingBoundary(boundaries, kind, ids, boundaryID)
	if !ok {
		return
	}
	boundary := boundaries[matchedID]
	if len(ids) == 0 {
		ids = append([]string(nil), boundary.PendingWaitIDs...)
	}
	if kind == WorkflowBoundaryTool {
		remaining := make([]string, 0, len(boundary.PendingWaitIDs))
		resolved := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			resolved[id] = struct{}{}
		}
		for _, id := range boundary.PendingWaitIDs {
			if _, done := resolved[id]; !done {
				remaining = append(remaining, id)
			}
		}
		boundary.PendingWaitIDs = remaining
		if len(remaining) > 0 {
			boundary.Outcome = "partial"
		} else {
			boundary.Status = WorkflowBoundaryResolved
			boundary.Outcome = "resumed"
			resolvedAt := timestamp
			boundary.ResolvedAt = &resolvedAt
		}
	} else {
		boundary.PendingWaitIDs = nil
		boundary.Status = WorkflowBoundaryResolved
		resolvedAt := timestamp
		boundary.ResolvedAt = &resolvedAt
		if outcome != "" {
			boundary.Outcome = outcome
		}
	}
	if requestDigest != "" {
		boundary.RequestDigest = requestDigest
	}
	boundaries[matchedID] = boundary
}

func workflowTerminalBoundaryStatus(status InvocationStatus) (WorkflowBoundaryStatus, WorkflowCheckpointStatus, WorkflowBranchStatus, string) {
	switch status {
	case InvocationCompleted:
		return WorkflowBoundaryCompleted, WorkflowCheckpointCompleted, WorkflowBranchCompleted, "completed"
	case InvocationCancelled:
		return WorkflowBoundaryCancelled, WorkflowCheckpointCancelled, WorkflowBranchCancelled, "cancelled"
	case InvocationFailed, InvocationExpired:
		return WorkflowBoundaryFailed, WorkflowCheckpointFailed, WorkflowBranchFailed, string(status)
	default:
		return "", "", "", ""
	}
}

func workflowCompactCheckpoint(checkpoint WorkflowCheckpoint) WorkflowCheckpoint {
	if len(checkpoint.Boundaries) > MaxWorkflowBoundaries {
		ordered := append([]WorkflowBoundary(nil), checkpoint.Boundaries...)
		sort.SliceStable(ordered, func(i, j int) bool {
			waitingI := ordered[i].Status == WorkflowBoundaryWaiting
			waitingJ := ordered[j].Status == WorkflowBoundaryWaiting
			if waitingI != waitingJ {
				return waitingI
			}
			if ordered[i].Sequence != ordered[j].Sequence {
				return ordered[i].Sequence > ordered[j].Sequence
			}
			return ordered[i].ID > ordered[j].ID
		})
		ordered = ordered[:MaxWorkflowBoundaries]
		checkpoint.Boundaries = ordered
	}
	if len(checkpoint.Branches) > MaxWorkflowBranches {
		ordered := append([]WorkflowBranch(nil), checkpoint.Branches...)
		current := strings.TrimSpace(checkpoint.CurrentBranchID)
		sort.SliceStable(ordered, func(i, j int) bool {
			priority := func(branch WorkflowBranch) int {
				if branch.ID == "root" || branch.ID == current || branch.Status == WorkflowBranchWaiting {
					return 0
				}
				return 1
			}
			if priority(ordered[i]) != priority(ordered[j]) {
				return priority(ordered[i]) < priority(ordered[j])
			}
			if ordered[i].UpdatedSequence != ordered[j].UpdatedSequence {
				return ordered[i].UpdatedSequence > ordered[j].UpdatedSequence
			}
			return ordered[i].ID < ordered[j].ID
		})
		ordered = ordered[:MaxWorkflowBranches]
		checkpoint.Branches = ordered
	}
	branchSet := make(map[string]struct{}, len(checkpoint.Branches))
	boundarySet := make(map[string]struct{}, len(checkpoint.Boundaries))
	for _, branch := range checkpoint.Branches {
		branchSet[branch.ID] = struct{}{}
	}
	for index := range checkpoint.Branches {
		parent := strings.TrimSpace(checkpoint.Branches[index].ParentBranchID)
		if parent != "" {
			if _, ok := branchSet[parent]; !ok {
				checkpoint.Branches[index].ParentBranchID = ""
			}
		}
	}
	for _, boundary := range checkpoint.Boundaries {
		if _, ok := branchSet[boundary.BranchID]; ok {
			boundarySet[boundary.ID] = struct{}{}
		} else {
			delete(boundarySet, boundary.ID)
		}
	}
	filteredBoundaries := checkpoint.Boundaries[:0]
	for _, boundary := range checkpoint.Boundaries {
		if _, ok := branchSet[boundary.BranchID]; !ok {
			continue
		}
		filteredBoundaries = append(filteredBoundaries, boundary)
	}
	checkpoint.Boundaries = filteredBoundaries
	sort.SliceStable(checkpoint.Boundaries, func(i, j int) bool {
		if checkpoint.Boundaries[i].Sequence != checkpoint.Boundaries[j].Sequence {
			return checkpoint.Boundaries[i].Sequence < checkpoint.Boundaries[j].Sequence
		}
		return checkpoint.Boundaries[i].ID < checkpoint.Boundaries[j].ID
	})
	for index := range checkpoint.Branches {
		if checkpoint.Branches[index].HeadBoundaryID != "" {
			if _, ok := boundarySet[checkpoint.Branches[index].HeadBoundaryID]; !ok {
				checkpoint.Branches[index].HeadBoundaryID = ""
			}
		}
	}
	filteredActive := checkpoint.ActiveBoundaryIDs[:0]
	for _, id := range checkpoint.ActiveBoundaryIDs {
		if _, ok := boundarySet[id]; ok {
			filteredActive = append(filteredActive, id)
		}
	}
	checkpoint.ActiveBoundaryIDs = filteredActive
	return checkpoint
}

// deriveWorkflowCheckpoint replays only stable Runtime facts. Unknown event
// types are ignored, and malformed optional metadata cannot create an
// executable capability; at worst it leaves the invocation in a conservative
// active/waiting state.
func deriveWorkflowCheckpoint(invocation Invocation, plan *TaskPlan, events []AgentEvent) WorkflowCheckpoint {
	checkpoint := WorkflowCheckpoint{Status: WorkflowCheckpointActive, CurrentBranchID: "root"}
	branches := map[string]WorkflowBranch{
		"root": {ID: "root", Status: WorkflowBranchActive},
	}
	boundaries := make(map[string]WorkflowBoundary)
	currentBranchID := "root"
	approvalIDs := make([]string, 0, 4)
	ordered := workflowOrderedEvents(events)
	for index, event := range ordered {
		sequence := event.Sequence
		if sequence <= 0 {
			sequence = int64(index + 1)
		}
		timestamp := workflowEventTimestamp(event, invocation.CreatedAt)
		data := event.Data
		if data == nil {
			data = map[string]any{}
		}
		if branchID := workflowEventString(data, "branch_id"); branchID != "" {
			parentBranchID := workflowEventString(data, "parent_branch_id")
			if parentBranchID != "" {
				if _, exists := branches[parentBranchID]; !exists {
					branches[parentBranchID] = WorkflowBranch{ID: parentBranchID, Status: WorkflowBranchActive, StartedSequence: sequence, UpdatedSequence: sequence}
				}
			}
			branch, exists := branches[branchID]
			if !exists {
				branch = WorkflowBranch{ID: branchID, ParentBranchID: parentBranchID, StartedSequence: sequence, Status: WorkflowBranchActive}
			} else if branch.ParentBranchID == "" {
				branch.ParentBranchID = parentBranchID
			}
			branch.UpdatedSequence = sequence
			branches[branchID] = branch
			currentBranchID = branchID
		}
		switch event.Type {
		case EventApprovalRequested:
			if id := workflowEventString(data, "approval_id", "confirmation_call_id", "approval_call_id"); id != "" {
				seen := false
				for _, existing := range approvalIDs {
					if existing == id {
						seen = true
						break
					}
				}
				if !seen {
					approvalIDs = append(approvalIDs, id)
					if len(approvalIDs) > MaxWorkflowBoundaryWaitIDs {
						approvalIDs = approvalIDs[len(approvalIDs)-MaxWorkflowBoundaryWaitIDs:]
					}
				}
			}
		case EventInvocationWaiting:
			kind := workflowBoundaryKindForReason(workflowEventString(data, "reason"))
			if kind == "" {
				continue
			}
			branchID := workflowEventString(data, "branch_id")
			if branchID == "" {
				branchID = currentBranchID
			}
			if parentBranchID := workflowEventString(data, "parent_branch_id"); parentBranchID != "" {
				if _, exists := branches[parentBranchID]; !exists {
					branches[parentBranchID] = WorkflowBranch{ID: parentBranchID, Status: WorkflowBranchActive, StartedSequence: sequence, UpdatedSequence: sequence}
				}
			}
			branch := branches[branchID]
			if branch.ID == "" {
				branch = WorkflowBranch{ID: branchID, StartedSequence: sequence}
			}
			branch.Status = WorkflowBranchWaiting
			branch.UpdatedSequence = sequence
			branches[branchID] = branch
			currentBranchID = branchID
			boundaryID := workflowBoundaryID(invocation.ID, sequence, event.ID, boundaries)
			waitIDs := workflowBoundaryIDs(data, kind, approvalIDs)
			stepID := workflowEventString(data, "step_id", "plan_step_id")
			if stepID == "" && plan != nil {
				stepID = strings.TrimSpace(plan.CurrentStepID)
			}
			boundary := WorkflowBoundary{
				ID: boundaryID, Sequence: sequence, Kind: kind, Status: WorkflowBoundaryWaiting,
				BranchID: branchID, ParentBoundaryID: workflowEventString(data, "parent_boundary_id"),
				StepID: stepID, WaitIDs: append([]string(nil), waitIDs...), PendingWaitIDs: append([]string(nil), waitIDs...),
				RequestDigest: workflowEventString(data, "request_digest"), CreatedAt: timestamp,
			}
			boundaries[boundaryID] = boundary
			branch.HeadBoundaryID = boundaryID
			branches[branchID] = branch
		case EventApprovalResolved:
			confirmed, hasConfirmed := workflowEventBool(data, "confirmed")
			outcome := workflowEventString(data, "outcome")
			if outcome == "" && hasConfirmed {
				if confirmed {
					outcome = "approved"
				} else {
					outcome = "rejected"
				}
			}
			ids := workflowBoundaryIDs(data, WorkflowBoundaryApproval, nil)
			workflowResolveBoundary(boundaries, WorkflowBoundaryApproval, ids, workflowEventString(data, "boundary_id"), workflowEventString(data, "request_digest"), outcome, timestamp)
			approvalIDs = workflowRemoveID(approvalIDs, workflowEventString(data, "approval_id", "confirmation_call_id", "approval_call_id"))
		case EventInvocationResumed:
			reason := strings.ToLower(strings.TrimSpace(workflowEventString(data, "reason")))
			kind := workflowBoundaryKindForReason(reason)
			if kind == "" && (workflowEventString(data, "approval_id", "confirmation_call_id") != "" || data["confirmed"] != nil) {
				kind = WorkflowBoundaryApproval
			}
			if kind == "" {
				continue
			}
			ids := workflowBoundaryIDs(data, kind, nil)
			if kind == WorkflowBoundaryTool {
				for _, id := range resumeStringList(data["wait_ids"]) {
					if len(ids) >= MaxWorkflowBoundaryWaitIDs {
						break
					}
					found := false
					for _, existing := range ids {
						if existing == id {
							found = true
							break
						}
					}
					if !found {
						ids = append(ids, id)
					}
				}
			}
			confirmed, hasConfirmed := workflowEventBool(data, "confirmed")
			outcome := workflowEventString(data, "outcome")
			if kind == WorkflowBoundaryApproval && outcome == "" && hasConfirmed {
				if confirmed {
					outcome = "approved"
				} else {
					outcome = "rejected"
				}
			}
			workflowResolveBoundary(boundaries, kind, ids, workflowEventString(data, "boundary_id"), workflowEventString(data, "request_digest"), outcome, timestamp)
			if kind == WorkflowBoundaryApproval {
				approvalIDs = workflowRemoveID(approvalIDs, workflowEventString(data, "approval_id", "confirmation_call_id", "approval_call_id"))
			}
		case EventInvocationCompleted, EventInvocationFailed, EventInvocationCancelled, EventInvocationExpired:
			boundaryStatus, checkpointStatus, branchStatus, outcome := workflowTerminalBoundaryStatus(InvocationStatus(strings.TrimPrefix(event.Type, "invocation.")))
			if boundaryStatus != "" {
				checkpoint.Status = checkpointStatus
				for id, boundary := range boundaries {
					if boundary.Status != WorkflowBoundaryWaiting {
						continue
					}
					boundary.Status = boundaryStatus
					boundary.PendingWaitIDs = nil
					boundary.Outcome = outcome
					resolvedAt := timestamp
					boundary.ResolvedAt = &resolvedAt
					boundaries[id] = boundary
				}
				for id, branch := range branches {
					branch.Status = branchStatus
					branch.UpdatedSequence = sequence
					branches[id] = branch
				}
			}
		}
	}
	if boundaryStatus, checkpointStatus, branchStatus, outcome := workflowTerminalBoundaryStatus(invocation.Status); boundaryStatus != "" {
		checkpoint.Status = checkpointStatus
		terminalAt := invocation.FinishedAt
		if terminalAt == nil {
			value := invocation.UpdatedAt
			terminalAt = &value
		}
		for id, boundary := range boundaries {
			if boundary.Status != WorkflowBoundaryWaiting {
				continue
			}
			boundary.Status = boundaryStatus
			boundary.PendingWaitIDs = nil
			boundary.Outcome = outcome
			resolvedAt := workflowEventTimestamp(AgentEvent{Timestamp: *terminalAt}, invocation.CreatedAt)
			boundary.ResolvedAt = &resolvedAt
			boundaries[id] = boundary
		}
		for id, branch := range branches {
			branch.Status = branchStatus
			branches[id] = branch
		}
	}
	if plan != nil && plan.Status == PlanBlocked && !invocation.Status.Terminal() {
		checkpoint.Status = WorkflowCheckpointBlocked
		for id, branch := range branches {
			branch.Status = WorkflowBranchBlocked
			branches[id] = branch
		}
	}
	if !invocation.Status.Terminal() && (plan == nil || plan.Status != PlanBlocked) {
		for id, branch := range branches {
			if branch.Status != WorkflowBranchWaiting {
				continue
			}
			waitingOnBranch := false
			for _, boundary := range boundaries {
				if boundary.BranchID == id && boundary.Status == WorkflowBoundaryWaiting {
					waitingOnBranch = true
					break
				}
			}
			if !waitingOnBranch {
				branch.Status = WorkflowBranchActive
				branches[id] = branch
			}
		}
	}
	checkpoint.CurrentBranchID = currentBranchID
	for _, branch := range branches {
		checkpoint.Branches = append(checkpoint.Branches, branch)
	}
	for _, boundary := range boundaries {
		checkpoint.Boundaries = append(checkpoint.Boundaries, boundary)
	}
	sort.SliceStable(checkpoint.Branches, func(i, j int) bool {
		if checkpoint.Branches[i].StartedSequence != checkpoint.Branches[j].StartedSequence {
			return checkpoint.Branches[i].StartedSequence < checkpoint.Branches[j].StartedSequence
		}
		return checkpoint.Branches[i].ID < checkpoint.Branches[j].ID
	})
	sort.SliceStable(checkpoint.Boundaries, func(i, j int) bool {
		if checkpoint.Boundaries[i].Sequence != checkpoint.Boundaries[j].Sequence {
			return checkpoint.Boundaries[i].Sequence < checkpoint.Boundaries[j].Sequence
		}
		return checkpoint.Boundaries[i].ID < checkpoint.Boundaries[j].ID
	})
	for _, boundary := range checkpoint.Boundaries {
		if boundary.Status == WorkflowBoundaryWaiting {
			checkpoint.ActiveBoundaryIDs = append(checkpoint.ActiveBoundaryIDs, boundary.ID)
		}
	}
	if checkpoint.Status == WorkflowCheckpointActive {
		if len(checkpoint.ActiveBoundaryIDs) > 0 || invocation.Status == InvocationWaitingTool || invocation.Status == InvocationWaitingApproval || invocation.Status == InvocationWaitingUser {
			checkpoint.Status = WorkflowCheckpointWaiting
		}
	}
	checkpoint = workflowCompactCheckpoint(checkpoint)
	return checkpoint
}
