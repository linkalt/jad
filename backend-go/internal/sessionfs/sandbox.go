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
	// DefaultSandboxExecutable is the bwrap a sandbox without executable starts, and the one
	// an overlay run's holder starts when the run has no bwrap sandbox of its own.
	DefaultSandboxExecutable = defaultSandboxExecutable
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
// the hooks setting), mcp (the session's stdio MCP servers) and lsp (the language servers of
// the lsp tool) are choices: the default covers only the tools the model drives.
// sandbox.tools is a filter, not a list checked against this one or against the run's tool
// set: a sandbox file is shared by profiles and sub-agents with different tools, so a listed
// tool the run lacks or that starts no child is accepted and has no effect.
var SandboxTools = []string{"exec", "tool_smith", "hooks", "mcp", "lsp"}

var defaultSandboxTools = []string{"exec", "tool_smith"}

// defaultSandboxReadPaths is the whole file system: read.paths lists the only paths a child
// can read, and its default keeps every sandbox as it was before the key existed.
var defaultSandboxReadPaths = []string{"/"}

// defaultSandboxWritablePaths keep the project, its git directory and the system temp folder
// writable: git in a worktree writes to the repository's git directory, outside the worktree,
// and exec and tool_smith keep their code, result and socket files under the temp folder.
var defaultSandboxWritablePaths = []string{"{workspace}", "{git_common_dir}", "{tmp}"}

// defaultSandboxPlanWritablePaths keep only the temp folder writable in a plan-mode run, where
// exec and tool_smith still need their files: the project and its git directory are read-only.
var defaultSandboxPlanWritablePaths = []string{"{tmp}"}

// defaultSandboxHidePaths are the credential stores a sandboxed child should not read: ssh and
// gpg keys, cloud and cluster logins, git, package registry and container registry tokens, and
// jad's credential directory ({home}/.jad/auth, where the removed provider sign-in tools
// wrote their files; still hidden so a leftover file stays unreadable). A profile that needs one replaces
// hide_paths; add_hide_paths adds to the list instead (docs/isolation-decisions.md D5).
var defaultSandboxHidePaths = []string{
	"{home}/.ssh", "{home}/.gnupg", "{home}/.aws", "{home}/.kube", "{home}/.config/gcloud", "{home}/.azure",
	"{home}/.config/gh", "{home}/.git-credentials", "{home}/.netrc", "{home}/.npmrc", "{home}/.pypirc",
	"{home}/.cargo/credentials", "{home}/.docker/config.json", "{home}/.jad/auth",
}

var sandboxKeys = []string{"template", "enabled", "tools", "backend", "executable", "network", "network_allow", "proxy_port", "read", "write", "writable_paths", "plan_writable_paths", "hide_paths", "add_hide_paths", "isolation", "command", "check_timeout_ms"}

// sandboxReadKeys and sandboxWriteKeys are the keys of sandbox.read and sandbox.write; each
// add_ key adds to the key it names instead of replacing it (D26).
var (
	sandboxReadKeys      = []string{"paths", "add_paths"}
	sandboxWriteKeys     = []string{"paths", "add_paths", "plan_paths", "add_plan_paths"}
	sandboxIsolationKeys = []string{"mode", "base", "materialize", "share_paths"}
)

// sandboxAddKeys maps a list key of a sandbox object (or of its read and write objects) to
// the key that adds to it.
var sandboxAddKeys = map[string]string{"paths": "add_paths", "plan_paths": "add_plan_paths", "hide_paths": "add_hide_paths"}

var sandboxPlaceholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

// sandboxPlaceholders are what a sandbox path or command may name; each is filled in when a
// child starts: the call's workspace root, its working directory, the git directory shared by
// the workspace's repository and its worktrees, the call's own temp folder (one per session,
// workspace and plan-mode form; the system temp folder for a process outside a call, such as
// an MCP server), the user's home folder and the session's state folder. One that names
// nothing (no repository, no home) leaves its path out. {system} is the platform's runtime
// folders (SandboxSystemPaths) and is a whole read path of its own. {profile_data} is the
// running profile's data folder (ProfileDataPlaceholder).
var sandboxPlaceholders = map[string]bool{"workspace": true, "cwd": true, "git_common_dir": true, "tmp": true, "home": true, "session_dir": true, "system": true, "profile_data": true}

const sandboxPlaceholderList = "{workspace}, {cwd}, {git_common_dir}, {tmp}, {home}, {session_dir}, {system}, {profile_data}"

