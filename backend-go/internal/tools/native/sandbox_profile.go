package native

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/sessionfs"
)

// ensureProfileData makes the running profile's data folder W/.jad/data/profiles/<cat>/<name>
// (mode 0700, with its parents) before a sandbox binds it, and refuses the call when any part
// below W/.jad is a link or not a folder: a link there could bind G or the configuration
// writable. W/.jad itself may be a link; the parts are checked below its target.
func ensureProfileData(data string) error {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(data)), "/")
	if len(parts) < 6 || parts[len(parts)-5] != ".jad" || parts[len(parts)-4] != "data" || parts[len(parts)-3] != "profiles" {
		return fmt.Errorf("sandbox: %s is not a profile data folder W/.jad/data/profiles/<category>/<name>", data)
	}
	jad := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(data))))
	current, err := filepath.EvalSymlinks(jad)
	if err != nil {
		return fmt.Errorf("sandbox: the workspace folder %s of the profile data folder: %w", jad, err)
	}
	return makeBelowJad(current, parts[len(parts)-4:], data, "profile data folder")
}

// ensureProfileRoots makes the folders every profile keeps its private and data folders in
// (SandboxSettings.ProfileHidden: W/.jad/profile-own and W/.jad/data/profiles, mode 0700) before
// a sandbox starts, so their hide rule always applies: a missing one gets no rule, and a
// writable path that covers its parent could then make another profile's folder in it (decider
// F1). It refuses the call when a part below W/.jad is a link or not a folder, or cannot be
// made. A W/.jad that does not exist (a worktree's moved copy of the paths) is left out: no
// profile reads its folders there, and a sealed W/.jad keeps it from being made.
func ensureProfileRoots(hidden []string) error {
	for _, root := range hidden {
		clean := filepath.Clean(root)
		var jad string
		var parts []string
		// The parent is W/.jad as written or its link target, which may have another name.
		switch {
		case filepath.Base(clean) == "profile-own":
			jad, parts = filepath.Dir(clean), []string{"profile-own"}
		case filepath.Base(clean) == "profiles" && filepath.Base(filepath.Dir(clean)) == "data":
			jad, parts = filepath.Dir(filepath.Dir(clean)), []string{"data", "profiles"}
		default:
			return fmt.Errorf("sandbox: %s is not a profile folder root W/.jad/profile-own or W/.jad/data/profiles", root)
		}
		current, err := filepath.EvalSymlinks(jad)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sandbox: the workspace folder %s of %s: %w", jad, root, err)
		}
		if err := makeBelowJad(current, parts, root, "profile folder root"); err != nil {
			return err
		}
	}
	return nil
}

// makeBelowJad makes each of parts below the real W/.jad folder current (mode 0700, when
// missing) and refuses a part that is a link or not a folder; target and what name the folder
// in the errors.
func makeBelowJad(current string, parts []string, target, what string) error {
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("sandbox: make the %s %s: %w", what, current, err)
			}
			if info, err = os.Lstat(current); err != nil {
				return fmt.Errorf("sandbox: the %s %s: %w", what, current, err)
			}
		} else if err != nil {
			return fmt.Errorf("sandbox: the %s %s: %w", what, current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("sandbox: %s is a link or not a folder; the %s %s is not bound, and the call does not start", current, what, target)
		}
	}
	return nil
}

// sandboxProfileRules hide the folders every profile keeps its private and data folders in
// (SandboxSettings.ProfileHidden: W/.jad/profile-own and W/.jad/data/profiles) and show the
// running profile's own again (ProfileShown): read-only, but for its data folder
// (ProfileData), which is writable whether or not a writable path covers it (decider S1, F2):
// ProfileData is set only for the profile's own top-level runs and cleared everywhere else. A
// shown folder is taken by its name below the hidden folder's target, and is not shown when it
// is missing or a link or any folder between them is (a link there could show another
// profile's folder). A hidden folder that does not exist (ensureProfileRoots makes them) or is
// not in the environment gets no rule. The rules come after the sealed ones, whose read-only
// W/.jad they cover.
func sandboxProfileRules(sandbox *sessionfs.SandboxSettings, visible func(string) bool) []sandboxRule {
	if len(sandbox.ProfileHidden) == 0 {
		return nil
	}
	var rules []sandboxRule
	var done []string
	for _, hidden := range sandbox.ProfileHidden {
		real, err := filepath.EvalSymlinks(hidden)
		if err != nil || containsString(done, real) || !visible(real) {
			continue
		}
		if info, err := os.Stat(real); err != nil || !info.IsDir() {
			continue
		}
		done = append(done, real)
		rule := sandboxRule{kind: sandboxHide, path: real, dir: true}
		data := ""
		if relative, ok := localBelow(hidden, sandbox.ProfileData); ok {
			data = filepath.Join(real, relative)
		}
		var kept []string
		for _, shown := range sandbox.ProfileShown {
			relative, ok := localBelow(hidden, shown)
			if !ok {
				continue
			}
			path := filepath.Join(real, relative)
			if containsString(kept, path) || !noSymlinkBelow(real, path) {
				continue
			}
			if info, err := os.Lstat(path); err != nil || !info.IsDir() {
				continue
			}
			kept = append(kept, path)
			rule.keep = append(rule.keep, sandboxKeep{path: path, write: path == data})
		}
		rules = append(rules, rule)
	}
	return rules
}

// localBelow is path relative to root when it lies below it (not root itself).
func localBelow(root, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || !filepath.IsLocal(relative) {
		return "", false
	}
	return relative, true
}
