package contentlibrary

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"jad/backend-go/internal/sessionfs"
)

//go:embed library_defaults.json
var embeddedDefaults []byte

const (
	// defaultMaxAssetBytes caps one prompt, template or skill file. A larger file is
	// skipped, so it is never read into memory.
	defaultMaxAssetBytes int64 = 1 << 20
	minMaxAssetBytes     int64 = 1024
	maxMaxAssetBytes     int64 = 8 << 20
)

// Options are the libraries of the workspaces a server serves: by workspace id, read once at
// start from global.json and workspace.json (no library setting is read per request, the files
// of the library themselves are). StartWorkspace is the one a request that names none gets.
type Options struct {
	Workspaces     map[string]*sessionfs.Libraries
	StartWorkspace string
}

// ConfigError is a safe, user-correctable root-library configuration error.
type ConfigError struct {
	Message string
}

func (e *ConfigError) Error() string { return e.Message }

type libraryDirectories struct {
	prompts       []string
	templates     []string
	skills        []string
	commands      []string
	maxAssetBytes int64
}

type libraryDefaults struct {
	Prompts   []string `json:"prompt_dirs"`
	Templates []string `json:"template_dirs"`
	Skills    []string `json:"skill_dirs"`
	Commands  []string `json:"command_dirs"`
}

func resolveLibraryPath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
}

func configError(format string, args ...any) error {
	return &ConfigError{Message: fmt.Sprintf(format, args...)}
}

func isSkippableFilesystemError(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOTDIR)
}
