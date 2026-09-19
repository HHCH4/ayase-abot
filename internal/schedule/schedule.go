// Package schedule 提供持久化未来任务和一个轻量级本地调度循环。
package schedule

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode 是任务的执行周期；仅支持内置 Agent，不包含外部编排平台执行方式。
type Mode string

const (
	ModeOnce     Mode = "once"
	ModeInterval Mode = "interval"
	ModeDaily    Mode = "daily"
	ModeWeekly   Mode = "weekly"
	ModeMonthly  Mode = "monthly"
	ModeCustom   Mode = "custom"
)

// Status 是未来任务的生命周期状态。
type Status string

const (
	StatusActive    Status = "active"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
)

var (
	// ErrNotFound 表示任务不存在。
	ErrNotFound = errors.New("未来任务不存在")
	// ErrInvalidRequest 表示任务参数无效。
	ErrInvalidRequest = errors.New("未来任务请求无效")
)

// Task 是未来任务的公开领域对象。时间统一保存为 UTC，前端负责展示本地时间。
type Task struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Request          string     `json:"request"`
	Mode             Mode       `json:"mode"`
	StartAt          *time.Time `json:"start_at,omitempty"`
	IntervalSeconds  int        `json:"interval_seconds,omitempty"`
	Weekday          int        `json:"weekday,omitempty"`
	MonthDay         int        `json:"month_day,omitempty"`
	TimeOfDay        string     `json:"time_of_day,omitempty"`
	Cron             string     `json:"cron,omitempty"`
	UserID           string     `json:"user_id"`
	ConversationID   string     `json:"conversation_id,omitempty"`
	AdapterID        string     `json:"adapter_id,omitempty"`
	ChatID           string     `json:"chat_id,omitempty"`
	Status           Status     `json:"status"`
	NextRunAt        *time.Time `json:"next_run_at,omitempty"`
	LastRunAt        *time.Time `json:"last_run_at,omitempty"`
	LastInvocationID string     `json:"last_invocation_id,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	Running          bool       `json:"running"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// Repository 是调度服务的最小持久化边界。
type Repository interface {
	List(context.Context, Status) ([]Task, error)
	Get(context.Context, string) (Task, error)
	Save(context.Context, Task) error
	Delete(context.Context, string) error
	ClaimDue(context.Context, time.Time, int) ([]Task, error)
	UpdateRun(context.Context, string, bool, string, string, *time.Time, *time.Time, Status) error
	SetStatus(context.Context, string, Status) error
}

// Executor 在任务到期后启动一次内置 Agent，并返回 invocation ID。
type Executor func(context.Context, Task) (string, error)

// Service 负责任务校验、下一次触发时间计算和后台调度。
type Service struct {
	repository Repository
	mu         sync.Mutex
}

// NewService 创建调度服务。
func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, errors.New("调度 Repository 不能为空")
	}
	return &Service{repository: repository}, nil
}

// List 返回任务列表；status 为空时包含全部状态。
func (s *Service) List(ctx context.Context, status Status) ([]Task, error) {
	return s.repository.List(ctx, status)
}

// Get 返回一个任务。
func (s *Service) Get(ctx context.Context, id string) (Task, error) {
	if strings.TrimSpace(id) == "" {
		return Task{}, fmt.Errorf("%w: 任务 ID 不能为空", ErrInvalidRequest)
	}
	return s.repository.Get(ctx, strings.TrimSpace(id))
}

// Save 新增或更新任务，并根据周期计算下一次触发时间。
func (s *Service) Save(ctx context.Context, item Task) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item = normalizeTask(item)
	if err := validateTask(item); err != nil {
		return Task{}, err
	}
	old, err := s.repository.Get(ctx, item.ID)
	if item.ID == "" {
		item.ID = newID("schedule")
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return Task{}, err
	}
	now := time.Now().UTC()
	if err == nil {
		item.CreatedAt = old.CreatedAt
		if item.NextRunAt == nil && old.NextRunAt != nil {
			value := *old.NextRunAt
			item.NextRunAt = &value
		}
		if item.Status == "" {
			item.Status = old.Status
		}
	} else {
		item.CreatedAt = now
	}
	if item.Status == "" {
		item.Status = StatusActive
	}
	if item.NextRunAt == nil || item.Status == StatusActive && item.NextRunAt.Before(now.Add(-time.Minute)) && item.LastRunAt == nil {
		next, nextErr := NextRun(item, now)
		if nextErr != nil {
			return Task{}, nextErr
		}
		item.NextRunAt = next
	}
	item.UpdatedAt = now
	if err := s.repository.Save(ctx, item); err != nil {
		return Task{}, fmt.Errorf("保存未来任务失败: %w", err)
	}
	return item, nil
}

// Delete 删除任务。
func (s *Service) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: 任务 ID 不能为空", ErrInvalidRequest)
	}
	return s.repository.Delete(ctx, strings.TrimSpace(id))
}

