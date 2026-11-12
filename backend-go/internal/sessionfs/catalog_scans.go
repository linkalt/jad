package sessionfs

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"jad/backend-go/internal/catalog"
)

// CatalogScans remembers the catalog scans made through it, so loading many sessions
// in one request reads each catalog directory once. It is meant to live for one
// request: a directory changed after its first scan is not seen again until a new
// CatalogScans is made. A nil *CatalogScans scans every time.
type CatalogScans struct {
	memo *scanMemo

	// recording is set on the view of one load whose SettingsStamp the caller wants.
	recording bool
	mu        sync.Mutex
	files     []fileStamp
}

// scanMemo is the part shared by a CatalogScans and the recording views made from it.
type scanMemo struct {
	mu      sync.Mutex
	results map[scanKey]scanResult
	// checks are the verdicts of single-file checks (see check), by kind and path.
	checks map[scanKey]fileCheck
	// templates are the sandbox template folders listed, by folder.
	templates map[string]templateFolder
	// overlay holds the contents a trial load reads in place of a profile file on disk, by
	// cleaned path (profile edit, decider S4): the candidate is judged before it is written.
	overlay map[string][]byte
	// ownFiles holds the own model and toolset files a trial load reads in place of the disk,
	// by kind, profile and ID; nil contents is a file the edit deletes (decider 3c C3).
	ownFiles map[*ownFileKind]map[string]map[string]*[]byte
	// since is the self-edit publish generation when the memo was made, before it read
	// anything: a load that reuses what the memo read checks against it (decider 3c.3b).
	since uint64
}

// templateFolder is a listed sandbox template folder: the files its listing left out for their
// names, with the folder stamped just before the listing.
type templateFolder struct {
	skipped []catalog.Skipped
	stamp   fileStamp
}

// fileCheck is the verdict of one file check with the stamp taken just before the file was
// read, so every load that reuses the verdict still stamps what the verdict was made from.
type fileCheck struct {
	stamp fileStamp
	err   error
}

type scanKey struct{ kind, dir string }

type scanResult struct {
	catalog *catalog.Catalog
	err     error
	// folders are the folders the scan listed, stamped just before each listing.
	folders []fileStamp
}

// NewCatalogScans returns an empty memo.
func NewCatalogScans() *CatalogScans {
	return &CatalogScans{memo: &scanMemo{results: make(map[scanKey]scanResult), checks: make(map[scanKey]fileCheck), templates: make(map[string]templateFolder), since: profilePublishes.now()}}
}

// trial reports a memo of an edit's trial load, which reads the candidate in place of the disk.
func (s *CatalogScans) trial() bool {
	return s.memo.overlay != nil || s.memo.ownFiles != nil
}

// afresh is a view with an empty memo that records like s, for a load that reads again
// because what s's memo holds may be of two revisions of a profile.
func (s *CatalogScans) afresh() *CatalogScans {
	fresh := NewCatalogScans()
	fresh.recording = s.recording
	return fresh
}

// newTrialScans is a memo whose loads read contents in place of the profile file at path.
func newTrialScans(path string, contents []byte) *CatalogScans {
	scans := NewCatalogScans()
	scans.memo.overlay = map[string][]byte{filepath.Clean(path): contents}
	return scans
}

// ownModelOverlay is the trial's own model files of profile, by model ID (nil contents: the
// edit deletes it); nil outside a trial.
func (s *CatalogScans) ownModelOverlay(profile string) map[string]*[]byte {
	return s.ownFileOverlay(ownModelKind, profile)
}

// ownFileOverlay is the trial's own files of kind of profile, by ID; nil outside a trial.
func (s *CatalogScans) ownFileOverlay(kind *ownFileKind, profile string) map[string]*[]byte {
	if s == nil {
		return nil
	}
	return s.memo.ownFiles[kind][profile]
}

// overlayOwnFiles makes the trial read the own model and toolset files of steps in place of
// the disk.
func (s *CatalogScans) overlayOwnFiles(profile string, steps []documentStep) {
	for _, kind := range []*ownFileKind{ownModelKind, ownToolsetKind} {
		s.overlayOwnKind(kind, profile, steps)
	}
}

func (s *CatalogScans) overlayOwnKind(kind *ownFileKind, profile string, steps []documentStep) {
	files := map[string]*[]byte{}
	for _, step := range steps {
		if step.kind != kind {
			continue
		}
		var contents *[]byte
		if !step.delete {
			contents = &step.content
		}
		files[step.record.Path] = contents
	}
	if len(files) > 0 {
		if s.memo.ownFiles == nil {
			s.memo.ownFiles = map[*ownFileKind]map[string]map[string]*[]byte{}
		}
		s.memo.ownFiles[kind] = map[string]map[string]*[]byte{profile: files}
	}
}

// readProfileOf reads a profile for judging it in a session's scope (judgeScope), with the
// overlay of a trial load: the profile at path is read from the overlay when it holds that
// file. A nil scans reads the disk. effortCheck is the profile's checkModelEffortOnly: a
// model_default whose model file fails the model rules does not fail the profile (the model
// is reported on its own), an effort the model does not offer still does.
func (s *CatalogScans) readProfileOf(path string, allModels *catalog.Catalog, effortCheck func(modelPath, effort string) error) (layoutProfileFile, *catalog.Catalog, error) {
	if s != nil && s.memo.overlay != nil {
		if contents, found := s.memo.overlay[filepath.Clean(path)]; found {
			var profile layoutProfileFile
			if err := parseLayoutFile(path, contents, &profile); err != nil {
				return layoutProfileFile{}, nil, err
			}
			return checkProfileWith(path, profile, allModels, effortCheck)
		}
	}
	return readProfileWith(path, allModels, effortCheck)
}

