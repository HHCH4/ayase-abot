package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const (
	maxFileBytes          = 1 << 20
	maxOutputBytes        = 1 << 20
	maxSearchHits         = 100
	maxSearchQueryBytes   = 4096
	maxSearchContextLines = 5
	maxReadRangeLines     = 2000
	maxReadRangeBytes     = 512 << 10
	maxReadManyFiles      = 32
	maxReadManyBytes      = 4 << 20
	maxGlobMatches        = 500
	maxBaselineOutput     = 1 << 20
	maxBaselinePaths      = 2000
)

func testWorkspace(ctx context.Context, item Workspace) (TestResult, error) {
	switch item.Type {
	case TypeLocal:
		root, err := resolveLocalPath(item.RootPath, ".", true)
		if err != nil {
			return TestResult{}, err
		}
		info, err := os.Stat(root)
		if err != nil {
			return TestResult{}, fmt.Errorf("读取项目目录失败: %w", err)
		}
		if !info.IsDir() {
			return TestResult{}, errors.New("用户指定的项目路径不是目录")
		}
		return TestResult{OK: true, Message: "本机项目目录可用"}, nil
	case TypeSSH:
		connection, fingerprint, err := dialSSH(ctx, item, true)
		if err != nil {
			return TestResult{Fingerprint: fingerprint}, err
		}
		defer connection.Close()
		result, err := runSSH(ctx, connection.client, "test -d "+shellQuote(item.RootPath)+" && pwd", "", 15*time.Second)
		if err != nil {
			return TestResult{Fingerprint: fingerprint}, fmt.Errorf("SSH 项目目录不可用: %w", err)
		}
		_ = result
		message := "SSH 连接和项目目录正常"
		if item.HostKeyFingerprint == "" {
			message = "SSH 连接正常，请确认并保存主机指纹"
		}
		return TestResult{OK: true, Message: message, Fingerprint: fingerprint}, nil
	default:
		return TestResult{}, ErrUnsupported
	}
}

func (s *Service) ListFiles(ctx context.Context, workspaceID, relative string) ([]FileEntry, error) {
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if item.Type == TypeLocal {
		return listLocalFiles(item.RootPath, relative)
	}
	return listSSHFiles(ctx, item, relative)
}

func (s *Service) ReadFile(ctx context.Context, workspaceID, relative string) (string, error) {
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if item.Type == TypeLocal {
		target, err := resolveLocalPath(item.RootPath, relative, true)
		if err != nil {
			return "", err
		}
		return readLimitedFile(target)
	}
	return readSSHFile(ctx, item, relative)
}

// Stat 返回项目根目录内单个路径的元信息，供 WebUI 和 Agent 做轻量判断。
func (s *Service) Stat(ctx context.Context, workspaceID, relative string) (FileStat, error) {
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return FileStat{}, err
	}
	if item.Type == TypeLocal {
		target, pathErr := resolveLocalPath(item.RootPath, relative, true)
		if pathErr != nil {
			return FileStat{}, pathErr
		}
		info, statErr := os.Stat(target)
		if statErr != nil {
			return FileStat{}, fmt.Errorf("读取路径状态失败: %w", statErr)
		}
		return FileStat{Path: filepath.ToSlash(cleanRelativePath(relative)), IsDir: info.IsDir(), Size: info.Size(), Mode: info.Mode().String(), ModTime: info.ModTime()}, nil
	}
	return statSSHFile(ctx, item, relative)
}

func (s *Service) Search(ctx context.Context, workspaceID, relative, query string) ([]SearchMatch, error) {
	return s.SearchWithOptions(ctx, workspaceID, relative, query, SearchOptions{Mode: SearchModeLiteral})
}

// SearchWithMode performs a bounded text search. Regex mode is deliberately
// opt-in; regexp.Compile uses RE2 semantics, so matching is linear-time and
// cannot trigger backtracking denial of service. Empty mode remains literal for
// compatibility with callers that predate the mode parameter.
func (s *Service) SearchWithMode(ctx context.Context, workspaceID, relative, query string, mode SearchMode) ([]SearchMatch, error) {
	return s.SearchWithOptions(ctx, workspaceID, relative, query, SearchOptions{Mode: mode})
}

// SearchWithOptions performs a bounded text search with explicit matching,
// path and presentation controls. The query is validated once before either
// local or SSH execution so transport choice cannot bypass limits.
func (s *Service) SearchWithOptions(ctx context.Context, workspaceID, relative, query string, options SearchOptions) ([]SearchMatch, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	query, matcher, normalized, globMatcher, err := compileSearchOptions(query, options)
	if err != nil {
		return nil, err
	}
	if item.Type == TypeLocal {
		return searchLocalWithOptions(ctx, item.RootPath, relative, query, normalized, matcher, globMatcher)
	}
	return searchSSHWithOptions(ctx, item, relative, query, normalized, matcher, globMatcher)
}

func compileSearchQuery(query string, mode SearchMode) (string, *regexp.Regexp, SearchMode, error) {
	query, matcher, normalized, _, err := compileSearchOptions(query, SearchOptions{Mode: mode})
	return query, matcher, normalized.Mode, err
}

func compileSearchOptions(query string, options SearchOptions) (string, *regexp.Regexp, SearchOptions, *regexp.Regexp, error) {
	var err error
	query = strings.TrimSpace(query)
	if query == "" {
		return "", nil, SearchOptions{}, nil, fmt.Errorf("%w: 搜索内容不能为空", ErrInvalidRequest)
	}
	if len([]byte(query)) > maxSearchQueryBytes {
		return "", nil, SearchOptions{}, nil, fmt.Errorf("%w: 搜索内容不能超过 %d 字节", ErrInvalidRequest, maxSearchQueryBytes)
	}
	if !utf8.ValidString(query) || strings.ContainsAny(query, "\x00\r\n") {
		return "", nil, SearchOptions{}, nil, fmt.Errorf("%w: 搜索内容必须是单行有效 UTF-8 文本", ErrInvalidRequest)
	}
	if options.Mode == "" {
		options.Mode = SearchModeLiteral
	}
	if !options.Mode.valid() {
		return "", nil, SearchOptions{}, nil, fmt.Errorf("%w: 不支持的搜索模式 %q", ErrInvalidRequest, options.Mode)
	}
	if options.MaxHits == 0 {
		options.MaxHits = maxSearchHits
	}
	if options.MaxHits < 1 || options.MaxHits > maxSearchHits {
		return "", nil, SearchOptions{}, nil, fmt.Errorf("%w: max_hits 必须在 1-%d 之间", ErrInvalidRequest, maxSearchHits)
	}
	if options.ContextLines < 0 || options.ContextLines > maxSearchContextLines {
		return "", nil, SearchOptions{}, nil, fmt.Errorf("%w: context_lines 必须在 0-%d 之间", ErrInvalidRequest, maxSearchContextLines)
	}
	var globMatcher *regexp.Regexp
	if strings.TrimSpace(options.Glob) != "" {
		var normalizedGlob string
		globMatcher, normalizedGlob, err = compileWorkspaceGlob(options.Glob)
		if err != nil {
			return "", nil, SearchOptions{}, nil, err
		}
		options.Glob = normalizedGlob
	}
	if options.Mode == SearchModeRegex {
		pattern := query
		if options.CaseInsensitive {
			// Go's regexp engine is RE2 based. Scoping the flag avoids mutating
			// caller-visible query text while keeping anchors/group semantics.
			pattern = "(?i:(?:" + pattern + "))"
		}
		matcher, err := regexp.Compile(pattern)
		if err != nil {
			return "", nil, SearchOptions{}, nil, fmt.Errorf("%w: 正则表达式无效: %v", ErrInvalidRequest, err)
		}
		return query, matcher, options, globMatcher, nil
	}
	return query, nil, options, globMatcher, nil
}

