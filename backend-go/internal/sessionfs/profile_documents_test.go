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

const ownNotes = "{profile_own}/documents/notes.md"

// editWith is an edit of the profile file (contents, empty: unchanged) and of documents.
func (e editFixture) editWith(t *testing.T, contents string, documents ...ProfileDocumentChange) (ProfileEditResult, error) {
	t.Helper()
	request := ProfileEditRequest{Action: ProfileEditActionEdit, IfMatch: e.revision(t), Documents: documents, Audit: ProfileEditAudit{Session: "s1", CallID: "call_1", Origin: "test"}}
	if contents != "" {
		request.Contents = json.RawMessage(contents)
	}
	return EditProfile(e.location, "dev/coder", request)
}

func (e editFixture) revert(t *testing.T, id string) (ProfileEditResult, error) {
	t.Helper()
	return EditProfile(e.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionRevert, HistoryID: id, IfMatch: e.revision(t)})
}

func (e editFixture) document(name string) string {
	return filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "documents", filepath.FromSlash(name))
}

// documentRevision is the revision config lists for the document, "" when it is absent.
func (e editFixture) documentRevision(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(e.document(name))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return CatalogETag(contents)
}

func (e editFixture) prompt(t *testing.T) string {
	t.Helper()
	config, err := LoadSessionSettings(e.location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	return config.Model.PromptText
}

func text(value string) []byte { return []byte(value) }

// A new document and the profile change that names it are one change; a document-only edit
// replaces it; config lists each with its revision; history records the documents, and revert
// restores them.
func TestProfileEditWritesOwnDocumentsInOneChange(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	first, err := e.editWith(t, editBase+`,"prompt_documents":["`+ownNotes+`"]}`, ProfileDocumentChange{Path: "notes.md", Content: text("first notes")})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || len(first.Documents) != 1 || first.Documents[0].Change != ProfileDocumentCreated || first.Documents[0].OldRevision != "" || first.Documents[0].Previous != nil {
		t.Fatalf("first = %+v", first)
	}
	if got, _ := os.ReadFile(e.document("notes.md")); string(got) != "first notes" || !strings.Contains(e.prompt(t), "first notes") {
		t.Fatalf("document %q, prompt %q", got, e.prompt(t))
	}
	if info, err := os.Stat(filepath.Dir(e.document("notes.md"))); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("documents folder = %v, %v", info, err)
	}
	config, err := LoadSessionSettings(e.location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	view, err := ReadProfileConfig(config, ProfileRun{})
	if err != nil || len(view.Documents) != 1 || view.Documents[0].Path != "notes.md" || view.Documents[0].Revision != e.documentRevision(t, "notes.md") || view.DocumentsDir != filepath.Dir(e.document("notes.md")) {
		t.Fatalf("config documents = %+v (%s), %v", view.Documents, view.DocumentsError, err)
	}

	profileBefore, _ := os.ReadFile(e.file)
	second, err := e.editWith(t, "", ProfileDocumentChange{Path: "notes.md", Content: text("second notes"), IfMatch: view.Documents[0].Revision})
	if err != nil {
		t.Fatal(err)
	}
	if profileAfter, _ := os.ReadFile(e.file); string(profileAfter) != string(profileBefore) || second.Revision != second.OldRevision || len(second.ChangedKeys) != 0 {
		t.Fatalf("a document-only edit changed the profile file: %+v", second)
	}
	if !strings.Contains(e.prompt(t), "second notes") {
		t.Fatal("the next load does not read the replaced document")
	}
	entry, err := readHistoryEntry(e.w, "dev/coder", second.HistoryID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Version != profileHistoryVersion || entry.Previous != "" || len(entry.Documents) != 1 || string(entry.Documents[0].Previous) != "first notes" || entry.Documents[0].Change != ProfileDocumentReplaced {
		t.Fatalf("entry = %+v", entry)
	}
	history, err := ReadProfileHistory(e.location, "dev/coder", 0)
	if err != nil || len(history.Entries) != 2 || len(history.Entries[0].Documents) != 1 || history.Entries[0].Documents[0].Previous != nil {
		t.Fatalf("history lists the documents without their bytes: %+v, %v", history, err)
	}

	if _, err := e.revert(t, second.HistoryID); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(e.document("notes.md")); string(got) != "first notes" {
		t.Fatalf("revert restored %q", got)
	}
	// Reverting the first entry removes the document and its reference together.
	if _, err := e.revert(t, first.HistoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.document("notes.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the created document is still there: %v", err)
	}
	if now, _ := os.ReadFile(e.file); strings.Contains(string(now), "notes.md") {
		t.Fatalf("the profile still names the document:\n%s", now)
	}
}

// Deleting a document the profile still names fails the trial: nothing is written and no
// history is kept. Deleting it with the reference in one change works, and revert brings both back.
func TestProfileEditDeletingANamedDocumentFailsTheTrial(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	named := editBase + `,"prompt_documents":["` + ownNotes + `"]}`
	if _, err := e.editWith(t, named, ProfileDocumentChange{Path: "notes.md", Content: text("notes")}); err != nil {
		t.Fatal(err)
	}
	count := e.historyCount(t)
	revision := e.documentRevision(t, "notes.md")
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "notes.md", Delete: true, IfMatch: revision}); err == nil || !strings.Contains(err.Error(), "does not load") {
		t.Fatalf("deleting a named document: %v", err)
	}
	if e.documentRevision(t, "notes.md") != revision || e.historyCount(t) != count {
		t.Fatal("a refused delete changed the document or kept history")
	}
	removed, err := e.editWith(t, editBase+`}`, ProfileDocumentChange{Path: "notes.md", Delete: true, IfMatch: revision})
	if err != nil {
		t.Fatal(err)
	}
	if e.documentRevision(t, "notes.md") != "" || removed.Documents[0].Change != ProfileDocumentDeleted {
		t.Fatalf("deleted: %+v", removed)
	}
	if _, err := e.revert(t, removed.HistoryID); err != nil {
		t.Fatal(err)
	}
	if e.documentRevision(t, "notes.md") != revision || !strings.Contains(e.prompt(t), "notes") {
		t.Fatal("revert did not bring the document and its reference back")
	}
}

