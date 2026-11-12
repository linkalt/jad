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

// editFixture is a workspace whose profile dev/coder has a file the tests edit.
type editFixture struct {
	f        wsFixture
	w        string
	location Location
	file     string
}

func newEditFixture(t *testing.T, globalJSON, workspaceJSON, profileJSON string) editFixture {
	t.Helper()
	f := newWSFixture(t)
	if globalJSON != "" {
		f.globalJSON(t, globalJSON)
	}
	if workspaceJSON == "" {
		workspaceJSON = `{}`
	}
	w := f.workspace(t, "proj", workspaceJSON)
	file := filepath.Join(w, ".jad", "profiles", "dev", "coder.json")
	if profileJSON != "" {
		wsPut(t, file, profileJSON)
	}
	return editFixture{f: f, w: w, location: f.reload(t, w), file: file}
}

func (f wsFixture) reload(t *testing.T, w string) Location {
	t.Helper()
	config := wsMustLoad(t, f, w)
	location, err := Resolve(config.Roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func (e editFixture) revision(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	return CatalogETag(contents)
}

func (e editFixture) edit(t *testing.T, contents string) (ProfileEditResult, error) {
	t.Helper()
	return EditProfile(e.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionEdit, Contents: json.RawMessage(contents), IfMatch: e.revision(t), Audit: ProfileEditAudit{Session: "s1", CallID: "call_1", Origin: "test"}})
}

func (e editFixture) historyCount(t *testing.T) int {
	t.Helper()
	history, err := ReadProfileHistory(e.location, "dev/coder", 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(history.Entries)
}

const editBase = `{"models":["openai/default"],"model_default":"openai/default"`

// profile_self_edit is the lower of the global level (on when unset) and the workspace's: a
// workspace.json cannot raise it; a bad value and the key in a profile file are load errors.
func TestProfileSelfEditLevelIsTheLowerOfGlobalAndWorkspace(t *testing.T) {
	for _, c := range []struct{ global, workspace, want string }{
		{"", "", "on"}, {"full", "", "full"}, {"", "full", "on"}, {"on", "full", "on"},
		{"full", "on", "on"}, {"on", "off", "off"}, {"off", "full", "off"}, {"full", "full", "full"},
	} {
		global, workspace := `{}`, `{}`
		if c.global != "" {
			global = `{"profile_self_edit":"` + c.global + `"}`
		}
		if c.workspace != "" {
			workspace = `{"profile_self_edit":"` + c.workspace + `"}`
		}
		e := newEditFixture(t, global, workspace, "")
		session, err := LoadSessionSettings(e.location, "dev/coder", "")
		if err != nil {
			t.Fatal(err)
		}
		if e.location.Root.Layout.ProfileSelfEdit != c.want || session.ProfileSelfEdit != c.want {
			t.Errorf("global %q workspace %q: %q / %q, want %q", c.global, c.workspace, e.location.Root.Layout.ProfileSelfEdit, session.ProfileSelfEdit, c.want)
		}
	}
	f := newWSFixture(t)
	w := f.workspace(t, "proj", `{"profile_self_edit":"yes"}`)
	wsMustRefuse(t, f, w, "profile_self_edit must be")
	f.globalJSON(t, `{"profile_self_edit":true}`)
	wsMustRefuse(t, f, f.workspace(t, "other", `{}`), "profile_self_edit must be")
	e := newEditFixture(t, "", "", editBase+`,"profile_self_edit":"full"}`)
	if _, err := LoadSessionSettings(e.location, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "profile_self_edit") {
		t.Fatalf("a profile file with profile_self_edit: %v", err)
	}
	wsPut(t, e.file, editBase+`,"settings":{"profile_self_edit":"full"}}`)
	if _, err := LoadSessionSettings(e.location, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "profile_self_edit") {
		t.Fatalf("profile settings with profile_self_edit: %v", err)
	}
}

// An edit replaces the file, keeps the file as it was in a history entry written first, reports
// the new revision, and the next load reads the new file.
func TestProfileEditKeepsHistoryAndTheNextLoadUsesIt(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`,"description":"old"}`)
	before, _ := os.ReadFile(e.file)
	result, err := e.edit(t, `{"description":"new","models":["openai/default"],"model_default":"openai/default","settings":{"max_tool_rounds":9}}`)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(e.file)
	if !result.Changed || result.Revision != CatalogETag(after) || result.OldRevision != CatalogETag(before) || result.HistoryID == "" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.HasPrefix(string(after), "{\n  \"models\"") || !strings.Contains(string(after), `"description": "new"`) {
		t.Fatalf("the file keeps the order of the old file and its indent:\n%s", after)
	}
	if strings.Join(result.ChangedKeys, ",") != "description,settings.max_tool_rounds" {
		t.Fatalf("changed keys = %v", result.ChangedKeys)
	}
	entry, err := readHistoryEntry(e.w, "dev/coder", result.HistoryID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Previous != string(before) || entry.NewRevision != result.Revision || entry.Action != ProfileEditActionEdit || entry.Audit.CallID != "call_1" || entry.SelfEdit != ProfileSelfEditOn {
		t.Fatalf("history entry = %+v", entry)
	}
	info, err := os.Stat(filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "history"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("history folder = %v, %v", info, err)
	}
	session, err := LoadSessionSettings(e.location, "dev/coder", "")
	if err != nil || session.MaxToolRounds != 9 {
		t.Fatalf("the next load: rounds %d, %v", session.MaxToolRounds, err)
	}
	history, err := ReadProfileHistory(e.location, "dev/coder", 0)
	if err != nil || len(history.Entries) != 1 || history.Entries[0].Previous != "" || history.Entries[0].ID != result.HistoryID {
		t.Fatalf("history = %+v, %v", history, err)
	}
	// The same file again changes nothing and keeps no entry.
	same, err := e.edit(t, string(after))
	if err != nil || same.Changed || e.historyCount(t) != 1 {
		t.Fatalf("an unchanged edit: %+v, %v", same, err)
	}
}

// if_match is required and must be the current revision; a wildcard is no revision.
func TestProfileEditNeedsTheCurrentRevision(t *testing.T) {
	e := newEditFixture(t, "", "", "")
	request := ProfileEditRequest{Action: ProfileEditActionEdit, Contents: json.RawMessage(editBase + `,"description":"x"}`)}
	var catalogErr *CatalogError
	if _, err := EditProfile(e.location, "dev/coder", request); !errors.As(err, &catalogErr) || catalogErr.Kind != CatalogPreconditionRequired {
		t.Fatalf("no if_match: %v", err)
	}
	for _, stale := range []string{"*", `"0000"`, CatalogETag([]byte("other"))} {
		request.IfMatch = stale
		if _, err := EditProfile(e.location, "dev/coder", request); !errors.As(err, &catalogErr) || catalogErr.Kind != CatalogPreconditionFailed {
			t.Fatalf("if_match %s: %v", stale, err)
		}
	}
	// The revision with or without its quotes.
	request.IfMatch = strings.Trim(e.revision(t), `"`)
	if _, err := EditProfile(e.location, "dev/coder", request); err != nil {
		t.Fatal(err)
	}
}

// A candidate that fails the load is refused: the file is unchanged and no history is kept.
func TestProfileEditTrialFailureLeavesTheFile(t *testing.T) {
	e := newEditFixture(t, "", `{"defaults":{"max_run_tokens":1000}}`, "")
	before, _ := os.ReadFile(e.file)
	for name, candidate := range map[string]string{
		"missing model":    `{"models":["openai/default"],"model_default":"openai/missing"}`,
		"unknown key":      editBase + `,"colour":"red"}`,
		"bad instructions": editBase + `,"instructions":""}`,
		"missing document": editBase + `,"prompt_documents":["{workspace}/missing.md"]}`,
		"not an object":    `[1]`,
	} {
		if _, err := e.edit(t, candidate); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	after, _ := os.ReadFile(e.file)
	if string(after) != string(before) || e.historyCount(t) != 0 {
		t.Fatalf("a refused edit changed the file or kept history:\n%s", after)
	}
}

// What each level lets a profile change (decider S5): off nothing; on its instructions,
// documents, models, tools and tuning; full its ceilings, commands and paths too, but never
// past a ceiling the defaults set, which the trial load still checks.
func TestProfileEditPolicyByLevel(t *testing.T) {
	cases := []struct {
		name, settings string
		on, full       bool
	}{
		{"instructions", `,"instructions":"be brief"`, true, true},
		{"compaction", `,"settings":{"compaction":{"trigger_tokens":5000}}`, true, true},
		{"parallel calls", `,"settings":{"max_parallel_tool_calls":2}`, true, true},
		{"tighter ceiling", `,"settings":{"max_run_tokens":500}`, false, true},
		{"raised ceiling", `,"settings":{"max_run_tokens":5000}`, false, false},
		{"budget", `,"settings":{"budget":{"max_session_tokens":10}}`, false, true},
		{"hooks", `,"settings":{"hooks":{}}`, false, true},
		{"env_file", `,"settings":{"env_file":"env.txt"}`, false, true},
		{"cwd", `,"settings":{"cwd":"WORKSPACE"}`, false, true},
		{"tool_storage_dir", `,"settings":{"tool_storage_dir":"x"}`, false, true},
		{"env_file in G/auth", `,"settings":{"env_file":"AUTH/key.env"}`, false, false},
	}
	for _, level := range []string{ProfileSelfEditOff, ProfileSelfEditOn, ProfileSelfEditFull} {
		for _, c := range cases {
			e := newEditFixture(t, `{"profile_self_edit":"full","defaults":{"max_run_tokens":1000}}`, `{"profile_self_edit":"`+level+`"}`, "")
			wsPut(t, filepath.Join(e.w, ".jad", "env.txt"), "A=1\n")
			wsPut(t, filepath.Join(e.f.global, "auth", "key.env"), "KEY=1\n")
			settings := strings.NewReplacer("AUTH", filepath.Join(e.f.global, "auth"), "WORKSPACE", e.w).Replace(c.settings)
			_, err := e.edit(t, editBase+settings+`}`)
			want := (level == ProfileSelfEditOn && c.on) || (level == ProfileSelfEditFull && c.full)
			if (err == nil) != want {
				t.Errorf("%s under %s: %v, want allowed %v", c.name, level, err, want)
			}
		}
	}
}

// The D3 limits for prompt documents a self-edit adds (design §4.5).
func TestProfileEditDocumentLimits(t *testing.T) {
	cases := []struct {
		name, document string
		on, full       bool
	}{
		{"workspace file", "{workspace}/docs/a.md", true, true},
		{"relative to the cwd", "docs/a.md", true, true},
		{"workspace documents", "{workspace}/.jad/documents/b.md", true, true},
		{"own documents", "{profile_own}/documents/c.md", true, true},
		{"workspace sessions", "{workspace}/.jad/sessions/s1.json", false, true},
		{"home", "~/x.md", false, true},
		{"global folder", "GLOBAL/notes.md", false, true},
		{"G/auth", "GLOBAL/auth/key.md", false, false},
		{"another workspace", "OTHER/docs/o.md", false, true},
		{"another profile's documents", "{workspace}/.jad/profile-own/dev/other/documents/d.md", false, false},
		{"a link out of the workspace", "{workspace}/docs/home-link.md", false, true},
	}
	for _, level := range []string{ProfileSelfEditOn, ProfileSelfEditFull} {
		for _, c := range cases {
			e := newEditFixture(t, `{"profile_self_edit":"full"}`, `{"profile_self_edit":"`+level+`"}`, "")
			t.Setenv("HOME", e.f.home)
			other := e.f.workspace(t, "other", `{}`)
			for _, path := range []string{filepath.Join(e.w, "docs", "a.md"), filepath.Join(e.w, ".jad", "documents", "b.md"), filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "documents", "c.md"),
				filepath.Join(e.f.home, "x.md"), filepath.Join(e.f.global, "notes.md"), filepath.Join(e.f.global, "auth", "key.md"), filepath.Join(other, "docs", "o.md"),
				filepath.Join(e.w, ".jad", "profile-own", "dev", "other", "documents", "d.md")} {
				wsPut(t, path, "text\n")
			}
			if err := os.Symlink(filepath.Join(e.f.home, "x.md"), filepath.Join(e.w, "docs", "home-link.md")); err != nil {
				t.Fatal(err)
			}
			document := strings.NewReplacer("GLOBAL", e.f.global, "OTHER", other).Replace(c.document)
			_, err := e.edit(t, editBase+`,"prompt_documents":["`+document+`"]}`)
			want := (level == ProfileSelfEditOn && c.on) || (level == ProfileSelfEditFull && c.full)
			if (err == nil) != want {
				t.Errorf("%s under %s: %v, want allowed %v", c.name, level, err, want)
			}
		}
	}
}

// A document the file already names is left as the user wrote it: an edit of something else
// is not refused for it.
func TestProfileEditKeepsTheDocumentsTheUserNamed(t *testing.T) {
	f := newWSFixture(t)
	t.Setenv("HOME", f.home)
	wsPut(t, filepath.Join(f.home, "x.md"), "text\n")
	w := f.workspace(t, "proj", `{}`)
	file := filepath.Join(w, ".jad", "profiles", "dev", "coder.json")
	wsPut(t, file, editBase+`,"prompt_documents":["~/x.md"]}`)
	e := editFixture{f: f, w: w, location: f.reload(t, w), file: file}
	if _, err := e.edit(t, editBase+`,"prompt_documents":["~/x.md"],"description":"d"}`); err != nil {
		t.Fatal(err)
	}
}

// revert restores the file a history entry kept, needs the current revision, keeps an entry of
// its own, and passes the same policy: under on it may not restore a ceiling value.
func TestProfileRevertRestoresUnderTheSameRules(t *testing.T) {
	e := newEditFixture(t, `{"profile_self_edit":"full"}`, `{}`, editBase+`,"settings":{"max_run_tokens":500}}`)
	original, _ := os.ReadFile(e.file)
	first, err := e.edit(t, editBase+`}`)
	if err != nil {
		t.Fatal(err)
	}
	revert := ProfileEditRequest{Action: ProfileEditActionRevert, HistoryID: first.HistoryID}
	var catalogErr *CatalogError
	if _, err := EditProfile(e.location, "dev/coder", revert); !errors.As(err, &catalogErr) {
		t.Fatalf("revert without if_match: %v", err)
	}
	// Under on (the workspace lowers the level) restoring the ceiling is refused.
	wsPut(t, filepath.Join(e.w, ".jad", "workspace.json"), `{"profile_self_edit":"on"}`)
	onLocation := e.f.reload(t, e.w)
	revert.IfMatch = e.revision(t)
	if _, err := EditProfile(onLocation, "dev/coder", revert); err == nil || !strings.Contains(err.Error(), "settings.max_run_tokens") {
		t.Fatalf("revert of a ceiling under on: %v", err)
	}
	if _, err := EditProfile(onLocation, "dev/coder", ProfileEditRequest{Action: ProfileEditActionRevert, HistoryID: "000009-0123456789ab", IfMatch: e.revision(t)}); err == nil {
		t.Fatal("a missing history entry was restored")
	}
	result, err := EditProfile(e.location, "dev/coder", revert)
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(e.file)
	if string(restored) != string(original) || result.RevertedTo != first.HistoryID || e.historyCount(t) != 2 {
		t.Fatalf("revert: %+v\n%s", result, restored)
	}
	entry, err := readHistoryEntry(e.w, "dev/coder", result.HistoryID)
	if err != nil || entry.Action != ProfileEditActionRevert || entry.RevertedTo != first.HistoryID {
		t.Fatalf("revert entry = %+v, %v", entry, err)
	}
	if !strings.HasPrefix(result.HistoryID, "000002-") {
		t.Fatalf("history ids follow each other: %s", result.HistoryID)
	}
}

// off refuses edit and revert; v1 has no self-edit.
func TestProfileEditOffAndV1(t *testing.T) {
	e := newEditFixture(t, "", `{"profile_self_edit":"off"}`, "")
	if _, err := e.edit(t, editBase+`,"description":"x"}`); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("edit under off: %v", err)
	}
	if _, err := EditProfile(Location{Root: Root{Layout: &Layout{}}}, "dev/coder", ProfileEditRequest{Action: ProfileEditActionEdit}); err == nil || !strings.Contains(err.Error(), "workspace layout") {
		t.Fatalf("edit in v1: %v", err)
	}
}

// A link in the history's place is refused: the history is never written through it.
func TestProfileHistoryRefusesALinkedFolder(t *testing.T) {
	e := newEditFixture(t, "", "", "")
	elsewhere := t.TempDir()
	own := filepath.Join(e.w, ".jad", "profile-own", "dev")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(own, "coder")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(e.file)
	if _, err := e.edit(t, editBase+`,"description":"x"}`); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Fatalf("edit through a linked folder: %v", err)
	}
	after, _ := os.ReadFile(e.file)
	entries, _ := os.ReadDir(elsewhere)
	if string(after) != string(before) || len(entries) != 0 {
		t.Fatalf("the edit wrote through the link: %d entries", len(entries))
	}
}

