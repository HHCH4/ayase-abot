package eval

import (
	"fmt"
	"sort"
	"strings"
)

// Comparison is a deterministic comparison of two results for the same case
// version. It never treats a changed case definition as a code regression.
type Comparison struct {
	Comparable            bool              `json:"comparable"`
	CaseID                string            `json:"case_id"`
	CaseVersion           string            `json:"case_version"`
	BasePassed            bool              `json:"base_passed"`
	CandidatePassed       bool              `json:"candidate_passed"`
	BaseHardFailure       bool              `json:"base_hard_failure"`
	CandidateHardFailure  bool              `json:"candidate_hard_failure"`
	BasePassRate          float64           `json:"base_pass_rate"`
	CandidatePassRate     float64           `json:"candidate_pass_rate"`
	PassRateDelta         float64           `json:"pass_rate_delta"`
	FailedAssertionDelta  int               `json:"failed_assertion_delta"`
	HardFailureIntroduced bool              `json:"hard_failure_introduced"`
	Regressions           []AssertionChange `json:"regressions,omitempty"`
	Reasons               []string          `json:"reasons,omitempty"`
}

type AssertionChange struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Base      bool   `json:"base"`
	Candidate bool   `json:"candidate"`
	Hard      bool   `json:"hard"`
	Code      string `json:"code,omitempty"`
}

func resultPassRate(result Result) float64 {
	if len(result.Assertions) == 0 {
		if result.Passed {
			return 1
		}
		return 0
	}
	passed := 0
	for _, assertion := range result.Assertions {
		if assertion.Passed {
			passed++
		}
	}
	return float64(passed) / float64(len(result.Assertions))
}

// CompareResults compares only equal case ID/version pairs. Assertion
// regressions are sorted by ID so API responses remain stable across runs.
func CompareResults(base, candidate Result) Comparison {
	comparison := Comparison{
		CaseID:                strings.TrimSpace(candidate.CaseID),
		CaseVersion:           strings.TrimSpace(candidate.CaseVersion),
		BasePassed:            base.Passed,
		CandidatePassed:       candidate.Passed,
		BaseHardFailure:       base.HardFailure,
		CandidateHardFailure:  candidate.HardFailure,
		BasePassRate:          resultPassRate(base),
		CandidatePassRate:     resultPassRate(candidate),
		FailedAssertionDelta:  countFailed(candidate) - countFailed(base),
		HardFailureIntroduced: candidate.HardFailure && !base.HardFailure,
	}
	comparison.PassRateDelta = comparison.CandidatePassRate - comparison.BasePassRate
	if strings.TrimSpace(base.CaseID) == "" || strings.TrimSpace(candidate.CaseID) == "" {
		comparison.Reasons = append(comparison.Reasons, "case_id_missing")
	}
	if strings.TrimSpace(base.CaseID) != strings.TrimSpace(candidate.CaseID) {
		comparison.Reasons = append(comparison.Reasons, fmt.Sprintf("case_id_mismatch:%s!=%s", base.CaseID, candidate.CaseID))
	}
	if strings.TrimSpace(base.CaseVersion) == "" || strings.TrimSpace(candidate.CaseVersion) == "" {
		comparison.Reasons = append(comparison.Reasons, "case_version_missing")
	}
	if strings.TrimSpace(base.CaseVersion) != strings.TrimSpace(candidate.CaseVersion) {
		comparison.Reasons = append(comparison.Reasons, fmt.Sprintf("case_version_mismatch:%s!=%s", base.CaseVersion, candidate.CaseVersion))
	}
	if len(comparison.Reasons) > 0 {
		return comparison
	}
	comparison.Comparable = true
	baseAssertions := make(map[string]AssertionResult, len(base.Assertions))
	for _, assertion := range base.Assertions {
		id := strings.TrimSpace(assertion.ID)
		if id != "" {
			baseAssertions[id] = assertion
		}
	}
	candidateAssertions := make(map[string]AssertionResult, len(candidate.Assertions))
	for _, assertion := range candidate.Assertions {
		id := strings.TrimSpace(assertion.ID)
		if id == "" {
			continue
		}
		candidateAssertions[id] = assertion
		previous, ok := baseAssertions[id]
		if ok && previous.Passed && !assertion.Passed {
			comparison.Regressions = append(comparison.Regressions, AssertionChange{ID: id, Kind: assertion.Kind, Base: previous.Passed, Candidate: assertion.Passed, Hard: assertion.Hard, Code: assertion.Code})
		}
	}
	// A candidate must not be able to make a passing baseline assertion
	// disappear. This catches an accidentally changed evaluator or fixture even
	// when the aggregate candidate result still reports Passed=true.
	for id, previous := range baseAssertions {
		if !previous.Passed {
			continue
		}
		if _, ok := candidateAssertions[id]; ok {
			continue
		}
		comparison.Regressions = append(comparison.Regressions, AssertionChange{ID: id, Kind: previous.Kind, Base: true, Candidate: false, Hard: previous.Hard, Code: "assertion_missing"})
	}
	sort.SliceStable(comparison.Regressions, func(i, j int) bool { return comparison.Regressions[i].ID < comparison.Regressions[j].ID })
	return comparison
}