// The profile file's if_match anchors every edit; each document has its own: its revision to
// replace or delete it, none to create it. Of two writers from one revision exactly one wins.
func TestProfileEditDocumentPreconditions(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	var catalogErr *CatalogError
	_, err := EditProfile(e.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionEdit, Documents: []ProfileDocumentChange{{Path: "a.md", Content: text("a")}}})
	if !errors.As(err, &catalogErr) || catalogErr.Kind != CatalogPreconditionRequired {
		t.Fatalf("without the profile if_match: %v", err)
	}
	_, err = EditProfile(e.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionEdit, IfMatch: CatalogETag(text("old")), Documents: []ProfileDocumentChange{{Path: "a.md", Content: text("a")}}})
	if !errors.As(err, &catalogErr) || catalogErr.Kind != CatalogPreconditionFailed {
		t.Fatalf("a stale profile if_match: %v", err)
	}
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "a.md", Content: text("a")}); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]ProfileDocumentChange{
		"create an existing document":  {Path: "a.md", Content: text("b")},
		"a stale document if_match":    {Path: "a.md", Content: text("b"), IfMatch: CatalogETag(text("x"))},
		"if_match on a missing one":    {Path: "b.md", Content: text("b"), IfMatch: CatalogETag(text("x"))},
		"delete with a stale if_match": {Path: "a.md", Delete: true, IfMatch: CatalogETag(text("x"))},
	} {
		if _, err := e.editWith(t, "", change); !errors.As(err, &catalogErr) || catalogErr.Kind != CatalogPreconditionFailed {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "b.md", Delete: true}); err == nil {
		t.Error("deleting a missing document was accepted")
	}
	if e.documentRevision(t, "a.md") != CatalogETag(text("a")) || e.historyCount(t) != 1 {
		t.Fatal("a refused change wrote")
	}
	// An unchanged document is no change.
	if same, err := e.editWith(t, "", ProfileDocumentChange{Path: "a.md", Content: text("a"), IfMatch: CatalogETag(text("a"))}); err != nil || same.Changed {
		t.Fatalf("an unchanged document: %+v, %v", same, err)
	}

	revision := e.revision(t)
	document := e.documentRevision(t, "a.md")
	var wait sync.WaitGroup
	results := make([]error, 2)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, results[index] = EditProfile(e.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionEdit, IfMatch: revision,
				Documents: []ProfileDocumentChange{{Path: "a.md", Content: text("writer " + string(rune('a'+index))), IfMatch: document}}})
		}()
	}
	wait.Wait()
	made := 0
	for _, err := range results {
		var conflict *ConflictError
		switch {
		case err == nil:
			made++
		case errors.As(err, &conflict), errors.As(err, &catalogErr) && catalogErr.Kind == CatalogPreconditionFailed:
		default:
			t.Fatalf("a writer failed otherwise: %v", err)
		}
	}
	if made != 1 || e.historyCount(t) != 2 {
		t.Fatalf("%d edits made from one document revision, %d history entries", made, e.historyCount(t))
	}
}

