package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var errProactiveSuppressed = errors.New("主动消息被 Bot Runtime 策略抑制")

var (
	markdownLinkPattern  = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`)
	markdownStylePattern = regexp.MustCompile(`(\*\*|__|~~|(?m)(^|\s)\*)([^\n]+?)(\*\*|__|~~|\*)`)
)

// sendConfigured 将所有出站文本交给表达层；控制消息保持单段，模型最终结果才做
// 语义分段。平台长度限制仍由适配器作为最后一道安全兜底。
func (m *Manager) sendConfigured(ctx context.Context, message Message, text string, llmResult bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	config, err := m.resolveMessageConfig(ctx, message)
	if err != nil {
		if message.IsProactive {
			// 主动任务必须等待下一次调度重试；配置解析失败时不能绕过
			// 安静时段、来源白名单和额度限制直接发到平台。
			slog.Warn("读取主动投递配置失败，保留任务等待重试", "adapter_id", message.AdapterID, "source", messageSource(message), "error", err)
			return fmt.Errorf("读取主动投递配置失败: %w", err)
		}
		slog.Warn("读取聊天表达配置失败，使用纯文本降级", "adapter_id", message.AdapterID, "error", err)
		return m.sendRawPrepared(ctx, message, normalizeMarkdownPlain(text), "")
	}
	settings := config.Extensions
	reservedAt := time.Time{}
	if message.IsProactive {
		var allowed bool
		reservedAt, allowed, err = m.reserveProactiveDelivery(ctx, message, settings)
		if err != nil {
			// 主动任务不能在额度状态读取失败时绕过限流直接发送；交给
			// 调度器重试，内部错误只进入日志，不暴露数据库细节。
			return fmt.Errorf("记录主动投递额度失败: %w", err)
		}
		if !allowed {
			// Follow-up 的内部任务仍然可以完成，但平台出站动作必须受冷却、
			// 安静时段、来源白名单和小时上限共同约束，不能悄悄越过策略。
			return errProactiveSuppressed
		}
	}
	text = config.Platform.ReplyPrefix + text
	message.ReplyMention = config.Platform.ReplyMention
	message.ReplyQuote = config.Platform.ReplyQuote
	if !isGroupChat(message.ChatType) {
		message.ReplyQuote = config.Platform.PrivateReplyQuote
	}
	// 先在原始 Markdown 上做语义分段，再分别渲染到平台格式；如果先去掉
	// 代码围栏，分段器就无法知道哪些内容必须保持完整。
	parts := []string{text}
	if llmResult && settings.ExpressionEnabled {
		// 自然聊天保留较大的话题块；结构化说明更早在段落边界分开，
		// 让长清单、配置和文档分析不会挤成一条平台消息。
		threshold := settings.ExpressionLongThreshold
		if settings.MessageStyle == "structured" && threshold > 0 {
			threshold /= 2
		}
		parts = semanticSegments(text, settings.ExpressionMaxSegments, threshold)
	}
	for index, part := range parts {
		if index > 0 && settings.ExpressionDelayMilliseconds > 0 {
			timer := time.NewTimer(time.Duration(settings.ExpressionDelayMilliseconds) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				if index == 0 && message.IsProactive {
					m.rollbackProactiveDelivery(ctx, message, reservedAt)
				}
				return ctx.Err()
			case <-timer.C:
			}
		}
		segmentMessage := message
		segmentMessage.RuntimeActionSequence = index + 1
		if index > 0 {
			segmentMessage.ReplyQuote = false
			segmentMessage.ReplyMention = false
		}
		rendered, format := renderPlatformText(segmentMessage.Platform, part)
		if err := m.sendRawPrepared(ctx, segmentMessage, rendered, format); err != nil {
			if index == 0 && message.IsProactive {
				// 首段尚未交给平台时，额度预留不应把一次失败永久
				// 计入冷却或小时上限；已经成功发送过的分段则保留事实。
				m.rollbackProactiveDelivery(ctx, message, reservedAt)
			}
			return err
		}
	}
	return nil
}

// sendLLM 只发送最终模型正文；工具过程和流式增量不会进入该入口。
func (m *Manager) sendLLM(ctx context.Context, message Message, text string) error {
	if config, err := m.resolveMessageConfig(ctx, message); err == nil && config.Platform.CheckResponse && blockedByPattern(text, config.Platform.BlockPatterns) {
		return m.sendRaw(ctx, message, "模型回复未通过内容规则检查。")
	}
	return m.sendConfigured(ctx, message, text, true)
}

func renderPlatformText(platform Type, text string) (string, string) {
	plain := normalizeMarkdownPlain(text)
	if platform != TypeTelegram {
		return plain, "plain"
	}
	// Telegram 的安全 HTML 只使用固定生成的标签；所有模型文本先转义，再恢复链接。
	escaped := html.EscapeString(plain)
	return escaped, "html"
}

// normalizeMarkdownPlain 把常见 Markdown 变成聊天平台可读纯文本，不把控制符原样发给用户。
func normalizeMarkdownPlain(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = markdownLinkPattern.ReplaceAllString(text, "$1：$2")
	text = strings.ReplaceAll(text, "```", "")
	text = strings.ReplaceAll(text, "`", "")
	text = markdownStylePattern.ReplaceAllString(text, "$2$3")
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			lines[index] = trimmed
			continue
		}
		if strings.HasPrefix(trimmed, "> ") {
			lines[index] = "引用：" + strings.TrimSpace(strings.TrimPrefix(trimmed, "> "))
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// semanticSegments 先按完整自然段组织，再在长段落的句末切分；短回复保持一条，
// 不为模拟聊天而把每句话机械拆成独立消息。
func semanticSegments(text string, maxSegments, longThreshold int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{""}
	}
	if maxSegments <= 1 {
		return []string{text}
	}
	if maxSegments > 8 {
		maxSegments = 8
	}
	if longThreshold <= 0 {
		longThreshold = 600
	}
	if utf8.RuneCountInString(text) < longThreshold {
		return []string{text}
	}
	paragraphs := splitExpressionBlocks(text)
	parts := make([]string, 0, maxSegments)
	for _, paragraph := range paragraphs {
		// fenced code 块是不可拆分的语义单元；平台长度兜底会在最后一层
		// 处理极端超长代码，但正常表达计划不能从代码中间切开。
		if isFencedCodeBlock(paragraph) {
			if len(parts) >= maxSegments {
				parts[len(parts)-1] += "\n\n" + paragraph
			} else {
				parts = append(parts, paragraph)
			}
			continue
		}
		if len(parts) < maxSegments-1 && utf8.RuneCountInString(paragraph) >= longThreshold/2 {
			left, right := splitSentenceGroup(paragraph)
			if right != "" {
				parts = append(parts, left)
				paragraph = right
			}
		}
		if len(parts) >= maxSegments {
			parts[len(parts)-1] += "\n\n" + paragraph
		} else {
			parts = append(parts, paragraph)
		}
	}
	return mergeTinyExpressionParts(parts)
}

