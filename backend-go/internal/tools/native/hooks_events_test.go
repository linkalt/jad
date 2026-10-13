package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

func eventSettings(pre, post []sessionfs.ToolEventHook) sessionfs.HooksSettings {
	settings := hookTestSettings()
	settings.PreTool, settings.PostTool = pre, post
	settings.MaxPayloadBytes = 4096
	return settings
}

func eventHook(name, match, script string) sessionfs.ToolEventHook {
	return sessionfs.ToolEventHook{Name: name, Match: match, Argv: []string{"sh", "-c", script}}
}

func quietEventHook(name, match, script string) sessionfs.ToolEventHook {
	hook := eventHook(name, match, script)
	hook.OnFailureOnly = true
	return hook
}

func recordingTool(name, id, source string) (tools.Tool, *int) {
	calls := 0
	tool := tools.Tool{
		Definition: tools.Definition{ID: id, Source: source, Name: name},
		Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
			calls++
			return `{"id":"c","status":"ok","summary":"done"}` + "\n", nil
		},
	}
	return tool, &calls
}

func runEventTool(t *testing.T, tool tools.Tool, root string, arguments map[string]any) string {
	t.Helper()
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "c1", WorkspaceRoot: root, AgentName: "scout"})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func TestToolEventHooksFireOncePerCallAndSeeThePayload(t *testing.T) {
	root := t.TempDir()
	pre := filepath.Join(root, "pre.json")
	post := filepath.Join(root, "post.json")
	counter := filepath.Join(root, "count")
	settings := eventSettings(
		[]sessionfs.ToolEventHook{quietEventHook("before", "*", `cat > `+pre+`; echo x >> `+counter)},
		[]sessionfs.ToolEventHook{quietEventHook("after", "grep", `cat > `+post+`; echo x >> `+counter)},
	)
	tool, calls := recordingTool("grep", "grep", "native")
	output := runEventTool(t, WithToolEventHooks(tool, settings, ""), root, map[string]any{"pattern": "needle"})
	if *calls != 1 {
		t.Fatalf("tool ran %d times", *calls)
	}
	data, _ := os.ReadFile(counter)
	if strings.Count(string(data), "x") != 2 {
		t.Fatalf("each event must fire once per call, counter = %q", data)
	}
	var before, after toolEventPayload
	for path, into := range map[string]*toolEventPayload{pre: &before, post: &after} {
		raw, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(raw, into) != nil {
			t.Fatalf("payload %s = %q, %v", path, raw, err)
		}
	}
	if before.Event != "pre_tool" || before.Tool != "grep" || before.CallID != "c1" || before.Agent != "scout" || string(before.Arguments) != `{"pattern":"needle"}` || before.Result != "" {
		t.Errorf("pre payload = %+v", before)
	}
	if after.Event != "post_tool" || after.Status != "ok" || !strings.Contains(after.Result, `"summary":"done"`) {
		t.Errorf("post payload = %+v", after)
	}
	if strings.Contains(output, "hooks for") {
		t.Errorf("a passing on_failure_only command must add no report:\n%s", output)
	}
}

func TestToolEventHooksAppendTheReportAndKeepTheStatus(t *testing.T) {
	root := t.TempDir()
	settings := eventSettings(
		[]sessionfs.ToolEventHook{eventHook("gate", "*", `echo before-note; exit 3`)},
		[]sessionfs.ToolEventHook{eventHook("audit", "*", `echo after-note`)},
	)
	tool, calls := recordingTool("read", "read", "native")
	output := runEventTool(t, WithToolEventHooks(tool, settings, ""), root, map[string]any{})
	if *calls != 1 {
		t.Fatalf("a failing pre_tool command must not block the call, calls = %d", *calls)
	}
	for _, want := range []string{`"status":"ok"`, "pre-tool hooks for read", "[gate] failed, exit 3", "before-note", "post-tool hooks for read", "[audit] passed", "after-note"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
}

func TestToolEventHooksMatchLikeToolsAllow(t *testing.T) {
	root := t.TempDir()
	mark := filepath.Join(root, "ran")
	script := `echo "$0" >> ` + mark
	cases := []struct {
		match, id, name, source string
		want                    bool
	}{
		{"*", "x", "anything", "native", true},
		{"read", "read", "read", "native", true},
		{"read", "grep", "grep", "native", false},
		{"mcp:docs:*", "mcp:docs:search", "docs_search", "mcp", true},
		{"mcp:docs:*", "mcp:docsx:search", "docsx_search", "mcp", false},
		{"mcp:docs:search", "mcp:docs:search", "docs_search", "mcp", true},
		{"mcp:docs:search", "mcp:docs:other", "docs_other", "mcp", false},
		{"read", "mcp:read:read", "read", "mcp", false},
	}
	for _, c := range cases {
		_ = os.Remove(mark)
		tool, _ := recordingTool(c.name, c.id, c.source)
		settings := eventSettings(nil, []sessionfs.ToolEventHook{eventHook("m", c.match, script)})
		runEventTool(t, WithToolEventHooks(tool, settings, ""), root, map[string]any{})
		_, err := os.Stat(mark)
		if ran := err == nil; ran != c.want {
			t.Errorf("match %q on %s (%s): ran = %v, want %v", c.match, c.id, c.source, ran, c.want)
		}
	}
}

func TestToolEventHooksAddNothingWhenOffOrUncovered(t *testing.T) {
	root := t.TempDir()
	tool, _ := recordingTool("read", "read", "native")
	hook := []sessionfs.ToolEventHook{eventHook("n", "*", `echo never`)}
	off := eventSettings(hook, hook)
	off.Enabled = false
	if wrapped := WithToolEventHooks(tool, off, ""); runEventTool(t, wrapped, root, nil) != runEventTool(t, tool, root, nil) {
		t.Error("a disabled hook changed the output")
	}
	other := eventSettings(nil, []sessionfs.ToolEventHook{eventHook("n", "grep", `echo never`)})
	if wrapped := WithToolEventHooks(tool, other, ""); wrapped.Execute == nil || strings.Contains(runEventTool(t, wrapped, root, nil), "hooks for") {
		t.Error("an uncovered tool changed")
	}
}

func TestToolEventHooksSkipPostToolForAFailedCall(t *testing.T) {
	root := t.TempDir()
	mark := filepath.Join(root, "ran")
	tool := tools.Tool{
		Definition: tools.Definition{ID: "boom", Source: "native", Name: "boom"},
		Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
			return "", errors.New("exploded")
		},
	}
	settings := eventSettings(nil, []sessionfs.ToolEventHook{eventHook("a", "*", `echo > `+mark)})
	if _, err := WithToolEventHooks(tool, settings, "").Execute(context.Background(), nil, tools.ExecutionContext{WorkspaceRoot: root}); err == nil {
		t.Fatal("the error must pass through")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("post_tool ran after an error")
	}
}

