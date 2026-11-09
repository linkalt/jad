package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/webui"
)

// subagentSnapshotLimits bounds what one snapshot read costs: what the snapshot keeps, the
// longest transcript line it parses, and how much of the file it scans at all.
type subagentSnapshotLimits struct {
	steps webui.SubagentLimits
	// maxLine skips a transcript line over it (a huge tool result, a hostile record).
	maxLine int64
	// maxScan stops the scan at this many bytes of the transcript; a child that wrote
	// more shows only the steps within them.
	maxScan int64
	// cacheEntries is how many snapshots stay cached.
	cacheEntries int
}

var defaultSubagentSnapshotLimits = subagentSnapshotLimits{
	steps:        webui.DefaultSubagentLimits(),
	maxLine:      1 << 20,
	maxScan:      16 << 20,
	cacheEntries: 256,
}

// subagentSnapshotEntry is what one subagent call's snapshot read left behind. A found
// transcript is valid while its size and modification time are the ones it was read at; a
// call whose transcript was not found stays absent while the storage directory is
// unchanged (a new child folder changes it), so a result whose child was deleted costs a
// stat, not a directory listing.
type subagentSnapshotEntry struct {
	path     string
	size     int64
	modTime  time.Time
	snapshot *webui.SubagentSnapshot
	// absent marks a call whose transcript was not found; rootModTime is the storage
	// directory's modification time then.
	absent      bool
	rootModTime time.Time
	used        uint64
}

type subagentSnapshotCache struct {
	mu      sync.Mutex
	entries map[string]*subagentSnapshotEntry
	clock   uint64
}

// subagentSnapshots is process-wide like the transcript index cache: its keys carry the
// storage directory, so resolvers and sessions never share an entry by accident.
var subagentSnapshots = &subagentSnapshotCache{entries: map[string]*subagentSnapshotEntry{}}

func (c *subagentSnapshotCache) get(key string) (subagentSnapshotEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return subagentSnapshotEntry{}, false
	}
	c.clock++
	entry.used = c.clock
	return *entry, true
}

func (c *subagentSnapshotCache) put(key string, entry subagentSnapshotEntry, capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock++
	entry.used = c.clock
	c.entries[key] = &entry
	for len(c.entries) > capacity {
		oldest, oldestKey := ^uint64(0), ""
		for candidate, kept := range c.entries {
			if kept.used < oldest {
				oldest, oldestKey = kept.used, candidate
			}
		}
		delete(c.entries, oldestKey)
	}
}

// subagentSnapshot is the snapshot of the child behind one subagent tool result of a
// session, nil when there is none to show: no subagent storage, no child folder, a
// transcript outside the storage directory, or no completed step yet.
func (r *Resolver) subagentSnapshot(ctx context.Context, config sessionfs.SessionConfig, call webui.SubagentCall) *webui.SubagentSnapshot {
	if config.NativeSubagent == nil || config.NativeSubagent.StorageDir == "" {
		return nil
	}
	return readSubagentSnapshot(ctx, config.NativeSubagent.StorageDir, call, defaultSubagentSnapshotLimits)
}

