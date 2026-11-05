package sessionfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// Where the upward search for project instruction files stops.
const (
	ProjectInstructionsStopGitRoot = "git_root"
	ProjectInstructionsStopCWD     = "cwd"
	ProjectInstructionsStopRoot    = "filesystem_root"
)

const (
	projectInstructionsSection                     = "project_instructions"
	defaultProjectInstructionsMaxBytes       int64 = 65_536
	defaultProjectInstructionsSubfolderBytes int64 = 16_384
	maxProjectInstructionsFileNames                = 16
)

var (
	projectInstructionsKeys         = []string{"enabled", "file_names", "stop_at", "max_bytes", "subfolders", "subfolder_max_bytes"}
	defaultProjectInstructionsNames = []string{"AGENTS.md", "CLAUDE.md"}
)

// ProjectInstructionsSettings are the normalized `project_instructions` settings: files
// such as AGENTS.md found by name instead of being listed in prompt_documents. Off by
// default.
type ProjectInstructionsSettings struct {
	Enabled bool
	// FileNames are the names looked for in each folder, most preferred first; a folder
	// gives at most one file, the first name it has.
	FileNames []string
	// StopAt is where the upward search from cwd stops: git_root (the nearest folder with
	// .git, or cwd alone when there is none), cwd, or filesystem_root.
	StopAt string
	// MaxBytes bounds the text all the files found upward add to the prompt.
	MaxBytes int64
	// Subfolders adds a subfolder's file to the result of the first read, write, edit,
	// apply_patch or notebook call of a run that touches a path inside that subfolder.
	Subfolders bool
	// SubfolderMaxBytes bounds each subfolder file.
	SubfolderMaxBytes int64
}

// SubfoldersActive reports whether subfolder files are added to tool results.
func (settings ProjectInstructionsSettings) SubfoldersActive() bool {
	return settings.Enabled && settings.Subfolders
}

// DefaultProjectInstructionsSettings is the setting when the key is absent: off.
func DefaultProjectInstructionsSettings() ProjectInstructionsSettings {
	return ProjectInstructionsSettings{
		FileNames:         append([]string(nil), defaultProjectInstructionsNames...),
		StopAt:            ProjectInstructionsStopGitRoot,
		MaxBytes:          defaultProjectInstructionsMaxBytes,
		SubfolderMaxBytes: defaultProjectInstructionsSubfolderBytes,
	}
}

func normalizeProjectInstructions(raw json.RawMessage, settingPath string) (ProjectInstructionsSettings, error) {
	settings := DefaultProjectInstructionsSettings()
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return settings, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": "+projectInstructionsSection)
	if err != nil {
		return ProjectInstructionsSettings{}, err
	}
	for key := range fields {
		if !slices.Contains(projectInstructionsKeys, key) {
			return ProjectInstructionsSettings{}, configError("%s: %s.%s is not supported", settingPath, projectInstructionsSection, key)
		}
	}
	prefix := settingPath + ": " + projectInstructionsSection
	if settings.Enabled, err = bashBool(fields, "enabled", false, prefix); err != nil {
		return ProjectInstructionsSettings{}, err
	}
	if settings.Subfolders, err = bashBool(fields, "subfolders", false, prefix); err != nil {
		return ProjectInstructionsSettings{}, err
	}
	if settings.MaxBytes, err = configuredInteger(fields, "max_bytes", defaultProjectInstructionsMaxBytes, 1, maxPromptDocumentBytes, settingPath, projectInstructionsSection); err != nil {
		return ProjectInstructionsSettings{}, err
	}
	if settings.SubfolderMaxBytes, err = configuredInteger(fields, "subfolder_max_bytes", defaultProjectInstructionsSubfolderBytes, 1, maxPromptDocumentBytes, settingPath, projectInstructionsSection); err != nil {
		return ProjectInstructionsSettings{}, err
	}
	if settings.StopAt, err = bashString(fields, "stop_at", ProjectInstructionsStopGitRoot, prefix, false); err != nil {
		return ProjectInstructionsSettings{}, err
	}
	switch settings.StopAt {
	case ProjectInstructionsStopGitRoot, ProjectInstructionsStopCWD, ProjectInstructionsStopRoot:
	default:
		return ProjectInstructionsSettings{}, configError("%s.stop_at must be git_root, cwd or filesystem_root", prefix)
	}
	if value, exists := fields["file_names"]; exists && strings.TrimSpace(string(value)) != "null" {
		var names []string
		if json.Unmarshal(value, &names) != nil || len(names) == 0 || len(names) > maxProjectInstructionsFileNames {
			return ProjectInstructionsSettings{}, configError("%s.file_names must be a list of 1 to %d file names", prefix, maxProjectInstructionsFileNames)
		}
		for _, name := range names {
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
				return ProjectInstructionsSettings{}, configError("%s.file_names entry %q must be one file name", prefix, name)
			}
		}
		settings.FileNames = slices.Compact(names)
	}
	return settings, nil
}

