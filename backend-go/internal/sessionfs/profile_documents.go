package sessionfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A profile's own prompt documents (docs/design_workspace_layout.md §13, decider phase 3b D1 to
// D7): the profile tool's edit creates, replaces and deletes files in
// W/.jad/profile-own/<category>/<name>/documents only, in one revision with the profile file
// and under its lock; the history entry keeps every touched document as it was.

// profileDocumentsFolder is the folder of the profile's own documents in its own folder:
// read-only to its sandboxed calls and hidden from other profiles when a sandbox is on.
const profileDocumentsFolder = "documents"

// Document changes, as results and history entries name them.
const (
	ProfileDocumentCreated  = "created"
	ProfileDocumentReplaced = "replaced"
	ProfileDocumentDeleted  = "deleted"
)

// ProfileDocumentChange is one document of an edit: Content creates or replaces it, Delete
// removes it. Path is relative to the documents folder. IfMatch is the revision config showed
// for an existing document; empty means the document must not exist yet.
type ProfileDocumentChange struct {
	Path    string
	Content []byte
	Delete  bool
	IfMatch string
}

// ProfileDocument is one own document as config lists it.
type ProfileDocument struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Revision string `json:"revision"`
}

// ProfileDocumentRecord is one document a change touched: its revision before and after (left
// out: the document was absent) and, in a history entry only, its bytes before.
type ProfileDocumentRecord struct {
	Path        string `json:"path"`
	Change      string `json:"change"`
	OldRevision string `json:"old_revision,omitempty"`
	NewRevision string `json:"new_revision,omitempty"`
	Previous    []byte `json:"previous,omitempty"`
}

// documentStep is a document change checked under the lock: the state it was read in and what
// it becomes.
type documentStep struct {
	record ProfileDocumentRecord
	// parts are the path's segments below the documents folder; lexical is the path as the
	// load names it ({profile_own}/documents/...), path where it was found ("" when absent).
	parts   []string
	lexical string
	path    string
	info    os.FileInfo
	content []byte
	delete  bool
}

// documentParts checks a document path given relative to the documents folder: no absolute
// path, no empty, . or .. segment, no segment starting with a dot (temporary files do not
// collide) and no backslash or NUL.
func documentParts(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("a document path is needed")
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\\x00") {
		return nil, errors.New("a document path is relative to {profile_own}/documents, with / between folders")
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || strings.HasPrefix(part, ".") {
			return nil, fmt.Errorf("a document path has no empty segment and no segment starting with a dot (%q)", part)
		}
	}
	if !filepath.IsLocal(filepath.Join(parts...)) {
		return nil, errors.New("a document path stays in {profile_own}/documents")
	}
	return parts, nil
}

// documentFolder is the folder of a document below documents, walked (and with create made
// 0700) from the real W/.jad down, refusing a link or a file on the way; "" when it does not
// exist and create is false.
func documentFolder(workspace, profile string, parts []string, create bool) (string, error) {
	folders := append([]string{profileDocumentsFolder}, parts[:len(parts)-1]...)
	return ownFolder(workspace, profile, folders, create)
}

