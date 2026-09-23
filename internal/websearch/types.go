package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	ProviderTavily     = "tavily"
	ProviderLangSearch = "langsearch"
	MaxSearchResults   = 10
	MaxQueryBytes      = 1000
	MaxSearchTextChars = 4000
)

var (
	ErrInvalidRequest = errors.New("网页搜索请求无效")
	ErrNotFound       = errors.New("网页搜索服务不存在")
	ErrNoServices     = errors.New("没有可用的网页搜索服务")
	ErrDailyLimit     = errors.New("已达到网页搜索每日请求上限")
	ErrTaskLimit      = errors.New("当前任务已达到网页搜索调用上限")
)

// Service 保存一把 API 密钥。AccountGroup 用于归并共享服务商额度的凭据，标签由用户填写且不包含凭据。
type Service struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Provider     string    `json:"provider"`
	AccountGroup string    `json:"account_group"`
	APIKey       string    `json:"-"`
	Enabled      bool      `json:"enabled"`
	Priority     int       `json:"priority"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ServiceView 是安全的管理接口数据结构，不会向客户端返回 APIKey。
type ServiceView struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Provider         string    `json:"provider"`
	AccountGroup     string    `json:"account_group"`
	APIKeyConfigured bool      `json:"api_key_configured"`
	Enabled          bool      `json:"enabled"`
	Priority         int       `json:"priority"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (item Service) Public() ServiceView {
	return ServiceView{
		ID: item.ID, Name: item.Name, Provider: item.Provider,
		AccountGroup: item.AccountGroup, APIKeyConfigured: strings.TrimSpace(item.APIKey) != "",
		Enabled: item.Enabled, Priority: item.Priority, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

type ServiceInput struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Provider     string  `json:"provider"`
	AccountGroup string  `json:"account_group"`
	APIKey       *string `json:"api_key"`
	Enabled      bool    `json:"enabled"`
	Priority     int     `json:"priority"`
}

func (input ServiceInput) Normalize() (ServiceInput, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	input.AccountGroup = strings.TrimSpace(input.AccountGroup)
	if input.Name == "" || len(input.Name) > 100 {
		return ServiceInput{}, fmt.Errorf("%w: 服务名称不能为空且不能超过 100 个字符", ErrInvalidRequest)
	}
	if input.Provider != ProviderTavily && input.Provider != ProviderLangSearch {
		return ServiceInput{}, fmt.Errorf("%w: 仅支持 Tavily 和 LangSearch", ErrInvalidRequest)
	}
	if len(input.AccountGroup) > 100 {
		return ServiceInput{}, fmt.Errorf("%w: 额度组名称不能超过 100 个字符", ErrInvalidRequest)
	}
	if input.Priority < 0 || input.Priority > 10000 {
		return ServiceInput{}, fmt.Errorf("%w: 优先级必须在 0-10000 之间", ErrInvalidRequest)
	}
	if input.APIKey != nil {
		key := strings.TrimSpace(*input.APIKey)
		if len(key) > 4000 {
			return ServiceInput{}, fmt.Errorf("%w: API Key 过长", ErrInvalidRequest)
		}
		input.APIKey = &key
	}
	return input, nil
}

type SearchRequest struct {
	Query          string   `json:"query"`
	MaxResults     int      `json:"max_results"`
	Freshness      string   `json:"freshness,omitempty"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
	IncludeText    bool     `json:"include_text,omitempty"`
	MaxTextChars   int      `json:"max_text_chars,omitempty"`
}

func (request SearchRequest) Normalize() (SearchRequest, error) {
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" || len([]byte(request.Query)) > MaxQueryBytes {
		return SearchRequest{}, fmt.Errorf("%w: query 不能为空且不能超过 %d 字节", ErrInvalidRequest, MaxQueryBytes)
	}
	if request.MaxResults == 0 {
		request.MaxResults = 5
	}
	if request.MaxResults < 1 || request.MaxResults > MaxSearchResults {
		return SearchRequest{}, fmt.Errorf("%w: max_results 必须在 1-%d 之间", ErrInvalidRequest, MaxSearchResults)
	}
	request.Freshness = strings.TrimSpace(request.Freshness)
	switch request.Freshness {
	case "", "noLimit", "oneDay", "oneWeek", "oneMonth", "oneYear":
	default:
		return SearchRequest{}, fmt.Errorf("%w: freshness 必须是 noLimit、oneDay、oneWeek、oneMonth 或 oneYear", ErrInvalidRequest)
	}
	request.IncludeDomains = normalizeDomains(request.IncludeDomains)
	request.ExcludeDomains = normalizeDomains(request.ExcludeDomains)
	if len(request.IncludeDomains) > 20 || len(request.ExcludeDomains) > 20 {
		return SearchRequest{}, fmt.Errorf("%w: 每类域名过滤最多 20 项", ErrInvalidRequest)
	}
	if request.MaxTextChars == 0 {
		request.MaxTextChars = 2000
	}
	if request.MaxTextChars < 200 || request.MaxTextChars > MaxSearchTextChars {
		return SearchRequest{}, fmt.Errorf("%w: max_text_chars 必须在 200-%d 之间", ErrInvalidRequest, MaxSearchTextChars)
	}
	return request, nil
}

func normalizeDomains(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		value = strings.TrimPrefix(value, "https://")
		value = strings.TrimPrefix(value, "http://")
		value = strings.TrimSuffix(value, "/")
		if value == "" || len(value) > 253 || strings.ContainsAny(value, "/?#@ ") {
			continue
		}
		parsed, err := url.Parse("https://" + value)
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

type Result struct {
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet,omitempty"`
	Text        string  `json:"text,omitempty"`
	Score       float64 `json:"score,omitempty"`
	PublishedAt string  `json:"published_at,omitempty"`
}

type Usage struct {
	Credits      int64 `json:"credits"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	Known        bool  `json:"known"`
}

type SearchResponse struct {
	Provider  string   `json:"provider"`
	ServiceID string   `json:"service_id"`
	Results   []Result `json:"results"`
	Usage     Usage    `json:"usage"`
	RequestID string   `json:"request_id,omitempty"`
	Fallbacks int      `json:"fallbacks"`
}

// UsageRecord 保存有界用量元数据，不保存 API 密钥或网页正文。
type UsageRecord struct {
	ID             string    `json:"id"`
	InvocationID   string    `json:"invocation_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	ServiceID      string    `json:"service_id"`
	ServiceName    string    `json:"service_name"`
	Provider       string    `json:"provider"`
	AccountGroup   string    `json:"account_group"`
	Query          string    `json:"query"`
	Status         string    `json:"status"`
	Usage          Usage     `json:"usage"`
	ResultCount    int       `json:"result_count"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	RequestID      string    `json:"request_id,omitempty"`
	Error          string    `json:"error,omitempty"`
	DurationMS     int64     `json:"duration_ms"`
	CreatedAt      time.Time `json:"created_at"`
}

type UsageSummary struct {
	PeriodStart    time.Time      `json:"period_start"`
	PeriodEnd      time.Time      `json:"period_end"`
	DailyCalls     int64          `json:"daily_calls"`
	DailyCallLimit int64          `json:"daily_call_limit"`
	AlertPercent   int            `json:"alert_percent"`
	Alert          bool           `json:"alert"`
	ByAccount      []AccountUsage `json:"by_account"`
	ByService      []ServiceUsage `json:"by_service"`
	Recent         []UsageRecord  `json:"recent"`
}

type ServiceUsage struct {
	ServiceID    string `json:"service_id"`
	ServiceName  string `json:"service_name"`
	Provider     string `json:"provider"`
	AccountGroup string `json:"account_group"`
	Calls        int64  `json:"calls"`
	Successes    int64  `json:"successes"`
	Failures     int64  `json:"failures"`
	Credits      int64  `json:"credits"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	UnknownUsage int64  `json:"unknown_usage"`
}

type AccountUsage struct {
	Provider     string `json:"provider"`
	AccountGroup string `json:"account_group"`
	Calls        int64  `json:"calls"`
	Successes    int64  `json:"successes"`
	Failures     int64  `json:"failures"`
	Credits      int64  `json:"credits"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	UnknownUsage int64  `json:"unknown_usage"`
}

type UsageAggregate struct {
	DailyCalls int64
	ByService  []ServiceUsage
}

// UsageAggregateRepository 供持久化存储直接计算精确汇总，避免将全部用量记录载入内存。
type UsageAggregateRepository interface {
	AggregateUsage(context.Context, time.Time, time.Time) (UsageAggregate, error)
}

type Budget struct {
	DailyCallLimit        int64
	MaxCallsPerInvocation int
	AlertPercent          int
}

type Repository interface {
	ListServices(context.Context) ([]Service, error)
	GetService(context.Context, string) (Service, error)
	SaveService(context.Context, Service) error
	DeleteService(context.Context, string) error
	ReserveUsage(context.Context, UsageRecord, Budget) (int64, error)
	CompleteUsage(context.Context, string, UsageRecord) error
	ListUsage(context.Context, time.Time, int) ([]UsageRecord, error)
}

func orderedServices(services []Service, preferredIDs []string) []Service {
	byID := make(map[string]Service, len(services))
	for _, item := range services {
		byID[item.ID] = item
	}
	ordered := make([]Service, 0, len(services))
	seen := make(map[string]struct{}, len(services))
	for _, id := range preferredIDs {
		id = strings.TrimSpace(id)
		item, ok := byID[id]
		if !ok || !item.Enabled {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ordered = append(ordered, item)
	}
	if len(preferredIDs) == 0 {
		for _, item := range services {
			if item.Enabled {
				ordered = append(ordered, item)
			}
		}
		sort.SliceStable(ordered, func(i, j int) bool {
			if ordered[i].Priority != ordered[j].Priority {
				return ordered[i].Priority < ordered[j].Priority
			}
			return ordered[i].ID < ordered[j].ID
		})
	}
	return ordered
}
