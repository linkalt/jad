package native

import (
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func seedGroupRecords(t *testing.T, store *RecordsStore, settings sessionfs.NativeRecordsSettings) {
	t.Helper()
	for _, data := range []map[string]any{
		{"status": "open", "area": "core"},
		{"status": "open", "area": "core"},
		{"status": "open", "area": "core", "archived": true},
		{"status": "open", "area": "ui"},
		{"status": "fixed", "area": "core"},
		{"area": "core"},
	} {
		if _, err := store.Create("bug", data, settings); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecordsStoreGroupByCountsFilteredRecords(t *testing.T) {
	root := t.TempDir()
	settings := recordsTestSettings(root)
	settings.MaxRecordsPerType = 10
	store := NewRecordsStore(root)
	seedGroupRecords(t, store, settings)

	result, err := store.GroupBy("bug", map[string]any{"area": "core"}, []map[string]any{{"archived": true}}, "status", settings)
	if err != nil {
		t.Fatal(err)
	}
	if result.AtLeast || result.Ungrouped != 1 || len(result.Groups) != 2 {
		t.Fatalf("group result = %+v", result)
	}
	if result.Groups[0].Value != "open" || result.Groups[0].Count != 2 || result.Groups[1].Value != "fixed" || result.Groups[1].Count != 1 {
		t.Fatalf("groups = %+v, want open=2 then fixed=1", result.Groups)
	}
}

func TestRecordsStoreGroupByReportsAtLeastWhenScanIsBounded(t *testing.T) {
	root := t.TempDir()
	settings := recordsTestSettings(root)
	settings.MaxRecordsPerType = 10
	store := NewRecordsStore(root)
	seedGroupRecords(t, store, settings)
	settings.MaxScanRecords = 2

	result, err := store.GroupBy("bug", map[string]any{}, nil, "status", settings)
	if err != nil {
		t.Fatal(err)
	}
	total := result.Ungrouped
	for _, group := range result.Groups {
		total += group.Count
	}
	if !result.AtLeast || total != 2 {
		t.Fatalf("bounded scan = %+v (counted %d), want at_least with 2 counted", result, total)
	}
}

func TestCollectRecordsStateContextSummaryGroupsAreFetchRecipes(t *testing.T) {
	root := t.TempDir()
	settings := recordsTestSettings(root)
	settings.MaxRecordsPerType = 10
	store := NewRecordsStore(root)
	seedGroupRecords(t, store, settings)
	selector := sessionfs.NativeRecordsStateContextSelector{Type: "bug", Filter: map[string]any{"area": "core"}, GroupBy: "status", Limit: 10}

	state, err := CollectRecordsStateContext(store, settings, []sessionfs.NativeRecordsStateContextSelector{selector})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 0 || len(state.Queries) != 0 || len(state.Summaries) != 1 {
		t.Fatalf("state = %+v, want only a summary", state)
	}
	summary := state.Summaries[0]
	if summary.Type != "bug" || summary.GroupBy != "status" || summary.Ungrouped != 1 || len(summary.Groups) != 2 {
		t.Fatalf("summary = %+v", summary)
	}
	for _, group := range summary.Groups {
		listed, err := store.List("bug", group.Filter, 10, "newest", nil, settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Items) != group.Count {
			t.Fatalf("group filter %v lists %d records, summary says %d", group.Filter, len(listed.Items), group.Count)
		}
	}
	if state.StateContextItemCount() != 3 {
		t.Fatalf("item count = %d, want 3 (two groups and the ungrouped entry)", state.StateContextItemCount())
	}
}

func TestRecordsStateContextTrimsSummaryEntriesWithoutDroppingTheSummary(t *testing.T) {
	state := &RecordsStateContext{Summaries: []RecordsStateContextSummary{{
		Type: "bug", GroupBy: "status", Ungrouped: 2,
		Groups: []RecordsStateContextGroup{{Filter: map[string]any{"status": "open"}, Count: 5}, {Filter: map[string]any{"status": "fixed"}, Count: 1}},
	}}}
	for want := 3; want > 0; want-- {
		if state.StateContextItemCount() != want || !state.TrimStateContextItem() {
			t.Fatalf("count = %d, want %d and a successful trim", state.StateContextItemCount(), want)
		}
	}
	summary := state.Summaries[0]
	if state.TrimStateContextItem() || summary.OmittedGroups != 3 || len(summary.Groups) != 0 || summary.Ungrouped != 0 || summary.Type != "bug" {
		t.Fatalf("summary after trimming = %+v", summary)
	}
}