// CaptureBaseline records only repository metadata at the current execution
// boundary. Local Git uses argv (never a user-controlled shell command); the
// remote variant uses the same fixed, boundary-checked script as other
// read-only transport operations.
func (s *Service) CaptureBaseline(ctx context.Context, workspaceID string) (WorktreeBaseline, error) {
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return WorktreeBaseline{}, err
	}
	if item.Type == TypeLocal {
		return captureLocalBaseline(ctx, item.RootPath)
	}
	return captureRemoteBaseline(ctx, item)
}

// ReadRange returns a bounded line slice while retaining the digest and line
// count of the complete file. It intentionally reuses the same path boundary
// and one-megabyte file limit as ReadFile.
func (s *Service) ReadRange(ctx context.Context, workspaceID, relative string, startLine, endLine int, includeLineNumbers bool) (ReadRangeResult, error) {
	startLine, endLine, err := normalizeReadRange(startLine, endLine)
	if err != nil {
		return ReadRangeResult{}, err
	}
	content, err := s.ReadFile(ctx, workspaceID, relative)
	if err != nil {
		return ReadRangeResult{}, err
	}
	return makeReadRangeResult(relative, content, startLine, endLine, includeLineNumbers), nil
}

// ReadMany reads a bounded batch in request order. Per-path failures are
// returned alongside successful entries; only invalid batch shape or context
// cancellation fails the whole request.
func (s *Service) ReadMany(ctx context.Context, workspaceID string, paths []string) (ReadManyResult, error) {
	if len(paths) == 0 || len(paths) > maxReadManyFiles {
		return ReadManyResult{}, fmt.Errorf("%w: 批量读取文件数必须在 1 到 %d 之间", ErrInvalidRequest, maxReadManyFiles)
	}
	result := ReadManyResult{Files: make([]ReadManyFile, 0, len(paths))}
	seen := make(map[string]struct{}, len(paths))
	for _, requested := range paths {
		select {
		case <-ctx.Done():
			return ReadManyResult{}, ctx.Err()
		default:
		}
		relative := cleanRelativePath(requested)
		if _, exists := seen[relative]; exists {
			return ReadManyResult{}, fmt.Errorf("%w: 批量读取路径重复: %s", ErrInvalidRequest, relative)
		}
		seen[relative] = struct{}{}
		item := ReadManyFile{Path: filepath.ToSlash(relative)}
		if result.Truncated {
			item.Error = fmt.Sprintf("达到批量读取总字节上限 %d", maxReadManyBytes)
			result.Files = append(result.Files, item)
			continue
		}
		content, err := s.ReadFile(ctx, workspaceID, relative)
		if err != nil {
			item.Error = sanitizeWorkspaceError(err)
			result.Files = append(result.Files, item)
			continue
		}
		item.ContentDigest = digestContent(content)
		item.TotalLines = countContentLines(content)
		item.Bytes = int64(len(content))
		remaining := maxReadManyBytes - int(result.TotalBytes)
		if remaining < len(content) {
			item.Content, item.Truncated = truncateUTF8(content, remaining)
			result.Truncated = true
		} else {
			item.Content = content
		}
		result.TotalBytes += int64(len(item.Content))
		result.Files = append(result.Files, item)
	}
	return result, nil
}

// Glob resolves a workspace-relative glob without exposing an arbitrary shell
// command. Supported wildcards are *, ?, character classes and recursive **.
func (s *Service) Glob(ctx context.Context, workspaceID, pattern string) (GlobResult, error) {
	matcher, normalized, err := compileWorkspaceGlob(pattern)
	if err != nil {
		return GlobResult{}, err
	}
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return GlobResult{}, err
	}
	if item.Type == TypeLocal {
		return globLocal(ctx, item.RootPath, normalized, matcher)
	}
	return globSSH(ctx, item, normalized, matcher)
}

func (s *Service) GitStatus(ctx context.Context, workspaceID string) (CommandResult, error) {
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return CommandResult{}, err
	}
	return executeWorkspaceCommand(ctx, item, ".", "git status --short --branch", 20)
}

// GitDiff 返回当前项目未提交的差异；这是只读能力，不会绕过工作区审批策略。
func (s *Service) GitDiff(ctx context.Context, workspaceID string) (CommandResult, error) {
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return CommandResult{}, err
	}
	return executeWorkspaceCommand(ctx, item, ".", "git --no-pager diff --no-ext-diff -- .", 20)
}

// GitLog 返回最近提交摘要，限制数量和范围，避免把整个仓库历史带回聊天上下文。
func (s *Service) GitLog(ctx context.Context, workspaceID string) (CommandResult, error) {
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return CommandResult{}, err
	}
	return executeWorkspaceCommand(ctx, item, ".", "git --no-pager log --oneline --decorate -20 -- .", 20)
}

