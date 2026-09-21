package bot

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxSegmentParts = 24

// sendConfigured 只对符合扩展策略的回复拆段；模型输出与控制消息保留独立语义。
func (m *Manager) sendConfigured(ctx context.Context, message Message, text string, llmResult bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	config, err := m.resolveMessageConfig(ctx, message)
	if err != nil {
		slog.Warn("读取分段回复配置失败，直接发送完整文本", "adapter_id", message.AdapterID, "error", err)
		return m.sendRaw(ctx, message, text)
	}
	settings := config.Extensions
	if !settings.SegmentedReplyEnabled || settings.SegmentOnlyLLM && !llmResult {
		return m.sendRaw(ctx, message, text)
	}
	parts := splitReply(text, settings)
	for index, part := range parts {
		if index > 0 {
			// 间隔受配置和上下文超时约束；取消后不能继续发送剩余片段。
			timer := time.NewTimer(segmentDelay(part, settings))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := m.sendRaw(ctx, message, part); err != nil {
			return err
		}
	}
	return nil
}

// sendLLM 只供最终模型结果使用，防止审批卡片和指令答复被“仅 LLM”策略误拆。
func (m *Manager) sendLLM(ctx context.Context, message Message, text string) error {
	// 可选对最终模型正文应用同一组内容规则，控制消息与审批卡片不受影响。
	if config, err := m.resolveMessageConfig(ctx, message); err == nil && config.Platform.CheckResponse && blockedByPattern(text, config.Platform.BlockPatterns) {
		return m.sendRaw(ctx, message, "模型回复未通过内容规则检查。")
	}
	return m.sendConfigured(ctx, message, text, true)
}

// splitReply 对短文本按正则或词表拆分；超长/异常规则保持完整文本以免丢内容。
func splitReply(text string, settings ExtensionConfig) []string {
	threshold := settings.SegmentWordsThreshold
	if threshold <= 0 {
		threshold = 150
	}
	if utf8.RuneCountInString(text) > threshold || strings.TrimSpace(text) == "" {
		return []string{text}
	}
	var parts []string
	if settings.SegmentSplitMode == "words" {
		parts = splitByWords(text, settings.SegmentSplitWords)
	} else {
		pattern := settings.SegmentRegex
		if pattern == "" {
			pattern = ".*?[。？！~…]+|.+$"
		}
		re, err := regexp.Compile("(?s)" + pattern)
		if err != nil {
			return []string{text}
		}
		parts = re.FindAllString(text, maxSegmentParts+1)
		// 正则可能只匹配了部分原文；这种情况必须原样投递，不能静默丢字符。
		if strings.Join(parts, "") != text {
			return []string{text}
		}
	}
	if len(parts) <= 1 || len(parts) > maxSegmentParts {
		return []string{text}
	}
	if settings.SegmentCleanupRegex != "" {
		cleanup, err := regexp.Compile(settings.SegmentCleanupRegex)
		if err != nil {
			return []string{text}
		}
		for index := range parts {
			parts[index] = cleanup.ReplaceAllString(parts[index], "")
		}
	}
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			result = append(result, part)
		}
	}
	if len(result) == 0 {
		return []string{text}
	}
	return result
}

// splitByWords 从左到右选择最长分隔词，片段本身不携带分隔词，与词表模式一致。
func splitByWords(text string, words []string) []string {
	if len(words) == 0 {
		return []string{text}
	}
	var parts []string
	start := 0
	for index := 0; index < len(text); {
		longest := ""
		for _, word := range words {
			if word != "" && strings.HasPrefix(text[index:], word) && len(word) > len(longest) {
				longest = word
			}
		}
		if longest != "" {
			parts = append(parts, text[start:index])
			index += len(longest)
			start = index
		} else {
			_, size := utf8.DecodeRuneInString(text[index:])
			index += size
		}
		if len(parts) > maxSegmentParts {
			return []string{text}
		}
	}
	parts = append(parts, text[start:])
	return parts
}

// segmentDelay 使用有界秒数计算间隔；无效旧配置按零间隔处理，不阻塞投递。
func segmentDelay(text string, settings ExtensionConfig) time.Duration {
	if settings.SegmentIntervalMethod == "log" {
		base := settings.SegmentLogBase
		if base <= 1 {
			base = 2.6
		}
		count := 0
		for _, char := range text {
			if unicode.IsLetter(char) || unicode.IsNumber(char) {
				count++
			}
		}
		return time.Duration(math.Min(10, math.Log(float64(count+1))/math.Log(base)+rand.Float64()*0.5) * float64(time.Second))
	}
	values := strings.Split(settings.SegmentInterval, ",")
	if len(values) != 2 {
		return 0
	}
	minimum, leftErr := strconv.ParseFloat(strings.TrimSpace(values[0]), 64)
	maximum, rightErr := strconv.ParseFloat(strings.TrimSpace(values[1]), 64)
	if leftErr != nil || rightErr != nil || minimum < 0 || maximum > 10 || minimum > maximum {
		return 0
	}
	return time.Duration((minimum + rand.Float64()*(maximum-minimum)) * float64(time.Second))
}
