package sessionfs

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The profile tool's edit, history and revert (docs/design_workspace_layout.md §4.7, decider
// phase 3 S1 to S7): a profile changes its own file, in the workspace layout only. The owner
// is the caller's run, never the model's input; the tool refuses sub-agents, other profiles,
// plan mode and runs another run or server started before this is reached.

// Self-edit actions, as the history records them.
const (
	ProfileEditActionEdit   = "edit"
	ProfileEditActionRevert = "revert"
)

// profileHistoryFolder is the history folder in the profile's own folder: read-only to the
// profile's own sandboxed calls and hidden from other profiles when a sandbox is on (S6).
const profileHistoryFolder = "history"

// profileHistoryName is <sequence>-<12 hex of the stored revision>.json.
var profileHistoryName = regexp.MustCompile(`^([0-9]{6,})-([0-9a-f]{12})\.json$`)

// ProfileEditRequest is one edit or revert of the caller's own profile file.
type ProfileEditRequest struct {
	Action string
	// Contents is the whole new profile file (edit).
	Contents json.RawMessage
	// HistoryID names the history entry whose stored file a revert restores.
	HistoryID string
	// IfMatch is the revision the caller read (profile config); required.
	IfMatch string
	Audit   ProfileEditAudit
}

// ProfileEditAudit is who made a change, as the history entry records it.
type ProfileEditAudit struct {
	Session string `json:"session"`
	CallID  string `json:"call_id,omitempty"`
	// Origin is where the run came from (the tool admits only a profile's own top-level run).
	Origin string `json:"origin"`
}

// ProfileEditResult is the tool's audit record of a change: the revisions before and after,
// the keys that changed and the history entry that holds the file as it was.
type ProfileEditResult struct {
	Profile     string   `json:"profile"`
	Action      string   `json:"action"`
	File        string   `json:"file"`
	Changed     bool     `json:"changed"`
	OldRevision string   `json:"old_revision"`
	Revision    string   `json:"revision"`
	ChangedKeys []string `json:"changed_keys,omitempty"`
	HistoryID   string   `json:"history_id,omitempty"`
	RevertedTo  string   `json:"reverted_to,omitempty"`
	SelfEdit    string   `json:"profile_self_edit"`
	Note        string   `json:"note"`
}

// ProfileHistoryEntry is one change in a profile's history: the file as it was before the
// change (Previous, left out of listings) and who made the change.
type ProfileHistoryEntry struct {
	ID          string           `json:"id"`
	Time        string           `json:"time"`
	Action      string           `json:"action"`
	Profile     string           `json:"profile"`
	Audit       ProfileEditAudit `json:"audit"`
	SelfEdit    string           `json:"profile_self_edit"`
	OldRevision string           `json:"old_revision"`
	NewRevision string           `json:"new_revision"`
	ChangedKeys []string         `json:"changed_keys,omitempty"`
	RevertedTo  string           `json:"reverted_to,omitempty"`
	Previous    string           `json:"previous,omitempty"`
}

// ProfileHistory is a page of a profile's history, newest first.
type ProfileHistory struct {
	Profile string                `json:"profile"`
	Folder  string                `json:"folder"`
	Entries []ProfileHistoryEntry `json:"entries"`
	More    bool                  `json:"more,omitempty"`
}

// profileFileLocks hold one lock per profile file for the writers of this process: edit,
// revert and the admin writes (model default, catalog write and delete). The lock is never
// taken with a session run lock held for it, so a run's own edit cannot deadlock (S3).
var profileFileLocks sync.Map

// lockProfileFile takes the lock of the profile file at path, or fails at once with a
// ConflictError when another writer holds it.
func lockProfileFile(path string) (func(), error) {
	value, _ := profileFileLocks.LoadOrStore(filepath.Clean(path), &sync.Mutex{})
	mu := value.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, &ConflictError{Message: fmt.Sprintf("%s is being changed by another writer; read it again and retry", path)}
	}
	return mu.Unlock, nil
}

