//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// newInterpreterCommand starts the resolved interpreter but keeps argv[0] as the
// name that was configured, like Node's spawn does. A script sees it as $0, so
// without this a call that resolved "bash" through PATH would print /usr/bin/bash
// on this backend and bash on the other.
func newInterpreterCommand(configured, resolved string, args ...string) *exec.Cmd {
	command := exec.Command(resolved, args...)
	command.Args[0] = configured
	return command
}

func resolveBashExecutable(file string, environment []string, cwd string) (string, error) {
	if filepath.IsAbs(file) || strings.ContainsRune(file, os.PathSeparator) {
		return file, nil
	}
	pathValue, hasPath := bashEnvironmentValue(environment, "PATH")
	if !hasPath {
		pathValue = "/usr/bin:/bin"
	}
	directories := filepath.SplitList(pathValue)
	if len(directories) == 0 {
		directories = []string{""}
	}
	var firstNonExecutable string
	for _, directory := range directories {
		if directory == "" {
			directory = "."
		}
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(cwd, directory)
		}
		candidate := filepath.Join(directory, file)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			if firstNonExecutable == "" {
				firstNonExecutable = candidate
			}
			continue
		}
		return candidate, nil
	}
	if firstNonExecutable != "" {
		return firstNonExecutable, nil
	}
	return "", exec.ErrNotFound
}

func bashEnvironmentValue(environment []string, name string) (string, bool) {
	for _, item := range environment {
		key, value, found := strings.Cut(item, "=")
		if found && key == name {
			return value, true
		}
	}
	return "", false
}
