package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRootsAndResolveAmbiguousReferences(t *testing.T) {
	cwd := t.TempDir()
	first := filepath.Join(cwd, "roots", "first")
	second := filepath.Join(cwd, "roots", "second")
	for _, directory := range []string{first, second} {
		if err := os.MkdirAll(filepath.Join(directory, "shared-session"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setting := `{"schema_version":1,"session_roots":[{"id":"alpha","label":"Alpha","path":"roots/first"},"roots/second"],"server":{"host":"0.0.0.0","port":8899,"max_body_bytes":4096,"run_state_retention_ms":0,"tool_stream_interval_ms":23,"sse_max_buffered_bytes":2048,"conditional_requests":false,"run_content_memory_bytes":4096,"run_content_write_batch_bytes":2048,"run_content_flush_interval_ms":12,"run_content_max_index_segments":64,"allowed_origins":["https://ui.example.test"]}}`
	if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(config.Roots) != 2 {
		t.Fatalf("Load() returned %d roots, want 2", len(config.Roots))
	}
	if config.Roots[0].ID != "alpha" || config.Roots[0].Label != "Alpha" || config.Roots[0].Directory != first {
		t.Errorf("first root = %+v", config.Roots[0])
	}
	if config.Roots[1].ID != "root-2" || config.Roots[1].Directory != second {
		t.Errorf("second root = %+v", config.Roots[1])
	}
	if config.Server.Host != "0.0.0.0" || config.Server.Port != 8899 || config.Server.MaxBodyBytes != 4096 {
		t.Errorf("server settings = %+v", config.Server)
	}
	if config.Server.RunStateRetention != 0 || !config.Server.AllowZeroRunStateRetention || config.Server.SSEMaxBufferedBytes != 2048 {
		t.Errorf("retention/SSE settings = %+v", config.Server)
	}
	if config.Server.ToolStreamInterval != 23*time.Millisecond {
		t.Errorf("tool stream interval = %s, want 23ms", config.Server.ToolStreamInterval)
	}
	if config.Server.RunContentMemoryBytes != 4096 || config.Server.RunContentWriteBatchBytes != 2048 || config.Server.RunContentFlushInterval != 12*time.Millisecond || config.Server.RunContentMaxIndexSegments != 64 {
		t.Errorf("run content settings = %+v", config.Server)
	}
	if got := config.Server.AllowedOrigins; len(got) != 1 || got[0] != "https://ui.example.test" {
		t.Errorf("allowed origins = %#v", got)
	}

	if _, err := Resolve(config.Roots, "shared-session"); err == nil {
		t.Fatal("Resolve(unqualified duplicate) succeeded, want ambiguity error")
	} else {
		var resolveErr *ResolveError
		if !asResolveError(err, &resolveErr) || resolveErr.Kind != ResolveAmbiguous {
			t.Fatalf("Resolve(unqualified duplicate) error = %v, want ambiguity ResolveError", err)
		}
		if !strings.Contains(resolveErr.Message, "include a root id") {
			t.Errorf("ambiguity message = %q", resolveErr.Message)
		}
	}
	location, err := Resolve(config.Roots, "alpha:shared-session")
	if err != nil {
		t.Fatalf("Resolve(qualified) error = %v", err)
	}
	if location.Reference != "alpha:shared-session" || location.Directory != filepath.Join(first, "shared-session") {
		t.Errorf("qualified location = %+v", location)
	}
}

func TestRunContentSettingDefaultsAndValidation(t *testing.T) {
	defaults := Defaults()
	if defaults.ToolStreamInterval != 120*time.Millisecond || defaults.RunContentMemoryBytes != 100*1024 || defaults.RunContentWriteBatchBytes != 32*1024 || defaults.RunContentFlushInterval != 25*time.Millisecond || defaults.RunContentMaxIndexSegments != 1024 {
		t.Fatalf("run content defaults = %+v", defaults)
	}

	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	invalidSettings := []struct {
		json string
		want string
	}{
		{`{"session_roots":[{"path":"sessions"}],"server":{"run_content_memory_bytes":-1}}`, "run_content_memory_bytes"},
		{`{"session_roots":[{"path":"sessions"}],"server":{"run_content_write_batch_bytes":3}}`, "server.run_content_write_batch_bytes must be an even safe integer between 2 and"},
		{`{"session_roots":[{"path":"sessions"}],"server":{"run_content_flush_interval_ms":60001}}`, "run_content_flush_interval_ms"},
		{`{"session_roots":[{"path":"sessions"}],"server":{"run_content_max_index_segments":65537}}`, "run_content_max_index_segments"},
		{`{"session_roots":[{"path":"sessions"}],"server":{"tool_stream_interval_ms":60001}}`, "tool_stream_interval_ms"},
		{`{"session_roots":[{"path":"sessions"}],"server":{"tool_stream_interval_ms":-1}}`, "tool_stream_interval_ms"},
	}
	for _, test := range invalidSettings {
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(test.json), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}}); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Load(%s) error = %v, want message containing %q", test.json, err, test.want)
		}
	}
}

