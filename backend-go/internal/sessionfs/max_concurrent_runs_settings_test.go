package sessionfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMaxConcurrentRunsDefaultsToNoCapAndTakesBounds(t *testing.T) {
	if defaults := Defaults(); defaults.MaxConcurrentRuns != 0 {
		t.Fatalf("default = %d, want 0 (no cap)", defaults.MaxConcurrentRuns)
	}
	if got := loadServerSetting(t, `{}`); got.MaxConcurrentRuns != 0 {
		t.Fatalf("unset key = %d, want 0", got.MaxConcurrentRuns)
	}
	for _, want := range []int{0, 1, 64, 1024} {
		got := loadServerSetting(t, `{"max_concurrent_runs":`+strconv.Itoa(want)+`}`)
		if got.MaxConcurrentRuns != want {
			t.Fatalf("max_concurrent_runs %d loaded as %d", want, got.MaxConcurrentRuns)
		}
	}
}

func TestMaxConcurrentRunsRefusesNonIntegersAndReadsNullAsAbsent(t *testing.T) {
	for _, value := range []string{`"4"`, `1.5`} {
		cwd := t.TempDir()
		writeServerSetting(t, cwd, `{"max_concurrent_runs":`+value+`}`)
		_, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
		if err == nil || !strings.Contains(err.Error(), "server.max_concurrent_runs") {
			t.Fatalf("value %s: error = %v, want server.max_concurrent_runs refused", value, err)
		}
	}
	// Like every other key of the server section, null means the key is absent.
	if got := loadServerSetting(t, `{"max_concurrent_runs":null}`); got.MaxConcurrentRuns != 0 {
		t.Fatalf("null = %d, want the default 0", got.MaxConcurrentRuns)
	}
}

func writeServerSetting(t *testing.T, cwd, server string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cwd, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
	if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
}
