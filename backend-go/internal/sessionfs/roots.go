// Package sessionfs resolves session roots and filesystem references using the
// root-setting conventions shared with the TypeScript backend.
package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"example.com/agent-harness/backend-go/internal/serverconfig"
)

var (
	rootIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

const (
	defaultRootID    = "default"
	defaultRootLabel = "Local sessions"
	maxServerPort    = 65535
	maxServerBody    = 64 * 1024 * 1024
	maxRetentionMS   = 24 * 60 * 60 * 1000
	maxSnapshotTurns = 100_000
	maxPageTurns     = 1_000
	maxInlineBytes   = 16 * 1024 * 1024
	minSSEBytes      = 1024
	maxSSEBytes      = 64 * 1024 * 1024
)

// ServerSettings mirrors the root setting fields used by the server. Some
// fields are retained now for schema compatibility with later route families.
type ServerSettings struct {
	Host                       string
	Port                       int
	MaxBodyBytes               int64
	RunStateRetention          time.Duration
	AllowZeroRunStateRetention bool
	ToolStreamInterval         time.Duration
	SSEMaxBufferedBytes        int
	// SSEHeartbeatInterval is the gap between keep-alive frames on a quiet run
	// stream; zero disables them.
	SSEHeartbeatInterval time.Duration
	// ReadHeaderTimeout is how long a client has to send complete request
	// headers; zero disables the limit.
	ReadHeaderTimeout time.Duration
	// IdleTimeout is how long an idle keep-alive connection stays open between
	// requests; zero disables the limit.
	IdleTimeout time.Duration
	// LogLevel, LogFormat and LogFile configure the Go backend's structured log.
	// LogFile is absolute (resolved against the setting file) or empty for stderr.
	LogLevel  string
	LogFormat string
	LogFile   string
	// RecoverProcessesAtStart makes the Go backend recover the managed-process storage
	// of every session when it starts, instead of at the first use of the process tool.
	RecoverProcessesAtStart bool
	// RecoverSubagentRunsAtStart does the same for the sub-agent run manifests: a run the
	// last server left running is marked lost at start, not at the first use of the tool.
	RecoverSubagentRunsAtStart bool
	ConditionalRequests        bool
	SnapshotTurns              int
	MaxPageTurns               int
	MaxInlinePartBytes         int
	RunContentMemoryBytes      int
	RunContentWriteBatchBytes  int
	RunContentFlushInterval    time.Duration
	RunContentMaxIndexSegments int
	AllowedOrigins             []string
}

// Root identifies one independent session directory and the workspace used to
// resolve its model/prompt configuration.
type Root struct {
	ID            string
	Label         string
	Directory     string
	GlobalJadRoot string
}

// Config is the filesystem and root-server configuration loaded at startup.
type Config struct {
	Roots       []Root
	Server      ServerSettings
	SettingPath string
}

// LoadOptions allows tests and embedders to provide deterministic environment
// and working-directory inputs. Empty values use the process environment and
// current working directory.
type LoadOptions struct {
	WorkingDir      string
	SessionsDir     string
	RootSettingPath string
	Environment     map[string]string
}

// ResolveErrorKind identifies a filesystem reference failure without coupling
// the session catalog to an HTTP status code.
type ResolveErrorKind string

const (
	ResolveInvalidReference ResolveErrorKind = "invalid_reference"
	ResolveRootNotFound     ResolveErrorKind = "root_not_found"
	ResolveSessionNotFound  ResolveErrorKind = "session_not_found"
	ResolveAmbiguous        ResolveErrorKind = "ambiguous_session"
)

// ResolveError is an intentional, transport-neutral session-reference error.
type ResolveError struct {
	Kind    ResolveErrorKind
	Message string
}

func (e *ResolveError) Error() string { return e.Message }

// Location is the canonical filesystem target selected for a session request.
type Location struct {
	Reference string
	SessionID string
	Root      Root
	Directory string
}

// Defaults returns the TypeScript root server defaults, converted to Go units.
func Defaults() ServerSettings {
	return ServerSettings{
		Host:                       "127.0.0.1",
		Port:                       8787,
		MaxBodyBytes:               1 << 20,
		RunStateRetention:          30 * time.Second,
		ToolStreamInterval:         time.Duration(serverconfig.DefaultToolStreamIntervalMS) * time.Millisecond,
		SSEMaxBufferedBytes:        1 << 20,
		SSEHeartbeatInterval:       time.Duration(serverconfig.DefaultSSEHeartbeatIntervalMS) * time.Millisecond,
		ReadHeaderTimeout:          time.Duration(serverconfig.DefaultReadHeaderTimeoutMS) * time.Millisecond,
		IdleTimeout:                time.Duration(serverconfig.DefaultIdleTimeoutMS) * time.Millisecond,
		LogLevel:                   serverconfig.DefaultLogLevel,
		LogFormat:                  serverconfig.DefaultLogFormat,
		RecoverProcessesAtStart:    serverconfig.DefaultRecoverProcessesAtStart,
		RecoverSubagentRunsAtStart: serverconfig.DefaultRecoverSubagentRunsAtStart,
		ConditionalRequests:        true,
		SnapshotTurns:              24,
		MaxPageTurns:               50,
		MaxInlinePartBytes:         16 * 1024,
		RunContentMemoryBytes:      serverconfig.DefaultRunContentMemoryBytes,
		RunContentWriteBatchBytes:  serverconfig.DefaultRunContentWriteBatchBytes,
		RunContentFlushInterval:    time.Duration(serverconfig.DefaultRunContentFlushIntervalMS) * time.Millisecond,
		RunContentMaxIndexSegments: serverconfig.DefaultRunContentMaxIndexSegments,
		AllowedOrigins:             []string{},
	}
}

// Load reads the selected roots and server settings. JAD_SESSIONS_DIR takes
// precedence over root settings, matching the TypeScript backend.
func Load(options LoadOptions) (Config, error) {
	cwd := options.WorkingDir
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return Config{}, fmt.Errorf("get working directory: %w", err)
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return Config{}, fmt.Errorf("resolve working directory: %w", err)
	}
	env := options.Environment
	if env == nil {
		env = processEnvironment()
	}

	sessionsDir := strings.TrimSpace(options.SessionsDir)
	if sessionsDir == "" {
		sessionsDir = strings.TrimSpace(env["JAD_SESSIONS_DIR"])
	}
	if sessionsDir != "" {
		directory := resolveFrom(cwd, sessionsDir)
		return Config{
			Roots: []Root{{
				ID:            defaultRootID,
				Label:         defaultRootLabel,
				Directory:     directory,
				GlobalJadRoot: directory,
			}},
			Server: Defaults(),
		}, nil
	}

	settingPath, err := findRootSettingPath(cwd, options.RootSettingPath, env["JAD_ROOT_SETTING_PATH"])
	if err != nil {
		return Config{}, err
	}
	if settingPath == "" {
		roots, err := defaultSessionRoots(cwd)
		if err != nil {
			return Config{}, err
		}
		return Config{Roots: roots, Server: Defaults()}, nil
	}

	setting, err := readObject(settingPath)
	if err != nil {
		return Config{}, err
	}
	roots, err := rootsFromSetting(setting, settingPath)
	if err != nil {
		return Config{}, err
	}
	serverSettings, err := normalizeServerSettings(setting["server"], settingPath+": server")
	if err != nil {
		return Config{}, err
	}
	if serverSettings.LogFile != "" {
		serverSettings.LogFile = resolveFrom(filepath.Dir(settingPath), serverSettings.LogFile)
	}
	return Config{Roots: roots, Server: serverSettings, SettingPath: settingPath}, nil
}