// The wording of a range error for a server.* number is the TypeScript loader's, read
// from backend/dist/src/session-roots.js for each key (port says "an integer", the batch
// size "an even safe integer", every other key "a safe integer").
func TestServerNumberRangeErrorsUseTheTypeScriptWording(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key, value, want string
	}{
		{"port", "0", "server.port must be an integer between 1 and 65535"},
		{"max_body_bytes", "0", "server.max_body_bytes must be a safe integer between 1 and 67108864"},
		{"run_state_retention_ms", "-1", "server.run_state_retention_ms must be a safe integer between 0 and 86400000"},
		{"tool_stream_interval_ms", "-1", "server.tool_stream_interval_ms must be a safe integer between 0 and 60000"},
		{"run_content_memory_bytes", "-1", "server.run_content_memory_bytes must be a safe integer between 0 and 16777216"},
		{"run_content_write_batch_bytes", "3", "server.run_content_write_batch_bytes must be an even safe integer between 2 and 16777216"},
		{"run_content_write_batch_bytes", "0", "server.run_content_write_batch_bytes must be an even safe integer between 2 and 16777216"},
		{"run_content_flush_interval_ms", "-1", "server.run_content_flush_interval_ms must be a safe integer between 0 and 60000"},
		{"run_content_max_index_segments", "0", "server.run_content_max_index_segments must be a safe integer between 1 and 65536"},
		{"sse_max_buffered_bytes", "1", "server.sse_max_buffered_bytes must be a safe integer between 1024 and 67108864"},
		{"sse_heartbeat_interval_ms", "-1", "server.sse_heartbeat_interval_ms must be a safe integer between 0 and 300000"},
		{"read_header_timeout_ms", "-1", "server.read_header_timeout_ms must be a safe integer between 0 and 300000"},
		{"idle_timeout_ms", "-1", "server.idle_timeout_ms must be a safe integer between 0 and 3600000"},
		{"snapshot_turns", "-1", "server.snapshot_turns must be a safe integer between 0 and 100000"},
		{"max_page_turns", "0", "server.max_page_turns must be a safe integer between 1 and 1000"},
		{"max_inline_part_bytes", "-1", "server.max_inline_part_bytes must be a safe integer between 0 and 16777216"},
		{"max_request_images", "-1", "server.max_request_images must be a safe integer between 0 and 16"},
		{"max_request_images", "17", "server.max_request_images must be a safe integer between 0 and 16"},
		{"max_request_image_bytes", "0", "server.max_request_image_bytes must be a safe integer between 1 and 26214400"},
		{"max_request_image_bytes", "26214401", "server.max_request_image_bytes must be a safe integer between 1 and 26214400"},
		{"max_request_files", "-1", "server.max_request_files must be a safe integer between 0 and 8"},
		{"max_request_files", "9", "server.max_request_files must be a safe integer between 0 and 8"},
		{"max_request_file_bytes", "0", "server.max_request_file_bytes must be a safe integer between 1 and 26214400"},
		{"max_request_file_bytes", "26214401", "server.max_request_file_bytes must be a safe integer between 1 and 26214400"},
		{"idle_timeout_ms", "1.5", "server.idle_timeout_ms must be a safe integer between 0 and 3600000"},
		{"idle_timeout_ms", `"5"`, "server.idle_timeout_ms must be a safe integer between 0 and 3600000"},
	} {
		setting := `{"session_roots":[{"path":"sessions"}],"server":{"` + tc.key + `":` + tc.value + `}}`
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("server.%s = %s: error = %v, want it to contain %q", tc.key, tc.value, err, tc.want)
		}
	}
}