// ReadSubagentSnapshot answers the subagent route: the completed steps of the child behind
// callID, as {"subagent": snapshot}, read now (a running child gives the steps it has
// finished, none before its first, and carries "running": true). The child is found by the call id alone, in the session
// profile's subagent storage; no folder is a 404, as is a session without storage.
func (r *Resolver) ReadSubagentSnapshot(ctx context.Context, reference, profile, callID string) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("subagent snapshot context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return nil, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return nil, mapSessionError(err)
	}
	notFound := &httpapi.StatusError{Code: 404, Message: "subagent transcript not found"}
	if callID == "" || config.NativeSubagent == nil || config.NativeSubagent.StorageDir == "" {
		return nil, notFound
	}
	// Liveness is read before the steps. A child that ends between the two reads is then
	// reported running with its final steps (the next read says it ended); read after, it
	// could be reported finished with steps that miss its last ones, and a client that
	// stops asking at "finished" would keep that gap.
	running := r.subagentCallRunning(location.Reference, config, callID)
	snapshot, found, err := readSubagentSteps(ctx, config.NativeSubagent.StorageDir, webui.SubagentCall{CallID: callID}, defaultSubagentSnapshotLimits)
	if err != nil {
		// A child that is there but cannot be read is not a missing one; the client gets
		// the generic 500 and the cause (with its path) stays in the server log.
		return nil, err
	}
	if !found {
		return nil, notFound
	}
	if snapshot == nil {
		snapshot = &webui.SubagentSnapshot{Steps: []webui.SubagentStep{}}
	}
	// The cached snapshot is shared and never edited, so the live flag rides beside it in
	// the body and is never stored: a finished child's body has no "running" key.
	return json.Marshal(struct {
		Subagent subagentRouteBody `json:"subagent"`
	}{Subagent: subagentRouteBody{SubagentSnapshot: snapshot, Running: running}})
}

// subagentRouteBody is the snapshot the route answers with, plus whether the child is
// running now. Running is a fact of the moment of the request, read from the subagent
// manager; it is not part of the snapshot, which is cached by the transcript's stat.
type subagentRouteBody struct {
	*webui.SubagentSnapshot
	// Running is true while the session's subagent manager holds a queued or running run
	// for the call; absent otherwise, and always absent for a child of an earlier server
	// process or a remote agent, which this server holds no run for.
	Running bool `json:"running,omitempty"`
}

// subagentCallRunning asks the storage directory's subagent manager whether the session's
// call callID has a run going. A directory without a manager built (nothing has started a
// child in this process) has none; this never builds one.
func (r *Resolver) subagentCallRunning(reference string, config sessionfs.SessionConfig, callID string) bool {
	manager := r.existingSubagentManager(config.NativeSubagent.StorageDir)
	return manager.CallRunning(sessionKey(reference, config.Profile), callID)
}

// attachSubagentSnapshots adds the summaries of the snapshots (agent and step count) to the
// subagent tool results of messages that a snapshot or message route serves. Messages it does not change are returned as
// they are; the others are copies, so a cached block is never edited. It is not part of
// normalizing a transcript: the fork route and the inline image route normalize without
// it and read no child transcript.
func (r *Resolver) attachSubagentSnapshots(ctx context.Context, config sessionfs.SessionConfig, messages []webui.Message) []webui.Message {
	if config.NativeSubagent == nil {
		return messages
	}
	return webui.AttachSubagents(messages, func(call webui.SubagentCall) *webui.SubagentSnapshot {
		return r.subagentSnapshot(ctx, config, call)
	})
}

// SubagentSnapshot is the live run's route to the same snapshot: the HTTP layer cannot
// import this package, so it asks the run's session through an interface of its own.
func (r boundRunner) SubagentSnapshot(call webui.SubagentCall) *webui.SubagentSnapshot {
	return r.resolver.subagentSnapshot(context.Background(), r.config, call)
}

// readSubagentSnapshot reads the completed steps of the child behind call, from the cache
// when the child's transcript is unchanged.
func readSubagentSnapshot(ctx context.Context, root string, call webui.SubagentCall, limits subagentSnapshotLimits) *webui.SubagentSnapshot {
	snapshot, _, _ := readSubagentSteps(ctx, root, call, limits)
	return snapshot
}

