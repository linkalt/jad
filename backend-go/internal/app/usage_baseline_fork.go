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
	"jad/backend-go/internal/webui"
)

// A fork copies the first end bytes of its source's transcript, and its usage must be what those
// bytes and the history they stand for cost, never less without saying so. The source's baseline
// (usage_baseline.go) holds checkpoints, each the exact cumulative usage at its offset. A fork
// gets, in its own baseline file, one of:
//
//   - a checkpoint: the source's entry with the largest Offset not past end. The usage of the
//     removed history and of the first Offset bytes is in its summary, and the fork's reader
//     scans on from there. The entry is bound to the fork's own bytes (offset and hash of what
//     was written), after proving that the source bytes it covers are what the entry hashes: the
//     copy drops blank lines, so its offsets can differ from the source's.
//   - an exact marker: no entry reaches back that far, but the newest one says nothing was ever
//     removed from before the bytes it covers (PrefixExact), so a scan of the copy is its usage.
//   - a partial marker: history was removed and the entries cannot say how much of it precedes
//     the cut (the source's entries are older than PrefixExact, or the entry could not be proven
//     to cover the copied bytes, or the source is partial itself). The fork counts the records it
//     copied and says its history is partial, in the baseline file, so that the flag outlives its
//     usage cache and the history block.
//
// A fork that copies nothing gets none of them: it holds none of the history.

// forkUsage is what a fork carries of its source's baseline, decided before the copy.
type forkUsage struct {
	entry   *usageBaselineEntry
	exact   bool
	partial bool
}

func (u forkUsage) none() bool { return u.entry == nil && !u.exact && !u.partial }

// forkUsageBaseline decides what a fork that copies the first end bytes of the source transcript
// carries. sourcePartial is the source's own HistoryPartial.
func (s *sharedSession) forkUsageBaseline(config sessionfs.SessionConfig, end int64, sourcePartial bool) forkUsage {
	if end <= 0 || sessionfs.UsageBaselinePath(config.UsageCachePath) == "" {
		return forkUsage{}
	}
	stat, err := statFile(config.MessagesPath)
	if err != nil {
		return forkUsage{partial: true}
	}
	entries := s.matchingUsageBaselines(config, stat, 0)
	var best *usageBaselineEntry
	for index := range entries {
		if entries[index].Offset <= end && (best == nil || entries[index].Offset > best.Offset) {
			best = &entries[index]
		}
	}
	switch {
	case best != nil:
		return forkUsage{entry: best}
	case len(entries) > 0 && entries[0].PrefixExact:
		return forkUsage{exact: true}
	case len(entries) > 0 || sourcePartial:
		return forkUsage{partial: true}
	}
	return forkUsage{}
}

// forkBaseline follows the lines written to the fork, to bind the checkpoint to them. It
// reconstructs the source bytes the lines came from (each line and a newline, a newline for every
// byte between lines: the blank lines the copy drops) to check them against the entry's hash, and
// hashes what it writes.
type forkBaseline struct {
	usage   forkUsage
	srcPos  int64
	srcHash hash.Hash
	outLen  int64
	outHash hash.Hash
	settled bool
	bound   *usageBaselineEntry
}

func newForkBaseline(usage forkUsage) *forkBaseline {
	if usage.none() {
		return nil
	}
	return &forkBaseline{usage: usage, srcHash: sha256.New(), outHash: sha256.New()}
}

// add is called with each line (without its newline) and its source offset, before the line is
// written to the fork.
func (b *forkBaseline) add(offset int64, line []byte) {
	if b == nil || b.usage.entry == nil || b.settled {
		return
	}
	if offset >= b.usage.entry.Offset {
		b.settle(offset)
		return
	}
	b.padSource(offset)
	_, _ = b.srcHash.Write(line)
	_, _ = b.srcHash.Write([]byte{'\n'})
	b.srcPos = offset + int64(len(line)) + 1
	_, _ = b.outHash.Write(line)
	_, _ = b.outHash.Write([]byte{'\n'})
	b.outLen += int64(len(line)) + 1
}

// padSource hashes a newline for each source byte between the last line and upTo.
func (b *forkBaseline) padSource(upTo int64) {
	if gap := upTo - b.srcPos; gap > 0 {
		blank := make([]byte, gap)
		for index := range blank {
			blank[index] = '\n'
		}
		_, _ = b.srcHash.Write(blank)
		b.srcPos = upTo
	}
}

// settle is called when the source has been read up to upTo, which is not before the entry's
// offset: the copy then holds every line the entry covers. The checkpoint is bound only if the
// source bytes it covers are proven to be the ones the entry hashes.
func (b *forkBaseline) settle(upTo int64) {
	b.settled = true
	entry := b.usage.entry
	if b.srcPos > entry.Offset || upTo < entry.Offset {
		return
	}
	b.padSource(entry.Offset)
	if hex.EncodeToString(b.srcHash.Sum(nil)) != entry.SHA256 {
		return
	}
	bound := *entry
	bound.Offset = b.outLen
	bound.SHA256 = hex.EncodeToString(b.outHash.Sum(nil))
	b.bound = &bound
}

// store writes the fork's baseline file, before the fork's transcript is renamed into place, so
// that a fork never exists without it; the entry is tied to bytes, so one left by a fork that then
// failed fits no file. end is where the copy stopped, and it reports whether a file was written.
func (b *forkBaseline) store(config sessionfs.SessionConfig, logger *slog.Logger, end int64) (bool, error) {
	if b == nil {
		return false, nil
	}
	path := sessionfs.UsageBaselinePath(config.UsageCachePath)
	var entry usageBaselineEntry
	switch {
	case b.usage.entry != nil:
		if !b.settled {
			b.settle(end)
		}
		if b.bound != nil {
			entry = *b.bound
			break
		}
		logging.Or(logger).Warn("fork cannot carry over the usage baseline: the source bytes it covers are not the ones it hashes; the fork's history is marked partial", "path", path)
		entry = forkMarker(true)
	default:
		entry = forkMarker(b.usage.partial)
	}
	encoded, err := json.Marshal(usageBaselineFile{SchemaVersion: usageBaselineSchemaVersion, Entries: []usageBaselineEntry{entry}})
	if err != nil {
		return false, fmt.Errorf("encode usage baseline: %w", err)
	}
	durable := config.Persistence.Durability != "process"
	if err := replaceFileAtomically(path, encoded, durable, durable, "usage baseline"); err != nil {
		return false, err
	}
	return true, nil
}

// forkMarker is an entry that covers no bytes and carries no usage: it fits every file, and says
// whether the history before the fork's first byte is nothing (exact) or unknown (partial).
func forkMarker(partial bool) usageBaselineEntry {
	empty := sha256.Sum256(nil)
	entry := usageBaselineEntry{SHA256: hex.EncodeToString(empty[:])}
	if partial {
		entry.Summary = webui.UsageSummary{HistoryPartial: true}
	} else {
		entry.PrefixExact = true
	}
	return entry
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
