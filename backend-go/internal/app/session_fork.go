package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

// ForkProfile makes a new session from an existing one: the session file is copied under
// the new reference, and the profile's transcript is copied up to a turn, line for line,
// so the new session goes on from that point and the old one is untouched. The copy's
// history block (the compaction backup) and the images its lines refer to come along;
// caches are rebuilt by the new session, and tool storage (memory, tasks, processes) and
// checkpoints start empty.
func (r *Resolver) ForkProfile(ctx context.Context, reference, profile string, request httpapi.ForkRequest) (httpapi.ForkResult, error) {
	if ctx == nil {
		return httpapi.ForkResult{}, errors.New("session fork context is required")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.ForkResult{}, err
	}
	source, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.ForkResult{}, mapSessionError(err)
	}
	if request.Profile != "" {
		result, err := r.forkIntoProfile(ctx, source, profile, request)
		if err == nil {
			// The reference as the client wrote it, so it can open the new profile's view.
			result.Session = reference
		}
		return result, err
	}
	target, err := sessionfs.Resolve(r.roots, request.Session)
	if err != nil {
		return httpapi.ForkResult{}, mapSessionError(err)
	}
	if target.Root.ID != source.Root.ID {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "a fork stays in the session root of its source, so relative paths in the session file keep their meaning"}
	}
	if filepath.Clean(target.File) == filepath.Clean(source.File) {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "a fork needs a new session name"}
	}
	config, err := sessionfs.LoadSessionSettings(source, profile, "")
	if err != nil {
		return httpapi.ForkResult{}, mapSessionError(err)
	}

	key := sessionKey(source.Reference, config.Profile)
	// The gate keeps a compaction or reset of the source out while its transcript is read.
	if !r.gate.Begin(key) {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusConflict, Message: "session profile is already changing"}
	}
	defer r.gate.End(key)
	// Both session locks, in name order so two forks never wait on each other. The source
	// lock serializes the run check below with a run start, as for a reset.
	references := []string{source.Reference, target.Reference}
	sort.Strings(references)
	for _, locked := range references {
		unlock := r.gate.Lock(locked)
		defer unlock()
	}
	// A run appends the user message before its answer, so its turn is not finished yet.
	if active := r.runActive.Load(); active != nil && (*active)(source.Reference, config.Profile) {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusConflict, Message: "cannot fork a profile while a run is active"}
	}

	point, err := r.forkPoint(ctx, config, request)
	if err != nil {
		return httpapi.ForkResult{}, err
	}
	sessionFile, err := sessionfs.ReadSessionFile(r.roots, source.Reference)
	if err != nil {
		return httpapi.ForkResult{}, mapCatalogFileError(err)
	}
	if _, _, err := r.WriteSessionConfig(ctx, target.Reference, sessionFile.Data, "", "*"); err != nil {
		return httpapi.ForkResult{}, err
	}
	result, err := r.writeFork(ctx, target, config.Profile, config, point)
	if err != nil {
		// The new session file goes again, so a failed fork leaves no half session.
		_ = os.Remove(target.File)
		return httpapi.ForkResult{}, err
	}
	result.Profile = config.Profile
	return result, nil
}

// forkPoint is how much of the source transcript a fork copies: the lines before byte end,
// which hold turns turns. A zero forkPoint copies nothing.
type forkPoint struct {
	end   int64
	turns int
}

