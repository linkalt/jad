package sessionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"jad/backend-go/internal/remote"
)

// checkRemoteFiles reads, in the workspace layout, every binding of G/remotes and every key file
// of G/auth/keys the way a session load would, and reports what is wrong with them. A key file's
// secret is never read into the report: the messages name the key and the binding only.
func (r *CheckReport) checkRemoteFiles(layout Layout) {
	if layout.Remotes == "" {
		return
	}
	entries, problems := remote.ListBindings(layout.Remotes)
	for _, problem := range problems {
		r.Checked++
		r.add("remote", "", problem)
	}
	bindings := map[string]bool{}
	for _, entry := range entries {
		r.Checked++
		bindings[entry.Name] = true
	}
	items, err := os.ReadDir(layout.remoteKeysDir())
	if err != nil {
		return // no key files is a valid state; an unreadable folder fails a run that needs a key
	}
	for _, item := range items {
		name, ok := strings.CutSuffix(item.Name(), ".json")
		if !ok || item.IsDir() {
			continue
		}
		r.Checked++
		binding, err := remote.KeyBinding(name, filepath.Join(layout.remoteKeysDir(), item.Name()))
		if err != nil {
			r.add("remote key", name, err)
			continue
		}
		if !bindings[binding] {
			r.add("remote key", name, fmt.Errorf("key %s is for binding %s, which has no valid file in %s", name, binding, layout.Remotes))
		}
	}
}

// checkRemoteKeysOf reports a session or profile whose remote_keys cannot be used, and notes each
// binding a tool lists that no key of the session reaches: the tool leaves it out and answers
// unknown_remote for it. A note never fails the check.
func (r *CheckReport) checkRemoteKeysOf(id string, config SessionConfig) {
	if config.RemoteKeyFault != nil {
		r.add("session", id, config.RemoteKeyFault)
		return
	}
	type remoteList struct {
		tool  string
		names []string
	}
	var lists []remoteList
	if s := config.NativeProfileMessage; s != nil {
		lists = append(lists, remoteList{"profile_message", s.Remotes})
	}
	if s := config.NativeSubagent; s != nil {
		lists = append(lists, remoteList{"subagent", s.Remotes})
	}
	if s := config.NativeProfile; s != nil {
		lists = append(lists, remoteList{"profile", s.Remotes})
	}
	if s := config.NativeTranscript; s != nil {
		lists = append(lists, remoteList{"transcript", s.Remotes})
	}
	if s := config.NativeTranscripts; s != nil {
		lists = append(lists, remoteList{"transcripts", s.Remotes})
	}
	for _, list := range lists {
		for _, name := range list.names {
			if _, keyed := config.RemoteKeys[name]; !keyed {
				r.note(fmt.Sprintf("%s: tools.native.%s.remotes lists binding %s, but no key in remote_keys reaches it: the tool leaves it out and answers unknown_remote", id, list.tool, name))
			}
		}
	}
}

// note records a message that does not fail the check, once.
func (r *CheckReport) note(message string) {
	if !slices.Contains(r.Notes, message) {
		r.Notes = append(r.Notes, message)
	}
}
