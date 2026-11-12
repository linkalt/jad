package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newToolsetEditFixture is an edit fixture with the global toolset coding/full (exec, async,
// skill) beside dev/coder.
func newToolsetEditFixture(t *testing.T) editFixture {
	t.Helper()
	e := newEditFixture(t, "", "", editBase+`}`)
	wsPut(t, filepath.Join(e.f.global, "tools", "coding", "full.json"), `{"schema_version":1,"native":{"exec":{"timeout_ms":1000,"max_timeout_ms":5000},"async":{},"skill":{}}}`)
	return e
}

const ownLeanEdit = `{"schema_version":1,"based_on":"global:coding/full","omit":{"native":["skill"]},"set":{"native":{"exec":{"timeout_ms":2000}}}}`

// editToolsets is an edit of the profile file (contents, empty: unchanged) and of own toolsets.
func (e editFixture) editToolsets(t *testing.T, contents string, toolsets ...ProfileDocumentChange) (ProfileEditResult, error) {
	t.Helper()
	request := ProfileEditRequest{Action: ProfileEditActionEdit, IfMatch: e.revision(t), Toolsets: toolsets, Audit: ProfileEditAudit{Session: "s1", CallID: "call_1", Origin: "test"}}
	if contents != "" {
		request.Contents = json.RawMessage(contents)
	}
	return EditProfile(e.location, "dev/coder", request)
}

func (e editFixture) ownToolset(id string) string {
	return filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "tools", filepath.FromSlash(id)+".json")
}

func (e editFixture) ownToolsetRevision(t *testing.T, id string) string {
	t.Helper()
	contents, err := os.ReadFile(e.ownToolset(id))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return CatalogETag(contents)
}

// A new own toolset and the profile change that names it are one change; config lists it with
// its revision; the next load applies it; a toolsets-only edit replaces it; history records it
// (version 4) and revert restores it, then removes it with its reference.
func TestProfileEditWritesOwnToolsetsInOneChange(t *testing.T) {
	e := newToolsetEditFixture(t)
	named := editBase + `,"toolset":"profile:coding/lean"}`
	first, err := e.editToolsets(t, named, ProfileDocumentChange{Path: "coding/lean", Content: text(ownLeanEdit)})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || len(first.Toolsets) != 1 || first.Toolsets[0].Change != ProfileDocumentCreated || len(first.Models) != 0 {
		t.Fatalf("first = %+v", first)
	}
	written, _ := os.ReadFile(e.ownToolset("coding/lean"))
	if !strings.HasPrefix(string(written), "{\n  \"schema_version\": 1,") {
		t.Fatalf("own toolset written as\n%s", written)
	}
	if info, err := os.Stat(filepath.Dir(e.ownToolset("coding/lean"))); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("tools category folder = %v, %v", info, err)
	}
	config, err := LoadSessionSettings(e.location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeExec == nil || config.NativeExec.Child.TimeoutMs != 2000 || config.NativeSkill != nil {
		t.Fatalf("next load: exec %+v skill %v", config.NativeExec, config.NativeSkill)
	}
	view, err := ReadProfileConfig(config, ProfileRun{})
	if err != nil || len(view.Toolsets) != 1 || view.Toolsets[0].Path != "coding/lean" || view.Toolsets[0].Revision != e.ownToolsetRevision(t, "coding/lean") || view.ToolsetsDir != filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "tools") {
		t.Fatalf("config toolsets = %+v (%s), %v", view.Toolsets, view.ToolsetsError, err)
	}

	tuned := `{"based_on":"global:coding/full","set":{"native":{"exec":{"timeout_ms":3000}}}}`
	second, err := e.editToolsets(t, "", ProfileDocumentChange{Path: "coding/lean", Content: text(tuned), IfMatch: view.Toolsets[0].Revision})
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != second.OldRevision || second.Toolsets[0].Change != ProfileDocumentReplaced {
		t.Fatalf("a toolsets-only edit: %+v", second)
	}
	entry, err := readHistoryEntry(e.w, "dev/coder", second.HistoryID)
	if err != nil || entry.Version != 4 || entry.Previous != "" || len(entry.Toolsets) != 1 || string(entry.Toolsets[0].Previous) != string(written) {
		t.Fatalf("entry = %+v, %v", entry, err)
	}
	history, err := ReadProfileHistory(e.location, "dev/coder", 0)
	if err != nil || len(history.Entries) != 2 || len(history.Entries[0].Toolsets) != 1 || history.Entries[0].Toolsets[0].Previous != nil {
		t.Fatalf("history lists the toolsets without their bytes: %+v, %v", history, err)
	}
	if _, err := e.revert(t, second.HistoryID); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(e.ownToolset("coding/lean")); string(now) != string(written) {
		t.Fatalf("revert restored %q", now)
	}
	if _, err := e.revert(t, first.HistoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.ownToolset("coding/lean")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the created own toolset is still there: %v", err)
	}
	if now, _ := os.ReadFile(e.file); strings.Contains(string(now), "profile:coding/lean") {
		t.Fatalf("the profile still names the own toolset:\n%s", now)
	}
}

