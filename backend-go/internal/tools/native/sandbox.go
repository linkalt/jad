package native

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/sessionfs"
)

// sandboxPlace is where a sandboxed child runs: the call's workspace root and working
// directory, which the {workspace} and {cwd} placeholders name.
type sandboxPlace struct {
	workspace string
	cwd       string
}

// sandboxLaunch is the program that starts a child, possibly inside a sandbox: the name it is
// configured as (argv[0]), the file that runs, and its arguments.
type sandboxLaunch struct {
	configured string
	resolved   string
	args       []string
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
	default:
		program, err := exec.LookPath(sandbox.Executable)
		if err != nil {
			return sandboxLaunch{}, err
		}
		return sandboxLaunch{configured: sandbox.Executable, resolved: program, args: bwrapArguments(sandbox, place, expand, configured, resolved, args)}, nil
	}
}

// bwrapArguments builds a bubblewrap call: the whole file system read-only, a fresh /dev and
// /proc, the writable paths bound read-write, the hidden paths covered, every namespace new
// (the network kept only with network true), and the child killed if bwrap dies. A writable
// or hidden path that does not exist is left out.
func bwrapArguments(sandbox *sessionfs.SandboxSettings, place sandboxPlace, expand func(string) string, configured, resolved string, args []string) []string {
	arguments := []string{"--die-with-parent", "--unshare-all"}
	if sandbox.Network {
		arguments = append(arguments, "--share-net")
	}
	arguments = append(arguments, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc")
	for _, path := range sandbox.WritablePaths {
		if path = expand(path); path != "" {
			arguments = append(arguments, "--bind-try", path, path)
		}
	}
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
	if place.cwd != "" {
		arguments = append(arguments, "--chdir", place.cwd)
	}
	arguments = append(arguments, "--argv0", configured, "--", resolved)
	return append(arguments, args...)
}

// sandboxExpander fills in {workspace}, {cwd}, {git_common_dir}, {tmp} and {home}. A value
// whose placeholder names nothing (no workspace, no repository, no home) becomes empty, so a
// path that names it is left out.
func sandboxExpander(place sandboxPlace) func(string) string {
	home, _ := os.UserHomeDir()
	values := map[string]string{"{workspace}": place.workspace, "{cwd}": place.cwd, "{tmp}": os.TempDir(), "{home}": home}
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
	return string(settings) + "\x00" + place.workspace
}
