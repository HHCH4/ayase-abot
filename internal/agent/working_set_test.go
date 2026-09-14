package agent

import (
	"errors"
	"testing"
	"time"
)

func TestNewFileSliceBindsFullAndRangeDigest(t *testing.T) {
	captured := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	slice, err := NewFileSlice("internal/demo.go", "package demo\n\nfunc Run() {}\n", 3, 3, "go", "Run", "file:internal/demo.go", captured)
	if err != nil {
		t.Fatal(err)
	}
	if slice.StartLine != 3 || slice.EndLine != 3 || slice.FileDigest == "" || slice.SliceDigest == "" || slice.FileDigest == slice.SliceDigest {
		t.Fatalf("file slice digest/range=%#v", slice)
	}
	if slice.CapturedAt != captured {
		t.Fatalf("captured_at=%v", slice.CapturedAt)
	}
}

func TestSelectWorkingSetPrefersRequiredAndExcludesStaleDuplicateAndBudget(t *testing.T) {
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	items := []WorkingSetItem{
		{ID: "optional", Kind: "file_slice", SourceRef: "file:a", Priority: WorkingSetPriorityP1, RelevanceScore: 0.9, TokenEstimate: 4, ContentDigest: "a", Freshness: WorkingSetFresh},
		{ID: "required", Kind: "verification_result", SourceRef: "verification:v1", Priority: WorkingSetPriorityP1, RelevanceScore: 0.1, TokenEstimate: 5, ContentDigest: "v", Freshness: WorkingSetFresh},
		{ID: "stale", Kind: "file_slice", SourceRef: "file:stale", Priority: WorkingSetPriorityP0, TokenEstimate: 1, ContentDigest: "s", Freshness: WorkingSetStale},
		{ID: "duplicate", Kind: "file_slice", SourceRef: "file:a", Priority: WorkingSetPriorityP1, RelevanceScore: 0.8, TokenEstimate: 4, ContentDigest: "a", Freshness: WorkingSetFresh},
		{ID: "too-large", Kind: "file_slice", SourceRef: "file:large", Priority: WorkingSetPriorityP2, TokenEstimate: 20, ContentDigest: "large", Freshness: WorkingSetFresh},
	}
	selected := SelectWorkingSet(items, 10, []string{"required"}, now)
	if len(selected.Included) != 2 || selected.Included[0].ID != "required" || selected.Included[1].ID != "optional" || selected.Used != 9 {
		t.Fatalf("selection=%#v", selected)
	}
	// Required items are admitted before score/budget competition. The total
	// can exceed the nominal budget only because required content is P0.
	reasons := map[string]string{}
	for _, excluded := range selected.Excluded {
		reasons[excluded.ID] = excluded.Reason
	}
	if reasons["stale"] != "stale" || reasons["duplicate"] != "duplicate" || reasons["too-large"] != "over_budget" {
		t.Fatalf("exclusion reasons=%#v", reasons)
	}
}

func TestWorkingSetValidationRejectsInvalidFreshnessAndDuplicateIDs(t *testing.T) {
	set := WorkingSet{InvocationID: "inv", Revision: 1, Items: []WorkingSetItem{
		{ID: "same", Kind: "file_slice", SourceRef: "file:a", Priority: 0, Freshness: WorkingSetFresh},
		{ID: "same", Kind: "file_slice", SourceRef: "file:b", Priority: 0, Freshness: WorkingSetFresh},
	}}
	if !errors.Is(set.Validate(), ErrInvalidWorkingSet) {
		t.Fatal("重复 item ID 应被拒绝")
	}
	set.Items = []WorkingSetItem{{ID: "one", Kind: "file_slice", SourceRef: "file:a", Priority: 0, Freshness: "bad"}}
	if !errors.Is(set.Validate(), ErrInvalidWorkingSet) {
		t.Fatal("非法 freshness 应被拒绝")
	}
}