func captureLocalBaseline(ctx context.Context, root string) (WorktreeBaseline, error) {
	rootReal, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return WorktreeBaseline{}, fmt.Errorf("解析工作区根目录失败: %w", err)
	}
	info, err := os.Stat(rootReal)
	if err != nil {
		return WorktreeBaseline{}, fmt.Errorf("读取工作区根目录失败: %w", err)
	}
	if !info.IsDir() {
		return WorktreeBaseline{}, errors.New("工作区根目录不是目录")
	}

	stdout, stderr, truncated, gitErr := runGitCapture(ctx, rootReal, "rev-parse", "--is-inside-work-tree")
	if gitErr != nil {
		message := strings.ToLower(strings.TrimSpace(stdout + "\n" + stderr))
		if strings.Contains(message, "not a git repository") || strings.Contains(message, "不是 git 仓库") {
			return directoryBaseline(rootReal), nil
		}
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return WorktreeBaseline{}, ctx.Err()
		}
		return WorktreeBaseline{}, fmt.Errorf("检测 Git 工作区失败: %w", gitErr)
	}
	if truncated || strings.TrimSpace(stdout) != "true" {
		return WorktreeBaseline{RepositoryType: "unknown", StatusDigest: baselineDigest("unknown", rootReal, stdout+stderr), StatusKnown: false, Truncated: truncated, CapturedAt: time.Now().UTC()}, nil
	}

	head, headErr := optionalGitValue(ctx, rootReal, "rev-parse", "--verify", "HEAD")
	if headErr != nil {
		return WorktreeBaseline{}, fmt.Errorf("读取 Git HEAD 失败: %w", headErr)
	}
	branch, branchErr := optionalGitValue(ctx, rootReal, "symbolic-ref", "--short", "-q", "HEAD")
	if branchErr != nil {
		return WorktreeBaseline{}, fmt.Errorf("读取 Git 分支失败: %w", branchErr)
	}
	statusOut, statusErrOut, statusTruncated, statusErr := runGitCapture(ctx, rootReal, "status", "--porcelain=v1", "--untracked-files=all", "--no-renames", "-z")
	if statusErr != nil {
		return WorktreeBaseline{}, fmt.Errorf("读取 Git 状态失败: %w: %s", statusErr, sanitizeBaselineText(statusErrOut))
	}
	statusPayload := statusOut
	if statusTruncated {
		// The bounded capture may end in the middle of a NUL-delimited record.
		// Keep complete records for diagnostics, but never turn an incomplete
		// capture into a fatal admission error: StatusKnown below remains false.
		statusPayload = completeNULRecords(statusOut)
	}
	paths, parseErr := parseGitStatus(statusPayload)
	if parseErr != nil {
		return WorktreeBaseline{}, parseErr
	}
	known := !statusTruncated
	if len(paths) > maxBaselinePaths {
		// A full path list is part of the attribution contract. Once it is
		// bounded, the baseline is retained for diagnostics but cannot claim
		// complete attribution.
		known = false
		paths = paths[:maxBaselinePaths]
	}
	return WorktreeBaseline{
		RepositoryType: "git", HeadRevision: head, Branch: branch,
		StatusDigest: baselineStatusDigest("git", head, branch, paths), ChangedPaths: paths,
		StatusKnown: known, Truncated: statusTruncated || !known, CapturedAt: time.Now().UTC(),
	}, nil
}

func captureRemoteBaseline(ctx context.Context, item Workspace) (WorktreeBaseline, error) {
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return WorktreeBaseline{}, err
	}
	defer connection.Close()
	// All path values are inserted only through remoteBoundaryCommand. The
	// script emits NUL-delimited metadata and Git porcelain records, avoiding
	// ambiguity for spaces, tabs and newlines in filenames.
	script := remoteBoundaryCommand(item.RootPath, item.RootPath, `if git -C "$target" rev-parse --is-inside-work-tree >/dev/null 2>&1; then printf 'TYPE\tgit\0'; head=$(git -C "$target" rev-parse --verify HEAD 2>/dev/null || true); printf 'HEAD\t%s\0' "$head"; branch=$(git -C "$target" symbolic-ref --short -q HEAD 2>/dev/null || true); printf 'BRANCH\t%s\0' "$branch"; if git -C "$target" status --porcelain=v1 --untracked-files=all --no-renames -z; then printf 'STATUS\tok\0'; else printf 'STATUS\terror\0'; fi; else printf 'TYPE\tdirectory\0'; fi`)
	result, runErr := runSSH(ctx, connection.client, script, "", 30*time.Second)
	if runErr != nil {
		return WorktreeBaseline{}, runErr
	}
	baseline := WorktreeBaseline{RepositoryType: "unknown", StatusKnown: false, Truncated: result.Truncated, CapturedAt: time.Now().UTC()}
	statusRecords := make([]string, 0)
	statusOK := false
	for _, record := range strings.Split(result.Stdout, "\x00") {
		switch {
		case strings.HasPrefix(record, "TYPE\t"):
			baseline.RepositoryType = strings.TrimSpace(strings.TrimPrefix(record, "TYPE\t"))
		case strings.HasPrefix(record, "HEAD\t"):
			baseline.HeadRevision = strings.TrimSpace(strings.TrimPrefix(record, "HEAD\t"))
		case strings.HasPrefix(record, "BRANCH\t"):
			baseline.Branch = strings.TrimSpace(strings.TrimPrefix(record, "BRANCH\t"))
		case strings.HasPrefix(record, "STATUS\t"):
			statusOK = strings.TrimSpace(strings.TrimPrefix(record, "STATUS\t")) == "ok"
		case record != "":
			statusRecords = append(statusRecords, record)
		}
	}
	statusPayload := strings.Join(statusRecords, "\x00")
	if result.Truncated {
		statusPayload = completeNULRecords(statusPayload)
	}
	paths, parseErr := parseGitStatus(statusPayload)
	if parseErr != nil {
		return WorktreeBaseline{}, parseErr
	}
	if baseline.RepositoryType == "git" {
		baseline.ChangedPaths = paths
		pathOverflow := len(paths) > maxBaselinePaths
		if pathOverflow {
			paths = paths[:maxBaselinePaths]
			baseline.ChangedPaths = paths
		}
		baseline.StatusKnown = statusOK && !result.Truncated && !pathOverflow
		baseline.Truncated = result.Truncated || pathOverflow
		baseline.StatusDigest = baselineStatusDigest("git", baseline.HeadRevision, baseline.Branch, paths)
	} else {
		baseline.RepositoryType = firstNonEmptyBaseline(baseline.RepositoryType, "unknown")
		baseline.StatusDigest = baselineDigest(baseline.RepositoryType, item.RootPath, result.Stdout)
		baseline.StatusKnown = false
	}
	return baseline, nil
}

func completeNULRecords(value string) string {
	index := strings.LastIndexByte(value, '\x00')
	if index < 0 {
		return ""
	}
	return value[:index+1]
}

func directoryBaseline(root string) WorktreeBaseline {
	return WorktreeBaseline{RepositoryType: "directory", StatusDigest: baselineDigest("directory", root, ""), StatusKnown: false, CapturedAt: time.Now().UTC()}
}

