package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	WorkingSetFresh       = "fresh"
	WorkingSetStale       = "stale"
	WorkingSetUnknown     = "unknown"
	WorkingSetPriorityP0  = 0
	WorkingSetPriorityP1  = 1
	WorkingSetPriorityP2  = 2
	WorkingSetPriorityP3  = 3
	WorkingSetMaxItems    = 512
	WorkingSetMaxSummary  = 4096
	WorkingSetMaxPath     = 4096
	WorkingSetMaxDeps     = 32
	WorkingSetMaxFileLine = 1_000_000
)

// FileSlice is a re-readable, digest-bound range of a workspace file. Content
// is intentionally not persisted in WorkingSet; callers may attach it only to
// the ephemeral model projection after revalidating FileDigest.
type FileSlice struct {
	Path        string    `json:"path"`
	StartLine   int       `json:"start_line"`
	EndLine     int       `json:"end_line"`
	FileDigest  string    `json:"file_digest"`
	SliceDigest string    `json:"slice_digest"`
	Language    string    `json:"language,omitempty"`
	Symbol      string    `json:"symbol,omitempty"`
	ContentRef  string    `json:"content_ref,omitempty"`
	CapturedAt  time.Time `json:"captured_at"`
}

// WorkingSetItem is metadata for one candidate context item. ContentRef is a
// stable pointer into its owning store; Summary is bounded and never treated
// as an instruction. Freshness is revalidated by the source owner.
type WorkingSetItem struct {
	ID             string     `json:"id"`
	InvocationID   string     `json:"invocation_id"`
	Kind           string     `json:"kind"`
	SourceRef      string     `json:"source_ref"`
	Scope          string     `json:"scope,omitempty"`
	Trust          string     `json:"trust,omitempty"`
	Priority       int        `json:"priority"`
	RelevanceScore float64    `json:"relevance_score"`
	TokenEstimate  int        `json:"token_estimate"`
	ContentDigest  string     `json:"content_digest"`
	DependencyKeys []string   `json:"dependency_keys,omitempty"`
	Freshness      string     `json:"freshness"`
	ContentRef     string     `json:"content_ref,omitempty"`
	Summary        string     `json:"summary,omitempty"`
	Supersedes     string     `json:"supersedes,omitempty"`
	FileSlice      *FileSlice `json:"file_slice,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ValidatedAt    time.Time  `json:"validated_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
}

