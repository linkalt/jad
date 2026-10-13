package app

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

func TestCollectStateContextSnapshotIncludesConfiguredMemoryItems(t *testing.T) {
	root := t.TempDir()
	settings := memoryStateContextTestSettings(root, 1024)
	store := native.NewMemoryStore(root)
	if _, err := store.Create("note", map[string]any{"title": "carry this forward", "status": "open", "html": "<tag>"}, settings); err != nil {
		t.Fatal(err)
	}
	config := sessionfs.SessionConfig{
		NativeMemory: &settings,
		StateContext: sessionfs.StateContextSettings{Enabled: true, Header: sessionfs.DefaultStateContextHeader, Triggers: compactionTrigger([]string{"memory"}, 4096)},
	}
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{memory: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("collect memory snapshot = include %v, error %v", include, err)
	}
	for _, expected := range []string{`"memory"`, `"type": "note"`, `"title": "carry this forward"`, `"html": "<tag>"`, `"has_more": false`} {
		if !strings.Contains(snapshot.Text, expected) {
			t.Fatalf("memory statecontext missing %q: %s", expected, snapshot.Text)
		}
	}
}

func TestCollectStateContextSnapshotTrimsOversizedMemoryItems(t *testing.T) {
	root := t.TempDir()
	settings := memoryStateContextTestSettings(root, 1024)
	store := native.NewMemoryStore(root)
	if _, err := store.Create("note", map[string]any{"payload": strings.Repeat("x", 4_000)}, settings); err != nil {
		t.Fatal(err)
	}
	config := sessionfs.SessionConfig{
		NativeMemory: &settings,
		StateContext: sessionfs.StateContextSettings{Enabled: true, Header: sessionfs.DefaultStateContextHeader, Triggers: compactionTrigger([]string{"memory"}, 1024)},
	}
	snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{memory: store}, "compaction")
	if err != nil || !include {
		t.Fatalf("collect trimmed memory snapshot = include %v, error %v", include, err)
	}
	if strings.Contains(snapshot.Text, strings.Repeat("x", 32)) || !strings.Contains(snapshot.Text, `"omitted_by_snapshot": 1`) || !strings.Contains(snapshot.Text, `"returned": 1`) {
		t.Fatalf("oversized memory item was not summarized: %s", snapshot.Text)
	}
}

func memoryStateContextTestSettings(root string, selectorLimit int64) sessionfs.NativeMemorySettings {
	return sessionfs.NativeMemorySettings{
		StorageDir: root, MaxTypes: 4, MaxRecordsPerType: 10, MaxTotalRecords: 20,
		MaxRecordBytes: 8 * 1024, MaxTotalBytes: 32 * 1024, ListLimit: 10, MaxListLimit: 20,
		MaxListBytes: 8 * 1024, MaxScanRecords: 50, MaxScanBytes: 32 * 1024,
		MaxFilterFields: 8, MaxFilterDepth: 8, MaxJSONDepth: 8,
		MaxCursors: 10, CursorTTLMS: 60_000, MaxCursorBytes: 8 * 1024,
		StateContext: []sessionfs.NativeMemoryStateContextSelector{{Type: "note", Filter: map[string]any{}, Limit: selectorLimit}},
	}
}

func TestCollectStateContextSnapshotSummarizesMemoryWithGroupBy(t *testing.T) {
	root := t.TempDir()
	settings := memoryStateContextTestSettings(root, 10)
	settings.MaxRecordsPerType = 40
	settings.MaxTotalRecords = 80
	store := native.NewMemoryStore(root)
	for index := 0; index < 30; index++ {
		status := "open"
		if index%3 == 0 {
			status = "fixed"
		}
		if _, err := store.Create("note", map[string]any{"status": status, "title": strings.Repeat("t", 60)}, settings); err != nil {
			t.Fatal(err)
		}
	}
	build := func(groupBy string) string {
		settings.StateContext = []sessionfs.NativeMemoryStateContextSelector{{Type: "note", Filter: map[string]any{}, Limit: 30, GroupBy: groupBy}}
		config := sessionfs.SessionConfig{
			NativeMemory: &settings,
			StateContext: sessionfs.StateContextSettings{Enabled: true, Header: sessionfs.DefaultStateContextHeader, Triggers: compactionTrigger([]string{"memory"}, 65536)},
		}
		snapshot, include, err := collectStateContextSnapshot(config, stateContextSources{memory: store}, "compaction")
		if err != nil || !include {
			t.Fatalf("collect group_by %q = include %v, error %v", groupBy, include, err)
		}
		return snapshot.Text
	}
	items, summary := build(""), build("status")
	for _, expected := range []string{`"group_by": "status"`, `"status": "fixed"`, `"count": 10`, `"count": 20`} {
		if !strings.Contains(summary, expected) {
			t.Fatalf("summary missing %q: %s", expected, summary)
		}
	}
	if strings.Contains(summary, "ttttt") || len(summary)*4 > len(items) {
		t.Fatalf("summary is %d bytes against %d for items; want records omitted and at most a quarter of the size", len(summary), len(items))
	}
}