func runGitCapture(ctx context.Context, root string, args ...string) (string, string, bool, error) {
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(runCtx, "git", args...)
	command.Dir = root
	command.Env = baselineCommandEnvironment()
	stdout := &limitedBuffer{limit: maxBaselineOutput}
	stderr := &limitedBuffer{limit: maxBaselineOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if runCtx.Err() != nil {
		return stdout.buffer.String(), stderr.buffer.String(), stdout.truncated || stderr.truncated, runCtx.Err()
	}
	return stdout.buffer.String(), stderr.buffer.String(), stdout.truncated || stderr.truncated, err
}

func optionalGitValue(ctx context.Context, root string, args ...string) (string, error) {
	stdout, stderr, _, err := runGitCapture(ctx, root, args...)
	if err != nil {
		if exitCode(err) == 128 || (len(args) > 0 && args[0] == "symbolic-ref" && exitCode(err) == 1) {
			// An unborn repository has no HEAD yet; this is a valid baseline.
			return "", nil
		}
		return "", fmt.Errorf("%w: %s", err, sanitizeBaselineText(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

func baselineCommandEnvironment() []string {
	environment := workspaceCommandEnvironment()
	return append(environment, "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
}

func parseGitStatus(value string) ([]PathStatus, error) {
	result := make([]PathStatus, 0)
	seen := make(map[string]string)
	for _, record := range strings.Split(value, "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("Git 状态记录格式无效")
		}
		status := record[:2]
		name := strings.ReplaceAll(record[3:], "\\", "/")
		cleaned := path.Clean(name)
		if name == "" || cleaned == "." || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, fmt.Errorf("Git 状态路径越过工作区边界")
		}
		if previous, ok := seen[cleaned]; ok {
			if previous != status {
				return nil, fmt.Errorf("Git 状态路径重复且状态不同: %s", cleaned)
			}
			continue
		}
		seen[cleaned] = status
		result = append(result, PathStatus{Path: cleaned, Status: status})
		if len(result) > maxBaselinePaths {
			// Continue parsing only enough to prove that the result is bounded;
			// the caller will mark attribution as incomplete.
			break
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result, nil
}

func baselineStatusDigest(repositoryType, head, branch string, paths []PathStatus) string {
	hasher := sha256.New()
	_, _ = fmt.Fprintf(hasher, "%s\x00%s\x00%s\x00", repositoryType, head, branch)
	for _, item := range paths {
		_, _ = fmt.Fprintf(hasher, "%s\x00%s\x00", item.Path, item.Status)
	}
	return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
}

func baselineDigest(repositoryType, root, payload string) string {
	sum := sha256.Sum256([]byte(repositoryType + "\x00" + root + "\x00" + payload))
	return fmt.Sprintf("sha256:%x", sum[:])
}

func sanitizeBaselineText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1024 {
		value = value[:1024] + "…"
	}
	return value
}

func firstNonEmptyBaseline(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func listLocalFiles(root, relative string) ([]FileEntry, error) {
	directory, err := resolveLocalPath(root, relative, true)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败: %w", err)
	}
	result := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		entryPath := filepath.ToSlash(filepath.Join(cleanRelativePath(relative), entry.Name()))
		result = append(result, FileEntry{Name: entry.Name(), Path: entryPath, IsDir: entry.IsDir(), Size: info.Size(), ModTime: info.ModTime()})
	}
	return result, nil
}

func readLimitedFile(target string) (string, error) {
	file, err := os.Open(target)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errors.New("目标路径是目录")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxFileBytes {
		return "", fmt.Errorf("文件超过 %d MB 读取上限", maxFileBytes>>20)
	}
	return string(data), nil
}

func searchLocal(ctx context.Context, root, relative, query string, mode SearchMode, matcher *regexp.Regexp) ([]SearchMatch, error) {
	return searchLocalWithOptions(ctx, root, relative, query, SearchOptions{Mode: mode, MaxHits: maxSearchHits}, matcher, nil)
}

func searchLocalWithOptions(ctx context.Context, root, relative, query string, options SearchOptions, matcher, globMatcher *regexp.Regexp) ([]SearchMatch, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.MaxHits <= 0 || options.MaxHits > maxSearchHits {
		options.MaxHits = maxSearchHits
	}
	if options.ContextLines < 0 {
		options.ContextLines = 0
	}
	base, err := resolveLocalPath(root, relative, true)
	if err != nil {
		return nil, err
	}
	result := make([]SearchMatch, 0, minInt(options.MaxHits, maxSearchHits))
	err = filepath.WalkDir(base, func(filePath string, entry os.DirEntry, walkErr error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if walkErr != nil || len(result) >= options.MaxHits {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		relativePath, relativeErr := filepath.Rel(root, filePath)
		if relativeErr != nil {
			return nil
		}
		relativePath = filepath.ToSlash(relativePath)
		if globMatcher != nil && !globMatcher.MatchString(relativePath) {
			return nil
		}
		safePath, boundaryErr := resolveLocalPath(root, relativePath, true)
		if boundaryErr != nil {
			// 搜索时跳过指向工作区外的符号链接，不把外部内容带入模型上下文。
			return nil
		}
		info, infoErr := os.Stat(safePath)
		if infoErr != nil || info.Size() > maxFileBytes {
			return nil
		}
		data, readErr := os.ReadFile(safePath)
		if readErr != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for index, line := range lines {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			matched := searchLineMatches(line, query, options, matcher)
			if matched {
				match := SearchMatch{Path: relativePath, Line: index + 1, Preview: truncateText(strings.TrimSpace(line), 300)}
				if options.ContextLines > 0 {
					match.ContextBefore = searchContextLines(lines, index-options.ContextLines, index, options.ContextLines)
					match.ContextAfter = searchContextLines(lines, index+1, index+1+options.ContextLines, options.ContextLines)
				}
				result = append(result, match)
				if len(result) >= options.MaxHits {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("搜索工作区失败: %w", err)
	}
	return result, nil
}

func searchLineMatches(line, query string, options SearchOptions, matcher *regexp.Regexp) bool {
	if options.Mode == SearchModeRegex {
		return matcher != nil && matcher.MatchString(line)
	}
	if options.CaseInsensitive {
		return strings.Contains(strings.ToLower(line), strings.ToLower(query))
	}
	return strings.Contains(line, query)
}

func searchContextLines(lines []string, start, end, limit int) []SearchContextLine {
	if limit <= 0 {
		return nil
	}
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return nil
	}
	result := make([]SearchContextLine, 0, end-start)
	for index := start; index < end && len(result) < limit; index++ {
		result = append(result, SearchContextLine{Line: index + 1, Text: truncateText(strings.TrimSpace(lines[index]), 300)})
	}
	return result
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func normalizeReadRange(startLine, endLine int) (int, int, error) {
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 {
		endLine = startLine + maxReadRangeLines - 1
	}
	if endLine < startLine {
		return 0, 0, fmt.Errorf("%w: 结束行不能小于起始行", ErrInvalidRequest)
	}
	if endLine-startLine+1 > maxReadRangeLines {
		return 0, 0, fmt.Errorf("%w: 单次范围读取最多 %d 行", ErrInvalidRequest, maxReadRangeLines)
	}
	return startLine, endLine, nil
}

func makeReadRangeResult(relative, content string, startLine, endLine int, includeLineNumbers bool) ReadRangeResult {
	lines := splitContentLines(content)
	result := ReadRangeResult{
		Path:               filepath.ToSlash(cleanRelativePath(relative)),
		StartLine:          startLine,
		EndLine:            endLine,
		TotalLines:         len(lines),
		IncludeLineNumbers: includeLineNumbers,
		ContentDigest:      digestContent(content),
	}
	if startLine <= len(lines) {
		last := endLine
		if last > len(lines) {
			last = len(lines)
		}
		var builder strings.Builder
		for index := startLine - 1; index < last; index++ {
			line := lines[index]
			if includeLineNumbers {
				line = strconv.Itoa(index+1) + "|" + line
			}
			builder.WriteString(line)
		}
		result.Content, result.Truncated = truncateUTF8(builder.String(), maxReadRangeBytes)
	}
	return result
}

func splitContentLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.SplitAfter(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func countContentLines(content string) int {
	return len(splitContentLines(content))
}

func digestContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", sum[:])
}

func truncateUTF8(value string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if len(value) <= limit {
		return value, false
	}
	if limit == 0 {
		return "", true
	}
	marker := []byte("…")
	if limit <= len(marker) {
		// Never return a partial UTF-8 sequence merely to fill a tiny limit.
		return "", true
	}
	cut := limit - len(marker)
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + string(marker), true
}

func sanitizeWorkspaceError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 1024 {
		message, _ = truncateUTF8(message, 1024)
	}
	return message
}

func normalizeGlobPattern(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || value == "." {
		return "", fmt.Errorf("%w: glob pattern 不能为空", ErrInvalidRequest)
	}
	if strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%w: glob pattern 必须是工作区内的相对路径", ErrInvalidRequest)
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == ".." {
			return "", fmt.Errorf("%w: glob pattern 不能越过工作区边界", ErrInvalidRequest)
		}
	}
	value = path.Clean(value)
	if value == "." || value == "" {
		return "", fmt.Errorf("%w: glob pattern 不能为空", ErrInvalidRequest)
	}
	return value, nil
}

func compileWorkspaceGlob(value string) (*regexp.Regexp, string, error) {
	normalized, err := normalizeGlobPattern(value)
	if err != nil {
		return nil, "", err
	}
	var expression strings.Builder
	expression.WriteByte('^')
	for index := 0; index < len(normalized); {
		switch normalized[index] {
		case '*':
			if index+1 < len(normalized) && normalized[index+1] == '*' {
				index += 2
				if index < len(normalized) && normalized[index] == '/' {
					index++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				index++
				expression.WriteString("[^/]*")
			}
		case '?':
			index++
			expression.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(normalized[index+1:], ']')
			if end < 0 {
				return nil, "", fmt.Errorf("%w: glob pattern 字符类未闭合", ErrInvalidRequest)
			}
			end += index + 1
			class := normalized[index+1 : end]
			if class == "" {
				return nil, "", fmt.Errorf("%w: glob pattern 字符类不能为空", ErrInvalidRequest)
			}
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			expression.WriteByte('[')
			expression.WriteString(class)
			expression.WriteByte(']')
			index = end + 1
		default:
			start := index
			index++
			expression.WriteString(regexp.QuoteMeta(normalized[start:index]))
		}
	}
	expression.WriteByte('$')
	matcher, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, "", fmt.Errorf("%w: glob pattern 无效", ErrInvalidRequest)
	}
	return matcher, normalized, nil
}

func globLocal(ctx context.Context, root, pattern string, matcher *regexp.Regexp) (GlobResult, error) {
	rootReal, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return GlobResult{}, fmt.Errorf("解析工作区根目录失败: %w", err)
	}
	result := GlobResult{Matches: make([]FileEntry, 0)}
	walkErr := filepath.WalkDir(rootReal, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if filePath == rootReal {
			return nil
		}
		relative, relativeErr := filepath.Rel(rootReal, filePath)
		if relativeErr != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if !matcher.MatchString(relative) {
			return nil
		}
		safePath, boundaryErr := resolveLocalPath(rootReal, relative, true)
		if boundaryErr != nil {
			return nil
		}
		info, infoErr := os.Stat(safePath)
		if infoErr != nil {
			return nil
		}
		if len(result.Matches) >= maxGlobMatches {
			result.Truncated = true
			return fs.SkipAll
		}
		result.Matches = append(result.Matches, FileEntry{Name: path.Base(relative), Path: relative, IsDir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
		return GlobResult{}, walkErr
	}
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return GlobResult{}, fmt.Errorf("搜索 glob 失败: %w", walkErr)
	}
	sortFileEntries(result.Matches)
	return result, nil
}

func sortFileEntries(entries []FileEntry) {
	sort.Slice(entries, func(left, right int) bool { return entries[left].Path < entries[right].Path })
}

func globSSH(ctx context.Context, item Workspace, pattern string, matcher *regexp.Regexp) (GlobResult, error) {
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return GlobResult{}, err
	}
	defer connection.Close()
	// The command is fixed and the only user-controlled value is shell-quoted
	// into the boundary check. find -P avoids following symlinked directories.
	command := remoteBoundaryCommand(item.RootPath, item.RootPath, `find -P "$target" \( -type f -o -type d \) -exec sh -c 'for p do if [ -d "$p" ]; then k=D; else k=F; fi; printf "%s\t%s\0" "$k" "$p"; done' sh {} +`)
	result, runErr := runSSH(ctx, connection.client, command, "", 30*time.Second)
	if runErr != nil {
		return GlobResult{}, runErr
	}
	globResult := GlobResult{Matches: make([]FileEntry, 0)}
	for _, record := range strings.Split(result.Output, "\x00") {
		fields := strings.SplitN(record, "\t", 2)
		if len(fields) != 2 || fields[1] == "" {
			continue
		}
		absolute := path.Clean(fields[1])
		prefix := strings.TrimSuffix(path.Clean(item.RootPath), "/") + "/"
		if !strings.HasPrefix(absolute, prefix) {
			continue
		}
		relative := strings.TrimPrefix(absolute, prefix)
		if !matcher.MatchString(relative) {
			continue
		}
		if len(globResult.Matches) >= maxGlobMatches {
			globResult.Truncated = true
			break
		}
		globResult.Matches = append(globResult.Matches, FileEntry{Name: path.Base(relative), Path: relative, IsDir: fields[0] == "D"})
	}
	if result.Truncated {
		globResult.Truncated = true
	}
	sortFileEntries(globResult.Matches)
	return globResult, nil
}

func resolveLocalPath(root, relative string, mustExist bool) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: 工作区根目录必须是绝对路径", ErrInvalidRequest)
	}
	rootReal, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("解析工作区根目录失败: %w", err)
	}
	relative = cleanRelativePath(relative)
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: 路径超出工作区", ErrInvalidRequest)
	}
	target := filepath.Join(rootReal, relative)
	checked := target
	if mustExist {
		checked, err = filepath.EvalSymlinks(target)
	} else {
		// 新建文件或目录时，目标本身以及中间目录可能尚不存在；沿路径向上找到最近的现有父级，
		// 再把不存在的部分拼回去，仍然可以检查父级符号链接是否越过工作区边界。
		checked, err = evalWithExistingParent(target)
	}
	if err != nil {
		return "", fmt.Errorf("解析工作区路径失败: %w", err)
	}
	if !pathWithin(rootReal, checked) {
		return "", fmt.Errorf("%w: 路径或符号链接超出工作区", ErrInvalidRequest)
	}
	return target, nil
}

func evalWithExistingParent(target string) (string, error) {
	current := target
	missing := make([]string, 0, 4)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func cleanRelativePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "."
	}
	return filepath.Clean(filepath.FromSlash(value))
}

func listSSHFiles(ctx context.Context, item Workspace, relative string) ([]FileEntry, error) {
	target, err := resolveRemotePath(item.RootPath, relative)
	if err != nil {
		return nil, err
	}
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	script := remoteBoundaryCommand(item.RootPath, target, "d=\"$target\"; for p in \"$d\"/* \"$d\"/.[!.]* \"$d\"/..?*; do [ -e \"$p\" ] || continue; if [ -d \"$p\" ]; then t=d; else t=f; fi; printf '%s\\t%s\\0' \"$t\" \"${p##*/}\"; done")
	result, err := runSSH(ctx, connection.client, script, "", 20*time.Second)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(result.Output, "\x00")
	entries := make([]FileEntry, 0, len(parts))
	for _, part := range parts {
		fields := strings.SplitN(part, "\t", 2)
		if len(fields) != 2 || fields[1] == "" {
			continue
		}
		entries = append(entries, FileEntry{Name: fields[1], Path: path.Join(cleanRemoteRelative(relative), fields[1]), IsDir: fields[0] == "d"})
	}
	return entries, nil
}

func readSSHFile(ctx context.Context, item Workspace, relative string) (string, error) {
	target, err := resolveRemotePath(item.RootPath, relative)
	if err != nil {
		return "", err
	}
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	result, err := runSSH(ctx, connection.client, remoteBoundaryCommand(item.RootPath, target, "cat -- \"$target\""), "", 20*time.Second)
	if err != nil {
		return "", err
	}
	if result.Truncated {
		return "", fmt.Errorf("文件超过 %d MB 读取上限", maxFileBytes>>20)
	}
	return result.Output, nil
}

func statSSHFile(ctx context.Context, item Workspace, relative string) (FileStat, error) {
	target, err := resolveRemotePath(item.RootPath, relative)
	if err != nil {
		return FileStat{}, err
	}
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return FileStat{}, err
	}
	defer connection.Close()
	// 同时兼容 GNU/Linux 和 BSD/macOS 的 stat，输出采用固定键值便于解析。
	command := remoteBoundaryCommand(item.RootPath, target, "if [ -d \"$target\" ]; then kind=dir; else kind=file; fi; size=$(wc -c < \"$target\" 2>/dev/null || printf 0); mtime=$(stat -c %Y \"$target\" 2>/dev/null || stat -f %m \"$target\" 2>/dev/null || printf 0); mode=$(stat -c %A \"$target\" 2>/dev/null || stat -f %Sp \"$target\" 2>/dev/null || printf ''); printf 'KIND\\t%s\\nSIZE\\t%s\\nMTIME\\t%s\\nMODE\\t%s\\n' \"$kind\" \"$size\" \"$mtime\" \"$mode\"")
	result, err := runSSH(ctx, connection.client, command, "", 20*time.Second)
	if err != nil {
		return FileStat{}, err
	}
	stat := FileStat{Path: cleanRemoteRelative(relative)}
	for _, line := range strings.Split(result.Output, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "KIND":
			stat.IsDir = parts[1] == "dir"
		case "SIZE":
			stat.Size, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		case "MTIME":
			seconds, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			if seconds > 0 {
				stat.ModTime = time.Unix(seconds, 0).UTC()
			}
		case "MODE":
			stat.Mode = parts[1]
		}
	}
	return stat, nil
}

