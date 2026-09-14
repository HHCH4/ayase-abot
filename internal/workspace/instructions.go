package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxInstructionBytes      = 128 * 1024
	maxInstructionTotalBytes = 512 * 1024
	maxInstructionDepth      = 32
)

var (
	ErrInstructionTooLarge = errors.New("项目指令文件过大")
	ErrInvalidInstruction  = errors.New("项目指令文件无效")
	ErrInstructionTooDeep  = errors.New("项目指令作用域过深")
)

// InstructionSnapshot is a bounded, digest-addressed view of one supported
// project instruction file. The content is kept here for the first P1
// injection path; later Context/Artifact storage can replace it with a ref.
type InstructionSnapshot struct {
	Path          string    `json:"path"`
	ScopePath     string    `json:"scope_path"`
	Source        string    `json:"source"`
	ContentDigest string    `json:"content_digest"`
	Content       string    `json:"content"`
	LoadedAt      time.Time `json:"loaded_at"`
	Priority      int       `json:"priority"`
}

type instructionCandidate struct {
	path      string
	scopePath string
	source    string
	priority  int
}

// DiscoverInstructions loads only the two supported instruction names. It
// walks from the workspace root to targetPath, so a nested AGENTS.md applies
// only to that directory and its descendants. Missing candidates are ignored;
// malformed, oversized or unreadable existing files fail the discovery rather
// than silently changing the agent's effective rules.
func (s *Service) DiscoverInstructions(ctx context.Context, workspaceID, targetPath string) ([]InstructionSnapshot, error) {
	item, err := s.Resolve(ctx, strings.TrimSpace(workspaceID))
	if err != nil {
		return nil, err
	}
	targetDir, err := instructionTargetDir(ctx, s, item, targetPath)
	if err != nil {
		return nil, err
	}
	candidates := instructionCandidates(targetDir)
	if len(candidates) > maxInstructionDepth*2+1 {
		return nil, fmt.Errorf("%w: 最多支持 %d 层目录", ErrInstructionTooDeep, maxInstructionDepth)
	}
	result := make([]InstructionSnapshot, 0, len(candidates))
	totalBytes := 0
	for _, candidate := range candidates {
		content, readErr := s.ReadFile(ctx, item.ID, candidate.path)
		if readErr != nil {
			if isMissingInstruction(readErr) {
				continue
			}
			return nil, fmt.Errorf("读取项目指令 %s 失败: %w", candidate.path, readErr)
		}
		if len([]byte(content)) > maxInstructionBytes {
			return nil, fmt.Errorf("%w: %s 超过 %d 字节", ErrInstructionTooLarge, candidate.path, maxInstructionBytes)
		}
		totalBytes += len([]byte(content))
		if totalBytes > maxInstructionTotalBytes {
			return nil, fmt.Errorf("%w: 所有适用指令超过 %d 字节", ErrInstructionTooLarge, maxInstructionTotalBytes)
		}
		if !utf8.ValidString(content) || strings.IndexByte(content, 0) >= 0 {
			return nil, fmt.Errorf("%w: %s 必须是 UTF-8 文本", ErrInvalidInstruction, candidate.path)
		}
		digest := sha256.Sum256([]byte(content))
		result = append(result, InstructionSnapshot{
			Path: candidate.path, ScopePath: candidate.scopePath, Source: candidate.source,
			ContentDigest: hex.EncodeToString(digest[:]), Content: content,
			LoadedAt: time.Now().UTC(), Priority: candidate.priority,
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		return result[i].Path < result[j].Path
	})
	return result, nil
}

func instructionTargetDir(ctx context.Context, service *Service, item Workspace, targetPath string) (string, error) {
	targetPath = cleanRelativePath(targetPath)
	if targetPath == "." {
		return ".", nil
	}
	// Prefer the actual path kind when it exists. For a new file, fall back to
	// its parent so instructions for the destination directory still apply.
	stat, err := service.Stat(ctx, item.ID, targetPath)
	if err == nil && stat.IsDir {
		return targetPath, nil
	}
	if err != nil && !isMissingInstruction(err) {
		// A remote stat may not expose a portable not-found sentinel. A missing
		// target is still safe to treat as a file path; other failures are not.
		if item.Type != TypeSSH && item.Type != TypeRemote {
			return "", err
		}
	}
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(targetPath)))
	if directory == "" {
		return ".", nil
	}
	return cleanRelativePath(directory), nil
}

func instructionCandidates(targetDir string) []instructionCandidate {
	targetDir = cleanRelativePath(targetDir)
	ancestors := make([]string, 0, 8)
	for current := targetDir; ; current = filepath.ToSlash(filepath.Dir(filepath.FromSlash(current))) {
		ancestors = append(ancestors, current)
		if current == "." {
			break
		}
	}
	// Reverse to load broad root rules first and more specific directory rules
	// later. .abot/instructions.md is a root-level Abot entry point; AGENTS.md
	// participates at every directory level.
	for left, right := 0, len(ancestors)-1; left < right; left, right = left+1, right-1 {
		ancestors[left], ancestors[right] = ancestors[right], ancestors[left]
	}
	result := make([]instructionCandidate, 0, len(ancestors)+1)
	priority := 0
	for _, scope := range ancestors {
		if scope == "." {
			result = append(result, instructionCandidate{path: filepath.ToSlash(filepath.Join(".abot", "instructions.md")), scopePath: ".", source: "abot", priority: priority})
			priority++
		}
		path := filepath.ToSlash(filepath.Join(scope, "AGENTS.md"))
		result = append(result, instructionCandidate{path: path, scopePath: scope, source: "agents", priority: priority})
		priority++
	}
	return result
}

func isMissingInstruction(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such file") || strings.Contains(message, "不存在") || strings.Contains(message, "not found")
}
