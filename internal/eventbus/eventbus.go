// Package eventbus 提供 Abot 内核内部的轻量事件总线。
package eventbus

import (
	"context"
	"errors"
	"sync"
)

const (
	// MessageReceived 是平台适配器收到用户消息后发布的事件。
	MessageReceived = "message.received"
)

// Event 是插件和内置模块之间传递的事件。Payload 的具体类型由事件名称约定。
type Event struct {
	Name    string
	Payload any
}

// Handler 是事件订阅者。返回错误不会中断其他订阅者，Publish 会汇总错误。
type Handler func(context.Context, Event) error

// Bus 是线程安全的进程内事件总线。
type Bus struct {
	mu       sync.RWMutex
	handlers map[string][]Handler
}

// New 创建空事件总线。
func New() *Bus {
	return &Bus{handlers: make(map[string][]Handler)}
}

// Subscribe 订阅指定事件，并返回取消订阅函数。
func (b *Bus) Subscribe(name string, handler Handler) (func(), error) {
	if b == nil {
		return nil, errors.New("事件总线不能为空")
	}
	if name == "" {
		return nil, errors.New("事件名称不能为空")
	}
	if handler == nil {
		return nil, errors.New("事件处理器不能为空")
	}
	b.mu.Lock()
	b.handlers[name] = append(b.handlers[name], handler)
	index := len(b.handlers[name]) - 1
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		items := b.handlers[name]
		if index < 0 || index >= len(items) || items[index] == nil {
			return
		}
		items[index] = nil
	}, nil
}

// Publish 同步调用当前事件的所有订阅者；每个订阅者都能独立返回错误。
func (b *Bus) Publish(ctx context.Context, event Event) []error {
	if b == nil {
		return []error{errors.New("事件总线不能为空")}
	}
	b.mu.RLock()
	items := append([]Handler(nil), b.handlers[event.Name]...)
	b.mu.RUnlock()
	var errs []error
	for _, handler := range items {
		if handler == nil {
			continue
		}
		if err := handler(ctx, event); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
