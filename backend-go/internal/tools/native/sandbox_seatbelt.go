package native

import (
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/sessionfs"
)

// seatbeltProfile writes the sandbox-exec profile of a seatbelt sandbox: everything is
// allowed except reading outside the read paths (when they are not the whole file system),
// writing outside the writable paths (and the terminal and null devices), the fixed rules
// (sandboxFixedRules), reading or writing a hidden path, and, without network, any network
// but local Unix sockets (exec and tool_smith talk to their helpers through sockets under the
// temp folder). Paths are resolved through symlinks first, because macOS names /tmp and the
// temp folder through /private and the sandbox matches resolved paths. A later rule wins over
// an earlier one. The read rules are not verified on a Mac (docs/isolation-decisions.md P0).
func seatbeltProfile(sandbox *sessionfs.SandboxSettings, place sandboxPlace, expand func(string) string) string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(allow default)\n")
	writes := sandboxWritePaths(sandbox.WritablePaths, place)
	writable := sandboxWritableRoots(writes, expand)
	whole := sandbox.WholeFileSystemReadable()
	var readable []sandboxReadRoot
	if !whole {
		readable = sandboxReadRoots(sandbox.ReadPaths, expand, writable)
	}
	// A shown profile folder outside the read paths stays unreadable.
	visible := func(path string) bool { return whole || sandboxVisible(path, readable, writable) }
	if !whole {
		// Metadata stays readable so that programs can walk to what they may read.
		profile.WriteString("(deny file-read*)\n(allow file-read-metadata)\n(allow file-read* (subpath \"/dev\")")
		for _, root := range readable {
			profile.WriteString(" (subpath " + seatbeltString(root.real) + ")")
		}
		for _, path := range writable {
			profile.WriteString(" (subpath " + seatbeltString(path) + ")")
		}
		profile.WriteString(")\n")
	}
	profile.WriteString("(deny file-write*)\n")
	profile.WriteString("(allow file-write* (literal \"/dev/null\") (literal \"/dev/zero\") (literal \"/dev/dtracehelper\") (regex #\"^/dev/tty\") (regex #\"^/dev/fd/\")")
	for _, path := range writable {
		profile.WriteString(" (subpath " + seatbeltString(path) + ")")
	}
	// A writable path that does not exist yet stays allowed, as before: seatbelt matches
	// names when the call opens them, so nothing is bound that a symlink could redirect.
	for _, path := range writes {
		if path = seatbeltPath(expand(path)); path != "" {
			if _, err := os.Lstat(path); err != nil {
				profile.WriteString(" (subpath " + seatbeltString(path) + ")")
			}
		}
	}
	profile.WriteString(")\n")
	profile.WriteString(seatbeltRuleProfile(sandboxFixedRules(place, writable, sandbox, sandboxWritableCandidates(writes, expand), visible)))
	for _, path := range sandbox.HidePaths {
		path = seatbeltPath(expand(path))
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		filter := "(literal " + seatbeltString(path) + ")"
		if info.IsDir() {
			filter = "(subpath " + seatbeltString(path) + ")"
		}
		profile.WriteString("(deny file-read* file-write* " + filter + ")\n")
	}
	if !sandbox.Network {
		profile.WriteString("(deny network*)\n(allow network* (local unix-socket) (remote unix-socket))\n")
	}
	return profile.String()
}

// seatbeltPath is path made absolute and resolved through symlinks as far as it exists;
// empty stays empty.
func seatbeltPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}

// seatbeltString quotes a string for the profile language.
func seatbeltString(value string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value) + "\""
}