func TestLoadRootSettingAliasesDeduplicatesPathsAndHonorsSessionsDir(t *testing.T) {
	cwd := t.TempDir()
	settingPath := filepath.Join(cwd, "custom.json")
	setting := `{"session_paths":[{"id":"one","path":"sessions"},{"id":"two","path":"./sessions"}]}`
	if err := os.WriteFile(settingPath, []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(LoadOptions{
		WorkingDir:      cwd,
		RootSettingPath: "custom.json",
		Environment:     map[string]string{},
	})
	if err != nil {
		t.Fatalf("Load(session_paths) error = %v", err)
	}
	if len(config.Roots) != 1 || config.Roots[0].ID != "one" {
		t.Fatalf("deduplicated roots = %+v, want only the first path owner", config.Roots)
	}
	otherCWD := t.TempDir()
	config, err = Load(LoadOptions{
		WorkingDir: otherCWD,
		Environment: map[string]string{
			"JAD_ROOT_SETTING_PATH": settingPath,
		},
	})
	if err != nil {
		t.Fatalf("Load(JAD_ROOT_SETTING_PATH) error = %v", err)
	}
	if len(config.Roots) != 1 || config.Roots[0].Directory != filepath.Join(cwd, "sessions") {
		t.Fatalf("root-setting environment override = %+v", config.Roots)
	}

	explicit := filepath.Join(cwd, "explicit-sessions")
	config, err = Load(LoadOptions{
		WorkingDir: cwd,
		Environment: map[string]string{
			"JAD_SESSIONS_DIR":      explicit,
			"JAD_ROOT_SETTING_PATH": filepath.Join(cwd, "missing.json"),
		},
	})
	if err != nil {
		t.Fatalf("Load(JAD_SESSIONS_DIR precedence) error = %v", err)
	}
	if len(config.Roots) != 1 || config.Roots[0].ID != "default" || config.Roots[0].Directory != explicit {
		t.Fatalf("explicit sessions root = %+v", config.Roots)
	}
	if config.SettingPath != "" || config.Server.Port != Defaults().Port {
		t.Errorf("JAD_SESSIONS_DIR should bypass root settings: %+v", config)
	}
}

func TestLoadDefaultRootsAndQualifiedSingleRootReference(t *testing.T) {
	cwd := t.TempDir()
	defaultPath := filepath.Join(cwd, ".jad", "sessions")
	if err := os.MkdirAll(defaultPath, 0o755); err != nil {
		t.Fatal(err)
	}
	config, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("Load(default roots) error = %v", err)
	}
	if len(config.Roots) != 1 || config.Roots[0].Directory != defaultPath || config.Roots[0].GlobalJadRoot != cwd {
		t.Errorf("default root = %+v", config.Roots)
	}
	location, err := Resolve(config.Roots, "default:session-1")
	if err != nil {
		t.Fatalf("Resolve(root-qualified single root) error = %v", err)
	}
	if location.Reference != "session-1" {
		t.Errorf("canonical reference = %q, want unqualified session ID", location.Reference)
	}
}

func TestResolveRejectsTraversalAndUnknownRoots(t *testing.T) {
	roots := []Root{{ID: "one", Directory: t.TempDir()}}
	for _, reference := range []string{"../secret", "one:../secret", "one:two:three", "bad root:session"} {
		if _, err := Resolve(roots, reference); err == nil {
			t.Errorf("Resolve(%q) succeeded, want invalid reference error", reference)
		}
	}
	if _, err := Resolve(roots, "missing:session"); err == nil {
		t.Fatal("Resolve(unknown root) succeeded")
	} else {
		var resolveErr *ResolveError
		if !asResolveError(err, &resolveErr) || resolveErr.Kind != ResolveRootNotFound {
			t.Fatalf("unknown root error = %v, want root-not-found ResolveError", err)
		}
	}
}

