// Package logging 提供内存中的有界日志快照和实时订阅，供管理台按需查看。
package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Entry 是日志管理台公开的内存快照；属性保留原始文本，便于还原完整请求和响应。
type Entry struct {
	Time       time.Time         `json:"time"`
	Level      string            `json:"level"`
	Message    string            `json:"message"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Store 保存最近的有限条数日志，进程重启后自动清空，不承担审计存储职责。
type Store struct {
	mu             sync.RWMutex
	max            int
	entries        []Entry
	nextSubscriber uint64
	subscribers    map[uint64]*subscriber
}

// subscriber 是日志流的单个订阅者；有界 channel 防止浏览器断开或网络变慢时反过来阻塞日志写入。
type subscriber struct {
	level  string
	events chan Entry
}

// NewStore 创建日志环形缓冲区。
func NewStore(max int) *Store {
	if max < 100 {
		max = 1000
	}
	return &Store{max: max, entries: make([]Entry, 0, max), subscribers: make(map[uint64]*subscriber)}
}

// Add 记录一条完整日志；结构化属性统一转成文本，确保请求和响应正文不会被丢弃。
func (s *Store) Add(record slog.Record) {
	s.add(record)
}

// add 记录日志并合并 Logger.With 预绑定的属性，避免管理台快照漏掉上下文信息。
func (s *Store) add(record slog.Record, bound ...slog.Attr) {
	if s == nil {
		return
	}
	entry := Entry{Time: record.Time.UTC(), Level: record.Level.String(), Message: record.Message, Attributes: map[string]string{}}
	for _, attr := range bound {
		entry.Attributes[attr.Key] = attributeText(attr.Value)
	}
	record.Attrs(func(attr slog.Attr) bool {
		entry.Attributes[attr.Key] = attributeText(attr.Value)
		return true
	})
	if len(entry.Attributes) == 0 {
		entry.Attributes = nil
	}
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	if excess := len(s.entries) - s.max; excess > 0 {
		s.entries = append([]Entry(nil), s.entries[excess:]...)
	}
	// 日志流只投递最新事件且不阻塞生产者；慢客户端会丢弃最旧的待发送事件，重新打开页面时会重新获取快照。
	for _, item := range s.subscribers {
		if item.level != "" && item.level != "ALL" && strings.ToUpper(entry.Level) != item.level {
			continue
		}
		select {
		case item.events <- entry:
		default:
			select {
			case <-item.events:
			default:
			}
			select {
			case item.events <- entry:
			default:
			}
		}
	}
	s.mu.Unlock()
}

// List 返回按时间倒序的日志快照。
func (s *Store) List(level string, limit int) []Entry {
	if s == nil {
		return []Entry{}
	}
	level = strings.ToUpper(strings.TrimSpace(level))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocked(level, limit)
}

// Subscribe 返回打开日志页时的快照和之后的实时事件；快照与订阅注册在同一把锁内完成，避免漏掉临界日志。
func (s *Store) Subscribe(level string, limit int) ([]Entry, <-chan Entry, func()) {
	if s == nil {
		events := make(chan Entry)
		close(events)
		return []Entry{}, events, func() {}
	}
	level = strings.ToUpper(strings.TrimSpace(level))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	item := &subscriber{level: level, events: make(chan Entry, 128)}
	s.mu.Lock()
	s.nextSubscriber++
	id := s.nextSubscriber
	initial := s.listLocked(level, limit)
	if s.subscribers == nil {
		s.subscribers = make(map[uint64]*subscriber)
	}
	s.subscribers[id] = item
	s.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if current, ok := s.subscribers[id]; ok && current == item {
				delete(s.subscribers, id)
				close(item.events)
			}
			s.mu.Unlock()
		})
	}
	return initial, item.events, cancel
}

func (s *Store) listLocked(level string, limit int) []Entry {
	result := make([]Entry, 0, limit)
	for index := len(s.entries) - 1; index >= 0 && len(result) < limit; index-- {
		entry := s.entries[index]
		if level != "" && level != "ALL" && strings.ToUpper(entry.Level) != level {
			continue
		}
		if entry.Attributes != nil {
			entry.Attributes = cloneAttributes(entry.Attributes)
		}
		result = append(result, entry)
	}
	return result
}

// Handler 将日志复制到 Store 后继续交给真正的终端 Handler。
type Handler struct {
	store *Store
	next  slog.Handler
	bound []slog.Attr
}

// NewHandler 创建带内存快照的 slog Handler。
func NewHandler(store *Store, next slog.Handler) slog.Handler {
	return &Handler{store: store, next: next}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.next == nil {
		return true
	}
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	if h.store != nil {
		h.store.add(record, h.bound...)
	}
	if h.next == nil {
		return nil
	}
	return h.next.Handle(ctx, record)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// 保留预绑定属性，让管理台快照与终端 Handler 展示相同的上下文。
	bound := make([]slog.Attr, 0, len(h.bound)+len(attrs))
	bound = append(bound, h.bound...)
	bound = append(bound, attrs...)
	result := &Handler{store: h.store, bound: bound}
	if h.next != nil {
		result.next = h.next.WithAttrs(attrs)
	}
	return result
}

func (h *Handler) WithGroup(name string) slog.Handler {
	result := &Handler{store: h.store, bound: append([]slog.Attr(nil), h.bound...)}
	if h.next != nil {
		result.next = h.next.WithGroup(name)
	}
	return result
}

// attributeText 保留 slog 属性的原始可读值；这里不能按字段名过滤或替换内容。
func attributeText(value slog.Value) string {
	value = value.Resolve()
	switch value.Kind() {
	case slog.KindString, slog.KindBool, slog.KindInt64, slog.KindUint64, slog.KindFloat64:
		return value.String()
	case slog.KindTime:
		return value.Time().Format(time.RFC3339Nano)
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindGroup:
		if encoded, err := json.Marshal(groupJSONValue(value.Group())); err == nil {
			return string(encoded)
		}
	}
	if item := value.Any(); item != nil {
		if err, ok := item.(error); ok {
			return err.Error()
		}
		if encoded, err := json.Marshal(item); err == nil {
			return string(encoded)
		}
		return fmt.Sprint(item)
	}
	return value.String()
}

// groupJSONValue 将 slog 分组属性展开成普通 JSON 对象，避免直接序列化 slog.Value 丢失具体值。
func groupJSONValue(attrs []slog.Attr) map[string]any {
	result := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		result[attr.Key] = slogJSONValue(attr.Value)
	}
	return result
}

// slogJSONValue 保留分组、时间、数值和任意对象的原始可序列化形态。
func slogJSONValue(value slog.Value) any {
	value = value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindBool:
		return value.Bool()
	case slog.KindInt64:
		return value.Int64()
	case slog.KindUint64:
		return value.Uint64()
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindTime:
		return value.Time()
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindGroup:
		return groupJSONValue(value.Group())
	case slog.KindAny:
		if item := value.Any(); item != nil {
			if err, ok := item.(error); ok {
				return err.Error()
			}
			return item
		}
	}
	return value.String()
}

func cloneAttributes(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
