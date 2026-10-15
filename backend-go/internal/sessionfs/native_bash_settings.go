package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultNativeBashShell                 = "/bin/bash"
	defaultNativeBashTimeoutMs       int64 = 120_000
	defaultNativeBashMaxTimeoutMs    int64 = 600_000
	defaultNativeBashKillGraceMs     int64 = 2_000
	maxNativeBashKillGraceMs         int64 = 60_000
	defaultNativeBashMaxBytes        int64 = 64 * 1024
	defaultNativeBashStdoutHead      int64 = 16 * 1024
	defaultNativeBashStdoutTail      int64 = 32 * 1024
	defaultNativeBashStderrHead      int64 = 4 * 1024
	defaultNativeBashStderrTail      int64 = 12 * 1024
	defaultNativeBashMaxLine         int64 = 2_000
	maxNativeBashMaxLine             int64 = 8_000
	defaultNativeBashStdinMax        int64 = 256 * 1024
	defaultNativeBashSpillCap        int64 = 64 * 1024 * 1024
	maxNativeBashSpillCap            int64 = 1024 * 1024 * 1024
	defaultNativeBashRetention       int64 = 14
	maxNativeBashRetention           int64 = 3650
	defaultNativeBashPruneInterval   int64 = 3600
	maxNativeBashPruneInterval       int64 = 86_400
	defaultNativeBashSpillDir              = ".bash-output"
	defaultNativeBashSpillFilePrefix       = "bash"
	defaultNativeBashSpillPath             = "{cwd}/.bash-output/bash-{call_id}.{stream}.log"
	defaultNativeExecSpillDir              = ".exec-output"
	defaultNativeExecSpillFilePrefix       = "exec"
	defaultNativeExecSpillPath             = "{cwd}/.exec-output/exec-{call_id}.{stream}.log"
	defaultNativeBashKillPolicy            = "always"
	defaultNativeBashSpillKeep             = "truncated_only"
	maxNativeBashTimeoutMs           int64 = 3_600_000
	NativeBashMinCallTimeoutMs       int64 = 1_000
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
	Enabled       bool
	Keep          string
	CapBytes      int64
	RetentionDays int64
	// PruneIntervalSeconds is the minimum time between prune scans of one spill
	// directory; 0 scans before every command.
	PruneIntervalSeconds int64
	StorageDir           string
	FilePrefix           string
	StdoutFull           string
	StderrFull           string
	AllowUnreadable      bool
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
}

// nativeBashStyleKeys are the settings of a tool that runs a child process. native.bash and
// native.exec each take them under their own name; neither reads the other's values.
var nativeBashStyleKeys = []string{"shell", "login_shell", "timeout_ms", "max_timeout_ms", "kill_grace_ms", "kill_process_group", "max_bytes", "stdout", "stderr", "spill", "max_line_chars", "stdin_max_bytes", "strip_ansi", "env_file", "env", "env_deny", "paths", "approval"}

func normalizeNativeBashSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase string,
	readPolicy, grepPolicy NativePathPolicy,
) (*NativeBashSettings, error) {
	const section = "tools.native.bash"
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
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if !containsString(nativeBashStyleKeys, key) && key != "capture_bytes" && key != "head_ratio" {
			return nil, configError("%s: %s.%s is not supported", settingPath, section, key)
		}
	}
	if _, exists := fields["capture_bytes"]; exists {
		return nil, configError("%s: %s.capture_bytes was removed; set %s.stdout and %s.stderr with head_bytes and tail_bytes instead", settingPath, section, section, section)
	}
	if _, exists := fields["head_ratio"]; exists {
		return nil, configError("%s: %s.head_ratio was removed; each stream now has its own head_bytes and tail_bytes", settingPath, section)
	}

	return normalizeBashStyleFields(fields, section, globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase, readPolicy, grepPolicy)
}

