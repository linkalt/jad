package native

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// sandboxPlace is where a sandboxed child runs: the call's workspace root and working
// directory, which the {workspace} and {cwd} placeholders name, the call's own temp folder
// (callTempDir), which {tmp} names and the child gets as TMPDIR, and the session's state
// folder, which {session_dir} names. An empty tmp (a process that belongs to no call: an MCP
// server, jad check) is the system temp folder; an empty sessionDir drops {session_dir} paths.
type sandboxPlace struct {
	workspace  string
	cwd        string
	tmp        string
	sessionDir string
}

// sandboxLaunch is the program that starts a child, possibly inside a sandbox: the name it is
// configured as (argv[0]), the file that runs, and its arguments.
type sandboxLaunch struct {
	configured string
	resolved   string
	args       []string
	// dir is the folder the process starts in on the host: the call's cwd, or the root for a
	// process that joins an overlay run's holder, whose cwd may exist only in there (the
	// holder sets it inside).
	dir string
}

// callSandbox is the sandbox the children tool starts for one call start in: the run's own
// (ExecutionContext.Environment) when the call has one, if it covers tool, else the tool's
// configured one; its plan-mode form for a call of a plan-mode run.
func callSandbox(tool string, configured *sessionfs.SandboxSettings, execution tools.ExecutionContext) *sessionfs.SandboxSettings {
	sandbox := configured
	if environment := execution.Environment; environment != nil {
		sandbox = nil
		if environment.Sandbox.Covers(tool) {
			sandbox = environment.Sandbox
		}
	}
	if execution.PlanMode {
		return sandbox.ForPlanMode()
	}
	return sandbox
}

// sandboxed returns how to start program (configured as configured, resolved to resolved)
// with args: unchanged without a sandbox, otherwise the sandbox program with the child after
// it. The child keeps its configured name as argv[0] under bwrap; a command backend sees the
// resolved path.
func sandboxed(sandbox *sessionfs.SandboxSettings, place sandboxPlace, configured, resolved string, args []string) (sandboxLaunch, error) {
	launch, err := sandboxedLaunch(sandbox, place, configured, resolved, args)
	if err != nil {
		return sandboxLaunch{}, err
	}
	launch.dir = place.cwd
	if holder := holderFor(place.workspace); holder != nil {
		// An overlay run: every process of the run, sandboxed or not, starts in its holder.
		return joinHolder(holder, place.cwd, launch)
	}
	return launch, nil
}

// sandboxedLaunch is sandboxed without an overlay run's holder.
func sandboxedLaunch(sandbox *sessionfs.SandboxSettings, place sandboxPlace, configured, resolved string, args []string) (sandboxLaunch, error) {
	if sandbox == nil {
		return sandboxLaunch{configured: configured, resolved: resolved, args: args}, nil
	}
	expand := sandboxExpander(place)
	if sandbox.Backend != sessionfs.SandboxBackendCommand {
		// Made before the first sandbox starts, so every sandbox hides it (S3).
		if _, err := ensureSandboxTempRoot(); err != nil {
			return sandboxLaunch{}, err
		}
	}
	switch sandbox.Backend {
	case sessionfs.SandboxBackendCommand:
		prefix := make([]string, len(sandbox.Command))
		for index, part := range sandbox.Command {
			prefix[index] = expand(part)
		}
		program, err := exec.LookPath(prefix[0])
		if err != nil {
			return sandboxLaunch{}, err
		}
		arguments := append(append(prefix[1:len(prefix):len(prefix)], resolved), args...)
		return sandboxLaunch{configured: prefix[0], resolved: program, args: arguments}, nil
	case sessionfs.SandboxBackendSeatbelt:
		program, err := exec.LookPath(sandbox.Executable)
		if err != nil {
			return sandboxLaunch{}, err
		}
		arguments := []string{"-p", seatbeltProfile(sandbox, place, expand), "--"}
		var variables []string
		if place.tmp != "" {
			variables = append(variables, "TMPDIR="+place.tmp)
		}
		if variables = append(variables, sandboxRunEnvironment(place)...); len(variables) > 0 {
			// sandbox-exec cannot set a variable itself.
			arguments = append(append(arguments, "/usr/bin/env"), variables...)
		}
		arguments = append(append(arguments, resolved), args...)
		return sandboxLaunch{configured: sandbox.Executable, resolved: program, args: arguments}, nil
	default:
		program, err := exec.LookPath(sandbox.Executable)
		if err != nil {
			return sandboxLaunch{}, err
		}
		var network *sandboxNetwork
		if len(sandbox.NetworkAllow) > 0 {
			socket, err := sandboxNetworkProxy(sandbox.NetworkAllow)
			if err != nil {
				return sandboxLaunch{}, err
			}
			forwarder, err := os.Executable()
			if err != nil {
				return sandboxLaunch{}, fmt.Errorf("find jad for the sandbox network forwarder: %w", err)
			}
			network = &sandboxNetwork{socket: socket, forwarder: forwarder}
		}
		return sandboxLaunch{configured: sandbox.Executable, resolved: program, args: bwrapArguments(sandbox, place, expand, network, configured, resolved, args)}, nil
	}
}

