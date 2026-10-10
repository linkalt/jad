package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultNativeBashShell               = "/bin/bash"
	defaultNativeBashTimeoutMs     int64 = 120_000
	defaultNativeBashMaxTimeoutMs  int64 = 600_000
	defaultNativeBashKillGraceMs   int64 = 2_000
	maxNativeBashKillGraceMs       int64 = 60_000
	defaultNativeBashMaxBytes      int64 = 64 * 1024
	defaultNativeBashStdoutHead    int64 = 16 * 1024
	defaultNativeBashStdoutTail    int64 = 32 * 1024
	defaultNativeBashStderrHead    int64 = 4 * 1024
	defaultNativeBashStderrTail    int64 = 12 * 1024
	defaultNativeBashMaxLine       int64 = 2_000
	maxNativeBashMaxLine           int64 = 8_000
	defaultNativeBashStdinMax      int64 = 256 * 1024
	defaultNativeBashSpillCap      int64 = 64 * 1024 * 1024
	maxNativeBashSpillCap          int64 = 1024 * 1024 * 1024
	defaultNativeBashRetention     int64 = 14
	maxNativeBashRetention         int64 = 3650
	defaultNativeBashSpillDir            = ".bash-output"
	defaultNativeBashSpillFilename       = "bash-{call_id}.{stream}.log"
	defaultNativeBashSpillPath           = "{cwd}/.bash-output/bash-{call_id}.{stream}.log"
	defaultNativeBashKillPolicy          = "always"
	defaultNativeBashSpillKeep           = "truncated_only"
	maxNativeBashTimeoutMs         int64 = 3_600_000
	NativeBashMinCallTimeoutMs     int64 = 1_000
)

var bashEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var bashSpillPlaceholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

// NativeBashStreamBudget bounds the head and tail retained for one command
// output stream.
type NativeBashStreamBudget struct {
	HeadBytes int64
	TailBytes int64
}

// NativeBashSpillSettings controls optional complete-output files.
type NativeBashSpillSettings struct {
	Enabled         bool
	Keep            string
	CapBytes        int64
	RetentionDays   int64
	StorageDir      string
	StdoutFull      string
	StderrFull      string
	AllowUnreadable bool
}

// NativeBashSettings contains normalized shell, path, output, and environment
// policy for native.bash.
type NativeBashSettings struct {
	Policy NativePathPolicy
	// Approval is "off" (default) or "ask".
	Approval         string
	Shell            string
	LoginShell       bool
	TimeoutMs        int64
	MaxTimeoutMs     int64
	KillGraceMs      int64
	KillProcessGroup string
	MaxBytes         int64
	Stdout           NativeBashStreamBudget
	Stderr           NativeBashStreamBudget
	Spill            NativeBashSpillSettings
	MaxLineChars     int64
	// StdinMaxBytes is the largest stdin text one call may pipe to its command.
	StdinMaxBytes int64
	StripANSI     bool
	EnvFile       string
	Env           map[string]string
	EnvDeny       []string
	// BaseEnv, FileEnv and InlineEnv are the parts Env was built from, in that order: the
	// built-in defaults, the env_file values, the inline `env` object. native.exec with its
	// own env_file swaps the file values and keeps the other two.
	BaseEnv   map[string]string
	FileEnv   map[string]string
	InlineEnv map[string]string
}

func normalizeNativeBashSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase string,
	write *NativeWriteSettings,
	readPolicy, grepPolicy NativePathPolicy,
) (*NativeBashSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["bash"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": tools.native.bash")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		switch key {
		case "shell", "login_shell", "timeout_ms", "max_timeout_ms", "kill_grace_ms", "kill_process_group", "max_bytes", "stdout", "stderr", "spill", "max_line_chars", "stdin_max_bytes", "strip_ansi", "env_file", "env", "env_deny", "paths", "approval", "capture_bytes", "head_ratio":
		default:
			return nil, configError("%s: tools.native.bash.%s is not supported", settingPath, key)
		}
	}
	if _, exists := fields["capture_bytes"]; exists {
		return nil, configError("%s: tools.native.bash.capture_bytes was removed; set tools.native.bash.stdout and tools.native.bash.stderr with head_bytes and tail_bytes instead", settingPath)
	}
	if _, exists := fields["head_ratio"]; exists {
		return nil, configError("%s: tools.native.bash.head_ratio was removed; each stream now has its own head_bytes and tail_bytes", settingPath)
	}

	settings := &NativeBashSettings{}
	if settings.Approval, err = normalizeApprovalMode(fields, "tools.native.bash", settingPath); err != nil {
		return nil, err
	}
	if settings.Shell, err = bashString(fields, "shell", defaultNativeBashShell, settingPath+": tools.native.bash", false); err != nil {
		return nil, err
	}
	if settings.LoginShell, err = bashBool(fields, "login_shell", false, settingPath+": tools.native.bash"); err != nil {
		return nil, err
	}
	if settings.TimeoutMs, err = configuredInteger(fields, "timeout_ms", defaultNativeBashTimeoutMs, 1, maxNativeBashTimeoutMs, settingPath, "tools.native.bash"); err != nil {
		return nil, err
	}
	if settings.MaxTimeoutMs, err = configuredInteger(fields, "max_timeout_ms", defaultNativeBashMaxTimeoutMs, 1, maxNativeBashTimeoutMs, settingPath, "tools.native.bash"); err != nil {
		return nil, err
	}
	if settings.MaxTimeoutMs < settings.TimeoutMs {
		return nil, configError("%s: tools.native.bash.max_timeout_ms must be greater than or equal to tools.native.bash.timeout_ms", settingPath)
	}
	if settings.KillGraceMs, err = configuredInteger(fields, "kill_grace_ms", defaultNativeBashKillGraceMs, 0, maxNativeBashKillGraceMs, settingPath, "tools.native.bash"); err != nil {
		return nil, err
	}
	settings.KillProcessGroup = defaultNativeBashKillPolicy
	if value, ok := fields["kill_process_group"]; ok && strings.TrimSpace(string(value)) != "null" {
		if err := json.Unmarshal(value, &settings.KillProcessGroup); err != nil || (settings.KillProcessGroup != "always" && settings.KillProcessGroup != "timeout_only") {
			return nil, configError("%s: tools.native.bash.kill_process_group must be always or timeout_only", settingPath)
		}
	}
	if settings.MaxBytes, err = configuredInteger(fields, "max_bytes", defaultNativeBashMaxBytes, 1, maxNativeToolBytes, settingPath, "tools.native.bash"); err != nil {
		return nil, err
	}
	settings.Stdout, err = normalizeNativeBashStream(fields["stdout"], NativeBashStreamBudget{HeadBytes: defaultNativeBashStdoutHead, TailBytes: defaultNativeBashStdoutTail}, settingPath, "tools.native.bash.stdout")
	if err != nil {
		return nil, err
	}
	settings.Stderr, err = normalizeNativeBashStream(fields["stderr"], NativeBashStreamBudget{HeadBytes: defaultNativeBashStderrHead, TailBytes: defaultNativeBashStderrTail}, settingPath, "tools.native.bash.stderr")
	if err != nil {
		return nil, err
	}
	if settings.Stdout.HeadBytes+settings.Stdout.TailBytes+settings.Stderr.HeadBytes+settings.Stderr.TailBytes > settings.MaxBytes {
		return nil, configError("%s: tools.native.bash stream budgets exceed tools.native.bash.max_bytes", settingPath)
	}
	settings.Spill, err = normalizeNativeBashSpill(fields["spill"], settingPath, cwd, sessionDir, toolStorageDir, readPolicy, grepPolicy)
	if err != nil {
		return nil, err
	}
	if settings.MaxLineChars, err = configuredInteger(fields, "max_line_chars", defaultNativeBashMaxLine, 20, maxNativeBashMaxLine, settingPath, "tools.native.bash"); err != nil {
		return nil, err
	}
	if settings.StdinMaxBytes, err = configuredInteger(fields, "stdin_max_bytes", defaultNativeBashStdinMax, 1, maxNativeToolBytes, settingPath, "tools.native.bash"); err != nil {
		return nil, err
	}
	if settings.StripANSI, err = bashBool(fields, "strip_ansi", true, settingPath+": tools.native.bash"); err != nil {
		return nil, err
	}

	settings.BaseEnv = map[string]string{"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"}
	settings.InlineEnv = map[string]string{}
	settings.Env = map[string]string{"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"}
	settings.EnvDeny = []string{}
	envFile, fileEnv, err := loadConfiguredEnvFile(fields, envFileBase, settingPath, "bash")
	if err != nil {
		return nil, err
	}
	settings.EnvFile = envFile
	settings.FileEnv = fileEnv
	for name, value := range fileEnv {
		settings.Env[name] = value
	}
	if rawEnv, ok := fields["env"]; ok && strings.TrimSpace(string(rawEnv)) != "null" {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(rawEnv, &values); err != nil || values == nil {
			return nil, configError("%s: tools.native.bash.env must be an object", settingPath)
		}
		for name, rawValue := range values {
			if !bashEnvNamePattern.MatchString(name) {
				return nil, configError("%s: tools.native.bash.env.%s is not a valid environment variable name", settingPath, name)
			}
			var value string
			if err := json.Unmarshal(rawValue, &value); err != nil {
				return nil, configError("%s: tools.native.bash.env.%s must be a string", settingPath, name)
			}
			settings.Env[name] = value
			settings.InlineEnv[name] = value
		}
	}
	if rawDeny, ok := fields["env_deny"]; ok && strings.TrimSpace(string(rawDeny)) != "null" {
		var names []json.RawMessage
		if err := json.Unmarshal(rawDeny, &names); err != nil || names == nil {
			return nil, configError("%s: tools.native.bash.env_deny must be an array", settingPath)
		}
		settings.EnvDeny = make([]string, 0, len(names))
		for index, rawName := range names {
			var name string
			if err := json.Unmarshal(rawName, &name); err != nil {
				return nil, configError("%s: tools.native.bash.env_deny[%d] must be an environment variable name", settingPath, index)
			}
			name = strings.TrimSpace(name)
			if name == "" || !bashEnvNamePattern.MatchString(name) {
				return nil, configError("%s: tools.native.bash.env_deny[%d] must be an environment variable name", settingPath, index)
			}
			settings.EnvDeny = append(settings.EnvDeny, name)
		}
	}

	fallbackPolicy := NativePathPolicy{Paths: []NativePathRule{}}
	if write != nil {
		fallbackPolicy = cloneNativePathPolicy(write.Policy)
	}
	settings.Policy = fallbackPolicy
	if paths, ok := fields["paths"]; ok {
		settings.Policy, err = normalizeNativePathPolicy(paths, globalJadRoot, settingPath+": tools.native.bash.paths")
		if err != nil {
			return nil, err
		}
	}
	return settings, nil
}

