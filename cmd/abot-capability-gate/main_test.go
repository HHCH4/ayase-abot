package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const gateProfileJSON = `{
    "provider_id": "provider",
    "model_id": "model",
    "protocol": "openai_compatible",
    "route": "chat",
    "tool_calling": {"state": "unknown"},
    "parallel_tool_calls": {"state": "unknown"},
    "structured_output": {"state": "unknown"},
    "structured_output_schema": {"state": "unknown"},
    "streaming": {"state": "unknown"},
    "images": {"state": "unknown"},
    "input_files": {"state": "unknown"},
    "audio": {"state": "unknown"},
    "reasoning_effort": {"state": "unknown"},
    "reasoning_summary": {"state": "unknown"},
    "prompt_caching": {"state": "unknown"},
    "native_compaction": {"state": "unknown"},
    "tokenizer": {"name": "abot-heuristic", "version": "heuristic-v1", "source": "heuristic", "quality": "heuristic", "calibration": {"safety_multiplier": 1}},
    "updated_at": "2026-09-12T00:00:00Z"
}`

func TestEvaluateCapabilityGateWritesReportAndReturnsFailureForDowngrade(t *testing.T) {
	input := `{"candidate_profiles":[` + strings.Replace(gateProfileJSON, `"tool_calling": {"state": "unknown"}`, `"tool_calling": {"state": "unsupported"}`, 1) + `],"baseline_profiles":[` + strings.Replace(gateProfileJSON, `"tool_calling": {"state": "unknown"}`, `"tool_calling": {"state": "supported"}`, 1) + `],"policy":{}}`
	var report bytes.Buffer
	err := evaluate(strings.NewReader(input), &report)
	var failed gateFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("退化门禁应返回可识别失败: %v", err)
	}
	if !strings.Contains(report.String(), `"passed": false`) || !strings.Contains(report.String(), `capability_downgrade:`) {
		t.Fatalf("门禁报告缺少稳定失败信息: %s", report.String())
	}
}

func TestEvaluateCapabilityGateRejectsUnknownAndTrailingInput(t *testing.T) {
	for name, input := range map[string]string{
		"unknown field":     `{"candidate_profiles":[],"unexpected":true}`,
		"trailing document": `{"candidate_profiles":[]} {"candidate_profiles":[]}`,
		"trailing garbage":  `{"candidate_profiles":[]} trailing`,
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := evaluate(strings.NewReader(input), &output); err == nil {
				t.Fatal("非法输入应拒绝")
			}
			if output.Len() != 0 {
				t.Fatalf("非法输入不应输出报告: %s", output.String())
			}
		})
	}
}

func TestEvaluateCapabilityGateAcceptsStableReport(t *testing.T) {
	input := `{"candidate_profiles":[` + gateProfileJSON + `],"policy":{}}`
	var report bytes.Buffer
	if err := evaluate(strings.NewReader(input), &report); err != nil {
		t.Fatalf("稳定 profile 不应失败: %v", err)
	}
	if !strings.Contains(report.String(), `"passed": true`) || !strings.Contains(report.String(), `"total_candidates": 1`) {
		t.Fatalf("稳定报告缺少结果: %s", report.String())
	}
}

func TestEvaluateCapabilityGateRejectsOversizedInput(t *testing.T) {
	var report bytes.Buffer
	input := strings.NewReader(`{"candidate_profiles":[],"padding":"` + strings.Repeat("x", maxCapabilityGateInputBytes) + `"}`)
	if err := evaluate(input, &report); err == nil {
		t.Fatal("超大输入应拒绝")
	}
	if report.Len() != 0 {
		t.Fatalf("超大输入不应输出报告: %s", report.String())
	}
}
