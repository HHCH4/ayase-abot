package workspace

import (
	"context"
	"fmt"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

type pathArgs struct {
	Path string `json:"path" jsonschema:"工作区根目录下的相对路径，根目录使用 ."`
}

type searchArgs struct {
	Path            string     `json:"path" jsonschema:"搜索起点的相对路径，根目录使用 ."`
	Query           string     `json:"query" jsonschema:"需要查找的原文字符串或正则表达式，最多 4096 字节"`
	Mode            SearchMode `json:"mode" jsonschema:"搜索模式：literal（默认）或 regex；regex 使用线性时间 RE2 语义"`
	CaseInsensitive bool       `json:"case_insensitive" jsonschema:"是否忽略大小写，默认 false"`
	Glob            string     `json:"glob" jsonschema:"可选的工作区相对路径 glob 过滤，例如 internal/**/*.go"`
	ContextLines    int        `json:"context_lines" jsonschema:"每个命中前后附带的上下文行数，范围 0 到 5"`
	MaxHits         int        `json:"max_hits" jsonschema:"最多返回的命中数，范围 1 到 100，默认 100"`
}

type globArgs struct {
	Pattern string `json:"pattern" jsonschema:"工作区根目录下的 glob 路径，例如 internal/**/*.go"`
}

type readRangeArgs struct {
	Path               string `json:"path" jsonschema:"工作区根目录下的目标相对路径"`
	StartLine          int    `json:"start_line" jsonschema:"起始行号，从 1 开始；省略或小于 1 时使用 1"`
	EndLine            int    `json:"end_line" jsonschema:"结束行号，最多覆盖 2000 行；省略时读取最多 2000 行"`
	IncludeLineNumbers bool   `json:"include_line_numbers" jsonschema:"是否在返回文本前加行号"`
}

type readManyArgs struct {
	Paths []string `json:"paths" jsonschema:"需要批量读取的工作区相对文件路径，最多 32 个且不能重复"`
}

type writeArgs struct {
	Path    string `json:"path" jsonschema:"工作区根目录下的目标相对路径"`
	Content string `json:"content" jsonschema:"要写入文件的完整文本内容"`
}

type patchArgs struct {
	Path    string `json:"path" jsonschema:"工作区根目录下的目标相对路径"`
	OldText string `json:"old_text" jsonschema:"需要精确匹配且只能出现一次的旧文本片段"`
	NewText string `json:"new_text" jsonschema:"替换旧文本片段的新文本"`
}

type changeSetPatchArgs struct {
	Path    string `json:"path" jsonschema:"工作区根目录下的目标相对路径"`
	OldText string `json:"old_text" jsonschema:"需要精确匹配且只能出现一次的旧文本片段"`
	NewText string `json:"new_text" jsonschema:"替换旧文本片段的新文本"`
}

type changeSetArgs struct {
	Patches []changeSetPatchArgs `json:"patches" jsonschema:"需要作为一个原子变更提交的多个文件补丁"`
}

type commandArgs struct {
	Command        string `json:"command" jsonschema:"需要在工作区中执行的 shell 命令"`
	CWD            string `json:"cwd" jsonschema:"工作区根目录下的相对工作目录，默认 ."`
	TimeoutSeconds int    `json:"timeout_seconds" jsonschema:"超时秒数，范围 1 到 120，默认 60"`
}

type filesResult struct {
	Files []FileEntry `json:"files"`
}

type fileResult struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type statResult struct {
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode,omitempty"`
	ModTime time.Time `json:"mod_time"`
}

type searchResult struct {
	Matches []SearchMatch `json:"matches"`
}

type commandToolResult struct {
	Output     string `json:"output"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	ExitCode   int    `json:"exit_code"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Unknown    bool   `json:"unknown,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type pendingOperationResult struct {
	OperationID string `json:"operation_id"`
	Status      string `json:"status"`
	Message     string `json:"message"`
}

// operationToolResult is kept wire-compatible with the legacy workspace
// operation result while allowing the resumed ADK tool call to report the
// actual side effect back to the model.
//
// The confirmation is requested explicitly from the handler instead of using
// functiontool's static RequireConfirmation switch. This lets us prepare and
// persist the Operation (including its diff/digest) before ADK emits the
// approval card. On resume the same handler looks up that prepared Operation;
// it never reconstructs a possibly different operation from user input.
func (s *Service) runApprovedOperation(ctx adkagent.Context, request OperationRequest, pendingMessage string) (pendingOperationResult, error) {
	request.UserID = UserIDFromContext(ctx)
	request.InvocationID = InvocationIDFromContext(ctx)
	request.ToolCallID = strings.TrimSpace(ctx.FunctionCallID())

	confirmation := ctx.ToolConfirmation()
	if confirmation != nil {
		operationID := confirmationOperationID(confirmation)
		if operationID == "" {
			operationID = s.findOperationID(ctx, request.InvocationID, request.ToolCallID)
		}
		if operationID == "" {
			return pendingOperationResult{}, fmt.Errorf("审批恢复缺少 operation_id")
		}
		operation, err := s.repository.GetOperation(ctx, operationID)
		if err != nil {
			return pendingOperationResult{OperationID: operationID}, err
		}
		// functiontool short-circuits rejected confirmations before invoking the
		// handler. This branch is still defensive for custom tool wrappers.
		if !confirmation.Confirmed {
			if operation.Status == OperationRejected {
				return pendingOperationResult{OperationID: operation.ID, Status: string(operation.Status), Message: "用户已拒绝"}, nil
			}
			if operation.Status != OperationPending && operation.Status != OperationPrepared {
				return pendingOperationResult{OperationID: operation.ID, Status: string(operation.Status), Message: operation.Result}, nil
			}
			rejected, rejectErr := s.RejectOperation(ctx, operation.ID)
			return pendingOperationResult{OperationID: rejected.ID, Status: string(rejected.Status), Message: rejected.Result}, rejectErr
		}
		if operation.Status == OperationCompleted || operation.Status == OperationFailed || operation.Status == OperationRejected {
			return pendingOperationResult{OperationID: operation.ID, Status: string(operation.Status), Message: operation.Result}, nil
		}
		completed, approveErr := s.ApproveOperation(ctx, operation.ID)
		message := completed.Result
		if message == "" {
			message = pendingMessage
		}
		return pendingOperationResult{OperationID: completed.ID, Status: string(completed.Status), Message: message}, approveErr
	}

	operation, err := s.CreateOperation(ctx, request)
	if err != nil {
		return pendingOperationResult{OperationID: operation.ID, Status: string(operation.Status), Message: pendingMessage}, err
	}
	payload := map[string]any{
		"operation_id":    operation.ID,
		"invocation_id":   operation.InvocationID,
		"tool_call_id":    operation.ToolCallID,
		"operation_type":  string(operation.Type),
		"expected_digest": operation.ExpectedDigest,
	}
	hint := pendingMessage
	if strings.TrimSpace(operation.Preview) != "" {
		hint += "：" + operation.Preview
	}
	if err := ctx.RequestConfirmation(hint, payload); err != nil {
		return pendingOperationResult{OperationID: operation.ID, Status: string(operation.Status), Message: pendingMessage}, err
	}
	return pendingOperationResult{OperationID: operation.ID, Status: string(operation.Status), Message: pendingMessage}, nil
}

func (s *Service) findOperationID(ctx context.Context, invocationID, toolCallID string) string {
	invocationID = strings.TrimSpace(invocationID)
	toolCallID = strings.TrimSpace(toolCallID)
	if invocationID == "" || toolCallID == "" {
		return ""
	}
	items, err := s.repository.ListOperations(ctx, "")
	if err != nil {
		return ""
	}
	for _, item := range items {
		if item.InvocationID == invocationID && item.ToolCallID == toolCallID {
			return item.ID
		}
	}
	return ""
}

func confirmationOperationID(confirmation *toolconfirmation.ToolConfirmation) string {
	if confirmation == nil {
		return ""
	}
	switch payload := confirmation.Payload.(type) {
	case map[string]any:
		if value, ok := payload["operation_id"].(string); ok {
			return strings.TrimSpace(value)
		}
	case map[string]string:
		return strings.TrimSpace(payload["operation_id"])
	}
	return ""
}

// ToolPolicy 是配置中心下发给项目工具宿主的权限策略；写入和命令即使开放仍需用户审批。
type ToolPolicy struct {
	Enabled               bool
	ReadEnabled           bool
	WriteEnabled          bool
	ExecEnabled           bool
	GitEnabled            bool
	CommandTimeoutSeconds int
}

// Tools 为某个对话创建绑定工具，模型不能在参数中切换工作区。
// conversationIDs 使用可变参数是为了兼容早期仅按工作区创建工具的调用方；新代码应传入对话 ID。
func (s *Service) Tools(ctx context.Context, workspaceID string, conversationIDs ...string) ([]tool.Tool, error) {
	return s.ToolsWithPolicy(ctx, workspaceID, firstConversationID(conversationIDs), ToolPolicy{
		Enabled: true, ReadEnabled: true, WriteEnabled: true, ExecEnabled: true, GitEnabled: true, CommandTimeoutSeconds: 60,
	})
}

// ToolsWithPolicy 按当前配置只暴露真实允许的工具，避免仅在提示词中“告知模型”权限而没有宿主约束。
func (s *Service) ToolsWithPolicy(ctx context.Context, workspaceID, conversationID string, policy ToolPolicy) ([]tool.Tool, error) {
	if !policy.Enabled {
		return []tool.Tool{}, nil
	}
	if policy.CommandTimeoutSeconds <= 0 {
		policy.CommandTimeoutSeconds = 60
	}
	if policy.CommandTimeoutSeconds > 120 {
		policy.CommandTimeoutSeconds = 120
	}
	item, err := s.Resolve(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("当前工作区为 %s（%s）。", item.Name, item.ID)
	result := make([]tool.Tool, 0, 16)
	if policy.ReadEnabled {
		listTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_list_files", Description: prefix + "列出指定相对目录中的文件。",
		}, func(ctx adkagent.Context, args pathArgs) (filesResult, error) {
			files, err := s.ListFiles(ctx, item.ID, args.Path)
			return filesResult{Files: files}, err
		})
		if toolErr != nil {
			return nil, toolErr
		}
		readTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_read_file", Description: prefix + "读取一个文本文件，路径必须位于工作区内。",
		}, func(ctx adkagent.Context, args pathArgs) (fileResult, error) {
			content, err := s.ReadFile(ctx, item.ID, args.Path)
			return fileResult{Path: args.Path, Content: content}, err
		})
		if toolErr != nil {
			return nil, toolErr
		}
		statTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_stat", Description: prefix + "查看文件或目录的大小、类型和修改时间，不读取正文。",
		}, func(ctx adkagent.Context, args pathArgs) (statResult, error) {
			stat, err := s.Stat(ctx, item.ID, args.Path)
			return statResult{Path: stat.Path, IsDir: stat.IsDir, Size: stat.Size, Mode: stat.Mode, ModTime: stat.ModTime}, err
		})
		if toolErr != nil {
			return nil, toolErr
		}
		searchTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_search_text", Description: prefix + "在工作区文本文件中进行有界搜索；默认原文匹配，可显式使用 RE2 正则，并支持大小写、路径 glob、上下文行和命中上限。",
		}, func(ctx adkagent.Context, args searchArgs) (searchResult, error) {
			matches, err := s.SearchWithOptions(ctx, item.ID, args.Path, args.Query, SearchOptions{
				Mode: args.Mode, CaseInsensitive: args.CaseInsensitive, Glob: args.Glob,
				ContextLines: args.ContextLines, MaxHits: args.MaxHits,
			})
			return searchResult{Matches: matches}, err
		})
		if toolErr != nil {
			return nil, toolErr
		}
		globTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_glob", Description: prefix + "按受控 glob 模式查找工作区内的文件和目录，支持 *、?、字符类和递归 **。",
		}, func(ctx adkagent.Context, args globArgs) (GlobResult, error) {
			return s.Glob(ctx, item.ID, args.Pattern)
		})
		if toolErr != nil {
			return nil, toolErr
		}
		rangeTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_read_range", Description: prefix + "按行读取文本文件的有限范围，并返回完整文件 digest 和总行数。",
		}, func(ctx adkagent.Context, args readRangeArgs) (ReadRangeResult, error) {
			return s.ReadRange(ctx, item.ID, args.Path, args.StartLine, args.EndLine, args.IncludeLineNumbers)
		})
		if toolErr != nil {
			return nil, toolErr
		}
		manyTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_read_many", Description: prefix + "按请求顺序批量读取有限数量的文本文件，单个文件或总字节超限会局部标记。",
		}, func(ctx adkagent.Context, args readManyArgs) (ReadManyResult, error) {
			return s.ReadMany(ctx, item.ID, args.Paths)
		})
		if toolErr != nil {
			return nil, toolErr
		}
		result = append(result, listTool, readTool, globTool, searchTool, rangeTool, manyTool, statTool)
		if policy.GitEnabled {
			gitTool, toolErr := functiontool.New(functiontool.Config{
				Name: "workspace_git_status", Description: prefix + "查看当前项目的 Git 分支和未提交变更。",
			}, func(ctx adkagent.Context, _ struct{}) (commandToolResult, error) {
				result, err := s.GitStatus(ctx, item.ID)
				return commandToolResult{Output: result.Output, Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode, Truncated: result.Truncated, TimedOut: result.TimedOut, Unknown: result.Unknown, DurationMS: result.DurationMS}, err
			})
			if toolErr != nil {
				return nil, toolErr
			}
			gitDiffTool, toolErr := functiontool.New(functiontool.Config{
				Name: "workspace_git_diff", Description: prefix + "查看当前项目未提交的 Git 差异。",
			}, func(ctx adkagent.Context, _ struct{}) (commandToolResult, error) {
				result, err := s.GitDiff(ctx, item.ID)
				return commandToolResult{Output: result.Output, Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode, Truncated: result.Truncated, TimedOut: result.TimedOut, Unknown: result.Unknown, DurationMS: result.DurationMS}, err
			})
			if toolErr != nil {
				return nil, toolErr
			}
			gitLogTool, toolErr := functiontool.New(functiontool.Config{
				Name: "workspace_git_log", Description: prefix + "查看当前项目最近的 Git 提交摘要。",
			}, func(ctx adkagent.Context, _ struct{}) (commandToolResult, error) {
				result, err := s.GitLog(ctx, item.ID)
				return commandToolResult{Output: result.Output, Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode, Truncated: result.Truncated, TimedOut: result.TimedOut, Unknown: result.Unknown, DurationMS: result.DurationMS}, err
			})
			if toolErr != nil {
				return nil, toolErr
			}
			result = append(result, gitTool, gitDiffTool, gitLogTool)
		}
	}
	if policy.WriteEnabled {
		writeTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_request_write", Description: prefix + "写入文本文件。工具会先生成待审阅操作并请求用户确认，批准后由本次 Agent 调用完成写入。",
		}, func(ctx adkagent.Context, args writeArgs) (pendingOperationResult, error) {
			return s.runApprovedOperation(ctx, OperationRequest{WorkspaceID: item.ID, ConversationID: conversationID, Type: OperationWriteFile, Path: args.Path, Content: args.Content}, "已提交写入申请，等待用户批准")
		})
		if toolErr != nil {
			return nil, toolErr
		}
		mkdirTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_request_mkdir", Description: prefix + "新建工作区内的目录。工具会先生成待审阅操作并请求用户确认。",
		}, func(ctx adkagent.Context, args pathArgs) (pendingOperationResult, error) {
			return s.runApprovedOperation(ctx, OperationRequest{WorkspaceID: item.ID, ConversationID: conversationID, Type: OperationMakeDirectory, Path: args.Path}, "已提交新建目录申请，等待用户批准")
		})
		if toolErr != nil {
			return nil, toolErr
		}
		patchTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_request_patch", Description: prefix + "对文本文件执行一次精确替换。工具会先生成待审阅操作并请求用户确认，批准后由本次 Agent 调用完成写入。",
		}, func(ctx adkagent.Context, args patchArgs) (pendingOperationResult, error) {
			return s.runApprovedOperation(ctx, OperationRequest{
				WorkspaceID: item.ID, ConversationID: conversationID, Type: OperationPatchFile, Path: args.Path,
				OldText: args.OldText, NewText: args.NewText,
			}, "已提交补丁申请，等待用户批准")
		})
		if toolErr != nil {
			return nil, toolErr
		}
		patchSetTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_request_patch_set", Description: prefix + "以一个 ChangeSet 原子修改多个文件。工具会先校验并生成统一 diff，再请求用户确认。",
		}, func(ctx adkagent.Context, args changeSetArgs) (pendingOperationResult, error) {
			patches := make([]FilePatch, 0, len(args.Patches))
			for _, patch := range args.Patches {
				patches = append(patches, FilePatch{Path: patch.Path, OldText: patch.OldText, NewText: patch.NewText})
			}
			return s.runApprovedOperation(ctx, OperationRequest{WorkspaceID: item.ID, ConversationID: conversationID, Type: OperationPatchSet, Patches: patches}, "已提交 ChangeSet，等待用户批准")
		})
		if toolErr != nil {
			return nil, toolErr
		}
		deleteTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_request_delete", Description: prefix + "删除工作区内的文件或空目录。工具会先生成待审阅操作并请求用户确认。",
		}, func(ctx adkagent.Context, args pathArgs) (pendingOperationResult, error) {
			return s.runApprovedOperation(ctx, OperationRequest{WorkspaceID: item.ID, ConversationID: conversationID, Type: OperationDeletePath, Path: args.Path}, "已提交删除申请，等待用户批准")
		})
		if toolErr != nil {
			return nil, toolErr
		}
		result = append(result, writeTool, mkdirTool, patchTool, patchSetTool, deleteTool)
	}
	if policy.ExecEnabled {
		commandTool, toolErr := functiontool.New(functiontool.Config{
			Name: "workspace_request_command", Description: prefix + "执行 shell 命令。工具会先生成待审阅操作并请求用户确认，批准后由本次 Agent 调用完成执行。",
		}, func(ctx adkagent.Context, args commandArgs) (pendingOperationResult, error) {
			timeout := args.TimeoutSeconds
			if timeout <= 0 || timeout > policy.CommandTimeoutSeconds {
				timeout = policy.CommandTimeoutSeconds
			}
			return s.runApprovedOperation(ctx, OperationRequest{
				WorkspaceID: item.ID, ConversationID: conversationID, Type: OperationCommand, Command: args.Command,
				CWD: args.CWD, Timeout: timeout,
			}, "已提交命令申请，等待用户批准")
		})
		if toolErr != nil {
			return nil, toolErr
		}
		result = append(result, commandTool)
	}
	return result, nil
}

func firstConversationID(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
