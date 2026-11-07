package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func codeContextTestSettings(storage string) sessionfs.NativeCodeContextSettings {
	return sessionfs.NativeCodeContextSettings{
		StorageDir:          storage,
		Policy:              sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		MaxEntries:          50,
		MaxDescriptionChars: 400,
		MaxRangeLines:       100,
		MaxFileBytes:        1 << 20,
		MaxOutputBytes:      1 << 20,
		MaxStorageBytes:     1 << 20,
	}
}

// codeContextFixture is a workspace with the tool loaded from the sample definition, so
// the sample file, the catalog wiring and the schema are exercised with every call.
type codeContextFixture struct {
	t         *testing.T
	root      string
	catalog   *tools.Catalog
	execution tools.ExecutionContext
}

func newCodeContextFixture(t *testing.T, mutate func(*sessionfs.NativeCodeContextSettings)) *codeContextFixture {
	t.Helper()
	root := t.TempDir()
	settings := codeContextTestSettings(filepath.Join(t.TempDir(), "ctx"))
	if mutate != nil {
		mutate(&settings)
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(root, "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "code_context.json")},
		NativeCodeContext:   &settings,
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	return &codeContextFixture{t: t, root: root, catalog: catalog, execution: tools.ExecutionContext{WorkspaceRoot: root, SessionKey: "demo\x00executor"}}
}

func (f *codeContextFixture) write(name, content string) {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *codeContextFixture) call(arguments string) string {
	f.t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": "code_context", "arguments": arguments})
	raw, err := f.catalog.ExecuteFunctionCall(context.Background(), call, f.execution)
	if err != nil {
		f.t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		f.t.Fatal(err)
	}
	return item.Output
}

// numbered is lines "<prefix>1" to "<prefix>n", each ending in a newline.
func numbered(prefix string, n int) string {
	var builder strings.Builder
	for line := 1; line <= n; line++ {
		builder.WriteString(prefix + strconv.Itoa(line) + "\n")
	}
	return builder.String()
}

func codeBlock(t *testing.T, output, path string) string {
	t.Helper()
	start := strings.Index(output, "=== "+path+" lines ")
	if start < 0 {
		t.Fatalf("no block for %s in:\n%s", path, output)
	}
	rest := output[start:]
	open := strings.Index(rest, "--- code ")
	if open < 0 {
		t.Fatalf("block for %s has no code:\n%s", path, output)
	}
	rest = rest[open:]
	rest = rest[strings.Index(rest, "\n")+1:]
	end := strings.Index(rest, "--- end code ")
	return rest[:end]
}

func TestCodeContextAddListAndGetPrintExactCode(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("pkg/a.go", numbered("a", 10))
	added := f.call(`{"action":"add","task":"T-001","path":"pkg/a.go","start":3,"end":5,"priority":1,"description":"  Builds the matcher.\nThe cap lives here.  "}`)
	for _, want := range []string{"task: \"T-001\"", "generation: 1", "  - id: 1\n    priority: 1\n", `command: read("pkg/a.go", 3, 5)`, "description: |-\n      Builds the matcher.\n      The cap lives here.\n", `updated_by: "executor"`} {
		if !strings.Contains(added, want) {
			t.Fatalf("add output lacks %q:\n%s", want, added)
		}
	}
	// The same range again is not a second entry.
	again := f.call(`{"action":"add","task":"T-001","path":"./pkg/a.go","start":3,"end":5,"priority":2,"description":"other words"}`)
	if !strings.Contains(again, "already saved") || !strings.Contains(again, "  - id: 1\n") || !strings.Contains(again, "generation: 1") {
		t.Fatalf("duplicate add = %s", again)
	}
	listed := f.call(`{"action":"list","task":"T-001"}`)
	if strings.Count(listed, "  - id: ") != 1 || !strings.Contains(listed, "stale: false") {
		t.Fatalf("list = %s", listed)
	}
	got := f.call(`{"action":"get","task":"T-001"}`)
	if code := codeBlock(t, got, "pkg/a.go"); code != "a3\na4\na5\n" {
		t.Fatalf("code = %q, want lines 3-5 exactly; output:\n%s", code, got)
	}
	if !strings.Contains(got, "#1 (priority 1, lines 3-5, updated by executor)\n  Builds the matcher.\n  The cap lives here.\n") {
		t.Fatalf("get lacks the description:\n%s", got)
	}
}

func TestCodeContextEntryIsStaleOnlyWhenItsLinesChange(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	original := numbered("b", 10)
	f.write("b.go", original)
	f.call(`{"action":"add","task":"T-002","path":"b.go","start":4,"end":6,"priority":1,"description":"the target"}`)

	// A change below the range leaves its lines as saved: still current.
	f.write("b.go", strings.Replace(original, "b9\n", "b9 changed\n", 1))
	if got := f.call(`{"action":"get","task":"T-002"}`); codeBlock(t, got, "b.go") != "b4\nb5\nb6\n" || strings.Contains(got, "stale") {
		t.Fatalf("an edit outside the range made it stale:\n%s", got)
	}

	// Lines inserted above move the code: the saved range now reads other bytes.
	f.write("b.go", "new1\nnew2\n"+original)
	got := f.call(`{"action":"get","task":"T-002"}`)
	if strings.Contains(got, "--- code ") || !strings.Contains(got, "1 stale entries") ||
		!strings.Contains(got, "- id 1  b.go  (was 4-6, priority 1): the file changed and these lines differ from what was saved\n  the target\n") {
		t.Fatalf("a moved range was not reported stale, or its code was printed:\n%s", got)
	}
	if listed := f.call(`{"action":"list","task":"T-002"}`); !strings.Contains(listed, "stale: true") || !strings.Contains(listed, `command: read("b.go", 4, 6)`) {
		t.Fatalf("list of a stale entry = %s", listed)
	}

	// The caller re-reads and updates the range; the id is kept.
	updated := f.call(`{"action":"update","task":"T-002","id":1,"start":6,"end":8}`)
	if !strings.Contains(updated, "  - id: 1\n") || !strings.Contains(updated, `command: read("b.go", 6, 8)`) || !strings.Contains(updated, "description: |-\n      the target\n") || !strings.Contains(updated, "generation: 2") {
		t.Fatalf("update = %s", updated)
	}
	if got := f.call(`{"action":"get","task":"T-002"}`); codeBlock(t, got, "b.go") != "b4\nb5\nb6\n" || strings.Contains(got, "stale") {
		t.Fatalf("updated entry:\n%s", got)
	}

	// The file is removed: stale, with the reason.
	if err := os.Remove(filepath.Join(f.root, "b.go")); err != nil {
		t.Fatal(err)
	}
	if got := f.call(`{"action":"get","task":"T-002"}`); !strings.Contains(got, "the file no longer exists") {
		t.Fatalf("missing file:\n%s", got)
	}
}

func TestCodeContextRevertedFileIsCurrentAgain(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	original := numbered("r", 5)
	f.write("r.go", original)
	f.call(`{"action":"add","task":"T-003","path":"r.go","start":2,"end":3,"priority":1,"description":"d"}`)
	f.write("r.go", strings.Replace(original, "r2\n", "r2 edited\n", 1))
	if got := f.call(`{"action":"get","task":"T-003"}`); !strings.Contains(got, "1 stale") {
		t.Fatalf("edited range not stale:\n%s", got)
	}
	f.write("r.go", original)
	if got := f.call(`{"action":"get","task":"T-003"}`); strings.Contains(got, "stale") || codeBlock(t, got, "r.go") != "r2\nr3\n" {
		t.Fatalf("reverted range not current:\n%s", got)
	}
}

func TestCodeContextGetMergesOverlappingRangesOfAFile(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("m.go", numbered("m", 20))
	f.call(`{"action":"add","task":"T-004","path":"m.go","start":2,"end":6,"priority":3,"description":"first"}`)
	f.call(`{"action":"add","task":"T-004","path":"m.go","start":5,"end":9,"priority":1,"description":"second"}`)
	f.call(`{"action":"add","task":"T-004","path":"m.go","start":10,"end":11,"priority":2,"description":"touching"}`)
	f.call(`{"action":"add","task":"T-004","path":"m.go","start":15,"end":16,"priority":4,"description":"apart"}`)
	got := f.call(`{"action":"get","task":"T-004"}`)
	if !strings.Contains(got, "=== m.go lines 2-11 ===") || !strings.Contains(got, "=== m.go lines 15-16 ===") || strings.Count(got, "--- code ") != 2 {
		t.Fatalf("blocks:\n%s", got)
	}
	if code := codeBlock(t, got, "m.go"); code != "m2\nm3\nm4\nm5\nm6\nm7\nm8\nm9\nm10\nm11\n" {
		t.Fatalf("merged code = %q", code)
	}
	for _, want := range []string{"#1 (priority 3, lines 2-6", "#2 (priority 1, lines 5-9", "#3 (priority 2, lines 10-11"} {
		if !strings.Contains(got, want) {
			t.Fatalf("merged block lacks %q:\n%s", want, got)
		}
	}
	// The merged block (best priority 1) comes before the priority 4 block.
	if strings.Index(got, "lines 2-11") > strings.Index(got, "lines 15-16") {
		t.Fatalf("blocks not ordered by priority:\n%s", got)
	}
	// A subset by id prints only those.
	if subset := f.call(`{"action":"get","task":"T-004","ids":[4,9]}`); !strings.Contains(subset, "=== m.go lines 15-16 ===") || strings.Contains(subset, "lines 2-") || !strings.Contains(subset, "no entry 9") {
		t.Fatalf("get by ids:\n%s", subset)
	}
}

func TestCodeContextGetLeavesOutTheHighestPriorityNumbersFirst(t *testing.T) {
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxOutputBytes = 400 })
	f.write("target.go", numbered("t", 5))
	f.write("big_test.go", numbered("long test line ", 60))
	f.write("helper.go", numbered("h", 3))
	f.call(`{"action":"add","task":"T-005","path":"big_test.go","start":1,"end":60,"priority":5,"description":"tests"}`)
	f.call(`{"action":"add","task":"T-005","path":"target.go","start":1,"end":5,"priority":1,"description":"target"}`)
	f.call(`{"action":"add","task":"T-005","path":"helper.go","start":1,"end":3,"priority":6,"description":"helper"}`)
	got := f.call(`{"action":"get","task":"T-005"}`)
	if !strings.Contains(got, "=== target.go lines 1-5 ===") || strings.Contains(got, "=== big_test.go") {
		t.Fatalf("budget did not keep priority 1 and leave out priority 5:\n%s", got)
	}
	// helper.go would fit, but it ranks below a block already left out.
	if strings.Contains(got, "=== helper.go") || !strings.Contains(got, "2 left out by max_output_bytes") ||
		!strings.Contains(got, "- id 1  big_test.go 1-60  (priority 5)") || !strings.Contains(got, "- id 3  helper.go 1-3  (priority 6)") {
		t.Fatalf("left-out list:\n%s", got)
	}
}

