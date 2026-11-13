package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// writeServerSetting makes cwd a workspace whose global folder is cwd+"-global" and gives it a
// global.json with the "server" section of the old-style setting text (its other keys are
// dropped), so a test can keep writing the section it exercises. A missing section leaves {}.
func writeServerSection(cwd, setting string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(setting), &fields); err != nil {
		return err
	}
	global := map[string]json.RawMessage{}
	if raw, ok := fields["server"]; ok {
		global["server"] = raw
	}
	data, _ := json.Marshal(global)
	if err := os.MkdirAll(filepath.Join(cwd, ".jad"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cwd+"-global", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cwd, ".jad", "workspace.json"), []byte(`{"schema_version":2}`), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cwd+"-global", "global.json"), data, 0o600)
}

// loadServerSetting loads the workspace writeServerSetting made.
func loadServerSection(cwd string) (Config, error) {
	return Load(LoadOptions{WorkingDir: cwd, GlobalDir: cwd + "-global", Environment: map[string]string{}})
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
		if err := writeServerSection(cwd, test.json); err != nil {
			t.Fatal(err)
		}
		if _, err := loadServerSection(cwd); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Load(%s) error = %v, want message containing %q", test.json, err, test.want)
		}
	}
}

// The wording of a range error for a server.* number is the TypeScript loader's, read
// from the removed TypeScript backend's session-roots (tag legacy-final) for each key (port says "an integer", the batch
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
		{"sse_write_timeout_ms", "-1", "server.sse_write_timeout_ms must be a safe integer between 0 and 300000"},
		{"sse_write_timeout_ms", "300001", "server.sse_write_timeout_ms must be a safe integer between 0 and 300000"},
		{"run_content_fallback_max_bytes", "1023", "server.run_content_fallback_max_bytes must be a safe integer between 1024 and 1073741824"},
		{"run_content_fallback_max_bytes", "1073741825", "server.run_content_fallback_max_bytes must be a safe integer between 1024 and 1073741824"},
		{"run_content_fallback_max_bytes", "0", "server.run_content_fallback_max_bytes must be a safe integer between 1024 and 1073741824"},
		{"sse_heartbeat_interval_ms", "-1", "server.sse_heartbeat_interval_ms must be a safe integer between 0 and 300000"},
		{"read_header_timeout_ms", "-1", "server.read_header_timeout_ms must be a safe integer between 0 and 300000"},
		{"idle_timeout_ms", "-1", "server.idle_timeout_ms must be a safe integer between 0 and 3600000"},
		{"session_delete_stop_wait_ms", "-1", "server.session_delete_stop_wait_ms must be a safe integer between 0 and 300000"},
		{"session_store_idle_ms", "-1", "server.session_store_idle_ms must be a safe integer between 0 and 86400000"},
		{"session_store_idle_ms", "86400001", "server.session_store_idle_ms must be a safe integer between 0 and 86400000"},
		{"session_store_idle_ms", `"600000"`, "server.session_store_idle_ms must be a safe integer between 0 and 86400000"},
		{"snapshot_turns", "-1", "server.snapshot_turns must be a safe integer between 0 and 100000"},
		{"max_page_turns", "0", "server.max_page_turns must be a safe integer between 1 and 1000"},
		{"max_inline_part_bytes", "-1", "server.max_inline_part_bytes must be a safe integer between 0 and 16777216"},
		{"max_request_images", "-1", "server.max_request_images must be a safe integer between 0 and 16"},
		{"max_request_images", "17", "server.max_request_images must be a safe integer between 0 and 16"},
		{"max_request_image_bytes", "0", "server.max_request_image_bytes must be a safe integer between 1 and 26214400"},
		{"max_request_image_bytes", "26214401", "server.max_request_image_bytes must be a safe integer between 1 and 26214400"},
		{"max_request_files", "-1", "server.max_request_files must be a safe integer between 0 and 8"},
		{"max_request_files", "9", "server.max_request_files must be a safe integer between 0 and 8"},
		{"max_concurrent_runs", "-1", "server.max_concurrent_runs must be a safe integer between 0 and 1024"},
		{"max_concurrent_runs", "1025", "server.max_concurrent_runs must be a safe integer between 0 and 1024"},
		{"max_leaf_calls", "0", "server.max_leaf_calls must be a safe integer between 1 and 1024"},
		{"max_leaf_calls", "1025", "server.max_leaf_calls must be a safe integer between 1 and 1024"},
		{"max_request_file_bytes", "0", "server.max_request_file_bytes must be a safe integer between 1 and 26214400"},
		{"max_request_file_bytes", "26214401", "server.max_request_file_bytes must be a safe integer between 1 and 26214400"},
		{"idle_timeout_ms", "1.5", "server.idle_timeout_ms must be a safe integer between 0 and 3600000"},
		{"idle_timeout_ms", `"5"`, "server.idle_timeout_ms must be a safe integer between 0 and 3600000"},
	} {
		setting := `{"session_roots":[{"path":"sessions"}],"server":{"` + tc.key + `":` + tc.value + `}}`
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		_, err := loadServerSection(cwd)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("server.%s = %s: error = %v, want it to contain %q", tc.key, tc.value, err, tc.want)
		}
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
		`{"session_roots":["a"],"server":{"port":70000}}`,
		`{"session_roots":["a"],"server":null}`,
		`{"session_roots":["a"],"server":{"allowed_origins":["https://example.test/"]}}`,
	} {
		t.Run(setting, func(t *testing.T) {
			cwd := t.TempDir()
			if err := writeServerSection(cwd, setting); err != nil {
				t.Fatal(err)
			}
			if _, err := loadServerSection(cwd); err == nil {
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
			if err := writeServerSection(cwd, test.json); err != nil {
				t.Fatal(err)
			}
			config, err := loadServerSection(cwd)
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
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		return loadServerSection(cwd)
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
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		return loadServerSection(cwd)
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
		{`{"shutdown_timeout_ms":300000}`, 60 * time.Second, 5 * time.Second},
		{`{"shutdown_timeout_ms":null}`, 60 * time.Second, 5 * time.Second},
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
		{"shutdown_timeout_ms", `-1`},
		{"shutdown_timeout_ms", `300001`},
		{"shutdown_timeout_ms", `1.5`},
		{"shutdown_timeout_ms", `"10000"`},
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
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		return loadServerSection(cwd)
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
		{`{"log_file":"logs/agent.log"}`, "info", "text", filepath.Join(cwd+"-global", "logs", "agent.log")},
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

// recover_processes_at_start went with tools.native.process: it is refused by name, whatever
// its value, so a stale setting says why instead of being ignored.
func TestLoadServerRecoverProcessesAtStartIsRefused(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`true`, `false`, `null`, `"yes"`} {
		setting := `{"session_roots":[{"path":"sessions"}],"server":{"recover_processes_at_start":` + value + `}}`
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		_, err := loadServerSection(cwd)
		if err == nil || !strings.Contains(err.Error(), "server.recover_processes_at_start was removed") {
			t.Errorf("recover_processes_at_start=%s: error = %v, want a removal error", value, err)
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
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		return loadServerSection(cwd)
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

func TestDefaultsRecoverSubagentRunsAtStart(t *testing.T) {
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

// A folder is not a session, so a folder named like a session file's id does not collide.
func TestLoadIgnoresFoldersThatAreNotLayoutSessions(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{SessionID: "shared", RootID: "alpha", ExtraRoots: []string{"beta"}})
	if err := os.MkdirAll(filepath.Join(fixture.RootDirs["beta"], "shared.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(LoadOptions{WorkingDir: fixture.Workspace, GlobalDir: fixture.GlobalDir, Environment: map[string]string{}}); err != nil {
		t.Fatalf("empty folder with the same name: %v", err)
	}
}

func TestSessionStoreIdleSettingDefaultsAndBounds(t *testing.T) {
	if got := Defaults().SessionStoreIdle; got != 10*time.Minute {
		t.Fatalf("default session store idle = %v, want 10m", got)
	}
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, accepted := range []struct {
		server string
		want   time.Duration
	}{
		{`{}`, 10 * time.Minute},
		{`{"session_store_idle_ms":null}`, 10 * time.Minute},
		{`{"session_store_idle_ms":0}`, 0},
		{`{"session_store_idle_ms":1500}`, 1500 * time.Millisecond},
		{`{"session_store_idle_ms":86400000}`, 24 * time.Hour},
	} {
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + accepted.server + `}`
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		config, err := loadServerSection(cwd)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", accepted.server, err)
		}
		if config.Server.SessionStoreIdle != accepted.want {
			t.Errorf("Load(%s) idle = %v, want %v", accepted.server, config.Server.SessionStoreIdle, accepted.want)
		}
	}
}

func TestSessionDeleteStopWaitSettingDefaultsAndValidation(t *testing.T) {
	if got := Defaults().SessionDeleteStopWait; got != 10*time.Second {
		t.Fatalf("default session delete stop wait = %v, want 10s", got)
	}
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := func(server string) (Config, error) {
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		return loadServerSection(cwd)
	}
	for _, accepted := range []struct {
		server string
		want   time.Duration
	}{
		{`{}`, 10 * time.Second},
		{`{"session_delete_stop_wait_ms":null}`, 10 * time.Second},
		{`{"session_delete_stop_wait_ms":0}`, 0},
		{`{"session_delete_stop_wait_ms":250}`, 250 * time.Millisecond},
		{`{"session_delete_stop_wait_ms":300000}`, 5 * time.Minute},
	} {
		config, err := load(accepted.server)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", accepted.server, err)
		}
		if config.Server.SessionDeleteStopWait != accepted.want {
			t.Errorf("Load(%s) wait = %v, want %v", accepted.server, config.Server.SessionDeleteStopWait, accepted.want)
		}
	}
	for _, rejected := range []string{`-1`, `300001`, `1.5`, `"5000"`} {
		_, err := load(`{"session_delete_stop_wait_ms":` + rejected + `}`)
		if err == nil || !strings.Contains(err.Error(), "session_delete_stop_wait_ms") {
			t.Errorf("Load(session_delete_stop_wait_ms=%s) error = %v, want a message naming the field", rejected, err)
		}
	}
}

func TestSSEWriteTimeoutAndFallbackCapDefaultsAndBounds(t *testing.T) {
	defaults := Defaults()
	if defaults.SSEWriteTimeout != 30*time.Second || defaults.RunContentFallbackMaxBytes != 64<<20 {
		t.Fatalf("defaults: write timeout %v, fallback cap %d; want 30s and 64 MiB", defaults.SSEWriteTimeout, defaults.RunContentFallbackMaxBytes)
	}
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := func(server string) (Config, error) {
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
		if err := writeServerSection(cwd, setting); err != nil {
			t.Fatal(err)
		}
		return loadServerSection(cwd)
	}
	for _, accepted := range []struct {
		server  string
		timeout time.Duration
		cap     int
	}{
		{`{}`, 30 * time.Second, 64 << 20},
		{`{"sse_write_timeout_ms":null,"run_content_fallback_max_bytes":null}`, 30 * time.Second, 64 << 20},
		{`{"sse_write_timeout_ms":0}`, 0, 64 << 20},
		{`{"sse_write_timeout_ms":250}`, 250 * time.Millisecond, 64 << 20},
		{`{"sse_write_timeout_ms":300000}`, 5 * time.Minute, 64 << 20},
		{`{"run_content_fallback_max_bytes":1024}`, 30 * time.Second, 1024},
		{`{"run_content_fallback_max_bytes":1073741824}`, 30 * time.Second, 1 << 30},
	} {
		config, err := load(accepted.server)
		if err != nil {
			t.Fatalf("Load(%s) error = %v", accepted.server, err)
		}
		if config.Server.SSEWriteTimeout != accepted.timeout || config.Server.RunContentFallbackMaxBytes != accepted.cap {
			t.Errorf("Load(%s) = %v / %d, want %v / %d", accepted.server, config.Server.SSEWriteTimeout, config.Server.RunContentFallbackMaxBytes, accepted.timeout, accepted.cap)
		}
	}
	for _, rejected := range []string{`{"sse_write_timeout_ms":1.5}`, `{"sse_write_timeout_ms":"30"}`, `{"run_content_fallback_max_bytes":"64"}`, `{"run_content_fallback_max_bytes":1.5}`} {
		if _, err := load(rejected); err == nil {
			t.Errorf("Load(%s) was accepted", rejected)
		}
	}
}