func TestIsExactOriginMatchesCanonicalHTTPOrigins(t *testing.T) {
	for _, origin := range []string{
		"http://example.test",
		"https://sub.example.test:8443",
		"https://example.test.",
		"http://127.0.0.1",
		"https://[2001:db8::1]",
		"https://[2001:db8::1]:8443",
	} {
		t.Run("accept/"+origin, func(t *testing.T) {
			if !isExactOrigin(origin) {
				t.Errorf("isExactOrigin(%q) = false, want true", origin)
			}
		})
	}

	for _, origin := range []string{
		"http://example.test/",
		"http://example.test?",
		"http://example.test#",
		"http://user@example.test",
		"http://EXAMPLE.test",
		"HTTP://example.test",
		"https://example.test:443",
		"http://example.test:80",
		"https://example.test:8443/",
		"https://example.test:65536",
		"https://example.test:",
		"https://example.test\\@evil.test",
		"file:///example.test",
		"https://éxample.test",
		"https://127.1",
		"https://0177.0.0.1",
		"https://0x7f000001",
		"https://[fe80::1%25eth0]",
	} {
		t.Run("reject/"+origin, func(t *testing.T) {
			if isExactOrigin(origin) {
				t.Errorf("isExactOrigin(%q) = true, want false", origin)
			}
		})
	}
}

func TestLoadRejectsInvalidRootsAndServerSettings(t *testing.T) {
	for _, setting := range []string{
		`{"session_roots":[],"session_paths":[]}`,
		`{"session_roots":[{"id":"same","path":"a"},{"id":"same","path":"b"}]}`,
		`{"session_roots":["a"],"server":{"port":70000}}`,
		`{"session_roots":["a"],"server":null}`,
		`{"session_roots":["a"],"server":{"allowed_origins":["https://example.test/"]}}`,
		`{"schema_version":2,"session_roots":[]}`,
	} {
		t.Run(setting, func(t *testing.T) {
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}}); err == nil {
				t.Fatalf("Load(%s) succeeded, want config error", setting)
			}
		})
	}
}

func TestRootSettingRunRetentionDefaultsAndNull(t *testing.T) {
	for _, test := range []struct {
		name      string
		json      string
		want      time.Duration
		allowZero bool
	}{
		{name: "omitted", json: `{"session_roots":[]}`, want: 30 * time.Second},
		{name: "null", json: `{"session_roots":[],"server":{"run_state_retention_ms":null}}`, want: 30 * time.Second},
		{name: "zero", json: `{"session_roots":[],"server":{"run_state_retention_ms":0}}`, allowZero: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(test.json), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if config.Server.RunStateRetention != test.want || config.Server.AllowZeroRunStateRetention != test.allowZero {
				t.Errorf("retention = %s allowZero=%t, want %s/%t", config.Server.RunStateRetention, config.Server.AllowZeroRunStateRetention, test.want, test.allowZero)
			}
		})
	}
}

func asResolveError(err error, target **ResolveError) bool {
	return errors.As(err, target)
}

func TestSSEHeartbeatIntervalSettingDefaultsAndValidation(t *testing.T) {
	if got := Defaults().SSEHeartbeatInterval; got != 5*time.Second {
		t.Fatalf("default SSE heartbeat interval = %v, want 5s", got)
	}
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := func(server string) (Config, error) {
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	}
	for _, accepted := range []struct {
		server string
		want   time.Duration
	}{
		{`{}`, 5 * time.Second},
		{`{"sse_heartbeat_interval_ms":null}`, 5 * time.Second},
		{`{"sse_heartbeat_interval_ms":0}`, 0},
		{`{"sse_heartbeat_interval_ms":100}`, 100 * time.Millisecond},
		{`{"sse_heartbeat_interval_ms":300000}`, 5 * time.Minute},
	} {
		config, err := load(accepted.server)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", accepted.server, err)
		}
		if config.Server.SSEHeartbeatInterval != accepted.want {
			t.Errorf("Load(%s) heartbeat = %v, want %v", accepted.server, config.Server.SSEHeartbeatInterval, accepted.want)
		}
	}
	for _, rejected := range []string{`-1`, `300001`, `1.5`, `"5000"`} {
		_, err := load(`{"sse_heartbeat_interval_ms":` + rejected + `}`)
		if err == nil || !strings.Contains(err.Error(), "sse_heartbeat_interval_ms") {
			t.Errorf("Load(sse_heartbeat_interval_ms=%s) error = %v, want a message naming the field", rejected, err)
		}
	}
}

