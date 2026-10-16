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

	defaultHooksTimeoutMS       int64 = 60_000
	minHooksTimeoutMS           int64 = 1_000
	maxHooksTimeoutMS           int64 = 600_000
	defaultHooksMaxOutputBytes  int64 = 8 * 1024
	minHooksMaxOutputBytes      int64 = 256
	maxHooksMaxOutputBytes      int64 = 1024 * 1024
	defaultHooksMaxPayloadBytes int64 = 64 * 1024
	minHooksMaxPayloadBytes     int64 = 1024
	maxHooksMaxPayloadBytes     int64 = 1024 * 1024
	maxHooksPostEdit                  = 32
	maxHooksToolEvent                 = 32
	maxHooksRunEvent                  = 32
	maxHookArgv                       = 32
	maxHookExtensions                 = 32
)

var (
	hooksKeys          = []string{"enabled", "env", "env_deny", "env_file", "kill_deadline_ms", "drain_after_exit_ms", "kill_grace_ms", "kill_process_group", "max_line_chars", "max_output_bytes", "max_payload_bytes", "post_edit", "post_tool", "pre_compact", "pre_tool", "run_end", "run_start", "strip_ansi", "timeout_ms"}
	postEditHookKeys   = []string{"argv", "cwd", "ext", "name", "on_failure_only"}
	toolEventHookKeys  = []string{"argv", "match", "name", "on_failure_only"}
	preToolHookKeys    = []string{"argv", "blocking", "match", "name", "on_failure_only"}
	runEventHookKeys   = []string{"argv", "name", "on_failure_only"}
	hookMatchPattern   = regexp.MustCompile(`^(\*|[A-Za-z0-9_-]{1,64}|mcp:[A-Za-z0-9_.-]{1,64}:(\*|[A-Za-z0-9_.-]{1,128}))$`)
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
	// PreTool and PostTool run around any tool call whose tool the entry's Match covers.
	PreTool  []ToolEventHook
	PostTool []ToolEventHook
	// RunStart and RunEnd run once around every run of the session itself (not its sub-agents).
	RunStart []RunEventHook
	RunEnd   []RunEventHook
	// PreCompact runs before every compaction attempt of the session, automatic or manual.
	PreCompact []RunEventHook
	// MaxPayloadBytes cuts the event JSON a command reads on stdin.
	MaxPayloadBytes int64
	// The child-process settings of a check. They are the hooks' own: nothing is read from
	// tools.native.bash. Env and EnvDeny shape the environment on top of the shared allowlist.
	KillGraceMs      int64
	KillDeadlineMs   int64
	DrainAfterExitMs int64
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

// ToolEventHook is one command run around a tool call. It reads the event as JSON on stdin.
type ToolEventHook struct {
	Name string
	// Match is "*" (every tool), a native tool name, "mcp:<server>:<tool>" or
	// "mcp:<server>:*", the same entries a profile's tools.allow takes.
	Match string
	// Argv is the program and its arguments, run with no shell and no placeholders.
	Argv []string
	// OnFailureOnly leaves a passing command out of the tool result.
	OnFailureOnly bool
	// Blocking (pre_tool only) stops the call when the command does not exit 0: a non-zero
	// exit, a signal, a timeout or a command that could not start.
	Blocking bool
}

// RunEventHook is one command run at the start or the end of a run, or before a compaction. It reads the event as
// JSON on stdin; its report goes to the server log, never to the model or the transcript.
type RunEventHook struct {
	Name string
	// Argv is the program and its arguments, run with no shell and no placeholders.
	Argv []string
	// OnFailureOnly leaves a passing command out of the log.
	OnFailureOnly bool
}

// Active reports whether any check can run.
func (settings HooksSettings) Active() bool {
	return settings.PostEditActive() || settings.ToolEventsActive() || settings.RunEventsActive()
}

// RunEventsActive reports whether a run_start, run_end or pre_compact command can run.
func (settings HooksSettings) RunEventsActive() bool {
	return settings.Enabled && (len(settings.RunStart) > 0 || len(settings.RunEnd) > 0 || len(settings.PreCompact) > 0)
}

// PostEditActive reports whether a post_edit check can run.
func (settings HooksSettings) PostEditActive() bool {
	return settings.Enabled && len(settings.PostEdit) > 0
}

// ToolEventsActive reports whether a pre_tool or post_tool command can run.
func (settings HooksSettings) ToolEventsActive() bool {
	return settings.Enabled && (len(settings.PreTool) > 0 || len(settings.PostTool) > 0)
}

// normalizeHooksSettings reads the `hooks` node. Only an absent key takes a default; a
// null or misspelt value is an error so a typo cannot quietly turn a check off.
func normalizeHooksSettings(raw json.RawMessage, settingPath, envFileBase string) (HooksSettings, error) {
	settings := HooksSettings{TimeoutMS: defaultHooksTimeoutMS, MaxOutputBytes: defaultHooksMaxOutputBytes, MaxPayloadBytes: defaultHooksMaxPayloadBytes}
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
	if err := bounded("max_payload_bytes", &settings.MaxPayloadBytes, minHooksMaxPayloadBytes, maxHooksMaxPayloadBytes); err != nil {
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
	var err error
	if settings.PreTool, err = normalizeToolEventHooks(fields, "pre_tool", settingPath); err != nil {
		return HooksSettings{}, err
	}
	if settings.PostTool, err = normalizeToolEventHooks(fields, "post_tool", settingPath); err != nil {
		return HooksSettings{}, err
	}
	if settings.RunStart, err = normalizeRunEventHooks(fields, "run_start", settingPath); err != nil {
		return HooksSettings{}, err
	}
	if settings.RunEnd, err = normalizeRunEventHooks(fields, "run_end", settingPath); err != nil {
		return HooksSettings{}, err
	}
	if settings.PreCompact, err = normalizeRunEventHooks(fields, "pre_compact", settingPath); err != nil {
		return HooksSettings{}, err
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
	if settings.KillDeadlineMs, err = configuredInteger(fields, "kill_deadline_ms", defaultNativeBashKillDeadlineMs, 1, maxNativeBashKillDeadlineMs, settingPath, section); err != nil {
		return err
	}
	if settings.DrainAfterExitMs, err = configuredInteger(fields, "drain_after_exit_ms", defaultNativeBashDrainAfterExit, 1, maxNativeBashDrainAfterExitMs, settingPath, section); err != nil {
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

func normalizeToolEventHooks(fields map[string]json.RawMessage, event, settingPath string) ([]ToolEventHook, error) {
	return normalizeEventHookList(fields, event, settingPath, maxHooksToolEvent)
}

// normalizeRunEventHooks reads run_start, run_end or pre_compact: the entries of a tool event without match.
func normalizeRunEventHooks(fields map[string]json.RawMessage, event, settingPath string) ([]RunEventHook, error) {
	entries, err := normalizeEventHookList(fields, event, settingPath, maxHooksRunEvent)
	var hooks []RunEventHook
	for _, entry := range entries {
		hooks = append(hooks, RunEventHook{Name: entry.Name, Argv: entry.Argv, OnFailureOnly: entry.OnFailureOnly})
	}
	return hooks, err
}

func normalizeEventHookList(fields map[string]json.RawMessage, event, settingPath string, maximum int) ([]ToolEventHook, error) {
	value, exists := fields[event]
	if !exists {
		return nil, nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(value, &entries) != nil || entries == nil || len(entries) > maximum {
		return nil, configError("%s: hooks.%s must be a list of at most %d commands", settingPath, event, maximum)
	}
	var hooks []ToolEventHook
	seen := map[string]bool{}
	for index, entry := range entries {
		hook, err := normalizeToolEventHook(entry, settingPath, event, index)
		if err != nil {
			return nil, err
		}
		if seen[hook.Name] {
			return nil, configError("%s: hooks.%s[%d].name %q is used twice", settingPath, event, index, hook.Name)
		}
		seen[hook.Name] = true
		hooks = append(hooks, hook)
	}
	return hooks, nil
}

func normalizeToolEventHook(raw json.RawMessage, settingPath, event string, index int) (ToolEventHook, error) {
	path := settingPath + ": hooks." + event + "[" + strconv.Itoa(index) + "]"
	fields, err := decodeConfigObject(raw, path)
	if err != nil {
		return ToolEventHook{}, configError("%s must be an object", path)
	}
	allowed := toolEventHookKeys
	switch event {
	case "pre_tool":
		allowed = preToolHookKeys
	case "run_start", "run_end", "pre_compact":
		allowed = runEventHookKeys
	}
	for key := range fields {
		if !containsString(allowed, key) {
			return ToolEventHook{}, unsupportedToolKey(path, key, allowed, "")
		}
	}
	hook := ToolEventHook{Match: "*"}
	var name string
	if json.Unmarshal(fields["name"], &name) != nil || !hookNamePattern.MatchString(name) {
		return ToolEventHook{}, configError("%s.name must match %s", path, hookNamePattern.String())
	}
	hook.Name = name
	if value, exists := fields["match"]; exists {
		var match string
		if json.Unmarshal(value, &match) != nil || !hookMatchPattern.MatchString(match) {
			return ToolEventHook{}, configError("%s.match must be \"*\", a native tool name, \"mcp:<server>:<tool>\" or \"mcp:<server>:*\"", path)
		}
		hook.Match = match
	}
	var argv []string
	if json.Unmarshal(fields["argv"], &argv) != nil || len(argv) == 0 || len(argv) > maxHookArgv {
		return ToolEventHook{}, configError("%s.argv must be 1 to %d non-empty strings: the program and its arguments", path, maxHookArgv)
	}
	for position, item := range argv {
		if strings.TrimSpace(item) == "" {
			return ToolEventHook{}, configError("%s.argv must be 1 to %d non-empty strings: the program and its arguments", path, maxHookArgv)
		}
		if hookPlaceholderRef.MatchString(item) {
			return ToolEventHook{}, configError("%s.argv[%d] has a placeholder; a %s command reads the event as JSON on stdin", path, position, event)
		}
	}
	hook.Argv = argv
	if value, exists := fields["on_failure_only"]; exists {
		if json.Unmarshal(value, &hook.OnFailureOnly) != nil || strings.TrimSpace(string(value)) == "null" {
			return ToolEventHook{}, configError("%s.on_failure_only must be a boolean", path)
		}
	}
	if value, exists := fields["blocking"]; exists {
		if json.Unmarshal(value, &hook.Blocking) != nil || strings.TrimSpace(string(value)) == "null" {
			return ToolEventHook{}, configError("%s.blocking must be a boolean", path)
		}
	}
	return hook, nil
}
