package agent

import (
	"context"
	"errors"
	"sort"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// RuntimeActionRequest 是模型请求 Bot Runtime 执行一次结构化动作的最小载荷。
// 动作的权限、持久化和平台副作用都由宿主回调负责，Agent 内核不直接接触数据库。
type RuntimeActionRequest struct {
	Type           string
	UserID         string
	BotID          string
	ConversationID string
	Message        string
	Delivery       *BotDeliveryTarget
	Payload        map[string]any
}

// RuntimeActionHandler 把主 Agent 与宿主运行时动作层连接起来。
type RuntimeActionHandler func(context.Context, RuntimeActionRequest) (string, error)

type createFollowUpArgs struct {
	Name              string `json:"name,omitempty" jsonschema:"任务名称"`
	Request           string `json:"request" jsonschema:"到期时交给 Agent 执行的请求"`
	Goal              string `json:"goal,omitempty" jsonschema:"任务目标"`
	DueAt             string `json:"due_at,omitempty" jsonschema:"一次性任务执行时间，RFC3339；不填表示稍后执行"`
	Mode              string `json:"mode,omitempty" jsonschema:"once、interval、daily、weekly、monthly 或 custom"`
	IntervalSeconds   int    `json:"interval_seconds,omitempty" jsonschema:"interval 模式的间隔秒数"`
	Weekday           int    `json:"weekday,omitempty" jsonschema:"weekly 模式的星期，0 到 6"`
	MonthDay          int    `json:"month_day,omitempty" jsonschema:"monthly 模式的日期，1 到 31"`
	TimeOfDay         string `json:"time_of_day,omitempty" jsonschema:"daily、weekly、monthly 模式的本地时间 HH:MM"`
	Cron              string `json:"cron,omitempty" jsonschema:"custom 模式的五段 Cron"`
	Timezone          string `json:"timezone,omitempty" jsonschema:"任务时区，例如 Asia/Shanghai"`
	MaxRetries        int    `json:"max_retries,omitempty" jsonschema:"失败后的最大重试次数"`
	RetryDelaySeconds int    `json:"retry_delay_seconds,omitempty" jsonschema:"首次重试等待秒数"`
	MaxDelaySeconds   int    `json:"max_delay_seconds,omitempty" jsonschema:"重试等待最大秒数"`
	QuietHoursStart   string `json:"quiet_hours_start,omitempty" jsonschema:"安静时段开始 HH:MM"`
	QuietHoursEnd     string `json:"quiet_hours_end,omitempty" jsonschema:"安静时段结束 HH:MM"`
}

type createFollowUpResult struct {
	FollowUpID string `json:"follow_up_id"`
	Status     string `json:"status"`
}

// newCreateFollowUpTool 暴露唯一的通用 Follow-up 动作。模型只能提交结构化
// 计划，宿主仍会再次校验“明确要求后才创建”、权限、来源和数量上限。
func newCreateFollowUpTool(handler RuntimeActionHandler, request ChatRequest) (tool.Tool, error) {
	if handler == nil {
		return nil, errors.New("Runtime 动作宿主未装配")
	}
	return functiontool.New(functiontool.Config{
		Name:        "create_follow_up",
		Description: "仅在用户明确要求提醒、稍后执行、定时执行或持续跟进时创建一个 Follow-up。不要根据普通聊天自行创建；工具执行结果只表示任务是否已登记。",
	}, func(ctx adkagent.Context, args createFollowUpArgs) (createFollowUpResult, error) {
		if strings.TrimSpace(args.Request) == "" {
			return createFollowUpResult{}, errors.New("Follow-up request 不能为空")
		}
		payload := map[string]any{
			"name": args.Name, "request": args.Request, "goal": args.Goal, "due_at": args.DueAt,
			"mode": args.Mode, "interval_seconds": args.IntervalSeconds, "weekday": args.Weekday,
			"month_day": args.MonthDay, "time_of_day": args.TimeOfDay, "cron": args.Cron,
			"timezone": args.Timezone, "max_retries": args.MaxRetries, "retry_delay_seconds": args.RetryDelaySeconds,
			"max_delay_seconds": args.MaxDelaySeconds, "quiet_hours_start": args.QuietHoursStart,
			"quiet_hours_end": args.QuietHoursEnd,
		}
		result, err := handler(contextFromADK(ctx), RuntimeActionRequest{
			Type: "create_follow_up", UserID: request.UserID, BotID: request.BotID,
			ConversationID: request.ConversationID, Message: request.Message, Delivery: request.BotDelivery,
			Payload: payload,
		})
		if err != nil {
			return createFollowUpResult{}, err
		}
		return createFollowUpResult{FollowUpID: strings.TrimSpace(result), Status: "scheduled"}, nil
	})
}

// contextFromADK 保留 ADK 工具的取消语义，同时避免把工具上下文类型泄漏给宿主接口。
func contextFromADK(ctx adkagent.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// normalizeToolNames 规范化请求级工具白名单，保持稳定顺序，便于 ToolSet
// 快照在重启和恢复时得到相同的摘要。
func normalizeToolNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// proactiveToolNames 在没有显式白名单时从已注册目录中筛出只读工具，避免
// 主动任务因为默认工具目录扩大而获得写入、执行或外部副作用能力。
func proactiveToolNames(descriptors []ToolDescriptor, available []string) []string {
	availableSet := make(map[string]struct{}, len(available))
	for _, name := range available {
		availableSet[strings.TrimSpace(name)] = struct{}{}
	}
	result := make([]string, 0, len(available))
	for _, descriptor := range descriptors {
		if _, exists := availableSet[descriptor.ModelName]; !exists {
			continue
		}
		unsafe := false
		for _, capability := range descriptor.Capabilities {
			switch capability {
			case ToolCapabilityWriteWorkspace, ToolCapabilityExecuteProcess, ToolCapabilityInteractiveTerminal,
				ToolCapabilityWriteMemory, ToolCapabilityExternalSideEffect, ToolCapabilityDestructive:
				unsafe = true
			}
		}
		if !unsafe {
			result = append(result, descriptor.ModelName)
		}
	}
	return normalizeToolNames(result)
}

// readOnlyToolNames 把调用方给出的“只读工具”名单再次与工具目录能力做交集。
// 配置文本本身不构成安全边界，避免把一个写入或外部副作用工具误标成只读。
func readOnlyToolNames(descriptors []ToolDescriptor, requested, available []string) []string {
	allowed := make(map[string]struct{}, len(normalizeToolNames(requested)))
	for _, name := range normalizeToolNames(requested) {
		allowed[name] = struct{}{}
	}
	if len(allowed) == 0 {
		return proactiveToolNames(descriptors, available)
	}
	filtered := make([]string, 0, len(allowed))
	for _, name := range proactiveToolNames(descriptors, available) {
		if _, ok := allowed[name]; ok {
			filtered = append(filtered, name)
		}
	}
	return normalizeToolNames(filtered)
}

// filterToolsByNames 给没有装配 ToolRegistry 的嵌入方提供同样的请求级工具
// 白名单；正式应用始终走带能力快照的 Registry 选择路径。
func filterToolsByNames(tools []tool.Tool, names []string) []tool.Tool {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range normalizeToolNames(names) {
		allowed[name] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil
	}
	result := make([]tool.Tool, 0, len(tools))
	for _, item := range tools {
		if item == nil {
			continue
		}
		if _, ok := allowed[strings.TrimSpace(item.Name())]; ok {
			result = append(result, item)
		}
	}
	return result
}

// filterProactiveToolsByName 是无目录嵌入方的保守兜底；有目录时不会使用名称
// 猜测，而是使用工具声明的能力枚举。
func filterProactiveToolsByName(tools []tool.Tool) []tool.Tool {
	result := make([]tool.Tool, 0, len(tools))
	for _, item := range tools {
		if item == nil || unsafeProactiveToolName(item.Name()) {
			continue
		}
		result = append(result, item)
	}
	return result
}

func unsafeProactiveToolName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{"write", "patch", "delete", "remove", "execute", "exec", "terminal", "shell", "command", "git", "send", "recall", "poke", "reaction", "memory_save", "save_memory"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}
