package sessionfs

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// read_only_calls was removed with D3: a setting that still has it is refused by name, so a
// person who chose "trusted" learns that a host without a sandbox no longer honors read_only.
func TestExecReadOnlyCallsIsRefusedByName(t *testing.T) {
	for _, value := range []any{"trusted", "sandboxed", nil} {
		_, err := loadSetting(t, nativeTools(map[string]any{"exec": map[string]any{"read_only_calls": value}}))
		if err == nil || !strings.Contains(err.Error(), "tools.native.exec.read_only_calls was removed") || !strings.Contains(err.Error(), "delete the key") {
			t.Errorf("%v: error %v", value, err)
		}
	}
}

func TestExecReadOnlyEnforcedOnlyByBwrapOrSeatbelt(t *testing.T) {
	for name, tc := range map[string]struct {
		sandbox  *SandboxSettings
		enforced bool
	}{
		"no sandbox":      {nil, false},
		"command backend": {&SandboxSettings{Backend: SandboxBackendCommand}, false},
		"bwrap":           {&SandboxSettings{Backend: SandboxBackendBwrap}, true},
		"seatbelt":        {&SandboxSettings{Backend: SandboxBackendSeatbelt}, true},
	} {
		settings := NativeExecSettings{}
		settings.Child.Sandbox = tc.sandbox
		if settings.ReadOnlyEnforced() != tc.enforced {
			t.Errorf("%s: ReadOnlyEnforced = %v, want %v", name, settings.ReadOnlyEnforced(), tc.enforced)
		}
	}
}

// Without a sandbox that covers exec, exec carries a fallback for its read_only and plan-mode
// calls (D2): it only keeps the project read-only, so reads, the network and the credential
// files stay as they are for a call outside any sandbox. A disabled sandbox object's
// executable and plan-mode paths shape it; an enabled one that covers exec needs none.
func TestExecGetsAPlanSandboxWhereNoSandboxCoversIt(t *testing.T) {
	backend := map[string]string{"linux": SandboxBackendBwrap, "darwin": SandboxBackendSeatbelt}[runtime.GOOS]
	config, err := loadSetting(t, nativeTools(map[string]any{"exec": map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	fallback := config.NativeExec.Child.PlanSandbox
	if backend == "" {
		if fallback != nil {
			t.Fatalf("a system without bwrap or seatbelt got a fallback: %+v", fallback)
		}
		t.Skip("no bwrap or seatbelt on this system")
	}
	if config.NativeExec.Child.Sandbox != nil || fallback == nil {
		t.Fatalf("sandbox %+v, fallback %+v", config.NativeExec.Child.Sandbox, fallback)
	}
	if fallback.Backend != backend || !fallback.Network || len(fallback.HidePaths) != 1 || filepath.Base(fallback.HidePaths[0]) != "auth" || !fallback.WholeFileSystemReadable() ||
		!slices.Equal(fallback.PlanWritablePaths, []string{"{tmp}"}) || !slices.Equal(fallback.ForPlanMode().WritablePaths, []string{"{tmp}"}) {
		t.Fatalf("fallback = %+v", fallback)
	}
	if len(fallback.ReadOnlyPaths) == 0 {
		t.Fatal("the fallback does not keep the session's configuration read-only")
	}
	if config.NativeToolSmith != nil && config.NativeToolSmith.Child.PlanSandbox != nil {
		t.Fatal("tool_smith got a fallback")
	}

	config, err = loadSetting(t, map[string]any{
		"tools":   map[string]any{"native": map[string]any{"exec": map[string]any{}}},
		"sandbox": map[string]any{"enabled": false, "backend": backend, "executable": "/opt/sb", "write": map[string]any{"plan_paths": []any{"{tmp}", "{workspace}/plans"}}, "network": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	fallback = config.NativeExec.Child.PlanSandbox
	if fallback == nil || fallback.Executable != "/opt/sb" || !slices.Equal(fallback.PlanWritablePaths, []string{"{tmp}", "{workspace}/plans"}) || !fallback.Network {
		t.Fatalf("fallback from a disabled sandbox = %+v", fallback)
	}

	config, err = loadSetting(t, map[string]any{
		"tools":   map[string]any{"native": map[string]any{"exec": map[string]any{}}},
		"sandbox": map[string]any{"enabled": true, "backend": backend, "tools": []any{"exec"}},
	})
	if err != nil {
		t.Skipf("the sandbox cannot be enabled here: %v", err)
	}
	if config.NativeExec.Child.Sandbox == nil || config.NativeExec.Child.PlanSandbox != nil {
		t.Fatalf("an enabled sandbox on exec: sandbox %+v, fallback %+v", config.NativeExec.Child.Sandbox, config.NativeExec.Child.PlanSandbox)
	}

	config, err = loadSetting(t, map[string]any{
		"tools":   map[string]any{"native": map[string]any{"exec": map[string]any{}}},
		"sandbox": map[string]any{"enabled": true, "backend": backend, "tools": []any{"hooks"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeExec.Child.Sandbox != nil || config.NativeExec.Child.PlanSandbox == nil {
		t.Fatalf("a sandbox that leaves exec out: sandbox %+v, fallback %+v", config.NativeExec.Child.Sandbox, config.NativeExec.Child.PlanSandbox)
	}
}
