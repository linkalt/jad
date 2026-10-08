package claudeauth

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestInspect(t *testing.T) {
	margin := 5 * time.Minute
	cases := []struct {
		name    string
		file    map[string]any
		mode    os.FileMode
		exists  bool
		problem string
		usable  bool
		exit    int
		expired bool
	}{
		{"valid", oldFile(fixedNow.Add(time.Hour)), 0o600, true, "", true, 0, false},
		{"inside the margin", oldFile(fixedNow.Add(time.Minute)), 0o600, true, "", false, 2, true},
		{"expired", oldFile(fixedNow.Add(-time.Hour)), 0o600, true, "", false, 2, true},
		{"open mode", oldFile(fixedNow.Add(time.Hour)), 0o644, true, "", true, 0, false},
		{"no access token", map[string]any{"version": 1, "expires_at": "2027-01-01T00:00:00Z"}, 0o600, true, "no access token", false, 1, false},
		{"bad expiry", map[string]any{"access_token": "a", "expires_at": "tomorrow"}, 0o600, true, "expires_at", false, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := storeIn(t)
			writeFile(t, store, c.file)
			_ = os.Chmod(store.Path, c.mode)
			report := Inspect(store, margin, fixedNow)
			if report.Exists != c.exists || report.UsableByExpiry != c.usable || report.ExitCode() != c.exit || report.Expired != c.expired ||
				!strings.Contains(report.Problem, c.problem) || report.ModeTooOpen != (c.mode == 0o644) {
				t.Fatalf("report %+v", report)
			}
		})
	}
}

func TestInspectMissingAndCorruptFile(t *testing.T) {
	store := storeIn(t)
	report := Inspect(store, 0, fixedNow)
	if report.Exists || report.ExitCode() != 1 || !strings.Contains(report.Problem, "not found") {
		t.Fatalf("missing: %+v", report)
	}
	_ = os.MkdirAll(store.Path[:strings.LastIndex(store.Path, "/")], 0o700)
	_ = os.WriteFile(store.Path, []byte("not json SECRET-VALUE"), 0o600)
	report = Inspect(store, 0, fixedNow)
	if !report.Exists || report.ExitCode() != 1 || report.Problem == "" || strings.Contains(report.Problem, "SECRET-VALUE") {
		t.Fatalf("corrupt: %+v", report)
	}
}

func TestInspectReportsAgeAndBackupAndHidesTokens(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldFile(fixedNow.Add(time.Hour)))
	_ = os.WriteFile(store.BackupPath(), []byte("{}"), 0o600)
	report := Inspect(store, 0, fixedNow)
	if !report.BackupExists || report.SinceRefresh != "1h0m0s" || !report.RefreshToken || !report.AccessToken {
		t.Fatalf("report %+v", report)
	}
}
