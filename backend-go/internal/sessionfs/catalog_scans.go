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
	mu      sync.Mutex
	results map[scanKey]scanResult

	// recording is set for a load whose SettingsStamp the caller wants.
	recording bool
	files     []fileStamp
}

type scanKey struct{ kind, dir string }

type scanResult struct {
	catalog *catalog.Catalog
	err     error
}

// NewCatalogScans returns an empty memo.
func NewCatalogScans() *CatalogScans {
	return &CatalogScans{results: make(map[scanKey]scanResult)}
}

// Scan is catalog.Scan, remembered. Scan errors are remembered too, so every session of
// one request sees the same directory state.
func (s *CatalogScans) Scan(kind, dir string) (*catalog.Catalog, error) {
	if s == nil {
		return catalog.Scan(kind, dir)
	}
	key := scanKey{kind: kind, dir: dir}
	s.mu.Lock()
	defer s.mu.Unlock()
	if result, seen := s.results[key]; seen {
		return result.catalog, result.err
	}
	var observe func(string)
	if s.recording {
		observe = func(folder string) { s.files = append(s.files, statStamp(folder)) }
	}
	scanned, err := catalog.ScanObserved(kind, dir, observe)
	s.results[key] = scanResult{catalog: scanned, err: err}
	return scanned, err
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

func (s *CatalogScans) stamp() SettingsStamp {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SettingsStamp{files: append([]fileStamp(nil), s.files...)}
}
