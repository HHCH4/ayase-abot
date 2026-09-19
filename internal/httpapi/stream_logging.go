package httpapi

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// sseStreamLog 聚合一次 SSE 响应的日志；网络传输仍然逐事件刷新，但日志只在流结束时写入一次。
type sseStreamLog struct {
	startedAt  time.Time
	kind       string
	fields     []any
	eventCount int
	bodyBytes  int64
	message    strings.Builder
	events     []sseStreamLogEvent
	truncated  bool
}

// sseStreamLogEvent 保存未合并事件的原始 JSON 文本，确保工具、审批和终态数据仍然完整可查。
type sseStreamLogEvent struct {
	Event string `json:"event"`
	Body  string `json:"body"`
}

// newSSEStreamLog 创建一个有界的流式日志聚合器，并附带会话或 invocation 关联信息。
func newSSEStreamLog(kind string, fields ...any) *sseStreamLog {
	return &sseStreamLog{startedAt: time.Now(), kind: kind, fields: fields}
}

// record 记录一次已经编码好的 SSE 事件；message 事件只保留合并后的文本，其他事件保留原始正文。
func (s *sseStreamLog) record(event string, data []byte) {
	if s == nil {
		return
	}
	s.eventCount++
	if event == "message" {
		var payload struct {
			Delta string `json:"delta"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(data, &payload); err == nil {
			text := payload.Delta
			if text == "" {
				text = payload.Text
			}
			if text != "" {
				// 文本增量是最容易产生大量日志碎片的部分，按顺序合并成一条完整正文。
				s.appendText(text)
				return
			}
		}
	}
	// 非文本事件需要保留原始内容，方便还原工具调用、审批和最终状态。
	s.appendEvent(event, data)
}

// appendText 把文本追加到聚合正文，并使用与 HTTP 请求日志相同的 32 MiB 上限保护进程内存。
func (s *sseStreamLog) appendText(text string) {
	remaining := requestLogBodyLimit - s.bodyBytes
	if remaining <= 0 {
		s.truncated = true
		return
	}
	if int64(len(text)) <= remaining {
		s.message.WriteString(text)
		s.bodyBytes += int64(len(text))
		return
	}
	cut := int(remaining)
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	if cut > 0 {
		s.message.WriteString(text[:cut])
		s.bodyBytes += int64(cut)
	}
	s.truncated = true
}

// appendEvent 保存非文本事件，截断只影响日志副本，不影响已经发送给客户端的 SSE 正文。
func (s *sseStreamLog) appendEvent(event string, data []byte) {
	remaining := requestLogBodyLimit - s.bodyBytes
	if remaining <= 0 {
		s.truncated = true
		return
	}
	if int64(len(data)) > remaining {
		data = data[:remaining]
		s.truncated = true
	}
	s.events = append(s.events, sseStreamLogEvent{Event: event, Body: string(data)})
	s.bodyBytes += int64(len(data))
}

// finish 在流结束时写出一条聚合日志；客户端断开时也会保留已收到的部分正文。
func (s *sseStreamLog) finish(status string) {
	if s == nil || s.eventCount == 0 {
		return
	}
	if strings.TrimSpace(status) == "" {
		status = "closed"
	}
	attributes := append([]any{}, s.fields...)
	attributes = append(attributes,
		"stream", s.kind,
		"status", status,
		"event_count", s.eventCount,
		"duration", time.Since(s.startedAt),
	)
	if s.message.Len() > 0 {
		attributes = append(attributes, "text", s.message.String())
	}
	if len(s.events) > 0 {
		if encoded, err := json.Marshal(s.events); err == nil {
			attributes = append(attributes, "events", string(encoded))
		}
	}
	if s.truncated {
		attributes = append(attributes, "body_truncated", true)
	}
	// 只记录聚合结果，避免每个思考或回答增量各自产生一条日志。
	slog.Info("HTTP SSE 响应正文", attributes...)
}