func normalizeNativeBashStream(raw json.RawMessage, fallback NativeBashStreamBudget, settingPath, section string) (NativeBashStreamBudget, error) {
	if len(raw) == 0 {
		return fallback, nil
	}
	sectionPath := settingPath + ": " + section
	fields, err := decodeConfigObject(raw, sectionPath)
	if err != nil {
		return NativeBashStreamBudget{}, configError("%s must be an object with head_bytes and tail_bytes", sectionPath)
	}
	head, err := configuredInteger(fields, "head_bytes", fallback.HeadBytes, 0, maxNativeToolBytes, settingPath, section)
	if err != nil {
		return NativeBashStreamBudget{}, err
	}
	tail, err := configuredInteger(fields, "tail_bytes", fallback.TailBytes, 0, maxNativeToolBytes, settingPath, section)
	if err != nil {
		return NativeBashStreamBudget{}, err
	}
	for key := range fields {
		if key != "head_bytes" && key != "tail_bytes" {
			return NativeBashStreamBudget{}, configError("%s.%s is not supported", sectionPath, key)
		}
	}
	return NativeBashStreamBudget{HeadBytes: head, TailBytes: tail}, nil
}

func normalizeNativeBashSpill(raw json.RawMessage, settingPath, cwd, sessionDir, toolStorageDir string, readPolicy, grepPolicy NativePathPolicy) (NativeBashSpillSettings, error) {
	settings := NativeBashSpillSettings{Enabled: true, Keep: defaultNativeBashSpillKeep, CapBytes: defaultNativeBashSpillCap, RetentionDays: defaultNativeBashRetention, AllowUnreadable: false}
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		var err error
		fields, err = decodeConfigObject(raw, settingPath+": tools.native.bash.spill")
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
	}
	for key := range fields {
		switch key {
		case "enabled", "keep", "cap_bytes", "retention_days", "storage_dir", "stdout_full", "stderr_full", "allow_unreadable":
		default:
			return NativeBashSpillSettings{}, configError("%s: tools.native.bash.spill.%s is not supported", settingPath, key)
		}
	}
	var err error
	if settings.Enabled, err = bashBool(fields, "enabled", true, settingPath+": tools.native.bash.spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}
	if value, ok := fields["keep"]; ok && strings.TrimSpace(string(value)) != "null" {
		if err := json.Unmarshal(value, &settings.Keep); err != nil || (settings.Keep != "always" && settings.Keep != "truncated_only") {
			return NativeBashSpillSettings{}, configError("%s: tools.native.bash.spill.keep must be always or truncated_only", settingPath)
		}
	}
	if settings.CapBytes, err = configuredInteger(fields, "cap_bytes", settings.CapBytes, 1, maxNativeBashSpillCap, settingPath, "tools.native.bash.spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}
	if settings.RetentionDays, err = configuredInteger(fields, "retention_days", settings.RetentionDays, 0, maxNativeBashRetention, settingPath, "tools.native.bash.spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}
	if settings.AllowUnreadable, err = bashBool(fields, "allow_unreadable", false, settingPath+": tools.native.bash.spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}

	_, hasStdoutTemplate := fields["stdout_full"]
	_, hasStderrTemplate := fields["stderr_full"]
	legacyTemplates := hasStdoutTemplate || hasStderrTemplate
	if legacyTemplates {
		if storage, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(storage)) != `"`+defaultNativeBashSpillDir+`"` {
			return NativeBashSpillSettings{}, configError("%s: tools.native.bash.spill.storage_dir cannot be combined with deprecated stdout_full/stderr_full templates; remove the templates to use storage_dir", settingPath)
		}
		stdout, err := bashString(fields, "stdout_full", defaultNativeBashSpillPath, settingPath+": tools.native.bash.spill", false)
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
		stderr, err := bashString(fields, "stderr_full", defaultNativeBashSpillPath, settingPath+": tools.native.bash.spill", false)
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
		stdout, err = normalizeBashSpillTemplate(stdout, cwd, sessionDir, toolStorageDir, settingPath, "stdout_full")
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
		stderr, err = normalizeBashSpillTemplate(stderr, cwd, sessionDir, toolStorageDir, settingPath, "stderr_full")
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
		if resolveBashSpillTemplate(stdout, cwd, sessionDir, "sample-call", "stdout") == resolveBashSpillTemplate(stderr, cwd, sessionDir, "sample-call", "stderr") {
			return NativeBashSpillSettings{}, configError("%s: tools.native.bash.spill.stdout_full and stderr_full resolve to the same file; give them different paths or include {stream}", settingPath)
		}
		if !settings.AllowUnreadable {
			for stream, template := range map[string]string{"stdout": stdout, "stderr": stderr} {
				target := resolveBashSpillTemplate(template, cwd, sessionDir, "sample-call", stream)
				if !bashSpillReadable(target, cwd, readPolicy, grepPolicy) {
					return NativeBashSpillSettings{}, configError("%s: tools.native.bash.spill.%s_full resolves to a file the model could not read back; keep it under the session cwd (%s) or extend tools.native.read.paths, or set tools.native.bash.spill.allow_unreadable", settingPath, stream, cwd)
				}
			}
		}
		settings.StdoutFull, settings.StderrFull = stdout, stderr
		return settings, nil
	}

	storageDir, err := bashString(fields, "storage_dir", defaultNativeBashSpillDir, settingPath+": tools.native.bash.spill", false)
	if err != nil {
		return NativeBashSpillSettings{}, err
	}
	storageDir = expandHome(storageDir)
	if !filepath.IsAbs(storageDir) {
		base := cwd
		if toolStorageDir != "" {
			base = toolStorageDir
		}
		storageDir = filepath.Join(base, storageDir)
	}
	settings.StorageDir = filepath.Clean(storageDir)
	if !settings.AllowUnreadable {
		for _, stream := range []string{"stdout", "stderr"} {
			fileName := strings.NewReplacer("{call_id}", "sample-call", "{stream}", stream).Replace(defaultNativeBashSpillFilename)
			target := filepath.Join(settings.StorageDir, fileName)
			if !bashSpillReadable(target, cwd, readPolicy, grepPolicy) {
				return NativeBashSpillSettings{}, configError("%s: tools.native.bash.spill.storage_dir resolves to a location the model could not read back; keep it under the session cwd (%s) or extend tools.native.read.paths, or set tools.native.bash.spill.allow_unreadable", settingPath, cwd)
			}
		}
	}
	return settings, nil
}

func normalizeBashSpillTemplate(template, cwd, sessionDir, toolStorageDir, settingPath, field string) (string, error) {
	for _, match := range bashSpillPlaceholderPattern.FindAllStringSubmatch(template, -1) {
		switch match[1] {
		case "cwd", "session_dir", "session_name", "call_id", "stream":
		default:
			return "", configError("%s: tools.native.bash.spill.%s uses unknown placeholder {%s}", settingPath, field, match[1])
		}
	}
	if !strings.Contains(template, "{call_id}") {
		return "", configError("%s: tools.native.bash.spill.%s must contain {call_id}, otherwise every call overwrites the previous call's file", settingPath, field)
	}
	sample := resolveBashSpillTemplate(template, cwd, sessionDir, "sample-call", "stdout")
	if !filepath.IsAbs(sample) {
		base := cwd
		if toolStorageDir != "" {
			base = toolStorageDir
		}
		return filepath.Join(base, template), nil
	}
	return template, nil
}

func resolveBashSpillTemplate(template, cwd, sessionDir, callID, stream string) string {
	values := map[string]string{
		"cwd":          cwd,
		"session_dir":  sessionDir,
		"session_name": filepath.Base(sessionDir),
		"call_id":      callID,
		"stream":       stream,
	}
	return bashSpillPlaceholderPattern.ReplaceAllStringFunc(template, func(placeholder string) string {
		return values[strings.Trim(placeholder, "{}")]
	})
}

func bashSpillReadable(target, cwd string, policies ...NativePathPolicy) bool {
	absolute, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	for _, policy := range policies {
		denied := false
		for _, rule := range policy.Paths {
			if rule.Action == "deny" && bashPathWithin(rule.Path, absolute) {
				denied = true
				break
			}
		}
		if denied {
			continue
		}
		if policy.UnrestrictedPaths || bashPathWithin(cwd, absolute) {
			return true
		}
		for _, rule := range policy.Paths {
			if rule.Action == "allow" && bashPathWithin(rule.Path, absolute) {
				return true
			}
		}
	}
	return false
}

func bashPathWithin(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && !filepath.IsAbs(relative) && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func bashString(fields map[string]json.RawMessage, name, fallback, pathPrefix string, allowEmpty bool) (string, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return fallback, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || (!allowEmpty && strings.TrimSpace(value) == "") {
		return "", configError("%s.%s must be a non-empty string", pathPrefix, name)
	}
	if !allowEmpty {
		value = strings.TrimSpace(value)
	}
	return value, nil
}

func bashBool(fields map[string]json.RawMessage, name string, fallback bool, pathPrefix string) (bool, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return fallback, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, configError("%s.%s must be a boolean", pathPrefix, name)
	}
	return value, nil
}
