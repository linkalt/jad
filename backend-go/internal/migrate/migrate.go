// Package migrate converts legacy sessions (one setting.json per session, with paths.profiles,
// mcp_servers and tools.native inside it) into the catalog layout: profiles, models, tools
// and mcp folders, a session.json per session, and the state of each profile under
// <state>/<session>/<category>/<name>/.
//
// The source is only read. The result is a new, self-contained tree that a server can be
// pointed at, and Run loads every migrated profile both ways and reports what differs.
package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/sessionlock"
)

// Options say what to convert and where to put the result.
type Options struct {
	// SourceSetting is the root setting.json of the legacy tree.
	SourceSetting string
	// Out is the directory that receives the new tree. It must be missing or empty.
	Out string
	// Environment is passed to the root loader; nil uses the process environment.
	Environment map[string]string
	// LockWait is how long to wait for a profile's transcript lock before refusing the
	// session as in use. Zero means 500 ms.
	LockWait time.Duration
}

// Profile is one converted profile.
type Profile struct {
	Legacy string // the legacy name
	ID     string // <category>/<name>
	Files  int    // state files copied
}

// Session is one converted session.
type Session struct {
	Root     string
	Name     string
	Profiles []Profile
}

// Report is what Run did. Differences is empty when every migrated profile loads to the
// same settings as before.
type Report struct {
	Out         string
	Sessions    []Session
	Skipped     []string
	Warnings    []string
	Differences []string
}

// Run converts every legacy session of the source tree.
func Run(options Options) (Report, error) {
	if strings.TrimSpace(options.SourceSetting) == "" || strings.TrimSpace(options.Out) == "" {
		return Report{}, errors.New("a source setting and an output directory are required")
	}
	out, err := filepath.Abs(options.Out)
	if err != nil {
		return Report{}, err
	}
	if err := requireEmptyDirectory(out); err != nil {
		return Report{}, err
	}
	source, err := sessionfs.Load(sessionfs.LoadOptions{RootSettingPath: options.SourceSetting, Environment: options.Environment})
	if err != nil {
		return Report{}, fmt.Errorf("read the source setting: %w", err)
	}
	if source.SettingPath == "" {
		return Report{}, errors.New("the source has no root setting.json; pass its path")
	}
	wait := options.LockWait
	if wait <= 0 {
		wait = 500 * time.Millisecond
	}
	run := &runState{
		report:  Report{Out: out},
		out:     out,
		source:  source,
		wait:    wait,
		mcpSeen: map[string][]mcpCopy{},
		taken:   map[string]string{},
		pathMap: map[string]string{},

		stateOwner: map[string]string{},
	}
	if err := run.convertAll(); err != nil {
		return run.report, err
	}
	if err := run.writeRootSetting(); err != nil {
		return run.report, err
	}
	run.verify()
	return run.report, nil
}

func requireEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty; migrate writes a new tree and never overwrites", path)
	}
	return nil
}

// runState carries what several sessions share: the MCP files already written and the
// names already used in a category.
type runState struct {
	report Report
	out    string
	source sessionfs.Config
	wait   time.Duration

	mcpSeen map[string][]mcpCopy // server id -> files written so far
	taken   map[string]string    // output path -> source it came from
	// stateOwner maps a legacy state path to the profile that used it first. Two profiles
	// may have shared one folder (tool storage under one cwd); each gets its own copy.
	stateOwner map[string]string
	toolDef    []string
	// pathMap maps a legacy path (state, model, MCP and env files, the session folder) to
	// its place in the new tree, so verify can compare two loads.
	pathMap map[string]string
	// converted keeps what verify needs.
	converted []convertedProfile
}

type mcpCopy struct {
	hash     string
	category string
}

type convertedProfile struct {
	location sessionfs.Location
	legacy   string
	id       string
	session  string
	root     string
	config   sessionfs.SessionConfig
	// moved maps each legacy state path of this profile to its place in the new tree.
	moved map[string]string
}

func (r *runState) convertAll() error {
	for _, root := range r.source.Roots {
		entries, err := os.ReadDir(root.Directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read session root %q: %w", root.ID, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if err := r.convertSession(root, name); err != nil {
				return fmt.Errorf("session %s:%s: %w", root.ID, name, err)
			}
		}
	}
	if len(r.report.Sessions) == 0 {
		return errors.New("no legacy session (a folder with a setting.json) was found")
	}
	return nil
}

// lockProfile refuses a profile whose transcript is being written by a run.
func (r *runState) lockProfile(messagesPath string) (func(), error) {
	lockPath, err := sessionlock.PathFor(messagesPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.wait)
	defer cancel()
	lock, err := sessionlock.Acquire(ctx, lockPath)
	if err != nil {
		return nil, fmt.Errorf("the transcript %s is in use (a run is active?); stop it and run again: %w", messagesPath, err)
	}
	return func() { _ = lock.Close() }, nil
}

func (r *runState) warn(format string, args ...any) {
	r.report.Warnings = append(r.report.Warnings, fmt.Sprintf(format, args...))
}

func rawObject(data []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("expected a JSON object")
	}
	return fields, nil
}