// ProfileDataPlaceholder is the running profile's data folder in a sandbox path (always
// the workspace layout). It names nothing where the sandbox is not that of the profile's
// own top-level run (SandboxSettings.ProfileData is empty), so its path is left out there.
const ProfileDataPlaceholder = "{profile_data}"

// WithoutProfileData is the sandbox with its ProfileData cleared: a copy when it has one.
func (s *SandboxSettings) WithoutProfileData() *SandboxSettings {
	if s == nil || s.ProfileData == "" {
		return s
	}
	copied := *s
	copied.ProfileData = ""
	return &copied
}

// SandboxSystemPlaceholder is the read path that stands for the platform's runtime folders.
const SandboxSystemPlaceholder = "{system}"

// SandboxSystemPaths are the folders {system} stands for on this platform: what programs need
// to start (binaries, libraries, the system configuration). Each one that does not exist is
// left out, like any other path.
func SandboxSystemPaths() []string {
	if runtime.GOOS == "darwin" {
		// Not verified on a Mac (docs/isolation-decisions.md, P0).
		return []string{"/System", "/usr", "/bin", "/sbin", "/Library", "/private/etc", "/private/var/db", "/dev"}
	}
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc"}
}

// Sandbox isolation modes (sandbox.isolation.mode): none runs a delegated run in the session's
// working directory, worktree in a git worktree of its own (the agent key isolation).
const (
	SandboxIsolationNone     = "none"
	SandboxIsolationWorktree = "worktree"
	// Bases of sandbox.isolation.base: head starts a run's worktree from HEAD, working from
	// a snapshot of the session's tracked and untracked, not ignored files.
	SandboxIsolationBaseHead    = "head"
	SandboxIsolationBaseWorking = "working"
	// Materialisers of sandbox.isolation.materialize: how a run's worktree gets its files.
	// checkout writes a full git checkout; overlay mounts the run's changes over a shared,
	// read-only checkout of the start commit inside a per-run bwrap holder (Linux, bwrap 0.10
	// or later, nsenter); auto, the default, takes overlay where a trial start shows it works
	// and checkout elsewhere (docs/isolation-decisions.md D1, D14).
	SandboxIsolationMaterializeAuto     = "auto"
	SandboxIsolationMaterializeCheckout = "checkout"
	SandboxIsolationMaterializeOverlay  = "overlay"
)

// SandboxSettings is the sandbox setting: the child processes of the listed tools start
// inside it. With backend bwrap, a child sees only ReadPaths (read-only), WritablePaths
// (read-write) and nothing of HidePaths; every namespace is new and the network is cut unless
// Network is set. With backend command, Command is put in front of the program and its
// arguments.
type SandboxSettings struct {
	// Template is the name of the .jad/sandbox file the setting came from; empty for an
	// inline object.
	Template   string `json:",omitempty"`
	Tools      []string
	Backend    string
	Executable string
	Network    bool
	// NetworkAllow, when not empty (backend bwrap, network false), lets the children reach
	// these hosts and their subdomains, and nothing else: the sandbox has no network of its
	// own, and a forwarder inside it hands loopback ProxyPort to jad's filtering proxy.
	NetworkAllow []string
	ProxyPort    int64
	// ReadPaths are the only paths a child can read (read.paths); ["/"], the default, is the
	// whole file system. Every writable path is readable too: the loader adds them here
	// unless "/" already covers everything.
	ReadPaths     []string
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
	// ReadOnlyPaths are absolute paths the session's own configuration lives in (the sandbox
	// template folder, the session file, the profile catalog, the root setting); the loader
	// sets them and every bwrap or seatbelt sandbox keeps them read-only, so a sandboxed call
	// cannot widen the environment of the next one by editing it.
	ReadOnlyPaths []string `json:",omitempty"`
	// SealedPaths (workspace layout only: G and W/.jad) are kept read-only as a whole by every
	// bwrap or seatbelt sandbox, missing ones too: unlike ReadOnlyPaths, no writable path is
	// bound again inside them, so neither a writable_paths entry nor a new file can change
	// the configuration the next run loads. SealedInner are the engine's own folders inside
	// them a covered tool still writes (W/.jad/exec, W/.jad/functions); the loader sets both.
	SealedPaths []string `json:",omitempty"`
	SealedInner []string `json:",omitempty"`
	// ProfileHidden (workspace layout only: W/.jad/profile-own and W/.jad/data/profiles) are
	// hidden in every bwrap or seatbelt sandbox, but for the ProfileShown folders inside them
	// (the running profile's own folder and data folder), which are shown read-only. ProfileData
	// is the running profile's data folder when this sandbox is that of its own top-level run:
	// the one shown folder that stays writable where a writable path covers it. Plan mode
	// (ForPlanMode), sub-agents (native callSandbox), NarrowSandbox and InheritSandbox clear it;
	// the loader sets all three (docs/design_workspace_layout.md §12).
	ProfileHidden []string `json:",omitempty"`
	ProfileShown  []string `json:",omitempty"`
	ProfileData   string   `json:",omitempty"`
	// Isolation is how a delegated run that uses this sandbox works (sandbox.isolation).
	Isolation SandboxIsolation `json:",omitzero"`
}