// EditProfile edits or reverts the profile file of profile in the session at location: the
// revision must match, the change must pass the profile_self_edit policy and a trial load of
// the session with the candidate in place of the file, and the file as it was is kept in the
// history before the new one is published (S3 to S6). The running run keeps the settings it
// loaded; the change applies from the next run.
func EditProfile(location Location, profile string, request ProfileEditRequest) (ProfileEditResult, error) {
	if location.Root.Layout == nil || location.Root.Layout.Workspace == "" || !location.UsesLayout() {
		return ProfileEditResult{}, &ClientError{Message: "a profile edits itself only in the workspace layout (W/.jad/workspace.json)"}
	}
	layout := *location.Root.Layout
	level := layout.ProfileSelfEdit
	if level == "" {
		level = ProfileSelfEditOn
	}
	if level == ProfileSelfEditOff {
		return ProfileEditResult{}, &ClientError{Message: "profile_self_edit is off in this workspace: a profile may not change its own file"}
	}
	if request.Action != ProfileEditActionEdit && request.Action != ProfileEditActionRevert {
		return ProfileEditResult{}, &ClientError{Message: "the action is edit or revert"}
	}
	ifMatch := strings.TrimSpace(request.IfMatch)
	if ifMatch == "" {
		return ProfileEditResult{}, &CatalogError{Kind: CatalogPreconditionRequired, Message: "if_match is required: give the revision profile config shows"}
	}
	loaded, err := loadLayoutSessionSettings(location, layout, profile, "", nil)
	if err != nil {
		return ProfileEditResult{}, err
	}
	path := loaded.ProfilePath
	own := layout.ProfileOwnDir(loaded.Profile)
	if path == "" || own == "" {
		return ProfileEditResult{}, &ClientError{Message: "the profile has no file of its own to edit"}
	}
	unlock, err := lockProfileFile(path)
	if err != nil {
		return ProfileEditResult{}, err
	}
	defer unlock()

	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return ProfileEditResult{}, configError("%s: not a regular file", path)
	}
	current, err := readRegularFile(path, maxInternalJSONFileBytes)
	if err != nil {
		return ProfileEditResult{}, configError("%s: could not be read: %v", path, err)
	}
	revision := CatalogETag(current)
	if !sameRevision(ifMatch, revision) {
		return ProfileEditResult{}, &CatalogError{Kind: CatalogPreconditionFailed, Message: fmt.Sprintf("the profile file changed since revision %s was read (now %s); read it again with config", ifMatch, revision)}
	}
	result := ProfileEditResult{Profile: loaded.Profile, Action: request.Action, File: path, OldRevision: revision, Revision: revision, SelfEdit: level}
	var candidate []byte
	if request.Action == ProfileEditActionEdit {
		if candidate, err = editedProfileFile(current, request.Contents); err != nil {
			return ProfileEditResult{}, err
		}
	} else {
		entry, err := readHistoryEntry(layout.Workspace, loaded.Profile, request.HistoryID)
		if err != nil {
			return ProfileEditResult{}, err
		}
		candidate, result.RevertedTo = []byte(entry.Previous), entry.ID
	}
	if bytes.Equal(candidate, current) {
		result.Note = "nothing changed"
		return result, nil
	}
	if len(candidate) > maxInternalJSONFileBytes {
		return ProfileEditResult{}, &ClientError{Message: fmt.Sprintf("the new profile file is larger than %d bytes", maxInternalJSONFileBytes)}
	}
	place := selfEditPlace{layout: layout, profile: loaded.Profile, cwd: loaded.WorkingDir}
	if result.ChangedKeys, err = checkSelfEditPolicy(level, current, candidate, place); err != nil {
		return ProfileEditResult{}, err
	}
	// The trial load: the whole load of the session with this profile selected, reading the
	// candidate in place of the file, which is not written before it passes (S4).
	if _, err := loadLayoutSessionSettings(location, layout, loaded.Profile, "", newTrialScans(path, candidate)); err != nil {
		return ProfileEditResult{}, &ClientError{Message: "the change does not load, so it was not made: " + err.Error()}
	}
	newRevision := CatalogETag(candidate)
	entry := ProfileHistoryEntry{Time: time.Now().UTC().Format(time.RFC3339Nano), Action: request.Action, Profile: loaded.Profile, Audit: request.Audit, SelfEdit: level,
		OldRevision: revision, NewRevision: newRevision, ChangedKeys: result.ChangedKeys, RevertedTo: result.RevertedTo, Previous: string(current)}
	// The history entry is on disk before the change is published: if publishing fails, the
	// entry holds the file as it still is (S6).
	if result.HistoryID, err = writeHistoryEntry(layout.Workspace, loaded.Profile, entry); err != nil {
		return ProfileEditResult{}, fmt.Errorf("keep the profile history: %w", err)
	}
	if err := writeFileIfUnchanged(path, candidate, before, true); err != nil {
		return ProfileEditResult{}, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return ProfileEditResult{}, err
	}
	result.Changed, result.Revision = true, newRevision
	result.Note = "saved; the running run keeps the settings it started with, the next run uses the new file"
	return result, nil
}