// sandboxNetwork is network_allow's proxy socket and the jad binary that forwards to it.
type sandboxNetwork struct {
	socket    string
	forwarder string
}

// bwrapArguments builds a bubblewrap call (docs/isolation-sandbox-design.md §9.3): the read
// paths read-only (the whole file system by default, else only the listed ones on an empty
// root), a fresh /dev and /proc, the writable paths bound read-write (sandboxWritableRoots),
// the fixed rules (sandboxFixedRules), the hidden paths covered, every namespace new (the
// network kept only with network true), and the child killed if bwrap dies. A read, writable
// or hidden path that does not exist is left out. A later argument wins over an earlier one
// for the same path.
func bwrapArguments(sandbox *sessionfs.SandboxSettings, place sandboxPlace, expand func(string) string, network *sandboxNetwork, configured, resolved string, args []string) []string {
	arguments := []string{"--die-with-parent", "--unshare-all"}
	if sandbox.Network {
		arguments = append(arguments, "--share-net")
	}
	writes := sandboxWritePaths(sandbox.WritablePaths, place)
	writable := sandboxWritableRoots(writes, expand)
	whole := sandbox.WholeFileSystemReadable()
	var readable []sandboxReadRoot
	if whole {
		arguments = append(arguments, "--ro-bind", "/", "/")
	} else {
		readable = sandboxReadRoots(sandbox.ReadPaths, expand, writable)
		for _, root := range readable {
			arguments = append(arguments, "--ro-bind", root.real, root.real)
		}
		for _, root := range readable {
			if root.link != "" {
				arguments = append(arguments, "--symlink", root.real, root.link)
			}
		}
	}
	arguments = append(arguments, "--dev", "/dev", "--proc", "/proc")
	for _, path := range writable {
		arguments = append(arguments, "--bind-try", path, path)
	}
	arguments = append(arguments, bwrapRuleArguments(sandboxFixedRules(place, writable, sandbox.ReadOnlyPaths, sandboxWritableCandidates(writes, expand)))...)
	for _, path := range sandbox.HidePaths {
		path = expand(path)
		info, err := statInRun(path)
		switch {
		case path == "" || err != nil:
		case !whole && !sandboxVisible(path, readable, writable):
			// Not in the environment at all; a cover would only make an empty folder.
		case info.IsDir():
			arguments = append(arguments, "--tmpfs", path)
		default:
			arguments = append(arguments, "--ro-bind", os.DevNull, path)
		}
	}
	if place.tmp != "" {
		arguments = append(arguments, "--setenv", "TMPDIR", place.tmp)
	}
	for _, variable := range sandboxRunEnvironment(place) {
		name, value, _ := strings.Cut(variable, "=")
		arguments = append(arguments, "--setenv", name, value)
	}
	if place.cwd != "" {
		arguments = append(arguments, "--chdir", place.cwd)
	}
	if network != nil {
		// No network of its own: the forwarder hands loopback proxy_port to jad's proxy,
		// whose socket is bound in, and starts the child with the proxy variables.
		if !whole {
			// jad's own forwarder runs inside, whatever the read paths list.
			arguments = append(arguments, "--ro-bind", network.forwarder, network.forwarder)
		}
		arguments = append(arguments, "--bind", network.socket, network.socket)
		arguments = append(arguments, sandboxProxyEnvironment(sandbox.ProxyPort)...)
		arguments = append(arguments, "--", network.forwarder, SandboxNetForwardCommand,
			"--socket", network.socket, "--port", strconv.FormatInt(sandbox.ProxyPort, 10), "--argv0", configured, "--", resolved)
		return append(arguments, args...)
	}
	arguments = append(arguments, "--argv0", configured, "--", resolved)
	return append(arguments, args...)
}

// sandboxExpander fills in {workspace}, {cwd}, {git_common_dir}, {tmp}, {home} and
// {session_dir}. A value whose placeholder names nothing (no workspace, no repository, no
// home, no session) becomes empty, so a path that names it is left out. {tmp} is the call's
// own temp folder, or the system one for a process that belongs to no call. {system} stands
// for several folders and is expanded by sandboxReadRoots, not here.
func sandboxExpander(place sandboxPlace) func(string) string {
	home, _ := os.UserHomeDir()
	tmp := place.tmp
	if tmp == "" {
		tmp = os.TempDir()
	}
	values := map[string]string{"{workspace}": place.workspace, "{cwd}": place.cwd, "{tmp}": tmp, "{home}": home, "{session_dir}": place.sessionDir}
	gitDirKnown := false
	return func(value string) string {
		if strings.Contains(value, "{git_common_dir}") && !gitDirKnown {
			values["{git_common_dir}"], gitDirKnown = gitCommonDir(place.workspace), true
		}
		pairs := make([]string, 0, 2*len(values))
		for name, filled := range values {
			if strings.Contains(value, name) && filled == "" {
				return ""
			}
			pairs = append(pairs, name, filled)
		}
		return strings.NewReplacer(pairs...).Replace(value)
	}
}

