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
	// InputArtifactRefs 是子任务实际使用的受控输入引用。图片 bytes 只在
	// provider 边界短暂存在，持久化审计和恢复依赖这里的不可变引用。
	InputArtifactRefs []AttachmentRef
	// DependencyResults 只包含当前 Run 显式声明依赖的有界结果。它不是主会话
	// 历史，不能扩大权限，也不会把其他 Run 的隐藏上下文带入模型。
	DependencyResults map[string]SubAgentRun
}

// SubAgentRunner 是主 Agent 调用通用子 Agent 的唯一执行接口。
type SubAgentRunner interface {
	RunSubAgent(context.Context, SubAgentRequest) (string, error)
}

// SubAgentGroupRunner 是文档解析、检索编排等批量子任务使用的统一入口。
// 它只返回结构化结果，不拥有平台发送、审批或父任务状态修改权限。
type SubAgentGroupRunner interface {
	RunSubAgentGroup(context.Context, SubAgentGroupRequest) (SubAgentGroupResult, error)
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

func (k *Kernel) subAgentToolRunner() SubAgentRunner {
	if manager, ok := k.subAgentGroupRunner().(SubAgentRunner); ok && manager != nil {
		return manager
	}
	return k.genericSubAgentRunner()
}

// SetSubAgentManager 安装 Group/Run 编排器。Manager 与普通 Runner 分开，确保
// 单次工具调用和文档图片批处理都经过相同的队列、预算、生命周期边界。
func (k *Kernel) SetSubAgentManager(manager SubAgentGroupRunner) {
	if k == nil {
		return
	}
	k.locksMu.Lock()
	k.subAgentManager = manager
	k.locksMu.Unlock()
}

func (k *Kernel) subAgentGroupRunner() SubAgentGroupRunner {
	if k == nil {
		return nil
	}
	k.locksMu.Lock()
	defer k.locksMu.Unlock()
	return k.subAgentManager
}
