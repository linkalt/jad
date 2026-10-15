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
	scanned, err := catalog.Scan(kind, dir)
	s.results[key] = scanResult{catalog: scanned, err: err}
	return scanned, err
}