// WorkingSet is the current, versioned candidate set for one Invocation. The
// source repository remains authoritative; this entity only records what may
// be selected for a model request.
type WorkingSet struct {
	InvocationID string           `json:"invocation_id"`
	Revision     int64            `json:"revision"`
	Items        []WorkingSetItem `json:"items"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

type WorkingSetExcluded struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

type WorkingSetSelection struct {
	Included []WorkingSetItem     `json:"included"`
	Excluded []WorkingSetExcluded `json:"excluded"`
	Used     int                  `json:"used"`
}

var ErrInvalidWorkingSet = errors.New("Working Set 无效")

func (s WorkingSet) Validate() error {
	if strings.TrimSpace(s.InvocationID) == "" {
		return fmt.Errorf("%w: invocation_id 不能为空", ErrInvalidWorkingSet)
	}
	if s.Revision <= 0 {
		return fmt.Errorf("%w: revision 必须为正数", ErrInvalidWorkingSet)
	}
	if len(s.Items) > WorkingSetMaxItems {
		return fmt.Errorf("%w: item 数量超过 %d", ErrInvalidWorkingSet, WorkingSetMaxItems)
	}
	seen := make(map[string]struct{}, len(s.Items))
	for index := range s.Items {
		item := s.Items[index]
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Kind) == "" || strings.TrimSpace(item.SourceRef) == "" {
			return fmt.Errorf("%w: item %d 缺少 id/kind/source_ref", ErrInvalidWorkingSet, index+1)
		}
		if _, ok := seen[item.ID]; ok {
			return fmt.Errorf("%w: item id %q 重复", ErrInvalidWorkingSet, item.ID)
		}
		seen[item.ID] = struct{}{}
		if len(item.SourceRef) > WorkingSetMaxPath || len(item.Summary) > WorkingSetMaxSummary {
			return fmt.Errorf("%w: item %q 引用或摘要过长", ErrInvalidWorkingSet, item.ID)
		}
		if item.Priority < WorkingSetPriorityP0 || item.Priority > WorkingSetPriorityP3 {
			return fmt.Errorf("%w: item %q priority 不受支持", ErrInvalidWorkingSet, item.ID)
		}
		if item.TokenEstimate < 0 || item.RelevanceScore < 0 || math.IsNaN(item.RelevanceScore) || math.IsInf(item.RelevanceScore, 0) {
			return fmt.Errorf("%w: item %q token/score 不能为负数", ErrInvalidWorkingSet, item.ID)
		}
		if item.Freshness != WorkingSetFresh && item.Freshness != WorkingSetStale && item.Freshness != WorkingSetUnknown {
			return fmt.Errorf("%w: item %q freshness 不受支持", ErrInvalidWorkingSet, item.ID)
		}
		if len(item.DependencyKeys) > WorkingSetMaxDeps {
			return fmt.Errorf("%w: item %q 依赖过多", ErrInvalidWorkingSet, item.ID)
		}
		if item.FileSlice != nil {
			if err := item.FileSlice.Validate(); err != nil {
				return fmt.Errorf("%w: item %q file slice: %v", ErrInvalidWorkingSet, item.ID, err)
			}
		}
	}
	return nil
}

func (s FileSlice) Validate() error {
	if strings.TrimSpace(s.Path) == "" || len(s.Path) > WorkingSetMaxPath {
		return errors.New("path 不能为空或过长")
	}
	if s.StartLine <= 0 || s.EndLine < s.StartLine || s.EndLine > WorkingSetMaxFileLine {
		return errors.New("行范围无效")
	}
	if len(strings.TrimSpace(s.FileDigest)) == 0 || len(strings.TrimSpace(s.SliceDigest)) == 0 {
		return errors.New("缺少 file_digest 或 slice_digest")
	}
	return nil
}

// NewFileSlice computes both the full-file and selected-range SHA-256
// digests. Lines are one-based and the returned range is clamped to the
// available content; empty ranges are rejected to avoid ambiguous references.
func NewFileSlice(path, content string, startLine, endLine int, language, symbol, contentRef string, capturedAt time.Time) (FileSlice, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileSlice{}, errors.New("文件路径不能为空")
	}
	lines := strings.Split(content, "\n")
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 {
		endLine = len(lines)
	}
	if startLine > len(lines) || endLine < startLine {
		return FileSlice{}, errors.New("文件切片行范围无效")
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	selected := strings.Join(lines[startLine-1:endLine], "\n")
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	} else {
		capturedAt = capturedAt.UTC()
	}
	slice := FileSlice{Path: path, StartLine: startLine, EndLine: endLine, FileDigest: digestWorkingSetText(content), SliceDigest: digestWorkingSetText(selected), Language: strings.TrimSpace(language), Symbol: strings.TrimSpace(symbol), ContentRef: strings.TrimSpace(contentRef), CapturedAt: capturedAt}
	return slice, nil
}

// SelectWorkingSet performs deterministic water-filling. Required IDs are
// admitted before scored candidates; stale/expired items are excluded and
// duplicate source+range+digest candidates collapse to the first stable ID.
func SelectWorkingSet(items []WorkingSetItem, budget int, requiredIDs []string, now time.Time) WorkingSetSelection {
	if budget < 0 {
		budget = 0
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	required := make(map[string]struct{}, len(requiredIDs))
	for _, id := range requiredIDs {
		required[strings.TrimSpace(id)] = struct{}{}
	}
	ordered := append([]WorkingSetItem(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		_, iRequired := required[ordered[i].ID]
		_, jRequired := required[ordered[j].ID]
		if iRequired != jRequired {
			return iRequired
		}
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		if ordered[i].RelevanceScore != ordered[j].RelevanceScore {
			return ordered[i].RelevanceScore > ordered[j].RelevanceScore
		}
		return ordered[i].ID < ordered[j].ID
	})
	result := WorkingSetSelection{Included: make([]WorkingSetItem, 0, len(ordered)), Excluded: make([]WorkingSetExcluded, 0)}
	seen := make(map[string]struct{})
	for _, item := range ordered {
		reason := ""
		if item.Freshness == WorkingSetStale {
			reason = "stale"
		} else if item.Freshness == WorkingSetUnknown {
			reason = "unknown"
		} else if item.ExpiresAt != nil && !item.ExpiresAt.After(now) {
			reason = "expired"
		}
		key := item.SourceRef + "|" + fmt.Sprint(item.FileSlice) + "|" + item.ContentDigest
		if reason == "" {
			if _, duplicate := seen[key]; duplicate {
				reason = "duplicate"
			} else if item.TokenEstimate > budget-result.Used {
				reason = "over_budget"
			}
		}
		if reason != "" {
			result.Excluded = append(result.Excluded, WorkingSetExcluded{ID: item.ID, Kind: item.Kind, Reason: reason})
			continue
		}
		seen[key] = struct{}{}
		result.Included = append(result.Included, item)
		result.Used += item.TokenEstimate
	}
	return result
}

func digestWorkingSetText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