// ReadProfileHistory lists the history of the profile's own file, newest first, without the
// stored files; at most limit entries (0: all).
func ReadProfileHistory(location Location, profile string, limit int) (ProfileHistory, error) {
	if location.Root.Layout == nil || location.Root.Layout.Workspace == "" {
		return ProfileHistory{}, &ClientError{Message: "a profile has a history only in the workspace layout (W/.jad/workspace.json)"}
	}
	own := location.Root.Layout.ProfileOwnDir(profile)
	if own == "" {
		return ProfileHistory{}, &ClientError{Message: "the profile has no folder of its own"}
	}
	history := ProfileHistory{Profile: profile, Folder: filepath.Join(own, profileHistoryFolder), Entries: []ProfileHistoryEntry{}}
	names, err := historyNames(location.Root.Layout.Workspace, profile, false)
	if err != nil || len(names) == 0 {
		return history, err
	}
	for index := len(names) - 1; index >= 0; index-- {
		if limit > 0 && len(history.Entries) == limit {
			history.More = true
			break
		}
		entry, err := readHistoryEntry(location.Root.Layout.Workspace, profile, strings.TrimSuffix(names[index], ".json"))
		if err != nil {
			return ProfileHistory{}, err
		}
		entry.Previous = ""
		history.Entries = append(history.Entries, entry)
	}
	return history, nil
}

// sameRevision compares a revision given by the caller with the file's, with or without the
// quotes of the entity tag. A wildcard is no revision: the caller must have read the file.
func sameRevision(given, revision string) bool {
	return strings.Trim(strings.TrimSpace(given), `"`) == strings.Trim(revision, `"`)
}

// editedProfileFile is the candidate of an edit: the whole new profile file, its keys in the
// order of the current file and then in the order given, indented like the current file.
func editedProfileFile(current []byte, contents json.RawMessage) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil || fields == nil {
		return nil, &ClientError{Message: "contents must be the whole profile file as a JSON object"}
	}
	order, _ := orderedObjectKeys(current)
	given, err := orderedObjectKeys(contents)
	if err != nil {
		return nil, &ClientError{Message: "contents must be the whole profile file as a JSON object"}
	}
	for _, key := range given {
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
	}
	compact, err := marshalOrderedObject(fields, order)
	if err != nil {
		return nil, &ClientError{Message: err.Error()}
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", indentOf(current)); err != nil {
		return nil, &ClientError{Message: err.Error()}
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// historyFolder checks, and with create makes (mode 0700), each folder from the real W/.jad
// down to W/.jad/profile-own/<category>/<name>/history, refusing a link or a file on the way:
// a link planted there could send the history elsewhere. It returns the folder, or "" when it
// does not exist and create is false.
func historyFolder(workspace, profile string, create bool) (string, error) {
	jad := filepath.Join(workspace, ".jad")
	current, err := filepath.EvalSymlinks(jad)
	if err != nil {
		return "", fmt.Errorf("the workspace folder %s: %w", jad, err)
	}
	category, name, found := strings.Cut(profile, "/")
	if !found || !filepath.IsLocal(category) || !filepath.IsLocal(name) || strings.Contains(name, "/") {
		return "", fmt.Errorf("%s is not a <category>/<name> profile", profile)
	}
	for _, part := range []string{profileOwnFolder, category, name, profileHistoryFolder} {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if !create {
				return "", nil
			}
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", err
			}
			if info, err = os.Lstat(current); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s is a link or not a folder; the profile history is not kept there", current)
		}
	}
	return current, nil
}

