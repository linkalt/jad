package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	defaultNativeProcessStorageDir         = ".processes"
	defaultNativeProcessMaxRunning   int64 = 8
	defaultNativeProcessWaitTimeout  int64 = 10_000
	defaultNativeProcessMaxWait      int64 = 120_000
	defaultNativeProcessLogCap       int64 = 64 * 1024 * 1024
	defaultNativeProcessStdinMax     int64 = 256 * 1024
	defaultNativeProcessMaxCompleted int64 = 100
	maxNativeProcessMaxRunning       int64 = 1024
	maxNativeProcessMaxCompleted     int64 = 100_000
	maxNativeProcessStateContext     int64 = 100_000
	defaultNativeProcessStateLimit   int64 = 20
	defaultStateContextMaxBytes      int64 = 64 * 1024
	maxStateContextBytes             int64 = 10 * 1024 * 1024
)

var defaultNativeProcessStateNames = []string{"running"}
var allowedNativeProcessStates = map[string]struct{}{
	"running": {}, "exited": {}, "stopped": {}, "failed": {}, "lost": {},
}

// NativeProcessStateContextSettings bounds process records copied into a
// compaction state-context snapshot.
type NativeProcessStateContextSettings struct {
	States []string
	Limit  int64
}

// NativeProcessSettings contains process lifecycle, output, environment, and
// storage policy for native.process.
type NativeProcessSettings struct {
	Policy NativePathPolicy
	// Approval is "off" (default) or "ask"; only a start asks.
	Approval         string
	Shell            string
	LoginShell       bool
	KillGraceMs      int64
	MaxRunning       int64
	MaxCompleted     int64
	RetentionDays    int64
	WaitTimeoutMs    int64
	MaxWaitTimeoutMs int64
	Stdout           NativeBashStreamBudget
	Stderr           NativeBashStreamBudget
	LogCapBytes      int64
	MaxLineChars     int64
	// StdinMaxBytes is the largest stdin text one start may write to its process.
	StdinMaxBytes int64
	StripANSI     bool
	// StopOrphans makes the manager, when it finds a process a crashed server left
	// running, stop its process group once it has checked the process is the one the
	// manifest names. Off only marks it lost.
	StopOrphans  bool
	EnvFile      string
	Env          map[string]string
	EnvDeny      []string
	StorageDir   string
	StateContext NativeProcessStateContextSettings
}

func normalizeNativeProcessSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir, envFileBase string,
) (*NativeProcessSettings, error) {
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
	processRaw, exists := nativeFields["process"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(processRaw, settingPath+": tools.native.process")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		switch key {
		case "shell", "login_shell", "kill_grace_ms", "max_running", "max_completed", "retention_days", "wait_timeout_ms", "max_wait_timeout_ms", "stdout", "stderr", "status_log_bytes", "log_cap_bytes", "max_line_chars", "stdin_max_bytes", "strip_ansi", "stop_orphans", "env_file", "env", "env_deny", "storage_dir", "paths", "statecontext", "approval":
		default:
			return nil, configError("%s: tools.native.process.%s is not supported", settingPath, key)
		}
	}
	if _, hasLegacyBudget := fields["status_log_bytes"]; hasLegacyBudget {
		if _, hasStdout := fields["stdout"]; hasStdout {
			return nil, configError("%s: tools.native.process.status_log_bytes cannot be combined with stdout or stderr budgets", settingPath)
		}
		if _, hasStderr := fields["stderr"]; hasStderr {
			return nil, configError("%s: tools.native.process.status_log_bytes cannot be combined with stdout or stderr budgets", settingPath)
		}
	}

	approval, err := normalizeApprovalMode(fields, "tools.native.process", settingPath)
	if err != nil {
		return nil, err
	}
	settings := &NativeProcessSettings{
		Approval:   approval,
		Shell:      defaultNativeBashShell,
		LoginShell: false,
		Env:        map[string]string{"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"},
		EnvDeny:    []string{},
	}
	if raw, ok := fields["shell"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.Shell); err != nil || strings.TrimSpace(settings.Shell) == "" {
			return nil, configError("%s: tools.native.process.shell must be a non-empty string", settingPath)
		}
		settings.Shell = strings.TrimSpace(settings.Shell)
	}
	if raw, ok := fields["login_shell"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.LoginShell); err != nil {
			return nil, configError("%s: tools.native.process.login_shell must be a boolean", settingPath)
		}
	}
	if settings.KillGraceMs, err = configuredInteger(fields, "kill_grace_ms", defaultNativeBashKillGraceMs, 0, maxNativeBashKillGraceMs, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if settings.MaxRunning, err = configuredInteger(fields, "max_running", defaultNativeProcessMaxRunning, 1, maxNativeProcessMaxRunning, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if settings.MaxCompleted, err = configuredInteger(fields, "max_completed", defaultNativeProcessMaxCompleted, 1, maxNativeProcessMaxCompleted, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if settings.RetentionDays, err = configuredInteger(fields, "retention_days", defaultNativeBashRetention, 0, maxNativeBashRetention, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if settings.WaitTimeoutMs, err = configuredInteger(fields, "wait_timeout_ms", defaultNativeProcessWaitTimeout, 1, maxNativeBashTimeoutMs, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if settings.MaxWaitTimeoutMs, err = configuredInteger(fields, "max_wait_timeout_ms", defaultNativeProcessMaxWait, 1, maxNativeBashTimeoutMs, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if settings.MaxWaitTimeoutMs < settings.WaitTimeoutMs {
		return nil, configError("%s: tools.native.process.max_wait_timeout_ms must be greater than or equal to wait_timeout_ms", settingPath)
	}

	stdoutFallback := NativeBashStreamBudget{HeadBytes: defaultNativeBashStdoutHead, TailBytes: defaultNativeBashStdoutTail}
	stderrFallback := NativeBashStreamBudget{HeadBytes: defaultNativeBashStderrHead, TailBytes: defaultNativeBashStderrTail}
	maxLineFallback := defaultNativeBashMaxLine
	stripANSIFallback := true
	if _, ok := fields["status_log_bytes"]; ok {
		legacy, err := configuredInteger(fields, "status_log_bytes", 1, 1, maxNativeToolBytes, settingPath, "tools.native.process")
		if err != nil {
			return nil, err
		}
		settings.Stdout = NativeBashStreamBudget{TailBytes: legacy}
		settings.Stderr = NativeBashStreamBudget{TailBytes: legacy}
	} else {
		stdoutRaw := fields["stdout"]
		stderrRaw := fields["stderr"]
		if settings.Stdout, err = normalizeNativeBashStream(stdoutRaw, stdoutFallback, settingPath, "tools.native.process.stdout"); err != nil {
			return nil, err
		}
		if settings.Stderr, err = normalizeNativeBashStream(stderrRaw, stderrFallback, settingPath, "tools.native.process.stderr"); err != nil {
			return nil, err
		}
	}
	statusOutputBytes := settings.Stdout.HeadBytes + settings.Stdout.TailBytes + settings.Stderr.HeadBytes + settings.Stderr.TailBytes
	if statusOutputBytes > maxNativeToolBytes {
		return nil, configError("%s: tools.native.process stdout and stderr budgets total %d bytes, exceeding %d", settingPath, statusOutputBytes, maxNativeToolBytes)
	}
	if settings.LogCapBytes, err = configuredInteger(fields, "log_cap_bytes", defaultNativeProcessLogCap, 1, maxNativeBashSpillCap, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if max(settings.Stdout.HeadBytes+settings.Stdout.TailBytes, settings.Stderr.HeadBytes+settings.Stderr.TailBytes) > settings.LogCapBytes {
		return nil, configError("%s: tools.native.process stream budgets must not exceed log_cap_bytes", settingPath)
	}
	if settings.MaxLineChars, err = configuredInteger(fields, "max_line_chars", maxLineFallback, 20, maxNativeBashMaxLine, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if settings.StdinMaxBytes, err = configuredInteger(fields, "stdin_max_bytes", defaultNativeProcessStdinMax, 1, maxNativeToolBytes, settingPath, "tools.native.process"); err != nil {
		return nil, err
	}
	if settings.StopOrphans, err = bashBool(fields, "stop_orphans", true, settingPath+": tools.native.process"); err != nil {
		return nil, err
	}
	settings.StripANSI = stripANSIFallback
	if raw, ok := fields["strip_ansi"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.StripANSI); err != nil {
			return nil, configError("%s: tools.native.process.strip_ansi must be a boolean", settingPath)
		}
	}
	// Safe defaults, then env_file, then inline env (inline wins). env_file is
	// the process tool's own setting and is not inherited from bash.
	envFile, fileEnv, err := loadConfiguredEnvFile(fields, envFileBase, settingPath, "process")
	if err != nil {
		return nil, err
	}
	settings.EnvFile = envFile
	for name, value := range fileEnv {
		settings.Env[name] = value
	}
	if raw, ok := fields["env"]; ok && strings.TrimSpace(string(raw)) != "null" {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			return nil, configError("%s: tools.native.process.env must be an object", settingPath)
		}
		for name, valueRaw := range values {
			if !bashEnvNamePattern.MatchString(name) {
				return nil, configError("%s: tools.native.process.env.%s is not a valid environment variable name", settingPath, name)
			}
			var value string
			if err := json.Unmarshal(valueRaw, &value); err != nil {
				return nil, configError("%s: tools.native.process.env.%s must be a string", settingPath, name)
			}
			settings.Env[name] = value
		}
	}
	if raw, ok := fields["env_deny"]; ok && strings.TrimSpace(string(raw)) != "null" {
		var names []json.RawMessage
		if err := json.Unmarshal(raw, &names); err != nil || names == nil {
			return nil, configError("%s: tools.native.process.env_deny must be an array", settingPath)
		}
		for index, rawName := range names {
			var name string
			if err := json.Unmarshal(rawName, &name); err != nil || !bashEnvNamePattern.MatchString(name) {
				return nil, configError("%s: tools.native.process.env_deny[%d] must be an environment variable name", settingPath, index)
			}
			settings.EnvDeny = append(settings.EnvDeny, name)
		}
	}
	settings.StorageDir, err = normalizeNativeProcessStorageDir(fields, cwd, toolStorageDir, settingPath)
	if err != nil {
		return nil, err
	}
	settings.StateContext, err = normalizeNativeProcessStateContext(fields["statecontext"], settingPath)
	if err != nil {
		return nil, err
	}
	settings.Policy = NativePathPolicy{Paths: []NativePathRule{}}
	if raw, exists := fields["paths"]; exists {
		settings.Policy, err = normalizeNativePathPolicy(raw, globalJadRoot, settingPath+": tools.native.process.paths")
		if err != nil {
			return nil, err
		}
	}
	return settings, nil
}

func normalizeNativeProcessStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	configured := defaultNativeProcessStorageDir
	if raw, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return "", configError("%s: tools.native.process.storage_dir must be a non-empty string", settingPath)
		}
		configured = strings.TrimSpace(configured)
	}
	configured = expandHome(configured)
	if !filepath.IsAbs(configured) {
		base := cwd
		if toolStorageDir != "" {
			base = toolStorageDir
		}
		configured = filepath.Join(base, configured)
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve tools.native.process.storage_dir: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func normalizeNativeProcessStateContext(raw json.RawMessage, settingPath string) (NativeProcessStateContextSettings, error) {
	settings := NativeProcessStateContextSettings{States: append([]string(nil), defaultNativeProcessStateNames...), Limit: defaultNativeProcessStateLimit}
	if len(raw) == 0 {
		return settings, nil
	}
	section := settingPath + ": tools.native.process.statecontext"
	fields, err := decodeConfigObject(raw, section)
	if err != nil {
		return NativeProcessStateContextSettings{}, configError("%s must be an object", section)
	}
	if value, exists := fields["states"]; exists && strings.TrimSpace(string(value)) != "null" {
		var rawStates []json.RawMessage
		if err := json.Unmarshal(value, &rawStates); err != nil || rawStates == nil {
			return NativeProcessStateContextSettings{}, configError("%s.states must contain process states", section)
		}
		states := make([]string, 0, len(rawStates))
		seen := make(map[string]struct{}, len(rawStates))
		for _, rawState := range rawStates {
			var state string
			if err := json.Unmarshal(rawState, &state); err != nil {
				return NativeProcessStateContextSettings{}, configError("%s.states must contain process states", section)
			}
			if _, ok := allowedNativeProcessStates[state]; !ok {
				return NativeProcessStateContextSettings{}, configError("%s.states must contain process states", section)
			}
			if _, exists := seen[state]; !exists {
				states = append(states, state)
				seen[state] = struct{}{}
			}
		}
		settings.States = states
	}
	if settings.Limit, err = configuredInteger(fields, "limit", defaultNativeProcessStateLimit, 1, maxNativeProcessStateContext, settingPath, "tools.native.process.statecontext"); err != nil {
		return NativeProcessStateContextSettings{}, err
	}
	return settings, nil
}