func countFailed(result Result) int {
	count := 0
	for _, assertion := range result.Assertions {
		if !assertion.Passed {
			count++
		}
	}
	return count
}

type GatePolicy struct {
	MinPassRate      float64 `json:"min_pass_rate"`
	MaxHardFailures  int     `json:"max_hard_failures"`
	RequireBaseline  bool    `json:"require_baseline"`
	FailOnRegression *bool   `json:"fail_on_regression,omitempty"`
}

func (p GatePolicy) normalized() (GatePolicy, error) {
	if p.MinPassRate == 0 {
		p.MinPassRate = 1
	}
	if p.MinPassRate < 0 || p.MinPassRate > 1 {
		return GatePolicy{}, fmt.Errorf("min_pass_rate 必须在 0 到 1 之间")
	}
	if p.MaxHardFailures < 0 {
		return GatePolicy{}, fmt.Errorf("max_hard_failures 不能为负数")
	}
	// A release gate is conservative by default. Exploratory comparisons may
	// explicitly set fail_on_regression=false.
	if p.FailOnRegression == nil {
		strict := true
		p.FailOnRegression = &strict
	}
	return p, nil
}

type GateResult struct {
	Passed       bool         `json:"passed"`
	TotalCases   int          `json:"total_cases"`
	PassedCases  int          `json:"passed_cases"`
	PassRate     float64      `json:"pass_rate"`
	HardFailures int          `json:"hard_failures"`
	Comparisons  []Comparison `json:"comparisons,omitempty"`
	Reasons      []string     `json:"reasons,omitempty"`
}

// EvaluateReleaseGate applies hard safety rules before aggregate scoring. A
// hard failure can never be hidden by a high average score.
func EvaluateReleaseGate(candidates, baseline []Result, policy GatePolicy) (GateResult, error) {
	policy, err := policy.normalized()
	if err != nil {
		return GateResult{}, err
	}
	gate := GateResult{TotalCases: len(candidates), Passed: true}
	if len(candidates) == 0 {
		gate.Passed = false
		gate.Reasons = append(gate.Reasons, "no_candidate_results")
		return gate, nil
	}
	for _, candidate := range candidates {
		if candidate.Passed {
			gate.PassedCases++
		}
		if candidate.HardFailure {
			gate.HardFailures++
		}
	}
	gate.PassRate = float64(gate.PassedCases) / float64(gate.TotalCases)
	if gate.HardFailures > policy.MaxHardFailures {
		gate.Passed = false
		gate.Reasons = append(gate.Reasons, fmt.Sprintf("hard_failures:%d>%d", gate.HardFailures, policy.MaxHardFailures))
	}
	if gate.PassRate < policy.MinPassRate {
		gate.Passed = false
		gate.Reasons = append(gate.Reasons, fmt.Sprintf("pass_rate:%.4f<%.4f", gate.PassRate, policy.MinPassRate))
	}
	if policy.RequireBaseline && len(baseline) == 0 {
		gate.Passed = false
		gate.Reasons = append(gate.Reasons, "baseline_required")
	}
	baseByKey := make(map[string]Result, len(baseline))
	for _, item := range baseline {
		baseByKey[resultKey(item)] = item
	}
	for _, candidate := range candidates {
		base, ok := baseByKey[resultKey(candidate)]
		if !ok {
			if policy.RequireBaseline {
				gate.Passed = false
				gate.Reasons = append(gate.Reasons, "baseline_missing:"+resultKey(candidate))
			}
			continue
		}
		comparison := CompareResults(base, candidate)
		gate.Comparisons = append(gate.Comparisons, comparison)
		if !comparison.Comparable {
			gate.Passed = false
			gate.Reasons = append(gate.Reasons, "baseline_incompatible:"+resultKey(candidate))
			continue
		}
		if *policy.FailOnRegression && (comparison.HardFailureIntroduced || len(comparison.Regressions) > 0 || (comparison.BasePassed && !comparison.CandidatePassed)) {
			gate.Passed = false
			gate.Reasons = append(gate.Reasons, "regression:"+resultKey(candidate))
		}
	}
	sort.Strings(gate.Reasons)
	sort.SliceStable(gate.Comparisons, func(i, j int) bool {
		if gate.Comparisons[i].CaseID == gate.Comparisons[j].CaseID {
			return gate.Comparisons[i].CaseVersion < gate.Comparisons[j].CaseVersion
		}
		return gate.Comparisons[i].CaseID < gate.Comparisons[j].CaseID
	})
	return gate, nil
}

func resultKey(result Result) string {
	return strings.TrimSpace(result.CaseID) + "@" + strings.TrimSpace(result.CaseVersion)
}