// Document paths stay in {profile_own}/documents: no absolute path, no .. or dot segment, no link
// on the way and no linked target; each document and one edit are capped.
func TestProfileEditDocumentPathsAndSizes(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	for _, path := range []string{"../escape.md", "/etc/passwd", "a/./b.md", "a/../b.md", ".hidden", "a/.git/x", "a//b.md", "a\\b.md", "", "a/"} {
		if _, err := e.editWith(t, "", ProfileDocumentChange{Path: path, Content: text("x")}); err == nil {
			t.Errorf("path %q was accepted", path)
		}
	}
	big := make([]byte, maxPromptDocumentBytes+1)
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "big.md", Content: big}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a document above the cap: %v", err)
	}
	half := make([]byte, maxPromptDocumentBytes/2+1)
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "a.md", Content: half}, ProfileDocumentChange{Path: "b.md", Content: half}); err == nil || !strings.Contains(err.Error(), "together") {
		t.Errorf("an edit above the cap: %v", err)
	}
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "a.md", Content: text("x")}, ProfileDocumentChange{Path: "a.md", Content: text("y")}); err == nil {
		t.Error("one path twice was accepted")
	}

	elsewhere := t.TempDir()
	documents := filepath.Dir(e.document("x"))
	if err := os.MkdirAll(documents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(documents, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "linked/x.md", Content: text("x")}); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Errorf("a linked folder on the way: %v", err)
	}
	target := filepath.Join(elsewhere, "target.md")
	wsPut(t, target, "outside")
	if err := os.Symlink(target, filepath.Join(documents, "link.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "link.md", Content: text("x"), IfMatch: CatalogETag(text("outside"))}); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Errorf("a linked target: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "outside" {
		t.Fatal("the edit wrote through a link")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 1 || e.historyCount(t) != 0 {
		t.Fatalf("a refused edit wrote elsewhere or kept history: %d entries", len(entries))
	}

	// The documents folder itself a link.
	f := newEditFixture(t, "", "", editBase+`}`)
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(f.document("x"))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Dir(f.document("x"))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.editWith(t, "", ProfileDocumentChange{Path: "x.md", Content: text("x")}); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Errorf("a linked documents folder: %v", err)
	}
}

// Under off nothing is written; revert takes no documents.
func TestProfileEditDocumentsOffAndRevertShape(t *testing.T) {
	e := newEditFixture(t, "", `{"profile_self_edit":"off"}`, editBase+`}`)
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "a.md", Content: text("a")}); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("off: %v", err)
	}
	if _, err := os.Stat(e.document("a.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("off wrote a document")
	}
	on := newEditFixture(t, "", "", editBase+`}`)
	if _, err := EditProfile(on.location, "dev/coder", ProfileEditRequest{Action: ProfileEditActionRevert, HistoryID: "000001-0123456789ab", IfMatch: on.revision(t), Documents: []ProfileDocumentChange{{Path: "a.md", Content: text("a")}}}); err == nil {
		t.Fatal("revert with documents was accepted")
	}
	if _, err := on.editWith(t, ""); err == nil {
		t.Fatal("an edit of nothing was accepted")
	}
}