// SandboxIsolation is sandbox.isolation, read even when the sandbox is not enabled, since a
// worktree without confinement is the agent key isolation.
type SandboxIsolation struct {
	Mode string `json:",omitempty"`
	// Base is what a run's worktree starts from: SandboxIsolationBaseHead (also when empty)
	// or SandboxIsolationBaseWorking (docs/isolation-decisions.md D12).
	Base string `json:",omitempty"`
	// Materialize is how the worktree gets its files: SandboxIsolationMaterializeAuto (also
	// when empty), SandboxIsolationMaterializeCheckout or SandboxIsolationMaterializeOverlay.
	Materialize string `json:",omitempty"`
	// SharePaths are folders, relative to the session's working directory, bound read-only
	// from the session into an overlay run (ignored folders such as node_modules are in no
	// commit); empty by default (D6). Never together with materialize checkout (refused at
	// load); with auto a host without overlay refuses the run (native.openIsolatedTree).
	SharePaths []string `json:",omitempty"`
}

// WholeFileSystemReadable reports whether ReadPaths cover the whole file system (the default),
// so nothing is left out of a child's view. Nil ReadPaths (a sandbox built in code, not read
// from a setting) is the whole file system too; the loader always sets a list, which is
// empty when read.paths is [] and nothing is writable.
func (s *SandboxSettings) WholeFileSystemReadable() bool {
	return s == nil || s.ReadPaths == nil || containsString(s.ReadPaths, string(filepath.Separator))
}

// Covers reports whether a tool's child processes start in the sandbox.
func (s *SandboxSettings) Covers(tool string) bool {
	return s != nil && containsString(s.Tools, tool)
}

// normalizeSandbox reads sandbox; absent, null or enabled false is nil (no sandbox), which is
// the default. An enabled sandbox whose program cannot be found is refused, so a session never
// runs commands outside a sandbox it asked for.
func normalizeSandbox(raw json.RawMessage, path string) (*SandboxSettings, error) {
	settings, enabled, err := parseSandbox(raw, path, "sandbox")
	if err != nil || !enabled {
		return nil, err
	}
	return settings, nil
}

// normalizeSessionSandbox is normalizeSandbox with the isolation of the sandbox, which is
// read whether or not it is enabled, and the sandbox object as written (nil when absent),
// whose backend, executable and plan-mode paths shape PlanFallbackSandbox even when it is off.
func normalizeSessionSandbox(raw json.RawMessage, path string) (*SandboxSettings, SandboxIsolation, *SandboxSettings, error) {
	settings, enabled, err := parseSandbox(raw, path, "sandbox")
	if err != nil || settings == nil {
		return nil, SandboxIsolation{}, nil, err
	}
	if !enabled {
		return nil, settings.Isolation, settings, nil
	}
	return settings, settings.Isolation, settings, nil
}

// ParseSandboxSetting reads one inline sandbox object as the loader does (no template names):
// nil when it is absent or not enabled. path names it in errors.
func ParseSandboxSetting(raw json.RawMessage, path string) (*SandboxSettings, error) {
	return normalizeSandbox(raw, path)
}

// parseSandbox reads a sandbox object (a template, if one was named, already expanded into
// it) whether or not it is enabled, so the isolation of a disabled one is still known. nil
// means absent or null. Each add_ key is folded into the list it adds to, then every write
// path is added to the read paths (write implies read). An enabled sandbox whose program
// cannot be found is refused.
func parseSandbox(raw json.RawMessage, path, section string) (*SandboxSettings, bool, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, false, nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return nil, false, configError("%s: %s %q names a sandbox template, which needs the catalog layout's sandbox folder", path, section, single)
	}
	fields, err := decodeConfigObject(raw, path+": "+section)
	if err != nil {
		return nil, false, configError("%s: %s must be an object or the name of a sandbox template", path, section)
	}
	for key := range fields {
		if !containsString(sandboxKeys, key) {
			return nil, false, unsupportedToolKey(path+": "+section, key, sandboxKeys, "")
		}
	}
	enabled, err := bashBool(fields, "enabled", false, path+": "+section)
	if err != nil {
		return nil, false, err
	}
	settings, err := buildSandbox(fields, path, section)
	if err != nil {
		return nil, false, err
	}
	if enabled {
		if err := checkSandboxProgram(settings, path, section); err != nil {
			return nil, false, err
		}
	}
	return settings, enabled, nil
}

