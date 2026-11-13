package sessionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sandboxCatalogCWD starts a catalogs.sandbox value that each session takes from its own cwd.
const sandboxCatalogCWD = "{cwd}"

// SessionFileID is the session id a root directory entry names: "<id>.json" for a regular
// file, with an id the session routes accept. Lock files and temporary files start with a
// dot or end differently, so they are never sessions.
func SessionFileID(entry os.DirEntry) (string, bool) {
	name, found := strings.CutSuffix(entry.Name(), sessionFileSuffix)
	if !found || !sessionIDPattern.MatchString(name) || name == "." || name == ".." {
		return "", false
	}
	if !entry.Type().IsRegular() {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return "", false
		}
	}
	return name, true
}

// UsesLayout reports whether the session is stored in the catalog layout: its root has
// a layout and holds <session id>.json.
func (l Location) UsesLayout() bool {
	if l.Root.Layout == nil {
		return false
	}
	info, err := os.Stat(l.File)
	return err == nil && info.Mode().IsRegular()
}

// SettingFile is the file that holds the session's own configuration and that its
// writers and lock use: <session id>.json.
func (l Location) SettingFile() string {
	return l.File
}

// StateDir is <state>/<session id>, where the session keeps its transcripts, and the base
// of every path a session setting names relative to "the session folder". Empty when the
// root has no layout.
func (l Location) StateDir() string {
	if l.Root.Layout == nil {
		return ""
	}
	return filepath.Join(l.Root.Layout.State, l.SessionID)
}

// HasSetting reports whether the directory is a session of either kind.
func (l Location) HasSetting() (bool, error) {
	info, err := os.Stat(l.SettingFile())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect session file %q: %w", l.File, err)
	}
	return info.Mode().IsRegular(), nil
}
