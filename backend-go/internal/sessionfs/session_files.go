package sessionfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"jad/backend-go/internal/catalog"
	"jad/backend-go/internal/sessionlock"
)

// ListSessionFiles lists the catalog-layout sessions of every root with the version of each
// <id>.json file. The ID is the session reference the other routes take.
func ListSessionFiles(roots []Root) ([]CatalogFileInfo, error) {
	return listSessionFiles(roots, nil)
}

// listSessionFiles is ListSessionFiles; with onUnreadable set, a session file that cannot
// be read is passed to it and skipped instead of failing the listing.
func listSessionFiles(roots []Root, onUnreadable func(location Location, err error)) ([]CatalogFileInfo, error) {
	var infos []CatalogFileInfo
	for _, root := range roots {
		if root.Layout == nil {
			continue
		}
		entries, err := os.ReadDir(root.Directory)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read session root %q: %w", root.ID, err)
		}
		for _, entry := range entries {
			id, ok := SessionFileID(entry)
			if !ok {
				continue
			}
			location := makeLocation(root, id, len(roots))
			data, err := readRegularFile(location.SettingFile(), maxInternalJSONFileBytes)
			if err != nil {
				if onUnreadable != nil {
					onUnreadable(location, err)
					continue
				}
				return nil, configError("%s: could not be read: %v", location.SettingFile(), err)
			}
			infos = append(infos, CatalogFileInfo{ID: location.Reference, ETag: CatalogETag(data), Name: fileDisplayName(data), Category: fileCategory(data), scope: fileScope(data)})
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

// fileScope is the profile selection a session file sets. A file that does not parse selects nothing
// it could be told apart from, so it is treated as selecting every profile, as an absent scope does.
func fileScope(data []byte) catalog.Selection {
	var scoped struct {
		Scope layoutScope `json:"scope"`
	}
	if json.Unmarshal(data, &scoped) != nil {
		return catalog.All()
	}
	return scoped.Scope.Profiles
}

// ProfileSessions maps each profile ID to the sessions whose scope.profiles selects it. A session
// whose scope names a profile that does not exist selects nothing, as it would at run time.
func ProfileSessions(layout Layout, roots []Root) (map[string][]SessionRef, error) {
	profiles, err := catalog.Scan(string(CatalogProfiles), layout.Profiles)
	if err != nil {
		return nil, configError("%v", err)
	}
	sessions, err := listSessionFiles(roots, func(Location, error) {})
	if err != nil {
		return nil, err
	}
	byProfile := make(map[string][]SessionRef)
	for _, session := range sessions {
		narrowed, err := profiles.Narrow(session.scope)
		if err != nil {
			continue
		}
		for _, id := range narrowed.IDs() {
			byProfile[id] = append(byProfile[id], SessionRef{ID: session.ID, Name: session.Name, Category: session.Category})
		}
	}
	return byProfile, nil
}

// existingLayoutSession resolves a reference to a session that is stored in the catalog layout.
func existingLayoutSession(roots []Root, reference string) (Location, error) {
	location, err := Resolve(roots, reference)
	if err != nil {
		var problem *ResolveError
		if errors.As(err, &problem) && problem.Kind == ResolveSessionNotFound {
			return Location{}, &CatalogError{Kind: CatalogNotFound, Message: fmt.Sprintf("session %q does not exist", reference)}
		}
		return Location{}, &CatalogError{Kind: CatalogBadID, Message: err.Error()}
	}
	if !location.UsesLayout() {
		return Location{}, &CatalogError{Kind: CatalogNotFound, Message: fmt.Sprintf("session %q is not stored in the catalog layout", reference)}
	}
	return location, nil
}

// newLayoutSession is the location a session reference names, whether or not it exists yet.
// A reference without a root id is only unambiguous when there is a single root.
func newLayoutSession(roots []Root, reference string) (Location, error) {
	rootID, sessionID, qualified, err := parseReference(reference)
	if err != nil {
		return Location{}, &CatalogError{Kind: CatalogBadID, Message: err.Error()}
	}
	var chosen *Root
	switch {
	case qualified:
		for index := range roots {
			if roots[index].ID == rootID {
				chosen = &roots[index]
			}
		}
		if chosen == nil {
			return Location{}, &CatalogError{Kind: CatalogBadID, Message: fmt.Sprintf("session root %q does not exist", rootID)}
		}
	case len(roots) == 1:
		chosen = &roots[0]
	default:
		return Location{}, &CatalogError{Kind: CatalogBadID, Message: "write the session as <root>:<name>; there is more than one session root"}
	}
	if chosen.Layout == nil {
		return Location{}, &CatalogError{Kind: CatalogBadID, Message: fmt.Sprintf("session root %q does not use the catalog layout", chosen.ID)}
	}
	return makeLocation(*chosen, sessionID, len(roots)), nil
}

// ReadSessionFile returns one session file as it is on disk.
func ReadSessionFile(roots []Root, reference string) (CatalogFile, error) {
	location, err := existingLayoutSession(roots, reference)
	if err != nil {
		return CatalogFile{}, err
	}
	data, err := readRegularFile(location.SettingFile(), maxInternalJSONFileBytes)
	if err != nil {
		return CatalogFile{}, configError("%s: could not be read: %v", location.SettingFile(), err)
	}
	return CatalogFile{CatalogFileInfo: CatalogFileInfo{ID: location.Reference, ETag: CatalogETag(data)}, Data: data}, nil
}

// WriteSessionFile checks data with the loader and stores it as the session file <id>.json,
// under the same preconditions as WriteCatalogFile. The result says whether the session is new.
func WriteSessionFile(roots []Root, reference string, data []byte, condition WriteCondition, syncWrite bool) (CatalogFileInfo, bool, error) {
	location, err := newLayoutSession(roots, reference)
	if err != nil {
		return CatalogFileInfo{}, false, err
	}
	path := location.File
	exists, before, err := checkWritePrecondition(path, "session", location.Reference, condition)
	if err != nil {
		return CatalogFileInfo{}, false, err
	}
	if !exists {
		if err := checkSessionIDFreeInOtherRoots(roots, location); err != nil {
			return CatalogFileInfo{}, false, err
		}
	}
	if err := validateSessionFile(location, data); err != nil {
		return CatalogFileInfo{}, false, err
	}
	if exists {
		if err := writeFileIfUnchanged(path, data, before, syncWrite); err != nil {
			return CatalogFileInfo{}, false, err
		}
	} else if err := createFileExclusive(path, data, syncWrite); err != nil {
		return CatalogFileInfo{}, false, err
	}
	return CatalogFileInfo{ID: location.Reference, ETag: CatalogETag(data)}, !exists, nil
}

// checkSessionIDFreeInOtherRoots refuses a create of an id that another root already
// holds: every root shares one state folder per id (see checkUniqueSessionIDs).
func checkSessionIDFreeInOtherRoots(roots []Root, location Location) error {
	for _, other := range roots {
		if other.ID == location.Root.ID {
			continue
		}
		info, err := os.Stat(filepath.Join(other.Directory, location.SessionID+sessionFileSuffix))
		if err == nil && info.Mode().IsRegular() {
			return &CatalogError{Kind: CatalogInUse, Message: fmt.Sprintf("session %q is already in session root %q; %q would share its state folder, so use another name", location.SessionID, other.ID, location.Root.ID)}
		}
	}
	return nil
}

// validateSessionFile loads data as the session would be loaded: from a temporary folder, so
// nothing is created in the session root until the text is accepted. The loader's messages
// name the real file.
func validateSessionFile(location Location, data []byte) error {
	scratch, err := os.MkdirTemp("", "jad-session-check-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	scratchFile := filepath.Join(scratch, filepath.Base(location.File))
	if err := os.WriteFile(scratchFile, data, 0o600); err != nil {
		return err
	}
	probe := location
	probe.File = scratchFile
	if _, err := LoadLayoutSessionSettings(probe, *location.Root.Layout, "", ""); err != nil {
		var problem *ConfigError
		if errors.As(err, &problem) {
			return configError("%s", strings.ReplaceAll(problem.Message, scratchFile, location.File))
		}
		return err
	}
	return nil
}

// CheckSessionDelete refuses a delete whose precondition does not hold, and reports the
// canonical reference of the session. It removes nothing.
func CheckSessionDelete(roots []Root, reference, ifMatch string) (Location, error) {
	location, err := existingLayoutSession(roots, reference)
	if err != nil {
		return Location{}, err
	}
	current, err := ReadSessionFile(roots, reference)
	if err != nil {
		return Location{}, err
	}
	if ifMatch == "" {
		return Location{}, &CatalogError{Kind: CatalogPreconditionRequired, Message: "If-Match is required to delete a file"}
	}
	if !IfMatchSatisfied(ifMatch, current.ETag) {
		return Location{}, &CatalogError{Kind: CatalogPreconditionFailed, Message: fmt.Sprintf("session %q changed since it was read; read it again", reference)}
	}
	return location, nil
}

// DeleteSessionFile removes the version of a session that ifMatch names: its file and the lock
// file beside it. With deleteState it also removes <state>/<session>, the transcripts of every
// profile, and returns that folder so the caller can drop what it cached for it; otherwise the
// state stays and the result is "". The caller has stopped the session's runs.
func DeleteSessionFile(roots []Root, reference, ifMatch string, deleteState bool) (string, error) {
	location, err := CheckSessionDelete(roots, reference, ifMatch)
	if err != nil {
		return "", err
	}
	if err := os.Remove(location.File); err != nil {
		return "", err
	}
	lock, err := sessionlock.PathFor(location.File)
	if err != nil {
		return "", err
	}
	if err := os.Remove(lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if !deleteState {
		return "", nil
	}
	stateDir := location.StateDir()
	return stateDir, os.RemoveAll(stateDir)
}
