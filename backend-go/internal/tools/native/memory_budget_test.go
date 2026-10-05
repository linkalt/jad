package native

import (
	"path/filepath"
	"testing"
)

func TestMemoryStoreListByteBudgetStopsBeforeNextRecord(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	store := NewMemoryStore(root)
	settings := memoryTestSettings(root)
	first, err := store.Create("note", map[string]any{"value": "x"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create("note", map[string]any{"value": "x"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	newest, err := store.Get("note", second.Ref, settings)
	if err != nil {
		t.Fatal(err)
	}
	oneItemBytes, err := memoryListItemsSize([]MemoryItem{newest})
	if err != nil {
		t.Fatal(err)
	}
	settings.MaxListBytes = int64(oneItemBytes)
	listed, err := store.List("note", nil, 2, "newest", nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.Items[0].Ref != second.Ref || !listed.HasMore || listed.NextCursor == "" {
		t.Fatalf("byte-limited page = %+v; first ref is %s", listed, first.Ref)
	}
}