// Every writer of a profile file takes the same lock: while one holds it the others get a
// conflict, and of two edits from one revision exactly one is made.
func TestProfileFileWritersShareOneLock(t *testing.T) {
	e := newEditFixture(t, "", "", "")
	unlock, err := lockProfileFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	var conflict *ConflictError
	if _, err := e.edit(t, editBase+`,"description":"x"}`); !errors.As(err, &conflict) {
		t.Fatalf("edit while locked: %v", err)
	}
	if _, err := SetLayoutModelDefaultIfMatch(e.location, "dev/coder", "openai/default", "", ""); !errors.As(err, &conflict) {
		t.Fatalf("model default while locked: %v", err)
	}
	contents, _ := os.ReadFile(e.file)
	if _, _, err := WriteCatalogFile(*e.location.Root.Layout, nil, CatalogProfiles, "dev/coder", contents, WriteCondition{IfMatch: CatalogETag(contents)}, false, nil); !errors.As(err, &conflict) {
		t.Fatalf("catalog write while locked: %v", err)
	}
	if err := DeleteCatalogFile(*e.location.Root.Layout, nil, CatalogProfiles, "dev/coder", CatalogETag(contents)); !errors.As(err, &conflict) {
		t.Fatalf("catalog delete while locked: %v", err)
	}
	unlock()

	revision := e.revision(t)
	var wait sync.WaitGroup
	results := make([]error, 2)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, results[index] = EditProfile(e.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionEdit, IfMatch: revision, Contents: json.RawMessage(editBase + `,"description":"writer ` + string(rune('a'+index)) + `"}`)})
		}()
	}
	wait.Wait()
	made := 0
	for _, err := range results {
		var catalogErr *CatalogError
		switch {
		case err == nil:
			made++
		case errors.As(err, &conflict), errors.As(err, &catalogErr) && catalogErr.Kind == CatalogPreconditionFailed:
		default:
			t.Fatalf("a writer failed otherwise: %v", err)
		}
	}
	if made != 1 || e.historyCount(t) != 1 {
		t.Fatalf("%d edits made from one revision, %d history entries", made, e.historyCount(t))
	}
}

// The trial load reads the candidate in place of the file and leaves the disk alone.
func TestTrialScansReadTheCandidate(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`,"settings":{"max_tool_rounds":3}}`)
	before, _ := os.ReadFile(e.file)
	config, err := loadLayoutSessionSettings(e.location, *e.location.Root.Layout, "dev/coder", "", newTrialScans(e.file, []byte(editBase+`,"settings":{"max_tool_rounds":11}}`)))
	if err != nil || config.MaxToolRounds != 11 {
		t.Fatalf("trial: rounds %d, %v", config.MaxToolRounds, err)
	}
	after, _ := os.ReadFile(e.file)
	if string(after) != string(before) {
		t.Fatal("the trial wrote the file")
	}
}
