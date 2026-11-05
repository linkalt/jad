package native

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// environmentWithoutServerToken is the process environment cut to the shell allowlist, for
// helper processes (language servers) that need no configured environment.
func environmentWithoutServerToken() []string {
	allowed := make(map[string]struct{}, len(bashEnvironmentAllowlist))
	for _, name := range bashEnvironmentAllowlist {
		allowed[name] = struct{}{}
	}
	environment := os.Environ()
	safe := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if _, ok := allowed[name]; ok {
			safe = append(safe, value)
		}
	}
	return safe
}

func exitCode(waitErr error) (int, bool) {
	if waitErr == nil {
		return 0, true
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return exitError.ExitCode(), true
	}
	return 0, false
}
