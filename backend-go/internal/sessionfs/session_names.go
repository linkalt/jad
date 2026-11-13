package sessionfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A session root is the folder of the session files <id>.json, but it may hold other JSON too, so
// a name alone does not say that a file was meant to be a session. A .json file whose name no
// session route accepts ("my session (1).json", the copy a file manager makes) is reported only
// when its content is a session: an object with only the keys of a session file (sessionFileKeys)
// and at least one that says it is not some other object (sessionMarkerKeys). The file is never
// loaded or listed under that name; its name is the only fault, and renaming it brings it back.

// sessionFileKeys are the top-level keys of layoutSessionFile. "category" is the removed session
// category (T-041): it is tolerated, so a file that sets it is still known as a session, but it is
// no marker, since it says nothing now.
var sessionFileKeys = map[string]struct{}{
	"schema_version": {}, "description": {}, "name": {}, "cwd": {}, "category": {}, "scope": {},
	"model_defaults": {}, "settings": {}, "remote_keys": {},
}

// sessionMarkerKeys are the session keys a bare {"name", "description"} object lacks. One of
// them must be present.
var sessionMarkerKeys = []string{"schema_version", "cwd", "scope", "model_defaults", "settings"}

const sessionNameRule = "a session id may only hold letters, digits, '.', '_' and '-', must start with a letter or digit and may be at most 128 characters"

// MisnamedSessions finds the session files that a session root holds under a name no session
// route accepts. It remembers its verdict on each file by size and modification time, so the
// session list, which runs every few seconds, reads such a file once and not at every poll. The
// zero value is ready; a nil *MisnamedSessions reads every time.
type MisnamedSessions struct {
	mu   sync.Mutex
	seen map[string]misnamedVerdict
}

type misnamedVerdict struct {
	stamp   fileStamp
	session bool
}

// Find returns one warning for each entry of the root's directory that holds a session but is
// named so that it is not one. entries are the root's directory entries, as os.ReadDir lists them.
func (m *MisnamedSessions) Find(root Root, entries []os.DirEntry) []string {
	var warnings []string
	directory := filepath.Clean(root.Directory)
	current := map[string]misnamedVerdict{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, sessionFileSuffix) || strings.HasPrefix(name, ".") || entry.IsDir() {
			continue
		}
		if _, valid := SessionFileID(entry); valid {
			continue
		}
		path := filepath.Join(directory, name)
		verdict, ok := m.lookup(path)
		if !ok {
			verdict = misnamedVerdict{stamp: statStamp(path)}
			verdict.session = looksLikeSession(path)
		}
		current[path] = verdict
		if verdict.session {
			warnings = append(warnings, fmt.Sprintf("session root %q: file %s left out: it holds a session but is not named like one (%s)", root.ID, path, sessionNameRule))
		}
	}
	if m != nil {
		m.mu.Lock()
		if m.seen == nil {
			m.seen = map[string]misnamedVerdict{}
		}
		// The root's other verdicts are kept; a file gone from this listing is forgotten.
		for path := range m.seen {
			if filepath.Dir(path) == directory {
				delete(m.seen, path)
			}
		}
		for path, verdict := range current {
			m.seen[path] = verdict
		}
		m.mu.Unlock()
	}
	return warnings
}

// lookup returns the remembered verdict on path when the file still has the size and time it
// had when it was read, and that stamp could not hide a later write.
func (m *MisnamedSessions) lookup(path string) (misnamedVerdict, bool) {
	if m == nil {
		return misnamedVerdict{}, false
	}
	m.mu.Lock()
	verdict, ok := m.seen[path]
	m.mu.Unlock()
	if !ok || verdict.stamp.racy() {
		return misnamedVerdict{}, false
	}
	now := statStamp(path)
	if now.exists != verdict.stamp.exists || now.size != verdict.stamp.size || !now.modTime.Equal(verdict.stamp.modTime) {
		return misnamedVerdict{}, false
	}
	return verdict, true
}

// looksLikeSession reports whether the file is a JSON object of the shape of a session file.
func looksLikeSession(path string) bool {
	contents, err := readRegularFile(path, maxInternalJSONFileBytes)
	if err != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(contents, &fields) != nil || len(fields) == 0 {
		return false
	}
	for key := range fields {
		if _, known := sessionFileKeys[key]; !known {
			return false
		}
	}
	for _, key := range sessionMarkerKeys {
		if _, has := fields[key]; has {
			return true
		}
	}
	return false
}
