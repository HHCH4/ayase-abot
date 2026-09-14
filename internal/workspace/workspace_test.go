package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"Abot/internal/artifact"
)

func TestLocalWorkspaceKeepsPathBoundaryAndRequiresApproval(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatalf("创建工作区服务失败: %v", err)
	}
	item, err := service.Save(ctx, SaveRequest{Workspace: Workspace{
		ID: "demo", Name: "演示项目", Type: TypeLocal, RootPath: root, Enabled: true,
	}})
	if err != nil {
		t.Fatalf("保存本机工作区失败: %v", err)
	}
	expectedRoot, _ := filepath.EvalSymlinks(root)
	if item.RootPath != expectedRoot {
		t.Fatalf("保存后的根目录 = %q，期望规范路径 %q", item.RootPath, expectedRoot)
	}
	tools, err := service.Tools(ctx, "demo")
	if err != nil || len(tools) != 16 {
		t.Fatalf("创建工作区 Agent 工具失败: count=%d err=%v", len(tools), err)
	}
	readRange, err := service.ReadRange(ctx, "demo", "README.md", 1, 1, true)
	if err != nil || readRange.Content != "1|hello workspace\n" || readRange.TotalLines != 1 || readRange.ContentDigest == "" {
		t.Fatalf("范围读取结果不正确: %#v err=%v", readRange, err)
	}
	if _, err := service.ReadRange(ctx, "demo", "README.md", 1, maxReadRangeLines+1, false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("过大范围读取错误=%v，期望 ErrInvalidRequest", err)
	}
	readMany, err := service.ReadMany(ctx, "demo", []string{"README.md", "missing.txt"})
	if err != nil || len(readMany.Files) != 2 || readMany.Files[0].Content != "hello workspace\n" || readMany.Files[0].ContentDigest == "" || readMany.Files[1].Error == "" {
		t.Fatalf("批量读取应保留成功和局部错误: %#v err=%v", readMany, err)
	}
	if _, err := service.ReadMany(ctx, "demo", []string{"README.md", "README.md"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("重复批量读取路径错误=%v，期望 ErrInvalidRequest", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "agent", "kernel.go"), []byte("package agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "agent", "kernel_test.go"), []byte("package agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	glob, err := service.Glob(ctx, "demo", "internal/**/*.go")
	if err != nil || len(glob.Matches) != 2 || glob.Matches[0].Path != "internal/agent/kernel.go" || glob.Matches[1].Path != "internal/agent/kernel_test.go" {
		t.Fatalf("递归 glob 结果不稳定或不完整: %#v err=%v", glob, err)
	}
	if _, err := service.Glob(ctx, "demo", "../*.go"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("glob 越界错误=%v，期望 ErrInvalidRequest", err)
	}
	content, err := service.ReadFile(ctx, "demo", "README.md")
	if err != nil || content != "hello workspace\n" {
		t.Fatalf("读取工作区文件结果不正确: content=%q err=%v", content, err)
	}
	if _, err := service.ReadFile(ctx, "demo", "../secret.txt"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("父目录逃逸错误 = %v，期望 ErrInvalidRequest", err)
	}
	if _, err := service.ReadFile(ctx, "demo", "escape/secret.txt"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("符号链接逃逸错误 = %v，期望 ErrInvalidRequest", err)
	}
	matches, err := service.Search(ctx, "demo", ".", "secret")
	if err != nil || len(matches) != 0 {
		t.Fatalf("搜索不应读取工作区外的符号链接: matches=%#v err=%v", matches, err)
	}
	if _, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "demo", Type: OperationWriteFile, Path: "escape/secret.txt", Content: "overwrite",
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("写入符号链接逃逸错误 = %v，期望 ErrInvalidRequest", err)
	}

	operation, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "demo", Type: OperationWriteFile, Path: "generated.txt", Content: "需要批准",
	})
	if err != nil {
		t.Fatalf("创建待审批写入失败: %v", err)
	}
	if operation.Status != OperationPrepared {
		t.Fatalf("新建操作状态=%s，期望 prepared", operation.Status)
	}
	if !strings.Contains(operation.Diff, "+需要批准") {
		t.Fatalf("待审批写入没有生成 diff: %q", operation.Diff)
	}
	if _, err := os.Stat(filepath.Join(root, "generated.txt")); !os.IsNotExist(err) {
		t.Fatal("待审批操作不应提前写入文件")
	}
	approved, err := service.ApproveOperation(ctx, operation.ID)
	if err != nil || approved.Status != OperationCompleted {
		t.Fatalf("批准写入失败: operation=%#v err=%v", approved, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "generated.txt"))
	if err != nil || string(data) != "需要批准" {
		t.Fatalf("批准后的文件内容不正确: data=%q err=%v", data, err)
	}

	mkdir, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "demo", Type: OperationMakeDirectory, Path: "nested/source",
	})
	if err != nil {
		t.Fatalf("创建新建目录操作失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "nested")); !os.IsNotExist(err) {
		t.Fatal("待审批新建目录不应提前改变文件系统")
	}
	if _, err := service.ApproveOperation(ctx, mkdir.ID); err != nil {
		t.Fatalf("批准新建目录失败: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "nested", "source")); err != nil || !info.IsDir() {
		t.Fatalf("新建目录结果不正确: info=%v err=%v", info, err)
	}

	if err := os.WriteFile(filepath.Join(root, "nested", "source", "config.txt"), []byte("before value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "demo", Type: OperationPatchFile, Path: "nested/source/config.txt", OldText: "before", NewText: "after",
	})
	if err != nil {
		t.Fatalf("创建精确补丁操作失败: %v", err)
	}
	if !strings.Contains(patch.Diff, "-before value") || !strings.Contains(patch.Diff, "+after value") {
		t.Fatalf("精确补丁 diff 不正确: %q", patch.Diff)
	}
	if _, err := service.ApproveOperation(ctx, patch.ID); err != nil {
		t.Fatalf("批准精确补丁失败: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(root, "nested", "source", "config.txt"))
	if err != nil || string(data) != "after value\n" {
		t.Fatalf("精确补丁后的文件内容不正确: data=%q err=%v", data, err)
	}

	delete, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "demo", Type: OperationDeletePath, Path: "nested/source/config.txt",
	})
	if err != nil {
		t.Fatalf("创建删除操作失败: %v", err)
	}
	if _, err := service.ApproveOperation(ctx, delete.ID); err != nil {
		t.Fatalf("批准删除操作失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "nested", "source", "config.txt")); !os.IsNotExist(err) {
		t.Fatal("批准删除后文件仍然存在")
	}

	command, err := service.CreateOperation(ctx, OperationRequest{
		WorkspaceID: "demo", Type: OperationCommand, Command: "printf command-ok", CWD: ".", Timeout: 5,
	})
	if err != nil {
		t.Fatalf("创建命令操作失败: %v", err)
	}
	command, err = service.ApproveOperation(ctx, command.ID)
	if err != nil || command.Result != "command-ok" {
		t.Fatalf("执行批准命令失败: operation=%#v err=%v", command, err)
	}

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changeSet, err := service.CreateOperation(ctx, OperationRequest{WorkspaceID: "demo", Type: OperationPatchSet, Patches: []FilePatch{
		{Path: "a.txt", OldText: "one", NewText: "ONE"}, {Path: "b.txt", OldText: "two", NewText: "TWO"},
	}})
	if err != nil {
		t.Fatalf("创建 ChangeSet 失败: %v", err)
	}
	if !strings.Contains(changeSet.Diff, "a.txt") || !strings.Contains(changeSet.Diff, "b.txt") {
		t.Fatalf("ChangeSet diff 不完整: %q", changeSet.Diff)
	}
	if _, err := service.ApproveOperation(ctx, changeSet.ID); err != nil {
		t.Fatalf("批准 ChangeSet 失败: %v", err)
	}
	for file, want := range map[string]string{"a.txt": "ONE\n", "b.txt": "TWO\n"} {
		data, readErr := os.ReadFile(filepath.Join(root, file))
		if readErr != nil || string(data) != want {
			t.Fatalf("ChangeSet %s 内容 = %q, err=%v", file, data, readErr)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hunks, err := service.CreateOperation(ctx, OperationRequest{WorkspaceID: "demo", Type: OperationPatchSet, Patches: []FilePatch{
		{Path: "a.txt", OldText: "one", NewText: "ONE"}, {Path: "a.txt", OldText: "second", NewText: "SECOND"},
	}})
	if err != nil {
		t.Fatalf("创建同文件多 hunk ChangeSet 失败: %v", err)
	}
	if _, err := service.ApproveOperation(ctx, hunks.ID); err != nil {
		t.Fatalf("批准同文件多 hunk ChangeSet 失败: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(data) != "ONE\nSECOND\n" {
		t.Fatalf("同文件多 hunk 结果 = %q, err=%v", data, err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	atomicStale, err := service.CreateOperation(ctx, OperationRequest{WorkspaceID: "demo", Type: OperationPatchSet, Patches: []FilePatch{
		{Path: "a.txt", OldText: "one", NewText: "ONE"}, {Path: "b.txt", OldText: "two", NewText: "TWO"},
	}})
	if err != nil {
		t.Fatalf("创建跨文件 stale ChangeSet 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveOperation(ctx, atomicStale.ID); !errors.Is(err, ErrOperationState) {
		t.Fatalf("跨文件 stale ChangeSet 错误 = %v，期望 ErrOperationState", err)
	}
	data, err = os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(data) != "one\n" {
		t.Fatalf("stale ChangeSet 不应部分提交，a.txt = %q, err=%v", data, err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("ONE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := service.CreateOperation(ctx, OperationRequest{WorkspaceID: "demo", Type: OperationPatchFile, Path: "a.txt", OldText: "ONE", NewText: "stale"})
	if err != nil {
		t.Fatalf("创建 stale 补丁失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveOperation(ctx, stale.ID); !errors.Is(err, ErrOperationState) {
		t.Fatalf("stale 补丁错误 = %v，期望 ErrOperationState", err)
	}
}

func TestWorkspaceSearchRegexIsBoundedAndContextAware(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("todo-123\nTODO-abc\nplain todo-9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("todo-456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte{'t', 'o', 'd', 'o', 0, '-', '9', '\n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "regex", Name: "regex", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	matches, err := service.SearchWithMode(context.Background(), "regex", ".", `^todo-[0-9]+$`, SearchModeRegex)
	if err != nil || len(matches) != 2 || matches[0].Path != "a.txt" || matches[0].Line != 1 || matches[1].Path != "b.txt" {
		t.Fatalf("正则搜索结果不正确: %#v err=%v", matches, err)
	}
	caseInsensitive, err := service.SearchWithOptions(context.Background(), "regex", ".", `TODO-[A-Z]+`, SearchOptions{Mode: SearchModeRegex, CaseInsensitive: true, Glob: "a.txt", ContextLines: 1, MaxHits: 1})
	if err != nil || len(caseInsensitive) != 1 || caseInsensitive[0].Line != 2 || len(caseInsensitive[0].ContextBefore) != 1 || caseInsensitive[0].ContextBefore[0].Line != 1 || len(caseInsensitive[0].ContextAfter) != 1 || caseInsensitive[0].ContextAfter[0].Line != 3 {
		t.Fatalf("大小写、glob、上下文或命中上限结果不正确: %#v err=%v", caseInsensitive, err)
	}
	literal, err := service.Search(context.Background(), "regex", ".", "todo-")
	if err != nil || len(literal) != 3 {
		t.Fatalf("默认原文搜索应保持兼容: %#v err=%v", literal, err)
	}
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(strings.Repeat("todo\n", maxSearchHits+5)), 0o600); err != nil {
		t.Fatal(err)
	}
	bounded, err := service.SearchWithMode(context.Background(), "regex", ".", "todo", SearchModeLiteral)
	if err != nil || len(bounded) != maxSearchHits {
		t.Fatalf("搜索命中数应封顶 %d: len=%d err=%v", maxSearchHits, len(bounded), err)
	}
	if _, err := service.SearchWithMode(context.Background(), "regex", ".", "[", SearchModeRegex); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("非法正则应返回 ErrInvalidRequest: %v", err)
	}
	if _, err := service.SearchWithMode(context.Background(), "regex", ".", "todo", SearchMode("glob")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("非法搜索模式应返回 ErrInvalidRequest: %v", err)
	}
	if _, err := service.SearchWithMode(context.Background(), "regex", ".", strings.Repeat("x", maxSearchQueryBytes+1), SearchModeLiteral); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("超长搜索内容应返回 ErrInvalidRequest: %v", err)
	}
	if _, err := service.SearchWithMode(context.Background(), "regex", ".", "line\npattern", SearchModeRegex); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("跨行正则应返回 ErrInvalidRequest: %v", err)
	}
	if _, err := service.SearchWithMode(context.Background(), "regex", ".", string([]byte{0xff}), SearchModeLiteral); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("无效 UTF-8 搜索内容应返回 ErrInvalidRequest: %v", err)
	}
	if _, err := service.SearchWithOptions(context.Background(), "regex", ".", "todo", SearchOptions{MaxHits: maxSearchHits + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("超出 max_hits 上限应返回 ErrInvalidRequest: %v", err)
	}
	if _, err := service.SearchWithOptions(context.Background(), "regex", ".", "todo", SearchOptions{ContextLines: maxSearchContextLines + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("超出 context_lines 上限应返回 ErrInvalidRequest: %v", err)
	}
	if _, err := service.SearchWithOptions(context.Background(), "regex", ".", "todo", SearchOptions{Glob: "["}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("非法 glob 应返回 ErrInvalidRequest: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.SearchWithMode(cancelled, "regex", ".", "todo", SearchModeRegex); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消搜索应返回 context.Canceled: %v", err)
	}
}

func TestParseRemoteSearchRecordKeepsMatchAndContextLinesBounded(t *testing.T) {
	match, ok := parseRemoteSearchRecord("/srv/project/main.go:12:func Run() {}", "/srv/project")
	if !ok || match.Path != "main.go" || match.Line != 12 || match.Text != "func Run() {}" || !match.IsMatch {
		t.Fatalf("远程命中记录解析错误: %#v ok=%v", match, ok)
	}
	contextLine, ok := parseRemoteSearchRecord("/srv/project/main.go-11-package main", "/srv/project")
	if !ok || contextLine.Path != "main.go" || contextLine.Line != 11 || contextLine.Text != "package main" || contextLine.IsMatch {
		t.Fatalf("远程上下文记录解析错误: %#v ok=%v", contextLine, ok)
	}
	if _, ok := parseRemoteSearchRecord("/outside/main.go:1:leak", "/srv/project"); ok {
		t.Fatal("工作区外的远程搜索记录不应被接受")
	}
}

func TestCaptureLocalBaselineRecordsGitStatusWithoutContents(t *testing.T) {
	root := t.TempDir()
	runGitTestCommand(t, root, "init", "-q")
	runGitTestCommand(t, root, "config", "user.email", "abot@example.invalid")
	runGitTestCommand(t, root, "config", "user.name", "Abot Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "add", "tracked.txt")
	runGitTestCommand(t, root, "commit", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new file.txt"), []byte("untracked secret-like content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "baseline-git", Name: "baseline", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	first, err := service.CaptureBaseline(context.Background(), "baseline-git")
	if err != nil {
		t.Fatalf("捕获 Git baseline 失败: %v", err)
	}
	if first.RepositoryType != "git" || !first.StatusKnown || first.Truncated || len(first.ChangedPaths) != 2 || first.StatusDigest == "" {
		t.Fatalf("Git baseline 元数据不正确: %#v", first)
	}
	if first.ChangedPaths[0].Path != "new file.txt" || first.ChangedPaths[0].Status != "??" || first.ChangedPaths[1].Path != "tracked.txt" || first.ChangedPaths[1].Status != " M" {
		t.Fatalf("Git changed paths 未稳定排序或状态丢失: %#v", first.ChangedPaths)
	}
	if first.HeadRevision == "" || strings.Contains(first.StatusDigest, "after") || strings.Contains(first.StatusDigest, "untracked secret") {
		t.Fatalf("baseline 不应包含文件正文: %#v", first)
	}
	second, err := service.CaptureBaseline(context.Background(), "baseline-git")
	if err != nil {
		t.Fatal(err)
	}
	if first.StatusDigest != second.StatusDigest || len(second.ChangedPaths) != len(first.ChangedPaths) {
		t.Fatalf("相同工作区 baseline digest 不稳定: first=%#v second=%#v", first, second)
	}
}

func TestCaptureBaselineForNonGitDirectoryIsConservative(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "baseline-dir", Name: "baseline", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	baseline, err := service.CaptureBaseline(context.Background(), "baseline-dir")
	if err != nil {
		t.Fatalf("捕获非 Git baseline 失败: %v", err)
	}
	if baseline.RepositoryType != "directory" || baseline.StatusKnown || len(baseline.ChangedPaths) != 0 || baseline.StatusDigest == "" {
		t.Fatalf("非 Git baseline 应保守标记状态未知: %#v", baseline)
	}
}

func TestCaptureBaselineBoundsLargeGitStatusConservatively(t *testing.T) {
	root := t.TempDir()
	runGitTestCommand(t, root, "init", "-q")
	runGitTestCommand(t, root, "config", "user.email", "abot@example.invalid")
	runGitTestCommand(t, root, "config", "user.name", "Abot Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "add", "tracked.txt")
	runGitTestCommand(t, root, "commit", "-qm", "initial")
	for index := 0; index < maxBaselinePaths+25; index++ {
		name := filepath.Join(root, "untracked-"+strconv.Itoa(index)+".txt")
		if err := os.WriteFile(name, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "baseline-large", Name: "baseline", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	baseline, err := service.CaptureBaseline(context.Background(), "baseline-large")
	if err != nil {
		t.Fatalf("大状态 Git baseline 不应因有界输出失败: %v", err)
	}
	if baseline.StatusKnown || !baseline.Truncated || len(baseline.ChangedPaths) != maxBaselinePaths {
		t.Fatalf("大状态应保留有界路径并标记未知/截断: %#v", baseline)
	}
}

func runGitTestCommand(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v 失败: %v, output=%s", args, err, output)
	}
}

func TestApprovedCommandNotifiesOperationObserverAfterCommit(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "observer", Name: "observer", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	observed := make(chan Operation, 1)
	service.SetOperationObserver(func(_ context.Context, operation Operation) {
		observed <- operation
	})
	prepared, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "observer", ConversationID: "conversation", InvocationID: "invocation", ToolCallID: "tool", Type: OperationCommand, Command: "printf observer-ok", CWD: ".", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := service.ApproveOperation(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-observed:
		if notification.ID != completed.ID || notification.Status != OperationCompleted || notification.Result != "observer-ok" || notification.InvocationID != "invocation" {
			t.Fatalf("observer 应收到已提交的完整命令结果: %#v", notification)
		}
		if notification.ExitCode == nil || *notification.ExitCode != 0 || notification.OutputDigest == "" || notification.DurationMS < 0 || notification.TimedOut || notification.Unknown {
			t.Fatalf("observer 应收到可审计命令元数据: %#v", notification)
		}
		if notification.CommandRunID != prepared.CommandRunID || notification.CommandOutcome != string(CommandOutcomeSuccess) {
			t.Fatalf("operation 应绑定成功 CommandRun: %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("批准命令后未收到 operation observer 通知")
	}
	run, err := service.GetCommandRun(context.Background(), prepared.CommandRunID)
	if err != nil || run.Status != CommandRunExited || run.Outcome != CommandOutcomeSuccess || run.Revision < 4 || run.StdoutBytes != int64(len("observer-ok")) {
		t.Fatalf("CommandRun 终态/计数不正确: %#v err=%v", run, err)
	}
}

func TestTTYSpecIsExplicitAndBounded(t *testing.T) {
	spec := &TTYSpec{Enabled: true}
	if err := spec.Validate(); err != nil {
		t.Fatalf("默认 TTY spec 不应被拒绝: %v", err)
	}
	if spec.Term != defaultTTYTerm || spec.Rows != defaultTTYRows || spec.Cols != defaultTTYCols {
		t.Fatalf("TTY 默认值不正确: %#v", spec)
	}
	for _, invalid := range []*TTYSpec{
		{Enabled: false},
		{Enabled: true, Rows: maxTTYDimension + 1, Cols: defaultTTYCols},
		{Enabled: true, Rows: defaultTTYRows, Cols: maxTTYDimension + 1},
		{Enabled: true, Term: "xterm\nunsafe"},
	} {
		if err := invalid.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("无效 TTY spec 应返回 ErrInvalidRequest: spec=%#v err=%v", invalid, err)
		}
	}
}

func TestLocalPTYCommandSupportsInputResizeAndTerminalChunks(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "pty", Name: "pty", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{
		WorkspaceID: "pty", InvocationID: "inv-pty", Type: OperationCommand,
		Command: "read line; printf 'got:%s\\n' \"$line\"", Timeout: 5,
		TTY: &TTYSpec{Enabled: true, Rows: 30, Cols: 100},
	})
	if err != nil {
		t.Fatalf("创建 PTY 操作失败: %v", err)
	}
	if operation.TTY == nil || operation.TTY.Rows != 30 || operation.TTY.Cols != 100 {
		t.Fatalf("操作未保存不可变 TTY spec: %#v", operation.TTY)
	}
	run, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || run.Mode != "pty" || run.TTY == nil || run.Capabilities == nil || run.Capabilities.PTY.State != ExecutionCapabilitySupported {
		t.Fatalf("CommandRun PTY admission 快照不正确: %#v err=%v", run, err)
	}

	resultCh := make(chan struct {
		operation Operation
		err       error
	}, 1)
	go func() {
		approved, approveErr := service.ApproveOperation(context.Background(), operation.ID)
		resultCh <- struct {
			operation Operation
			err       error
		}{approved, approveErr}
	}()

	deadline := time.Now().Add(2 * time.Second)
	var token string
	for token == "" {
		if time.Now().After(deadline) {
			t.Fatal("PTY 命令未进入可 attach 状态")
		}
		if current, getErr := service.GetCommandRun(context.Background(), operation.CommandRunID); getErr == nil && current.Status == CommandRunRunning {
			if resizeErr := service.ResizeCommandPTY(context.Background(), operation.CommandRunID, 40, 120); resizeErr != nil {
				t.Fatalf("PTY resize 失败: %v", resizeErr)
			}
			var acquireErr error
			token, acquireErr = service.AcquireCommandPTYWriter(context.Background(), operation.CommandRunID, "")
			if acquireErr != nil {
				t.Fatalf("获取 PTY writer lease 失败: %v", acquireErr)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := service.AcquireCommandPTYWriter(context.Background(), operation.CommandRunID, "different"); !errors.Is(err, ErrPTYWriterHeld) {
		t.Fatalf("第二个 writer 应被拒绝: %v", err)
	}
	if _, err := service.WriteCommandPTY(context.Background(), operation.CommandRunID, token, []byte("hello\n")); err != nil {
		t.Fatalf("PTY stdin 写入失败: %v", err)
	}
	if _, err := service.WriteCommandPTY(context.Background(), operation.CommandRunID, token, make([]byte, maxPTYInputBytes+1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("超限 stdin 应被拒绝: %v", err)
	}
	result := <-resultCh
	if result.err != nil || result.operation.Status != OperationCompleted {
		t.Fatalf("PTY 操作未成功完成: %#v err=%v", result.operation, result.err)
	}
	finalRun, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || finalRun.Status != CommandRunExited || finalRun.Outcome != CommandOutcomeSuccess {
		t.Fatalf("PTY CommandRun 终态不正确: %#v err=%v", finalRun, err)
	}
	page, err := service.ListCommandOutput(context.Background(), operation.CommandRunID, 0, 20)
	if err != nil || len(page.Chunks) == 0 {
		t.Fatalf("PTY terminal chunk 未持久化: %#v err=%v", page, err)
	}
	for _, chunk := range page.Chunks {
		if chunk.Stream != "terminal" {
			t.Fatalf("PTY 输出必须使用 terminal stream: %#v", chunk)
		}
	}
	if !strings.Contains(result.operation.Result, "got:hello") {
		t.Fatalf("PTY 结果未包含交互输入回显: %q", result.operation.Result)
	}
}

func TestCommandRunNonzeroIsCompletedAttemptAndNeverReruns(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "nonzero", Name: "非零", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "nonzero", InvocationID: "inv-nonzero", Type: OperationCommand, Command: "printf failure >&2; exit 3", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	completed, approveErr := service.ApproveOperation(context.Background(), operation.ID)
	if approveErr != nil || completed.Status != OperationCompleted || completed.CommandOutcome != string(CommandOutcomeNonzero) || completed.ExitCode == nil || *completed.ExitCode != 3 {
		t.Fatalf("非零退出应是已完成尝试且保留退出码: %#v err=%v", completed, approveErr)
	}
	run, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || run.Status != CommandRunExited || run.Outcome != CommandOutcomeNonzero || run.ExitCode == nil || *run.ExitCode != 3 {
		t.Fatalf("非零 CommandRun 终态不正确: %#v err=%v", run, err)
	}
	retried, retryErr := service.ApproveOperation(context.Background(), operation.ID)
	if retryErr != nil || retried.Status != OperationCompleted {
		t.Fatalf("重复批准应读取既有终态而不是重跑: %#v err=%v", retried, retryErr)
	}
}

func TestArtifactWriterProjectsCommandLogAndDiffRefs(t *testing.T) {
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	var requests []artifact.PutRequest
	service.SetArtifactWriter(func(_ context.Context, request artifact.PutRequest, reader io.Reader) (artifact.Artifact, error) {
		data, readErr := io.ReadAll(reader)
		if readErr != nil {
			return artifact.Artifact{}, readErr
		}
		requests = append(requests, request)
		return artifact.Artifact{ID: "artifact-" + strconv.Itoa(len(requests)), Version: 2, UserID: request.UserID, ConversationID: request.ConversationID, InvocationID: request.InvocationID, Kind: request.Kind, Name: request.Name, MIMEType: request.MIMEType, Size: int64(len(data)), Digest: digestText(string(data)), Status: artifact.StatusReady, StorageKey: "sha256/test", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, nil
	})
	root := t.TempDir()
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "artifact-projection", Name: "Artifact", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "before.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diffOperation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "artifact-projection", UserID: "user-1", ConversationID: "conversation-1", InvocationID: "inv-diff", Type: OperationWriteFile, Path: "before.txt", Content: "after\n"})
	if err != nil {
		t.Fatalf("创建 diff operation 失败: %v", err)
	}
	if diffOperation.DiffArtifact == nil || diffOperation.DiffArtifact.ID != "artifact-1" || len(requests) != 1 || requests[0].Kind != artifact.KindDiff {
		t.Fatalf("diff 未生成 Artifact ref: operation=%#v requests=%#v", diffOperation, requests)
	}
	commandOperation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "artifact-projection", UserID: "user-1", ConversationID: "conversation-1", InvocationID: "inv-command", Type: OperationCommand, Command: "printf 'command output'", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	completed, approveErr := service.ApproveOperation(context.Background(), commandOperation.ID)
	if approveErr != nil || completed.OutputArtifact == nil || completed.OutputArtifact.ID != "artifact-2" {
		t.Fatalf("command log 未生成 Artifact ref: operation=%#v err=%v", completed, approveErr)
	}
	run, runErr := service.GetCommandRun(context.Background(), commandOperation.CommandRunID)
	if runErr != nil || run.OutputArtifact == nil || run.OutputArtifact.ID != completed.OutputArtifact.ID || run.ArtifactError != "" {
		t.Fatalf("CommandRun Artifact ref 未持久化: run=%#v err=%v", run, runErr)
	}
	if len(requests) != 2 || requests[1].Kind != artifact.KindCommandLog || requests[1].ProducerID != commandOperation.CommandRunID {
		t.Fatalf("command log projection request 不正确: %#v", requests)
	}
}

func TestCommandRunOutputIsPersistedAndPaged(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "output-page", Name: "输出分页", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "output-page", InvocationID: "inv-output-page", Type: OperationCommand, Command: "printf one; printf two >&2; printf three", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveOperation(context.Background(), operation.ID); err != nil {
		t.Fatal(err)
	}
	first, err := service.ListCommandOutput(context.Background(), operation.CommandRunID, 0, 1)
	if err != nil || len(first.Chunks) != 1 || !first.HasMore || first.NextAfter == 0 {
		t.Fatalf("首个输出页不正确: %#v err=%v", first, err)
	}
	second, err := service.ListCommandOutput(context.Background(), operation.CommandRunID, first.NextAfter, maxCommandOutputPageLimit)
	if err != nil || len(second.Chunks) < 1 || second.HasMore || second.NextAfter <= first.NextAfter {
		t.Fatalf("后续输出页不正确: %#v err=%v", second, err)
	}
	all := append(append([]CommandOutputChunk{}, first.Chunks...), second.Chunks...)
	joined := make(map[string]string)
	for index, chunk := range all {
		if chunk.Sequence != uint64(index+1) || chunk.ByteLength != int64(len([]byte(chunk.Data))) || chunk.CapturedAt.IsZero() {
			t.Fatalf("chunk sequence/metadata 不正确: %#v", all)
		}
		joined[chunk.Stream] += chunk.Data
	}
	if joined["stdout"] != "onethree" || joined["stderr"] != "two" {
		t.Fatalf("stdout/stderr 输出不正确: %#v", joined)
	}
}

func TestCommandRunOutputDownloadPreservesTextAndNDJSON(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "output-download", Name: "输出下载", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "output-download", Type: OperationCommand, Command: "printf one; printf two; printf three", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveOperation(context.Background(), operation.ID); err != nil {
		t.Fatal(err)
	}

	var text bytes.Buffer
	textResult, err := service.DownloadCommandOutput(context.Background(), operation.CommandRunID, CommandOutputDownloadText, &text)
	if err != nil || textResult.Chunks == 0 || text.String() != "onetwothree" || textResult.Bytes != int64(len(text.String())) || textResult.Truncated {
		t.Fatalf("文本日志下载不正确: result=%#v output=%q err=%v", textResult, text.String(), err)
	}

	var ndjson bytes.Buffer
	ndjsonResult, err := service.DownloadCommandOutput(context.Background(), operation.CommandRunID, CommandOutputDownloadNDJSON, &ndjson)
	if err != nil || ndjsonResult.Chunks != textResult.Chunks || ndjsonResult.Bytes != int64(ndjson.Len()) {
		t.Fatalf("NDJSON 日志下载不正确: result=%#v bytes=%d err=%v", ndjsonResult, ndjson.Len(), err)
	}
	var joined strings.Builder
	decoder := json.NewDecoder(&ndjson)
	var previous uint64
	for index := 0; index < ndjsonResult.Chunks; index++ {
		var chunk CommandOutputChunk
		if err := decoder.Decode(&chunk); err != nil {
			t.Fatalf("解析 NDJSON chunk 失败: %v", err)
		}
		if chunk.CommandRunID != operation.CommandRunID || chunk.Stream != "stdout" || chunk.Sequence <= previous {
			t.Fatalf("NDJSON chunk 元数据不正确: %#v", chunk)
		}
		previous = chunk.Sequence
		joined.WriteString(chunk.Data)
	}
	if joined.String() != "onetwothree" {
		t.Fatalf("NDJSON chunk 数据不完整: %q", joined.String())
	}
	if decoder.More() {
		t.Fatal("NDJSON 下载不应有额外记录")
	}
}

func TestCommandRunOutputDownloadValidatesFormatAndWriter(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "output-download-validation", Name: "输出下载校验", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "output-download-validation", Type: OperationCommand, Command: "printf ok", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveOperation(context.Background(), operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DownloadCommandOutput(context.Background(), operation.CommandRunID, CommandOutputDownloadFormat("xml"), &bytes.Buffer{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("非法下载格式应返回 ErrInvalidRequest: %v", err)
	}
	if _, err := service.DownloadCommandOutput(context.Background(), operation.CommandRunID, CommandOutputDownloadText, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("空下载目标应返回 ErrInvalidRequest: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.DownloadCommandOutput(ctx, operation.CommandRunID, CommandOutputDownloadText, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消下载上下文应返回 context.Canceled: %v", err)
	}
}

func TestCommandRunOutputDownloadCrossesPageBoundary(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "output-download-pages", Name: "输出下载分页", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "output-download-pages", Type: OperationCommand, Command: "true", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveOperation(context.Background(), operation.ID); err != nil {
		t.Fatal(err)
	}
	capturedAt := time.Now().UTC()
	for sequence := uint64(1); sequence <= 401; sequence++ {
		if err := repository.CreateCommandOutputChunk(context.Background(), CommandOutputChunk{
			CommandRunID: operation.CommandRunID,
			Sequence:     sequence,
			Stream:       "stdout",
			Offset:       int64(sequence - 1),
			Data:         "x",
			ByteLength:   1,
			CapturedAt:   capturedAt,
		}); err != nil {
			t.Fatalf("写入分页 chunk %d 失败: %v", sequence, err)
		}
	}
	var output bytes.Buffer
	result, err := service.DownloadCommandOutput(context.Background(), operation.CommandRunID, CommandOutputDownloadText, &output)
	if err != nil || result.Chunks != 401 || output.Len() != 401 || strings.Repeat("x", 401) != output.String() {
		t.Fatalf("跨页日志下载不正确: result=%#v bytes=%d err=%v", result, output.Len(), err)
	}
}

func TestCommandRunOutputPersistenceFailureBlocksRetry(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "output-failure", Name: "输出持久化失败", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{
		WorkspaceID:  "output-failure",
		InvocationID: "inv-output-failure",
		Type:         OperationCommand,
		Command:      "printf attempt >> attempts; printf output",
		Timeout:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	repository.commandOutputFailure = errors.New("output store unavailable")
	completed, approveErr := service.ApproveOperation(context.Background(), operation.ID)
	if approveErr == nil || completed.Status != OperationUnknown || !completed.Unknown || completed.CommandOutcome != string(CommandOutcomeUnknown) {
		t.Fatalf("输出持久化失败应保守标为 unknown: %#v err=%v", completed, approveErr)
	}
	run, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || run.Status != CommandRunUnknown || run.Outcome != CommandOutcomeUnknown {
		t.Fatalf("输出持久化失败的 CommandRun 终态不正确: %#v err=%v", run, err)
	}
	first, err := os.ReadFile(filepath.Join(root, "attempts"))
	if err != nil || string(first) != "attempt" {
		t.Fatalf("命令应已经执行一次: data=%q err=%v", first, err)
	}
	retried, retryErr := service.ApproveOperation(context.Background(), operation.ID)
	if retryErr != nil || retried.Status != OperationUnknown {
		t.Fatalf("unknown 结果不得触发重跑: %#v err=%v", retried, retryErr)
	}
	second, err := os.ReadFile(filepath.Join(root, "attempts"))
	if err != nil || string(second) != string(first) {
		t.Fatalf("重复批准不应改变命令副作用: first=%q second=%q err=%v", first, second, err)
	}
}

func TestCancelQueuedCommandRunIsIdempotentAndPreventsApproval(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "cancel-queued", Name: "取消排队命令", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "cancel-queued", Type: OperationCommand, Command: "touch should-not-run", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.CancelCommandRun(context.Background(), operation.CommandRunID, "用户取消排队命令")
	if err != nil || run.Status != CommandRunCancelled || run.Outcome != CommandOutcomeCancelled {
		t.Fatalf("排队命令取消结果不正确: %#v err=%v", run, err)
	}
	if _, err := service.CancelCommandRun(context.Background(), operation.CommandRunID, "重复取消"); err != nil {
		t.Fatalf("重复取消终态应幂等: %v", err)
	}
	approved, approveErr := service.ApproveOperation(context.Background(), operation.ID)
	if approveErr != nil || approved.Status != OperationCancelled {
		t.Fatalf("批准已取消排队命令不得执行: %#v err=%v", approved, approveErr)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
		t.Fatalf("排队取消后不应产生命令副作用: %v", err)
	}
}

func TestCancelActiveCommandRunStopsProcessAndDoesNotRerun(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "cancel-active", Name: "取消活动命令", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "cancel-active", Type: OperationCommand, Command: "printf attempt >> attempts; sleep 30", Timeout: 60})
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan struct {
		operation Operation
		err       error
	}, 1)
	go func() {
		approved, approveErr := service.ApproveOperation(context.Background(), operation.ID)
		resultCh <- struct {
			operation Operation
			err       error
		}{approved, approveErr}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if attempts, readErr := os.ReadFile(filepath.Join(root, "attempts")); readErr == nil && string(attempts) == "attempt" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("活动命令未产生预期副作用")
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, cancelErr := service.CancelCommandRun(context.Background(), operation.CommandRunID, "用户取消活动命令")
	if cancelErr != nil || run.Status != CommandRunCancelled || run.Outcome != CommandOutcomeCancelled {
		t.Fatalf("活动命令取消结果不正确: %#v err=%v", run, cancelErr)
	}
	select {
	case result := <-resultCh:
		if result.err == nil || result.operation.Status != OperationCancelled {
			t.Fatalf("批准线程应返回取消终态: %#v err=%v", result.operation, result.err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("取消活动命令后批准线程未返回")
	}
	first, err := os.ReadFile(filepath.Join(root, "attempts"))
	if err != nil || string(first) != "attempt" {
		t.Fatalf("活动命令应只执行一次: data=%q err=%v", first, err)
	}
	retried, retryErr := service.ApproveOperation(context.Background(), operation.ID)
	if retryErr != nil || retried.Status != OperationCancelled {
		t.Fatalf("重复批准取消命令不得重跑: %#v err=%v", retried, retryErr)
	}
	second, err := os.ReadFile(filepath.Join(root, "attempts"))
	if err != nil || string(second) != string(first) {
		t.Fatalf("重复批准不应改变取消命令副作用: first=%q second=%q err=%v", first, second, err)
	}
}

func TestCommandRunTimeoutPersistsTerminalFailureAndNeverReruns(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "timeout-run", Name: "超时检查点", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "timeout-run", InvocationID: "inv-timeout-run", Type: OperationCommand, Command: "printf attempt >> attempts; sleep 30 & wait", Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	completed, approveErr := service.ApproveOperation(context.Background(), operation.ID)
	if approveErr == nil || completed.Status != OperationFailed || completed.CommandOutcome != string(CommandOutcomeTimedOut) || !completed.TimedOut {
		t.Fatalf("超时应保存为已结束的失败尝试: %#v err=%v", completed, approveErr)
	}
	run, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || run.Status != CommandRunTerminated || run.Outcome != CommandOutcomeTimedOut || run.FinishedAt == nil {
		t.Fatalf("超时 CommandRun 终态不正确: %#v err=%v", run, err)
	}
	before, err := os.ReadFile(filepath.Join(root, "attempts"))
	if err != nil || string(before) != "attempt" {
		t.Fatalf("首次命令应只执行一次: data=%q err=%v", before, err)
	}
	retried, retryErr := service.ApproveOperation(context.Background(), operation.ID)
	if retryErr != nil || retried.Status != OperationFailed {
		t.Fatalf("重复批准超时命令不得重跑: %#v err=%v", retried, retryErr)
	}
	after, err := os.ReadFile(filepath.Join(root, "attempts"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("重复批准改变了命令副作用: before=%q after=%q err=%v", before, after, err)
	}
}

func TestCommandRunStartFailurePersistsWithoutExecuting(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "gone")
	if err := os.Mkdir(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "start-failure", Name: "启动失败", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "start-failure", InvocationID: "inv-start-failure", Type: OperationCommand, Command: "touch must-not-run", CWD: "gone", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	completed, approveErr := service.ApproveOperation(context.Background(), operation.ID)
	if approveErr == nil || completed.Status != OperationFailed || completed.CommandOutcome != string(CommandOutcomeRuntimeError) {
		t.Fatalf("启动失败应保存独立终态: %#v err=%v", completed, approveErr)
	}
	run, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || run.Status != CommandRunStartFailed || run.Outcome != CommandOutcomeRuntimeError {
		t.Fatalf("启动失败 CommandRun 终态不正确: %#v err=%v", run, err)
	}
	if _, err := os.Stat(filepath.Join(root, "must-not-run")); !os.IsNotExist(err) {
		t.Fatalf("启动失败不得产生命令副作用: err=%v", err)
	}
}

func TestCommandRunCancellationPersistsTerminalFailureAndNeverReruns(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "cancel-run", Name: "取消检查点", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "cancel-run", InvocationID: "inv-cancel-run", Type: OperationCommand, Command: "printf attempt >> attempts; sleep 30", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type approvalResult struct {
		operation Operation
		err       error
	}
	resultCh := make(chan approvalResult, 1)
	go func() {
		approved, approveErr := service.ApproveOperation(ctx, operation.ID)
		resultCh <- approvalResult{operation: approved, err: approveErr}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		attempts, _ := os.ReadFile(filepath.Join(root, "attempts"))
		if string(attempts) == "attempt" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("命令未产生预期副作用: attempts=%q", attempts)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	var result approvalResult
	select {
	case result = <-resultCh:
	case <-time.After(4 * time.Second):
		t.Fatal("取消命令未在预期时间内返回")
	}
	if result.err == nil || result.operation.Status != OperationCancelled || result.operation.CommandOutcome != string(CommandOutcomeCancelled) {
		t.Fatalf("取消应保存为已结束的失败尝试: %#v err=%v", result.operation, result.err)
	}
	run, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || run.Status != CommandRunCancelled || run.Outcome != CommandOutcomeCancelled || run.FinishedAt == nil {
		t.Fatalf("取消 CommandRun 终态不正确: %#v err=%v", run, err)
	}
	before, err := os.ReadFile(filepath.Join(root, "attempts"))
	if err != nil || string(before) != "attempt" {
		t.Fatalf("首次命令应只执行一次: data=%q err=%v", before, err)
	}
	retried, retryErr := service.ApproveOperation(context.Background(), operation.ID)
	if retryErr != nil || retried.Status != OperationCancelled {
		t.Fatalf("重复批准取消命令不得重跑: %#v err=%v", retried, retryErr)
	}
	after, err := os.ReadFile(filepath.Join(root, "attempts"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("重复批准改变了命令副作用: before=%q after=%q err=%v", before, after, err)
	}
}

func TestCommandRunUnknownBlocksSideEffectRetry(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "unknown-run", Name: "未知", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "unknown-run", InvocationID: "inv-unknown", Type: OperationCommand, Command: "touch should-not-run", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = CommandRunRunning
	run.Revision++
	if _, err := repository.UpdateCommandRun(context.Background(), run, run.Revision-1); err != nil {
		t.Fatal(err)
	}
	blocked, blockErr := service.ApproveOperation(context.Background(), operation.ID)
	if !errors.Is(blockErr, ErrOperationState) || blocked.Status != OperationUnknown {
		t.Fatalf("运行中 checkpoint 必须转 unknown 并阻止重跑: %#v err=%v", blocked, blockErr)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
		t.Fatalf("unknown checkpoint 不得重新执行命令: err=%v", err)
	}
	stored, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || stored.Status != CommandRunUnknown || stored.Outcome != CommandOutcomeUnknown {
		t.Fatalf("unknown checkpoint 未持久化终态: %#v err=%v", stored, err)
	}
}

func TestReconcileCommandRunsMarksRunningUnknownAndOperation(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "reconcile", Name: "恢复", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "reconcile", InvocationID: "inv-reconcile", Type: OperationCommand, Command: "printf never", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := repository.GetCommandRun(context.Background(), operation.CommandRunID)
	stored.Status = CommandRunRunning
	stored.Revision++
	if _, err := repository.UpdateCommandRun(context.Background(), stored, stored.Revision-1); err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileCommandRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	reconciled, _ := service.GetCommandRun(context.Background(), operation.CommandRunID)
	updated, _ := repository.GetOperation(context.Background(), operation.ID)
	if reconciled.Status != CommandRunUnknown || updated.Status != OperationUnknown || updated.CommandOutcome != string(CommandOutcomeUnknown) {
		t.Fatalf("服务恢复应同时关闭 command run 和 operation: run=%#v operation=%#v", reconciled, updated)
	}
}

func TestCommandOutputRetentionPurgesChunksAndKeepsMetadata(t *testing.T) {
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	finished := now.Add(-2 * time.Hour)
	retainedUntil := now.Add(-time.Hour)
	exitCode := 0
	run := CommandRun{
		ID: "retention-run", WorkspaceID: "retention-workspace", OperationID: "retention-operation",
		Executor: "local", Mode: "shell", Status: CommandRunExited, Outcome: CommandOutcomeSuccess,
		ExitCode: &exitCode, StoredBytes: 3, OutputDigest: digestText("log"),
		OutputRetentionState: CommandOutputRetentionRetained, OutputRetainedUntil: &retainedUntil,
		FinishedAt: &finished, Revision: 1, QueuedAt: finished, CreatedAt: finished, UpdatedAt: finished,
	}
	if err := repository.CreateCommandRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	chunk := CommandOutputChunk{CommandRunID: run.ID, Sequence: 1, Stream: "stdout", Offset: 0, Data: "log", ByteLength: 3, CapturedAt: finished}
	if err := repository.CreateCommandOutputChunk(context.Background(), chunk); err != nil {
		t.Fatal(err)
	}
	purge, err := service.PurgeExpiredCommandOutput(context.Background(), now)
	if err != nil || purge.Scanned != 1 || purge.Purged != 1 || purge.Bytes != 3 {
		t.Fatalf("过期日志清理结果不正确: %#v err=%v", purge, err)
	}
	stored, err := service.GetCommandRun(context.Background(), run.ID)
	if err != nil || stored.OutputRetentionState != CommandOutputRetentionPurged || stored.StoredBytes != 3 || stored.OutputDigest != run.OutputDigest || stored.ExitCode == nil || *stored.ExitCode != 0 || stored.OutputPurgedAt == nil {
		t.Fatalf("清理后 CommandRun 元数据未保留: %#v err=%v", stored, err)
	}
	page, err := service.ListCommandOutput(context.Background(), run.ID, 0, 10)
	if err != nil || page.RetentionState != CommandOutputRetentionPurged || len(page.Chunks) != 0 {
		t.Fatalf("清理后分页应明确表示正文已过期: %#v err=%v", page, err)
	}
	var output bytes.Buffer
	if _, err := service.DownloadCommandOutput(context.Background(), run.ID, CommandOutputDownloadText, &output); !errors.Is(err, ErrCommandOutputExpired) {
		t.Fatalf("清理后下载应返回 ErrCommandOutputExpired: %v", err)
	}
	leftovers, err := repository.ListCommandOutputChunks(context.Background(), run.ID, 0, 10)
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("清理后不应遗留正文 chunk: %#v err=%v", leftovers, err)
	}
	repeated, err := service.PurgeExpiredCommandOutput(context.Background(), now.Add(time.Hour))
	if err != nil || repeated.Purged != 0 {
		t.Fatalf("重复清理应幂等: %#v err=%v", repeated, err)
	}
}

func TestCommandRunTerminalCheckpointCapturesRetentionDeadline(t *testing.T) {
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetCommandOutputRetention(CommandOutputRetentionPolicy{MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	queued := CommandRun{ID: "retention-deadline", WorkspaceID: "workspace", OperationID: "operation", Executor: "local", Mode: "shell", Status: CommandRunQueued, Revision: 1, QueuedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repository.CreateCommandRun(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	started, err := service.transitionCommandRun(context.Background(), queued, CommandRunStarting, "", CommandResult{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	running, err := service.transitionCommandRun(context.Background(), started, CommandRunRunning, "", CommandResult{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := service.transitionCommandRun(context.Background(), running, CommandRunExited, CommandOutcomeSuccess, CommandResult{Output: "ok", ExitCode: 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if completed.OutputRetentionState != CommandOutputRetentionRetained || completed.OutputRetainedUntil == nil || !completed.OutputRetainedUntil.After(completed.FinishedAt.Add(59*time.Minute)) {
		t.Fatalf("终态检查点未捕获稳定保留期限: %#v", completed)
	}
}

func TestCommandRunCapturesConservativeExecutionCapabilities(t *testing.T) {
	root := t.TempDir()
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "capabilities", Name: "能力披露", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "capabilities", InvocationID: "inv-capabilities", Type: OperationCommand, Command: "printf ok", CWD: ".", Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Capabilities == nil {
		t.Fatal("新建命令必须保存执行能力快照")
	}
	capabilities := run.Capabilities
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("能力快照无效: %v", err)
	}
	if capabilities.ProfileID != "host-process-l0" || capabilities.IsolationLevel != "l0_host_process" || capabilities.Executor != "local" {
		t.Fatalf("本地命令能力 profile 不正确: %#v", capabilities)
	}
	if capabilities.Filesystem.State != ExecutionCapabilityPartial || capabilities.Network.State != ExecutionCapabilityUnsupported || capabilities.PTY.State != ExecutionCapabilityUnsupported || capabilities.Reattach.State != ExecutionCapabilityUnsupported {
		t.Fatalf("本地命令未保守披露能力差异: %#v", capabilities)
	}
	if strings.Contains(strings.ToLower(capabilities.Credentials.Detail), "secret broker") == false {
		t.Fatalf("凭据能力应明确没有 secret broker: %#v", capabilities.Credentials)
	}
	snapshot := *capabilities
	if _, err := service.ApproveOperation(context.Background(), operation.ID); err != nil {
		t.Fatal(err)
	}
	finished, err := service.GetCommandRun(context.Background(), operation.CommandRunID)
	if err != nil || finished.Capabilities == nil || *finished.Capabilities != snapshot {
		t.Fatalf("命令状态推进不能改写 admission 能力快照: %#v err=%v", finished.Capabilities, err)
	}

	ssh := DefaultCommandExecutionCapabilities(Workspace{Type: TypeSSH})
	if ssh == nil || ssh.ProfileID != "ssh-host-process" || ssh.IsolationLevel != "ssh_account" || ssh.Filesystem.State != ExecutionCapabilityPartial || ssh.ProcessControl.State != ExecutionCapabilityPartial {
		t.Fatalf("SSH 能力 profile 不正确: %#v", ssh)
	}
	unknown := DefaultCommandExecutionCapabilities(Workspace{Type: Type("future")})
	if unknown == nil || unknown.IsolationLevel != "unknown" || unknown.Network.State != ExecutionCapabilityUnknown || unknown.Reattach.State != ExecutionCapabilityUnknown {
		t.Fatalf("未知执行器必须 fail-closed: %#v", unknown)
	}
}

func TestExecutionCapabilityValidationRejectsMalformedSnapshot(t *testing.T) {
	valid := DefaultCommandExecutionCapabilities(Workspace{Type: TypeLocal})
	valid.Filesystem.State = ExecutionCapabilityState("maybe")
	if err := valid.Validate(); !errors.Is(err, ErrOperationState) {
		t.Fatalf("非法 capability state 应拒绝: %v", err)
	}
	valid = DefaultCommandExecutionCapabilities(Workspace{Type: TypeLocal})
	valid.Network.Detail = strings.Repeat("x", 1001)
	if err := valid.Validate(); !errors.Is(err, ErrOperationState) {
		t.Fatalf("过长 capability detail 应拒绝: %v", err)
	}
}

func TestReadManyEnforcesBatchLimitsWithoutDroppingEarlierResults(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "batch", Name: "batch", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", maxFileBytes)
	paths := make([]string, 0, 5)
	for index := 0; index < 5; index++ {
		name := filepath.Join(root, "file-"+string(rune('a'+index))+".txt")
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, filepath.Base(name))
	}
	result, err := service.ReadMany(context.Background(), "batch", paths)
	if err != nil || len(result.Files) != 5 || !result.Truncated || result.TotalBytes != maxReadManyBytes || !result.Files[4].Truncated || result.Files[4].Error != "" {
		t.Fatalf("批量读取上限结果不正确: %#v err=%v", result, err)
	}
	if result.Files[0].Bytes != maxFileBytes || result.Files[0].ContentDigest == "" || len(result.Files[0].Content) != maxFileBytes {
		t.Fatalf("批量读取首项不应被截断: %#v", result.Files[0])
	}
	tooMany := make([]string, maxReadManyFiles+1)
	if _, err := service.ReadMany(context.Background(), "batch", tooMany); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("超出批量文件数错误=%v，期望 ErrInvalidRequest", err)
	}
}

func TestLocalCommandStreamsSeparatedChunksAndKillsProcessGroup(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "stream", Name: "流式", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	chunks := make(chan CommandChunk, 16)
	ctx := WithCommandObserver(context.Background(), func(chunk CommandChunk) { chunks <- chunk })
	item, err := service.Resolve(ctx, "stream")
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeWorkspaceCommand(ctx, item, ".", "printf out; printf err >&2", 5)
	if err != nil {
		t.Fatalf("流式命令失败: %v", err)
	}
	if result.Stdout != "out" || result.Stderr != "err" || result.Output != "outerr" || result.TimedOut || result.Unknown {
		t.Fatalf("命令结果 = %#v", result)
	}
	seen := map[string]string{}
	for {
		select {
		case chunk := <-chunks:
			seen[chunk.Stream] += chunk.Data
		case <-time.After(200 * time.Millisecond):
			if seen["stdout"] == "out" && seen["stderr"] == "err" {
				return
			}
			t.Fatalf("未收到完整命令 chunk: %#v", seen)
		}
	}
}

func TestCommandChunksCarryRunSequenceAndPerStreamOffsets(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "chunk-meta", Name: "chunk", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	chunks := make(chan CommandChunk, 16)
	ctx := WithCommandRunID(WithCommandObserver(context.Background(), func(chunk CommandChunk) { chunks <- chunk }), "command-run-test")
	item, err := service.Resolve(ctx, "chunk-meta")
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeWorkspaceCommand(ctx, item, ".", "printf out; printf err >&2", 5)
	if err != nil || result.StdoutBytes != 3 || result.StderrBytes != 3 {
		t.Fatalf("命令输出计数不正确: %#v err=%v", result, err)
	}
	seen := map[string]bool{}
	sequenceSeen := map[uint64]bool{}
	for len(seen) < 2 {
		select {
		case chunk := <-chunks:
			if chunk.CommandRunID != "command-run-test" || chunk.Sequence == 0 || sequenceSeen[chunk.Sequence] || (chunk.Stream == "stdout" && chunk.Offset != 0) || (chunk.Stream == "stderr" && chunk.Offset != 0) {
				t.Fatalf("chunk 元数据不正确: %#v", chunk)
			}
			sequenceSeen[chunk.Sequence] = true
			seen[chunk.Stream] = true
		case <-time.After(time.Second):
			t.Fatalf("未收到 stdout/stderr chunk: %#v", seen)
		}
	}
}

func TestLocalCommandTimeoutDrainsAndReportsTimeout(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "timeout", Name: "超时", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	item, err := service.Resolve(context.Background(), "timeout")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := executeWorkspaceCommand(context.Background(), item, ".", "sleep 30 & wait", 1)
	if err == nil || !result.TimedOut || result.Unknown {
		t.Fatalf("超时命令结果 = %#v, err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("进程组超时回收耗时过长: %s", elapsed)
	}
}

func TestWorkspaceRequiresUserSuppliedRoot(t *testing.T) {
	service, _ := NewService(newMemoryRepository())
	_, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{
		ID: "demo", Name: "演示项目", Type: TypeLocal, Enabled: true,
	}})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("未指定项目目录时错误 = %v，期望 ErrInvalidRequest", err)
	}
}

func TestListLocalDirectoriesReturnsOnlyDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("read-only listing test"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	listing, err := service.ListLocalDirectories(context.Background(), root)
	if err != nil {
		t.Fatalf("浏览本机目录失败: %v", err)
	}
	expectedRoot, _ := filepath.EvalSymlinks(root)
	if listing.Path != expectedRoot || listing.Parent != filepath.Dir(expectedRoot) {
		t.Fatalf("目录位置不正确: %#v", listing)
	}
	if len(listing.Directories) != 1 || listing.Directories[0].Name != "src" {
		t.Fatalf("目录选择器返回了非目录或漏目录: %#v", listing.Directories)
	}
}

func TestDiscoverInstructionsUsesNestedScopeAndDigest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".abot", "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "feature"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".abot/instructions.md": "Abot project rules\n",
		"AGENTS.md":             "root rules\n",
		"src/AGENTS.md":         "src rules\n",
		"src/feature/AGENTS.md": "feature rules\n",
		"src/feature/main.go":   "package feature\n",
	}
	for relative, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(relative)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "instructions", Name: "指令测试", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	items, err := service.DiscoverInstructions(context.Background(), "instructions", "src/feature/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("指令数量=%d, items=%#v", len(items), items)
	}
	wantPaths := []string{".abot/instructions.md", "AGENTS.md", "src/AGENTS.md", "src/feature/AGENTS.md"}
	for index, want := range wantPaths {
		if items[index].Path != want || !strings.Contains(items[index].Content, "rules") || items[index].ContentDigest == "" {
			t.Fatalf("指令[%d]=%#v, want path=%s", index, items[index], want)
		}
	}
	rootOnly, err := service.DiscoverInstructions(context.Background(), "instructions", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(rootOnly) != 2 {
		t.Fatalf("根路径目标不应加载嵌套指令: %#v", rootOnly)
	}
}

func TestDiscoverInstructionsRejectsOversizedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(strings.Repeat("x", maxInstructionBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "largeinstructions", Name: "大文件", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DiscoverInstructions(context.Background(), "largeinstructions", ""); !errors.Is(err, ErrInstructionTooLarge) {
		t.Fatalf("超大指令错误=%v, 期望 ErrInstructionTooLarge", err)
	}
}

func TestApproveOperationRechecksInstructionAtWriteBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(newMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "boundary", Name: "边界", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	var checks int
	service.SetInstructionWriteValidator(func(context.Context, string) error {
		checks++
		return errors.New("instruction digest changed")
	})
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "boundary", InvocationID: "inv-boundary", Type: OperationWriteFile, Path: "notes.txt", Content: "after\n"})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := service.ApproveOperation(context.Background(), operation.ID)
	if !errors.Is(err, ErrOperationState) || approved.Status != OperationStale {
		t.Fatalf("写入边界 stale 结果=%#v err=%v", approved, err)
	}
	if checks != 1 {
		t.Fatalf("写入边界应只执行一次 validator，实际=%d", checks)
	}
	data, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil || string(data) != "before\n" {
		t.Fatalf("指令变化时不应写入文件=%q err=%v", data, err)
	}
}

func TestOperationAdmissionValidatorRunsBeforePrepareAndApproval(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository := newMemoryRepository()
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), SaveRequest{Workspace: Workspace{ID: "workflow-gate", Name: "workflow gate", Type: TypeLocal, RootPath: root, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	checks := 0
	service.SetOperationAdmissionValidator(func(context.Context, OperationRequest) error {
		checks++
		if checks > 1 {
			return errors.New("workflow phase changed")
		}
		return nil
	})
	operation, err := service.CreateOperation(context.Background(), OperationRequest{WorkspaceID: "workflow-gate", InvocationID: "inv-workflow-gate", Type: OperationWriteFile, Path: "notes.txt", Content: "after\n"})
	if err != nil {
		t.Fatalf("工作流门禁不应阻止首次准备: %v", err)
	}
	approved, err := service.ApproveOperation(context.Background(), operation.ID)
	if !errors.Is(err, ErrOperationState) || approved.Status != OperationStale {
		t.Fatalf("批准前工作流门禁应将操作置为 stale: operation=%#v err=%v", approved, err)
	}
	if checks != 2 {
		t.Fatalf("工作流门禁应在准备和批准各检查一次，实际=%d", checks)
	}
	data, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil || string(data) != "before\n" {
		t.Fatalf("工作流步骤变化时不应写入文件: data=%q err=%v", data, err)
	}
	operations, err := service.ListOperations(context.Background(), "workflow-gate")
	if err != nil || len(operations) != 1 || operations[0].Status != OperationStale {
		t.Fatalf("stale 操作应保留在账本中: operations=%#v err=%v", operations, err)
	}
}

type memoryRepository struct {
	workspaces           map[string]Workspace
	operations           map[string]Operation
	commandRuns          map[string]CommandRun
	commandOutputs       map[string]map[uint64]CommandOutputChunk
	commandOutputFailure error
	commandMu            sync.RWMutex
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{workspaces: map[string]Workspace{}, operations: map[string]Operation{}, commandRuns: map[string]CommandRun{}, commandOutputs: map[string]map[uint64]CommandOutputChunk{}}
}

func (r *memoryRepository) List(context.Context) ([]Workspace, error) {
	result := make([]Workspace, 0, len(r.workspaces))
	for _, item := range r.workspaces {
		result = append(result, item)
	}
	return result, nil
}

func (r *memoryRepository) Get(_ context.Context, id string) (Workspace, error) {
	item, ok := r.workspaces[id]
	if !ok {
		return Workspace{}, ErrNotFound
	}
	return item, nil
}

func (r *memoryRepository) Save(_ context.Context, item Workspace) error {
	r.workspaces[item.ID] = item
	return nil
}

func (r *memoryRepository) Delete(_ context.Context, id string) error {
	if _, ok := r.workspaces[id]; !ok {
		return ErrNotFound
	}
	delete(r.workspaces, id)
	return nil
}

func (r *memoryRepository) ListOperations(_ context.Context, workspaceID string) ([]Operation, error) {
	result := make([]Operation, 0, len(r.operations))
	for _, item := range r.operations {
		if workspaceID == "" || item.WorkspaceID == workspaceID {
			result = append(result, item)
		}
	}
	return result, nil
}

func (r *memoryRepository) GetOperation(_ context.Context, id string) (Operation, error) {
	item, ok := r.operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return item, nil
}

func (r *memoryRepository) SaveOperation(_ context.Context, item Operation) error {
	r.operations[item.ID] = item
	return nil
}

func (r *memoryRepository) CreateCommandRun(_ context.Context, item CommandRun) error {
	r.commandMu.Lock()
	defer r.commandMu.Unlock()
	if err := item.Validate(); err != nil {
		return err
	}
	if _, exists := r.commandRuns[item.ID]; exists {
		return ErrCommandRunConflict
	}
	r.commandRuns[item.ID] = item
	r.commandOutputs[item.ID] = map[uint64]CommandOutputChunk{}
	return nil
}

func (r *memoryRepository) GetCommandRun(_ context.Context, id string) (CommandRun, error) {
	r.commandMu.RLock()
	defer r.commandMu.RUnlock()
	item, ok := r.commandRuns[id]
	if !ok {
		return CommandRun{}, ErrNotFound
	}
	return item, nil
}

func (r *memoryRepository) ListCommandRuns(_ context.Context, workspaceID string) ([]CommandRun, error) {
	r.commandMu.RLock()
	defer r.commandMu.RUnlock()
	result := make([]CommandRun, 0, len(r.commandRuns))
	for _, item := range r.commandRuns {
		if workspaceID == "" || item.WorkspaceID == workspaceID {
			result = append(result, item)
		}
	}
	return result, nil
}

func (r *memoryRepository) UpdateCommandRun(_ context.Context, item CommandRun, expectedRevision int64) (CommandRun, error) {
	r.commandMu.Lock()
	defer r.commandMu.Unlock()
	current, ok := r.commandRuns[item.ID]
	if !ok {
		return CommandRun{}, ErrNotFound
	}
	if current.Revision != expectedRevision || item.Revision != expectedRevision+1 {
		return CommandRun{}, ErrCommandRunConflict
	}
	if !EqualCommandExecutionCapabilities(current.Capabilities, item.Capabilities) {
		return CommandRun{}, ErrCommandRunConflict
	}
	if !EqualTTYSpec(current.TTY, item.TTY) {
		return CommandRun{}, ErrCommandRunConflict
	}
	if err := item.Validate(); err != nil {
		return CommandRun{}, err
	}
	r.commandRuns[item.ID] = item
	return item, nil
}

func (r *memoryRepository) DeleteCommandRun(_ context.Context, id string) error {
	r.commandMu.Lock()
	defer r.commandMu.Unlock()
	if _, ok := r.commandRuns[id]; !ok {
		return ErrNotFound
	}
	delete(r.commandRuns, id)
	delete(r.commandOutputs, id)
	return nil
}

func (r *memoryRepository) CreateCommandOutputChunk(_ context.Context, item CommandOutputChunk) error {
	r.commandMu.Lock()
	defer r.commandMu.Unlock()
	if r.commandOutputFailure != nil {
		return r.commandOutputFailure
	}
	if err := item.Validate(); err != nil {
		return err
	}
	if _, ok := r.commandRuns[item.CommandRunID]; !ok {
		return ErrNotFound
	}
	outputs := r.commandOutputs[item.CommandRunID]
	if outputs == nil {
		outputs = map[uint64]CommandOutputChunk{}
		r.commandOutputs[item.CommandRunID] = outputs
	}
	if existing, ok := outputs[item.Sequence]; ok {
		if existing == item {
			return nil
		}
		return ErrCommandRunConflict
	}
	outputs[item.Sequence] = item
	return nil
}

func (r *memoryRepository) ListCommandOutputChunks(_ context.Context, commandRunID string, after uint64, limit int) ([]CommandOutputChunk, error) {
	r.commandMu.RLock()
	defer r.commandMu.RUnlock()
	if _, ok := r.commandRuns[commandRunID]; !ok {
		return nil, ErrNotFound
	}
	if limit <= 0 {
		return nil, ErrInvalidRequest
	}
	sequences := make([]uint64, 0, len(r.commandOutputs[commandRunID]))
	for sequence := range r.commandOutputs[commandRunID] {
		if sequence > after {
			sequences = append(sequences, sequence)
		}
	}
	sort.Slice(sequences, func(left, right int) bool { return sequences[left] < sequences[right] })
	if len(sequences) > limit {
		sequences = sequences[:limit]
	}
	items := make([]CommandOutputChunk, 0, len(sequences))
	for _, sequence := range sequences {
		items = append(items, r.commandOutputs[commandRunID][sequence])
	}
	return items, nil
}

func (r *memoryRepository) DeleteCommandOutputChunks(_ context.Context, commandRunID string) error {
	r.commandMu.Lock()
	defer r.commandMu.Unlock()
	if _, ok := r.commandRuns[commandRunID]; !ok {
		return ErrNotFound
	}
	delete(r.commandOutputs, commandRunID)
	r.commandOutputs[commandRunID] = map[uint64]CommandOutputChunk{}
	return nil
}

func (r *memoryRepository) PurgeCommandOutput(_ context.Context, id string, expectedRevision int64, item CommandRun) (CommandRun, error) {
	r.commandMu.Lock()
	defer r.commandMu.Unlock()
	current, ok := r.commandRuns[id]
	if !ok {
		return CommandRun{}, ErrNotFound
	}
	if current.Revision != expectedRevision || item.Revision != expectedRevision+1 {
		return CommandRun{}, ErrCommandRunConflict
	}
	if err := item.Validate(); err != nil {
		return CommandRun{}, err
	}
	delete(r.commandOutputs, id)
	r.commandOutputs[id] = map[uint64]CommandOutputChunk{}
	r.commandRuns[id] = item
	return item, nil
}