func searchSSH(ctx context.Context, item Workspace, relative, query string, mode SearchMode) ([]SearchMatch, error) {
	query, matcher, options, globMatcher, err := compileSearchOptions(query, SearchOptions{Mode: mode})
	if err != nil {
		return nil, err
	}
	return searchSSHWithOptions(ctx, item, relative, query, options, matcher, globMatcher)
}

type remoteSearchRecord struct {
	Path    string
	Line    int
	Text    string
	IsMatch bool
}

func searchSSHWithOptions(ctx context.Context, item Workspace, relative, query string, options SearchOptions, matcher, globMatcher *regexp.Regexp) ([]SearchMatch, error) {
	target, err := resolveRemotePath(item.RootPath, relative)
	if err != nil {
		return nil, err
	}
	connection, _, err := dialSSH(ctx, item, false)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	// 使用 -r 而不是 -R，避免跟随目录中的符号链接搜索到工作区外。
	// GNU/BSD grep both provide -F (literal) and -E (ERE). The query was
	// already compiled with Go's linear-time regexp before reaching SSH. We
	// still classify matches locally so context and the same hit bound apply
	// across transports; remote grep's ERE syntax can be a smaller subset.
	grepMode := "F"
	if options.Mode == SearchModeRegex {
		grepMode = "E"
	}
	flags := "-rIn" + grepMode + "I"
	if options.CaseInsensitive {
		flags = "-rIni" + grepMode + "I"
	}
	contextArg := ""
	if options.ContextLines > 0 {
		contextArg = " -C " + strconv.Itoa(options.ContextLines)
	}
	command := remoteBoundaryCommand(item.RootPath, target, "grep "+flags+contextArg+" --exclude-dir=.git --exclude-dir=node_modules -- "+shellQuote(query)+" \"$target\"")
	result, runErr := runSSH(ctx, connection.client, command, "", 30*time.Second)
	if runErr != nil && result.ExitCode != 1 {
		return nil, runErr
	}
	records := make([]remoteSearchRecord, 0)
	byPath := make(map[string]map[int]string)
	for _, rawLine := range strings.Split(result.Output, "\n") {
		if rawLine == "" || rawLine == "--" {
			continue
		}
		record, ok := parseRemoteSearchRecord(rawLine, item.RootPath)
		if !ok {
			continue
		}
		if globMatcher != nil && !globMatcher.MatchString(record.Path) {
			continue
		}
		record.IsMatch = searchLineMatches(record.Text, query, options, matcher)
		records = append(records, record)
		if byPath[record.Path] == nil {
			byPath[record.Path] = make(map[int]string)
		}
		byPath[record.Path][record.Line] = record.Text
	}
	matches := make([]SearchMatch, 0, minInt(options.MaxHits, maxSearchHits))
	for _, record := range records {
		if !record.IsMatch {
			continue
		}
		match := SearchMatch{Path: record.Path, Line: record.Line, Preview: truncateText(strings.TrimSpace(record.Text), 300)}
		if options.ContextLines > 0 {
			lines := byPath[record.Path]
			before := make([]SearchContextLine, 0, options.ContextLines)
			after := make([]SearchContextLine, 0, options.ContextLines)
			for lineNumber := record.Line - options.ContextLines; lineNumber < record.Line; lineNumber++ {
				if text, exists := lines[lineNumber]; exists {
					before = append(before, SearchContextLine{Line: lineNumber, Text: truncateText(strings.TrimSpace(text), 300)})
				}
			}
			for lineNumber := record.Line + 1; lineNumber <= record.Line+options.ContextLines; lineNumber++ {
				if text, exists := lines[lineNumber]; exists {
					after = append(after, SearchContextLine{Line: lineNumber, Text: truncateText(strings.TrimSpace(text), 300)})
				}
			}
			match.ContextBefore, match.ContextAfter = before, after
		}
		matches = append(matches, match)
		if len(matches) >= options.MaxHits {
			break
		}
	}
	sort.SliceStable(matches, func(left, right int) bool {
		if matches[left].Path == matches[right].Path {
			return matches[left].Line < matches[right].Line
		}
		return matches[left].Path < matches[right].Path
	})
	return matches, nil
}