// prepareDocumentSteps checks every document change under the profile's lock, before anything
// is written (D2, D3): the path, the size (each document and all of them at most
// maxPromptDocumentBytes), that each existing target is a regular file reached without a link,
// and each precondition. Unchanged documents are left out.
func prepareDocumentSteps(layout Layout, profile string, changes []ProfileDocumentChange) ([]documentStep, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	root := filepath.Join(layout.ProfileOwnDir(profile), profileDocumentsFolder)
	var steps []documentStep
	var conflicts []string
	seen := make(map[string]bool, len(changes))
	total := 0
	for index, change := range changes {
		parts, err := documentParts(change.Path)
		if err != nil {
			return nil, &ClientError{Message: fmt.Sprintf("documents[%d] %q: %v", index, change.Path, err)}
		}
		relative := strings.Join(parts, "/")
		if seen[relative] {
			return nil, &ClientError{Message: fmt.Sprintf("documents[%d]: %s is given twice", index, relative)}
		}
		seen[relative] = true
		if change.Delete && change.Content != nil {
			return nil, &ClientError{Message: fmt.Sprintf("documents[%d] %s: give content or delete, not both", index, relative)}
		}
		if !change.Delete {
			if len(change.Content) > maxPromptDocumentBytes {
				return nil, &ClientError{Message: fmt.Sprintf("documents[%d] %s is larger than %d bytes", index, relative, maxPromptDocumentBytes)}
			}
			total += len(change.Content)
		}
		step := documentStep{parts: parts, lexical: filepath.Join(append([]string{root}, parts...)...), content: change.Content, delete: change.Delete}
		if !within(step.lexical, root) {
			return nil, &ClientError{Message: fmt.Sprintf("documents[%d] %s is not in {profile_own}/documents", index, relative)}
		}
		step.record.Path = relative
		folder, err := documentFolder(layout.Workspace, profile, parts, false)
		if err != nil {
			return nil, &ClientError{Message: fmt.Sprintf("documents[%d] %s: %v", index, relative, err)}
		}
		if folder != "" {
			path := filepath.Join(folder, parts[len(parts)-1])
			info, err := os.Lstat(path)
			switch {
			case err == nil && !info.Mode().IsRegular():
				return nil, &ClientError{Message: fmt.Sprintf("documents[%d] %s is a link or not a regular file; it is not changed through it", index, relative)}
			case err == nil:
				old, err := readRegularFile(path, maxPromptDocumentBytes)
				if err != nil {
					return nil, fmt.Errorf("documents[%d] %s: %w", index, relative, err)
				}
				step.path, step.info = path, info
				step.record.OldRevision, step.record.Previous = CatalogETag(old), old
			case !errors.Is(err, fs.ErrNotExist):
				return nil, err
			}
		}
		ifMatch := strings.TrimSpace(change.IfMatch)
		switch exists := step.path != ""; {
		case !exists && change.Delete && ifMatch == "":
			return nil, &ClientError{Message: fmt.Sprintf("documents[%d] %s does not exist: nothing to delete", index, relative)}
		case exists && ifMatch == "":
			conflicts = append(conflicts, relative+" exists: give the revision config shows as its if_match")
			continue
		case !exists && ifMatch != "":
			conflicts = append(conflicts, relative+" does not exist any more: leave if_match out to create it")
			continue
		case exists && !sameRevision(ifMatch, step.record.OldRevision):
			conflicts = append(conflicts, fmt.Sprintf("%s changed since revision %s was read (now %s)", relative, ifMatch, step.record.OldRevision))
			continue
		}
		switch {
		case change.Delete:
			step.record.Change = ProfileDocumentDeleted
		case step.path == "":
			step.record.Change, step.record.NewRevision = ProfileDocumentCreated, CatalogETag(change.Content)
		default:
			step.record.Change, step.record.NewRevision = ProfileDocumentReplaced, CatalogETag(change.Content)
			if bytes.Equal(change.Content, step.record.Previous) {
				continue
			}
		}
		steps = append(steps, step)
	}
	if len(conflicts) > 0 {
		return nil, &CatalogError{Kind: CatalogPreconditionFailed, Message: "documents changed or are not as given; read them again with config: " + strings.Join(conflicts, "; ")}
	}
	if total > maxPromptDocumentBytes {
		return nil, &ClientError{Message: fmt.Sprintf("the documents of one edit are larger than %d bytes together; split the edit", maxPromptDocumentBytes)}
	}
	return steps, nil
}

