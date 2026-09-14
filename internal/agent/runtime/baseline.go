package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// PathStatus is the small, metadata-only description of one path observed in
// a worktree baseline. File contents are deliberately not copied into the
// Runtime record.
type PathStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// WorktreeBaseline is the immutable repository fact captured immediately
// before an Invocation starts. It lets reports distinguish user-existing
// changes from operations recorded by this Invocation without pretending that
// a later worktree scan represents the original state.
type WorktreeBaseline struct {
	InvocationID   string       `json:"invocation_id"`
	WorkspaceID    string       `json:"workspace_id,omitempty"`
	TargetPath     string       `json:"target_path,omitempty"`
	RepositoryType string       `json:"repository_type"`
	HeadRevision   string       `json:"head_revision,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	StatusDigest   string       `json:"status_digest"`
	ChangedPaths   []PathStatus `json:"changed_paths,omitempty"`
	StatusKnown    bool         `json:"status_known"`
	Truncated      bool         `json:"truncated,omitempty"`
	Revision       int64        `json:"revision"`
	CapturedAt     time.Time    `json:"captured_at"`
}

var ErrInvalidWorktreeBaseline = errors.New("工作区基线无效")

const MaxWorktreeBaselinePaths = 2000

// WorktreeBaselineResolver captures a workspace fact without executing a
// model, tool, command or mutation. The target path is included so the audit
// record remains tied to the exact scope used by the Invocation.
type WorktreeBaselineResolver func(context.Context, string, string) (WorktreeBaseline, error)

// Normalize fills only deterministic defaults and returns a defensive copy.
// Persistors call this before applying their immutable insert/compare logic.
func (b WorktreeBaseline) Normalize(now time.Time) (WorktreeBaseline, error) {
	b.InvocationID = strings.TrimSpace(b.InvocationID)
	b.WorkspaceID = strings.TrimSpace(b.WorkspaceID)
	b.TargetPath = strings.TrimSpace(b.TargetPath)
	b.RepositoryType = strings.TrimSpace(b.RepositoryType)
	b.HeadRevision = strings.TrimSpace(b.HeadRevision)
	b.Branch = strings.TrimSpace(b.Branch)
	b.StatusDigest = strings.TrimSpace(b.StatusDigest)
	if b.Revision <= 0 {
		b.Revision = 1
	}
	if b.CapturedAt.IsZero() {
		if now.IsZero() {
			now = time.Now().UTC()
		}
		b.CapturedAt = now.UTC()
	} else {
		b.CapturedAt = b.CapturedAt.UTC()
	}
	paths := make([]PathStatus, 0, len(b.ChangedPaths))
	seen := make(map[string]string, len(b.ChangedPaths))
	for _, item := range b.ChangedPaths {
		item.Path = strings.TrimSpace(strings.ReplaceAll(item.Path, "\\", "/"))
		item.Status = strings.TrimRight(item.Status, "\r\n")
		if item.Path == "" {
			return WorktreeBaseline{}, fmt.Errorf("%w: changed path 不能为空", ErrInvalidWorktreeBaseline)
		}
		if strings.ContainsRune(item.Path, '\x00') {
			return WorktreeBaseline{}, fmt.Errorf("%w: changed path 不能包含 NUL", ErrInvalidWorktreeBaseline)
		}
		cleaned := path.Clean(item.Path)
		if cleaned == "." || cleaned == ".." || path.IsAbs(cleaned) || strings.HasPrefix(cleaned, "../") {
			return WorktreeBaseline{}, fmt.Errorf("%w: changed path 越过工作区边界: %q", ErrInvalidWorktreeBaseline, item.Path)
		}
		item.Path = cleaned
		if previous, ok := seen[item.Path]; ok {
			if previous != item.Status {
				return WorktreeBaseline{}, fmt.Errorf("%w: changed path %q 重复且状态不同", ErrInvalidWorktreeBaseline, item.Path)
			}
			continue
		}
		seen[item.Path] = item.Status
		paths = append(paths, item)
	}
	sort.Slice(paths, func(left, right int) bool { return paths[left].Path < paths[right].Path })
	b.ChangedPaths = paths
	if b.StatusDigest == "" {
		b.StatusDigest = worktreeStatusDigest(b.RepositoryType, b.HeadRevision, b.Branch, b.ChangedPaths)
	}
	if err := b.Validate(); err != nil {
		return WorktreeBaseline{}, err
	}
	return cloneWorktreeBaseline(b), nil
}

func (b WorktreeBaseline) Validate() error {
	if b.InvocationID == "" {
		return fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidWorktreeBaseline)
	}
	if b.RepositoryType == "" || len(b.RepositoryType) > 64 {
		return fmt.Errorf("%w: repository_type 无效", ErrInvalidWorktreeBaseline)
	}
	if b.Revision != 1 {
		return fmt.Errorf("%w: revision 必须为 1", ErrInvalidWorktreeBaseline)
	}
	if b.StatusDigest == "" || len(b.StatusDigest) > 256 {
		return fmt.Errorf("%w: status_digest 无效", ErrInvalidWorktreeBaseline)
	}
	if !strings.HasPrefix(b.StatusDigest, "sha256:") {
		return fmt.Errorf("%w: status_digest 必须使用 sha256 前缀", ErrInvalidWorktreeBaseline)
	}
	if len(b.HeadRevision) > 256 || len(b.Branch) > 512 || len(b.TargetPath) > 4096 || len(b.WorkspaceID) > 256 {
		return fmt.Errorf("%w: 基线文本字段过长", ErrInvalidWorktreeBaseline)
	}
	if len(b.ChangedPaths) > MaxWorktreeBaselinePaths {
		return fmt.Errorf("%w: changed_paths 不能超过 %d", ErrInvalidWorktreeBaseline, MaxWorktreeBaselinePaths)
	}
	for _, item := range b.ChangedPaths {
		if item.Path == "" || len(item.Path) > 4096 || strings.ContainsRune(item.Path, '\x00') || path.IsAbs(item.Path) || item.Path == "." || item.Path == ".." || strings.HasPrefix(item.Path, "../") {
			return fmt.Errorf("%w: changed path 越过工作区边界: %q", ErrInvalidWorktreeBaseline, item.Path)
		}
		if len(item.Status) > 32 {
			return fmt.Errorf("%w: changed path 状态过长", ErrInvalidWorktreeBaseline)
		}
	}
	if b.CapturedAt.IsZero() {
		return fmt.Errorf("%w: captured_at 不能为空", ErrInvalidWorktreeBaseline)
	}
	return nil
}

func worktreeStatusDigest(repositoryType, head, branch string, paths []PathStatus) string {
	hasher := sha256.New()
	_, _ = fmt.Fprintf(hasher, "%s\x00%s\x00%s\x00", repositoryType, head, branch)
	for _, item := range paths {
		_, _ = fmt.Fprintf(hasher, "%s\x00%s\x00", item.Path, item.Status)
	}
	return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
}

func cloneWorktreeBaseline(item WorktreeBaseline) WorktreeBaseline {
	item.ChangedPaths = append([]PathStatus(nil), item.ChangedPaths...)
	return item
}

func sameWorktreeBaseline(left, right WorktreeBaseline) bool {
	left = cloneWorktreeBaseline(left)
	right = cloneWorktreeBaseline(right)
	return left.InvocationID == right.InvocationID && left.WorkspaceID == right.WorkspaceID && left.TargetPath == right.TargetPath && left.RepositoryType == right.RepositoryType && left.HeadRevision == right.HeadRevision && left.Branch == right.Branch && left.StatusDigest == right.StatusDigest && left.StatusKnown == right.StatusKnown && left.Truncated == right.Truncated && left.Revision == right.Revision && left.CapturedAt.Equal(right.CapturedAt) && equalPathStatuses(left.ChangedPaths, right.ChangedPaths)
}

// EqualWorktreeBaseline compares normalized immutable baseline values. It is
// exported for persistence adapters that must implement idempotent saves
// without duplicating field-by-field comparison rules.
func EqualWorktreeBaseline(left, right WorktreeBaseline) bool {
	return sameWorktreeBaseline(left, right)
}

func equalPathStatuses(left, right []PathStatus) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