func parseRemoteSearchRecord(rawLine, root string) (remoteSearchRecord, bool) {
	// grep -n emits path:line:text for matches and path-line-text for context
	// lines when -C is active. Find a delimiter followed by decimal digits,
	// then use the following delimiter to distinguish both forms. This keeps
	// filenames containing ordinary ':' or '-' usable in the common case.
	for index := 0; index < len(rawLine); index++ {
		if rawLine[index] != ':' && rawLine[index] != '-' {
			continue
		}
		lineStart := index + 1
		lineEnd := lineStart
		for lineEnd < len(rawLine) && rawLine[lineEnd] >= '0' && rawLine[lineEnd] <= '9' {
			lineEnd++
		}
		if lineEnd == lineStart || lineEnd >= len(rawLine) || (rawLine[lineEnd] != ':' && rawLine[lineEnd] != '-') {
			continue
		}
		lineNumber, err := strconv.Atoi(rawLine[lineStart:lineEnd])
		if err != nil || lineNumber <= 0 {
			continue
		}
		absolute := rawLine[:index]
		prefix := strings.TrimSuffix(path.Clean(root), "/") + "/"
		relativePath := strings.TrimPrefix(absolute, prefix)
		if relativePath == absolute || relativePath == "" || path.IsAbs(relativePath) || strings.HasPrefix(relativePath, "../") {
			continue
		}
		return remoteSearchRecord{Path: relativePath, Line: lineNumber, Text: rawLine[lineEnd+1:], IsMatch: rawLine[lineEnd] == ':'}, true
	}
	return remoteSearchRecord{}, false
}