// A failure after publishing began undoes what was published and names the history entry; an
// entry whose change was only partly published can be reverted.
func TestProfileEditPublishFailureRollsBack(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	if _, err := e.editWith(t, "", ProfileDocumentChange{Path: "old.md", Content: text("old")}); err != nil {
		t.Fatal(err)
	}
	profileBefore, _ := os.ReadFile(e.file)
	for _, step := range []string{"document", "profile", "delete"} {
		editPublishFault = func(at string) error {
			if at == step {
				return errors.New("injected at " + at)
			}
			return nil
		}
		_, err := e.editWith(t, editBase+`,"description":"changed","prompt_documents":["`+ownNotes+`"]}`,
			ProfileDocumentChange{Path: "notes.md", Content: text("new")},
			ProfileDocumentChange{Path: "old.md", Delete: true, IfMatch: CatalogETag(text("old"))})
		editPublishFault = nil
		if err == nil || !strings.Contains(err.Error(), "history entry 0000") || !strings.Contains(err.Error(), "was undone") {
			t.Fatalf("fault at %s: %v", step, err)
		}
		if now, _ := os.ReadFile(e.file); string(now) != string(profileBefore) || e.documentRevision(t, "notes.md") != "" || e.documentRevision(t, "old.md") != CatalogETag(text("old")) {
			t.Fatalf("fault at %s: the rollback left changes", step)
		}
	}
	t.Cleanup(func() { editPublishFault = nil })

	// A partly published entry: its document is in place, its profile change is not.
	edited, err := e.editWith(t, editBase+`,"prompt_documents":["`+ownNotes+`"]}`, ProfileDocumentChange{Path: "notes.md", Content: text("partial")})
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := readHistoryEntry(e.w, "dev/coder", edited.HistoryID)
	wsPut(t, e.file, entry.Previous)
	if _, err := e.revert(t, edited.HistoryID); err != nil {
		t.Fatal(err)
	}
	if e.documentRevision(t, "notes.md") != "" {
		t.Fatal("revert of a partly published entry left its document")
	}
}

// Revert refuses when a document it touched changed after the entry.
func TestProfileRevertRefusesALaterDocumentChange(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	edited, err := e.editWith(t, "", ProfileDocumentChange{Path: "a.md", Content: text("mine")})
	if err != nil {
		t.Fatal(err)
	}
	wsPut(t, e.document("a.md"), "changed by hand")
	var conflict *ConflictError
	if _, err := e.revert(t, edited.HistoryID); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "a.md") {
		t.Fatalf("revert over a later change: %v", err)
	}
	if got, _ := os.ReadFile(e.document("a.md")); string(got) != "changed by hand" {
		t.Fatal("revert overwrote a later change")
	}
}