// forkPoint finds the end of the lines the fork keeps and how many turns they hold: all of
// them without a cursor; otherwise those before the turn that starts at the cursor, and
// that turn too with KeepTurn. It reads no line into memory beyond one turn at a time; the
// copy itself streams them later (copyForkLines), under the same gate that keeps a
// compaction or reset of the source out.
func (r *Resolver) forkPoint(ctx context.Context, config sessionfs.SessionConfig, request httpapi.ForkRequest) (forkPoint, error) {
	if request.State == httpapi.ForkStateNone {
		return forkPoint{}, nil
	}
	shared, release, err := r.sharedStore(config)
	if err != nil {
		return forkPoint{}, err
	}
	defer release()
	index, err := scanTranscriptIndex(shared, config)
	if err != nil {
		return forkPoint{}, fmt.Errorf("read the session transcript: %w", err)
	}
	end, turns := index.size, len(index.starts)
	cursor := request.Cursor
	if request.MessageID != "" {
		found, err := messageTurnCursor(ctx, shared, index, request.MessageID)
		if err != nil {
			return forkPoint{}, err
		}
		cursor = &found
	}
	if cursor != nil {
		turn := -1
		for candidate, start := range index.starts {
			if start == *cursor {
				turn = candidate
				break
			}
		}
		if turn < 0 {
			return forkPoint{}, &httpapi.StatusError{Code: http.StatusNotFound, Message: "no turn of the transcript starts at that cursor; use the cursor of a message in the session snapshot"}
		}
		end, turns = index.starts[turn], turn
		if request.KeepTurn {
			turns = turn + 1
			if turn+1 < len(index.starts) {
				end = index.starts[turn+1]
			} else {
				end = index.size
			}
		}
	}
	return forkPoint{end: end, turns: turns}, nil
}

// messageTurnCursor finds the turn that holds a message of the live transcript, by
// normalizing it as the session snapshot does: one turn at a time, as the snapshot's
// pages and the message route do, so only the largest turn is held in memory.
func messageTurnCursor(ctx context.Context, shared *sharedSession, index transcriptIndex, messageID string) (int64, error) {
	// An answer's id is the provider's response id, which a provider may repeat: an id
	// found in two turns names no fork point.
	var found *int64
	var records []webui.TranscriptRecord
	search := func() error {
		if len(records) == 0 {
			return nil
		}
		messages, err := webui.NormalizeTranscript(records)
		records = records[:0]
		if err != nil {
			return err
		}
		for _, message := range messages {
			if message.ID != messageID || message.Cursor == nil {
				continue
			}
			if found != nil && *found != *message.Cursor {
				return &httpapi.StatusError{Code: http.StatusConflict, Message: "several turns hold a message with that id; fork at the turn's user message instead"}
			}
			cursor := *message.Cursor
			found = &cursor
		}
		return nil
	}
	err := shared.store.ReadRecordsRange(0, index.size, func(offset int64, line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cursor := cursorForOffset(index.starts, offset)
		if len(records) > 0 && records[0].Cursor != cursor {
			if err := search(); err != nil {
				return err
			}
		}
		records = append(records, webui.TranscriptRecord{Offset: offset, Prefix: "o", Raw: append(json.RawMessage(nil), line...), Cursor: cursor, HasCursor: true})
		return nil
	})
	if err == nil {
		err = search()
	}
	var refused *httpapi.StatusError
	switch {
	case errors.As(err, &refused):
		return 0, err
	case err != nil:
		return 0, fmt.Errorf("read the session transcript: %w", err)
	case found != nil:
		return *found, nil
	}
	return 0, &httpapi.StatusError{Code: http.StatusNotFound, Message: "message not found in the transcript; a message of the compacted history cannot be a fork point"}
}

// writeFork writes the transcript of profile in the target session, its history block and
// the images the two refer to. The target must not have a transcript for the profile yet.
// The history block comes along only with a transcript: an empty fork starts with none.
func (r *Resolver) writeFork(ctx context.Context, target sessionfs.Location, profile string, source sessionfs.SessionConfig, point forkPoint) (httpapi.ForkResult, error) {
	config, err := sessionfs.LoadSessionSettings(target, profile, "")
	if err != nil {
		return httpapi.ForkResult{}, mapSessionError(err)
	}
	if info, err := os.Stat(config.MessagesPath); err == nil && info.Size() > 0 {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusConflict, Message: "the new session already has a transcript for this profile (left by a session of that name); remove its state folder first"}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return httpapi.ForkResult{}, err
	}
	result := httpapi.ForkResult{Session: target.Reference, Turns: point.turns}
	images := map[string]struct{}{}
	err = writeStreamAtomic(config.MessagesPath, func(out io.Writer) error {
		return r.copyForkLines(ctx, source, point, func(line []byte) error {
			result.Bytes += int64(len(line)) + 1
			for _, name := range imagestore.RefNames(line) {
				images[name] = struct{}{}
			}
			if _, err := out.Write(line); err != nil {
				return err
			}
			_, err := out.Write([]byte{'\n'})
			return err
		})
	})
	if err != nil {
		return httpapi.ForkResult{}, fmt.Errorf("write the fork's transcript: %w", err)
	}
	if backup := source.StoragePaths.MessagesBackup; result.Bytes > 0 && backup != "" && config.StoragePaths.MessagesBackup != "" && filepath.Clean(backup) != filepath.Clean(config.StoragePaths.MessagesBackup) {
		if err := copyHistoryBlock(backup, config.StoragePaths.MessagesBackup, images); err != nil {
			return httpapi.ForkResult{}, err
		}
	}
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)
	copied, err := copyStoredImages(source.ImageStore.Dir, config.ImageStore.Dir, names)
	if err != nil {
		return httpapi.ForkResult{}, fmt.Errorf("copy the fork's images: %w", err)
	}
	result.Images = copied
	return result, nil
}