func resolveRemotePath(root, relative string) (string, error) {
	if !strings.HasPrefix(root, "/") {
		return "", fmt.Errorf("%w: SSH 工作区根目录必须是绝对路径", ErrInvalidRequest)
	}
	relative = cleanRemoteRelative(relative)
	if relative == ".." || strings.HasPrefix(relative, "../") || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("%w: 路径超出工作区", ErrInvalidRequest)
	}
	return path.Join(root, relative), nil
}

func cleanRemoteRelative(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "."
	}
	return path.Clean(value)
}

func remoteBoundaryCommand(root, target, command string) string {
	return "root=$(realpath -- " + shellQuote(root) + ") || exit 2; target=$(realpath -- " + shellQuote(target) + ") || exit 2; case \"$target\" in \"$root\"|\"$root\"/*) " + command + ";; *) echo '路径超出工作区' >&2; exit 3;; esac"
}

type sshConnection struct {
	client    *ssh.Client
	agentConn net.Conn
}

func (c *sshConnection) Close() {
	if c.client != nil {
		_ = c.client.Close()
	}
	if c.agentConn != nil {
		_ = c.agentConn.Close()
	}
}

func dialSSH(ctx context.Context, item Workspace, allowUnknownFingerprint bool) (*sshConnection, string, error) {
	authMethods, agentConn, err := sshAuthMethods(item)
	if err != nil {
		return nil, "", err
	}
	fingerprint := ""
	callback := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fingerprint = ssh.FingerprintSHA256(key)
		if item.HostKeyFingerprint == "" && allowUnknownFingerprint {
			return nil
		}
		if fingerprint != item.HostKeyFingerprint {
			return fmt.Errorf("SSH 主机指纹不匹配，实际为 %s", fingerprint)
		}
		return nil
	}
	config := &ssh.ClientConfig{User: item.User, Auth: authMethods, HostKeyCallback: callback, Timeout: 10 * time.Second}
	address := net.JoinHostPort(item.Host, strconv.Itoa(item.Port))
	netConn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		if agentConn != nil {
			_ = agentConn.Close()
		}
		return nil, fingerprint, fmt.Errorf("连接 SSH 失败: %w", err)
	}
	clientConn, channels, requests, err := ssh.NewClientConn(netConn, address, config)
	if err != nil {
		_ = netConn.Close()
		if agentConn != nil {
			_ = agentConn.Close()
		}
		return nil, fingerprint, fmt.Errorf("SSH 握手失败: %w", err)
	}
	return &sshConnection{client: ssh.NewClient(clientConn, channels, requests), agentConn: agentConn}, fingerprint, nil
}

