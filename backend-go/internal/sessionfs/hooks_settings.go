package sessionfs

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

const (
	// HookCwdFileDir runs a check in the directory of the changed file; HookCwdWorkspace
	// runs it in the workspace root; HookCwdNearestPrefix + a marker file name runs it in
	// the closest directory, from the file upward to the workspace root, that holds the marker.
	HookCwdFileDir       = "file_dir"
	HookCwdWorkspace     = "workspace"
	HookCwdNearestPrefix = "nearest:"

	defaultHooksTimeoutMS      int64 = 60_000
	minHooksTimeoutMS          int64 = 1_000
	maxHooksTimeoutMS          int64 = 600_000
	defaultHooksMaxOutputBytes int64 = 8 * 1024
	minHooksMaxOutputBytes     int64 = 256
	maxHooksMaxOutputBytes     int64 = 1024 * 1024
	maxHooksPostEdit                 = 32
	maxHookArgv                      = 32
	maxHookExtensions                = 32
)

var (
	hooksKeys          = []string{"enabled", "env", "env_deny", "env_file", "kill_grace_ms", "kill_process_group", "max_line_chars", "max_output_bytes", "post_edit", "strip_ansi", "timeout_ms"}
	postEditHookKeys   = []string{"argv", "cwd", "ext", "name", "on_failure_only"}
	hookNamePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_+.-]{0,31}$`)
	hookExtPattern     = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)
	hookMarkerPattern  = regexp.MustCompile(`^[A-Za-z0-9._+-][A-Za-z0-9._ +-]{0,63}$`)
	hookPlaceholderRef = regexp.MustCompile(`\{[a-z_]*\}`)
	hookPlaceholders   = []string{"{file}", "{dir}", "{rel}", "{reldir}"}
)

// HooksSettings controls the optional checks that run after the native write and edit
// tools change a file. Everything is off by default: with Enabled false (or the key
// absent) no command is ever started and no tool result changes.
type HooksSettings struct {
	Enabled bool
	// TimeoutMS is the longest one check may run before it is stopped and reported.
	TimeoutMS int64
	// MaxOutputBytes cuts the text a check adds to the tool result.
	MaxOutputBytes int64
	// PostEdit lists the checks; every entry whose extension matches the changed file runs, in order.
	PostEdit []PostEditHook
	// The child-process settings of a check. They are the hooks' own: nothing is read from
	// tools.native.bash. Env and EnvDeny shape the environment on top of the shared allowlist.
	KillGraceMs      int64
	KillProcessGroup string
	MaxLineChars     int64
	StripANSI        bool
	EnvFile          string
	Env              map[string]string
	EnvDeny          []string
}

// PostEditHook is one check: an argv (no shell) run after a file with one of Ext changed.
type PostEditHook struct {
	Name string
	// Ext lists the file extensions (with the dot) the check applies to, compared without case.
	Ext []string
	// Argv is the program and its arguments. {file}, {dir}, {rel} and {reldir} are replaced
	// in every argument after the first: the absolute file, its absolute directory, the file
	// and the directory relative to the check's working directory.
	Argv []string
	// Cwd is file_dir, workspace or nearest:<marker>.
	Cwd string
	// OnFailureOnly leaves a passing check out of the tool result.
	OnFailureOnly bool
}

// Active reports whether any check can run.
func (settings HooksSettings) Active() bool {
	return settings.Enabled && len(settings.PostEdit) > 0
}

// normalizeHooksSettings reads the `hooks` node. Only an absent key takes a default; a
// null or misspelt value is an error so a typo cannot quietly turn a check off.
func normalizeHooksSettings(raw json.RawMessage, settingPath, envFileBase string) (HooksSettings, error) {
	settings := HooksSettings{TimeoutMS: defaultHooksTimeoutMS, MaxOutputBytes: defaultHooksMaxOutputBytes}
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		var err error
		if fields, err = decodeConfigObject(raw, settingPath+": hooks"); err != nil {
			return HooksSettings{}, configError("%s: hooks must be an object", settingPath)
		}
	}
	for key := range fields {
		if !containsString(hooksKeys, key) {
			return HooksSettings{}, unsupportedToolKey(settingPath+": hooks", key, hooksKeys, "")
		}
	}
	if value, exists := fields["enabled"]; exists {
		var enabled bool
		if json.Unmarshal(value, &enabled) != nil || strings.TrimSpace(string(value)) == "null" {
			return HooksSettings{}, configError("%s: hooks.enabled must be a boolean", settingPath)
		}
		settings.Enabled = enabled
	}
	bounded := func(name string, target *int64, minimum, maximum int64) error {
		value, exists := fields[name]
		if !exists {
			return nil
		}
		configured, ok := configSafeInteger(value)
		if !ok || configured < minimum || configured > maximum {
			return configError("%s: hooks.%s must be a safe integer between %d and %d", settingPath, name, minimum, maximum)
		}
		*target = configured
		return nil
	}
	if err := bounded("timeout_ms", &settings.TimeoutMS, minHooksTimeoutMS, maxHooksTimeoutMS); err != nil {
		return HooksSettings{}, err
	}
	if err := bounded("max_output_bytes", &settings.MaxOutputBytes, minHooksMaxOutputBytes, maxHooksMaxOutputBytes); err != nil {
		return HooksSettings{}, err
	}
	if err := normalizeHooksChild(&settings, fields, settingPath, envFileBase); err != nil {
		return HooksSettings{}, err
	}
	if value, exists := fields["post_edit"]; exists {
		var entries []json.RawMessage
		if json.Unmarshal(value, &entries) != nil || entries == nil || len(entries) > maxHooksPostEdit {
			return HooksSettings{}, configError("%s: hooks.post_edit must be a list of at most %d checks", settingPath, maxHooksPostEdit)
		}
		seen := map[string]bool{}
		for index, entry := range entries {
			hook, err := normalizePostEditHook(entry, settingPath, index)
			if err != nil {
				return HooksSettings{}, err
			}
			if seen[hook.Name] {
				return HooksSettings{}, configError("%s: hooks.post_edit[%d].name %q is used twice", settingPath, index, hook.Name)
			}
			seen[hook.Name] = true
			settings.PostEdit = append(settings.PostEdit, hook)
		}
	}
	return settings, nil
}

// normalizeHooksChild reads the child-process keys of the hooks node.
func normalizeHooksChild(settings *HooksSettings, fields map[string]json.RawMessage, settingPath, envFileBase string) error {
	const section = "hooks"
	var err error
	if settings.KillGraceMs, err = configuredInteger(fields, "kill_grace_ms", defaultNativeBashKillGraceMs, 0, maxNativeBashKillGraceMs, settingPath, section); err != nil {
		return err
	}
	if settings.KillProcessGroup, err = normalizeKillProcessGroup(fields, section, settingPath); err != nil {
		return err
	}
	if settings.MaxLineChars, err = configuredInteger(fields, "max_line_chars", defaultNativeBashMaxLine, 20, maxNativeBashMaxLine, settingPath, section); err != nil {
		return err
	}
	if settings.StripANSI, err = bashBool(fields, "strip_ansi", true, settingPath+": "+section); err != nil {
		return err
	}
	envFile, fileEnv, err := loadConfiguredEnvFile(fields, envFileBase, settingPath, section)
	if err != nil {
		return err
	}
	settings.EnvFile = envFile
	settings.Env, settings.EnvDeny, err = normalizeChildEnvironment(fields, section, settingPath, fileEnv)
	return err
}

func normalizePostEditHook(raw json.RawMessage, settingPath string, index int) (PostEditHook, error) {
	path := settingPath + ": hooks.post_edit[" + strconv.Itoa(index) + "]"
	fields, err := decodeConfigObject(raw, path)
	if err != nil {
		return PostEditHook{}, configError("%s must be an object", path)
	}
	for key := range fields {
		if !containsString(postEditHookKeys, key) {
			return PostEditHook{}, unsupportedToolKey(path, key, postEditHookKeys, "")
		}
	}
	hook := PostEditHook{Cwd: HookCwdFileDir}
	var name string
	if json.Unmarshal(fields["name"], &name) != nil || !hookNamePattern.MatchString(name) {
		return PostEditHook{}, configError("%s.name must match %s", path, hookNamePattern.String())
	}
	hook.Name = name
	var extensions []string
	if json.Unmarshal(fields["ext"], &extensions) != nil || len(extensions) == 0 || len(extensions) > maxHookExtensions {
		return PostEditHook{}, configError("%s.ext must be 1 to %d extensions such as \".go\"", path, maxHookExtensions)
	}
	for _, extension := range extensions {
		if !hookExtPattern.MatchString(extension) {
			return PostEditHook{}, configError("%s.ext entry %q must be a dot and 1 to 8 letters or digits", path, extension)
		}
	}
	hook.Ext = extensions
	var argv []string
	if json.Unmarshal(fields["argv"], &argv) != nil || len(argv) == 0 || len(argv) > maxHookArgv {
		return PostEditHook{}, configError("%s.argv must be 1 to %d non-empty strings: the program and its arguments", path, maxHookArgv)
	}
	for position, item := range argv {
		if strings.TrimSpace(item) == "" {
			return PostEditHook{}, configError("%s.argv must be 1 to %d non-empty strings: the program and its arguments", path, maxHookArgv)
		}
		if position == 0 && hookPlaceholderRef.MatchString(item) {
			return PostEditHook{}, configError("%s.argv[0] is the program and cannot hold a placeholder", path)
		}
		for _, found := range hookPlaceholderRef.FindAllString(item, -1) {
			if !containsString(hookPlaceholders, found) {
				return PostEditHook{}, configError("%s.argv[%d] has the unknown placeholder %s; use %s", path, position, found, strings.Join(hookPlaceholders, ", "))
			}
		}
	}
	hook.Argv = argv
	if value, exists := fields["cwd"]; exists {
		var cwd string
		if json.Unmarshal(value, &cwd) != nil || !validHookCwd(cwd) {
			return PostEditHook{}, configError("%s.cwd must be \"file_dir\", \"workspace\" or \"nearest:<marker file name>\"", path)
		}
		hook.Cwd = cwd
	}
	if value, exists := fields["on_failure_only"]; exists {
		if json.Unmarshal(value, &hook.OnFailureOnly) != nil || strings.TrimSpace(string(value)) == "null" {
			return PostEditHook{}, configError("%s.on_failure_only must be a boolean", path)
		}
	}
	return hook, nil
}

func validHookCwd(cwd string) bool {
	if cwd == HookCwdFileDir || cwd == HookCwdWorkspace {
		return true
	}
	marker, found := strings.CutPrefix(cwd, HookCwdNearestPrefix)
	return found && marker != "." && marker != ".." && hookMarkerPattern.MatchString(marker)
}
