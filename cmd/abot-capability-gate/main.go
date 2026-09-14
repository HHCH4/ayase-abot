// Command abot-capability-gate evaluates a provider capability candidate
// against a baseline without contacting providers or mutating the registry.
// It is intentionally a separate binary so release pipelines can run the
// metadata-only gate before publishing a build.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"Abot/internal/provider"
)

const maxCapabilityGateInputBytes = 4 * 1024 * 1024

type gateInput struct {
	CandidateProfiles []provider.ModelCapabilityProfile `json:"candidate_profiles"`
	BaselineProfiles  []provider.ModelCapabilityProfile `json:"baseline_profiles,omitempty"`
	Policy            provider.CapabilityGatePolicy     `json:"policy"`
}

type gateFailedError struct{}

func (gateFailedError) Error() string { return "capability release gate failed" }

// evaluate reads one bounded JSON document, evaluates it, and writes the
// deterministic result even when the policy fails. A valid failed gate is
// returned as gateFailedError so the caller can use exit status 1 while CI
// still receives the machine-readable report on stdout.
func evaluate(input io.Reader, output io.Writer) error {
	if input == nil || output == nil {
		return errors.New("capability gate input/output 不能为空")
	}
	encoded, err := io.ReadAll(io.LimitReader(input, maxCapabilityGateInputBytes+1))
	if err != nil {
		return fmt.Errorf("读取 capability gate 输入失败: %w", err)
	}
	if len(encoded) > maxCapabilityGateInputBytes {
		return fmt.Errorf("capability gate 输入超过 %d 字节上限", maxCapabilityGateInputBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var payload gateInput
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("解析 capability gate 输入失败: %w", err)
	}
	// Decode a second value to reject trailing JSON and to keep pipeline input
	// unambiguous. The bounded reader also prevents a large trailing document
	// from bypassing the size guard.
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("capability gate 输入包含多个 JSON 文档")
		}
		return fmt.Errorf("读取 capability gate 输入尾部失败: %w", err)
	}
	result, err := provider.EvaluateCapabilityReleaseGate(payload.CandidateProfiles, payload.BaselineProfiles, payload.Policy)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("写出 capability gate 报告失败: %w", err)
	}
	if !result.Passed {
		return gateFailedError{}
	}
	return nil
}

func main() {
	flags := flag.NewFlagSet("abot-capability-gate", flag.ExitOnError)
	inputPath := flags.String("input", "-", "capability gate JSON 文件路径，- 表示 stdin")
	flags.Parse(os.Args[1:])

	input := io.Reader(os.Stdin)
	var file *os.File
	if *inputPath != "" && *inputPath != "-" {
		opened, err := os.Open(*inputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "打开 capability gate 输入失败: %v\n", err)
			os.Exit(2)
		}
		file = opened
		defer file.Close()
		input = file
	}

	if err := evaluate(input, os.Stdout); err != nil {
		var gateFailed gateFailedError
		if errors.As(err, &gateFailed) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "capability gate 执行失败: %v\n", err)
		os.Exit(2)
	}
}