func sshAuthMethods(item Workspace) ([]ssh.AuthMethod, net.Conn, error) {
	switch item.AuthType {
	case AuthPassword:
		if item.Password == "" {
			return nil, nil, errors.New("SSH 密码尚未配置")
		}
		return []ssh.AuthMethod{ssh.Password(item.Password)}, nil, nil
	case AuthKeyFile:
		keyData, err := os.ReadFile(item.KeyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("读取 SSH 私钥失败: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(keyData)
		if err != nil {
			return nil, nil, fmt.Errorf("解析 SSH 私钥失败: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil, nil
	case AuthAgent:
		socket := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
		if socket == "" {
			return nil, nil, errors.New("SSH_AUTH_SOCK 未设置，无法使用 ssh-agent")
		}
		connection, err := net.Dial("unix", socket)
		if err != nil {
			return nil, nil, fmt.Errorf("连接 ssh-agent 失败: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(connection).Signers)}, connection, nil
	default:
		return nil, nil, errors.New("SSH 认证方式无效")
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
	written   int64
}

// CommandChunk is emitted while a command is running. It is intentionally
// transport-neutral so the Agent Runtime can persist it as AgentEvent without
// making Workspace depend on the runtime package.
type CommandChunk struct {
	CommandRunID string    `json:"command_run_id,omitempty"`
	Sequence     uint64    `json:"sequence,omitempty"`
	Offset       int64     `json:"offset,omitempty"`
	Stream       string    `json:"stream"`
	Data         string    `json:"data"`
	Timestamp    time.Time `json:"timestamp"`
	Truncated    bool      `json:"truncated,omitempty"`
}

type commandObserverKey struct{}

// WithCommandObserver attaches a non-blocking observer to a command context.
// The observer is called from pipe-draining goroutines and must not block.
func WithCommandObserver(ctx context.Context, observer func(CommandChunk)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, commandObserverKey{}, observer)
}

func commandObserver(ctx context.Context) func(CommandChunk) {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(commandObserverKey{}).(func(CommandChunk))
	return observer
}

type observedBuffer struct {
	buffer    limitedBuffer
	stream    string
	observer  func(CommandChunk)
	sink      func(CommandChunk)
	commandID string
	sequence  *atomic.Uint64
	offset    atomic.Int64
	mu        sync.Mutex
}

func newObservedBuffer(ctx context.Context, stream string) *observedBuffer {
	buffer := &observedBuffer{buffer: limitedBuffer{limit: maxOutputBytes / 2}, stream: stream, observer: commandObserver(ctx), sink: commandChunkSink(ctx), commandID: CommandRunIDFromContext(ctx)}
	if ctx != nil {
		if state, _ := ctx.Value(commandRunContextKey{}).(*commandChunkState); state != nil {
			buffer.sequence = &state.sequence
		}
	}
	return buffer
}

func (b *observedBuffer) Write(value []byte) (int, error) {
	startOffset := b.offset.Add(int64(len(value))) - int64(len(value))
	b.mu.Lock()
	before := b.buffer.buffer.Len()
	n, err := b.buffer.Write(value)
	data := append([]byte(nil), b.buffer.buffer.Bytes()[before:]...)
	truncated := b.buffer.truncated
	b.mu.Unlock()
	if len(data) > 0 {
		chunk := CommandChunk{CommandRunID: b.commandID, Stream: b.stream, Data: string(data), Timestamp: time.Now().UTC(), Truncated: truncated}
		if b.sequence != nil {
			chunk.Sequence = b.sequence.Add(1)
		}
		chunk.Offset = startOffset
		if b.observer != nil {
			b.observer(chunk)
		}
		if b.sink != nil {
			b.sink(chunk)
		}
	}
	return n, err
}

func (b *observedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.buffer.String()
}

func (b *observedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.truncated
}

func drainCommandPipe(reader io.Reader, buffer *observedBuffer, done *sync.WaitGroup) {
	defer done.Done()
	chunk := make([]byte, 32*1024)
	for {
		count, err := reader.Read(chunk)
		if count > 0 {
			_, _ = buffer.Write(chunk[:count])
		}
		if err != nil {
			return
		}
	}
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	b.written += int64(original)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
			b.truncated = true
		}
		_, _ = b.buffer.Write(value)
	} else {
		b.truncated = true
	}
	return original, nil
}

func (b *observedBuffer) BytesWritten() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.written
}

func runSSH(ctx context.Context, client *ssh.Client, command, stdin string, timeout time.Duration) (CommandResult, error) {
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	session, err := client.NewSession()
	if err != nil {
		return CommandResult{ExitCode: -1, StartFailed: true, DurationMS: time.Since(started).Milliseconds()}, fmt.Errorf("创建 SSH 会话失败: %w", err)
	}
	defer session.Close()
	stdout := newObservedBuffer(ctx, "stdout")
	stderr := newObservedBuffer(ctx, "stderr")
	session.Stdout = stdout
	session.Stderr = stderr
	if stdin != "" {
		session.Stdin = strings.NewReader(stdin)
	}
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err := <-done:
		result := CommandResult{Output: stdout.String() + stderr.String(), Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode(err), Truncated: stdout.Truncated() || stderr.Truncated(), Started: true, DurationMS: time.Since(started).Milliseconds(), StdoutBytes: stdout.BytesWritten(), StderrBytes: stderr.BytesWritten()}
		if err != nil {
			return result, fmt.Errorf("SSH 命令失败（退出码 %d）: %s", result.ExitCode, strings.TrimSpace(result.Output))
		}
		return result, nil
	case <-runCtx.Done():
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = session.Signal(ssh.SIGKILL)
		select {
		case err := <-done:
			result := CommandResult{Output: stdout.String() + stderr.String(), Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode(err), Truncated: stdout.Truncated() || stderr.Truncated(), TimedOut: timedOut, Started: true, DurationMS: time.Since(started).Milliseconds(), StdoutBytes: stdout.BytesWritten(), StderrBytes: stderr.BytesWritten()}
			if timedOut {
				return result, fmt.Errorf("SSH 命令超时: %w", runCtx.Err())
			}
			return result, fmt.Errorf("SSH 命令已取消: %w", runCtx.Err())
		case <-time.After(2 * time.Second):
			return CommandResult{Output: stdout.String() + stderr.String(), Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1, Truncated: stdout.Truncated() || stderr.Truncated(), TimedOut: timedOut, Unknown: true, Started: true, DurationMS: time.Since(started).Milliseconds(), StdoutBytes: stdout.BytesWritten(), StderrBytes: stderr.BytesWritten()}, fmt.Errorf("SSH 命令状态未知: %w", runCtx.Err())
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	var sshErr *ssh.ExitError
	if errors.As(err, &sshErr) {
		return sshErr.ExitStatus()
	}
	return -1
}

func truncateText(value string, limit int) string {
	characters := []rune(value)
	if len(characters) <= limit {
		return value
	}
	return string(characters[:limit]) + "…"
}
