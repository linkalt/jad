//go:build windows

package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// newInterpreterCommand starts the resolved interpreter but keeps argv[0] as the
// name that was configured, like Node's spawn does.
func newInterpreterCommand(configured, resolved string, args ...string) *exec.Cmd {
	command := exec.Command(resolved, args...)
	command.Args[0] = configured
	return command
}

func resolveBashExecutable(file string, environment []string, cwd string) (string, error) {
	if filepath.IsAbs(file) || strings.ContainsAny(file, `/\\`) {
		return file, nil
	}
	pathValue, hasPath := bashEnvironmentValue(environment, "PATH")
	if !hasPath {
		if systemRoot, ok := bashEnvironmentValue(environment, "SYSTEMROOT"); ok {
			pathValue = filepath.Join(systemRoot, "System32")
		} else {
			pathValue = "."
		}
	}
	pathExtensions, hasExtensions := bashEnvironmentValue(environment, "PATHEXT")
	if !hasExtensions || pathExtensions == "" {
		pathExtensions = ".COM;.EXE;.BAT;.CMD"
	}
	extensions := filepath.SplitList(pathExtensions)
	if filepath.Ext(file) != "" {
		extensions = []string{""}
	}
	directories := filepath.SplitList(pathValue)
	for _, directory := range directories {
		if directory == "" {
			directory = "."
		}
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(cwd, directory)
		}
		for _, extension := range extensions {
			candidate := filepath.Join(directory, file+strings.TrimSpace(extension))
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

func bashEnvironmentValue(environment []string, name string) (string, bool) {
	for _, item := range environment {
		key, value, found := strings.Cut(item, "=")
		if found && strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}