func TestLoadServerReadHeaderAndIdleTimeouts(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := func(server string) (Config, error) {
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	}
	for _, accepted := range []struct {
		server           string
		wantHeader, idle time.Duration
	}{
		{`{}`, 60 * time.Second, 5 * time.Second},
		{`{"read_header_timeout_ms":null,"idle_timeout_ms":null}`, 60 * time.Second, 5 * time.Second},
		{`{"read_header_timeout_ms":0,"idle_timeout_ms":0}`, 0, 0},
		{`{"read_header_timeout_ms":1500,"idle_timeout_ms":250}`, 1500 * time.Millisecond, 250 * time.Millisecond},
		{`{"read_header_timeout_ms":300000,"idle_timeout_ms":3600000}`, 5 * time.Minute, time.Hour},
	} {
		config, err := load(accepted.server)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", accepted.server, err)
		}
		if config.Server.ReadHeaderTimeout != accepted.wantHeader || config.Server.IdleTimeout != accepted.idle {
			t.Errorf("Load(%s) = header %v idle %v, want %v and %v", accepted.server, config.Server.ReadHeaderTimeout, config.Server.IdleTimeout, accepted.wantHeader, accepted.idle)
		}
	}
	for _, rejected := range []struct{ field, value string }{
		{"read_header_timeout_ms", `-1`},
		{"read_header_timeout_ms", `300001`},
		{"read_header_timeout_ms", `1.5`},
		{"read_header_timeout_ms", `"60000"`},
		{"idle_timeout_ms", `-1`},
		{"idle_timeout_ms", `3600001`},
		{"idle_timeout_ms", `1.5`},
		{"idle_timeout_ms", `"5000"`},
	} {
		_, err := load(`{"` + rejected.field + `":` + rejected.value + `}`)
		if err == nil || !strings.Contains(err.Error(), "server."+rejected.field) {
			t.Errorf("Load(%s=%s) error = %v, want a message naming the field", rejected.field, rejected.value, err)
		}
	}
}

func TestLoadServerLoggingSettings(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := func(server string) (Config, error) {
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	}
	for _, accepted := range []struct {
		server, level, format, file string
	}{
		{`{}`, "info", "text", ""},
		{`{"log_level":null,"log_format":null,"log_file":null}`, "info", "text", ""},
		{`{"log_level":"debug","log_format":"json"}`, "debug", "json", ""},
		{`{"log_level":"off"}`, "off", "text", ""},
		{`{"log_file":""}`, "info", "text", ""},
		{`{"log_file":"  "}`, "info", "text", ""},
		{`{"log_file":"logs/agent.log"}`, "info", "text", filepath.Join(cwd, "logs", "agent.log")},
		{`{"log_file":"/var/log/agent.log"}`, "info", "text", "/var/log/agent.log"},
	} {
		config, err := load(accepted.server)
		if err != nil {
			t.Errorf("Load(%s) error = %v", accepted.server, err)
			continue
		}
		got := config.Server
		if got.LogLevel != accepted.level || got.LogFormat != accepted.format || got.LogFile != accepted.file {
			t.Errorf("Load(%s) = %q/%q/%q, want %q/%q/%q", accepted.server, got.LogLevel, got.LogFormat, got.LogFile, accepted.level, accepted.format, accepted.file)
		}
	}
	for _, rejected := range []struct{ field, value string }{
		{"log_level", `"verbose"`},
		{"log_level", `"INFO"`},
		{"log_level", `""`},
		{"log_level", `1`},
		{"log_format", `"xml"`},
		{"log_format", `true`},
		{"log_file", `1`},
		{"log_file", `["a"]`},
	} {
		_, err := load(`{"` + rejected.field + `":` + rejected.value + `}`)
		if err == nil || !strings.Contains(err.Error(), "server."+rejected.field) {
			t.Errorf("Load(%s=%s) error = %v, want a message naming the field", rejected.field, rejected.value, err)
		}
	}
}

