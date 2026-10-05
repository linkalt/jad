package transcript

import "os"

// compactionMemo is the parsed compaction cache a history handle reuses until
// the cache files change. Re-parsing and re-validating every compacted item on
// each provider round costs time proportional to the compacted history, yet the
// files only change when a compaction or reset runs.
type compactionMemo struct {
	valid      bool
	generation uint64
	stamps     [2]os.FileInfo
	cache      *compactionCache
	// prefix is cache.Items as plain byte slices, shared read-only by readers.
	prefix [][]byte
	// tail is the expansion of cache.ActiveTail, computed on first use.
	tail     [][]byte
	tailDone bool
}

// activeTailItems returns the expanded active tail of cache, computing it once
// per memoized cache. The returned bytes are shared and read-only.
func (m *compactionMemo) activeTailItems(cache *compactionCache) ([][]byte, error) {
	if m.valid && m.cache == cache && m.tailDone {
		return m.tail, nil
	}
	var tail [][]byte
	for _, record := range cache.ActiveTail {
		expanded, err := expandTranscriptRecord(record)
		if err != nil {
			return nil, err
		}
		tail = append(tail, expanded...)
	}
	if m.valid && m.cache == cache {
		m.tail, m.tailDone = tail, true
	}
	return tail, nil
}

// readCompactionCacheMemo returns the compaction cache and its items. The memo
// is reused only while the in-process compaction counter and the stamps of the
// primary and backup cache files are unchanged; any other state is re-read.
// The returned item bytes are shared and must be treated as read-only.
func (s *TurnStore) readCompactionCacheMemo(memo *compactionMemo) (*compactionCache, [][]byte, error) {
	if s.options.CompactionCachePath == "" {
		return nil, nil, nil
	}
	// The counter and stamps are taken before the read, so a cache replaced
	// during the read leaves the memo looking stale and forces one more read.
	generation := s.compactionGeneration.Load()
	stamps, stampable := s.compactionCacheStamps()
	if stampable && memo.valid && memo.generation == generation && sameStamps(memo.stamps, stamps) {
		return memo.cache, memo.prefix, nil
	}
	cache, err := s.readCompactionCache()
	if err != nil {
		*memo = compactionMemo{}
		return nil, nil, err
	}
	var prefix [][]byte
	if cache != nil {
		prefix = make([][]byte, len(cache.Items))
		for index, item := range cache.Items {
			prefix[index] = item
		}
	}
	if !stampable {
		*memo = compactionMemo{}
		return cache, prefix, nil
	}
	*memo = compactionMemo{valid: true, generation: generation, stamps: stamps, cache: cache, prefix: prefix}
	return cache, prefix, nil
}

// compactionCacheStamps stats the primary and backup cache files. A missing
// file is a nil stamp. Any other stat error makes the state unstampable.
func (s *TurnStore) compactionCacheStamps() ([2]os.FileInfo, bool) {
	var stamps [2]os.FileInfo
	for index, path := range []string{s.options.CompactionCachePath, s.options.CompactionCachePath + ".backup"} {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return stamps, false
		}
		stamps[index] = info
	}
	return stamps, true
}

func sameStamps(left, right [2]os.FileInfo) bool {
	for index := range left {
		a, b := left[index], right[index]
		if a == nil || b == nil {
			if a != b {
				return false
			}
			continue
		}
		if !os.SameFile(a, b) || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
			return false
		}
	}
	return true
}
