package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	tavilySearchURL        = "https://api.tavily.com/search"
	langSearchSearchURL    = "https://api.langsearch.com/v1/web-search"
	maxSearchResponseBytes = 4 << 20
)

type providerFailure struct {
	status      int
	message     string
	fallback    bool
	cooldown    bool
	cooldownFor time.Duration
}

func (failure *providerFailure) Error() string {
	if failure == nil {
		return "网页搜索服务失败"
	}
	if failure.status != 0 {
		return fmt.Sprintf("HTTP %d: %s", failure.status, failure.message)
	}
	return failure.message
}

func callSearchProvider(ctx context.Context, client *http.Client, service Service, request SearchRequest) (SearchResponse, int, error) {
	switch service.Provider {
	case ProviderTavily:
		return callTavily(ctx, client, service, request)
	case ProviderLangSearch:
		return callLangSearch(ctx, client, service, request)
	default:
		return SearchResponse{}, 0, fmt.Errorf("%w: 不支持的服务类型", ErrInvalidRequest)
	}
}

func callTavily(ctx context.Context, client *http.Client, service Service, input SearchRequest) (SearchResponse, int, error) {
	body := map[string]any{
		"query": input.Query, "search_depth": "basic", "max_results": input.MaxResults,
		"topic": "general", "include_answer": false, "include_raw_content": false,
		"include_usage": true, "include_domains": input.IncludeDomains, "exclude_domains": input.ExcludeDomains,
	}
	if input.IncludeText {
		body["include_raw_content"] = "markdown"
	}
	if value := tavilyTimeRange(input.Freshness); value != "" {
		body["time_range"] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return SearchResponse{}, 0, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilySearchURL, bytes.NewReader(encoded))
	if err != nil {
		return SearchResponse{}, 0, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+service.APIKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := client.Do(httpRequest)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return SearchResponse{}, 0, ctx.Err()
		}
		return SearchResponse{}, 0, &providerFailure{message: "请求 Tavily 失败: " + err.Error(), fallback: true, cooldown: true}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSearchResponseBytes+1))
	if err != nil {
		return SearchResponse{}, response.StatusCode, &providerFailure{status: response.StatusCode, message: "读取 Tavily 响应失败", fallback: true}
	}
	if len(data) > maxSearchResponseBytes {
		return SearchResponse{}, response.StatusCode, &providerFailure{status: response.StatusCode, message: "Tavily 响应超过 4 MiB 限制"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return SearchResponse{}, response.StatusCode, providerHTTPFailure(response.StatusCode, data, service.APIKey, response.Header)
	}
	var payload struct {
		RequestID string `json:"request_id"`
		Usage     struct {
			Credits *int64 `json:"credits"`
		} `json:"usage"`
		Results []struct {
			Title         string  `json:"title"`
			URL           string  `json:"url"`
			Content       string  `json:"content"`
			RawContent    *string `json:"raw_content"`
			Score         float64 `json:"score"`
			PublishedDate string  `json:"published_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return SearchResponse{}, response.StatusCode, &providerFailure{status: response.StatusCode, message: "解析 Tavily 响应失败", fallback: true}
	}
	usage := Usage{Known: payload.Usage.Credits != nil}
	if payload.Usage.Credits != nil {
		usage.Credits = *payload.Usage.Credits
	}
	result := SearchResponse{Provider: service.Provider, ServiceID: service.ID, RequestID: payload.RequestID, Usage: usage}
	for _, item := range payload.Results {
		if !validResultURL(item.URL) {
			continue
		}
		entry := Result{Title: boundedText(item.Title, 500), URL: item.URL, Snippet: boundedText(item.Content, 1200), Score: normalizedScore(item.Score), PublishedAt: boundedText(item.PublishedDate, 128)}
		if input.IncludeText && item.RawContent != nil {
			entry.Text = boundedText(*item.RawContent, input.MaxTextChars)
		}
		result.Results = append(result.Results, entry)
		if len(result.Results) >= input.MaxResults {
			break
		}
	}
	return result, response.StatusCode, nil
}

func callLangSearch(ctx context.Context, client *http.Client, service Service, input SearchRequest) (SearchResponse, int, error) {
	body := map[string]any{"query": input.Query, "count": input.MaxResults, "freshness": langSearchFreshness(input.Freshness)}
	if len(input.IncludeDomains) > 0 {
		body["includeDomains"] = input.IncludeDomains
	}
	if len(input.ExcludeDomains) > 0 {
		body["excludeDomains"] = input.ExcludeDomains
	}
	if input.IncludeText {
		body["contents"] = map[string]any{"text": map[string]any{"maxCharacters": input.MaxTextChars}}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return SearchResponse{}, 0, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, langSearchSearchURL, bytes.NewReader(encoded))
	if err != nil {
		return SearchResponse{}, 0, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+service.APIKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := client.Do(httpRequest)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return SearchResponse{}, 0, ctx.Err()
		}
		return SearchResponse{}, 0, &providerFailure{message: "请求 LangSearch 失败: " + err.Error(), fallback: true, cooldown: true}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSearchResponseBytes+1))
	if err != nil {
		return SearchResponse{}, response.StatusCode, &providerFailure{status: response.StatusCode, message: "读取 LangSearch 响应失败", fallback: true}
	}
	if len(data) > maxSearchResponseBytes {
		return SearchResponse{}, response.StatusCode, &providerFailure{status: response.StatusCode, message: "LangSearch 响应超过 4 MiB 限制"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return SearchResponse{}, response.StatusCode, providerHTTPFailure(response.StatusCode, data, service.APIKey, response.Header)
	}
	var payload struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"msg"`
		LogID   string          `json:"log_id"`
		Usage   struct {
			InputTokens  *int64 `json:"input_tokens"`
			OutputTokens *int64 `json:"output_tokens"`
		} `json:"usage"`
		Data struct {
			Usage struct {
				InputTokens  *int64 `json:"input_tokens"`
				OutputTokens *int64 `json:"output_tokens"`
			} `json:"usage"`
			WebPages struct {
				Value []struct {
					Name          string  `json:"name"`
					URL           string  `json:"url"`
					Snippet       string  `json:"snippet"`
					Text          string  `json:"text"`
					Summary       string  `json:"summary"`
					Score         float64 `json:"score"`
					DatePublished string  `json:"datePublished"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return SearchResponse{}, response.StatusCode, &providerFailure{status: response.StatusCode, message: "解析 LangSearch 响应失败", fallback: true}
	}
	if code := strings.TrimSpace(strings.Trim(string(payload.Code), `"`)); code != "" && code != "200" {
		failure := &providerFailure{status: response.StatusCode, message: boundedText(payload.Message, 512), fallback: isRetryableProviderStatus(response.StatusCode)}
		if failure.message == "" {
			failure.message = "LangSearch 返回业务错误码 " + code
		}
		if response.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(failure.message), "quota") || strings.Contains(failure.message, "额度") {
			failure.fallback, failure.cooldown = true, true
			failure.cooldownFor = parseRetryAfter(response.Header.Get("Retry-After"))
		}
		return SearchResponse{}, response.StatusCode, failure
	}
	usage := payload.Usage
	if usage.InputTokens == nil && usage.OutputTokens == nil {
		usage = payload.Data.Usage
	}
	accounting := Usage{Known: usage.InputTokens != nil || usage.OutputTokens != nil}
	if usage.InputTokens != nil {
		accounting.InputTokens = *usage.InputTokens
	}
	if usage.OutputTokens != nil {
		accounting.OutputTokens = *usage.OutputTokens
	}
	result := SearchResponse{Provider: service.Provider, ServiceID: service.ID, RequestID: payload.LogID, Usage: accounting}
	for _, item := range payload.Data.WebPages.Value {
		if !validResultURL(item.URL) {
			continue
		}
		snippet := item.Snippet
		if snippet == "" {
			snippet = item.Summary
		}
		entry := Result{Title: boundedText(item.Name, 500), URL: item.URL, Snippet: boundedText(snippet, 1200), Score: normalizedScore(item.Score), PublishedAt: boundedText(item.DatePublished, 128)}
		if input.IncludeText {
			entry.Text = boundedText(item.Text, input.MaxTextChars)
		}
		result.Results = append(result.Results, entry)
		if len(result.Results) >= input.MaxResults {
			break
		}
	}
	return result, response.StatusCode, nil
}

func providerHTTPFailure(status int, body []byte, apiKey string, headers http.Header) error {
	message := strings.TrimSpace(string(body))
	message = strings.ReplaceAll(message, apiKey, "[API_KEY]")
	message = boundedText(message, 512)
	if message == "" {
		message = http.StatusText(status)
	}
	failure := &providerFailure{status: status, message: message, fallback: isRetryableProviderStatus(status)}
	if status == http.StatusTooManyRequests || status == 432 || status == 433 {
		failure.fallback, failure.cooldown = true, true
		failure.cooldownFor = parseRetryAfter(headers.Get("Retry-After"))
	}
	return failure
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0
		}
		if seconds > 24*60*60 {
			seconds = 24 * 60 * 60
		}
		return time.Duration(seconds) * time.Second
	}
	until, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := time.Until(until)
	if delay < 0 {
		return 0
	}
	if delay > 24*time.Hour {
		return 24 * time.Hour
	}
	return delay
}

func isRetryableProviderStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500 || status == 432 || status == 433
}

func tavilyTimeRange(freshness string) string {
	switch freshness {
	case "oneDay":
		return "day"
	case "oneWeek":
		return "week"
	case "oneMonth":
		return "month"
	case "oneYear":
		return "year"
	default:
		return ""
	}
}

func langSearchFreshness(freshness string) string {
	if freshness == "" {
		return "noLimit"
	}
	return freshness
}

func validResultURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Hostname() != ""
}

func normalizedScore(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return 0
	}
	return value
}

func boundedText(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func withSearchTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, 45*time.Second)
}
