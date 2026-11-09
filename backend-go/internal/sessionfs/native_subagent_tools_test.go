package sessionfs

import (
	"strings"
	"testing"
)

// An agent's tools key is its own narrowing, written like a profile's: an array is the allow list, an
// object has allow and deny, and nothing at all leaves the whole catalog.
func TestNativeSubagentToolsKeyIsTheAgentsOwnAllowAndDeny(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{
		"none": {},
		"list": {"tools": ["read_image", "mcp:zvec:*"]},
		"object": {"tools": {"allow": ["read_image"], "deny": ["exec"]}},
		"deny_only": {"tools": {"deny": ["exec"]}},
		"object_opt_in": {"tools": {"allow": ["exec", "async"]}},
		"opt_in": {"tools": ["exec", "async"]},
		"empty": {"tools": []}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct {
		allow, deny []string
		hasAllow    bool
		unrestrict  bool
		deprecated  bool
	}{
		"none":          {nil, nil, false, true, false},
		"list":          {[]string{"read_image", "mcp:zvec:*"}, nil, true, false, false},
		"object":        {[]string{"read_image"}, []string{"exec"}, true, false, false},
		"deny_only":     {nil, []string{"exec"}, false, true, false},
		"object_opt_in": {[]string{"exec", "async"}, nil, true, false, false},
		"opt_in":        {nil, nil, false, true, true},
		"empty":         {[]string{}, nil, true, false, false},
	} {
		agent, _ := got.Agent(name)
		settings := agent.ToolSettings([]string{"task"})
		if agent.ToolsUnrestricted() != want.unrestrict || agent.ToolsListDeprecated() != want.deprecated || settings.HasAllow != want.hasAllow {
			t.Errorf("%s: unrestricted %v deprecated %v hasAllow %v, want %+v", name, agent.ToolsUnrestricted(), agent.ToolsListDeprecated(), settings.HasAllow, want)
		}
		if want.hasAllow && strings.Join(settings.Allow, ",") != strings.Join(want.allow, ",") {
			t.Errorf("%s: allow %v, want %v", name, settings.Allow, want.allow)
		}
		// The session profile's tools.deny comes first, then the agent's own.
		if wantDeny := strings.Join(append([]string{"task"}, want.deny...), ","); strings.Join(settings.Deny, ",") != wantDeny {
			t.Errorf("%s: deny %v, want %s", name, settings.Deny, wantDeny)
		}
	}
}

// A tools key that is neither an array nor a valid block is refused, naming the key.
func TestNativeSubagentToolsKeyRefusals(t *testing.T) {
	for name, test := range map[string]struct{ section, want string }{
		"not an array":        {`{"agents":{"a":{"tools":"exec"}}}`, "tools must be an array of tool names, or an object with allow and deny"},
		"object with nothing": {`{"agents":{"a":{"tools":{}}}}`, "tools needs allow or deny"},
		"unknown object key":  {`{"agents":{"a":{"tools":{"allow":["exec"],"nope":1}}}}`, "nope"},
		"bad deny entry":      {`{"agents":{"a":{"tools":{"deny":["raed"]}}}}`, "deny[0]"},
	} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(test.section), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

// A list of only the old opt-in names is ignored with a warning that names the agent.
func TestDeprecatedAgentToolsWarning(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{
		"old": {"tools": ["exec", "async"]}, "real": {"tools": ["read_image", "exec"]}, "none": {}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	warnings := deprecatedAgentToolsWarnings(got, "/s/setting.json")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "agent old") || !strings.Contains(warnings[0], "exec, async") {
		t.Fatalf("warnings = %v, want one for agent old", warnings)
	}
	if deprecatedAgentToolsWarnings(nil, "x") != nil {
		t.Fatal("a session without a subagent tool warned")
	}
}
