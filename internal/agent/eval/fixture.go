package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	MaxFixtureFiles       = 256
	MaxFixtureFileBytes   = 1 << 20
	MaxFixtureTotalBytes  = 8 << 20
	fixtureDirectoryPerms = 0o700
	fixtureFilePerms      = 0o600
)

var (
	ErrInvalidFixture = errors.New("隔离工作区 fixture 无效")
	ErrFixtureClosed  = errors.New("隔离工作区 fixture 已关闭")
	ErrFixtureCleanup = errors.New("隔离工作区 fixture 清理失败")
)

// FixtureFile is an input file for an isolated workspace fixture. Path must
// be a canonical, relative slash-separated path. Content is copied before
// writing and is never retained by WorkspaceFixture after construction.
type FixtureFile struct {
	Path    string      `json:"path"`
	Content []byte      `json:"-"`
	Mode    fs.FileMode `json:"mode,omitempty"`
}

// FixtureFileMetadata is the only file information exposed to an evaluator.
// It intentionally contains a digest and size, never file content.
type FixtureFileMetadata struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Digest string `json:"digest"`
	Mode   uint32 `json:"mode"`
}

type FixtureManifest struct {
	Files      []FixtureFileMetadata `json:"files"`
	TotalBytes int64                 `json:"total_bytes"`
	Digest     string                `json:"digest"`
}

// WorkspaceFixture owns one temporary workspace. It is deliberately scoped
// to one workflow case and must be closed by the creator; Close is idempotent
// and removes only the fixture directory it created.
type WorkspaceFixture struct {
	mu       sync.Mutex
	root     string
	manifest FixtureManifest
}

// NewWorkspaceFixture creates a bounded, private temporary workspace from a
// deterministic set of files. All definitions are validated before any
// directory is created, so an invalid fixture cannot leave a partial tree.
func NewWorkspaceFixture(ctx context.Context, files []FixtureFile) (*WorkspaceFixture, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFixture, err)
	}
	normalized, manifest, err := validateFixtureFiles(files)
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "abot-eval-fixture-")
	if err != nil {
		return nil, fmt.Errorf("%w: 创建临时目录失败: %v", ErrInvalidFixture, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(root)
		}
	}()
	for _, file := range normalized {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidFixture, err)
		}
		path := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), fixtureDirectoryPerms); err != nil {
			return nil, fmt.Errorf("%w: 创建 %s 父目录失败: %v", ErrInvalidFixture, file.Path, err)
		}
		mode := file.Mode.Perm()
		if mode == 0 {
			mode = fixtureFilePerms
		}
		if err := os.WriteFile(path, file.Content, mode); err != nil {
			return nil, fmt.Errorf("%w: 写入 %s 失败: %v", ErrInvalidFixture, file.Path, err)
		}
	}
	cleanup = false
	return &WorkspaceFixture{root: root, manifest: manifest}, nil
}

// RootPath returns the private workspace path, or an empty string after
// Close. Callers should prefer Resolve for paths supplied by a fixture case.
func (f *WorkspaceFixture) RootPath() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.root
}

