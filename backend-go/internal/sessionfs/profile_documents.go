package sessionfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/catalog"
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
	// kind is the folder the file is in: documents or models.
	kind *ownFileKind
	// parts are the path's segments below the kind's folder; lexical is the path as the load
	// names it ({profile_own}/<folder>/...), path where it was found ("" when absent).
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

// ownFileKind is a folder of the profile's own that edit writes file by file: its documents
// (3b) and its own models (3c.2). Both share the checks, the lock, the trial, the history and
// the publish order; they differ in the folder, the names and the size limit.
type ownFileKind struct {
	// folder is the folder below {profile_own}; argument the edit argument that names the
	// files (documents, models).
	folder, argument string
	// suffix is left out of a file's name (an own model is named by its ID, without .json).
	suffix string
	// limit caps each file and all files of the kind in one edit.
	limit int
	// parts turns the name an edit gives into the path's segments below folder.
	parts func(name string) ([]string, error)
}

var (
	ownDocumentKind = &ownFileKind{folder: profileDocumentsFolder, argument: "documents", limit: maxPromptDocumentBytes, parts: documentParts}
	ownModelKind    = &ownFileKind{folder: profileModelsFolder, argument: "models", suffix: ".json", limit: maxInternalJSONFileBytes, parts: ownModelParts}
	ownToolsetKind  = &ownFileKind{folder: profileToolsetsFolder, argument: "toolsets", suffix: ".json", limit: maxInternalJSONFileBytes, parts: ownToolsetParts}
)

// ownModelParts checks an own model ID (<category>/<name>, both catalog identifiers) and
// gives its file's segments below the models folder.
func ownModelParts(id string) ([]string, error) { return ownCatalogParts("own model", id) }

// ownToolsetParts is ownModelParts for an own toolset (phase 3c.3).
func ownToolsetParts(id string) ([]string, error) { return ownCatalogParts("own toolset", id) }

// ownCatalogParts checks the ID of an own catalog file (what: own model, own toolset).
func ownCatalogParts(what, id string) ([]string, error) {
	category, name, found := strings.Cut(id, "/")
	if !found {
		return nil, fmt.Errorf("an %s is named <category>/<name>", what)
	}
	if err := catalog.ValidateIdentifier(category); err != nil {
		return nil, fmt.Errorf("an %s's category %w", what, err)
	}
	if err := catalog.ValidateIdentifier(name); err != nil {
		return nil, fmt.Errorf("an %s's name %w", what, err)
	}
	return []string{category, name + ".json"}, nil
}

// folderOf is the folder of a file below the kind's folder, walked (and with create made
// 0700) from the real W/.jad down, refusing a link or a file on the way; "" when it does not
// exist and create is false.
func (k *ownFileKind) folderOf(workspace, profile string, parts []string, create bool) (string, error) {
	folders := append([]string{k.folder}, parts[:len(parts)-1]...)
	return ownFolder(workspace, profile, folders, create)
}

