package bot

import (
	"context"
	"strings"
	"unicode"
)

// runtimeBehaviorResult 是确定性行为层的受限输出；它只描述动作和原因码，
// 不生成正文，也不把群聊内容交给一个额外模型来决定是否插话。
type runtimeBehaviorResult struct {
	Action         string
	ReasonCodes    []string
	Confidence     float64
	RequiresAgent  bool
	AllowedTools   []string
	Urgency        string
	FollowUpPolicy string
	SafetyPolicy   string
	DirectText     string
}

// decideRuntimeBehavior 先执行安全和场景规则，再选择最小处理成本。复杂内容
// 才返回 reply_agent；问候、确认和无关群消息不会创建完整 Invocation。
func (m *Manager) decideRuntimeBehavior(_ context.Context, message Message, config MessageConfig) runtimeBehaviorResult {
	result := runtimeBehaviorResult{Action: "reply_agent", Confidence: 1, RequiresAgent: true, Urgency: "normal", FollowUpPolicy: "explicit_only", SafetyPolicy: "default", AllowedTools: append([]string(nil), config.Extensions.AllowedReadOnlyTools...)}
	if isGroupChat(message.ChatType) && !message.Mentioned && strings.TrimSpace(message.ReplyToMessageID) == "" && !m.groupMessageAllowed(message) {
		return runtimeBehaviorResult{Action: "observe", ReasonCodes: []string{"group_not_addressed"}, Confidence: 1, Urgency: "low", FollowUpPolicy: "none", SafetyPolicy: "default"}
	}
	text := strings.TrimSpace(message.Text)
	if text == "" && len(message.Attachments) > 0 {
		result.ReasonCodes = []string{"attachment_request"}
		return result
	}
	if direct := directAcknowledgement(text); direct != "" {
		return runtimeBehaviorResult{Action: "acknowledge", ReasonCodes: []string{"simple_social_message"}, Confidence: 1, Urgency: "normal", FollowUpPolicy: "none", SafetyPolicy: "default", DirectText: direct}
	}
	if strings.HasSuffix(text, "…") || strings.HasSuffix(text, "等下") || strings.Contains(text, "我再补充") {
		result.Action = "wait_more"
		result.RequiresAgent = false
		result.ReasonCodes = []string{"user_signalled_continuation"}
		result.Urgency = "low"
		return result
	}
	result.ReasonCodes = []string{"addressed_complex_request"}
	return result
}

// directAcknowledgement 只处理低风险、语义稳定的社交短句；任何包含疑问、
// 数字或附件上下文的内容都留给 Agent，避免误把真实任务当成寒暄。
func directAcknowledgement(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > 12 {
		return ""
	}
	for _, r := range text {
		if unicode.IsDigit(r) || strings.ContainsRune("?？:：/\\", r) {
			return ""
		}
	}
	switch text {
	case "你好", "嗨", "哈喽", "早", "早上好", "晚上好", "晚安":
		return "你好呀。"
	case "在吗", "在不在", "有人吗":
		return "在的。"
	case "收到", "好的", "好", "行", "嗯", "嗯嗯", "明白了":
		return "好。"
	case "谢谢", "感谢":
		return "不客气。"
	default:
		return ""
	}
}