// Resolve returns an in-root path for a canonical relative fixture path. It
// prevents an executor from accidentally escaping the isolated workspace.
func (f *WorkspaceFixture) Resolve(relative string) (string, error) {
	path, err := normalizeFixturePath(relative)
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", ErrFixtureClosed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	root := f.root
	if root == "" {
		return "", ErrFixtureClosed
	}
	candidate := filepath.Join(root, filepath.FromSlash(path))
	if err := validateResolvedFixturePath(root, candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

// Manifest returns a defensive, metadata-only copy. File contents and the
// temporary root are never returned to the evaluator through this method.
func (f *WorkspaceFixture) Manifest() FixtureManifest {
	if f == nil {
		return FixtureManifest{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	result := f.manifest
	result.Files = append([]FixtureFileMetadata(nil), f.manifest.Files...)
	return result
}

// Close removes the temporary workspace. It is safe to call more than once.
func (f *WorkspaceFixture) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	root := f.root
	f.root = ""
	f.mu.Unlock()
	if root == "" {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("%w: %v", ErrFixtureCleanup, err)
	}
	return nil
}

// WorkspaceWorkflowCase binds a model-free EvalCase to a fresh isolated
// workspace. A new fixture is created and destroyed for every case execution;
// Execute must return only the durable metadata projection consumed by Eval.
type WorkspaceWorkflowCase struct {
	Case    EvalCase
	Files   []FixtureFile
	Execute func(context.Context, *WorkspaceFixture) (Input, error)
}

// EvaluateWorkspaceWorkflowSuite runs fixture-backed cases through the same
// bounded, deterministic workflow suite. Case, executor and fixture
// definitions are all validated before the first fixture is created.
func EvaluateWorkspaceWorkflowSuite(ctx context.Context, cases []WorkspaceWorkflowCase, baseline []Result, policy GatePolicy) (SuiteResult, error) {
	if len(cases) == 0 || len(cases) > MaxSuiteCases {
		return SuiteResult{}, fmt.Errorf("%w: case 数量必须在 1 到 %d 之间", ErrInvalidWorkflowCase, MaxSuiteCases)
	}
	workflows := make([]WorkflowCase, 0, len(cases))
	for index, item := range cases {
		if err := item.Case.Validate(); err != nil {
			return SuiteResult{}, fmt.Errorf("%w: case[%d]: %v", ErrInvalidWorkflowCase, index, err)
		}
		if item.Execute == nil {
			return SuiteResult{}, fmt.Errorf("%w: case[%d] 缺少 execute", ErrInvalidWorkflowCase, index)
		}
		if _, _, err := validateFixtureFiles(item.Files); err != nil {
			return SuiteResult{}, fmt.Errorf("%w: case[%d] fixture: %v", ErrInvalidWorkflowCase, index, err)
		}
		current := item
		workflows = append(workflows, WorkflowCase{
			Case: current.Case,
			Execute: func(executionContext context.Context) (Input, error) {
				fixture, err := NewWorkspaceFixture(executionContext, current.Files)
				if err != nil {
					return Input{}, err
				}
				input, executeErr := current.Execute(executionContext, fixture)
				cleanupErr := fixture.Close()
				if executeErr != nil {
					if cleanupErr != nil {
						return Input{}, fmt.Errorf("%w: %v; %w", ErrWorkflowExecution, executeErr, cleanupErr)
					}
					return Input{}, executeErr
				}
				if cleanupErr != nil {
					return Input{}, cleanupErr
				}
				return input, nil
			},
		})
	}
	return EvaluateWorkflowSuite(ctx, workflows, baseline, policy)
}

func validateFixtureFiles(files []FixtureFile) ([]FixtureFile, FixtureManifest, error) {
	if len(files) > MaxFixtureFiles {
		return nil, FixtureManifest{}, fmt.Errorf("%w: 文件数量不能超过 %d", ErrInvalidFixture, MaxFixtureFiles)
	}
	normalized := make([]FixtureFile, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	var total int64
	for index, file := range files {
		path, err := normalizeFixturePath(file.Path)
		if err != nil {
			return nil, FixtureManifest{}, fmt.Errorf("%w: files[%d]: %v", ErrInvalidFixture, index, err)
		}
		if _, exists := seen[path]; exists {
			return nil, FixtureManifest{}, fmt.Errorf("%w: 文件重复 %s", ErrInvalidFixture, path)
		}
		seen[path] = struct{}{}
		if int64(len(file.Content)) > MaxFixtureFileBytes {
			return nil, FixtureManifest{}, fmt.Errorf("%w: %s 超过 %d bytes", ErrInvalidFixture, path, MaxFixtureFileBytes)
		}
		mode := file.Mode.Perm()
		if file.Mode != 0 && file.Mode&^0o777 != 0 {
			return nil, FixtureManifest{}, fmt.Errorf("%w: %s mode 含不安全权限位", ErrInvalidFixture, path)
		}
		if mode == 0 {
			mode = fixtureFilePerms
		}
		if total > MaxFixtureTotalBytes-int64(len(file.Content)) {
			return nil, FixtureManifest{}, fmt.Errorf("%w: 总文件大小不能超过 %d bytes", ErrInvalidFixture, MaxFixtureTotalBytes)
		}
		total += int64(len(file.Content))
		copyContent := append([]byte(nil), file.Content...)
		normalized = append(normalized, FixtureFile{Path: path, Content: copyContent, Mode: mode})
	}
	sort.SliceStable(normalized, func(i, j int) bool { return normalized[i].Path < normalized[j].Path })
	metadata := make([]FixtureFileMetadata, 0, len(normalized))
	hasher := sha256.New()
	for _, file := range normalized {
		sum := sha256.Sum256(file.Content)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		item := FixtureFileMetadata{Path: file.Path, Bytes: int64(len(file.Content)), Digest: digest, Mode: uint32(file.Mode.Perm())}
		metadata = append(metadata, item)
		_, _ = fmt.Fprintf(hasher, "%s\x00%d\x00%s\x00%o\n", item.Path, item.Bytes, item.Digest, item.Mode)
	}
	rootDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	return normalized, FixtureManifest{Files: metadata, TotalBytes: total, Digest: rootDigest}, nil
}

func normalizeFixturePath(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" || strings.ContainsRune(raw, '\x00') || strings.Contains(raw, "\\") {
		return "", errors.New("path 必须是非空的相对 slash 路径")
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	canonical := filepath.ToSlash(clean)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || raw != canonical {
		return "", fmt.Errorf("path %q 越界或不是 canonical 相对路径", value)
	}
	return canonical, nil
}

func validateResolvedFixturePath(root, candidate string) error {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if !fixturePathWithin(root, candidate) {
		return fmt.Errorf("%w: resolved path 越出 fixture root", ErrInvalidFixture)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("%w: 校验 fixture root 失败: %v", ErrInvalidFixture, err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(candidate); resolveErr == nil {
		if !fixturePathWithin(realRoot, resolved) {
			return fmt.Errorf("%w: resolved path 含越界符号链接", ErrInvalidFixture)
		}
		return nil
	}
	// The target may not exist yet. Evaluate its nearest existing parent so a
	// symlink such as fixture/link/new cannot redirect a later write outside.
	parent := filepath.Dir(candidate)
	for {
		resolvedParent, parentErr := filepath.EvalSymlinks(parent)
		if parentErr == nil {
			if !fixturePathWithin(realRoot, resolvedParent) {
				return fmt.Errorf("%w: fixture parent 含越界符号链接", ErrInvalidFixture)
			}
			return nil
		}
		next := filepath.Dir(parent)
		if next == parent {
			return fmt.Errorf("%w: 无法校验 fixture path", ErrInvalidFixture)
		}
		parent = next
	}
}

func fixturePathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}
