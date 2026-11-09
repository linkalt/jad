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
	// SandboxBackendSeatbelt is macOS's sandbox-exec with a profile jad writes from the same
	// settings as bwrap's: writes only to the writable paths, the hidden paths unreadable,
	// and no network unless network is true.
	SandboxBackendSeatbelt    = "seatbelt"
	defaultSeatbeltExecutable = "sandbox-exec"

	defaultSandboxBackend    = SandboxBackendBwrap
	defaultSandboxExecutable = "bwrap"
	maxSandboxPaths          = 64

	// defaultSandboxProxyPort is the loopback port inside the sandbox where network_allow's
	// proxy answers; the child's HTTP_PROXY and HTTPS_PROXY name it.
	defaultSandboxProxyPort int64 = 3128
	maxSandboxNetworkAllow        = 256

	defaultSandboxCheckTimeoutMs int64 = 10000
	minSandboxCheckTimeoutMs     int64 = 100
	maxSandboxCheckTimeoutMs     int64 = 600000
)

// SandboxTools are the tools whose child processes a sandbox can wrap. hooks (the commands of
// the hooks setting) and mcp (the session's stdio MCP servers) are choices: the default
// covers only the tools the model drives. sandbox.tools is a filter, not a list checked
// against this one or against the run's tool set: a sandbox file is shared by profiles and
// sub-agents with different tools, so a listed tool the run lacks or that starts no child is
// accepted and has no effect.
var SandboxTools = []string{"exec", "tool_smith", "hooks", "mcp"}

var defaultSandboxTools = []string{"exec", "tool_smith"}

// defaultSandboxWritablePaths keep the project, its git directory and the system temp folder
// writable: git in a worktree writes to the repository's git directory, outside the worktree,
// and exec and tool_smith keep their code, result and socket files under the temp folder.
var defaultSandboxWritablePaths = []string{"{workspace}", "{git_common_dir}", "{tmp}"}

// defaultSandboxPlanWritablePaths keep only the temp folder writable in a plan-mode run, where
// exec and tool_smith still need their files: the project and its git directory are read-only.
var defaultSandboxPlanWritablePaths = []string{"{tmp}"}

var sandboxKeys = []string{"enabled", "tools", "backend", "executable", "network", "network_allow", "proxy_port", "writable_paths", "plan_writable_paths", "hide_paths", "command", "check_timeout_ms"}

var sandboxPlaceholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

// sandboxPlaceholders are what a sandbox path or command may name; each is filled in when a
// child starts: the call's workspace root, its working directory, the git directory shared by
// the workspace's repository and its worktrees, the call's own temp folder (one per session,
// workspace and plan-mode form; the system temp folder for a process outside a call, such as
// an MCP server) and the user's home folder. One that names nothing (no repository, no home)
// leaves its path out.
var sandboxPlaceholders = map[string]bool{"workspace": true, "cwd": true, "git_common_dir": true, "tmp": true, "home": true}

const sandboxPlaceholderList = "{workspace}, {cwd}, {git_common_dir}, {tmp}, {home}"