func buildSandbox(fields map[string]json.RawMessage, path, section string) (*SandboxSettings, error) {
	var err error
	settings := &SandboxSettings{Tools: append([]string(nil), defaultSandboxTools...), Backend: defaultSandboxBackend, Executable: defaultSandboxExecutable}
	if raw, exists := fields["template"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.Template); err != nil || strings.TrimSpace(settings.Template) == "" {
			return nil, configError("%s: %s.template must be the name of a sandbox template", path, section)
		}
	}
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
	if err := readSandboxPathKeys(settings, fields, path, section); err != nil {
		return nil, err
	}
	if settings.Isolation, err = sandboxIsolation(fields, path, section); err != nil {
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
			if strings.Contains(part, SandboxSystemPlaceholder) {
				return nil, configError("%s: %s.command[%d]: {system} is a read path of its own, not part of a command", path, section, index)
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
	return settings, nil
}

// checkSandboxProgram refuses an enabled sandbox that cannot run on this system.
func checkSandboxProgram(settings *SandboxSettings, path, section string) error {
	if settings.Backend == SandboxBackendBwrap && runtime.GOOS != "linux" {
		return configError("%s: %s.backend %q runs on Linux only; use backend %q with a sandbox program of this system", path, section, SandboxBackendBwrap, SandboxBackendCommand)
	}
	if settings.Backend == SandboxBackendSeatbelt && runtime.GOOS != "darwin" {
		return configError("%s: %s.backend %q runs on macOS only", path, section, SandboxBackendSeatbelt)
	}
	if !sandboxPlaceholderPattern.MatchString(settings.Executable) {
		if _, err := exec.LookPath(settings.Executable); err != nil {
			return configError("%s: %s is enabled but its program %q cannot be run: %v", path, section, settings.Executable, err)
		}
	}
	return nil
}

// readSandboxPathKeys reads read, write (or the older writable_paths and plan_writable_paths,
// D11) and hide_paths with their add_ keys, folds each add_ list into its key (a path listed
// twice counts once) and then makes every write path readable.
func readSandboxPathKeys(settings *SandboxSettings, fields map[string]json.RawMessage, path, section string) error {
	read, err := sandboxSubObject(fields, "read", sandboxReadKeys, path, section)
	if err != nil {
		return err
	}
	write, err := sandboxSubObject(fields, "write", sandboxWriteKeys, path, section)
	if err != nil {
		return err
	}
	for old, key := range map[string]string{"writable_paths": "paths", "plan_writable_paths": "plan_paths"} {
		if _, has := fields[old]; !has {
			continue
		}
		if _, both := write[key]; both {
			return configError("%s: %s.%s is the older spelling of %s.write.%s; set one of them", path, section, old, section, key)
		}
	}
	type list struct {
		into       *[]string
		object     map[string]json.RawMessage
		key, where string
		fallback   []string
		system     bool
	}
	lists := []list{
		{&settings.ReadPaths, read, "paths", section + ".read", defaultSandboxReadPaths, true},
		{&settings.WritablePaths, write, "paths", section + ".write", defaultSandboxWritablePaths, false},
		{&settings.PlanWritablePaths, write, "plan_paths", section + ".write", defaultSandboxPlanWritablePaths, false},
		{&settings.HidePaths, fields, "hide_paths", section, defaultSandboxHidePaths, false},
	}
	if _, has := fields["writable_paths"]; has {
		lists[1].object, lists[1].key, lists[1].where = fields, "writable_paths", section
	}
	if _, has := fields["plan_writable_paths"]; has {
		lists[2].object, lists[2].key, lists[2].where = fields, "plan_writable_paths", section
	}
	addKeys := map[string]string{"writable_paths": "add_paths", "plan_writable_paths": "add_plan_paths"}
	for _, item := range lists {
		values, err := sandboxPaths(item.object, item.key, item.fallback, path, item.where, item.system)
		if err != nil {
			return err
		}
		addObject, addKey, addWhere := item.object, sandboxAddKeys[item.key], item.where
		if key, older := addKeys[item.key]; older {
			addObject, addKey, addWhere = write, key, section+".write"
		}
		added, err := sandboxPaths(addObject, addKey, nil, path, addWhere, item.system)
		if err != nil {
			return err
		}
		*item.into = appendNewStrings(values, added...)
	}
	if !settings.WholeFileSystemReadable() {
		settings.ReadPaths = appendNewStrings(settings.ReadPaths, settings.WritablePaths...)
		settings.ReadPaths = appendNewStrings(settings.ReadPaths, settings.PlanWritablePaths...)
	}
	return nil
}

// sandboxSubObject reads sandbox.read or sandbox.write: absent or null is empty.
func sandboxSubObject(fields map[string]json.RawMessage, name string, keys []string, path, section string) (map[string]json.RawMessage, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	object, err := decodeConfigObject(raw, path+": "+section+"."+name)
	if err != nil {
		return nil, configError("%s: %s.%s must be an object with %s", path, section, name, strings.Join(keys, ", "))
	}
	for key := range object {
		if !containsString(keys, key) {
			return nil, unsupportedToolKey(path+": "+section+"."+name, key, keys, "")
		}
	}
	return object, nil
}

// sandboxIsolation reads sandbox.isolation; absent or null is no mode.
func sandboxIsolation(fields map[string]json.RawMessage, path, section string) (SandboxIsolation, error) {
	object, err := sandboxSubObject(fields, "isolation", sandboxIsolationKeys, path, section)
	if err != nil {
		return SandboxIsolation{}, err
	}
	var isolation SandboxIsolation
	if raw, exists := object["mode"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &isolation.Mode); err != nil || (isolation.Mode != SandboxIsolationNone && isolation.Mode != SandboxIsolationWorktree) {
			return SandboxIsolation{}, configError("%s: %s.isolation.mode must be %q or %q", path, section, SandboxIsolationNone, SandboxIsolationWorktree)
		}
	}
	if raw, exists := object["base"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &isolation.Base); err != nil || (isolation.Base != SandboxIsolationBaseHead && isolation.Base != SandboxIsolationBaseWorking) {
			return SandboxIsolation{}, configError("%s: %s.isolation.base must be %q or %q", path, section, SandboxIsolationBaseHead, SandboxIsolationBaseWorking)
		}
	}
	if raw, exists := object["materialize"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &isolation.Materialize); err != nil || (isolation.Materialize != SandboxIsolationMaterializeAuto && isolation.Materialize != SandboxIsolationMaterializeCheckout && isolation.Materialize != SandboxIsolationMaterializeOverlay) {
			return SandboxIsolation{}, configError("%s: %s.isolation.materialize must be %q, %q or %q", path, section, SandboxIsolationMaterializeAuto, SandboxIsolationMaterializeCheckout, SandboxIsolationMaterializeOverlay)
		}
	}
	if raw, exists := object["share_paths"]; exists && strings.TrimSpace(string(raw)) != "null" {
		var shares []string
		if err := json.Unmarshal(raw, &shares); err != nil {
			return SandboxIsolation{}, configError("%s: %s.isolation.share_paths must be an array of folders relative to the session's working directory", path, section)
		}
		for _, share := range shares {
			if !validSharePath(share) {
				return SandboxIsolation{}, configError("%s: %s.isolation.share_paths entry %q must be a folder relative to the session's working directory, without .. or placeholders", path, section, share)
			}
			isolation.SharePaths = appendNewStrings(isolation.SharePaths, filepath.ToSlash(filepath.Clean(share)))
		}
	}
	if isolation.Materialize == SandboxIsolationMaterializeCheckout && len(isolation.SharePaths) > 0 {
		// share_paths are binds of the overlay materialiser; a checkout has no way to give them
		// (and a link into the session would let a run write through), so the pair is refused
		// rather than the folders dropped.
		return SandboxIsolation{}, configError("%s: %s.isolation.share_paths needs the overlay materialiser: it cannot be set with materialize %q (use %q or %q)", path, section, SandboxIsolationMaterializeCheckout, SandboxIsolationMaterializeAuto, SandboxIsolationMaterializeOverlay)
	}
	return isolation, nil
}

