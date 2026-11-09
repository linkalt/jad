package native

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// sandboxPlace is where a sandboxed child runs: the call's workspace root and working
// directory, which the {workspace} and {cwd} placeholders name, and the call's own temp
// folder (callTempDir), which {tmp} names and the child gets as TMPDIR. An empty tmp (a
// process that belongs to no call: an MCP server, jad check) is the system temp folder.
type sandboxPlace struct {
	workspace string
	cwd       string
	tmp       string
}

// sandboxLaunch is the program that starts a child, possibly inside a sandbox: the name it is
// configured as (argv[0]), the file that runs, and its arguments.
type sandboxLaunch struct {
	configured string
	resolved   string
	args       []string
}

// callSandbox is the sandbox one call's children start in: its plan-mode form for a call of
// a plan-mode run, else the configured one.
func callSandbox(sandbox *sessionfs.SandboxSettings, execution tools.ExecutionContext) *sessionfs.SandboxSettings {
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
		if place.tmp != "" {
			// sandbox-exec cannot set a variable itself.
			arguments = append(arguments, "/usr/bin/env", "TMPDIR="+place.tmp)
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

// bwrapArguments builds a bubblewrap call: the whole file system read-only, a fresh /dev and
// /proc, the writable paths bound read-write (sandboxWritableRoots), the fixed rules
// (sandboxFixedRules), the hidden paths covered, every namespace new (the network kept only
// with network true), and the child killed if bwrap dies. A writable or hidden path that does
// not exist is left out. A later argument wins over an earlier one for the same path.
func bwrapArguments(sandbox *sessionfs.SandboxSettings, place sandboxPlace, expand func(string) string, network *sandboxNetwork, configured, resolved string, args []string) []string {
	arguments := []string{"--die-with-parent", "--unshare-all"}
	if sandbox.Network {
		arguments = append(arguments, "--share-net")
	}
	arguments = append(arguments, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc")
	writable := sandboxWritableRoots(sandbox.WritablePaths, expand)
	for _, path := range writable {
		arguments = append(arguments, "--bind-try", path, path)
	}
	arguments = append(arguments, bwrapRuleArguments(sandboxFixedRules(place, writable))...)
	for _, path := range sandbox.HidePaths {
		path = expand(path)
		info, err := os.Stat(path)
		switch {
		case path == "" || err != nil:
		case info.IsDir():
			arguments = append(arguments, "--tmpfs", path)
		default:
			arguments = append(arguments, "--ro-bind", os.DevNull, path)
		}
	}
	if place.tmp != "" {
		arguments = append(arguments, "--setenv", "TMPDIR", place.tmp)
	}
	if place.cwd != "" {
		arguments = append(arguments, "--chdir", place.cwd)
	}
	if network != nil {
		// No network of its own: the forwarder hands loopback proxy_port to jad's proxy,
		// whose socket is bound in, and starts the child with the proxy variables.
		arguments = append(arguments, "--bind", network.socket, network.socket)
		arguments = append(arguments, sandboxProxyEnvironment(sandbox.ProxyPort)...)
		arguments = append(arguments, "--", network.forwarder, SandboxNetForwardCommand,
			"--socket", network.socket, "--port", strconv.FormatInt(sandbox.ProxyPort, 10), "--argv0", configured, "--", resolved)
		return append(arguments, args...)
	}
	arguments = append(arguments, "--argv0", configured, "--", resolved)
	return append(arguments, args...)
}

// sandboxExpander fills in {workspace}, {cwd}, {git_common_dir}, {tmp} and {home}. A value
// whose placeholder names nothing (no workspace, no repository, no home) becomes empty, so a
// path that names it is left out. {tmp} is the call's own temp folder, or the system one for
// a process that belongs to no call.
func sandboxExpander(place sandboxPlace) func(string) string {
	home, _ := os.UserHomeDir()
	tmp := place.tmp
	if tmp == "" {
		tmp = os.TempDir()
	}
	values := map[string]string{"{workspace}": place.workspace, "{cwd}": place.cwd, "{tmp}": tmp, "{home}": home}
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

// gitCommonDir is the git directory of the repository that holds dir: the .git folder of a
// checkout, or for a worktree (whose .git is a file naming its own git folder) the folder its
// commondir names. Empty when dir is in no repository.
func gitCommonDir(dir string) string {
	if dir == "" {
		return ""
	}
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		dotGit := filepath.Join(current, ".git")
		if info, err := os.Stat(dotGit); err == nil {
			if info.IsDir() {
				return dotGit
			}
			return worktreeCommonDir(current, dotGit)
		}
		if parent := filepath.Dir(current); parent == current {
			return ""
		}
	}
}

func worktreeCommonDir(top, dotGit string) string {
	content, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	gitDir, found := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir:")
	if !found {
		return ""
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(top, gitDir)
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	return filepath.Clean(commonDir)
}

// sandboxKey is the part of a standby's identity the sandbox makes: two launches in different
// sandboxes (or workspaces) are different processes.
func sandboxKey(sandbox *sessionfs.SandboxSettings, place sandboxPlace) string {
	if sandbox == nil {
		return ""
	}
	settings, _ := json.Marshal(sandbox)
	return string(settings) + "\x00" + place.workspace + "\x00" + place.tmp
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
	text := " Sandbox: every call runs in " + kind + " sandbox. Writable: " + sandboxPathList(sandbox.WritablePaths) +
		"; everything else is read-only, so a write elsewhere fails with \"Read-only file system\"." +
		" The repository's git hooks and config stay read-only and other runs' worktrees are hidden."
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
	names := strings.NewReplacer("{workspace}", "<workspace>", "{cwd}", "<working directory>", "{git_common_dir}", "<repository git directory>", "{tmp}", "$TMPDIR (this session's own temp folder)", "{home}", "~")
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
