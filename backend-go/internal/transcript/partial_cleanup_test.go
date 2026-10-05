package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupOrphanedPartialFilesRemovesOnlyConfirmedDeadLocalCache(t *testing.T) {
	staging := t.TempDir()
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	deadPID := 1 << 30
	alive, confirmed := processAlive(deadPID)
	if !confirmed || alive {
		t.Skipf("cannot establish a dead test PID (%d): alive=%v confirmed=%v", deadPID, alive, confirmed)
	}

	makeTurn := func(name string, owner *partialOwnerRecord) string {
		t.Helper()
		dir := filepath.Join(staging, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for file, content := range map[string]string{
			"manifest.json":        `{"status":"streaming"}`,
			partialContentFilename: "content",
			partialCompactFilename: "temporary",
		} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if owner != nil {
			data, err := json.Marshal(owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, partialOwnerFilename), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	deadTurn := makeTurn("dead-turn", &partialOwnerRecord{Hostname: hostname, PID: deadPID})
	liveTurn := makeTurn("live-turn", &partialOwnerRecord{Hostname: hostname, PID: os.Getpid()})
	remoteTurn := makeTurn("remote-turn", &partialOwnerRecord{Hostname: "another-host", PID: deadPID})
	unknownTurn := makeTurn("unknown-turn", nil)

	cleanupOrphanedPartialFiles(staging)

	for _, filename := range []string{partialContentFilename, partialCompactFilename, partialOwnerFilename} {
		if _, err := os.Stat(filepath.Join(deadTurn, filename)); !os.IsNotExist(err) {
			t.Errorf("dead-owner cache file %s remains: %v", filename, err)
		}
	}
	if _, err := os.Stat(filepath.Join(deadTurn, "manifest.json")); err != nil {
		t.Errorf("dead-turn manifest was removed: %v", err)
	}
	for _, dir := range []string{liveTurn, remoteTurn, unknownTurn} {
		if _, err := os.Stat(filepath.Join(dir, partialContentFilename)); err != nil {
			t.Errorf("cache for unconfirmed owner %s was removed: %v", filepath.Base(dir), err)
		}
	}
}