// An own toolset that breaks the rules, takes a shared toolset's ID, has no base, does not
// compose or fails the toolset rules is refused, whether the profile names it or not; nothing is
// written. The refusals hold at full too.
func TestProfileEditRefusesOwnToolsetsThatBreakTheRules(t *testing.T) {
	for _, level := range []string{ProfileSelfEditOn, ProfileSelfEditFull} {
		e := newToolsetEditFixture(t)
		e.location.Root.Layout.ProfileSelfEdit = level
		for _, tc := range []struct{ name, id, file, contents, want string }{
			{"refused key", "coding/lean", `{"based_on":"global:coding/full","set":{"native":{"exec":{"env_file":"/etc/x"}}}}`, "", "cannot be changed"},
			{"hooks", "coding/lean", `{"based_on":"global:coding/full","hooks":{"enabled":true}}`, "", "hooks is not a key"},
			{"shared ID", "coding/full", `{"based_on":"global:coding/full"}`, "", "is a shared toolset"},
			{"no base", "coding/lean", `{"based_on":"global:coding/gone"}`, "", "coding/gone"},
			{"adds a tool", "coding/lean", `{"based_on":"global:coding/full","set":{"native":{"lsp":{"max_results":2}}}}`, "", "has no native tool lsp"},
			{"tool rules, unnamed", "coding/lean", `{"based_on":"global:coding/full","set":{"native":{"exec":{"timeout_ms":9000}}}}`, "", "timeout_ms"},
			{"tool rules, named", "coding/lean", `{"based_on":"global:coding/full","set":{"native":{"exec":{"timeout_ms":9000}}}}`, editBase + `,"toolset":"profile:coding/lean"}`, "timeout_ms"},
			{"bad ID", "lean", `{"based_on":"global:coding/full"}`, "", "<category>/<name>"},
			{"not an object", "coding/lean", `[1]`, "", "JSON object"},
		} {
			_, err := e.editToolsets(t, tc.contents, ProfileDocumentChange{Path: tc.id, Content: text(tc.file)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s %s: %v, want %q", level, tc.name, err, tc.want)
			}
			if e.ownToolsetRevision(t, "coding/lean") != "" || e.historyCount(t) != 0 {
				t.Fatalf("%s %s: something was written", level, tc.name)
			}
		}
	}
	e := newToolsetEditFixture(t)
	e.location.Root.Layout.ProfileSelfEdit = ProfileSelfEditOff
	if _, err := e.editToolsets(t, "", ProfileDocumentChange{Path: "coding/lean", Content: text(ownLeanEdit)}); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("off: %v", err)
	}
}

// The preconditions of an own toolset are those of an own model; two writers from one revision
// make one change; a failure at each publish step leaves everything as it was.
func TestProfileEditOwnToolsetPreconditionsAndPublish(t *testing.T) {
	e := newToolsetEditFixture(t)
	wsPut(t, e.ownToolset("coding/lean"), ownLeanEdit)
	var failed *CatalogError
	if _, err := e.editToolsets(t, "", ProfileDocumentChange{Path: "coding/lean", Content: text(ownLeanEdit + " ")}); !errors.As(err, &failed) || failed.Kind != CatalogPreconditionFailed {
		t.Fatalf("replace without if_match: %v", err)
	}
	ifMatch := e.revision(t)
	var wg sync.WaitGroup
	results := make([]error, 2)
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[index] = EditProfile(e.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionEdit, IfMatch: ifMatch,
				Toolsets: []ProfileDocumentChange{{Path: "coding/same", Content: text(`{"based_on":"global:coding/full","omit":{"native":["` + []string{"skill", "async"}[index] + `"]}}`)}}})
		}()
	}
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) || e.historyCount(t) != 1 {
		t.Fatalf("two writers: %v, %v, %d history entries", results[0], results[1], e.historyCount(t))
	}

	old := e.ownToolsetRevision(t, "coding/lean")
	profileBefore, _ := os.ReadFile(e.file)
	t.Cleanup(func() { editPublishFault = nil })
	for _, step := range []string{"document", "profile", "delete"} {
		editPublishFault = func(at string) error {
			if at == step {
				return errors.New("injected at " + at)
			}
			return nil
		}
		_, err := e.editToolsets(t, editBase+`,"toolset":"profile:coding/new"}`,
			ProfileDocumentChange{Path: "coding/new", Content: text(ownLeanEdit)},
			ProfileDocumentChange{Path: "coding/lean", Delete: true, IfMatch: old})
		editPublishFault = nil
		if err == nil || !strings.Contains(err.Error(), "was undone") {
			t.Fatalf("fault at %s: %v", step, err)
		}
		if now, _ := os.ReadFile(e.file); string(now) != string(profileBefore) || e.ownToolsetRevision(t, "coding/new") != "" || e.ownToolsetRevision(t, "coding/lean") != old {
			t.Fatalf("fault at %s: the rollback left changes", step)
		}
	}
}

