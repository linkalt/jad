package sessionfs

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	SandboxBackendBwrap   = "bwrap"
	SandboxBackendCommand = "command"

	defaultSandboxBackend    = SandboxBackendBwrap
	defaultSandboxExecutable = "bwrap"
	maxSandboxPaths          = 64
)

// SandboxTools are the tools whose child processes a sandbox can wrap.
var SandboxTools = []string{"bash", "exec", "process", "tool_smith"}

// defaultSandboxWritablePaths keep the project, its git directory and the system temp folder
// writable: git in a worktree writes to the repository's git directory, outside the worktree,
// and exec and tool_smith keep their code, result and socket files under the temp folder.
var defaultSandboxWritablePaths = []string{"{workspace}", "{git_common_dir}", "{tmp}"}

var sandboxKeys = []string{"enabled", "tools", "backend", "executable", "network", "writable_paths", "hide_paths", "command"}

var sandboxPlaceholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

// sandboxPlaceholders are what a sandbox path or command may name; each is filled in when a
// child starts: the call's workspace root, its working directory, the git directory shared by
// the workspace's repository and its worktrees, the system temp folder and the user's home
// folder. One that names nothing (no repository, no home) leaves its path out.
var sandboxPlaceholders = map[string]bool{"workspace": true, "cwd": true, "git_common_dir": true, "tmp": true, "home": true}

const sandboxPlaceholderList = "{workspace}, {cwd}, {git_common_dir}, {tmp}, {home}"

// SandboxSettings is the sandbox setting: the child processes of the listed tools start
// inside it. With backend bwrap, the whole file system is read-only except WritablePaths,
// HidePaths are empty, every namespace is new and the network is cut unless Network is set.
// With backend command, Command is put in front of the program and its arguments.
type SandboxSettings struct {
	Tools         []string
	Backend       string
	Executable    string
	Network       bool
	WritablePaths []string
	HidePaths     []string
	Command       []string
}

// Covers reports whether a tool's child processes start in the sandbox.
func (s *SandboxSettings) Covers(tool string) bool {
	return s != nil && containsString(s.Tools, tool)
}

