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
	for _, part := range parts[len(parts)-4:] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("sandbox: make the profile data folder %s: %w", current, err)
			}
			if info, err = os.Lstat(current); err != nil {
				return fmt.Errorf("sandbox: the profile data folder %s: %w", current, err)
			}
		} else if err != nil {
			return fmt.Errorf("sandbox: the profile data folder %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("sandbox: %s is a link or not a folder; the profile data folder %s is not bound, and the call does not start", current, data)
		}
	}
	return nil
}

// sandboxProfileRules hide the folders every profile keeps its private and data folders in
// (SandboxSettings.ProfileHidden: W/.jad/profile-own and W/.jad/data/profiles) and show the
// running profile's own again (ProfileShown): read-only, but for its data folder
// (ProfileData), which is writable where a writable path covers it. A shown folder is taken by
// its name below the hidden folder's target, and is not shown when it is missing or a link or
// any folder between them is (a link there could show another profile's folder). A hidden
// folder that does not exist or is not in the environment gets no rule. The rules come after
// the sealed ones, whose read-only W/.jad they cover.
func sandboxProfileRules(sandbox *sessionfs.SandboxSettings, writable []string, visible func(string) bool) []sandboxRule {
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
			rule.keep = append(rule.keep, sandboxKeep{path: path, write: path == data && insideAny(path, writable)})
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
