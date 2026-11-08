package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

func newSearchTestStore(t *testing.T) (*RecordsStore, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "records")
	store := NewRecordsStore(root)
	store.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	return store, root
}

func TestRecordsStoreDeleteChecksETagAndFreesCapacity(t *testing.T) {
	store, root := newSearchTestStore(t)
	settings := recordsTestSettings(root)
	created := make([]RecordsItem, 0, settings.MaxRecordsPerType)
	for id := int64(1); id <= settings.MaxRecordsPerType; id++ {
		item, err := store.Create("note", map[string]any{"id": id}, settings)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, item)
	}
	if _, err := store.Create("note", map[string]any{"id": 99}, settings); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("a full type accepted a record: %v", err)
	}
	_, err := store.Delete("note", created[0].Ref, created[1].ETag, settings)
	var conflict *RecordsConflictError
	if !errors.As(err, &conflict) || conflict.CurrentETag != created[0].ETag {
		t.Fatalf("delete with a stale ETag = %v", err)
	}
	deleted, err := store.Delete("note", created[0].Ref, created[0].ETag, settings)
	if err != nil || deleted.Ref != created[0].Ref || deleted.ETag != created[0].ETag {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	var storeError *RecordsStoreError
	if _, err := store.Get("note", created[0].Ref, settings); !errors.As(err, &storeError) || storeError.Kind != recordsErrorRef {
		t.Fatalf("get after delete = %v", err)
	}
	if _, err := store.Delete("note", created[0].Ref, "", settings); !errors.As(err, &storeError) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("second delete = %v", err)
	}
	if _, err := store.Create("note", map[string]any{"id": 5}, settings); err != nil {
		t.Fatalf("a delete did not free capacity: %v", err)
	}
	if _, err := store.Delete("note", "other/"+strings.TrimPrefix(created[1].Ref, "note/"), "", settings); err == nil {
		t.Fatal("a ref of another type was deleted")
	}

	// The last record of a type takes its directory with it, so the type no longer
	// counts toward max_types.
	single, err := store.Create("single", map[string]any{"id": 1}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete("single", single.Ref, "", settings); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "single")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the empty type directory stayed: %v", err)
	}

	// A malformed record can still be deleted.
	if err := os.WriteFile(filepath.Join(root, created[1].Ref), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete("note", created[1].Ref, "", settings); err != nil {
		t.Fatalf("delete of a malformed record = %v", err)
	}
}

func TestRecordsCursorContinuesAfterItsRecordIsDeleted(t *testing.T) {
	store, root := newSearchTestStore(t)
	settings := recordsTestSettings(root)
	created := make([]RecordsItem, 0, 4)
	for id := 1; id <= 4; id++ {
		item, err := store.Create("page", map[string]any{"id": id}, settings)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, item)
	}
	for _, order := range []string{"newest", "oldest"} {
		first, err := store.List("page", nil, 1, order, nil, settings)
		if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
			t.Fatalf("%s first page = %+v, %v", order, first, err)
		}
		// The page's only record is deleted before the next page is asked for.
		if _, err := store.Delete("page", first.Items[0].Ref, "", settings); err != nil {
			t.Fatal(err)
		}
		second, err := store.List("page", nil, 1, nil, first.NextCursor, settings)
		if err != nil || len(second.Items) != 1 {
			t.Fatalf("%s page after a delete = %+v, %v", order, second, err)
		}
		want := created[2].Ref
		if order == "oldest" {
			want = created[1].Ref
		}
		if second.Items[0].Ref != want {
			t.Fatalf("%s page after a delete started at %s, want %s", order, second.Items[0].Ref, want)
		}
	}
}

