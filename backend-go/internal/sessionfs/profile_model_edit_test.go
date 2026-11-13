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

const ownFastEdit = `{"schema_version":1,"based_on":"global:openai/default","set":{"name":"Fast","efforts":["low","high"],"request":{"temperature":0.2}}}`

// editModels is an edit of the profile file (contents, empty: unchanged) and of own models.
func (e editFixture) editModels(t *testing.T, contents string, models ...ProfileDocumentChange) (ProfileEditResult, error) {
	t.Helper()
	request := ProfileEditRequest{Action: ProfileEditActionEdit, IfMatch: e.revision(t), Models: models, Audit: ProfileEditAudit{Session: "s1", CallID: "call_1", Origin: "test"}}
	if contents != "" {
		request.Contents = json.RawMessage(contents)
	}
	return EditProfile(e.location, "dev/coder", request)
}

func (e editFixture) ownModel(id string) string {
	return filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "models", filepath.FromSlash(id)+".json")
}

func (e editFixture) ownModelRevision(t *testing.T, id string) string {
	t.Helper()
	contents, err := os.ReadFile(e.ownModel(id))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return CatalogETag(contents)
}

// A new own model and the profile change that offers it are one change; the model is written
// indented, config lists it with its revision, the next load runs it composed from its base,
// a models-only edit replaces it, history records it (version 3) and revert restores it and
// then removes it together with its reference.
func TestProfileEditWritesOwnModelsInOneChange(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	offered := `{"models":["openai/default","profile:openai/fast"],"model_default":"profile:openai/fast@high"}`
	first, err := e.editModels(t, offered, ProfileDocumentChange{Path: "openai/fast", Content: text(ownFastEdit)})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || len(first.Models) != 1 || first.Models[0].Change != ProfileDocumentCreated || first.Models[0].Path != "openai/fast" || len(first.Documents) != 0 {
		t.Fatalf("first = %+v", first)
	}
	written, _ := os.ReadFile(e.ownModel("openai/fast"))
	if !strings.HasPrefix(string(written), "{\n  \"schema_version\": 1,\n  \"based_on\"") || !strings.HasSuffix(string(written), "}\n") {
		t.Fatalf("own model written as\n%s", written)
	}
	if info, err := os.Stat(filepath.Dir(e.ownModel("openai/fast"))); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("models category folder = %v, %v", info, err)
	}
	config, err := LoadSessionConfig(e.location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if !config.OwnModel(config.ModelPath) || len(config.Model.Efforts) != 2 || config.ReasoningEffort != "high" {
		t.Fatalf("next load: path %s, efforts %v, effort %q", config.ModelPath, config.Model.Efforts, config.ReasoningEffort)
	}
	// Files that are no own model (not two levels deep, not .json) are not listed.
	wsPut(t, filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "models", "README.md"), "notes")
	wsPut(t, filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "models", "openai", "deep", "x.json"), "{}")
	wsPut(t, filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "models", "openai", "notes.txt"), "notes")
	view, err := ReadProfileConfig(config, ProfileRun{})
	if err != nil || len(view.Models) != 1 || view.Models[0].Path != "openai/fast" || view.Models[0].Revision != e.ownModelRevision(t, "openai/fast") || view.ModelsDir != filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "models") {
		t.Fatalf("config models = %+v (%s), %v", view.Models, view.ModelsError, err)
	}

	profileBefore, _ := os.ReadFile(e.file)
	tuned := `{"based_on":"global:openai/default","set":{"name":"Fast","efforts":["low","high"],"request":{"temperature":0.7}}}`
	second, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/fast", Content: text(tuned), IfMatch: view.Models[0].Revision})
	if err != nil {
		t.Fatal(err)
	}
	if profileAfter, _ := os.ReadFile(e.file); string(profileAfter) != string(profileBefore) || second.Revision != second.OldRevision || second.Models[0].Change != ProfileDocumentReplaced {
		t.Fatalf("a models-only edit: %+v", second)
	}
	entry, err := readHistoryEntry(e.w, "dev/coder", second.HistoryID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Version != profileHistoryVersion || entry.Previous != "" || len(entry.Documents) != 0 || len(entry.Models) != 1 || string(entry.Models[0].Previous) != string(written) {
		t.Fatalf("entry = %+v", entry)
	}
	history, err := ReadProfileHistory(e.location, "dev/coder", 0)
	if err != nil || len(history.Entries) != 2 || len(history.Entries[0].Models) != 1 || history.Entries[0].Models[0].Previous != nil {
		t.Fatalf("history lists the models without their bytes: %+v, %v", history, err)
	}

	if _, err := e.revert(t, second.HistoryID); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(e.ownModel("openai/fast")); string(now) != string(written) {
		t.Fatalf("revert restored %q", now)
	}
	if _, err := e.revert(t, first.HistoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.ownModel("openai/fast")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the created own model is still there: %v", err)
	}
	if now, _ := os.ReadFile(e.file); strings.Contains(string(now), "profile:openai/fast") {
		t.Fatalf("the profile still offers the own model:\n%s", now)
	}
}

