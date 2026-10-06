package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeNativeReadSettingsDefaultsAndOverrides(t *testing.T) {
	global := t.TempDir()
	defaults, err := normalizeNativeReadSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"read":{}}}`),
	}, global, "/session/setting.json")
	if err != nil {
		t.Fatalf("normalizeNativeReadSettings(defaults) error = %v", err)
	}
	if defaults == nil || defaults.MaxBytes != 64*1024 || defaults.MaxFileBytes != 64*1024*1024 || defaults.DefaultLimit != 250 {
		t.Fatalf("native read defaults = %+v", defaults)
	}
	if defaults.Policy.UnrestrictedPaths || len(defaults.Policy.Paths) != 0 {
		t.Fatalf("default path policy = %+v", defaults.Policy)
	}

	configured, err := normalizeNativeReadSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"read":{"max_bytes":128,"max_file_bytes":4096,"default_limit":7,"paths":{"allow":["outside"],"deny":["private"]}}}}`),
	}, global, "/session/setting.json")
	if err != nil {
		t.Fatalf("normalizeNativeReadSettings(configured) error = %v", err)
	}
	if configured.MaxBytes != 128 || configured.MaxFileBytes != 4096 || configured.DefaultLimit != 7 {
		t.Fatalf("configured native read settings = %+v", configured)
	}
	if configured.Policy.UnrestrictedPaths || len(configured.Policy.Paths) != 2 {
		t.Fatalf("configured native read path policy = %+v", configured.Policy)
	}
	if configured.Policy.Paths[0].Action != "allow" || configured.Policy.Paths[0].Path != filepath.Join(global, "outside") {
		t.Fatalf("allow path rule = %+v", configured.Policy.Paths[0])
	}
}

func TestNormalizeNativeToolRoundLimitAndDefinitionPaths(t *testing.T) {
	if got, err := normalizeMaxToolRounds(nil, "/session/setting.json"); err != nil || got != 8 {
		t.Fatalf("default max_tool_rounds = %d, error = %v", got, err)
	}
	if got, err := normalizeMaxToolRounds(json.RawMessage(`0`), "/session/setting.json"); err != nil || got != 0 {
		t.Fatalf("zero max_tool_rounds = %d, error = %v", got, err)
	}
	if got, err := normalizeMaxToolRounds(json.RawMessage(`1.0`), "/session/setting.json"); err != nil || got != 1 {
		t.Fatalf("integral decimal max_tool_rounds = %d, error = %v", got, err)
	}
	if got, err := normalizeMaxToolRounds(json.RawMessage(`null`), "/session/setting.json"); err != nil || got != 8 {
		t.Fatalf("null max_tool_rounds = %d, error = %v", got, err)
	}
	for _, raw := range []string{`-1`, `50001`, `1.5`, `"8"`} {
		if _, err := normalizeMaxToolRounds(json.RawMessage(raw), "/session/setting.json"); err == nil {
			t.Errorf("normalizeMaxToolRounds(%s) succeeded", raw)
		}
	}

	global := t.TempDir()
	paths, err := normalizeToolDefinitionPaths(json.RawMessage(`[`+`"tools/read.json","/tmp/other.json"`+`]`), global, "/session/setting.json")
	if err != nil {
		t.Fatalf("normalizeToolDefinitionPaths() error = %v", err)
	}
	if len(paths) != 2 || paths[0] != filepath.Join(global, "tools", "read.json") || paths[1] != filepath.Clean("/tmp/other.json") {
		t.Fatalf("normalized definition paths = %#v", paths)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("unexpected stat result for unresolved configured file: %v", err)
	}
}

func TestNormalizeNativeReadSettingsRejectsUnknownKeys(t *testing.T) {
	_, err := normalizeNativeReadSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"read":{"max_bytes":4,"mystery":true}}}`),
	}, t.TempDir(), "/session/setting.json")
	if err == nil {
		t.Fatal("normalizeNativeReadSettings() accepted an unknown setting")
	}
}

func TestNormalizeMaxParallelToolCalls(t *testing.T) {
	const path = "/session/setting.json"
	for _, accepted := range []struct {
		raw  string
		want int64
	}{{``, 8}, {`null`, 8}, {`1`, 1}, {`64`, 64}, {`4.0`, 4}} {
		got, err := normalizeMaxParallelToolCalls(json.RawMessage(accepted.raw), path)
		if err != nil || got != accepted.want {
			t.Errorf("normalizeMaxParallelToolCalls(%q) = %d, %v; want %d", accepted.raw, got, err, accepted.want)
		}
	}
	for _, rejected := range []string{`0`, `65`, `-1`, `1.5`, `"4"`, `true`} {
		_, err := normalizeMaxParallelToolCalls(json.RawMessage(rejected), path)
		if err == nil || !strings.Contains(err.Error(), "max_parallel_tool_calls must be a safe integer between 1 and 64") {
			t.Errorf("normalizeMaxParallelToolCalls(%s) error = %v", rejected, err)
		}
	}
}

func TestNormalizeToolCallScheduling(t *testing.T) {
	const path = "/session/setting.json"
	for _, accepted := range []struct {
		raw  string
		want string
	}{{``, "ordered"}, {`"ordered"`, "ordered"}, {`"concurrent"`, "concurrent"}} {
		got, err := normalizeToolCallScheduling(json.RawMessage(accepted.raw), path)
		if err != nil || got != accepted.want {
			t.Errorf("normalizeToolCallScheduling(%q) = %q, %v; want %q", accepted.raw, got, err, accepted.want)
		}
	}
	// Only an absent key means the default: null, other strings and other types are errors.
	for _, rejected := range []string{`null`, `"parallel"`, `""`, `1`, `true`, `"Ordered"`} {
		_, err := normalizeToolCallScheduling(json.RawMessage(rejected), path)
		if err == nil || !strings.Contains(err.Error(), `tool_call_scheduling must be "ordered" or "concurrent"`) {
			t.Errorf("normalizeToolCallScheduling(%s) error = %v", rejected, err)
		}
	}
}

func TestNormalizeMaxRunTokens(t *testing.T) {
	const path = "/session/setting.json"
	// An absent key means no cap, returned as 0.
	if got, err := normalizeMaxRunTokens(json.RawMessage(``), path); err != nil || got != 0 {
		t.Errorf("normalizeMaxRunTokens(absent) = %d, %v; want 0, nil", got, err)
	}
	for _, accepted := range []struct {
		raw  string
		want int64
	}{{`1`, 1}, {`5000`, 5000}, {`1000000000000`, 1_000_000_000_000}} {
		got, err := normalizeMaxRunTokens(json.RawMessage(accepted.raw), path)
		if err != nil || got != accepted.want {
			t.Errorf("normalizeMaxRunTokens(%s) = %d, %v; want %d", accepted.raw, got, err, accepted.want)
		}
	}
	// null is an error, like every other non-integer and out-of-range value.
	for _, rejected := range []string{`null`, `0`, `-5`, `1.5`, `"100"`, `true`, `[]`, `{}`, `1000000000001`, `9007199254740991`} {
		_, err := normalizeMaxRunTokens(json.RawMessage(rejected), path)
		if err == nil || !strings.Contains(err.Error(), "max_run_tokens must be a safe integer between 1 and 1000000000000") {
			t.Errorf("normalizeMaxRunTokens(%s) error = %v", rejected, err)
		}
	}
}