func TestCodeContextDropClearAndRefusals(t *testing.T) {
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) {
		settings.MaxEntries = 2
		settings.MaxRangeLines = 5
	})
	f.write("d.go", numbered("d", 10))
	f.write("bin.dat", "a\x00b\n")
	for arguments, want := range map[string]string{
		`{"action":"add","task":"../x","path":"d.go","start":1,"end":2,"priority":1,"description":"d"}`:     `"status":"invalid_input"`,
		`{"action":"add","task":"T-6","path":"d.go","start":1,"end":2,"priority":7,"description":"d"}`:      `"status":"invalid_input"`,
		`{"action":"add","task":"T-6","path":"d.go","start":9,"end":11,"priority":1,"description":"d"}`:     "error_code: range_outside_file",
		`{"action":"add","task":"T-6","path":"d.go","start":5,"end":4,"priority":1,"description":"d"}`:      "error_code: invalid_range",
		`{"action":"add","task":"T-6","path":"d.go","start":1,"end":6,"priority":1,"description":"d"}`:      "error_code: range_too_long",
		`{"action":"add","task":"T-6","path":"d.go","start":1,"end":2,"priority":1,"description":"   "}`:    "error_code: invalid_description",
		`{"action":"add","task":"T-6","path":"none.go","start":1,"end":2,"priority":1,"description":"d"}`:   "error_code: not_found",
		`{"action":"add","task":"T-6","path":"../out.go","start":1,"end":2,"priority":1,"description":"d"}`: "error_code: path_outside_workspace",
		`{"action":"add","task":"T-6","path":"bin.dat","start":1,"end":1,"priority":1,"description":"d"}`:   "error_code: not_text",
		`{"action":"add","task":"T-6","path":"d.go","start":1,"end":2,"description":"d"}`:                   `"status":"invalid_input"`,
		`{"action":"update","task":"T-6","id":3}`:                                                           "error_code: entry_not_found",
	} {
		if got := f.call(arguments); !strings.Contains(got, want) {
			t.Errorf("%s = %s, want %s", arguments, got, want)
		}
	}
	f.call(`{"action":"add","task":"T-6","path":"d.go","start":1,"end":2,"priority":1,"description":"one"}`)
	f.call(`{"action":"add","task":"T-6","path":"d.go","start":4,"end":5,"priority":1,"description":"two"}`)
	if got := f.call(`{"action":"add","task":"T-6","path":"d.go","start":7,"end":8,"priority":1,"description":"three"}`); !strings.Contains(got, "error_code: too_many_entries") {
		t.Fatalf("max_entries not enforced: %s", got)
	}
	if got := f.call(`{"action":"update","task":"T-6","id":2,"start":1,"end":2}`); !strings.Contains(got, "error_code: duplicate_entry") {
		t.Fatalf("update onto another entry's range: %s", got)
	}
	if got := f.call(`{"action":"drop","task":"T-6","ids":[1,5]}`); got != "code_context task T-6 generation 3: dropped 1; no entry 5\n" {
		t.Fatalf("drop = %q", got)
	}
	// Ids are never reused.
	if got := f.call(`{"action":"add","task":"T-6","path":"d.go","start":7,"end":8,"priority":1,"description":"three"}`); !strings.Contains(got, "  - id: 3\n") {
		t.Fatalf("id after a drop: %s", got)
	}
	if got := f.call(`{"action":"clear","task":"T-6"}`); got != "code_context task T-6 cleared\n" {
		t.Fatalf("clear = %q", got)
	}
	if got := f.call(`{"action":"get","task":"T-6"}`); !strings.Contains(got, "the task has no entries") {
		t.Fatalf("get after clear = %s", got)
	}
	if got := f.call(`{"action":"clear","task":"T-6"}`); !strings.Contains(got, "nothing to clear") {
		t.Fatalf("second clear = %q", got)
	}
}

