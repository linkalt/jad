package contentlibrary

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed library_defaults.json
var embeddedDefaults []byte

// Options identify the global setting.json used by the TypeScript-compatible
// root library. Values are captured at service construction; setting.json and
// the library files themselves are re-read for each request.
type Options struct {
	WorkingDir      string
	RootSettingPath string
	SessionsDir     string
	Environment     map[string]string
}

// ConfigError is a safe, user-correctable root-library configuration error.
type ConfigError struct {
	Message string
}

func (e *ConfigError) Error() string { return e.Message }

type libraryDirectories struct {
	prompts   []string
	templates []string
	skills    []string
}

type libraryDefaults struct {
	Prompts   []string `json:"prompt_dirs"`
	Templates []string `json:"template_dirs"`
	Skills    []string `json:"skill_dirs"`
}

func loadDirectories(options Options) (libraryDirectories, error) {
	workingDir := strings.TrimSpace(options.WorkingDir)
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return libraryDirectories{}, fmt.Errorf("get working directory: %w", err)
		}
	}
	workingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return libraryDirectories{}, fmt.Errorf("resolve working directory: %w", err)
	}

	environment := options.Environment
	if environment == nil {
		environment = processEnvironment()
	}
	settingPath, err := findLibrarySettingPath(workingDir, options, environment)
	if err != nil {
		return libraryDirectories{}, err
	}

	globalRoot := workingDir
	setting := map[string]json.RawMessage(nil)
	if settingPath != "" {
		globalRoot = filepath.Dir(settingPath)
		setting, err = readLibrarySetting(settingPath)
		if err != nil {
			return libraryDirectories{}, err
		}
	}

	var defaults libraryDefaults
	if err := json.Unmarshal(embeddedDefaults, &defaults); err != nil {
		return libraryDirectories{}, fmt.Errorf("decode embedded root-library defaults: %w", err)
	}
	var configured map[string]json.RawMessage
	if raw, exists := setting["libraries"]; exists {
		if err := json.Unmarshal(raw, &configured); err != nil || configured == nil {
			return libraryDirectories{}, configError("%s: libraries must be an object", settingPath)
		}
	}

	settingLabel := settingPath
	if settingLabel == "" {
		settingLabel = globalRoot
	}
	prompts, err := resolveLibraryDirectoryList(configured, "prompt_dirs", defaults.Prompts, settingLabel, globalRoot)
	if err != nil {
		return libraryDirectories{}, err
	}
	templates, err := resolveLibraryDirectoryList(configured, "template_dirs", defaults.Templates, settingLabel, globalRoot)
	if err != nil {
		return libraryDirectories{}, err
	}
	skills, err := resolveLibraryDirectoryList(configured, "skill_dirs", defaults.Skills, settingLabel, globalRoot)
	if err != nil {
		return libraryDirectories{}, err
	}
	return libraryDirectories{prompts: prompts, templates: templates, skills: skills}, nil
}

func findLibrarySettingPath(workingDir string, options Options, environment map[string]string) (string, error) {
	configured := strings.TrimSpace(options.RootSettingPath)
	if configured == "" {
		configured = strings.TrimSpace(environment["JAD_ROOT_SETTING_PATH"])
	}
	if configured != "" {
		return resolveLibraryPath(workingDir, configured), nil
	}
	if strings.TrimSpace(options.SessionsDir) != "" || strings.TrimSpace(environment["JAD_SESSIONS_DIR"]) != "" {
		return "", nil
	}
	for _, candidate := range []string{
		filepath.Join(workingDir, "setting.json"),
		filepath.Join(workingDir, "..", "setting.json"),
	} {
		info, err := os.Stat(candidate)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		if info.Mode().IsRegular() {
			return filepath.Clean(candidate), nil
		}
	}
	return "", nil
}

func readLibrarySetting(path string) (map[string]json.RawMessage, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, configError("%s: file is required", path)
		}
		return nil, err
	}
	if strings.TrimSpace(string(contents)) == "" {
		return nil, configError("%s: file is empty", path)
	}
	var setting map[string]json.RawMessage
	if err := json.Unmarshal(contents, &setting); err != nil {
		return nil, configError("%s: invalid JSON: %v", path, err)
	}
	if setting == nil {
		return nil, configError("%s: expected a JSON object", path)
	}
	return setting, nil
}

func resolveLibraryDirectoryList(
	configured map[string]json.RawMessage,
	key string,
	fallback []string,
	settingPath string,
	globalRoot string,
) ([]string, error) {
	values := fallback
	if raw, exists := configured[key]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			return nil, configError("%s: libraries.%s must be an array of directories", configLabel(settingPath), key)
		}
	}

	resolved := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, configError("%s: libraries.%s[%d] must be a non-empty string", configLabel(settingPath), key, index)
		}
		path := resolveLibraryPath(globalRoot, value)
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		resolved = append(resolved, path)
	}
	return resolved, nil
}

func resolveLibraryPath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
}

func configLabel(settingPath string) string {
	if settingPath == "" {
		return "root setting"
	}
	return settingPath
}

func configError(format string, args ...any) error {
	return &ConfigError{Message: fmt.Sprintf(format, args...)}
}

func processEnvironment() map[string]string {
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[key] = value
		}
	}
	return environment
}

func cloneEnvironment(source map[string]string) map[string]string {
	if source == nil {
		return processEnvironment()
	}
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func isSkippableFilesystemError(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOTDIR)
}