// recorder is a view that shares the memo and records what its own load reads: a
// load that reuses a scan made for an earlier session still gets that scan's folders.
func (s *CatalogScans) recorder() *CatalogScans {
	return &CatalogScans{memo: s.memo, recording: true}
}

// sharedScopeNames are the scopes of the shared catalogs, in folder order: G's folder, then the
// workspace's own. A profile's own models are a scope of their own, added for its load only
// (Layout.profileModels), never a folder here.
var sharedScopeNames = []string{catalog.ScopeGlobal, catalog.ScopeWorkspace}

// scanScopes scans the folder of each scope ("" is no scope) and merges them; one folder is
// Scan as it is.
func (s *CatalogScans) scanScopes(kind string, dirs ...string) (*catalog.Catalog, error) {
	scopes, err := s.scanScopeList(kind, dirs...)
	if err != nil {
		return nil, err
	}
	return catalog.MergeScopes(kind, scopes...), nil
}

// scanScopeList scans the folder of each shared scope: the first folder is G's, the second the
// workspace's ("global:" and "workspace:" pick them). More folders than shared scopes is an
// error: a folder is never named a scope it is not.
func (s *CatalogScans) scanScopeList(kind string, dirs ...string) ([]catalog.Scope, error) {
	if len(dirs) > len(sharedScopeNames) {
		return nil, fmt.Errorf("%s catalog: %d folders given, but only %d shared scopes (%s) exist", kind, len(dirs), len(sharedScopeNames), strings.Join(sharedScopeNames, ", "))
	}
	scopes := make([]catalog.Scope, 0, len(dirs))
	for index, dir := range dirs {
		if dir == "" && index > 0 {
			continue
		}
		scanned, err := s.Scan(kind, dir)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, catalog.Scope{Name: sharedScopeNames[index], Catalog: scanned})
	}
	return scopes, nil
}

// Scan is catalog.Scan, remembered. Scan errors are remembered too, so every session of
// one request sees the same directory state.
func (s *CatalogScans) Scan(kind, dir string) (*catalog.Catalog, error) {
	if s == nil {
		return catalog.Scan(kind, dir)
	}
	key := scanKey{kind: kind, dir: dir}
	s.memo.mu.Lock()
	result, seen := s.memo.results[key]
	if !seen {
		var folders []fileStamp
		scanned, err := catalog.ScanObserved(kind, dir, func(folder string) { folders = append(folders, statStamp(folder)) })
		result = scanResult{catalog: scanned, err: err, folders: folders}
		s.memo.results[key] = result
	}
	s.memo.mu.Unlock()
	if s.recording {
		s.mu.Lock()
		s.files = append(s.files, result.folders...)
		s.mu.Unlock()
	}
	return result.catalog, result.err
}

// skippedTemplates lists the sandbox template folder once per memo and returns the template
// files it leaves out for their names. A recording view stamps the folder, so renaming such a
// file (or adding one) changes the stamp of every load that listed the folder. A nil scans lists
// every time.
func (s *CatalogScans) skippedTemplates(dir string) []catalog.Skipped {
	if s == nil {
		_, skipped := scanSandboxTemplates(dir)
		return skipped
	}
	s.memo.mu.Lock()
	folder, seen := s.memo.templates[dir]
	if !seen {
		folder.stamp = statStamp(dir)
		_, folder.skipped = scanSandboxTemplates(dir)
		s.memo.templates[dir] = folder
	}
	s.memo.mu.Unlock()
	if s.recording {
		s.add([]fileStamp{folder.stamp})
	}
	return folder.skipped
}

// check runs a check of the file at path, once per kind and path for the life of the memo,
// and stamps the file for a recording view like noteFile. A nil scans runs it every time.
func (s *CatalogScans) check(kind, path string, run func(path string) error) error {
	if s == nil {
		return run(path)
	}
	key := scanKey{kind: kind, dir: path}
	s.memo.mu.Lock()
	result, seen := s.memo.checks[key]
	if !seen {
		result = fileCheck{stamp: statStamp(path)}
		result.err = run(path)
		s.memo.checks[key] = result
	}
	s.memo.mu.Unlock()
	if s.recording {
		s.add([]fileStamp{result.stamp})
	}
	return result.err
}

// noteFile stamps path before the caller reads it. A nil or non-recording scans does
// nothing.
func (s *CatalogScans) noteFile(path string) {
	if s == nil || !s.recording {
		return
	}
	stamp := statStamp(path)
	s.mu.Lock()
	s.files = append(s.files, stamp)
	s.mu.Unlock()
}

func (s *CatalogScans) add(stamps []fileStamp) {
	s.mu.Lock()
	s.files = append(s.files, stamps...)
	s.mu.Unlock()
}

func (s *CatalogScans) stamp() SettingsStamp {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SettingsStamp{files: append([]fileStamp(nil), s.files...)}
}
