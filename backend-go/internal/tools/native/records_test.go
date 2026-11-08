package native

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func recordsTestSettings(root string) sessionfs.NativeRecordsSettings {
	return sessionfs.NativeRecordsSettings{
		StorageDir: root, MaxTypes: 4, MaxRecordsPerType: 4, MaxTotalRecords: 10,
		MaxRecordBytes: 10_000, MaxTotalBytes: 50_000, ListLimit: 2, MaxListLimit: 4,
		MaxListBytes: 10_000, MaxScanRecords: 20, MaxScanBytes: 50_000,
		MaxFilterFields: 4, MaxFilterDepth: 4, MaxJSONDepth: 8,
		MaxCursors: 8, CursorTTLMS: 60_000, MaxCursorBytes: 10_000,
		StateContext: []sessionfs.NativeRecordsStateContextSelector{},
	}
}

func TestRecordsStorePreservesRecordJSONAndETag(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	store := NewRecordsStore(root)
	store.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	settings := recordsTestSettings(root)
	value, err := parseRecordsJSON([]byte(`{"z":1,"a":{"second":2,"first":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	data := recordsGoMap(value)
	created, err := store.CreateValue("task", value, data, settings)
	if err != nil {
		t.Fatal(err)
	}
	wantText := "{\n  \"z\": 1,\n  \"a\": {\n    \"second\": 2,\n    \"first\": 1\n  }\n}\n"
	stored, err := os.ReadFile(filepath.Join(root, created.Ref))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != wantText {
		t.Fatalf("stored JSON = %q, want %q", stored, wantText)
	}
	digest := sha256.Sum256([]byte(wantText))
	if created.ETag != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatalf("created ETag = %q", created.ETag)
	}
	loaded, err := store.Get("task", created.Ref, settings)
	if err != nil || loaded.ETag != created.ETag || loaded.Data["z"].(json.Number).String() != "1" {
		t.Fatalf("loaded item = %+v, error = %v", loaded, err)
	}
}

func TestRecordsStoreConditionalUpdateAndFilteredPagination(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	store := NewRecordsStore(root)
	store.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	settings := recordsTestSettings(root)
	created := make([]RecordsItem, 0, 4)
	for id := 1; id <= 4; id++ {
		value := recordsValueFromGo(map[string]any{"id": id, "project": map[string]any{"name": "core"}})
		item, err := store.CreateValue("page", value, recordsGoMap(value), settings)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, item)
	}
	first, err := store.List("page", map[string]any{"project.name": "core"}, 2, "newest", nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Items[0].Ref != created[3].Ref || first.Items[1].Ref != created[2].Ref || first.NextCursor == "" || !first.HasMore {
		t.Fatalf("first page = %+v", first)
	}
	if !strings.HasPrefix(first.NextCursor, "m_") || strings.Contains(first.NextCursor, "page") {
		t.Fatalf("cursor leaked record details: %q", first.NextCursor)
	}
	second, err := store.List("page", nil, 2, nil, first.NextCursor, settings)
	if err != nil || len(second.Items) != 2 || second.Items[0].Ref != created[1].Ref || second.Items[1].Ref != created[0].Ref || second.NextCursor != "" {
		t.Fatalf("second page = %+v, error = %v", second, err)
	}
	if _, err := store.List("page", map[string]any{"id": 1}, 2, "newest", first.NextCursor, settings); err == nil || !strings.Contains(err.Error(), "cursor is invalid") {
		t.Fatalf("mismatched cursor error = %v", err)
	}

	current, err := store.Get("page", created[0].Ref, settings)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.Update("page", created[0].Ref, current.ETag, map[string]any{"value": "winner"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("page", created[0].Ref, current.ETag, map[string]any{"value": "stale"}, settings)
	var conflict *RecordsConflictError
	if !errors.As(err, &conflict) || conflict.CurrentETag != updated.ETag {
		t.Fatalf("stale update error = %v", err)
	}
}

func TestRecordsStoreLegacyCursorAndPathPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	store := NewRecordsStore(root)
	store.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	settings := recordsTestSettings(root)
	first, err := store.Create("notes", map[string]any{"id": 1}, settings)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create("notes", map[string]any{"id": 2}, settings)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := recordsFilterFingerprint(map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON, _ := json.Marshal(recordsLegacyCursor{V: 1, Type: "notes", Order: "newest", Filter: fingerprint, Last: second.Ref})
	legacy := base64.RawURLEncoding.EncodeToString(legacyJSON)
	listed, err := store.List("notes", map[string]any{}, 2, "newest", legacy, settings)
	if err != nil || len(listed.Items) != 2 || listed.Items[0].Ref != second.Ref || listed.Items[1].Ref != first.Ref {
		t.Fatalf("legacy cursor page = %+v, error = %v", listed, err)
	}
	if _, err := store.Get("../escape", "../escape/ref", settings); err == nil || !strings.Contains(err.Error(), "type is invalid") {
		t.Fatalf("unsafe record type error = %v", err)
	}
	if _, err := store.Get("notes", "notes/../../outside", settings); err == nil || !strings.Contains(err.Error(), "ref is invalid") {
		t.Fatalf("unsafe record ref error = %v", err)
	}
}

func TestNativeRecordsToolCreateGetAndSchemaValidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	settings := recordsTestSettings(root)
	store := NewRecordsStore(root)
	tool, err := NewRecordsTool(settings, store)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	createCall, _ := json.Marshal(map[string]any{
		"type": "function_call", "call_id": "call-records-create", "name": "records",
		"arguments": `{"operation":"create","type":"note","data":{"title":"ordered","count":1}}`,
	})
	createdOutput, err := catalog.ExecuteFunctionCall(context.Background(), createCall, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(createdOutput, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created.Output, "Record created") || strings.Contains(created.Output, `"title": "ordered"`) {
		t.Fatalf("create output = %s", created.Output)
	}
	listed, err := store.List("note", nil, 1, "newest", nil, settings)
	if err != nil || len(listed.Items) != 1 {
		t.Fatalf("list after create = %+v, error = %v", listed, err)
	}
	normalizedCreate := strings.Replace(created.Output, listed.Items[0].Ref, "note/RECORDS_REF", 1)
	createLine, err := json.Marshal(struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}{Type: "function_call_output", CallID: "call-records-create", Output: normalizedCreate})
	if err != nil {
		t.Fatal(err)
	}
	createLine = append(createLine, '\n')
	createGolden, err := os.ReadFile(filepath.Join("testdata", "messages-native-records-create.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(createLine, createGolden) {
		t.Fatalf("native.records create messages.jsonl differs from Node golden\\n got: %s\\nwant: %s", createLine, createGolden)
	}
	listCall, _ := json.Marshal(map[string]any{
		"type": "function_call", "call_id": "call-records-list", "name": "records",
		"arguments": `{"operation":"list","type":"note","filter":{},"limit":2}`,
	})
	listOutput, err := catalog.ExecuteFunctionCall(context.Background(), listCall, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	var listedResponse struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(listOutput, &listedResponse); err != nil {
		t.Fatal(err)
	}
	listTokenIndex := strings.Index(listedResponse.Output, "token=")
	if listTokenIndex < 0 {
		t.Fatalf("list output has no content token: %s", listedResponse.Output)
	}
	listTokenStart := listTokenIndex + len("token=")
	listTokenEnd := strings.Index(listedResponse.Output[listTokenStart:], " ")
	if listTokenEnd < 0 {
		t.Fatalf("list output has malformed content token: %s", listedResponse.Output)
	}
	listToken := listedResponse.Output[listTokenStart : listTokenStart+listTokenEnd]
	normalizedList := strings.ReplaceAll(listedResponse.Output, listed.Items[0].Ref, "note/RECORDS_REF")
	normalizedList = strings.ReplaceAll(normalizedList, listToken, "CONTENT_TOKEN")
	listLine, err := json.Marshal(struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}{Type: "function_call_output", CallID: "call-records-list", Output: normalizedList})
	if err != nil {
		t.Fatal(err)
	}
	listLine = append(listLine, '\n')
	listGolden, err := os.ReadFile(filepath.Join("testdata", "messages-native-records-list.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(listLine, listGolden) {
		t.Fatalf("native.records list messages.jsonl differs from Node golden\\n got: %s\\nwant: %s", listLine, listGolden)
	}
	getArgs, _ := json.Marshal(map[string]any{"operation": "get", "type": "note", "ref": listed.Items[0].Ref})
	getCall, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "call-records-get", "name": "records", "arguments": string(getArgs)})
	getOutput, err := catalog.ExecuteFunctionCall(context.Background(), getCall, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	var fetched struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(getOutput, &fetched); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fetched.Output, `"title": "ordered"`) || strings.Index(fetched.Output, `"title"`) > strings.Index(fetched.Output, `"count"`) {
		t.Fatalf("get output did not preserve JSON order: %s", fetched.Output)
	}
	tokenIndex := strings.Index(fetched.Output, "token=")
	if tokenIndex < 0 {
		t.Fatalf("get output has no content token: %s", fetched.Output)
	}
	tokenStart := tokenIndex + len("token=")
	tokenEnd := strings.Index(fetched.Output[tokenStart:], " ")
	if tokenEnd < 0 {
		t.Fatalf("get output has malformed content token: %s", fetched.Output)
	}
	contentToken := fetched.Output[tokenStart : tokenStart+tokenEnd]
	normalizedGet := strings.ReplaceAll(fetched.Output, listed.Items[0].Ref, "note/RECORDS_REF")
	normalizedGet = strings.ReplaceAll(normalizedGet, contentToken, "CONTENT_TOKEN")
	getLine, err := json.Marshal(struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}{Type: "function_call_output", CallID: "call-records-get", Output: normalizedGet})
	if err != nil {
		t.Fatal(err)
	}
	getLine = append(getLine, '\n')
	getGolden, err := os.ReadFile(filepath.Join("testdata", "messages-native-records-get.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(getLine, getGolden) {
		t.Fatalf("native.records get messages.jsonl differs from Node golden\\n got: %s\\nwant: %s", getLine, getGolden)
	}
	invalid, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "call-records-invalid", "name": "records", "arguments": `{"operation":"create","type":"note"}`})
	invalidOutput, err := catalog.ExecuteFunctionCall(context.Background(), invalid, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	invalidLine := append(append([]byte(nil), invalidOutput...), '\n')
	invalidGolden, err := os.ReadFile(filepath.Join("testdata", "messages-native-records-invalid-create.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(invalidLine, invalidGolden) {
		t.Fatalf("native.records schema-error messages.jsonl differs from Node golden\\n got: %s\\nwant: %s", invalidLine, invalidGolden)
	}
}