// copyStoredImages copies the named image files from one store directory to another and
// reports how many it wrote; a name the target already holds is skipped (names are content
// hashes). Nothing is copied when both are the same directory or either store is off.
func copyStoredImages(from, to string, names []string) (int, error) {
	if from == "" || to == "" || filepath.Clean(from) == filepath.Clean(to) {
		return 0, nil
	}
	copied := 0
	seen := map[string]bool{}
	for _, name := range names {
		if _, ok := imagestore.ValidName(name); !ok || seen[name] {
			continue
		}
		seen[name] = true
		destination := filepath.Join(to, name)
		if _, err := os.Lstat(destination); err == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, name))
		if errors.Is(err, fs.ErrNotExist) {
			// The source has lost it too; the reference stays as broken as it was.
			continue
		}
		if err != nil {
			return copied, err
		}
		if err := writeFileAtomic(destination, data); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, nil
}

// copyForkLines passes each transcript line of source before point.end to visit, read as a
// stream from the shared store. It reads a snapshot, so the journal lock is not held while
// visit writes the line out; the fork's gate keeps a compaction or reset of the source out,
// and a transcript rewritten meanwhile all the same fails the fork (409) instead of copying
// stale lines.
func (r *Resolver) copyForkLines(ctx context.Context, source sessionfs.SessionConfig, point forkPoint, visit func(line []byte) error) error {
	if point.end <= 0 {
		return nil
	}
	shared, release, err := r.sharedStore(source)
	if err != nil {
		return err
	}
	defer release()
	err = shared.store.ReadRecordsSnapshot(0, point.end, func(_ int64, line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return visit(line)
	})
	if errors.Is(err, transcript.ErrTranscriptChanged) {
		return &httpapi.StatusError{Code: http.StatusConflict, Message: "the session transcript changed while it was copied; fork again"}
	}
	if err != nil {
		return fmt.Errorf("read the session transcript: %w", err)
	}
	return nil
}

// copyHistoryBlock copies the history block (the compaction backup) at source to
// destination as a stream, adding the images its lines refer to. A missing block copies
// nothing.
func copyHistoryBlock(source, destination string, images map[string]struct{}) error {
	file, err := os.Open(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the history block: %w", err)
	}
	defer file.Close()
	err = writeStreamAtomic(destination, func(out io.Writer) error {
		reader := bufio.NewReader(file)
		for {
			line, readErr := reader.ReadBytes('\n')
			for _, name := range imagestore.RefNames(bytes.TrimSuffix(line, []byte{'\n'})) {
				images[name] = struct{}{}
			}
			if _, err := out.Write(line); err != nil {
				return err
			}
			switch {
			case errors.Is(readErr, io.EOF):
				return nil
			case readErr != nil:
				return fmt.Errorf("read the history block: %w", readErr)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("write the fork's history block: %w", err)
	}
	return nil
}

// writeStreamAtomic writes what fill writes to a temporary file beside path, syncs it and
// renames it, so path holds either its old content or all of the new.
func writeStreamAtomic(path string, fill func(io.Writer) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".fork-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	buffered := bufio.NewWriter(temporary)
	if err := fill(buffered); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := buffered.Flush(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// writeFileAtomic writes data to a temporary file beside path, syncs it and renames it.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".fork-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
