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
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": profileSession(map[string]any{"allow": []string{"skill", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"skill"}})}, false, nil, &mcpFixture{calls: &atomic.Int32{}})
	status, body := f.sessionTools(t)
	if status == http.StatusOK {
		t.Fatalf("a profile with tools.lazy and no load_tool key was not refused: %s", body)
	}
	if !strings.Contains(body, "tools.native.load_tool is not enabled") {
		t.Fatalf("refusal = %s, want the missing load_tool key named", body)
	}
}

// A tools.lazy entry is allowed without an allow entry, and so is load_tool while the profile has lazy
// tools: an allow that names neither still lists load_tool, keeps the lazy read unlisted until it is
// loaded, and lists it once load_tool loads it.
func TestLazyToolsNeedNoAllowEntry(t *testing.T) {
	steps := []subagentStep{{"load_tool", `{"names":["skill"]}`}}
	f, _ := lazyFixtureWithExtraTools(t, map[string]any{"allow": []string{"mcp:fixture:lookup"}, "lazy": []string{"skill"}}, nil, steps)
	status, body := f.sessionTools(t)
	if status != http.StatusOK {
		t.Fatalf("a lazy profile whose allow names neither read nor load_tool was refused: %d %s", status, body)
	}
	if names := sessionToolNames(t, f); slices.Contains(names, "skill") || !slices.Contains(names, "load_tool") {
		t.Fatalf("before a load the route lists %v, want load_tool and no unloaded skill", names)
	}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if names := sessionToolNames(t, f); !slices.Contains(names, "skill") {
		t.Fatalf("after a load the route lists %v, want skill", names)
	}
}

// deny still wins over tools.lazy: a lazy profile whose deny names load_tool is refused, since load_tool
// is the only way to load its lazy tools.
func TestLoadToolHiddenByDenyRefusesALazyProfile(t *testing.T) {
	f, _ := lazyFixtureWithExtraTools(t, map[string]any{"deny": []string{"load_tool"}, "lazy": []string{"skill"}}, nil, nil)
	status, body := f.sessionTools(t)
	if status == http.StatusOK {
		t.Fatalf("a lazy profile whose deny hides load_tool was not refused: %s", body)
	}
	if !strings.Contains(body, `hides \"load_tool\"`) || !strings.Contains(body, "tools.deny") {
		t.Fatalf("refusal = %s, want it to name the hidden load_tool and tools.deny", body)
	}
}

// With tools.native.load_tool enabled and tools.lazy set, the session lists load_tool.
func TestLoadToolIsListedWhenEnabled(t *testing.T) {
	f, _ := lazyFixture(t, []string{"skill"}, nil)
	if !slices.Contains(sessionToolNames(t, f), "load_tool") {
		t.Fatal("load_tool is not listed with tools.native.load_tool enabled")
	}
}

// The tools route lists what the model sees: an unloaded lazy tool is absent until a load lists it.
func TestLazyRouteListsOnlyTheLazyToolsTheModelSees(t *testing.T) {
	steps := []subagentStep{{"load_tool", `{"names":["skill"]}`}}
	f, _ := lazyFixture(t, []string{"skill"}, steps)
	if names := sessionToolNames(t, f); slices.Contains(names, "skill") || !slices.Contains(names, "load_tool") {
		t.Fatalf("before a load the route lists %v, want load_tool and no unloaded skill", names)
	}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if names := sessionToolNames(t, f); !slices.Contains(names, "skill") {
		t.Fatalf("after a load the route lists %v, want skill", names)
	}
}

// The same route check for an MCP lazy tool: the unloaded mcp:fixture:lookup is absent until
// load_tool loads it, and then the route lists its model-facing name.
func TestLazyRouteListsAnUnloadedMCPLazyToolOnlyAfterLoad(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{{"load_tool", `{"names":["` + name + `"]}`}}
	f, _ := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	if names := sessionToolNames(t, f); slices.Contains(names, name) || !slices.Contains(names, "load_tool") {
		t.Fatalf("before a load the route lists %v, want load_tool and no unloaded %s", names, name)
	}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if names := sessionToolNames(t, f); !slices.Contains(names, name) {
		t.Fatalf("after a load the route lists %v, want %s", names, name)
	}
}

// tools.lazy without tools.native.load_tool refuses the session at load, and the error names both.
func TestLazyWithoutLoadToolRefusesTheSession(t *testing.T) {
	profileTools := map[string]any{"allow": []string{"skill", "mcp:fixture:lookup"}, "lazy": []string{"skill"}}
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