// SetStatus 暂停、恢复或完成任务；恢复时会重新计算下一次触发时间。
func (s *Service) SetStatus(ctx context.Context, id string, status Status) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if status != StatusActive && status != StatusPaused && status != StatusCompleted {
		return Task{}, fmt.Errorf("%w: 不支持的任务状态", ErrInvalidRequest)
	}
	item.Status = status
	item.Running = false
	if status == StatusActive {
		next, nextErr := NextRun(item, time.Now().UTC())
		if nextErr != nil {
			return Task{}, nextErr
		}
		item.NextRunAt = next
	}
	item.UpdatedAt = time.Now().UTC()
	if err := s.repository.Save(ctx, item); err != nil {
		return Task{}, err
	}
	return item, nil
}

// Start 启动后台调度循环；任务执行通过 Executor 接入内置 Agent。
func (s *Service) Start(ctx context.Context, executor Executor) {
	if executor == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			s.dispatch(ctx, executor)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// dispatch 只领取到期任务，具体模型调用在独立 goroutine 中执行，避免一个慢任务阻塞其他任务。
func (s *Service) dispatch(ctx context.Context, executor Executor) {
	items, err := s.repository.ClaimDue(ctx, time.Now().UTC(), 20)
	if err != nil {
		return
	}
	for _, item := range items {
		go func(task Task) {
			invocationID, executeErr := executor(ctx, task)
			finishAt := time.Now().UTC()
			next := (*time.Time)(nil)
			status := StatusActive
			if task.Mode == ModeOnce {
				status = StatusCompleted
			} else if executeErr == nil {
				if calculated, nextErr := NextRun(task, finishAt); nextErr == nil {
					next = calculated
				} else {
					executeErr = nextErr
				}
			}
			errText := ""
			if executeErr != nil {
				errText = executeErr.Error()
			}
			_ = s.repository.UpdateRun(context.WithoutCancel(ctx), task.ID, false, invocationID, errText, &finishAt, next, status)
		}(item)
	}
}

// NextRun 根据任务周期计算严格晚于 base 的下一次触发时间。
func NextRun(item Task, base time.Time) (*time.Time, error) {
	base = base.UTC()
	switch item.Mode {
	case ModeOnce:
		if item.StartAt == nil {
			return nil, fmt.Errorf("%w: 一次性任务需要指定时间", ErrInvalidRequest)
		}
		value := item.StartAt.UTC()
		return &value, nil
	case ModeInterval:
		if item.IntervalSeconds < 1 {
			return nil, fmt.Errorf("%w: 间隔必须大于 0", ErrInvalidRequest)
		}
		value := base.Add(time.Duration(item.IntervalSeconds) * time.Second)
		return &value, nil
	case ModeDaily, ModeWeekly, ModeMonthly:
		return nextCalendarRun(item, base)
	case ModeCustom:
		value, err := nextCron(base, item.Cron)
		if err != nil {
			return nil, err
		}
		return &value, nil
	default:
		return nil, fmt.Errorf("%w: 不支持的执行方式 %q", ErrInvalidRequest, item.Mode)
	}
}

func normalizeTask(item Task) Task {
	item.ID = strings.TrimSpace(item.ID)
	item.Name = strings.TrimSpace(item.Name)
	item.Request = strings.TrimSpace(item.Request)
	item.UserID = strings.TrimSpace(item.UserID)
	item.ConversationID = strings.TrimSpace(item.ConversationID)
	item.AdapterID = strings.TrimSpace(item.AdapterID)
	item.ChatID = strings.TrimSpace(item.ChatID)
	item.TimeOfDay = strings.TrimSpace(item.TimeOfDay)
	item.Cron = strings.TrimSpace(item.Cron)
	if item.UserID == "" {
		// 没有指定用户时使用专用调度身份，保证任务不会落入空用户会话。
		item.UserID = "scheduler"
	}
	return item
}

func validateTask(item Task) error {
	if item.Name == "" || len(item.Name) > 200 {
		return fmt.Errorf("%w: 任务名称不能为空且不能超过 200 个字符", ErrInvalidRequest)
	}
	if item.Request == "" || len(item.Request) > 10000 {
		return fmt.Errorf("%w: 任务需求不能为空且不能超过 10000 个字符", ErrInvalidRequest)
	}
	if item.Mode != ModeOnce && item.Mode != ModeInterval && item.Mode != ModeDaily && item.Mode != ModeWeekly && item.Mode != ModeMonthly && item.Mode != ModeCustom {
		return fmt.Errorf("%w: 执行方式无效", ErrInvalidRequest)
	}
	if item.Mode == ModeInterval && item.IntervalSeconds < 1 {
		return fmt.Errorf("%w: 间隔必须大于 0", ErrInvalidRequest)
	}
	if item.Mode == ModeCustom {
		if _, err := nextCron(time.Now().UTC(), item.Cron); err != nil {
			return err
		}
	}
	if item.Mode == ModeWeekly && (item.Weekday < 0 || item.Weekday > 6) {
		return fmt.Errorf("%w: 星期必须在 0-6 之间", ErrInvalidRequest)
	}
	if item.Mode == ModeMonthly && (item.MonthDay < 1 || item.MonthDay > 31) {
		return fmt.Errorf("%w: 月内日期必须在 1-31 之间", ErrInvalidRequest)
	}
	if item.Mode == ModeDaily || item.Mode == ModeWeekly || item.Mode == ModeMonthly {
		if _, _, err := parseTimeOfDay(item.TimeOfDay); err != nil {
			return err
		}
	}
	return nil
}

func nextCalendarRun(item Task, base time.Time) (*time.Time, error) {
	hour, minute, err := parseTimeOfDay(item.TimeOfDay)
	if err != nil {
		return nil, err
	}
	for day := 0; day < 370; day++ {
		candidateDay := base.AddDate(0, 0, day)
		candidate := time.Date(candidateDay.Year(), candidateDay.Month(), candidateDay.Day(), hour, minute, 0, 0, time.UTC)
		if !candidate.After(base) {
			continue
		}
		switch item.Mode {
		case ModeDaily:
		case ModeWeekly:
			if int(candidate.Weekday()) != item.Weekday {
				continue
			}
		case ModeMonthly:
			if candidate.Day() != item.MonthDay {
				continue
			}
		}
		return &candidate, nil
	}
	return nil, fmt.Errorf("%w: 找不到下一次日历触发时间", ErrInvalidRequest)
}

func parseTimeOfDay(value string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%w: 时间必须是 HH:MM", ErrInvalidRequest)
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%w: 时间必须是合法的 HH:MM", ErrInvalidRequest)
	}
	return hour, minute, nil
}