// prepareDocumentSteps checks every change of one kind (documents or own models) under the
// profile's lock, before anything is written (D2, D3): the name, the size (each file and all of
// them at most the kind's limit), that each existing target is a regular file reached without a
// link, and each precondition. Unchanged files are left out.
func prepareDocumentSteps(layout Layout, profile string, kind *ownFileKind, changes []ProfileDocumentChange) ([]documentStep, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	root := filepath.Join(layout.ProfileOwnDir(profile), kind.folder)
	argument := kind.argument
	var steps []documentStep
	var conflicts []string
	seen := make(map[string]bool, len(changes))
	total := 0
	for index, change := range changes {
		parts, err := kind.parts(change.Path)
		if err != nil {
			return nil, &ClientError{Message: fmt.Sprintf("%s[%d] %q: %v", argument, index, change.Path, err)}
		}
		relative := strings.TrimSuffix(strings.Join(parts, "/"), kind.suffix)
		if seen[relative] {
			return nil, &ClientError{Message: fmt.Sprintf("%s[%d]: %s is given twice", argument, index, relative)}
		}
		seen[relative] = true
		if change.Delete && change.Content != nil {
			return nil, &ClientError{Message: fmt.Sprintf("%s[%d] %s: give content or delete, not both", argument, index, relative)}
		}
		if !change.Delete {
			if len(change.Content) > kind.limit {
				return nil, &ClientError{Message: fmt.Sprintf("%s[%d] %s is larger than %d bytes", argument, index, relative, kind.limit)}
			}
			total += len(change.Content)
		}
		step := documentStep{kind: kind, parts: parts, lexical: filepath.Join(append([]string{root}, parts...)...), content: change.Content, delete: change.Delete}
		if !within(step.lexical, root) {
			return nil, &ClientError{Message: fmt.Sprintf("%s[%d] %s is not in {profile_own}/%s", argument, index, relative, kind.folder)}
		}
		step.record.Path = relative
		folder, err := kind.folderOf(layout.Workspace, profile, parts, false)
		if err != nil {
			return nil, &ClientError{Message: fmt.Sprintf("%s[%d] %s: %v", argument, index, relative, err)}
		}
		if folder != "" {
			path := filepath.Join(folder, parts[len(parts)-1])
			info, err := os.Lstat(path)
			switch {
			case err == nil && !info.Mode().IsRegular():
				return nil, &ClientError{Message: fmt.Sprintf("%s[%d] %s is a link or not a regular file; it is not changed through it", argument, index, relative)}
			case err == nil:
				old, err := readRegularFile(path, int64(kind.limit))
				if err != nil {
					return nil, fmt.Errorf("%s[%d] %s: %w", argument, index, relative, err)
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
			return nil, &ClientError{Message: fmt.Sprintf("%s[%d] %s does not exist: nothing to delete", argument, index, relative)}
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
		return nil, &CatalogError{Kind: CatalogPreconditionFailed, Message: argument + " changed or are not as given; read them again with config: " + strings.Join(conflicts, "; ")}
	}
	if total > kind.limit {
		return nil, &ClientError{Message: fmt.Sprintf("the %s of one edit are larger than %d bytes together; split the edit", argument, kind.limit)}
	}
	return steps, nil
}

// revertDocumentChanges are the changes of one kind that restore what entry touched (D7, records
// are the entry's documents or models): each file must now be as the entry left it (its new revision) or already as it was before (its
// old revision, left alone); any other state is a conflict, so nothing changed later is
// overwritten.
func revertDocumentChanges(layout Layout, profile string, kind *ownFileKind, entry ProfileHistoryEntry, records []ProfileDocumentRecord) ([]ProfileDocumentChange, error) {
	var changes []ProfileDocumentChange
	var conflicts []string
	for _, document := range records {
		parts, err := kind.parts(document.Path)
		if err != nil {
			return nil, fmt.Errorf("history entry %s is damaged: %v", entry.ID, err)
		}
		if document.OldRevision != "" && CatalogETag(document.Previous) != document.OldRevision {
			return nil, fmt.Errorf("history entry %s is damaged: %s does not hold its old revision", entry.ID, document.Path)
		}
		folder, err := kind.folderOf(layout.Workspace, profile, parts, false)
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
				contents, err := readRegularFile(path, int64(kind.limit))
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
		return nil, &ConflictError{Message: fmt.Sprintf("these %s changed after history entry %s, so it is not reverted (revert the newer entries first): %s", kind.argument, entry.ID, strings.Join(conflicts, ", "))}
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
	// A session load that reads while the files change, or what an undo leaves, reads again
	// (decider 3c.3b).
	defer profilePublishes.begin(profilePath)()
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
		folder, err := step.kind.folderOf(workspace, profile, step.parts, true)
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

// listProfileDocuments lists the own files of kind of the profile whose own folder is own
// (W/.jad/profile-own/<category>/<name>): the regular files below the kind's folder reached
// without a link, with their sizes and revisions, for config. A document is named by its path
// below documents, an own model by its ID (<category>/<name>; other files are not listed).
func listProfileDocuments(own, profile string, kind *ownFileKind) ([]ProfileDocument, error) {
	workspace := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(own))))
	if filepath.Join(workspace, ".jad", profileOwnFolder, filepath.FromSlash(profile)) != filepath.Clean(own) {
		return nil, fmt.Errorf("%s is not the own folder of %s", own, profile)
	}
	folder, err := ownFolder(workspace, profile, []string{kind.folder}, false)
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
		name := filepath.ToSlash(relative)
		if kind.suffix != "" {
			id, isJSON := strings.CutSuffix(name, kind.suffix)
			if parts, err := kind.parts(id); !isJSON || err != nil || filepath.Join(parts...) != relative {
				return nil
			}
			name = id
		}
		contents, err := readRegularFile(path, int64(kind.limit))
		if err != nil {
			return err
		}
		documents = append(documents, ProfileDocument{Path: name, Size: int64(len(contents)), Revision: CatalogETag(contents)})
		return nil
	})
	return documents, err
}