// A 3a history entry (no version, the profile file only) still decodes, lists and reverts.
func TestProfileHistoryReadsPhase3aEntries(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`,"description":"before"}`)
	before, _ := os.ReadFile(e.file)
	wsPut(t, e.file, editBase+`,"description":"after"}`)
	old := CatalogETag(before)
	id := "000001-" + strings.Trim(old, `"`)[:12]
	legacy := `{"id":"` + id + `","time":"2026-11-12T00:00:00Z","action":"edit","profile":"dev/coder","audit":{"session":"s1","origin":"test"},"profile_self_edit":"on","old_revision":` + jsonQuote(old) + `,"new_revision":` + jsonQuote(e.revision(t)) + `,"changed_keys":["description"],"previous":` + jsonQuote(string(before)) + `}`
	dir, err := historyFolder(e.w, "dev/coder", true)
	if err != nil {
		t.Fatal(err)
	}
	wsPut(t, filepath.Join(dir, id+".json"), legacy)
	history, err := ReadProfileHistory(e.location, "dev/coder", 0)
	if err != nil || len(history.Entries) != 1 || history.Entries[0].Version != 0 {
		t.Fatalf("history = %+v, %v", history, err)
	}
	result, err := e.revert(t, id)
	if err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(e.file); string(now) != string(before) || !strings.HasPrefix(result.HistoryID, "000002-") {
		t.Fatalf("revert of a 3a entry: %+v", result)
	}
	// A version 2 entry without the profile file and without documents is damaged.
	broken := `{"version":2,"id":"000003-` + strings.Trim(old, `"`)[:12] + `","action":"edit","profile":"dev/coder","audit":{"session":"s1","origin":"test"}}`
	wsPut(t, filepath.Join(dir, "000003-"+strings.Trim(old, `"`)[:12]+".json"), broken)
	if _, err := readHistoryEntry(e.w, "dev/coder", "000003-"+strings.Trim(old, `"`)[:12]); err == nil {
		t.Fatal("an empty entry was read")
	}
}

func jsonQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// A setting a self-edit changes never names a profile-own folder, at any level, so no
// in-process writer or sandbox bind reaches the documents and the history around the lock.
func TestProfileEditRefusesSettingsIntoProfileOwn(t *testing.T) {
	e := newEditFixture(t, `{"profile_self_edit":"full"}`, `{"profile_self_edit":"full"}`, editBase+`}`)
	for _, value := range []string{
		`"tool_storage_dir":"{profile_own}/documents"`,
		`"tool_storage_dir":"` + filepath.Join(e.w, ".jad", "profile-own", "dev", "other") + `"`,
		`"tool_storage_dir":"profile-own/dev/coder"`,
		`"sandbox":{"enabled":true,"backend":"bwrap","writable_paths":["{workspace}/.jad/profile-own"]}`,
	} {
		if _, err := e.edit(t, editBase+`,"settings":{`+value+`}}`); err == nil || !strings.Contains(err.Error(), "profile-own folder") {
			t.Errorf("%s: %v", value, err)
		}
	}
	if _, err := e.edit(t, editBase+`,"settings":{"max_tool_rounds":4}}`); err != nil {
		t.Fatal(err)
	}
}

// The trial answers only the prompt documents a profile and its session name; a document of the
// change another reader took from the disk is refused.
func TestDocumentOverlayNamesOtherReaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "documents", "a.md")
	overlay := newDocumentOverlay([]documentStep{{lexical: path, content: text("new")}, {lexical: filepath.Join(dir, "documents", "gone.md"), delete: true}})
	if got, err := overlay.read(path, 10); err != nil || string(got) != "new" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if _, err := overlay.read(filepath.Join(dir, "documents", "gone.md"), 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleted document: %v", err)
	}
	if _, err := overlay.read(path, 2); err == nil {
		t.Fatal("the limit was not applied")
	}
	config := SessionConfig{PromptDocuments: []string{path}}
	if outside := overlay.readOutside(config); outside != "" {
		t.Fatalf("the profile's own documents are answered by the overlay: %s", outside)
	}
	config.StateContext.Triggers = map[string]StateContextTrigger{"t": {PromptDocuments: []string{path}}}
	if outside := overlay.readOutside(config); outside != path {
		t.Fatalf("a trigger document: %q", outside)
	}
	nested := SessionConfig{AgentToolsets: map[string]SessionConfig{"x": {ProjectInstructionFiles: []string{path}}}}
	if outside := overlay.readOutside(nested); outside != path {
		t.Fatalf("an agent toolset's instruction file: %q", outside)
	}
}