// readSubagentSteps is readSubagentSnapshot that also says whether the child's transcript
// was found: a found child with no completed step yet (or no transcript file yet) gives
// (nil, true, nil). A transcript that was found but could not be read gives (nil, true,
// err), so a caller can tell it from a missing child; a failed read is never cached.
func readSubagentSteps(ctx context.Context, root string, call webui.SubagentCall, limits subagentSnapshotLimits) (*webui.SubagentSnapshot, bool, error) {
	key := root + "\x00" + call.CallID + "\x00" + call.Agent + "\x00" + call.Transcript
	if cached, ok := subagentSnapshots.get(key); ok {
		if cached.absent {
			if info, err := os.Stat(root); err == nil && info.ModTime().Equal(cached.rootModTime) {
				return nil, false, nil
			}
		} else if info, err := os.Stat(cached.path); err == nil && info.Size() == cached.size && info.ModTime().Equal(cached.modTime) {
			return cached.snapshot, true, nil
		}
	}
	path := resolveSubagentTranscript(root, call)
	if path == "" {
		// A folder that exists without its messages.jsonl is a child that has not written
		// yet. Creating the file does not change the storage directory's modification time,
		// so remembering the call as absent would hide the child for good.
		if subagentFolderFor(root, call.CallID) != "" {
			return nil, true, nil
		}
		if info, err := os.Stat(root); err == nil {
			subagentSnapshots.put(key, subagentSnapshotEntry{absent: true, rootModTime: info.ModTime()}, limits.cacheEntries)
		}
		return nil, false, nil
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		// The child was removed since its folder was found.
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, true, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("subagent transcript %s is not a regular file", path)
	}
	builder := webui.NewSubagentBuilder(call.Agent, limits.steps)
	err = native.ScanTranscriptLinesSkipping(ctx, file, min(info.Size(), limits.maxScan), limits.maxLine, builder.Add, func(int64) { builder.Unseen() })
	if err != nil {
		return nil, true, err
	}
	if info.Size() > limits.maxScan {
		// The file goes on past the window; what it holds there is unknown.
		builder.Unseen()
	}
	snapshot := builder.Snapshot()
	subagentSnapshots.put(key, subagentSnapshotEntry{path: path, size: info.Size(), modTime: info.ModTime(), snapshot: snapshot}, limits.cacheEntries)
	return snapshot, true, nil
}

// resolveSubagentTranscript finds the messages.jsonl of a call's child, always inside
// root. The folder a run opens is <sanitised call id>-<8 hex>, so it is found by name;
// when no folder or several carry the call id, the transcript path the result printed is
// used, if it is a child folder under root. Nothing else is trusted, and a path that
// fails is rejected without a word.
func resolveSubagentTranscript(root string, call webui.SubagentCall) string {
	if call.CallID == "" || root == "" || !filepath.IsAbs(root) {
		return ""
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	if folder := subagentFolderFor(root, call.CallID); folder != "" {
		if path := confinedTranscript(realRoot, filepath.Join(root, folder, "messages.jsonl")); path != "" {
			return path
		}
	}
	hint := call.Transcript
	if hint == "" || !filepath.IsAbs(hint) {
		return ""
	}
	return confinedTranscript(realRoot, hint)
}

// subagentFolderFor is the one folder under root named for callID, "" when there is none
// or more than one (the call id is then not enough to tell the children apart).
func subagentFolderFor(root, callID string) string {
	name := unsafeDirectoryCharacters.ReplaceAllString(callID, "_")
	prefix := name + "-"
	dir, err := os.Open(root)
	if err != nil {
		return ""
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return ""
	}
	found := ""
	for _, entry := range entries {
		folder := entry.Name()
		if !entry.IsDir() || len(folder) != len(prefix)+8 || !strings.HasPrefix(folder, prefix) || !lowerHex(folder[len(prefix):]) {
			continue
		}
		if found != "" {
			return ""
		}
		found = folder
	}
	return found
}

func lowerHex(text string) bool {
	for _, char := range text {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// confinedTranscript resolves symlinks and returns the path only when it is a
// messages.jsonl one child folder below the real storage directory (never .runs).
func confinedTranscript(realRoot, path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	relative, err := filepath.Rel(realRoot, real)
	if err != nil || filepath.Base(relative) != "messages.jsonl" || !native.SubagentTranscriptFolder(realRoot, filepath.Dir(real)) {
		return ""
	}
	return real
}
