package app

import (
	"encoding/json"
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

// load_tool is not added by the app: a profile without tools.native.load_tool has no load_tool,
// even though it has lazy tools and so has a lazy set.
func TestLoadToolIsAbsentWithoutItsNativeKey(t *testing.T) {
	f, _ := lazyFixtureWithExtraTools(t, map[string]any{"allow": []string{"read", "mcp:fixture:lookup"}, "lazy": []string{"read"}}, nil, nil)
	for _, name := range sessionToolNames(t, f) {
		if name == "load_tool" {
			t.Fatal("load_tool is listed without tools.native.load_tool")
		}
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
