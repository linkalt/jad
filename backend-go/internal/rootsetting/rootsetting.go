// Package rootsetting finds the root setting.json when no path is given. The session
// loader and the content library both look for it and must agree on where.
package rootsetting

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// fileName is the root setting's file name; Folder holds it in a project and in a home.
const (
	fileName = "setting.json"
	folder   = ".jad"
)

// Candidates lists where the root setting is looked for, best first: the project's
// .jad/setting.json, the user's $HOME/.jad/setting.json, then the older places (the
// working directory and its parent). An empty home leaves the home file out.
func Candidates(cwd, home string) []string {
	list := []string{filepath.Join(cwd, folder, fileName)}
	if home != "" {
		list = append(list, filepath.Join(home, folder, fileName))
	}
	return append(list, filepath.Join(cwd, fileName), filepath.Join(cwd, "..", fileName))
}

// Find returns the first candidate that is a regular file, or "" when there is none. A
// directory with that name is skipped; any other error is returned.
func Find(cwd, home string) (string, error) {
	for _, candidate := range Candidates(cwd, home) {
		info, err := os.Stat(candidate)
		if err == nil {
			if info.Mode().IsRegular() {
				return filepath.Clean(candidate), nil
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect root setting %q: %w", candidate, err)
		}
	}
	return "", nil
}