func TestLoadServerRecoverProcessesAtStart(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := func(server string) (Config, error) {
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	}
	for _, accepted := range []struct {
		server string
		want   bool
	}{
		{`{}`, true},
		{`{"recover_processes_at_start":null}`, true},
		{`{"recover_processes_at_start":true}`, true},
		{`{"recover_processes_at_start":false}`, false},
	} {
		config, err := load(accepted.server)
		if err != nil {
			t.Errorf("Load(%s) error = %v", accepted.server, err)
			continue
		}
		if config.Server.RecoverProcessesAtStart != accepted.want {
			t.Errorf("Load(%s).RecoverProcessesAtStart = %v, want %v", accepted.server, config.Server.RecoverProcessesAtStart, accepted.want)
		}
	}
	for _, rejected := range []string{`1`, `"yes"`, `[]`, `{}`} {
		_, err := load(`{"recover_processes_at_start":` + rejected + `}`)
		if err == nil || !strings.Contains(err.Error(), "server.recover_processes_at_start") {
			t.Errorf("Load(recover_processes_at_start=%s) error = %v, want a message naming the field", rejected, err)
		}
	}
}

func TestLoadServerRecoverSubagentRunsAtStart(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := func(server string) (Config, error) {
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	}
	for _, accepted := range []struct {
		server string
		want   bool
	}{
		{`{}`, true},
		{`{"recover_subagent_runs_at_start":null}`, true},
		{`{"recover_subagent_runs_at_start":true}`, true},
		{`{"recover_subagent_runs_at_start":false}`, false},
	} {
		config, err := load(accepted.server)
		if err != nil {
			t.Errorf("Load(%s) error = %v", accepted.server, err)
			continue
		}
		if config.Server.RecoverSubagentRunsAtStart != accepted.want {
			t.Errorf("Load(%s).RecoverSubagentRunsAtStart = %v, want %v", accepted.server, config.Server.RecoverSubagentRunsAtStart, accepted.want)
		}
	}
	for _, rejected := range []string{`1`, `"yes"`, `[]`, `{}`} {
		_, err := load(`{"recover_subagent_runs_at_start":` + rejected + `}`)
		if err == nil || !strings.Contains(err.Error(), "server.recover_subagent_runs_at_start") {
			t.Errorf("Load(recover_subagent_runs_at_start=%s) error = %v, want a message naming the field", rejected, err)
		}
	}
}

func TestDefaultsRecoverProcessesAtStart(t *testing.T) {
	if !Defaults().RecoverProcessesAtStart {
		t.Error("Defaults().RecoverProcessesAtStart = false, want true")
	}
	if !Defaults().RecoverSubagentRunsAtStart {
		t.Error("Defaults().RecoverSubagentRunsAtStart = false, want true")
	}
}

func TestDefaultsKeepLoggingAtInfoText(t *testing.T) {
	defaults := Defaults()
	if defaults.LogLevel != "info" || defaults.LogFormat != "text" || defaults.LogFile != "" {
		t.Errorf("Defaults() logging = %q/%q/%q", defaults.LogLevel, defaults.LogFormat, defaults.LogFile)
	}
}

// The root setting is looked for in .jad/setting.json of the working directory first, then
// in $HOME/.jad/setting.json, then in the older places.
func TestLoadFindsTheRootSettingInTheJadFolderThenHome(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	write := func(path, id string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"session_roots":[{"id":"`+id+`","path":"s"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rootID := func() string {
		t.Helper()
		config, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{"HOME": home}})
		if err != nil {
			t.Fatal(err)
		}
		return config.Roots[0].ID
	}
	write(filepath.Join(cwd, "setting.json"), "old")
	if got := rootID(); got != "old" {
		t.Fatalf("old place: root = %q", got)
	}
	write(filepath.Join(home, ".jad", "setting.json"), "home")
	if got := rootID(); got != "home" {
		t.Fatalf("home must win over the old place: root = %q", got)
	}
	write(filepath.Join(cwd, ".jad", "setting.json"), "project")
	if got := rootID(); got != "project" {
		t.Fatalf("the project file must win: root = %q", got)
	}
	// the folder of the root setting is the global root: its sessions are resolved there
	config, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, ".jad", "s"); config.Roots[0].Directory != want {
		t.Fatalf("directory = %q, want %q", config.Roots[0].Directory, want)
	}
	// an explicit path still wins
	explicit := filepath.Join(cwd, "elsewhere.json")
	write(explicit, "explicit")
	config, err = Load(LoadOptions{WorkingDir: cwd, RootSettingPath: explicit, Environment: map[string]string{"HOME": home}})
	if err != nil || config.Roots[0].ID != "explicit" {
		t.Fatalf("explicit path: %v, %+v", err, config.Roots)
	}
}