// normalizeBashStyleFields reads the child-process settings of one tool section. Every
// message names section, so native.exec's errors say tools.native.exec.
func normalizeBashStyleFields(
	fields map[string]json.RawMessage,
	section, globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase string,
	readPolicy, grepPolicy NativePathPolicy,
) (*NativeBashSettings, error) {
	var err error
	settings := &NativeBashSettings{}
	if settings.Approval, err = normalizeApprovalMode(fields, section, settingPath); err != nil {
		return nil, err
	}
	if settings.Shell, err = bashString(fields, "shell", defaultNativeBashShell, settingPath+": "+section, false); err != nil {
		return nil, err
	}
	if settings.LoginShell, err = bashBool(fields, "login_shell", false, settingPath+": "+section); err != nil {
		return nil, err
	}
	if settings.TimeoutMs, err = configuredInteger(fields, "timeout_ms", defaultNativeBashTimeoutMs, 1, maxNativeBashTimeoutMs, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxTimeoutMs, err = configuredInteger(fields, "max_timeout_ms", defaultNativeBashMaxTimeoutMs, 1, maxNativeBashTimeoutMs, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxTimeoutMs < settings.TimeoutMs {
		return nil, configError("%s: %s.max_timeout_ms must be greater than or equal to %s.timeout_ms", settingPath, section, section)
	}
	if settings.KillGraceMs, err = configuredInteger(fields, "kill_grace_ms", defaultNativeBashKillGraceMs, 0, maxNativeBashKillGraceMs, settingPath, section); err != nil {
		return nil, err
	}
	if settings.KillProcessGroup, err = normalizeKillProcessGroup(fields, section, settingPath); err != nil {
		return nil, err
	}
	if settings.MaxBytes, err = configuredInteger(fields, "max_bytes", defaultNativeBashMaxBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	settings.Stdout, err = normalizeNativeBashStream(fields["stdout"], NativeBashStreamBudget{HeadBytes: defaultNativeBashStdoutHead, TailBytes: defaultNativeBashStdoutTail}, settingPath, section+".stdout")
	if err != nil {
		return nil, err
	}
	settings.Stderr, err = normalizeNativeBashStream(fields["stderr"], NativeBashStreamBudget{HeadBytes: defaultNativeBashStderrHead, TailBytes: defaultNativeBashStderrTail}, settingPath, section+".stderr")
	if err != nil {
		return nil, err
	}
	if settings.Stdout.HeadBytes+settings.Stdout.TailBytes+settings.Stderr.HeadBytes+settings.Stderr.TailBytes > settings.MaxBytes {
		return nil, configError("%s: %s stream budgets exceed %s.max_bytes", settingPath, section, section)
	}
	settings.Spill, err = normalizeNativeBashSpill(fields["spill"], section, settingPath, cwd, sessionDir, toolStorageDir, readPolicy, grepPolicy)
	if err != nil {
		return nil, err
	}
	if settings.MaxLineChars, err = configuredInteger(fields, "max_line_chars", defaultNativeBashMaxLine, 20, maxNativeBashMaxLine, settingPath, section); err != nil {
		return nil, err
	}
	if settings.StdinMaxBytes, err = configuredInteger(fields, "stdin_max_bytes", defaultNativeBashStdinMax, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.StripANSI, err = bashBool(fields, "strip_ansi", true, settingPath+": "+section); err != nil {
		return nil, err
	}

	envFile, fileEnv, err := loadConfiguredEnvFile(fields, envFileBase, settingPath, section)
	if err != nil {
		return nil, err
	}
	settings.EnvFile = envFile
	if settings.Env, settings.EnvDeny, err = normalizeChildEnvironment(fields, section, settingPath, fileEnv); err != nil {
		return nil, err
	}

	settings.Policy = NativePathPolicy{Paths: []NativePathRule{}}
	if paths, ok := fields["paths"]; ok {
		settings.Policy, err = normalizeNativePathPolicy(paths, globalJadRoot, settingPath+": "+section+".paths")
		if err != nil {
			return nil, err
		}
	}
	return settings, nil
}

// normalizeKillProcessGroup reads kill_process_group from the section's fields.
func normalizeKillProcessGroup(fields map[string]json.RawMessage, section, settingPath string) (string, error) {
	policy := defaultNativeBashKillPolicy
	if value, ok := fields["kill_process_group"]; ok && strings.TrimSpace(string(value)) != "null" {
		if err := json.Unmarshal(value, &policy); err != nil || (policy != "always" && policy != "timeout_only") {
			return "", configError("%s: %s.kill_process_group must be always or timeout_only", settingPath, section)
		}
	}
	return policy, nil
}

// normalizeChildEnvironment builds the variables a child process gets on top of the
// allowlist: the defaults, then fileEnv, then the section's env; env_deny names the ones to drop.
func normalizeChildEnvironment(fields map[string]json.RawMessage, section, settingPath string, fileEnv map[string]string) (map[string]string, []string, error) {
	environment := map[string]string{"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"}
	denied := []string{}
	for name, value := range fileEnv {
		environment[name] = value
	}
	if rawEnv, ok := fields["env"]; ok && strings.TrimSpace(string(rawEnv)) != "null" {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(rawEnv, &values); err != nil || values == nil {
			return nil, nil, configError("%s: %s.env must be an object", settingPath, section)
		}
		for name, rawValue := range values {
			if !bashEnvNamePattern.MatchString(name) {
				return nil, nil, configError("%s: %s.env.%s is not a valid environment variable name", settingPath, section, name)
			}
			var value string
			if err := json.Unmarshal(rawValue, &value); err != nil {
				return nil, nil, configError("%s: %s.env.%s must be a string", settingPath, section, name)
			}
			environment[name] = value
		}
	}
	if rawDeny, ok := fields["env_deny"]; ok && strings.TrimSpace(string(rawDeny)) != "null" {
		var names []json.RawMessage
		if err := json.Unmarshal(rawDeny, &names); err != nil || names == nil {
			return nil, nil, configError("%s: %s.env_deny must be an array", settingPath, section)
		}
		denied = make([]string, 0, len(names))
		for index, rawName := range names {
			var name string
			if err := json.Unmarshal(rawName, &name); err != nil {
				return nil, nil, configError("%s: %s.env_deny[%d] must be an environment variable name", settingPath, section, index)
			}
			name = strings.TrimSpace(name)
			if name == "" || !bashEnvNamePattern.MatchString(name) {
				return nil, nil, configError("%s: %s.env_deny[%d] must be an environment variable name", settingPath, section, index)
			}
			denied = append(denied, name)
		}
	}
	return environment, denied, nil
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

func normalizeNativeBashSpill(raw json.RawMessage, section, settingPath, cwd, sessionDir, toolStorageDir string, readPolicy, grepPolicy NativePathPolicy) (NativeBashSpillSettings, error) {
	defaultDir, defaultPath, filePrefix := defaultNativeBashSpillDir, defaultNativeBashSpillPath, defaultNativeBashSpillFilePrefix
	if section == "tools.native.exec" {
		defaultDir, defaultPath, filePrefix = defaultNativeExecSpillDir, defaultNativeExecSpillPath, defaultNativeExecSpillFilePrefix
	}
	settings := NativeBashSpillSettings{FilePrefix: filePrefix, Enabled: true, Keep: defaultNativeBashSpillKeep, CapBytes: defaultNativeBashSpillCap, RetentionDays: defaultNativeBashRetention, PruneIntervalSeconds: defaultNativeBashPruneInterval, AllowUnreadable: false}
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		var err error
		fields, err = decodeConfigObject(raw, settingPath+": "+section+".spill")
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
	}
	for key := range fields {
		switch key {
		case "enabled", "keep", "cap_bytes", "retention_days", "prune_interval_seconds", "storage_dir", "stdout_full", "stderr_full", "allow_unreadable":
		default:
			return NativeBashSpillSettings{}, configError("%s: %s.spill.%s is not supported", settingPath, section, key)
		}
	}
	var err error
	if settings.Enabled, err = bashBool(fields, "enabled", true, settingPath+": "+section+".spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}
	if value, ok := fields["keep"]; ok && strings.TrimSpace(string(value)) != "null" {
		if err := json.Unmarshal(value, &settings.Keep); err != nil || (settings.Keep != "always" && settings.Keep != "truncated_only") {
			return NativeBashSpillSettings{}, configError("%s: %s.spill.keep must be always or truncated_only", settingPath, section)
		}
	}
	if settings.CapBytes, err = configuredInteger(fields, "cap_bytes", settings.CapBytes, 1, maxNativeBashSpillCap, settingPath, section+".spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}
	if settings.RetentionDays, err = configuredInteger(fields, "retention_days", settings.RetentionDays, 0, maxNativeBashRetention, settingPath, section+".spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}
	if settings.PruneIntervalSeconds, err = configuredInteger(fields, "prune_interval_seconds", settings.PruneIntervalSeconds, 0, maxNativeBashPruneInterval, settingPath, section+".spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}
	if settings.AllowUnreadable, err = bashBool(fields, "allow_unreadable", false, settingPath+": "+section+".spill"); err != nil {
		return NativeBashSpillSettings{}, err
	}

	_, hasStdoutTemplate := fields["stdout_full"]
	_, hasStderrTemplate := fields["stderr_full"]
	legacyTemplates := hasStdoutTemplate || hasStderrTemplate
	if legacyTemplates {
		if storage, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(storage)) != `"`+defaultDir+`"` {
			return NativeBashSpillSettings{}, configError("%s: %s.spill.storage_dir cannot be combined with deprecated stdout_full/stderr_full templates; remove the templates to use storage_dir", settingPath, section)
		}
		stdout, err := bashString(fields, "stdout_full", defaultPath, settingPath+": "+section+".spill", false)
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
		stderr, err := bashString(fields, "stderr_full", defaultPath, settingPath+": "+section+".spill", false)
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
		stdout, err = normalizeBashSpillTemplate(stdout, cwd, sessionDir, toolStorageDir, settingPath, section, "stdout_full")
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
		stderr, err = normalizeBashSpillTemplate(stderr, cwd, sessionDir, toolStorageDir, settingPath, section, "stderr_full")
		if err != nil {
			return NativeBashSpillSettings{}, err
		}
		if resolveBashSpillTemplate(stdout, cwd, sessionDir, "sample-call", "stdout") == resolveBashSpillTemplate(stderr, cwd, sessionDir, "sample-call", "stderr") {
			return NativeBashSpillSettings{}, configError("%s: %s.spill.stdout_full and stderr_full resolve to the same file; give them different paths or include {stream}", settingPath, section)
		}
		if !settings.AllowUnreadable {
			for stream, template := range map[string]string{"stdout": stdout, "stderr": stderr} {
				target := resolveBashSpillTemplate(template, cwd, sessionDir, "sample-call", stream)
				if !bashSpillReadable(target, cwd, readPolicy, grepPolicy) {
					return NativeBashSpillSettings{}, configError("%s: %s.spill.%s_full resolves to a file the model could not read back; keep it under the session cwd (%s) or extend tools.native.read.paths, or set %s.spill.allow_unreadable", settingPath, section, stream, cwd, section)
				}
			}
		}
		settings.StdoutFull, settings.StderrFull = stdout, stderr
		return settings, nil
	}

	storageDir, err := bashString(fields, "storage_dir", defaultDir, settingPath+": "+section+".spill", false)
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
			fileName := strings.NewReplacer("{call_id}", "sample-call", "{stream}", stream).Replace(filePrefix + "-{call_id}.{stream}.log")
			target := filepath.Join(settings.StorageDir, fileName)
			if !bashSpillReadable(target, cwd, readPolicy, grepPolicy) {
				return NativeBashSpillSettings{}, configError("%s: %s.spill.storage_dir resolves to a location the model could not read back; keep it under the session cwd (%s) or extend tools.native.read.paths, or set %s.spill.allow_unreadable", settingPath, section, cwd, section)
			}
		}
	}
	return settings, nil
}

func normalizeBashSpillTemplate(template, cwd, sessionDir, toolStorageDir, settingPath, section, field string) (string, error) {
	for _, match := range bashSpillPlaceholderPattern.FindAllStringSubmatch(template, -1) {
		switch match[1] {
		case "cwd", "session_dir", "session_name", "call_id", "stream":
		default:
			return "", configError("%s: %s.spill.%s uses unknown placeholder {%s}", settingPath, section, field, match[1])
		}
	}
	if !strings.Contains(template, "{call_id}") {
		return "", configError("%s: %s.spill.%s must contain {call_id}, otherwise every call overwrites the previous call's file", settingPath, section, field)
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
