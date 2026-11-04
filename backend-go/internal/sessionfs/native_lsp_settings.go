package sessionfs

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

const (
	defaultNativeLSPRequestTimeoutMs  int64 = 30000
	maxNativeLSPRequestTimeoutMs      int64 = 600000
	defaultNativeLSPDiagnosticsWaitMs int64 = 3000
	maxNativeLSPDiagnosticsWaitMs     int64 = 60000
	defaultNativeLSPIdleShutdownMs    int64 = 600000
	maxNativeLSPIdleShutdownMs        int64 = 86400000
	defaultNativeLSPMaxResults        int64 = 50
	maxNativeLSPMaxResults            int64 = 1000
	defaultNativeLSPMaxFileBytes      int64 = 2 << 20
	maxNativeLSPServers                     = 32
)

var nativeLSPKeys = []string{"servers", "paths", "request_timeout_ms", "diagnostics_wait_ms", "idle_shutdown_ms", "max_results", "max_file_bytes"}

var nativeLSPServerKeys = []string{"command", "extensions", "language_id", "initialization_options", "env"}

var lspServerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

var lspExtensionPattern = regexp.MustCompile(`^\.[A-Za-z0-9_+-]{1,16}$`)

// NativeLSPSettings configure native.lsp: code intelligence from language servers (gopls,
// pyright, typescript-language-server, rust-analyzer, ...) that jad starts on demand, one
// per server and workspace, and stops after IdleShutdownMs without a call.
type NativeLSPSettings struct {
	// Policy is the tool's own read rules: which files it may ask about.
	Policy  NativePathPolicy
	Servers []NativeLSPServer
	// RequestTimeoutMs bounds one request, the server's start and initialize included.
	RequestTimeoutMs int64
	// DiagnosticsWaitMs is how long a diagnostics call waits for the server to publish.
	DiagnosticsWaitMs int64
	IdleShutdownMs    int64
	MaxResults        int64
	// MaxFileBytes is the largest file sent to a server.
	MaxFileBytes int64
}

// NativeLSPServer is one language server: the program, the file extensions it serves and
// the LSP languageId of those files.
type NativeLSPServer struct {
	Name                  string
	Command               []string
	Extensions            []string
	LanguageID            string
	InitializationOptions json.RawMessage
	Env                   map[string]string
}

// ServerFor is the server whose extensions include the file's, or nil.
func (s *NativeLSPSettings) ServerFor(path string) *NativeLSPServer {
	lower := strings.ToLower(path)
	for index := range s.Servers {
		for _, extension := range s.Servers[index].Extensions {
			if strings.HasSuffix(lower, strings.ToLower(extension)) {
				return &s.Servers[index]
			}
		}
	}
	return nil
}