// Resolve selects a root from an unqualified session ID or a root:session
// reference. An unqualified ID is ambiguous when it exists in multiple roots.
func Resolve(roots []Root, reference string) (Location, error) {
	rootID, sessionID, qualified, err := parseReference(reference)
	if err != nil {
		return Location{}, err
	}
	if len(roots) == 0 {
		return Location{}, &ResolveError{Kind: ResolveRootNotFound, Message: "session root not found"}
	}
	if qualified {
		for _, root := range roots {
			if root.ID == rootID {
				return makeLocation(root, sessionID, len(roots)), nil
			}
		}
		return Location{}, &ResolveError{Kind: ResolveRootNotFound, Message: "session root not found"}
	}
	if len(roots) == 1 {
		return makeLocation(roots[0], sessionID, len(roots)), nil
	}

	var matches []Root
	for _, root := range roots {
		entries, readErr := os.ReadDir(root.Directory)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return Location{}, fmt.Errorf("read session root %q: %w", root.ID, readErr)
		}
		for _, entry := range entries {
			if entry.IsDir() && entry.Name() == sessionID {
				matches = append(matches, root)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return Location{}, &ResolveError{Kind: ResolveSessionNotFound, Message: "session not found"}
	case 1:
		return makeLocation(matches[0], sessionID, len(roots)), nil
	default:
		return Location{}, &ResolveError{Kind: ResolveAmbiguous, Message: "session reference must include a root id"}
	}
}

func makeLocation(root Root, sessionID string, rootCount int) Location {
	reference := sessionID
	if rootCount > 1 {
		reference = root.ID + ":" + sessionID
	}
	return Location{
		Reference: reference,
		SessionID: sessionID,
		Root:      root,
		Directory: filepath.Join(root.Directory, sessionID),
	}
}

func parseReference(value string) (rootID, sessionID string, qualified bool, err error) {
	parts := strings.Split(value, ":")
	if len(parts) == 1 {
		sessionID = parts[0]
	} else if len(parts) == 2 {
		rootID, sessionID, qualified = parts[0], parts[1], true
		if !rootIDPattern.MatchString(rootID) {
			return "", "", false, &ResolveError{Kind: ResolveInvalidReference, Message: "invalid session root id"}
		}
	} else {
		return "", "", false, &ResolveError{Kind: ResolveInvalidReference, Message: "invalid session reference"}
	}
	if !sessionIDPattern.MatchString(sessionID) || sessionID == "." || sessionID == ".." {
		return "", "", false, &ResolveError{Kind: ResolveInvalidReference, Message: "invalid session id"}
	}
	return rootID, sessionID, qualified, nil
}

func findRootSettingPath(cwd, explicit, fromEnvironment string) (string, error) {
	configured := strings.TrimSpace(explicit)
	if configured == "" {
		configured = strings.TrimSpace(fromEnvironment)
	}
	if configured != "" {
		return resolveFrom(cwd, configured), nil
	}
	for _, candidate := range []string{
		filepath.Join(cwd, "setting.json"),
		filepath.Join(cwd, "..", "setting.json"),
	} {
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

func defaultSessionRoots(cwd string) ([]Root, error) {
	candidates := []Root{
		{Directory: filepath.Join(cwd, ".jad", "sessions"), GlobalJadRoot: cwd},
		{Directory: filepath.Join(cwd, "..", ".jad", "sessions"), GlobalJadRoot: filepath.Clean(filepath.Join(cwd, ".."))},
	}
	selected := candidates[0]
	for _, candidate := range candidates {
		info, err := os.Stat(candidate.Directory)
		if err == nil {
			if info.IsDir() {
				selected = candidate
				break
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect default session root %q: %w", candidate.Directory, err)
		}
	}
	selected.ID = defaultRootID
	selected.Label = defaultRootLabel
	return []Root{selected}, nil
}

func readObject(path string) (map[string]json.RawMessage, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: file is required", path)
		}
		return nil, fmt.Errorf("read root setting %q: %w", path, err)
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return nil, fmt.Errorf("%s: file is empty", path)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(contents, &value); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	if value == nil {
		return nil, fmt.Errorf("%s: expected a JSON object", path)
	}
	return value, nil
}

func rootsFromSetting(setting map[string]json.RawMessage, settingPath string) ([]Root, error) {
	if raw, exists := setting["schema_version"]; exists {
		version, ok := configSafeInteger(raw)
		if !ok || version != 1 {
			return nil, fmt.Errorf("%s: unsupported schema_version %s", settingPath, strings.TrimSpace(string(raw)))
		}
	}
	rootsRaw, hasRoots := setting["session_roots"]
	pathsRaw, hasPaths := setting["session_paths"]
	if hasRoots && hasPaths {
		return nil, fmt.Errorf("%s: use only one of session_roots or session_paths", settingPath)
	}
	if !hasRoots {
		rootsRaw = pathsRaw
	}
	var entries []json.RawMessage
	if !hasRoots && !hasPaths || json.Unmarshal(rootsRaw, &entries) != nil || entries == nil {
		return nil, fmt.Errorf("%s: session_roots must be an array", settingPath)
	}
	settingDir := filepath.Dir(settingPath)
	roots := make([]Root, 0, len(entries))
	ids := make(map[string]struct{}, len(entries))
	directories := make(map[string]struct{}, len(entries))
	for index, raw := range entries {
		entryPath := fmt.Sprintf("%s: session_roots[%d]", settingPath, index)
		entry, err := parseRootEntry(raw, index, entryPath)
		if err != nil {
			return nil, err
		}
		if _, exists := ids[entry.id]; exists {
			return nil, fmt.Errorf("%s: duplicate session root id %s", settingPath, entry.id)
		}
		directory := resolveFrom(settingDir, entry.path)
		if _, exists := directories[directory]; exists {
			continue
		}
		ids[entry.id] = struct{}{}
		directories[directory] = struct{}{}
		roots = append(roots, Root{
			ID:            entry.id,
			Label:         entry.label,
			Directory:     directory,
			GlobalJadRoot: settingDir,
		})
	}
	return roots, nil
}

type rootEntry struct{ id, label, path string }

func parseRootEntry(raw json.RawMessage, index int, entryPath string) (rootEntry, error) {
	var path string
	var id, label *string
	if err := json.Unmarshal(raw, &path); err != nil {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return rootEntry{}, fmt.Errorf("%s must be a path string or object", entryPath)
		}
		if err := json.Unmarshal(fields["path"], &path); err != nil {
			return rootEntry{}, fmt.Errorf("%s: path must be a non-empty string", entryPath)
		}
		if rawID, ok := fields["id"]; ok {
			var value string
			if err := json.Unmarshal(rawID, &value); err != nil || strings.TrimSpace(value) == "" {
				return rootEntry{}, fmt.Errorf("%s: id must be a non-empty string", entryPath)
			}
			value = strings.TrimSpace(value)
			id = &value
		}
		if rawLabel, ok := fields["label"]; ok {
			var value string
			if err := json.Unmarshal(rawLabel, &value); err != nil || strings.TrimSpace(value) == "" {
				return rootEntry{}, fmt.Errorf("%s: label must be a non-empty string", entryPath)
			}
			value = strings.TrimSpace(value)
			label = &value
		}
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return rootEntry{}, fmt.Errorf("%s: path must be a non-empty string", entryPath)
	}
	if id == nil {
		value := fmt.Sprintf("root-%d", index+1)
		id = &value
	}
	if !rootIDPattern.MatchString(*id) {
		return rootEntry{}, fmt.Errorf("%s: id must match %s", entryPath, rootIDPattern.String())
	}
	if label == nil {
		label = id
	}
	return rootEntry{id: *id, label: *label, path: path}, nil
}

func normalizeServerSettings(raw json.RawMessage, path string) (ServerSettings, error) {
	settings := Defaults()
	if len(raw) == 0 {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ServerSettings{}, fmt.Errorf("%s: server must be an object", path)
	}
	readString := func(name string, target *string) error {
		value, ok := fields[name]
		if !ok {
			return nil
		}
		var parsed string
		if err := json.Unmarshal(value, &parsed); err != nil || strings.TrimSpace(parsed) == "" {
			return fmt.Errorf("%s: server.%s must be a non-empty string", path, name)
		}
		*target = strings.TrimSpace(parsed)
		return nil
	}
	readInt := func(name string, target *int64, minimum, maximum int64) error {
		value, ok := fields[name]
		if !ok || string(value) == "null" {
			return nil
		}
		parsed, valid := configSafeInteger(value)
		if !valid || parsed < minimum || parsed > maximum {
			return fmt.Errorf("%s: server.%s must be an integer between %d and %d", path, name, minimum, maximum)
		}
		*target = parsed
		return nil
	}
	if err := readString("host", &settings.Host); err != nil {
		return ServerSettings{}, err
	}
	port := int64(settings.Port)
	if err := readInt("port", &port, 1, maxServerPort); err != nil {
		return ServerSettings{}, err
	}
	settings.Port = int(port)
	maxBody := settings.MaxBodyBytes
	if err := readInt("max_body_bytes", &maxBody, 1, maxServerBody); err != nil {
		return ServerSettings{}, err
	}
	settings.MaxBodyBytes = maxBody
	retention := int64(settings.RunStateRetention / time.Millisecond)
	if err := readInt("run_state_retention_ms", &retention, 0, maxRetentionMS); err != nil {
		return ServerSettings{}, err
	}
	settings.RunStateRetention = time.Duration(retention) * time.Millisecond
	if configured, ok := fields["run_state_retention_ms"]; ok && string(configured) != "null" && retention == 0 {
		settings.AllowZeroRunStateRetention = true
	}
	toolInterval := int64(settings.ToolStreamInterval / time.Millisecond)
	if err := readInt("tool_stream_interval_ms", &toolInterval, 0, serverconfig.MaxToolStreamIntervalMS); err != nil {
		return ServerSettings{}, err
	}
	settings.ToolStreamInterval = time.Duration(toolInterval) * time.Millisecond
	sseBytes := int64(settings.SSEMaxBufferedBytes)
	if err := readInt("sse_max_buffered_bytes", &sseBytes, minSSEBytes, maxSSEBytes); err != nil {
		return ServerSettings{}, err
	}
	settings.SSEMaxBufferedBytes = int(sseBytes)
	heartbeat := int64(settings.SSEHeartbeatInterval / time.Millisecond)
	if err := readInt("sse_heartbeat_interval_ms", &heartbeat, 0, serverconfig.MaxSSEHeartbeatIntervalMS); err != nil {
		return ServerSettings{}, err
	}
	settings.SSEHeartbeatInterval = time.Duration(heartbeat) * time.Millisecond
	readHeaderTimeout := int64(settings.ReadHeaderTimeout / time.Millisecond)
	if err := readInt("read_header_timeout_ms", &readHeaderTimeout, 0, serverconfig.MaxReadHeaderTimeoutMS); err != nil {
		return ServerSettings{}, err
	}
	settings.ReadHeaderTimeout = time.Duration(readHeaderTimeout) * time.Millisecond
	idleTimeout := int64(settings.IdleTimeout / time.Millisecond)
	if err := readInt("idle_timeout_ms", &idleTimeout, 0, serverconfig.MaxIdleTimeoutMS); err != nil {
		return ServerSettings{}, err
	}
	settings.IdleTimeout = time.Duration(idleTimeout) * time.Millisecond
	if err := readChoice(fields, "log_level", &settings.LogLevel, serverconfig.IsLogLevel, "debug, info, warn, error or off", path); err != nil {
		return ServerSettings{}, err
	}
	if err := readChoice(fields, "log_format", &settings.LogFormat, serverconfig.IsLogFormat, "text or json", path); err != nil {
		return ServerSettings{}, err
	}
	if value, ok := fields["log_file"]; ok && string(value) != "null" {
		var parsed string
		if err := json.Unmarshal(value, &parsed); err != nil {
			return ServerSettings{}, fmt.Errorf("%s: server.log_file must be a string", path)
		}
		settings.LogFile = strings.TrimSpace(parsed)
	}
	if err := readBool(fields, "recover_processes_at_start", &settings.RecoverProcessesAtStart, path); err != nil {
		return ServerSettings{}, err
	}
	if err := readBool(fields, "recover_subagent_runs_at_start", &settings.RecoverSubagentRunsAtStart, path); err != nil {
		return ServerSettings{}, err
	}
	if err := readBool(fields, "conditional_requests", &settings.ConditionalRequests, path); err != nil {
		return ServerSettings{}, err
	}
	snapshotTurns := int64(settings.SnapshotTurns)
	if err := readInt("snapshot_turns", &snapshotTurns, 0, maxSnapshotTurns); err != nil {
		return ServerSettings{}, err
	}
	settings.SnapshotTurns = int(snapshotTurns)
	pageTurns := int64(settings.MaxPageTurns)
	if err := readInt("max_page_turns", &pageTurns, 1, maxPageTurns); err != nil {
		return ServerSettings{}, err
	}
	settings.MaxPageTurns = int(pageTurns)
	inlineBytes := int64(settings.MaxInlinePartBytes)
	if err := readInt("max_inline_part_bytes", &inlineBytes, 0, maxInlineBytes); err != nil {
		return ServerSettings{}, err
	}
	settings.MaxInlinePartBytes = int(inlineBytes)
	runContentMemoryBytes := int64(settings.RunContentMemoryBytes)
	if err := readInt("run_content_memory_bytes", &runContentMemoryBytes, 0, serverconfig.MaxRunContentMemoryBytes); err != nil {
		return ServerSettings{}, err
	}
	settings.RunContentMemoryBytes = int(runContentMemoryBytes)
	runContentWriteBatchBytes := int64(settings.RunContentWriteBatchBytes)
	if err := readInt("run_content_write_batch_bytes", &runContentWriteBatchBytes, 2, serverconfig.MaxRunContentMemoryBytes); err != nil {
		return ServerSettings{}, err
	}
	if runContentWriteBatchBytes%2 != 0 {
		return ServerSettings{}, fmt.Errorf("%s: server.run_content_write_batch_bytes must be an even integer", path)
	}
	settings.RunContentWriteBatchBytes = int(runContentWriteBatchBytes)
	runContentFlushIntervalMS := int64(settings.RunContentFlushInterval / time.Millisecond)
	if err := readInt("run_content_flush_interval_ms", &runContentFlushIntervalMS, 0, serverconfig.MaxRunContentFlushIntervalMS); err != nil {
		return ServerSettings{}, err
	}
	settings.RunContentFlushInterval = time.Duration(runContentFlushIntervalMS) * time.Millisecond
	runContentMaxIndexSegments := int64(settings.RunContentMaxIndexSegments)
	if err := readInt("run_content_max_index_segments", &runContentMaxIndexSegments, 1, serverconfig.MaxRunContentIndexSegments); err != nil {
		return ServerSettings{}, err
	}
	settings.RunContentMaxIndexSegments = int(runContentMaxIndexSegments)
	if rawOrigins, ok := fields["allowed_origins"]; ok && string(rawOrigins) != "null" {
		var origins []string
		if err := json.Unmarshal(rawOrigins, &origins); err != nil || origins == nil {
			return ServerSettings{}, fmt.Errorf("%s: server.allowed_origins must be an array of exact http(s) origins", path)
		}
		for _, origin := range origins {
			if !isExactOrigin(origin) {
				return ServerSettings{}, fmt.Errorf("%s: server.allowed_origins must be an array of exact http(s) origins", path)
			}
		}
		settings.AllowedOrigins = append([]string(nil), origins...)
	}
	return settings, nil
}

// readChoice reads a string setting that must satisfy valid. Absent and null
// keep the default already in target.
func readChoice(fields map[string]json.RawMessage, name string, target *string, valid func(string) bool, allowed, path string) error {
	value, ok := fields[name]
	if !ok || string(value) == "null" {
		return nil
	}
	var parsed string
	if err := json.Unmarshal(value, &parsed); err != nil || !valid(parsed) {
		return fmt.Errorf("%s: server.%s must be %s", path, name, allowed)
	}
	*target = parsed
	return nil
}

func readBool(fields map[string]json.RawMessage, name string, target *bool, path string) error {
	value, ok := fields[name]
	if !ok || string(value) == "null" {
		return nil
	}
	var parsed bool
	if err := json.Unmarshal(value, &parsed); err != nil {
		return fmt.Errorf("%s: server.%s must be a boolean", path, name)
	}
	*target = parsed
	return nil
}

func isExactOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	hostname := parsed.Hostname()
	if hostname == "" || !isASCIIURLHost(hostname) || hostname != strings.ToLower(hostname) || strings.ContainsAny(hostname, "%\\") {
		return false
	}
	canonicalHost := hostname
	ip := net.ParseIP(hostname)
	if ip != nil {
		canonical := ip.String()
		if hostname != canonical {
			return false
		}
		if strings.Contains(hostname, ":") {
			canonicalHost = "[" + canonical + "]"
		} else {
			canonicalHost = canonical
		}
	} else {
		if strings.HasPrefix(parsed.Host, "[") || strings.Contains(hostname, ":") || urlHostEndsInNumber(hostname) {
			return false
		}
		if strings.ContainsAny(hostname, " #%/:<>?@[\\]^|") {
			return false
		}
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return false
	}
	port := parsed.Port()
	authority := canonicalHost
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 0 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
			return false
		}
		if (parsed.Scheme == "http" && portNumber == 80) || (parsed.Scheme == "https" && portNumber == 443) {
			return false
		}
		authority = net.JoinHostPort(hostname, port)
	}
	return value == parsed.Scheme+"://"+authority
}

func isASCIIURLHost(host string) bool {
	for index := 0; index < len(host); index++ {
		if host[index] >= 0x80 {
			return false
		}
	}
	return true
}

func urlHostEndsInNumber(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	last := host[strings.LastIndex(host, ".")+1:]
	if last == "" {
		return false
	}
	decimal := true
	for index := 0; index < len(last); index++ {
		if last[index] < '0' || last[index] > '9' {
			decimal = false
			break
		}
	}
	if decimal {
		return true
	}
	if !strings.HasPrefix(last, "0x") {
		return false
	}
	for index := 2; index < len(last); index++ {
		character := last[index]
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func resolveFrom(base, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(base, value))
}

func processEnvironment() map[string]string {
	values := make(map[string]string)
	for _, pair := range os.Environ() {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}
