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
		{`{"session_roots":[{"path":"sessions"}],"server":{"run_content_write_batch_bytes":3}}`, "even integer"},
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