// SandboxSettings is the sandbox setting: the child processes of the listed tools start
// inside it. With backend bwrap, the whole file system is read-only except WritablePaths,
// HidePaths are empty, every namespace is new and the network is cut unless Network is set.
// With backend command, Command is put in front of the program and its arguments.
type SandboxSettings struct {
	Tools      []string
	Backend    string
	Executable string
	Network    bool
	// NetworkAllow, when not empty (backend bwrap, network false), lets the children reach
	// these hosts and their subdomains, and nothing else: the sandbox has no network of its
	// own, and a forwarder inside it hands loopback ProxyPort to jad's filtering proxy.
	NetworkAllow  []string
	ProxyPort     int64
	WritablePaths []string
	// PlanWritablePaths replace WritablePaths for the calls of a plan-mode run and for an
	// exec call with read_only (backend bwrap or seatbelt), so a tool kept in plan mode, or a
	// call that says it only reads, cannot change the project even if it was wrongly marked
	// read-only or an allow entry kept it.
	PlanWritablePaths []string
	HidePaths         []string
	Command           []string
	// CheckTimeoutMs bounds the trial start "jad check" makes to see that the sandbox works.
	CheckTimeoutMs int64
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
	settings := &SandboxSettings{Tools: append([]string(nil), defaultSandboxTools...), Backend: defaultSandboxBackend, Executable: defaultSandboxExecutable}
	if raw, exists := fields["tools"]; exists && strings.TrimSpace(string(raw)) != "null" {
		var names []string
		if err := json.Unmarshal(raw, &names); err != nil || len(names) == 0 {
			return nil, configError("%s: %s.tools must be a non-empty array of tool names", path, section)
		}
		settings.Tools = nil
		for index, name := range names {
			if strings.TrimSpace(name) == "" {
				return nil, configError("%s: %s.tools[%d] must be a non-empty tool name", path, section, index)
			}
			if !containsString(settings.Tools, name) {
				settings.Tools = append(settings.Tools, name)
			}
		}
	}
	if raw, exists := fields["backend"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.Backend); err != nil || (settings.Backend != SandboxBackendBwrap && settings.Backend != SandboxBackendCommand && settings.Backend != SandboxBackendSeatbelt) {
			return nil, configError("%s: %s.backend must be %q, %q or %q", path, section, SandboxBackendBwrap, SandboxBackendSeatbelt, SandboxBackendCommand)
		}
		if settings.Backend == SandboxBackendSeatbelt {
			settings.Executable = defaultSeatbeltExecutable
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
	if raw, exists := fields["network_allow"]; exists && strings.TrimSpace(string(raw)) != "null" {
		var hosts []string
		if err := json.Unmarshal(raw, &hosts); err != nil || hosts == nil || len(hosts) > maxSandboxNetworkAllow {
			return nil, configError("%s: %s.network_allow must be a list of at most %d host names", path, section, maxSandboxNetworkAllow)
		}
		normalized, ok := NormalizeWebSearchDomains(hosts)
		if !ok {
			return nil, configError("%s: %s.network_allow must be a list of at most %d host names", path, section, maxSandboxNetworkAllow)
		}
		settings.NetworkAllow = normalized
	}
	if settings.ProxyPort, err = configuredInteger(fields, "proxy_port", defaultSandboxProxyPort, 1, 65535, path, section); err != nil {
		return nil, err
	}
	if len(settings.NetworkAllow) > 0 && settings.Network {
		return nil, configError("%s: %s.network_allow narrows a sandbox without network; with network true every host is reachable, so set one or the other", path, section)
	}
	if settings.WritablePaths, err = sandboxPaths(fields, "writable_paths", defaultSandboxWritablePaths, path, section); err != nil {
		return nil, err
	}
	if settings.PlanWritablePaths, err = sandboxPaths(fields, "plan_writable_paths", defaultSandboxPlanWritablePaths, path, section); err != nil {
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
	if settings.CheckTimeoutMs, err = configuredInteger(fields, "check_timeout_ms", defaultSandboxCheckTimeoutMs, minSandboxCheckTimeoutMs, maxSandboxCheckTimeoutMs, path, section); err != nil {
		return nil, err
	}
	switch settings.Backend {
	case SandboxBackendCommand:
		if len(settings.NetworkAllow) > 0 {
			return nil, configError("%s: %s.network_allow needs backend %q, which can cut the network", path, section, SandboxBackendBwrap)
		}
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
	case SandboxBackendSeatbelt:
		if len(settings.Command) > 0 {
			return nil, configError("%s: %s.command is for backend %q", path, section, SandboxBackendCommand)
		}
		if len(settings.NetworkAllow) > 0 {
			return nil, configError("%s: %s.network_allow needs backend %q; with %q the network is all or nothing", path, section, SandboxBackendBwrap, SandboxBackendSeatbelt)
		}
	}
	if !enabled {
		return nil, nil
	}
	if settings.Backend == SandboxBackendBwrap && runtime.GOOS != "linux" {
		return nil, configError("%s: %s.backend %q runs on Linux only; use backend %q with a sandbox program of this system", path, section, SandboxBackendBwrap, SandboxBackendCommand)
	}
	if settings.Backend == SandboxBackendSeatbelt && runtime.GOOS != "darwin" {
		return nil, configError("%s: %s.backend %q runs on macOS only", path, section, SandboxBackendSeatbelt)
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

// ForPlanMode returns the sandbox a plan-mode run's calls start in: with backend bwrap, the
// same sandbox with PlanWritablePaths as its writable paths. A command backend is the
// person's own program and is returned unchanged, as is no sandbox.
func (s *SandboxSettings) ForPlanMode() *SandboxSettings {
	if s == nil || (s.Backend != SandboxBackendBwrap && s.Backend != SandboxBackendSeatbelt) {
		return s
	}
	plan := *s
	plan.WritablePaths = append([]string(nil), s.PlanWritablePaths...)
	return &plan
}

// applySandbox hands an enabled sandbox to each enabled tool it covers, and to the hooks
// when it covers them.
func applySandbox(sandbox *SandboxSettings, execSettings *NativeExecSettings, toolSmith *NativeToolSmithSettings, hooks *HooksSettings) {
	if sandbox == nil {
		return
	}
	if hooks != nil && sandbox.Covers("hooks") {
		hooks.Sandbox = sandbox
	}
	if execSettings != nil && sandbox.Covers("exec") {
		execSettings.Child.Sandbox = sandbox
	}
	if toolSmith != nil && sandbox.Covers("tool_smith") {
		toolSmith.Child.Sandbox = sandbox
	}
}
