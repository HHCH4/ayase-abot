package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCompactionFailureEventIsBoundedAndMetadataOnly(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	event := compactionFailureEvent(" inv-compaction ", errors.New("provider body api_key=secret-value"), 3, now)
	if event.InvocationID != "inv-compaction" || event.Type != EventContextCompactionFailed || !event.Timestamp.Equal(now) {
		t.Fatalf("压缩失败事件基础字段错误: %#v", event)
	}
	if got, _ := event.Data["code"].(string); got != "context_compaction_failed" {
		t.Fatalf("压缩失败事件缺少稳定 code: %#v", event.Data)
	}
	if retryable, _ := event.Data["retryable"].(bool); !retryable {
		t.Fatalf("压缩失败应保留可重试标记: %#v", event.Data)
	}
	if count, _ := event.Data["consecutive_failures"].(int); count != 3 {
		t.Fatalf("连续失败次数未保留: %#v", event.Data)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-value") || strings.Contains(string(encoded), "provider body") {
		t.Fatalf("压缩失败事件不应复制上游错误正文: %s", encoded)
	}
}

func TestInvocationTraceIncludesCompactionFailure(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	invocation := Invocation{ID: "inv-compaction-trace", Status: InvocationRunning, CreatedAt: now, UpdatedAt: now}
	event := compactionFailureEvent(invocation.ID, errors.New("unsafe details"), 2, now.Add(time.Second))
	trace := buildInvocationTrace(invocation, []AgentEvent{event}, nil, nil, nil, nil, now.Add(2*time.Second))
	if trace.Metrics.RuntimeNoticeCodes == nil || len(trace.Metrics.RuntimeNoticeCodes) != 1 || trace.Metrics.RuntimeNoticeCodes[0] != "context_compaction_failed" {
		t.Fatalf("Trace 未记录压缩失败 code: %#v", trace.Metrics)
	}
	if trace.Spans[0].ErrorCode != "context_compaction_failed" {
		t.Fatalf("Trace 根 span 未记录压缩失败: %#v", trace.Spans[0])
	}
	found := false
	for _, span := range trace.Spans {
		if span.Kind == "context" && span.ErrorCode == "context_compaction_failed" {
			found = true
			if span.Status != "failed" || span.Outcome != "error" || span.Attributes["consecutive_failures"] != 2 {
				t.Fatalf("压缩失败 span 字段错误: %#v", span)
			}
		}
	}
	if !found {
		t.Fatalf("Trace 未生成压缩失败 context span: %#v", trace.Spans)
	}
}