func TestRecordsStoreSearchMatchesEveryTermInValues(t *testing.T) {
	store, root := newSearchTestStore(t)
	settings := recordsTestSettings(root)
	settings.MaxRecordsPerType = 10
	settings.MaxQueryBytes = 64
	records := []map[string]any{
		{"title": "Deploy Alpha", "notes": []any{"needs the BETA flag"}, "port": 8080}, // all terms, two fields
		{"title": "alpha only", "status": "open"},
		{"beta": "alpha"}, // "beta" is only a key
		{"nested": map[string]any{"deep": "ALPHA and beta"}, "status": "done"}, // nested, upper case
	}
	created := make([]RecordsItem, 0, len(records))
	for _, data := range records {
		item, err := store.Create("note", data, settings)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, item)
	}
	refs := func(result RecordsListResult) []string {
		out := make([]string, 0, len(result.Items))
		for _, item := range result.Items {
			out = append(out, item.Ref)
		}
		return out
	}
	found, err := store.Search("note", "  alpha   Beta ", nil, 10, "oldest", nil, settings)
	if err != nil || strings.Join(refs(found), ",") != created[0].Ref+","+created[3].Ref {
		t.Fatalf("search alpha beta = %v, %v", refs(found), err)
	}
	found, err = store.Search("note", "8080", nil, 10, nil, nil, settings)
	if err != nil || len(found.Items) != 1 || found.Items[0].Ref != created[0].Ref {
		t.Fatalf("search by a number = %v, %v", refs(found), err)
	}
	found, err = store.Search("note", "alpha", map[string]any{"status": "done"}, 10, nil, nil, settings)
	if err != nil || len(found.Items) != 1 || found.Items[0].Ref != created[3].Ref {
		t.Fatalf("search with a filter = %v, %v", refs(found), err)
	}

	// A search cursor continues only the same search.
	first, err := store.Search("note", "alpha", nil, 1, "oldest", nil, settings)
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first search page = %+v, %v", first, err)
	}
	second, err := store.Search("note", "ALPHA", nil, 1, nil, first.NextCursor, settings)
	if err != nil || len(second.Items) != 1 || second.Items[0].Ref != created[1].Ref {
		t.Fatalf("second search page = %v, %v", refs(second), err)
	}
	if _, err := store.Search("note", "beta", nil, 1, nil, first.NextCursor, settings); err == nil || !strings.Contains(err.Error(), "cursor is invalid") {
		t.Fatalf("a search cursor continued another query: %v", err)
	}
	if _, err := store.List("note", nil, 1, nil, first.NextCursor, settings); err == nil || !strings.Contains(err.Error(), "cursor is invalid") {
		t.Fatalf("a search cursor continued a list: %v", err)
	}
	listed, err := store.List("note", nil, 1, "oldest", nil, settings)
	if err != nil || listed.NextCursor == "" {
		t.Fatal(err)
	}
	if _, err := store.Search("note", "alpha", nil, 1, nil, listed.NextCursor, settings); err == nil || !strings.Contains(err.Error(), "cursor is invalid") {
		t.Fatalf("a list cursor continued a search: %v", err)
	}

	if _, err := store.Search("note", "   ", nil, 1, nil, nil, settings); err == nil || !strings.Contains(err.Error(), "no words") {
		t.Fatalf("blank query = %v", err)
	}
	if _, err := store.Search("note", strings.Repeat("a", 65), nil, 1, nil, nil, settings); err == nil || !strings.Contains(err.Error(), "64 bytes") {
		t.Fatalf("long query = %v", err)
	}
}

func TestListCursorFingerprintIsUnchangedWithoutAQuery(t *testing.T) {
	filter := map[string]any{"status": "open"}
	plain, err := recordsFilterFingerprint(filter, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := recordsCursorFingerprint(filter, nil, "")
	if err != nil || listed != plain {
		t.Fatalf("a list cursor fingerprint changed: %s, %v", listed, err)
	}
	searched, err := recordsCursorFingerprint(filter, nil, "open")
	if err != nil || searched == plain {
		t.Fatalf("a search cursor fingerprint did not include the query: %s, %v", searched, err)
	}
}

func TestNativeRecordsToolDeleteAndSearch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	settings := recordsTestSettings(root)
	settings.MaxQueryBytes = 64
	store := NewRecordsStore(root)
	tool, err := NewRecordsTool(settings, store)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	call := func(id, arguments string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": id, "name": "records", "arguments": arguments})
		output, err := catalog.ExecuteFunctionCall(context.Background(), raw, tools.ExecutionContext{})
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(output, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded.Output
	}
	kept, err := store.Create("note", map[string]any{"title": "Release checklist"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := store.Create("note", map[string]any{"title": "Old release plan"}, settings)
	if err != nil {
		t.Fatal(err)
	}

	output := call("search", `{"operation":"search","type":"note","query":"release"}`)
	if !strings.Contains(output, "2 records found") || !strings.Contains(output, "operation: search") {
		t.Fatalf("search output = %s", output)
	}
	output = call("stale", `{"operation":"delete","type":"note","ref":"`+gone.Ref+`","if_match":"`+kept.ETag+`"}`)
	if !strings.Contains(output, "records_conflict") || !strings.Contains(output, gone.ETag) || !strings.Contains(output, "delete") {
		t.Fatalf("stale delete output = %s", output)
	}
	output = call("delete", `{"operation":"delete","type":"note","ref":"`+gone.Ref+`","if_match":"`+gone.ETag+`"}`)
	if !strings.Contains(output, "Record deleted") || !strings.Contains(output, gone.Ref) {
		t.Fatalf("delete output = %s", output)
	}
	output = call("again", `{"operation":"delete","type":"note","ref":"`+gone.Ref+`"}`)
	if !strings.Contains(output, "not found") || !strings.Contains(output, kept.Ref) {
		t.Fatalf("delete of a deleted record should list the refs left: %s", output)
	}
	output = call("one", `{"operation":"search","type":"note","query":"RELEASE plan"}`)
	if !strings.Contains(output, "0 records found") {
		t.Fatalf("search after delete = %s", output)
	}
	for name, arguments := range map[string]string{
		"no query":       `{"operation":"search","type":"note"}`,
		"no ref":         `{"operation":"delete","type":"note"}`,
		"data on delete": `{"operation":"delete","type":"note","ref":"` + kept.Ref + `","data":{}}`,
		"query on list":  `{"operation":"list","type":"note","query":"x"}`,
	} {
		if output := call(name, arguments); strings.Contains(output, `"status":"ok"`) {
			t.Errorf("%s was accepted: %s", name, output)
		}
	}
	if _, err := store.Get("note", kept.Ref, settings); err != nil {
		t.Fatalf("a refused call deleted the kept record: %v", err)
	}
}
