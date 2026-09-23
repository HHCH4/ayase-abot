package agent

import (
	"context"
	"errors"

	"Abot/internal/document"

	"google.golang.org/adk/v2/tool"
)

// ErrSubAgentsDisabled 表示当前会话禁止主 Agent 启动子 Agent。
var ErrSubAgentsDisabled = errors.New("当前会话已停用子 Agent")

// SubAgentRequest 描述一次通用子 Agent 调用。子任务继承父 Context，不单独设置
// 总超时；工具、图片和请求文本仅在这次调用中传递，不进入另一套任务队列。
type SubAgentRequest struct {
	InvocationID   string
	ParentNodeID   string
	UserID         string
	ConversationID string
	Purpose        string
	Prompt         string
	Runtime        RuntimeOptions
	Tools          []tool.Tool
	Images         []document.Image
}

// SubAgentRunner 是主 Agent 调用通用子 Agent 的唯一执行接口。
type SubAgentRunner interface {
	RunSubAgent(context.Context, SubAgentRequest) (string, error)
}

// SetSubAgentRunner 安装通用子 Agent 执行器，并把固定工具 schema 纳入工具目录。
func (k *Kernel) SetSubAgentRunner(runner SubAgentRunner) {
	if k == nil {
		return
	}
	k.locksMu.Lock()
	k.subAgentRunner = runner
	registry := k.toolRegistry
	k.locksMu.Unlock()
	if runner == nil || registry == nil {
		return
	}
	placeholder, err := newRunSubAgentTool(runner, SubAgentRequest{}, nil, 0)
	if err == nil {
		_ = registry.RegisterRuntimeTools([]tool.Tool{placeholder}, ToolSourceBuiltin)
	}
}

func (k *Kernel) genericSubAgentRunner() SubAgentRunner {
	if k == nil {
		return nil
	}
	k.locksMu.Lock()
	defer k.locksMu.Unlock()
	return k.subAgentRunner
}