func TestCodeContextHonorsItsPathRules(t *testing.T) {
	var secret string
	f := newCodeContextFixture(t, nil)
	secret = filepath.Join(f.root, "secret")
	settings := codeContextTestSettings(filepath.Join(t.TempDir(), "ctx"))
	settings.Policy = sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: secret}}}
	tool, err := NewCodeContextTool(settings)
	if err != nil {
		t.Fatal(err)
	}
	f.write("secret/key.go", "k\n")
	output, err := tool.Execute(context.Background(), map[string]any{"action": "add", "task": "T-7", "path": "secret/key.go", "start": json.Number("1"), "end": json.Number("1"), "priority": json.Number("1"), "description": "d"}, f.execution)
	if err != nil || !strings.Contains(output, "error_code: path_denied") {
		t.Fatalf("denied path = %s, %v", output, err)
	}
}

func TestCodeContextProfilesSharingADirectorySeeEachOthersEntries(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.WriteFile(filepath.Join(root, "s.go"), []byte(numbered("s", 4)), 0o644); err != nil {
		t.Fatal(err)
	}
	executor, _ := NewCodeContextTool(codeContextTestSettings(shared))
	reviewer, _ := NewCodeContextTool(codeContextTestSettings(shared))
	add := map[string]any{"action": "add", "task": "T-8", "path": "s.go", "start": json.Number("1"), "end": json.Number("2"), "priority": json.Number("1"), "description": "d"}
	if _, err := executor.Execute(context.Background(), add, tools.ExecutionContext{WorkspaceRoot: root, SessionKey: "demo\x00jad/reviewer"}); err != nil {
		t.Fatal(err)
	}
	got, _ := reviewer.Execute(context.Background(), map[string]any{"action": "get", "task": "T-8"}, tools.ExecutionContext{WorkspaceRoot: root, SessionKey: "demo\x00jad/reviewer0", AgentName: "checker"})
	if !strings.Contains(got, "updated by jad/reviewer)") || !strings.Contains(got, "s1\ns2\n") {
		t.Fatalf("shared get = %s", got)
	}
	updated, _ := reviewer.Execute(context.Background(), map[string]any{"action": "update", "task": "T-8", "id": json.Number("1")}, tools.ExecutionContext{WorkspaceRoot: root, AgentName: "checker"})
	if !strings.Contains(updated, `updated_by: "checker"`) || !strings.Contains(updated, `last_updated_by: "checker"`) {
		t.Fatalf("update by a sub-agent = %s", updated)
	}
}