// nextCron 支持常见五段式 Cron 的数字、*、*/n、逗号和短横线表达式。
func nextCron(base time.Time, expression string) (time.Time, error) {
	parts := strings.Fields(strings.TrimSpace(expression))
	if len(parts) != 5 {
		return time.Time{}, fmt.Errorf("%w: 自定义 Cron 必须是五段表达式", ErrInvalidRequest)
	}
	sets := make([]map[int]bool, 5)
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i, part := range parts {
		parsed, err := parseCronPart(part, ranges[i][0], ranges[i][1])
		if err != nil {
			return time.Time{}, err
		}
		sets[i] = parsed
	}
	start := base.Truncate(time.Minute).Add(time.Minute)
	for offset := 0; offset <= 366*24*60; offset++ {
		candidate := start.Add(time.Duration(offset) * time.Minute)
		if sets[0][candidate.Minute()] && sets[1][candidate.Hour()] && sets[2][candidate.Day()] && sets[3][int(candidate.Month())] && sets[4][int(candidate.Weekday())] {
			return candidate, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: 一年内找不到 Cron 触发时间", ErrInvalidRequest)
}

func parseCronPart(value string, min, max int) (map[int]bool, error) {
	result := make(map[int]bool)
	for _, segment := range strings.Split(strings.TrimSpace(value), ",") {
		step := 1
		if strings.Contains(segment, "/") {
			pieces := strings.Split(segment, "/")
			if len(pieces) != 2 {
				return nil, fmt.Errorf("%w: Cron 步长无效", ErrInvalidRequest)
			}
			parsed, err := strconv.Atoi(pieces[1])
			if err != nil || parsed < 1 {
				return nil, fmt.Errorf("%w: Cron 步长无效", ErrInvalidRequest)
			}
			step, segment = parsed, pieces[0]
		}
		start, end := min, max
		if segment != "*" {
			if strings.Contains(segment, "-") {
				pieces := strings.Split(segment, "-")
				if len(pieces) != 2 {
					return nil, fmt.Errorf("%w: Cron 范围无效", ErrInvalidRequest)
				}
				var err error
				start, err = strconv.Atoi(pieces[0])
				if err != nil {
					return nil, fmt.Errorf("%w: Cron 数字无效", ErrInvalidRequest)
				}
				end, err = strconv.Atoi(pieces[1])
				if err != nil {
					return nil, fmt.Errorf("%w: Cron 数字无效", ErrInvalidRequest)
				}
			} else {
				value, err := strconv.Atoi(segment)
				if err != nil {
					return nil, fmt.Errorf("%w: Cron 数字无效", ErrInvalidRequest)
				}
				start, end = value, value
			}
		}
		if start < min || end > max || start > end {
			return nil, fmt.Errorf("%w: Cron 数字超出范围", ErrInvalidRequest)
		}
		for current := start; current <= end; current += step {
			result[current] = true
		}
	}
	return result, nil
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
}
