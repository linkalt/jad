package sessionfs

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// removedToolHints is a phrase of each removed tool's refusal that says what to use instead.
var removedToolHints = map[string]string{
	"read":        "read files with exec",
	"write":       "write files with exec",
	"edit":        "edit files with exec",
	"apply_patch": "apply patches with exec",
	"notebook":    "edit notebooks with exec",
	"grep":        "search with exec",
	"glob":        "list files with exec",
	"bash":        "move the settings to tools.native.exec",
	"process":     "becomes an async handle",
	"fetch":       "fetch pages with exec",
	"memory":      "memory was renamed to records",
}

// The tools exec replaced are refused by name under tools.native, each with what to do
// instead, also when the block is empty and when another tool stands next to it.
func TestRemovedNativeToolsAreRefusedByName(t *testing.T) {
	if len(removedToolHints) != len(removedNativeTools) {
		t.Fatalf("the loader removes %d tools, this test knows %d", len(removedNativeTools), len(removedToolHints))
	}
	for name, hint := range removedToolHints {
		t.Run(name, func(t *testing.T) {
			if containsString(nativeToolNames, name) {
				t.Fatalf("%s is still a native tool name", name)
			}
			for _, native := range []map[string]any{
				{name: map[string]any{}},
				{"exec": map[string]any{}, name: map[string]any{"max_bytes": 1}},
			} {
				_, err := loadSetting(t, nativeTools(native))
				if err == nil || !strings.Contains(err.Error(), "tools.native."+name+" was removed") || !strings.Contains(err.Error(), hint) {
					t.Fatalf("tools.native.%s: error = %v, want a removal error with %q", name, err, hint)
				}
			}
		})
	}
}

// A profile's tools list and an agent's tools list that name a removed tool say it was
// removed, instead of the generic "not a native tool".
func TestToolListsNamingARemovedToolSayItWasRemoved(t *testing.T) {
	for name, hint := range removedToolHints {
		err := checkProfileToolEntry(name)
		if err == nil || !strings.Contains(err.Error(), `"`+name+`" was removed`) || !strings.Contains(err.Error(), hint) || !strings.Contains(err.Error(), "remove it from the list") {
			t.Errorf("profile entry %s: error = %v, want a removal error with %q", name, err, hint)
		}
		_, err = normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{"tools":["exec","`+name+`"]}}}`), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), `tools[1] "`+name+`" was removed`) {
			t.Errorf("agent tools %s: error = %v, want a removal error", name, err)
		}
	}
	if err := checkProfileToolEntry("exec"); err != nil {
		t.Fatalf("exec is a native tool: %v", err)
	}
	// Through the loader: a profile whose tools.allow still names read.
	spec := layouttest.Spec{
		Native: map[string]any{"exec": map[string]any{}},
		Profiles: map[string]map[string]any{"dev/coder": {
			"models": []string{"openai/default"}, "model_default": "openai/default", "tools": map[string]any{"allow": []string{"exec", "read"}},
		}},
	}
	if _, err := loadLayoutSpec(t, spec); err == nil || !strings.Contains(err.Error(), `"read" was removed`) {
		t.Fatalf("profile tools.allow with read: error = %v, want a removal error", err)
	}
}

// An inline agent's paths narrowed its write and edit; tools.native.subagent's
// process_max_running limited a child's managed processes. Both are refused by name.
func TestSubagentPathsAndProcessMaxRunningAreRefused(t *testing.T) {
	for _, tc := range []struct{ setting, want string }{
		{`{"agents":{"a":{"tools":["exec"],"paths":{"allow":["src"]}}}}`, "agents.a.paths was removed; it narrowed the agent's write and edit"},
		{`{"agents":{"a":{"paths":null}}}`, "agents.a.paths was removed"},
		{`{"agents":{"a":{}},"process_max_running":4}`, "process_max_running was removed; it limited a child's managed processes"},
	} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(tc.setting), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.setting, err, tc.want)
		}
	}
}

// tool_smith's scripts called read, grep and glob by default; those are gone, so the
// default is no nested tool, and a list naming a removed tool says it was removed.
func TestToolSmithToolsDefaultToNoneAndRefuseRemovedTools(t *testing.T) {
	got, err := normalizeToolSmithForTest(t, `{"tool_smith":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("default tools = %#v, want none", got.Tools)
	}
	for _, name := range []string{"read", "grep", "bash", "process"} {
		_, err := normalizeToolSmithForTest(t, `{"tool_smith":{"tools":["read_image","`+name+`"]}}`)
		if err == nil || !strings.Contains(err.Error(), `[1] "`+name+`" was removed`) || !strings.Contains(err.Error(), removedToolHints[name]) {
			t.Errorf("tool_smith tools %s: error = %v, want a removal error", name, err)
		}
	}
	if got, err := normalizeToolSmithForTest(t, `{"tool_smith":{"tools":["read_image","lsp"]}}`); err != nil || len(got.Tools) != 2 {
		t.Fatalf("tool_smith tools read_image, lsp: %+v, %v", got, err)
	}
}