func TestCodeContextLines(t *testing.T) {
	for _, tc := range []struct {
		data       string
		start, end int64
		want       string
		lines      int64
		ok         bool
	}{
		{"a\nb\nc\n", 1, 1, "a\n", 3, true},
		{"a\nb\nc\n", 2, 3, "b\nc\n", 3, true},
		{"a\nb\nc", 3, 3, "c", 3, true},
		{"a\nb\nc", 1, 3, "a\nb\nc", 3, true},
		{"a\nb\nc\n", 3, 4, "", 3, false},
		{"a\r\nb\r\n", 2, 2, "b\r\n", 2, true},
		{"\n\n", 2, 2, "\n", 2, true},
		{"", 1, 1, "", 0, false},
	} {
		got, lines, ok := codeContextLines([]byte(tc.data), tc.start, tc.end)
		if string(got) != tc.want || lines != tc.lines || ok != tc.ok {
			t.Errorf("codeContextLines(%q, %d, %d) = %q, %d, %v; want %q, %d, %v", tc.data, tc.start, tc.end, got, lines, ok, tc.want, tc.lines, tc.ok)
		}
	}
}

func TestCodeContextSampleDefinitionMatchesTheGoSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "code_context.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []tools.Definition `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("code_context.json: %v", err)
	}
	tool, err := NewCodeContextTool(codeContextTestSettings(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	sample := document.Tools[0]
	if same, err := sameJSON(sample.InputSchema, tool.Definition.InputSchema); err != nil || !same {
		t.Fatalf("sample schema differs from the Go schema (%v)", err)
	}
	if sample.ID != tool.Definition.ID || sample.Name != tool.Definition.Name || sample.Description != tool.Definition.Description {
		t.Fatalf("sample definition %+v, want %+v", sample, tool.Definition)
	}
}
