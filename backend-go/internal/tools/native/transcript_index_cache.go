package native

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
)

// transcriptIndexTailBytes is how much of the indexed part of a file, ending where the index
// ended, the cache compares before it trusts an index: a front-compaction keeps the first line
// (the identity) and cuts the file, so a file that was cut and then grew past the old end has
// other bytes there.
const transcriptIndexTailBytes = 4096

// transcriptIndexEntry is the turn table of one transcript up to end, the end of its last
// complete line, for the file whose first line hashes to identity.
type transcriptIndexEntry struct {
	identity string
	end      int64
	tail     [32]byte
	turns    []transcriptTurn
	used     uint64
}

// transcriptIndexCache keeps the turn tables of the transcripts read lately, so a call parses
// only what was appended since the last one instead of every line of the file. Tables are never
// changed in place: an update stores a new slice, so a reader's copy stays valid.
type transcriptIndexCache struct {
	mu      sync.Mutex
	entries map[string]*transcriptIndexEntry
	clock   uint64
}

var transcriptIndexes = &transcriptIndexCache{entries: map[string]*transcriptIndexEntry{}}

// transcriptIndexKey names a table by file and line limit: a line over the limit is not parsed,
// so the limit can change where turns start.
func transcriptIndexKey(path string, maxLine int64) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return path + "\x00" + strconv.FormatInt(maxLine, 10)
}

// transcriptTail hashes file[end-transcriptIndexTailBytes, end).
func transcriptTail(file *os.File, end int64) ([32]byte, error) {
	start := max(end-transcriptIndexTailBytes, 0)
	buffer := make([]byte, end-start)
	if _, err := file.ReadAt(buffer, start); err != nil && !errors.Is(err, io.EOF) {
		return [32]byte{}, err
	}
	return sha256.Sum256(buffer), nil
}

// turns returns the turn table of file[0:size) and where its last complete line ends, the same
// as indexTranscriptTurns. With capacity > 0 it starts from the table kept for the file when that
// table still describes the file's start (same identity, the bytes before its end unchanged) and
// parses from the start of its last turn, which an append may still extend or renumber; it then
// keeps the new table, dropping the least recently used ones over capacity. With capacity 0 it
// neither reads nor keeps a table.
func (c *transcriptIndexCache) turns(ctx context.Context, file *os.File, path, identity string, size, maxLine int64, capacity int) ([]transcriptTurn, int64, error) {
	if capacity <= 0 {
		return indexTranscriptTurns(ctx, file, size, maxLine)
	}
	key := transcriptIndexKey(path, maxLine)
	c.mu.Lock()
	cached := c.entries[key]
	c.mu.Unlock()
	var prefix []transcriptTurn
	var turns []transcriptTurn
	var end int64
	var err error
	if cached != nil && cached.identity == identity && cached.end <= size && len(cached.turns) > 0 {
		if tail, tailErr := transcriptTail(file, cached.end); tailErr == nil && tail == cached.tail {
			last := cached.turns[len(cached.turns)-1]
			prefix = cached.turns[:len(cached.turns)-1]
			turns, end, err = indexTranscriptTurnsFrom(ctx, file, last.off, size, maxLine, last.number)
		} else {
			cached = nil
		}
	} else {
		cached = nil
	}
	if cached == nil {
		turns, end, err = indexTranscriptTurns(ctx, file, size, maxLine)
	}
	if err != nil {
		return nil, 0, err
	}
	turns = slices.Concat(prefix, turns)
	if len(turns) == 0 {
		return turns, end, nil
	}
	tail, err := transcriptTail(file, end)
	if err != nil {
		return turns, end, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock++
	c.entries[key] = &transcriptIndexEntry{identity: identity, end: end, tail: tail, turns: turns, used: c.clock}
	for len(c.entries) > capacity {
		oldest := ""
		for name, entry := range c.entries {
			if oldest == "" || entry.used < c.entries[oldest].used {
				oldest = name
			}
		}
		delete(c.entries, oldest)
	}
	return turns, end, nil
}

// transcriptLineMayHead reports whether a line can carry a field indexTranscriptTurnsFrom reads:
// a user role (which starts a turn) or a session_turn (which numbers it). Transcripts are written
// by encoding/json, which writes these keys and the value "user" as plain bytes, so a line without
// them decodes to an empty head and is not parsed. A false positive only costs the parse.
func transcriptLineMayHead(data []byte) bool {
	return bytes.Contains(data, []byte(`"session_turn"`)) || (bytes.Contains(data, []byte(`"role"`)) && bytes.Contains(data, []byte(`"user"`)))
}