// normalizeSandbox reads sandbox; absent, null or enabled false is nil (no sandbox), which is
// the default. An enabled sandbox whose program cannot be found is refused, so a session never
// runs commands outside a sandbox it asked for.
func normalizeSandbox(raw json.RawMessage, path string) (*SandboxSettings, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	const section = "sandbox"
	fields, err := decodeConfigObject(raw, path+": "+section)
	if err != nil {
		return nil, configError("%s: %s must be an object", path, section)
	}
	for key := range fields {
		if !containsString(sandboxKeys, key) {
			return nil, unsupportedToolKey(path+": "+section, key, sandboxKeys, "")
		}
	}
	enabled, err := bashBool(fields, "enabled", false, path+": "+section)
	if err != nil {
		return nil, err
	}
	settings := &SandboxSettings{Tools: append([]string(nil), SandboxTools...), Backend: defaultSandboxBackend, Executable: defaultSandboxExecutable}
	if raw, exists := fields["tools"]; exists && strings.TrimSpace(string(raw)) != "null" {
		var names []string
		if err := json.Unmarshal(raw, &names); err != nil || len(names) == 0 {
			return nil, configError("%s: %s.tools must list one or more of %s", path, section, strings.Join(SandboxTools, ", "))
		}
		settings.Tools = nil
		for index, name := range names {
			if !containsString(SandboxTools, name) {
				return nil, configError("%s: %s.tools[%d] must be one of %s", path, section, index, strings.Join(SandboxTools, ", "))
			}
			if containsString(settings.Tools, name) {
				return nil, configError("%s: %s.tools lists %s twice", path, section, name)
			}
			settings.Tools = append(settings.Tools, name)
		}
	}
	if raw, exists := fields["backend"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.Backend); err != nil || (settings.Backend != SandboxBackendBwrap && settings.Backend != SandboxBackendCommand) {
			return nil, configError("%s: %s.backend must be %q or %q", path, section, SandboxBackendBwrap, SandboxBackendCommand)
		}
	}
	if raw, exists := fields["executable"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.Executable); err != nil || strings.TrimSpace(settings.Executable) == "" {
			return nil, configError("%s: %s.executable must be a program name or path", path, section)
		}
		settings.Executable = strings.TrimSpace(settings.Executable)
	}
	if settings.Network, err = bashBool(fields, "network", false, path+": "+section); err != nil {
		return nil, err
	}
	if settings.WritablePaths, err = sandboxPaths(fields, "writable_paths", defaultSandboxWritablePaths, path, section); err != nil {
		return nil, err
	}
	if settings.HidePaths, err = sandboxPaths(fields, "hide_paths", nil, path, section); err != nil {
		return nil, err
	}
	if raw, exists := fields["command"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.Command); err != nil || len(settings.Command) == 0 || strings.TrimSpace(settings.Command[0]) == "" {
			return nil, configError("%s: %s.command must be a non-empty array of strings (the program first)", path, section)
		}
		for index, part := range settings.Command {
			if err := checkSandboxPlaceholders(part); err != nil {
				return nil, configError("%s: %s.command[%d]: %v", path, section, index, err)
			}
		}
	}
	switch settings.Backend {
	case SandboxBackendCommand:
		if len(settings.Command) == 0 {
			return nil, configError("%s: %s.command is required with backend %q", path, section, SandboxBackendCommand)
		}
		if _, exists := fields["executable"]; exists {
			return nil, configError("%s: %s.executable is for backend %q; with backend %q the program is command[0]", path, section, SandboxBackendBwrap, SandboxBackendCommand)
		}
		settings.Executable = settings.Command[0]
	case SandboxBackendBwrap:
		if len(settings.Command) > 0 {
			return nil, configError("%s: %s.command is for backend %q", path, section, SandboxBackendCommand)
		}
	}
	if !enabled {
		return nil, nil
	}
	if settings.Backend == SandboxBackendBwrap && runtime.GOOS != "linux" {
		return nil, configError("%s: %s.backend %q runs on Linux only; use backend %q with a sandbox program of this system", path, section, SandboxBackendBwrap, SandboxBackendCommand)
	}
	if !sandboxPlaceholderPattern.MatchString(settings.Executable) {
		if _, err := exec.LookPath(settings.Executable); err != nil {
			return nil, configError("%s: %s is enabled but its program %q cannot be run: %v", path, section, settings.Executable, err)
		}
	}
	return settings, nil
}

func sandboxPaths(fields map[string]json.RawMessage, name string, fallback []string, path, section string) ([]string, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return append([]string(nil), fallback...), nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil || len(values) > maxSandboxPaths {
		return nil, configError("%s: %s.%s must be an array of at most %d paths", path, section, name, maxSandboxPaths)
	}
	paths := make([]string, 0, len(values))
	for index, value := range values {
		value = strings.TrimSpace(value)
		if err := checkSandboxPlaceholders(value); err != nil {
			return nil, configError("%s: %s.%s[%d]: %v", path, section, name, index, err)
		}
		if !strings.HasPrefix(value, "{") && !filepath.IsAbs(value) {
			return nil, configError("%s: %s.%s[%d] must be an absolute path or start with a placeholder (%s)", path, section, name, index, sandboxPlaceholderList)
		}
		paths = append(paths, value)
	}
	return paths, nil
}

func checkSandboxPlaceholders(value string) error {
	for _, match := range sandboxPlaceholderPattern.FindAllStringSubmatch(value, -1) {
		if !sandboxPlaceholders[match[1]] {
			return configError("unknown placeholder {%s}; known are %s", match[1], sandboxPlaceholderList)
		}
	}
	return nil
}

// applySandbox hands an enabled sandbox to each enabled tool it covers.
func applySandbox(sandbox *SandboxSettings, bash *NativeBashSettings, execSettings *NativeExecSettings, toolSmith *NativeToolSmithSettings, process *NativeProcessSettings) {
	if sandbox == nil {
		return
	}
	if bash != nil && sandbox.Covers("bash") {
		bash.Sandbox = sandbox
	}
	if execSettings != nil && sandbox.Covers("exec") {
		execSettings.Child.Sandbox = sandbox
	}
	if toolSmith != nil && sandbox.Covers("tool_smith") {
		toolSmith.Child.Sandbox = sandbox
	}
	if process != nil && sandbox.Covers("process") {
		process.Sandbox = sandbox
	}
}
