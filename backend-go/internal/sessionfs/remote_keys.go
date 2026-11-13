package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/remote"
)

// remote_keys (workspace layout, docs/design_workspace_layout.md §30.4): the names of the key files
// (G/auth/keys/<key>.json) whose credentials the profiles of a workspace, a session or one profile
// may use for the remote bindings the key files name. Levels, nearest wins: workspace.json, the
// session file, the profile file; absent inherits, an empty list means none, a non-empty list
// replaces the levels above it. A key gives a binding its credential and nothing else: what a tool
// may reach is that tool's own remotes list. It is never a key of a toolset, and a profile cannot
// change its own.

const remoteKeysKey = "remote_keys"

// readRemoteKeyNames reads a remote_keys value: nil when absent, else the distinct key names.
func readRemoteKeyNames(raw json.RawMessage, where string) (*[]string, error) {
	if raw == nil {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil || names == nil {
		return nil, configError("%s must be an array of key names (the files of G/auth/keys without .json)", where)
	}
	return checkRemoteKeyNames(&names, where)
}

// checkRemoteKeyNames validates the names of an already decoded remote_keys.
func checkRemoteKeyNames(names *[]string, where string) (*[]string, error) {
	if names == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, name := range *names {
		if !remote.NamePattern.MatchString(name) {
			return nil, configError("%s: %q is not a key name (1 to 64 letters, digits, '_' or '-')", where, name)
		}
		if seen[name] {
			return nil, configError("%s: lists key %q more than once", where, name)
		}
		seen[name] = true
	}
	return names, nil
}

// remoteKeysDir is G/auth/keys.
func (l Layout) remoteKeysDir() string { return filepath.Join(l.GlobalDir, "auth", "keys") }

// resolveRemoteKeys is the session's effective keys by binding. The nearest level that sets
// remote_keys decides (profile, session, workspace); each key must have a valid key file and
// name a binding that exists, and two effective keys may not name one binding. A broken key fails
// the load of the session that uses it, with the key's name and the file that listed it.
func (l Layout) resolveRemoteKeys(session layoutSessionFile, sessionPath string, profile layoutProfileFile, profilePath string) (map[string]RemoteKey, error) {
	names, where := l.RemoteKeys, l.WorkspaceSetting
	if session.RemoteKeys != nil {
		names, where = session.RemoteKeys, sessionPath
	}
	if profile.RemoteKeys != nil {
		names, where = profile.RemoteKeys, profilePath
	}
	if names == nil || len(*names) == 0 {
		return nil, nil
	}
	keys := make(map[string]RemoteKey, len(*names))
	for _, name := range *names {
		path := filepath.Join(l.remoteKeysDir(), name+".json")
		binding, err := remote.KeyBinding(name, path)
		if err != nil {
			return nil, configError("%s: remote_keys: %v", where, err)
		}
		if _, err := remote.LoadBinding(l.Remotes, binding); err != nil {
			return nil, configError("%s: remote_keys: key %s is for binding %s, which cannot be used: %s", where, name, binding, strings.TrimPrefix(err.Error(), "remote "+binding+": "))
		}
		if other, taken := keys[binding]; taken {
			return nil, configError("%s: remote_keys: keys %s and %s are both for binding %s; a session has one key per binding", where, other.Name, name, binding)
		}
		keys[binding] = RemoteKey{Name: name, File: path}
	}
	return keys, nil
}
