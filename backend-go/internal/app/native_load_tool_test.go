package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// sessionToolNames returns the names the session's tools endpoint lists for the profile.
func sessionToolNames(t *testing.T, f subagentFixture) []string {
	t.Helper()
	status, body := f.sessionTools(t)
	if status != 200 {
		t.Fatalf("tools status = %d, body %s", status, body)
	}
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("tools body %s: %v", body, err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// load_tool is not added by the app: a profile without tools.native.load_tool has no load_tool. Its
// tools.lazy is refused at load for the missing key, so the profile never runs without load_tool.
func TestLoadToolIsAbsentWithoutItsNativeKey(t *testing.T) {
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": profileSession(map[string]any{"allow": []string{"read", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"read"}})}, false, nil, &mcpFixture{calls: &atomic.Int32{}})
	status, body := f.sessionTools(t)
	if status == http.StatusOK {
		t.Fatalf("a profile with tools.lazy and no load_tool key was not refused: %s", body)
	}
	if !strings.Contains(body, "tools.native.load_tool is not enabled") {
		t.Fatalf("refusal = %s, want the missing load_tool key named", body)
	}
}

// A profile whose allow omits load_tool is refused when it has lazy tools: load_tool is the only way to
// load them, and the profile would otherwise run with them unreachable.
func TestLoadToolHiddenByAllowRefusesALazyProfile(t *testing.T) {
	f, _ := lazyFixtureWithExtraTools(t, map[string]any{"allow": []string{"read", "mcp:fixture:lookup"}, "lazy": []string{"read"}}, nil, nil)
	status, body := f.sessionTools(t)
	if status == http.StatusOK {
		t.Fatalf("a lazy profile whose allow hides load_tool was not refused: %s", body)
	}
	if !strings.Contains(body, `hides \"load_tool\"`) {
		t.Fatalf("refusal = %s, want it to name the hidden load_tool", body)
	}
}

// With tools.native.load_tool enabled and tools.lazy set, the session lists load_tool.
func TestLoadToolIsListedWhenEnabled(t *testing.T) {
	f, _ := lazyFixture(t, []string{"read"}, nil)
	if !slices.Contains(sessionToolNames(t, f), "load_tool") {
		t.Fatal("load_tool is not listed with tools.native.load_tool enabled")
	}
}

// tools.lazy without tools.native.load_tool refuses the session at load, and the error names both.
func TestLazyWithoutLoadToolRefusesTheSession(t *testing.T) {
	profileTools := map[string]any{"allow": []string{"read", "mcp:fixture:lookup"}, "lazy": []string{"read"}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": profileSession(profileTools)}, false, nil, &mcpFixture{calls: &atomic.Int32{}})
	status, body := f.sessionTools(t)
	if status == 200 {
		t.Fatalf("tools status = 200 with lazy and no load_tool; body %s", body)
	}
	for _, want := range []string{"tools.lazy", "tools.native.load_tool"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %s does not name %s", body, want)
		}
	}
}