// historyNames are the history entry files of the profile, oldest first.
func historyNames(workspace, profile string, create bool) ([]string, error) {
	dir, err := historyFolder(workspace, profile, create)
	if err != nil || dir == "" {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if profileHistoryName.MatchString(entry.Name()) && entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int { return compareSequence(a, b) })
	return names, nil
}

// compareSequence orders history file names by their sequence number.
func compareSequence(a, b string) int {
	first, _ := strconv.ParseUint(profileHistoryName.FindStringSubmatch(a)[1], 10, 64)
	second, _ := strconv.ParseUint(profileHistoryName.FindStringSubmatch(b)[1], 10, 64)
	switch {
	case first < second:
		return -1
	case first > second:
		return 1
	}
	return strings.Compare(a, b)
}

// writeHistoryEntry stores entry as the next history file of the profile (made new in the 0700
// history folder, synced with its folder) and returns its id.
func writeHistoryEntry(workspace, profile string, entry ProfileHistoryEntry) (string, error) {
	names, err := historyNames(workspace, profile, true)
	if err != nil {
		return "", err
	}
	dir, err := historyFolder(workspace, profile, false)
	if err != nil {
		return "", err
	}
	next := uint64(1)
	if len(names) > 0 {
		last, _ := strconv.ParseUint(profileHistoryName.FindStringSubmatch(names[len(names)-1])[1], 10, 64)
		next = last + 1
	}
	sum := strings.Trim(entry.OldRevision, `"`)
	if len(sum) < 12 || !isHex(sum[:12]) {
		return "", errors.New("the revision is not a hash")
	}
	entry.ID = fmt.Sprintf("%06d-%s", next, sum[:12])
	encoded, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return "", err
	}
	if err := createFileExclusive(filepath.Join(dir, entry.ID+".json"), append(encoded, '\n'), true); err != nil {
		return "", err
	}
	if err := syncDirectory(dir); err != nil {
		return "", err
	}
	return entry.ID, nil
}

// readHistoryEntry reads the history entry id of the profile: a regular file of its history
// folder, whose stored file is valid JSON.
func readHistoryEntry(workspace, profile, id string) (ProfileHistoryEntry, error) {
	id = strings.TrimSpace(id)
	if !profileHistoryName.MatchString(id + ".json") {
		return ProfileHistoryEntry{}, &ClientError{Message: fmt.Sprintf("history_id %q is not a history entry id; the history action lists them", id)}
	}
	dir, err := historyFolder(workspace, profile, false)
	if err != nil {
		return ProfileHistoryEntry{}, err
	}
	if dir == "" {
		return ProfileHistoryEntry{}, &ClientError{Message: "the profile has no history yet"}
	}
	path := filepath.Join(dir, id+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ProfileHistoryEntry{}, &ClientError{Message: fmt.Sprintf("history entry %s does not exist", id)}
	}
	if err != nil || !info.Mode().IsRegular() {
		return ProfileHistoryEntry{}, fmt.Errorf("history entry %s is not a regular file", id)
	}
	// The stored file is a JSON string inside the entry, so the entry may be larger than it.
	contents, err := readRegularFile(path, 8*maxInternalJSONFileBytes)
	if err != nil {
		return ProfileHistoryEntry{}, err
	}
	var entry ProfileHistoryEntry
	if err := json.Unmarshal(contents, &entry); err != nil || entry.ID != id || !json.Valid([]byte(entry.Previous)) {
		return ProfileHistoryEntry{}, fmt.Errorf("history entry %s is damaged", id)
	}
	return entry, nil
}

// syncDirectory makes a rename or a new file in dir durable.
func syncDirectory(dir string) error {
	folder, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := folder.Sync()
	closeErr := folder.Close()
	return errors.Join(syncErr, closeErr)
}

func isHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}