func TestToolEventHooksWrapExecuteOutputToolsAndKeepTheirShape(t *testing.T) {
	root := t.TempDir()
	tool := tools.Tool{
		Definition: tools.Definition{ID: "mcp:docs:search", Source: "mcp", Server: "docs", Name: "docs_search"},
		ExecuteOutput: func(context.Context, map[string]any, tools.ExecutionContext) (json.RawMessage, error) {
			return json.RawMessage(`[{"type":"input_text","text":"hit"}]`), nil
		},
	}
	settings := eventSettings(nil, []sessionfs.ToolEventHook{eventHook("audit", "mcp:docs:*", `echo seen`)})
	wrapped := WithToolEventHooks(tool, settings, "")
	if wrapped.Execute != nil || wrapped.ExecuteOutput == nil {
		t.Fatal("a tool must keep exactly one executor")
	}
	output, err := wrapped.ExecuteOutput(context.Background(), nil, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	var parts []struct{ Type, Text string }
	if json.Unmarshal(output, &parts) != nil || len(parts) != 2 || parts[0].Text != "hit" || parts[1].Type != "input_text" || !strings.Contains(parts[1].Text, "post-tool hooks for docs_search") || !strings.Contains(parts[1].Text, "seen") {
		t.Fatalf("output = %s", output)
	}

	image := tools.Tool{
		Definition: tool.Definition,
		ExecuteOutput: func(context.Context, map[string]any, tools.ExecutionContext) (json.RawMessage, error) {
			return json.RawMessage(`{"odd":true}`), nil
		},
	}
	odd, _ := WithToolEventHooks(image, settings, "").ExecuteOutput(context.Background(), nil, tools.ExecutionContext{WorkspaceRoot: root})
	if string(odd) != `{"odd":true}` {
		t.Errorf("an output that is not a part list must stay as it was, got %s", odd)
	}
}

func TestToolEventPayloadStaysWithinItsBudgetWhenJSONEscapesGrowIt(t *testing.T) {
	settings := eventSettings(nil, nil)
	settings.MaxPayloadBytes = 2048
	text := strings.Repeat("<a href=\"x\">&\n", 2000)
	payload := buildToolEventPayload(settings, "post_tool", tools.Definition{ID: "read", Name: "read"}, map[string]any{"q": "<"}, tools.ExecutionContext{}, text)
	if len(payload) > 2048 {
		t.Fatalf("payload is %d bytes for a 2048-byte budget", len(payload))
	}
	var decoded toolEventPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil || !decoded.ResultCut || decoded.Result == "" || !strings.HasPrefix(text, decoded.Result) {
		t.Fatalf("payload = %s, %v", payload, err)
	}
	escaped := strings.Repeat("<", 400)
	settings.MaxPayloadBytes = 1024
	payload = buildToolEventPayload(settings, "pre_tool", tools.Definition{ID: "read", Name: "read"}, map[string]any{"q": escaped}, tools.ExecutionContext{}, "")
	if len(payload) > 1024 || !strings.Contains(payload, `"arguments_omitted":true`) {
		t.Fatalf("escaped arguments must be left out when they overflow, got %d bytes: %s", len(payload), payload)
	}
}

func TestOnlyToolEventsLeaveWriteAndEditUnwrapped(t *testing.T) {
	settings := eventSettings(nil, []sessionfs.ToolEventHook{eventHook("n", "grep", `echo never`)})
	plain, _ := recordingTool("write", "write", "native")
	wrapped := WithPostEditHooks(plain, settings, "")
	if reflect.ValueOf(wrapped.Execute).Pointer() != reflect.ValueOf(plain.Execute).Pointer() {
		t.Error("post_edit wrapped a tool though no post_edit check is configured")
	}
}

func TestToolEventPayloadStaysWithinItsBudget(t *testing.T) {
	settings := eventSettings(nil, nil)
	settings.MaxPayloadBytes = 1024
	definition := tools.Definition{ID: "read", Name: "read"}
	huge := strings.Repeat("é", 4000)
	payload := buildToolEventPayload(settings, "post_tool", definition, map[string]any{"text": huge}, tools.ExecutionContext{}, huge)
	if len(payload) > 1024+256 {
		t.Fatalf("payload is %d bytes for a 1024-byte budget", len(payload))
	}
	var decoded toolEventPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if !decoded.ArgumentsOmitted || !decoded.ResultCut || decoded.Result == "" {
		t.Errorf("payload = %+v", decoded)
	}
}
