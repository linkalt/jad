package sessionfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// ErrReadmeMissing means the working directory has no readable README file.
var ErrReadmeMissing = errors.New("no README file in the working directory")

// ErrReadmeNotText means the README file is not UTF-8 text.
var ErrReadmeNotText = errors.New("the README file is not UTF-8 text")

// SessionReadme is the README file of a working directory, cut at the configured size.
type SessionReadme struct {
	Name      string `json:"name"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// ReadSessionReadme reads settings.ReadmeFile from the top of root. The file must be a
// regular file that stays inside root after symlinks are resolved and that the native read
// policy allows, the same rules as the file listing, so the route shows nothing the listing
// would hide.
func ReadSessionReadme(root string, settings SessionFileListingSettings, policy NativePathPolicy) (SessionReadme, error) {
	workspaceRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return SessionReadme{}, ErrReadmeMissing
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	path := filepath.Join(root, settings.ReadmeFile)
	resolved, regular := containedRegularFile(path, workspaceRoot)
	if !regular || !allowedByNativeReadPolicy(path, root, resolved, workspaceRoot, policy) {
		return SessionReadme{}, ErrReadmeMissing
	}
	file, err := os.Open(resolved)
	if err != nil {
		return SessionReadme{}, ErrReadmeMissing
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(settings.ReadmeMaxBytes)+1))
	if err != nil {
		return SessionReadme{}, err
	}
	readme := SessionReadme{Name: settings.ReadmeFile}
	if len(data) > settings.ReadmeMaxBytes {
		data = data[:settings.ReadmeMaxBytes]
		readme.Truncated = true
		// The cut may fall inside a character; drop the part of it that was read.
		for len(data) > 0 && !utf8.Valid(data) && !utf8.FullRune(data[lastRuneStart(data):]) {
			data = data[:lastRuneStart(data)]
		}
	}
	if !utf8.Valid(data) {
		return SessionReadme{}, ErrReadmeNotText
	}
	readme.Content = string(data)
	return readme, nil
}

// lastRuneStart is the index where the final, possibly incomplete, character begins.
func lastRuneStart(data []byte) int {
	start := len(data) - 1
	for start > 0 && len(data)-start < utf8.UTFMax && !utf8.RuneStart(data[start]) {
		start--
	}
	return start
}
