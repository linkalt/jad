package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"os"
	"path/filepath"

	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/sessionfs"
)

// A fork copies the first end bytes of its source's transcript. When the source's totals rest on
// a usage baseline entry (a rewrite removed records), the fork carries that entry over, so the
// removed history's spend is not lost to the fork. The entry's summary is the usage of the
// removed records plus the whole first Offset bytes of the transcript, and cannot be split by
// prefix: it is carried over only when the fork keeps all of those bytes (end >= Offset) and
// the copied bytes provably are them. A fork that cuts inside the covered prefix gets no entry
// and counts the records it copied, as a fork of a source with no baseline always did. What a
// rewrite removed may lie before the prefix (compaction) or after it (front compaction), so the
// removed part cannot be told from the part that a cut keeps.
//
// The fork's entry names the file the source's rewrite replaced, as the source's entry does:
// it does not fit a file that begins with those bytes, which a fork of the source never does.

// forkBaselineEntry returns the entry of the source's baseline that a fork copying the first end
// bytes of the source transcript can carry over, or nil. Nothing is carried over for an empty
// copy: it holds none of the history.
func (s *sharedSession) forkBaselineEntry(config sessionfs.SessionConfig, end int64) *usageBaselineEntry {
	if end <= 0 || sessionfs.UsageBaselinePath(config.UsageCachePath) == "" {
		return nil
	}
	stat, err := statFile(config.MessagesPath)
	if err != nil {
		return nil
	}
	entry, ok := s.matchUsageBaseline(config, stat)
	if !ok || entry.Offset > end {
		return nil
	}
	return &entry
}

// forkBaseline checks, while the lines are written to the fork, that the fork begins with the
// bytes the source's entry covers: the copy writes each line and a newline, so a blank line of
// the source is dropped and the fork's bytes are then not the source's.
type forkBaseline struct {
	entry   usageBaselineEntry
	hasher  hash.Hash
	written int64
	settled bool
	covered bool
}

func newForkBaseline(entry *usageBaselineEntry) *forkBaseline {
	if entry == nil {
		return nil
	}
	return &forkBaseline{entry: *entry, hasher: sha256.New()}
}

// reach is called with the source offset of the next line before it is written.
func (b *forkBaseline) reach(offset int64) {
	if b != nil && !b.settled && offset >= b.entry.Offset {
		b.settle()
	}
}

// add records a line (without its newline) the fork has written.
func (b *forkBaseline) add(line []byte) {
	if b != nil && !b.settled {
		_, _ = b.hasher.Write(line)
		_, _ = b.hasher.Write([]byte{'\n'})
		b.written += int64(len(line)) + 1
	}
}

func (b *forkBaseline) settle() {
	b.settled = true
	b.covered = b.written == b.entry.Offset && hex.EncodeToString(b.hasher.Sum(nil)) == b.entry.SHA256
}

// store writes the entry as the fork's usage baseline when the fork's bytes are the covered
// ones, and reports whether it did. It runs before the fork's transcript is renamed into place,
// so a fork never exists without its totals; the entry is tied to bytes, so one left by a fork
// that then failed fits no file.
func (b *forkBaseline) store(config sessionfs.SessionConfig, logger *slog.Logger) (bool, error) {
	if b == nil {
		return false, nil
	}
	if !b.settled {
		b.settle()
	}
	path := sessionfs.UsageBaselinePath(config.UsageCachePath)
	if !b.covered {
		logging.Or(logger).Warn("fork does not carry over the usage baseline: the copied transcript is not the bytes it covers (blank lines in the source?)", "path", path)
		return false, nil
	}
	encoded, err := json.Marshal(usageBaselineFile{SchemaVersion: usageBaselineSchemaVersion, Entries: []usageBaselineEntry{b.entry}})
	if err != nil {
		return false, fmt.Errorf("encode usage baseline: %w", err)
	}
	durable := config.Persistence.Durability != "process"
	if err := replaceFileAtomically(path, encoded, durable, durable, "usage baseline"); err != nil {
		return false, err
	}
	return true, nil
}

// removeStaleUsageState deletes the usage cache and baseline the fork's target left from an
// earlier session of that name. The target has no transcript, and an entry fits any file that
// begins with its bytes (one at offset 0 fits every file), so it must not stand for the fork's.
// Nothing is removed when the target shares the source's files (explicit paths in the profile).
func removeStaleUsageState(source, target sessionfs.SessionConfig) error {
	if target.UsageCachePath == "" || sameFile(source.UsageCachePath, target.UsageCachePath) {
		return nil
	}
	for _, path := range []string{target.UsageCachePath, sessionfs.UsageBaselinePath(target.UsageCachePath)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the stale %s of the new session: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func sameFile(a, b string) bool {
	return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b)
}