// revertDocumentChanges are the document changes that restore what entry touched (D7): each
// document must now be as the entry left it (its new revision) or already as it was before (its
// old revision, left alone); any other state is a conflict, so nothing changed later is
// overwritten.
func revertDocumentChanges(layout Layout, profile string, entry ProfileHistoryEntry) ([]ProfileDocumentChange, error) {
	var changes []ProfileDocumentChange
	var conflicts []string
	for _, document := range entry.Documents {
		parts, err := documentParts(document.Path)
		if err != nil {
			return nil, fmt.Errorf("history entry %s is damaged: %v", entry.ID, err)
		}
		if document.OldRevision != "" && CatalogETag(document.Previous) != document.OldRevision {
			return nil, fmt.Errorf("history entry %s is damaged: %s does not hold its old revision", entry.ID, document.Path)
		}
		folder, err := documentFolder(layout.Workspace, profile, parts, false)
		if err != nil {
			return nil, &ClientError{Message: fmt.Sprintf("%s: %v", document.Path, err)}
		}
		current := ""
		if folder != "" {
			path := filepath.Join(folder, parts[len(parts)-1])
			if info, err := os.Lstat(path); err == nil {
				if !info.Mode().IsRegular() {
					return nil, &ClientError{Message: fmt.Sprintf("%s is a link or not a regular file; it is not changed through it", document.Path)}
				}
				contents, err := readRegularFile(path, maxPromptDocumentBytes)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", document.Path, err)
				}
				current = CatalogETag(contents)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		switch current {
		case document.OldRevision:
			continue
		case document.NewRevision:
		default:
			conflicts = append(conflicts, document.Path)
			continue
		}
		change := ProfileDocumentChange{Path: document.Path, IfMatch: current, Delete: document.OldRevision == ""}
		if !change.Delete {
			change.Content = append([]byte{}, document.Previous...)
		}
		changes = append(changes, change)
	}
	if len(conflicts) > 0 {
		return nil, &ConflictError{Message: fmt.Sprintf("these documents changed after history entry %s, so it is not reverted (revert the newer entries first): %s", entry.ID, strings.Join(conflicts, ", "))}
	}
	return changes, nil
}

// documentOverlay holds what a trial load reads in place of the documents an edit would write
// or delete (D4), by cleaned path and by path after links; a nil entry is a deleted document.
type documentOverlay struct {
	files map[string]*[]byte
}

func newDocumentOverlay(steps []documentStep) *documentOverlay {
	if len(steps) == 0 {
		return nil
	}
	overlay := &documentOverlay{files: make(map[string]*[]byte, 2*len(steps))}
	for _, step := range steps {
		var contents *[]byte
		if !step.delete {
			contents = &step.content
		}
		overlay.files[filepath.Clean(step.lexical)] = contents
		overlay.files[resolveExisting(filepath.Clean(step.lexical))] = contents
	}
	return overlay
}

// lookup finds path in the overlay as written or after links.
func (o *documentOverlay) lookup(path string) (*[]byte, bool) {
	if o == nil {
		return nil, false
	}
	path = filepath.Clean(path)
	if contents, found := o.files[path]; found {
		return contents, true
	}
	contents, found := o.files[resolveExisting(path)]
	return contents, found
}

// read is readRegularFile, answered from the overlay for a document it holds.
func (o *documentOverlay) read(path string, limit int64) ([]byte, error) {
	contents, found := o.lookup(path)
	if !found {
		return readRegularFile(path, limit)
	}
	if contents == nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	if int64(len(*contents)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return *contents, nil
}

// readOutside names a document of the edit that the trial configuration read through a reader
// the overlay does not answer for (statecontext triggers, sub-agents, project instructions):
// for those the trial judged the disk, not the change, so the edit is refused (fail closed).
func (o *documentOverlay) readOutside(config SessionConfig) string {
	if o == nil {
		return ""
	}
	documents := append([]string(nil), config.ProjectInstructionFiles...)
	for _, trigger := range config.StateContext.Triggers {
		documents = append(documents, trigger.PromptDocuments...)
	}
	if subagent := config.NativeSubagent; subagent != nil {
		for _, agent := range subagent.Agents {
			documents = append(documents, agent.PromptDocuments...)
		}
	}
	for _, document := range documents {
		if _, found := o.lookup(document); found {
			return document
		}
	}
	for _, derived := range config.AgentToolsets {
		if document := o.readOutside(derived); document != "" {
			return document
		}
	}
	return ""
}

// editPublishFault, set by tests only, fails the publish after the named step.
var editPublishFault func(step string) error

// appliedChange is a file the publish changed, with what undoes it.
type appliedChange struct {
	path     string
	previous []byte
	// existed: the file was there before (replaced or deleted); deleted: it was removed.
	existed, deleted bool
}

// publishProfileEdit publishes an edit whose history entry is on disk (D6): new and changed
// documents first, the profile file next, deleted documents last (the old profile never loses a
// document it names), then every touched folder is synced. A failure undoes what was published;
// the returned error says whether that finished.
func publishProfileEdit(workspace, profile string, steps []documentStep, profilePath string, candidate, current []byte, before os.FileInfo, profileChanged bool) error {
	var applied []appliedChange
	var folders []string
	fail := func(err error) error {
		if len(applied) == 0 {
			return fmt.Errorf("%w; nothing was published", err)
		}
		if undo := rollbackChanges(applied); undo != nil {
			return fmt.Errorf("%w; undoing what was published did not finish: %v", err, undo)
		}
		return fmt.Errorf("%w; what was published was undone", err)
	}
	fault := func(step string) error {
		if editPublishFault == nil {
			return nil
		}
		return editPublishFault(step)
	}
	for _, step := range steps {
		if step.delete {
			continue
		}
		folder, err := documentFolder(workspace, profile, step.parts, true)
		if err != nil {
			return fail(err)
		}
		path := filepath.Join(folder, step.parts[len(step.parts)-1])
		if step.path != "" {
			if path != step.path {
				return fail(&ConflictError{Message: fmt.Sprintf("the folder of %s changed during the edit", step.record.Path)})
			}
			err = writeFileIfUnchanged(path, step.content, step.info, true)
		} else {
			err = createFileExclusive(path, step.content, true)
		}
		if err != nil {
			return fail(fmt.Errorf("write %s: %w", step.record.Path, err))
		}
		applied = append(applied, appliedChange{path: path, previous: step.record.Previous, existed: step.path != ""})
		folders = appendNewStrings(folders, folder)
		if err := fault("document"); err != nil {
			return fail(err)
		}
	}
	if profileChanged {
		if err := writeFileIfUnchanged(profilePath, candidate, before, true); err != nil {
			return fail(err)
		}
		applied = append(applied, appliedChange{path: profilePath, previous: current, existed: true})
		folders = appendNewStrings(folders, filepath.Dir(profilePath))
		if err := fault("profile"); err != nil {
			return fail(err)
		}
	}
	for _, step := range steps {
		if !step.delete {
			continue
		}
		now, err := os.Lstat(step.path)
		if err != nil || !os.SameFile(now, step.info) || now.Size() != step.info.Size() || !now.ModTime().Equal(step.info.ModTime()) {
			return fail(&ConflictError{Message: fmt.Sprintf("%s changed during the edit", step.record.Path)})
		}
		if err := os.Remove(step.path); err != nil {
			return fail(fmt.Errorf("delete %s: %w", step.record.Path, err))
		}
		applied = append(applied, appliedChange{path: step.path, previous: step.record.Previous, existed: true, deleted: true})
		folders = appendNewStrings(folders, filepath.Dir(step.path))
		if err := fault("delete"); err != nil {
			return fail(err)
		}
	}
	for _, folder := range folders {
		if err := syncDirectory(folder); err != nil {
			return fail(err)
		}
	}
	return nil
}

// rollbackChanges undoes applied, newest first: a created document is removed, a replaced file
// gets its old bytes back and a deleted document is made again.
func rollbackChanges(applied []appliedChange) error {
	var errs []error
	var folders []string
	for index := len(applied) - 1; index >= 0; index-- {
		change := applied[index]
		var err error
		switch {
		case !change.existed:
			err = os.Remove(change.path)
		case change.deleted:
			err = createFileExclusive(change.path, change.previous, true)
		default:
			var now os.FileInfo
			if now, err = os.Stat(change.path); err == nil {
				err = writeFileIfUnchanged(change.path, change.previous, now, true)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", change.path, err))
		}
		folders = appendNewStrings(folders, filepath.Dir(change.path))
	}
	for _, folder := range folders {
		if err := syncDirectory(folder); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// listProfileDocuments lists the own documents of the profile whose own folder is own
// (W/.jad/profile-own/<category>/<name>): the regular files below its documents folder
// reached without a link, with their sizes and revisions, for config.
func listProfileDocuments(own, profile string) ([]ProfileDocument, error) {
	workspace := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(own))))
	if filepath.Join(workspace, ".jad", profileOwnFolder, filepath.FromSlash(profile)) != filepath.Clean(own) {
		return nil, fmt.Errorf("%s is not the own folder of %s", own, profile)
	}
	folder, err := ownFolder(workspace, profile, []string{profileDocumentsFolder}, false)
	if err != nil || folder == "" {
		return []ProfileDocument{}, err
	}
	documents := []ProfileDocument{}
	err = filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == folder || !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(folder, path)
		if err != nil {
			return err
		}
		contents, err := readRegularFile(path, maxPromptDocumentBytes)
		if err != nil {
			return err
		}
		documents = append(documents, ProfileDocument{Path: filepath.ToSlash(relative), Size: int64(len(contents)), Revision: CatalogETag(contents)})
		return nil
	})
	return documents, err
}
