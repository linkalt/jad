package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolLimitsRefuseExponentNotation(t *testing.T) {
	for _, raw := range []string{`1e3`, `1E3`, `1e0`, `2.5e1`} {
		if _, err := normalizeMaxToolRounds(json.RawMessage(raw), "/session/setting.json"); err == nil {
			t.Errorf("normalizeMaxToolRounds(%s) succeeded, want refused", raw)
		}
		if _, err := normalizeMaxRunTokens(json.RawMessage(raw), "/session/setting.json"); err == nil {
			t.Errorf("normalizeMaxRunTokens(%s) succeeded, want refused", raw)
		}
		if _, err := normalizeMaxParallelToolCalls(json.RawMessage(raw), "/session/setting.json"); err == nil {
			t.Errorf("normalizeMaxParallelToolCalls(%s) succeeded, want refused", raw)
		}
	}
}

// session_files.paths took over the path rules of tools.native.read.paths: the file list and
// the transcript tools read them from there.
func TestNormalizeSessionFilesPolicyDefaultsAndRules(t *testing.T) {
	global := t.TempDir()
	for _, raw := range []string{``, `null`, `{}`, `{"enabled":false}`} {
		policy, err := normalizeSessionFilesPolicy(json.RawMessage(raw), global, "/session/setting.json")
		if err != nil || policy.UnrestrictedPaths || len(policy.Paths) != 0 || policy.Paths == nil {
			t.Fatalf("session_files %q: policy = %+v, err = %v; want an empty rule list", raw, policy, err)
		}
	}
	configured, err := normalizeSessionFilesPolicy(json.RawMessage(`{"paths":{"allow":["outside"],"deny":["private"]}}`), global, "/session/setting.json")
	if err != nil {
		t.Fatalf("normalizeSessionFilesPolicy(configured) error = %v", err)
	}
	if configured.UnrestrictedPaths || len(configured.Paths) != 2 {
		t.Fatalf("configured policy = %+v", configured)
	}
	if configured.Paths[0].Action != "allow" || configured.Paths[0].Path != filepath.Join(global, "outside") {
		t.Fatalf("allow path rule = %+v", configured.Paths[0])
	}
	for _, raw := range []string{`{"paths":null}`, `{"paths":3}`, `{"paths":[{"action":"maybe","path":"x"}]}`} {
		if _, err := normalizeSessionFilesPolicy(json.RawMessage(raw), global, "/session/setting.json"); err == nil || !strings.Contains(err.Error(), "session_files.paths") {
			t.Errorf("session_files %s: error = %v, want one naming session_files.paths", raw, err)
		}
	}
	// The file list accepts paths next to its own keys, and still refuses a key it does not know.
	if _, err := normalizeSessionFileListing(json.RawMessage(`{"max_depth":3,"paths":{"deny":["private"]}}`), "/session/setting.json"); err != nil {
		t.Fatalf("session_files with paths: %v", err)
	}
	if _, err := normalizeSessionFileListing(json.RawMessage(`{"path":{"deny":["private"]}}`), "/session/setting.json"); err == nil || !strings.Contains(err.Error(), "session_files") {
		t.Fatalf("session_files.path (a typo) error = %v, want it refused", err)
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