// gitCommonDir is the git directory of the repository that holds dir: the one jad recorded
// for an isolated run's worktree, else found from dir (sessionfs.GitCommonDir).
func gitCommonDir(dir string) string {
	if run, ok := isolatedRunOf(dir); ok {
		return run.common
	}
	return sessionfs.GitCommonDir(dir)
}

// sandboxKey is the part of a standby's identity the sandbox makes: two launches in different
// sandboxes (or workspaces) are different processes.
func sandboxKey(sandbox *sessionfs.SandboxSettings, place sandboxPlace) string {
	holder := holderOf(place.workspace)
	if sandbox == nil {
		if holder != 0 {
			// Without a sandbox an overlay run's process still lives in its holder.
			return "holder\x00" + strconv.Itoa(holder)
		}
		return ""
	}
	settings, _ := json.Marshal(sandbox)
	key := string(settings) + "\x00" + place.workspace + "\x00" + place.tmp + "\x00" + place.sessionDir
	if holder != 0 {
		key += "\x00holder\x00" + strconv.Itoa(holder)
	}
	return key
}

// sandboxDescription is the sentence added to a tool's description when its calls run in a
// sandbox, so the model knows what will fail and why instead of guessing. Empty without one.
func sandboxDescription(sandbox *sessionfs.SandboxSettings) string {
	if sandbox == nil {
		return ""
	}
	if sandbox.Backend == sessionfs.SandboxBackendCommand {
		return " Sandbox: every call runs under " + sandbox.Command[0] + ", which limits what it may read, write and reach; a refused file or network access is the sandbox, not a bug to work around."
	}
	kind := "a bubblewrap"
	if sandbox.Backend == sessionfs.SandboxBackendSeatbelt {
		kind = "a macOS sandbox-exec"
	}
	text := " Sandbox: every call runs in " + kind + " sandbox."
	if !sandbox.WholeFileSystemReadable() {
		text += " Readable: only " + sandboxPathList(sandbox.ReadPaths) + "; nothing else exists in it."
	}
	text += " Writable: " + sandboxPathList(sandbox.WritablePaths) +
		"; everything else is read-only, so a write elsewhere fails with \"Read-only file system\"." +
		" The repository's git hooks and config and jad's own settings, catalogs and transcripts stay read-only, and other runs' worktrees are hidden."
	if !slices.Equal(sandbox.PlanWritablePaths, sandbox.WritablePaths) {
		text += " In plan mode, writable: " + sandboxPathList(sandbox.PlanWritablePaths) + "."
	}
	if len(sandbox.HidePaths) > 0 {
		text += " Hidden (empty): " + sandboxPathList(sandbox.HidePaths) + "."
	}
	switch {
	case sandbox.Network:
		text += " Network: on."
	case len(sandbox.NetworkAllow) > 0:
		text += " Network: only " + strings.Join(sandbox.NetworkAllow, ", ") + " (and their subdomains), through the HTTP(S) proxy in HTTP_PROXY and HTTPS_PROXY; other hosts get 403 and programs that ignore the proxy reach nothing."
	default:
		text += " Network: off, so downloads, package installs and git fetch or push fail."
	}
	return text + " Do not try to get around the sandbox; say what access a task needs instead."
}

// sandboxPathList names configured paths for the model: placeholders that differ per call by
// what they stand for, the temp and home folders by their place on this machine.
func sandboxPathList(paths []string) string {
	if len(paths) == 0 {
		return "nothing"
	}
	names := strings.NewReplacer("{workspace}", "<workspace>", "{cwd}", "<working directory>", "{git_common_dir}", "<repository git directory>", "{tmp}", "$TMPDIR (this session's own temp folder)", "{home}", "~", "{session_dir}", "<session state folder>", "{system}", "<system folders: "+strings.Join(sessionfs.SandboxSystemPaths(), " ")+">")
	named := make([]string, len(paths))
	for index, path := range paths {
		named[index] = names.Replace(path)
	}
	return strings.Join(named, ", ")
}

// SandboxCommand returns how to start program (as found on the PATH) with args inside
// sandbox from cwd, for a caller outside this package (an MCP stdio server): the file to
// run, the name it runs as (argv[0]) and its arguments. Without a sandbox they are the
// program's own.
func SandboxCommand(sandbox *sessionfs.SandboxSettings, workspace, cwd, program string, args []string) (string, string, []string, error) {
	resolved, err := exec.LookPath(program)
	if err != nil {
		return "", "", nil, err
	}
	launch, err := sandboxed(sandbox, sandboxPlace{workspace: workspace, cwd: cwd}, program, resolved, args)
	if err != nil {
		return "", "", nil, err
	}
	return launch.resolved, launch.configured, launch.args, nil
}
