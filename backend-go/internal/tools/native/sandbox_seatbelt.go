package native

import (
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/sessionfs"
)

// seatbeltProfile writes the sandbox-exec profile of a seatbelt sandbox: everything is
// allowed except writing outside the writable paths (and the terminal and null devices),
// reading or writing a hidden path, and, without network, any network but local Unix
// sockets (exec and tool_smith talk to their helpers through sockets under the temp
// folder). Paths are resolved through symlinks first, because macOS names /tmp and the
// temp folder through /private and the sandbox matches resolved paths.
func seatbeltProfile(sandbox *sessionfs.SandboxSettings, expand func(string) string) string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	profile.WriteString("(allow file-write* (literal \"/dev/null\") (literal \"/dev/zero\") (literal \"/dev/dtracehelper\") (regex #\"^/dev/tty\") (regex #\"^/dev/fd/\")")
	for _, path := range sandbox.WritablePaths {
		if path = seatbeltPath(expand(path)); path != "" {
			profile.WriteString(" (subpath " + seatbeltString(path) + ")")
		}
	}
	profile.WriteString(")\n")
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
