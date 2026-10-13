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
)

// sessionLockFileName is the lock the settings writers hold next to session.json.
const sessionLockFileName = "." + layoutSessionFileName + ".jad-lock"

// ListSessionFiles lists the catalog-layout sessions of every root with the version of each
// session.json. The ID is the session reference the other routes take.
func ListSessionFiles(roots []Root) ([]CatalogFileInfo, error) {
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
			if !entry.IsDir() {
				continue
			}
			location := makeLocation(root, entry.Name(), len(roots))
			if !location.UsesLayout() {
				continue
			}
			data, err := os.ReadFile(location.SettingFile())
			if err != nil {
				return nil, configError("%s: could not be read: %v", location.SettingFile(), err)
			}
			// A file that does not parse is still listed, so it can be opened and repaired.
			var named struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(data, &named)
			infos = append(infos, CatalogFileInfo{ID: location.Reference, ETag: CatalogETag(data), Name: named.Name})
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
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

// ReadSessionFile returns one session.json as it is on disk.
func ReadSessionFile(roots []Root, reference string) (CatalogFile, error) {
	location, err := existingLayoutSession(roots, reference)
	if err != nil {
		return CatalogFile{}, err
	}
	data, err := os.ReadFile(location.SettingFile())
	if err != nil {
		return CatalogFile{}, configError("%s: could not be read: %v", location.SettingFile(), err)
	}
	return CatalogFile{CatalogFileInfo: CatalogFileInfo{ID: location.Reference, ETag: CatalogETag(data)}, Data: data}, nil
}

// WriteSessionFile checks data with the loader and stores it as the session's session.json,
// under the same preconditions as WriteCatalogFile. The result says whether the session is new.
func WriteSessionFile(roots []Root, reference string, data []byte, condition WriteCondition, syncWrite bool) (CatalogFileInfo, bool, error) {
	location, err := newLayoutSession(roots, reference)
	if err != nil {
		return CatalogFileInfo{}, false, err
	}
	path := filepath.Join(location.Directory, layoutSessionFileName)
	exists, before, err := checkWritePrecondition(path, "session", location.Reference, condition)
	if err != nil {
		return CatalogFileInfo{}, false, err
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

// validateSessionFile loads data as the session would be loaded: from a temporary folder, so
// nothing is created in the session root until the text is accepted. The loader's messages
// name the real file.
func validateSessionFile(location Location, data []byte) error {
	scratch, err := os.MkdirTemp("", "jad-session-check-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	scratchFile := filepath.Join(scratch, layoutSessionFileName)
	if err := os.WriteFile(scratchFile, data, 0o600); err != nil {
		return err
	}
	probe := location
	probe.Directory = scratch
	target := filepath.Join(location.Directory, layoutSessionFileName)
	if _, err := LoadLayoutSessionSettings(probe, *location.Root.Layout, "", ""); err != nil {
		var problem *ConfigError
		if errors.As(err, &problem) {
			return configError("%s", strings.ReplaceAll(problem.Message, scratchFile, target))
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

// DeleteSessionFile removes the version of a session that ifMatch names: session.json and
// the lock file beside it, then the folder, which must hold nothing else. With deleteState it
// also removes <state>/<session>, the transcripts of every profile, and returns that folder so
// the caller can drop what it cached for it; otherwise the state stays and the result is "".
// The caller has stopped the session's runs.
func DeleteSessionFile(roots []Root, reference, ifMatch string, deleteState bool) (string, error) {
	location, err := CheckSessionDelete(roots, reference, ifMatch)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(location.Directory)
	if err != nil {
		return "", err
	}
	var extra []string
	for _, entry := range entries {
		if name := entry.Name(); name != layoutSessionFileName && name != sessionLockFileName {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		return "", &CatalogError{Kind: CatalogInUse, Message: fmt.Sprintf("session folder %s holds files besides session.json: %s; remove them first", location.Directory, strings.Join(extra, ", "))}
	}
	if err := os.Remove(location.SettingFile()); err != nil {
		return "", err
	}
	if err := os.Remove(filepath.Join(location.Directory, sessionLockFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Remove(location.Directory); err != nil {
		return "", err
	}
	if !deleteState {
		return "", nil
	}
	stateDir := filepath.Join(location.Root.Layout.State, location.SessionID)
	return stateDir, os.RemoveAll(stateDir)
}