func normalizeNativeLSPSettings(setting map[string]json.RawMessage, globalJadRoot, settingPath string) (*NativeLSPSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil || !exists {
		return nil, err
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["lsp"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.lsp"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if !containsString(nativeLSPKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeLSPKeys, "")
		}
	}
	settings := &NativeLSPSettings{Policy: NativePathPolicy{Paths: []NativePathRule{}}}
	if rawPaths, exists := fields["paths"]; exists {
		if settings.Policy, err = normalizeNativePathPolicy(rawPaths, globalJadRoot, settingPath+": "+section+".paths"); err != nil {
			return nil, err
		}
	}
	for _, field := range []struct {
		name               string
		target             *int64
		fallback, min, max int64
	}{
		{"request_timeout_ms", &settings.RequestTimeoutMs, defaultNativeLSPRequestTimeoutMs, 100, maxNativeLSPRequestTimeoutMs},
		{"diagnostics_wait_ms", &settings.DiagnosticsWaitMs, defaultNativeLSPDiagnosticsWaitMs, 0, maxNativeLSPDiagnosticsWaitMs},
		{"idle_shutdown_ms", &settings.IdleShutdownMs, defaultNativeLSPIdleShutdownMs, 1000, maxNativeLSPIdleShutdownMs},
		{"max_results", &settings.MaxResults, defaultNativeLSPMaxResults, 1, maxNativeLSPMaxResults},
		{"max_file_bytes", &settings.MaxFileBytes, defaultNativeLSPMaxFileBytes, 1, maxNativeToolBytes},
	} {
		if *field.target, err = configuredInteger(fields, field.name, field.fallback, field.min, field.max, settingPath, section); err != nil {
			return nil, err
		}
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(fields["servers"], &servers); err != nil || len(servers) == 0 {
		return nil, configError("%s: %s.servers must name one or more language servers, for example {\"go\": {\"command\": [\"gopls\"], \"extensions\": [\".go\"], \"language_id\": \"go\"}}", settingPath, section)
	}
	if len(servers) > maxNativeLSPServers {
		return nil, configError("%s: %s.servers has %d servers; at most %d", settingPath, section, len(servers), maxNativeLSPServers)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	owner := map[string]string{}
	for _, name := range names {
		server, err := normalizeNativeLSPServer(name, servers[name], settingPath, section)
		if err != nil {
			return nil, err
		}
		for _, extension := range server.Extensions {
			key := strings.ToLower(extension)
			if other, taken := owner[key]; taken {
				return nil, configError("%s: %s.servers.%s and %s both claim %s", settingPath, section, other, name, extension)
			}
			owner[key] = name
		}
		settings.Servers = append(settings.Servers, server)
	}
	return settings, nil
}

func normalizeNativeLSPServer(name string, raw json.RawMessage, settingPath, section string) (NativeLSPServer, error) {
	prefix := section + ".servers." + name
	if !lspServerNamePattern.MatchString(name) {
		return NativeLSPServer{}, configError("%s: %s.servers: %q must be 1-64 of a-z, 0-9, _, . and -", settingPath, section, name)
	}
	fields, err := decodeConfigObject(raw, settingPath+": "+prefix)
	if err != nil {
		return NativeLSPServer{}, configError("%s: %s must be an object", settingPath, prefix)
	}
	for key := range fields {
		if !containsString(nativeLSPServerKeys, key) {
			return NativeLSPServer{}, unsupportedToolKey(settingPath+": "+prefix, key, nativeLSPServerKeys, "")
		}
	}
	server := NativeLSPServer{Name: name}
	if err := json.Unmarshal(fields["command"], &server.Command); err != nil || len(server.Command) == 0 || strings.TrimSpace(server.Command[0]) == "" {
		return NativeLSPServer{}, configError("%s: %s.command must be a non-empty array of strings (the program first), for example [\"gopls\"] or [\"pyright-langserver\", \"--stdio\"]", settingPath, prefix)
	}
	if err := json.Unmarshal(fields["extensions"], &server.Extensions); err != nil || len(server.Extensions) == 0 {
		return NativeLSPServer{}, configError("%s: %s.extensions must list one or more file extensions, for example [\".go\"]", settingPath, prefix)
	}
	for index, extension := range server.Extensions {
		if !lspExtensionPattern.MatchString(extension) {
			return NativeLSPServer{}, configError("%s: %s.extensions[%d] must be a dot and 1-16 letters, digits, _, + or -", settingPath, prefix, index)
		}
	}
	if server.LanguageID, err = bashString(fields, "language_id", "", settingPath+": "+prefix, false); err != nil || server.LanguageID == "" {
		return NativeLSPServer{}, configError("%s: %s.language_id must be the LSP language identifier of the files (go, python, typescript, rust, ...)", settingPath, prefix)
	}
	if options, exists := fields["initialization_options"]; exists && strings.TrimSpace(string(options)) != "null" {
		var object map[string]json.RawMessage
		if json.Unmarshal(options, &object) != nil || object == nil {
			return NativeLSPServer{}, configError("%s: %s.initialization_options must be an object", settingPath, prefix)
		}
		server.InitializationOptions = append(json.RawMessage(nil), options...)
	}
	if env, exists := fields["env"]; exists && strings.TrimSpace(string(env)) != "null" {
		if json.Unmarshal(env, &server.Env) != nil || server.Env == nil {
			return NativeLSPServer{}, configError("%s: %s.env must be an object of strings", settingPath, prefix)
		}
		for key := range server.Env {
			if !triggerEnvPattern.MatchString(key) {
				return NativeLSPServer{}, configError("%s: %s.env: %q is not an environment variable name", settingPath, prefix, key)
			}
		}
	}
	return server, nil
}