// splitExpressionBlocks 将围栏代码块作为原子块，其余内容按空行分成自然段。
// 这样长文可以继续按句群组织，而代码、链接所在的完整表达不会被粗暴拆开。
func splitExpressionBlocks(text string) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	blocks := make([]string, 0, 8)
	var normal []string
	var code []string
	inCode := false
	flushNormal := func() {
		value := strings.TrimSpace(strings.Join(normal, "\n"))
		if value != "" {
			blocks = append(blocks, value)
		}
		normal = nil
	}
	flushCode := func() {
		value := strings.TrimSpace(strings.Join(code, "\n"))
		if value != "" {
			blocks = append(blocks, value)
		}
		code = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inCode {
				flushNormal()
				inCode = true
			}
			code = append(code, line)
			if len(code) > 1 && trimmed == "```" {
				inCode = false
				flushCode()
			}
			continue
		}
		if inCode {
			code = append(code, line)
			continue
		}
		if trimmed == "" {
			flushNormal()
			continue
		}
		normal = append(normal, line)
	}
	if inCode {
		// 未闭合围栏仍视为代码块，优先保持原文而不是在中间拆分。
		flushCode()
	} else {
		flushNormal()
	}
	if len(blocks) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return blocks
}

func isFencedCodeBlock(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return len(lines) >= 2 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```")
}

func splitSentenceGroup(text string) (string, string) {
	runes := []rune(text)
	middle := len(runes) / 2
	for offset := 0; offset < len(runes)/2; offset++ {
		for _, index := range []int{middle + offset, middle - offset} {
			if index <= 0 || index >= len(runes)-1 {
				continue
			}
			if strings.ContainsRune("。！？!?；;\n", runes[index]) {
				left := strings.TrimSpace(string(runes[:index+1]))
				right := strings.TrimSpace(string(runes[index+1:]))
				if right != "" && safeExpressionBoundary(left, right, runes[index]) {
					return left, right
				}
			}
		}
	}
	return text, ""
}

// safeExpressionBoundary 防止句末切分破坏行内代码、Markdown 链接、表格行和列表项。
// 这些内容即使最终渲染为纯文本，也必须先作为完整表达单元保留。
func safeExpressionBoundary(left, right string, boundary rune) bool {
	if strings.Count(left, "\x60")%2 != 0 ||
		strings.Count(left, "[") != strings.Count(left, "]") ||
		strings.Count(left, "(") != strings.Count(left, ")") {
		return false
	}
	for _, line := range strings.Split(left+"\n"+right, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") || strings.HasSuffix(trimmed, "|") {
			return false
		}
	}
	// 一个列表项内部的句号不能把项目标题和正文拆开；换行本身是完整列表项的
	// 安全边界，句号等标点则继续向后寻找，避免把“1.”和内容分到两条消息。
	if boundary != '\n' && expressionListLine(left) {
		return false
	}
	return true
}

func expressionListLine(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return false
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	return strings.HasPrefix(last, "- ") || strings.HasPrefix(last, "* ") ||
		strings.HasPrefix(last, "+ ") || isNumberedExpressionLine(last)
}

func isNumberedExpressionLine(line string) bool {
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	return index > 0 && index+1 < len(line) &&
		(line[index] == '.' || line[index] == ')') && line[index+1] == ' '
}

func mergeTinyExpressionParts(parts []string) []string {
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(result) > 0 && utf8.RuneCountInString(part) < 24 {
			result[len(result)-1] += "\n\n" + part
			continue
		}
		result = append(result, part)
	}
	return result
}

// reserveProactiveDelivery 先执行确定性的来源策略，再优先使用持久仓储领取
// 冷却和小时额度；没有仓储的嵌入式调用才退回有界内存窗口。
func (m *Manager) reserveProactiveDelivery(ctx context.Context, message Message, settings ExtensionConfig) (time.Time, bool, error) {
	if !settings.ProactiveEnabled || !settings.FollowUpEnabled {
		slog.Info("Follow-up 主动消息被关闭", "source", messageSource(message))
		return time.Time{}, false, nil
	}
	degree := strings.ToLower(strings.TrimSpace(settings.ProactiveDegree))
	if degree == "off" {
		slog.Info("Follow-up 主动消息被主动程度配置关闭", "source", messageSource(message))
		return time.Time{}, false, nil
	}
	if !settings.EmergencyBypassQuietHours && runtimeInQuietHours(settings, time.Now()) {
		slog.Info("主动消息处于安静时段", "source", messageSource(message), "timezone", settings.QuietHoursTimezone)
		return time.Time{}, false, nil
	}
	if len(settings.FollowUpAllowedSources) > 0 && !containsString(settings.FollowUpAllowedSources, messageSource(message)) {
		slog.Info("主动消息来源不在白名单", "source", messageSource(message))
		return time.Time{}, false, nil
	}
	limit := settings.PrivateHourlyReplyLimit
	if isGroupChat(message.ChatType) {
		limit = settings.GroupHourlyReplyLimit
	}
	limit = proactiveHourlyLimit(limit, degree)
	cooldown := proactiveCooldown(settings.CooldownSeconds, degree)
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if quota, ok := repository.(RuntimeProactiveQuotaRepository); ok {
		reservedAt, allowed, err := quota.ReserveProactiveDelivery(ctx, message.AdapterID, messageSource(message), message.ChatType, limit, cooldown, time.Now().UTC())
		if err != nil {
			return time.Time{}, false, err
		}
		if !allowed {
			slog.Info("主动消息未通过持久化冷却或额度检查", "source", messageSource(message), "limit", limit, "cooldown_seconds", int(cooldown/time.Second))
		}
		return reservedAt, allowed, nil
	}
	key := messageSource(message)
	now := time.Now()
	m.mu.Lock()
	last := m.proactiveLast[key]
	if cooldown > 0 && !last.IsZero() && now.Sub(last) < cooldown {
		m.mu.Unlock()
		slog.Info("主动消息处于冷却期", "source", key, "cooldown_seconds", int(cooldown/time.Second))
		return time.Time{}, false, nil
	}
	windowKey := "proactive:" + key
	entries := m.rateWindows[windowKey]
	kept := entries[:0]
	for _, item := range entries {
		if now.Sub(item) < time.Hour {
			kept = append(kept, item)
		}
	}
	if limit > 0 && len(kept) >= limit {
		m.rateWindows[windowKey] = kept
		m.mu.Unlock()
		slog.Info("主动消息达到小时上限", "source", key, "limit", limit)
		return time.Time{}, false, nil
	}
	m.rateWindows[windowKey] = append(kept, now)
	m.proactiveLast[key] = now
	// 主动窗口最多保留 512 个键，避免陌生 UMO 无限增长。
	if len(m.rateWindows) > 512 {
		for candidate := range m.rateWindows {
			if strings.HasPrefix(candidate, "proactive:") && candidate != windowKey {
				delete(m.rateWindows, candidate)
				break
			}
		}
	}
	m.mu.Unlock()
	return now, true, nil
}

// rollbackProactiveDelivery 撤销尚未产生任何平台消息的主动投递预留。
// 只匹配本次精确时间戳，避免并发来源互相回滚额度。
func (m *Manager) rollbackProactiveDelivery(ctx context.Context, message Message, reservedAt time.Time) {
	if reservedAt.IsZero() {
		return
	}
	m.mu.RLock()
	repository := m.runtimeState
	m.mu.RUnlock()
	if quota, ok := repository.(RuntimeProactiveQuotaRepository); ok {
		if err := quota.RollbackProactiveDelivery(ctx, message.AdapterID, messageSource(message), reservedAt); err != nil {
			slog.Warn("回滚主动投递额度失败", "source", messageSource(message), "error", err)
		}
		return
	}
	key := messageSource(message)
	windowKey := "proactive:" + key
	m.mu.Lock()
	entries := m.rateWindows[windowKey]
	kept := entries[:0]
	removed := false
	for _, item := range entries {
		if !removed && item.Equal(reservedAt) {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	if removed {
		m.rateWindows[windowKey] = kept
	}
	if last := m.proactiveLast[key]; last.Equal(reservedAt) {
		delete(m.proactiveLast, key)
	}
	m.mu.Unlock()
}

// proactiveCooldown 和 proactiveHourlyLimit 把主动程度映射为确定性的限流强度；
// 不使用随机数决定是否发言，便于审计、恢复和资源预算验证。
func proactiveCooldown(seconds int, degree string) time.Duration {
	if seconds <= 0 {
		return 0
	}
	multiplier := 1
	switch degree {
	case "low":
		multiplier = 2
	case "high":
		multiplier = 1
	}
	return time.Duration(seconds*multiplier) * time.Second
}

func proactiveHourlyLimit(limit int, degree string) int {
	if limit <= 0 || degree != "high" {
		return limit
	}
	if limit > int(^uint(0)>>1)/2 {
		return limit
	}
	return limit * 2
}

// runtimeInQuietHours 用机器人级时区判断当前是否处于安静窗口；支持跨午夜区间，
// 解析失败时不擅自拦截消息，而是保留日志让配置校验层提示管理员修正。
func runtimeInQuietHours(settings ExtensionConfig, now time.Time) bool {
	start, startOK := parseRuntimeClock(settings.QuietHoursStart)
	end, endOK := parseRuntimeClock(settings.QuietHoursEnd)
	if !startOK || !endOK || settings.QuietHoursStart == settings.QuietHoursEnd {
		return false
	}
	location := time.Local
	if name := strings.TrimSpace(settings.QuietHoursTimezone); name != "" {
		if loaded, err := time.LoadLocation(name); err == nil {
			location = loaded
		}
	}
	current := now.In(location)
	minute := current.Hour()*60 + current.Minute()
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

func parseRuntimeClock(value string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func containsString(items []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, item := range items {
		if strings.TrimSpace(item) == target {
			return true
		}
	}
	return false
}
