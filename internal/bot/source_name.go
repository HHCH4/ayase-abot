package bot

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const maxSourceNameRunes = 100

// SourceNameRepository 持久化 UMO 的显示名称；空名称表示尚未设置。
type SourceNameRepository interface {
	GetSourceName(context.Context, string) (string, error)
	SetSourceName(context.Context, string, string) error
}

// SourceNameLister 是来源目录读取别名的可选扩展，用于把升级前已经保存的
// /name 别名也合并进 UMO 下拉框，即使它们没有对应的历史 Conversation。
type SourceNameLister interface {
	ListSourceNames(context.Context, string) ([]SourceName, error)
}

// SourceName 是来源键和用户手工别名的目录记录。
type SourceName struct {
	Source    string
	Name      string
	UpdatedAt time.Time
}

func (m *Manager) sourceNameRepository() (SourceNameRepository, bool) {
	if m == nil || m.repository == nil {
		return nil, false
	}
	repository, ok := m.repository.(SourceNameRepository)
	return repository, ok
}

// sourceName 先读持久化名称，再使用内存兜底，保证 /sid 在最小嵌入环境也能工作。
func (m *Manager) sourceName(ctx context.Context, source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("消息来源不能为空")
	}
	if repository, ok := m.sourceNameRepository(); ok {
		name, err := repository.GetSourceName(ctx, source)
		if err != nil {
			return "", err
		}
		name = strings.TrimSpace(name)
		m.mu.Lock()
		if m.sourceNames == nil {
			m.sourceNames = make(map[string]string)
		}
		if name == "" {
			delete(m.sourceNames, source)
		} else {
			m.sourceNames[source] = name
		}
		m.mu.Unlock()
		return name, nil
	}
	m.mu.RLock()
	name := strings.TrimSpace(m.sourceNames[source])
	m.mu.RUnlock()
	return name, nil
}

// setSourceName 校验并保存名称；持久化成功后才更新内存缓存，避免两处状态不一致。
func (m *Manager) setSourceName(ctx context.Context, source, name string) error {
	source = strings.TrimSpace(source)
	name = strings.TrimSpace(name)
	if source == "" {
		return fmt.Errorf("消息来源不能为空")
	}
	if err := validateSourceName(name); err != nil {
		return err
	}
	if repository, ok := m.sourceNameRepository(); ok {
		if err := repository.SetSourceName(ctx, source, name); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if m.sourceNames == nil {
		m.sourceNames = make(map[string]string)
	}
	if name == "" {
		delete(m.sourceNames, source)
	} else {
		m.sourceNames[source] = name
	}
	m.mu.Unlock()
	return nil
}

func validateSourceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("显示名称不能为空")
	}
	if strings.ContainsAny(name, "\x00\r\n") {
		return fmt.Errorf("显示名称不能包含控制字符")
	}
	if utf8.RuneCountInString(name) > maxSourceNameRunes {
		return fmt.Errorf("显示名称不能超过 %d 个字符", maxSourceNameRunes)
	}
	return nil
}