// Revert refuses when an own toolset it touched changed after the entry, and a 3c.2 (version 3)
// entry of own models only still reads and reverts.
func TestProfileRevertOwnToolsetsAndVersion3Entries(t *testing.T) {
	e := newToolsetEditFixture(t)
	edited, err := e.editToolsets(t, "", ProfileDocumentChange{Path: "coding/lean", Content: text(ownLeanEdit)})
	if err != nil {
		t.Fatal(err)
	}
	wsPut(t, e.ownToolset("coding/lean"), `{"based_on":"global:coding/full","omit":{"native":["async"]}}`)
	var conflict *ConflictError
	if _, err := e.revert(t, edited.HistoryID); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "toolsets changed") || !strings.Contains(err.Error(), "coding/lean") {
		t.Fatalf("revert over a later change: %v", err)
	}

	model := `{"based_on":"global:openai/default"}`
	wsPut(t, e.ownModel("openai/v3"), model)
	old := e.revision(t)
	id := "000099-" + strings.Trim(old, `"`)[:12]
	v3 := `{"version":3,"id":"` + id + `","time":"2026-11-12T00:00:00Z","action":"edit","profile":"dev/coder","audit":{"session":"s1","origin":"test"},"profile_self_edit":"on","old_revision":` + jsonQuote(old) + `,"new_revision":` + jsonQuote(old) +
		`,"models":[{"path":"openai/v3","change":"created","new_revision":` + jsonQuote(CatalogETag(text(model))) + `}]}`
	dir, _ := historyFolder(e.w, "dev/coder", true)
	wsPut(t, filepath.Join(dir, id+".json"), v3)
	if _, err := e.revert(t, id); err != nil {
		t.Fatalf("revert of a version 3 entry: %v", err)
	}
	if e.ownModelRevision(t, "openai/v3") != "" {
		t.Fatal("the version 3 entry was not reverted")
	}
}

// A trial reads the candidate own toolsets in place of the disk.
func TestTrialScansReadOwnToolsetCandidates(t *testing.T) {
	e := newToolsetEditFixture(t)
	wsPut(t, e.ownToolset("coding/old"), `{"based_on":"global:coding/full"}`)
	scans := newTrialScans(e.file, nil)
	scans.overlayOwnFiles("dev/coder", []documentStep{{kind: ownToolsetKind, record: ProfileDocumentRecord{Path: "coding/new"}, content: text(ownLeanEdit)}, {kind: ownToolsetKind, record: ProfileDocumentRecord{Path: "coding/old"}, delete: true}})
	own, err := e.location.Root.Layout.profileToolsets(scans, "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := own.catalog.Lookup("profile:coding/old"); err == nil {
		t.Fatal("the deleted own toolset is still in the trial catalog")
	}
	entry, err := own.catalog.Lookup("profile:coding/new")
	if err != nil {
		t.Fatal(err)
	}
	toolset, err := own.toolset(entry.Path)
	if _, skill := toolset.Native["skill"]; err != nil || skill || toolset.Native["exec"] == nil {
		t.Fatalf("trial toolset = %+v, %v", toolset, err)
	}
	if _, err := os.Stat(e.ownToolset("coding/new")); !errors.Is(err, os.ErrNotExist) || e.ownToolsetRevision(t, "coding/old") == "" {
		t.Fatal("the trial touched the disk")
	}
}

// jad check reports the own toolset files a load would refuse, with the loader's reason.
func TestCheckReportsOwnToolsetFiles(t *testing.T) {
	e := newToolsetEditFixture(t)
	wsPut(t, e.ownToolset("coding/lean"), ownLeanEdit)
	wsPut(t, e.ownToolset("coding/env"), `{"based_on":"global:coding/full","set":{"native":{"exec":{"env":{"K":"v"}}}}}`)
	wsPut(t, e.ownToolset("coding/gone"), `{"based_on":"global:coding/missing"}`)
	wsPut(t, e.ownToolset("coding/slow"), `{"based_on":"global:coding/full","set":{"native":{"exec":{"timeout_ms":9000}}}}`)
	report, err := CheckLayout(wsMustLoad(t, e.f, e.w).Roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, problem := range report.Problems {
		if problem.Kind == "own toolset" {
			found[problem.ID] = problem.Message
		}
	}
	if len(found) != 3 || !strings.Contains(found["dev/coder profile:coding/env"], "set.native.exec.env") || !strings.Contains(found["dev/coder profile:coding/gone"], "coding/missing") || !strings.Contains(found["dev/coder profile:coding/slow"], "timeout_ms") {
		t.Fatalf("own toolset problems = %v (all: %+v)", found, report.Problems)
	}
}