// ProjectInstructionFile is one instruction file found by name.
type ProjectInstructionFile struct {
	Path string
	Text string
	// Cut reports that Text is the start of the file, cut at a byte limit.
	Cut bool
}

// FindProjectInstructionFile returns the first of names that is a regular file in dir,
// read up to limit bytes. found is false when dir has none.
func FindProjectInstructionFile(dir string, names []string, limit int64) (ProjectInstructionFile, bool, error) {
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			return ProjectInstructionFile{}, false, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		text, cut, err := readInstructionText(path, limit)
		if err != nil {
			return ProjectInstructionFile{}, false, err
		}
		return ProjectInstructionFile{Path: path, Text: text, Cut: cut}, true, nil
	}
	return ProjectInstructionFile{}, false, nil
}

// readInstructionText reads at most limit bytes of path, cut back to a whole UTF-8
// character.
func readInstructionText(path string, limit int64) (string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", false, err
	}
	if int64(len(data)) <= limit {
		return string(data), false, nil
	}
	data = data[:limit]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data), true, nil
}

// projectInstructionDirs lists the folders searched upward from cwd, outermost first.
func projectInstructionDirs(cwd, stopAt string) []string {
	var dirs []string
	switch stopAt {
	case ProjectInstructionsStopCWD:
		return []string{cwd}
	case ProjectInstructionsStopGitRoot:
		found := false
		for dir := cwd; ; {
			dirs = append(dirs, dir)
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				found = true
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		if !found {
			return []string{cwd}
		}
	default:
		for dir := cwd; ; {
			dirs = append(dirs, dir)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	slices.Reverse(dirs)
	return dirs
}

// ProjectInstructionText formats one file for the model: a heading that names the file
// and the folder it covers, then the text.
func ProjectInstructionText(file ProjectInstructionFile, shownPath string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "# Project instructions: %s\n\n", shownPath)
	text.WriteString(strings.TrimRight(file.Text, "\n"))
	if file.Cut {
		fmt.Fprintf(&text, "\n\n[%s was cut at the project_instructions byte limit]", shownPath)
	}
	return text.String()
}

// discoverProjectInstructions reads the instruction files from the folders above and at
// cwd, skipping files already read as prompt documents. It returns the prompt text to add
// and the paths it read.
func discoverProjectInstructions(settings ProjectInstructionsSettings, cwd string, already []string, settingPath string) (string, []string, error) {
	if !settings.Enabled {
		return "", nil, nil
	}
	remaining := settings.MaxBytes
	var texts, paths []string
	for _, dir := range projectInstructionDirs(cwd, settings.StopAt) {
		if remaining <= 0 {
			break
		}
		file, found, err := FindProjectInstructionFile(dir, settings.FileNames, remaining)
		if err != nil {
			return "", nil, configError("%s: project_instructions could not read a file in %s: %v", settingPath, dir, err)
		}
		if !found || slices.Contains(already, file.Path) {
			continue
		}
		remaining -= int64(len(file.Text))
		texts = append(texts, ProjectInstructionText(file, file.Path))
		paths = append(paths, file.Path)
	}
	return strings.Join(texts, "\n\n"), paths, nil
}