// An own model that breaks the own model rules, names no shared base, sits on a failing base,
// takes a shared model's ID or has a bad ID is refused at every level, and nothing is written.
func TestProfileEditRefusesOwnModelsThatBreakTheRules(t *testing.T) {
	cases := map[string]struct{ id, content, want string }{
		"base_url":           {"openai/fast", `{"based_on":"global:openai/default","set":{"base_url":"https://elsewhere.example"}}`, "set.base_url cannot be changed"},
		"api_key_env":        {"openai/fast", `{"based_on":"global:openai/default","set":{"api_key_env":"GITHUB_TOKEN"}}`, "set.api_key_env cannot be changed"},
		"provider":           {"openai/fast", `{"based_on":"global:openai/default","set":{"provider":"anthropic"}}`, "set.provider cannot be changed"},
		"store":              {"openai/fast", `{"based_on":"global:openai/default","set":{"request":{"store":true}}}`, "set.request.store cannot be changed"},
		"extra_headers":      {"openai/fast", `{"based_on":"global:openai/default","set":{"chat_completions":{"extra_headers":{"a":"b"}}}}`, "extra_headers cannot be changed"},
		"unknown set key":    {"openai/fast", `{"based_on":"global:openai/default","set":{"new_key":1}}`, "set.new_key is not one"},
		"wrong type":         {"openai/fast", `{"based_on":"global:openai/default","set":{"request":{"temperature":"0.2"}}}`, "must be a JSON number"},
		"top-level key":      {"openai/fast", `{"based_on":"global:openai/default","api_key_env":"K"}`, "api_key_env is not a key of a profile's own model"},
		"no scope":           {"openai/fast", `{"based_on":"openai/default"}`, "must start with global: or workspace:"},
		"own base":           {"openai/fast", `{"based_on":"profile:openai/other"}`, "must start with global: or workspace:"},
		"missing base":       {"openai/fast", `{"based_on":"global:openai/missing"}`, "does not load"},
		"failing base":       {"openai/fast", `{"based_on":"workspace:local/broken"}`, "request must be a JSON object"},
		"not an object":      {"openai/fast", `["based_on"]`, "content must be the own model file as a JSON object"},
		"shared ID":          {"openai/default", `{"based_on":"global:openai/default"}`, "global:openai/default is a shared model"},
		"no category":        {"fast", `{"based_on":"global:openai/default"}`, "named <category>/<name>"},
		"dot segment":        {"../fast", `{"based_on":"global:openai/default"}`, "category"},
		"nested":             {"openai/a/b", `{"based_on":"global:openai/default"}`, "name"},
		"hidden name":        {"openai/.fast", `{"based_on":"global:openai/default"}`, "must start with a letter or digit"},
		"unknown effort use": {"openai/fast", `{"based_on":"global:openai/default","set":{"efforts":["low"]}}`, ""},
	}
	for _, level := range []string{"on", "full"} {
		for name, c := range cases {
			e := newEditFixture(t, `{"profile_self_edit":"`+level+`"}`, `{"profile_self_edit":"`+level+`"}`, editBase+`}`)
			wsPut(t, filepath.Join(e.w, ".jad", "models", "local", "broken.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY","request":"not an object"}`)
			before, _ := os.ReadFile(e.file)
			result, err := e.editModels(t, "", ProfileDocumentChange{Path: c.id, Content: text(c.content)})
			if c.want == "" {
				if err != nil {
					t.Errorf("%s %s: a lawful own model is refused: %v", level, name, err)
				}
				continue
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s %s: %+v, %v", level, name, result, err)
			}
			// A key the own model rules refuse is named before the trial reads any base.
			if name == "base_url" && strings.Contains(err.Error(), "does not load") {
				t.Errorf("%s base_url: refused only by the trial: %v", level, err)
			}
			var client *ClientError
			if !errors.As(err, &client) {
				t.Errorf("%s %s: not a client error: %T", level, name, err)
			}
			if now, _ := os.ReadFile(e.file); string(now) != string(before) || e.historyCount(t) != 0 {
				t.Errorf("%s %s: the refused edit changed something", level, name)
			}
			if _, err := os.Stat(filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "models")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s %s: the models folder was made: %v", level, name, err)
			}
		}
	}
}

// The trial judges the whole selected profile with the change: deleting an own model the
// profile still offers, dropping the effort its model_default names, or pointing model_default
// at an own model whose base is missing is refused; a fault that was there before the change
// does not block an unrelated edit.
func TestProfileEditJudgesTheModelARunWouldUse(t *testing.T) {
	offered := `{"models":["openai/default","profile:openai/fast"],"model_default":"profile:openai/fast@high"}`
	e := newEditFixture(t, "", "", offered)
	wsPut(t, e.ownModel("openai/fast"), ownFastEdit)
	revision := e.ownModelRevision(t, "openai/fast")
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/fast", Delete: true, IfMatch: revision}); err == nil || !strings.Contains(err.Error(), "does not load") {
		t.Fatalf("deleting an offered own model: %v", err)
	}
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/fast", Content: text(`{"based_on":"global:openai/default","set":{"efforts":["low"]}}`), IfMatch: revision}); err == nil || !strings.Contains(err.Error(), `"high"`) {
		t.Fatalf("dropping the default's effort: %v", err)
	}
	if e.ownModelRevision(t, "openai/fast") != revision || e.historyCount(t) != 0 {
		t.Fatal("a refused edit changed the own model")
	}

	// A hand-written own model on a missing base: naming it as model_default is refused (the
	// load leaves it out and the run would fail with ModelFault).
	wsPut(t, e.ownModel("openai/gone"), `{"based_on":"global:openai/missing"}`)
	pointing := `{"models":["openai/default","profile:openai/fast","profile:openai/gone"],"model_default":"profile:openai/gone"}`
	if _, err := e.editModels(t, pointing); err == nil || !strings.Contains(err.Error(), "the model a run of the profile would use") {
		t.Fatalf("model_default on a broken own model: %v", err)
	}
	// The same fault already there does not block another change.
	wsPut(t, e.file, pointing)
	if _, err := e.editModels(t, `{"models":["openai/default","profile:openai/fast","profile:openai/gone"],"model_default":"profile:openai/gone","description":"still broken"}`); err != nil {
		t.Fatalf("an unrelated edit under a known fault: %v", err)
	}
	// Repairing it in the same change is allowed.
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/gone", Content: text(`{"based_on":"global:openai/default"}`), IfMatch: e.ownModelRevision(t, "openai/gone")}); err != nil {
		t.Fatalf("repairing the own model: %v", err)
	}
	if _, err := LoadSessionConfig(e.location, "dev/coder", ""); err != nil {
		t.Fatalf("after the repair: %v", err)
	}
}

// Own model preconditions follow the document rules: an existing model needs its revision, a
// new one none; two writers of one revision make one change.
func TestProfileEditOwnModelPreconditionsAndOneWriter(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	wsPut(t, e.ownModel("openai/fast"), ownFastEdit)
	var failed *CatalogError
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/fast", Content: text(ownFastEdit + " ")}); !errors.As(err, &failed) || failed.Kind != CatalogPreconditionFailed {
		t.Fatalf("replace without if_match: %v", err)
	}
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/fast", Delete: true, IfMatch: `"stale"`}); !errors.As(err, &failed) {
		t.Fatalf("delete with a stale revision: %v", err)
	}
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/new", Delete: true}); err == nil || !strings.Contains(err.Error(), "nothing to delete") {
		t.Fatalf("delete of a missing model: %v", err)
	}
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/x", Content: text(`{"based_on":"global:openai/default"}`)}, ProfileDocumentChange{Path: "openai/x", Delete: true}); err == nil || !strings.Contains(err.Error(), "given twice") {
		t.Fatalf("one model twice: %v", err)
	}
	// A category folder that is a link is not written through.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "models", "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "linked/x", Content: text(`{"based_on":"global:openai/default"}`)}); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("a linked category folder: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("written through the link: %v", entries)
	}
	ifMatch := e.revision(t)
	var wg sync.WaitGroup
	results := make([]error, 2)
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[index] = EditProfile(e.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionEdit, IfMatch: ifMatch,
				Models: []ProfileDocumentChange{{Path: "openai/same", Content: text(`{"based_on":"global:openai/default","set":{"name":"` + string(rune('a'+index)) + `"}}`)}}})
		}()
	}
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) || e.historyCount(t) != 1 {
		t.Fatalf("two writers: %v, %v, %d history entries", results[0], results[1], e.historyCount(t))
	}
}

// A failure at each publish step undoes the own models with the rest.
func TestProfileEditOwnModelPublishFailureRollsBack(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	wsPut(t, e.ownModel("openai/old"), `{"based_on":"global:openai/default"}`)
	old := e.ownModelRevision(t, "openai/old")
	profileBefore, _ := os.ReadFile(e.file)
	t.Cleanup(func() { editPublishFault = nil })
	for _, step := range []string{"document", "profile", "delete"} {
		editPublishFault = func(at string) error {
			if at == step {
				return errors.New("injected at " + at)
			}
			return nil
		}
		_, err := e.editModels(t, `{"models":["openai/default","profile:openai/fast"],"model_default":"openai/default"}`,
			ProfileDocumentChange{Path: "openai/fast", Content: text(ownFastEdit)},
			ProfileDocumentChange{Path: "openai/old", Delete: true, IfMatch: old})
		editPublishFault = nil
		if err == nil || !strings.Contains(err.Error(), "was undone") {
			t.Fatalf("fault at %s: %v", step, err)
		}
		if now, _ := os.ReadFile(e.file); string(now) != string(profileBefore) || e.ownModelRevision(t, "openai/fast") != "" || e.ownModelRevision(t, "openai/old") != old {
			t.Fatalf("fault at %s: the rollback left changes", step)
		}
	}
}

// Revert refuses when an own model it touched changed after the entry, and a 3b (version 2)
// entry of documents only still reads and reverts.
func TestProfileRevertOwnModelsAndVersion2Entries(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	edited, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/fast", Content: text(ownFastEdit)})
	if err != nil {
		t.Fatal(err)
	}
	wsPut(t, e.ownModel("openai/fast"), `{"based_on":"global:openai/default","set":{"name":"by hand"}}`)
	var conflict *ConflictError
	if _, err := e.revert(t, edited.HistoryID); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "models changed") || !strings.Contains(err.Error(), "openai/fast") {
		t.Fatalf("revert over a later change: %v", err)
	}
	if got, _ := os.ReadFile(e.ownModel("openai/fast")); !strings.Contains(string(got), "by hand") {
		t.Fatal("revert overwrote a later change")
	}

	wsPut(t, e.document("v2.md"), "written by 3b")
	old := e.revision(t)
	id := "000099-" + strings.Trim(old, `"`)[:12]
	v2 := `{"version":2,"id":"` + id + `","time":"2026-11-12T00:00:00Z","action":"edit","profile":"dev/coder","audit":{"session":"s1","origin":"test"},"profile_self_edit":"on","old_revision":` + jsonQuote(old) + `,"new_revision":` + jsonQuote(old) +
		`,"documents":[{"path":"v2.md","change":"created","new_revision":` + jsonQuote(CatalogETag(text("written by 3b"))) + `}]}`
	dir, _ := historyFolder(e.w, "dev/coder", true)
	wsPut(t, filepath.Join(dir, id+".json"), v2)
	if _, err := e.revert(t, id); err != nil {
		t.Fatalf("revert of a version 2 entry: %v", err)
	}
	if e.documentRevision(t, "v2.md") != "" {
		t.Fatal("the version 2 entry was not reverted")
	}
}

// A trial reads the candidate own models in place of the disk: a created one is in the
// profile's catalog with its contents, a deleted one is not, and the disk is not touched.
func TestTrialScansReadOwnModelCandidates(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	wsPut(t, e.ownModel("openai/old"), `{"based_on":"global:openai/default"}`)
	created := text(`{"based_on":"global:openai/default","set":{"efforts":["trial"]}}`)
	scans := newTrialScans(e.file, nil)
	scans.overlayOwnFiles("dev/coder", []documentStep{{kind: ownModelKind, record: ProfileDocumentRecord{Path: "openai/new"}, content: created}, {kind: ownModelKind, record: ProfileDocumentRecord{Path: "openai/old"}, delete: true}})
	own, err := e.location.Root.Layout.profileModels(scans, "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := own.catalog.Lookup("profile:openai/old"); err == nil {
		t.Fatal("the deleted own model is still in the trial catalog")
	}
	entry, err := own.catalog.Lookup("profile:openai/new")
	if err != nil {
		t.Fatal(err)
	}
	model, err := loadModelOf(entry.Path, own.bases, "", "")
	if err != nil || len(model.Efforts) != 1 || model.Efforts[0] != "trial" {
		t.Fatalf("trial model = %+v, %v", model, err)
	}
	if _, err := os.Stat(e.ownModel("openai/new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the trial wrote the own model")
	}
	if e.ownModelRevision(t, "openai/old") == "" {
		t.Fatal("the trial deleted the own model")
	}
}

// jad check reports the own model files a load leaves out, with the loader's reason.
func TestCheckReportsOwnModelFiles(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	wsPut(t, e.ownModel("openai/fast"), ownFastEdit)
	wsPut(t, e.ownModel("openai/stored"), `{"based_on":"global:openai/default","set":{"request":{"store":true}}}`)
	wsPut(t, e.ownModel("openai/gone"), `{"based_on":"global:openai/missing"}`)
	report, err := CheckLayout(wsMustLoad(t, e.f, e.w).Roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, problem := range report.Problems {
		if problem.Kind == "own model" {
			found[problem.ID] = problem.Message
		}
	}
	if len(found) != 2 || !strings.Contains(found["dev/coder profile:openai/stored"], "set.request.store") || !strings.Contains(found["dev/coder profile:openai/gone"], "openai/missing") {
		t.Fatalf("own model problems = %v (all: %+v)", found, report.Problems)
	}
}
