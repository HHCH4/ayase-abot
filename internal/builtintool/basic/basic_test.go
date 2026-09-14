package basic

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	tests := []struct {
		expression string
		want       float64
	}{
		{"1 + 2 * 3", 7},
		{"(1 + 2) * 3", 9},
		{"2^3^2", 512},
		{"-2^2", -4},
		{"sqrt(81) + abs(-2)", 11},
		{"max(2, pow(3, 2), 4) + min(8, 5)", 14},
		{"1.5e2 / 3", 50},
		{"round(pi * 1000) / 1000", 3.142},
	}
	for _, test := range tests {
		t.Run(test.expression, func(t *testing.T) {
			got, err := evaluate(test.expression)
			if err != nil {
				t.Fatalf("evaluate() error = %v", err)
			}
			if math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("evaluate() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestEvaluateRejectsInvalidInput(t *testing.T) {
	for _, expression := range []string{"", "1/0", "sqrt(-1)", "unknown(1)", "1 + os.Exit(1)", "pow(2)", "(1+2"} {
		if _, err := evaluate(expression); err == nil {
			t.Errorf("evaluate(%q) unexpectedly succeeded", expression)
		}
	}
	if _, err := evaluate(strings.Repeat("1", maxExpressionLength+1)); err == nil {
		t.Fatal("oversized expression unexpectedly succeeded")
	}
}

func TestTools(t *testing.T) {
	fixed := time.Date(2026, time.September, 10, 4, 5, 6, 0, time.UTC)
	tools, err := toolsWithClock(func() time.Time { return fixed })
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(tools) != 2 || tools[0].Name() != "current_time" || tools[1].Name() != "calculate" {
		t.Fatalf("unexpected built-in tools: %#v", tools)
	}
	if got := formatUTCOffset(8 * 3600); got != "+08:00" {
		t.Fatalf("offset = %q", got)
	}
	if got := formatUTCOffset(-(3*3600 + 30*60)); got != "-03:30" {
		t.Fatalf("offset = %q", got)
	}
}
