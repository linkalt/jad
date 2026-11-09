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