// validSharePath reports whether share names a folder inside the session's working directory:
// slash-separated names, none of them empty, . or .., and no placeholder (the schema's
// pattern says the same).
func validSharePath(share string) bool {
	if share == "" || strings.ContainsAny(share, "{}\\") {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(share, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// appendNewStrings appends each value not already in list.
func appendNewStrings(list []string, values ...string) []string {
	for _, value := range values {
		if !containsString(list, value) {
			list = append(list, value)
		}
	}
	return list
}

// sandboxPaths reads one list of paths; system says {system} may be an entry (read paths).
func sandboxPaths(fields map[string]json.RawMessage, name string, fallback []string, path, section string, system bool) ([]string, error) {
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
		if strings.Contains(value, SandboxSystemPlaceholder) && (!system || value != SandboxSystemPlaceholder) {
			return nil, configError("%s: %s.%s[%d]: {system} is a read path of its own (read.paths or read.add_paths), not part of a path", path, section, name, index)
		}
		if !strings.HasPrefix(value, "{") && !filepath.IsAbs(value) {
			return nil, configError("%s: %s.%s[%d] must be an absolute path or start with a placeholder (%s)", path, section, name, index, sandboxPlaceholderList)
		}
		if !containsString(paths, value) {
			paths = append(paths, value)
		}
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
	plan.ProfileData = ""
	return &plan
}

// EnforcesPlanMode reports whether the sandbox's plan-mode form keeps a call from writing the
// project: bwrap and seatbelt keep only the plan-mode writable paths writable. A command
// backend is the person's own program, which jad cannot narrow; no sandbox enforces nothing.
func (s *SandboxSettings) EnforcesPlanMode() bool {
	return s != nil && (s.Backend == SandboxBackendBwrap || s.Backend == SandboxBackendSeatbelt)
}

// planFallbackBackend is the backend PlanFallbackSandbox uses on this system: bwrap on Linux,
// seatbelt on macOS, none elsewhere.
func planFallbackBackend() string {
	switch runtime.GOOS {
	case "linux":
		return SandboxBackendBwrap
	case "darwin":
		return SandboxBackendSeatbelt
	}
	return ""
}

// PlanFallbackSandbox is the sandbox a read_only exec call, and every exec call of a plan-mode
// run, starts in when no sandbox covers exec (docs/isolation-decisions.md D2): it only keeps
// the project from being written. Every path stays readable, nothing is hidden and the network
// stays open, as for a call outside any sandbox; only the plan-mode writable paths (by default
// {tmp}) are writable. configured is the session's sandbox object, enabled or not: its
// executable, plan-mode writable paths and check timeout are used when it names this system's
// backend. Nil where this system has neither bwrap nor seatbelt. Whether the program starts on
// this host is for the tool to probe.
func PlanFallbackSandbox(configured *SandboxSettings) *SandboxSettings {
	backend := planFallbackBackend()
	if backend == "" {
		return nil
	}
	fallback := &SandboxSettings{
		Tools: []string{"exec"}, Backend: backend, Executable: defaultSandboxExecutable, Network: true,
		ProxyPort: defaultSandboxProxyPort, ReadPaths: append([]string(nil), defaultSandboxReadPaths...),
		PlanWritablePaths: append([]string(nil), defaultSandboxPlanWritablePaths...), HidePaths: []string{},
		CheckTimeoutMs: defaultSandboxCheckTimeoutMs,
	}
	if backend == SandboxBackendSeatbelt {
		fallback.Executable = defaultSeatbeltExecutable
	}
	if configured != nil && configured.Backend == backend {
		fallback.Executable = configured.Executable
		fallback.PlanWritablePaths = append([]string(nil), configured.PlanWritablePaths...)
		fallback.CheckTimeoutMs = configured.CheckTimeoutMs
	}
	// Outside plan mode it is never used; its writable paths are the plan-mode ones all the same.
	fallback.WritablePaths = append([]string(nil), fallback.PlanWritablePaths...)
	return fallback
}

// applySandbox hands an enabled sandbox to each enabled tool it covers, and to the hooks
// when it covers them.
func applySandbox(sandbox, configured *SandboxSettings, execSettings *NativeExecSettings, toolSmith *NativeToolSmithSettings, hooks *HooksSettings, lsp *NativeLSPSettings) {
	if execSettings != nil && execSettings.Child.Sandbox == nil && !sandbox.Covers("exec") {
		// No sandbox covers exec: its read_only and plan-mode calls get one that only keeps
		// the project read-only, where it works on the host.
		execSettings.Child.PlanSandbox = PlanFallbackSandbox(configured)
	}
	if sandbox == nil {
		return
	}
	if lsp != nil && sandbox.Covers("lsp") {
		lsp.Sandbox = sandbox
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
