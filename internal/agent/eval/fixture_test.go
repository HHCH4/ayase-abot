package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestWorkspaceFixtureIsolatedAndMetadataOnly(t *testing.T) {
	files := []FixtureFile{
		{Path: "src/main.go", Content: []byte("package main\n")},
		{Path: "README.md", Content: []byte("fixture\n")},
	}
	first, err := NewWorkspaceFixture(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	root := first.RootPath()
	if root == "" {
		t.Fatal("fixture root 不能为空")
	}
	path, err := first.Resolve("src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("fixture 文件不可读: content=%q err=%v", content, err)
	}
	manifest := first.Manifest()
	if len(manifest.Files) != 2 || manifest.TotalBytes != int64(len("package main\nfixture\n")) || manifest.Digest == "" {
		t.Fatalf("fixture manifest 不正确: %#v", manifest)
	}
	encodedMetadata := fmt.Sprintf("%#v", manifest)
	if strings.Contains(encodedMetadata, "package main") || strings.Contains(encodedMetadata, "fixture") {
		t.Fatalf("fixture metadata 不应包含文件正文: %s", encodedMetadata)
	}

	second, err := NewWorkspaceFixture(context.Background(), []FixtureFile{files[1], files[0]})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest, second.Manifest()) {
		t.Fatalf("相同文件定义的 manifest digest 应稳定: first=%#v second=%#v", manifest, second.Manifest())
	}
	secondRoot := second.RootPath()
	if root == secondRoot {
		t.Fatal("每个 fixture 必须使用独立临时目录")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Close 后 fixture 目录仍存在: stat err=%v", err)
	}
	if _, err := first.Resolve("src/main.go"); !errors.Is(err, ErrFixtureClosed) {
		t.Fatalf("关闭后的 fixture 应拒绝路径解析: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close 应幂等: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFixtureResolveRejectsSymlinkEscape(t *testing.T) {
	fixture, err := NewWorkspaceFixture(context.Background(), []FixtureFile{{Path: "inside.txt", Content: []byte("ok")}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.Close() }()
	outside := t.TempDir()
	if err := os.Symlink(outside, fixture.RootPath()+"/escape"); err != nil {
		t.Skipf("当前环境不支持创建符号链接: %v", err)
	}
	if _, err := fixture.Resolve("escape/file.txt"); !errors.Is(err, ErrInvalidFixture) {
		t.Fatalf("符号链接越界应被拒绝: %v", err)
	}
}

func TestWorkspaceFixtureRejectsUnsafeOrOversizedDefinitions(t *testing.T) {
	tooLarge := make([]byte, MaxFixtureFileBytes+1)
	tooMuch := make([]FixtureFile, 9)
	for index := range tooMuch {
		tooMuch[index] = FixtureFile{Path: fmt.Sprintf("chunk-%d", index), Content: make([]byte, MaxFixtureFileBytes)}
	}
	tooMany := make([]FixtureFile, MaxFixtureFiles+1)
	for index := range tooMany {
		tooMany[index] = FixtureFile{Path: fmt.Sprintf("file-%d", index)}
	}
	cases := []struct {
		name  string
		files []FixtureFile
	}{
		{name: "empty path", files: []FixtureFile{{Path: ""}}},
		{name: "absolute path", files: []FixtureFile{{Path: "/tmp/escape"}}},
		{name: "parent path", files: []FixtureFile{{Path: "../escape"}}},
		{name: "non canonical", files: []FixtureFile{{Path: "./file"}}},
		{name: "backslash", files: []FixtureFile{{Path: "dir\\file"}}},
		{name: "duplicate", files: []FixtureFile{{Path: "file", Content: []byte("a")}, {Path: "file", Content: []byte("b")}}},
		{name: "file bytes", files: []FixtureFile{{Path: "large", Content: tooLarge}}},
		{name: "total bytes", files: tooMuch},
		{name: "unsafe mode", files: []FixtureFile{{Path: "setuid", Mode: 0o6000}}},
		{name: "file count", files: tooMany},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if _, err := NewWorkspaceFixture(context.Background(), item.files); !errors.Is(err, ErrInvalidFixture) {
				t.Fatalf("不安全 fixture 应被拒绝: %v", err)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewWorkspaceFixture(ctx, nil); !errors.Is(err, ErrInvalidFixture) {
		t.Fatalf("取消 context 应阻止 fixture 创建: %v", err)
	}
}

func TestEvaluateWorkspaceWorkflowSuiteUsesFreshFixturesAndPrevalidates(t *testing.T) {
	var roots []string
	cases := []WorkspaceWorkflowCase{
		{
			Case:  EvalCase{ID: "z-case", Version: "1", ExpectedOutcome: "completed"},
			Files: []FixtureFile{{Path: "marker.txt", Content: []byte("z")}},
			Execute: func(_ context.Context, fixture *WorkspaceFixture) (Input, error) {
				path, err := fixture.Resolve("marker.txt")
				if err != nil {
					return Input{}, err
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "z" {
					return Input{}, fmt.Errorf("fixture content=%q err=%v", data, err)
				}
				roots = append(roots, fixture.RootPath())
				return completedWorkflowInput("inv-z"), nil
			},
		},
		{
			Case:  EvalCase{ID: "a-case", Version: "1", ExpectedOutcome: "completed"},
			Files: []FixtureFile{{Path: "marker.txt", Content: []byte("a")}},
			Execute: func(_ context.Context, fixture *WorkspaceFixture) (Input, error) {
				path, err := fixture.Resolve("marker.txt")
				if err != nil {
					return Input{}, err
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "a" {
					return Input{}, fmt.Errorf("fixture content=%q err=%v", data, err)
				}
				roots = append(roots, fixture.RootPath())
				return completedWorkflowInput("inv-a"), nil
			},
		},
	}
	result, err := EvaluateWorkspaceWorkflowSuite(context.Background(), cases, nil, GatePolicy{MinPassRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 || result.Results[0].CaseID != "a-case" || result.Results[1].CaseID != "z-case" || !result.Gate.Passed {
		t.Fatalf("fixture workflow suite 结果不正确: %#v", result)
	}
	if len(roots) != 2 || roots[0] == roots[1] {
		t.Fatalf("每个 case 应使用独立 fixture: %#v", roots)
	}
	for _, root := range roots {
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("workflow 完成后 fixture 未清理: root=%s err=%v", root, err)
		}
	}

	called := false
	invalid := []WorkspaceWorkflowCase{
		{
			Case: EvalCase{ID: "valid", Version: "1"},
			Execute: func(context.Context, *WorkspaceFixture) (Input, error) {
				called = true
				return completedWorkflowInput("valid"), nil
			},
		},
		{
			Case:  EvalCase{ID: "invalid", Version: "1"},
			Files: []FixtureFile{{Path: "../escape"}},
			Execute: func(context.Context, *WorkspaceFixture) (Input, error) {
				called = true
				return Input{}, nil
			},
		},
	}
	if _, err := EvaluateWorkspaceWorkflowSuite(context.Background(), invalid, nil, GatePolicy{}); !errors.Is(err, ErrInvalidWorkflowCase) {
		t.Fatalf("fixture 定义应在 executor 前校验: %v", err)
	}
	if called {
		t.Fatal("fixture 定义无效时不应执行任何 case")
	}
}
