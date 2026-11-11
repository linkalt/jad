package sessionfs

import (
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
	return &CatalogScans{memo: &scanMemo{results: make(map[scanKey]scanResult), checks: make(map[scanKey]fileCheck)}}
}

// recorder is a view that shares the memo and records what its own load reads: a
// load that reuses a scan made for an earlier session still gets that scan's folders.
func (s *CatalogScans) recorder() *CatalogScans {
	return &CatalogScans{memo: s.memo, recording: true}
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
