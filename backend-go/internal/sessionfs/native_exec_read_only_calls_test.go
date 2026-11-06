package sessionfs

import (
	"strings"
	"testing"
)

func TestExecReadOnlyCallsIsReadWithTrustedAsTheDefault(t *testing.T) {
	for name, tc := range map[string]struct {
		exec map[string]any
		want string
	}{
		"absent":    {map[string]any{}, NativeExecReadOnlyCallsTrusted},
		"null":      {map[string]any{"read_only_calls": nil}, NativeExecReadOnlyCallsTrusted},
		"trusted":   {map[string]any{"read_only_calls": "trusted"}, NativeExecReadOnlyCallsTrusted},
		"sandboxed": {map[string]any{"read_only_calls": "sandboxed"}, NativeExecReadOnlyCallsSandboxed},
	} {
		config, err := loadSetting(t, nativeTools(map[string]any{"exec": tc.exec}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := config.NativeExec.ReadOnlyCalls; got != tc.want {
			t.Errorf("%s: read_only_calls = %q, want %q", name, got, tc.want)
		}
	}
	for _, bad := range []any{"never", true, 1} {
		_, err := loadSetting(t, nativeTools(map[string]any{"exec": map[string]any{"read_only_calls": bad}}))
		if err == nil || !strings.Contains(err.Error(), `tools.native.exec.read_only_calls must be "trusted" or "sandboxed"`) {
			t.Errorf("%v: error %v", bad, err)
		}
	}
}

func TestExecReadOnlyHonoredOnlyUnderAnEnforcingSandboxWhenSandboxed(t *testing.T) {
	for name, tc := range map[string]struct {
		mode     string
		sandbox  *SandboxSettings
		enforced bool
		honored  bool
	}{
		"trusted, no sandbox":          {NativeExecReadOnlyCallsTrusted, nil, false, true},
		"trusted, bwrap":               {NativeExecReadOnlyCallsTrusted, &SandboxSettings{Backend: SandboxBackendBwrap}, true, true},
		"sandboxed, no sandbox":        {NativeExecReadOnlyCallsSandboxed, nil, false, false},
		"sandboxed, command backend":   {NativeExecReadOnlyCallsSandboxed, &SandboxSettings{Backend: SandboxBackendCommand}, false, false},
		"sandboxed, bwrap":             {NativeExecReadOnlyCallsSandboxed, &SandboxSettings{Backend: SandboxBackendBwrap}, true, true},
		"sandboxed, seatbelt":          {NativeExecReadOnlyCallsSandboxed, &SandboxSettings{Backend: SandboxBackendSeatbelt}, true, true},
		"unset mode counts as trusted": {"", nil, false, true},
	} {
		settings := NativeExecSettings{ReadOnlyCalls: tc.mode}
		settings.Child.Sandbox = tc.sandbox
		if settings.ReadOnlyEnforced() != tc.enforced || settings.ReadOnlyHonored() != tc.honored {
			t.Errorf("%s: enforced %v honored %v, want %v %v", name, settings.ReadOnlyEnforced(), settings.ReadOnlyHonored(), tc.enforced, tc.honored)
		}
	}
}

// A read_only agent would be scheduled as a reader while each of its exec calls counts as a
// change, so it is refused where read_only is not honored: from the session's own exec, or
// from the exec of the toolset the agent names.
func TestReadOnlyAgentIsRefusedWhereReadOnlyIsNotHonored(t *testing.T) {
	agent := map[string]any{"tools": []any{"exec"}, "read_only": true}
	sessionWith := func(mode string) map[string]any {
		return nativeTools(map[string]any{
			"exec":     map[string]any{"read_only_calls": mode},
			"subagent": map[string]any{"agents": map[string]any{"finder": agent}},
		})
	}
	const want = "tools.native.subagent.agents.finder.read_only is not honored: tools.native.exec.read_only_calls is \"sandboxed\""
	if _, err := loadSetting(t, sessionWith("sandboxed")); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("session exec sandboxed: error %v", err)
	}
	if _, err := loadSetting(t, sessionWith("trusted")); err != nil {
		t.Fatalf("session exec trusted: %v", err)
	}
	// A plain agent is not affected.
	plain := nativeTools(map[string]any{
		"exec":     map[string]any{"read_only_calls": "sandboxed"},
		"subagent": map[string]any{"agents": map[string]any{"fixer": map[string]any{"tools": []any{"exec"}}}},
	})
	if _, err := loadSetting(t, plain); err != nil {
		t.Fatalf("writer agent: %v", err)
	}

	toolsetAgent := map[string]any{"toolset": "extra/search", "tools": []any{"exec"}, "read_only": true}
	for mode, refused := range map[string]bool{"sandboxed": true, "trusted": false} {
		_, err := loadToolsetSession(t, toolsetSpec(toolsetAgent), map[string]string{
			"extra/search": `{"native":{"exec":{"read_only_calls":"` + mode + `"},"async":{}}}`,
		})
		if refused && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("toolset exec %s: error %v", mode, err)
		}
		if !refused && err != nil {
			t.Errorf("toolset exec %s: %v", mode, err)
		}
	}
}

// The agents a toolset itself defines are dropped when an agent uses that toolset, so they
// are not checked: only the session's own agents are.
func TestReadOnlyCheckSkipsTheAgentsOfAnAgentsToolset(t *testing.T) {
	_, err := loadToolsetSession(t, toolsetSpec(map[string]any{"toolset": "extra/search"}), map[string]string{
		"extra/search": `{"native":{"exec":{"read_only_calls":"sandboxed"},"async":{},"subagent":{"agents":{"nested":{"tools":["exec"],"read_only":true}}}}}`,
	})
	if err != nil {
		t.Fatalf("a dropped toolset agent refused the session: %v", err)
	}
}
