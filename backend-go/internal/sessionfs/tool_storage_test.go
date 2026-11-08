package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storage_dir is from the cwd when configured relative, as given when absolute, and
// under the profile's tool storage only when omitted. One rule for every tool that has it.
func TestNativeToolStorageDirResolution(t *testing.T) {
	cwd := t.TempDir()
	toolStorage := filepath.Join(cwd, ".jad", "state", "s", "dev", "coder", "tools")
	absolute := filepath.Join(t.TempDir(), "shared")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	type resolver func(storageDir, toolStorageDir string) (string, error)
	fieldsFor := func(storageDir string) map[string]json.RawMessage {
		if storageDir == "" {
			return map[string]json.RawMessage{}
		}
		raw, _ := json.Marshal(storageDir)
		return map[string]json.RawMessage{"storage_dir": raw}
	}
	direct := func(normalize func(map[string]json.RawMessage, string, string, string) (string, error)) resolver {
		return func(storageDir, toolStorageDir string) (string, error) {
			return normalize(fieldsFor(storageDir), cwd, toolStorageDir, "/session/setting.json")
		}
	}
	spill := func(section string) resolver {
		return func(storageDir, toolStorageDir string) (string, error) {
			raw := json.RawMessage(`{}`)
			if storageDir != "" {
				encoded, _ := json.Marshal(storageDir)
				raw = json.RawMessage(`{"storage_dir":` + string(encoded) + `}`)
			}
			settings, err := normalizeNativeBashSpill(raw, section, "/session/setting.json", cwd, t.TempDir(), toolStorageDir)
			return settings.StorageDir, err
		}
	}
	tools := []struct {
		name, section, fallback string
		resolve                 resolver
	}{
		{"task", "tools.native.task", defaultNativeTaskStorageDir, direct(normalizeNativeTaskStorageDir)},
		{"memory", "tools.native.memory", defaultNativeMemoryStorageDir, direct(normalizeNativeMemoryStorageDir)},
		{"subagent", "tools.native.subagent", defaultNativeSubagentStorageDir, direct(normalizeNativeSubagentStorageDir)},
		{"code_context", "tools.native.code_context", defaultNativeCodeContextStorageDir, direct(normalizeNativeCodeContextStorageDir)},
		{"bash spill", "tools.native.bash", defaultNativeBashSpillDir, spill("tools.native.bash")},
		{"exec spill", "tools.native.exec", defaultNativeExecSpillDir, spill("tools.native.exec")},
	}
	for _, tool := range tools {
		t.Run(tool.name, func(t *testing.T) {
			check := func(label, storageDir, toolStorageDir, want string) {
				t.Helper()
				got, err := tool.resolve(storageDir, toolStorageDir)
				if err != nil || got != want {
					t.Errorf("%s: storage_dir = %q, error = %v; want %q", label, got, err, want)
				}
			}
			check("explicit relative, profile", "ctx/data", toolStorage, filepath.Join(cwd, "ctx", "data"))
			check("explicit relative, no profile", "ctx/data", "", filepath.Join(cwd, "ctx", "data"))
			check("explicit relative is cleaned", "./a/../ctx", toolStorage, filepath.Join(cwd, "ctx"))
			check("absolute", absolute, toolStorage, absolute)
			check("home", "~/jad-storage-test", toolStorage, filepath.Join(home, "jad-storage-test"))
			check("omitted, profile", "", toolStorage, filepath.Join(toolStorage, tool.fallback))
			check("omitted, no profile", "", "", filepath.Join(cwd, tool.fallback))
		})
	}
}

func TestNativeToolStorageDirRejectsEmptyAndNonString(t *testing.T) {
	cwd := t.TempDir()
	for _, tool := range []string{"task", "memory", "subagent", "code_context"} {
		normalize := map[string]func(map[string]json.RawMessage, string, string, string) (string, error){
			"task": normalizeNativeTaskStorageDir, "memory": normalizeNativeMemoryStorageDir,
			"subagent": normalizeNativeSubagentStorageDir, "code_context": normalizeNativeCodeContextStorageDir,
		}[tool]
		for _, raw := range []string{`""`, `"  "`, `3`} {
			_, err := normalize(map[string]json.RawMessage{"storage_dir": json.RawMessage(raw)}, cwd, cwd, "/session/setting.json")
			want := "/session/setting.json: tools.native." + tool + ".storage_dir must be a non-empty string"
			if err == nil || err.Error() != want {
				t.Errorf("%s storage_dir %s: error = %v, want %q", tool, raw, err, want)
			}
		}
	}
	_, err := normalizeNativeBashSpill(json.RawMessage(`{"storage_dir":" "}`), "tools.native.exec", "/session/setting.json", cwd, cwd, cwd)
	if err == nil || !strings.Contains(err.Error(), "tools.native.exec.spill.storage_dir must be a non-empty string") {
		t.Errorf("exec spill empty storage_dir: error = %v", err)
	}
}

// A storage_dir that cannot be made absolute stays a config error (HTTP 422), as it was
// for the sub-agent store before the rule was shared.
func TestNativeSubagentStorageDirUnresolvableIsAConfigError(t *testing.T) {
	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Skipf("cannot remove the working directory: %v", err)
	}
	if _, err := os.Getwd(); err == nil {
		t.Skip("the working directory is still reported")
	}
	_, err := normalizeNativeSubagentStorageDir(nil, "relative-cwd", "", "/s/setting.json")
	var problem *ConfigError
	if !errors.As(err, &problem) {
		t.Fatalf("error = %T %v, want *ConfigError", err, err)
	}
	if want := "/s/setting.json: tools.native.subagent.storage_dir could not be resolved: "; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %q, want prefix %q", err.Error(), want)
	}
}
