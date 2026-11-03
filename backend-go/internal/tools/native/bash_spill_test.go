package native

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func writeSpillFixtures(tb testing.TB, storage string, count int, age time.Duration) {
	tb.Helper()
	if err := os.MkdirAll(storage, 0o755); err != nil {
		tb.Fatal(err)
	}
	old := time.Now().Add(-age)
	for index := 0; index < count; index++ {
		stream := "stdout"
		if index%2 == 1 {
			stream = "stderr"
		}
		path := filepath.Join(storage, fmt.Sprintf("bash-call%d.%s.log", index, stream))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			tb.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			tb.Fatal(err)
		}
	}
}

func countSpillFiles(tb testing.TB, storage string) int {
	tb.Helper()
	entries, err := os.ReadDir(storage)
	if err != nil {
		tb.Fatal(err)
	}
	return len(entries)
}

func TestPlanBashSpillPrunesOncePerInterval(t *testing.T) {
	workspace := t.TempDir()
	storage := filepath.Join(workspace, "spill")
	writeSpillFixtures(t, storage, 4, 30*24*time.Hour)
	policy := sessionfs.NativeBashSpillSettings{Enabled: true, CapBytes: 1024, RetentionDays: 14, PruneIntervalSeconds: 3600, StorageDir: storage}

	planBashSpill(policy, workspace, workspace, "first")
	if got := countSpillFiles(t, storage); got != 0 {
		t.Fatalf("first plan left %d expired files, want 0", got)
	}

	writeSpillFixtures(t, storage, 4, 30*24*time.Hour)
	planBashSpill(policy, workspace, workspace, "second")
	if got := countSpillFiles(t, storage); got != 4 {
		t.Fatalf("second plan inside the interval left %d files, want the 4 new expired files untouched", got)
	}
}

func TestPruneBashSpillThrottleForgetsExpiredKeys(t *testing.T) {
	directory := t.TempDir()
	interval := time.Millisecond
	for call := 0; call < 50; call++ {
		pattern := regexp.MustCompile(fmt.Sprintf("^bash-%d\\.log$", call))
		pruneBashSpillFilesThrottled(directory, pattern.String(), func() *regexp.Regexp { return pattern }, 14, interval)
		time.Sleep(2 * interval)
	}
	bashSpillPrunes.Lock()
	size := 0
	for key := range bashSpillPrunes.last {
		if strings.HasPrefix(key, directory+"\x00") {
			size++
		}
	}
	bashSpillPrunes.Unlock()
	if size > 5 {
		t.Fatalf("throttle map holds %d keys for this directory after 50 calls with distinct patterns, want the expired ones forgotten", size)
	}
}

func TestPlanBashSpillScansEveryCommandWhenIntervalIsZero(t *testing.T) {
	workspace := t.TempDir()
	storage := filepath.Join(workspace, "spill")
	policy := sessionfs.NativeBashSpillSettings{Enabled: true, CapBytes: 1024, RetentionDays: 14, PruneIntervalSeconds: 0, StorageDir: storage}
	for round := 0; round < 3; round++ {
		writeSpillFixtures(t, storage, 4, 30*24*time.Hour)
		planBashSpill(policy, workspace, workspace, fmt.Sprintf("call%d", round))
		if got := countSpillFiles(t, storage); got != 0 {
			t.Fatalf("round %d left %d expired files, want 0", round, got)
		}
	}
}

func TestPlanBashSpillKeepsDirectoriesIndependent(t *testing.T) {
	workspace := t.TempDir()
	first := filepath.Join(workspace, "first")
	second := filepath.Join(workspace, "second")
	writeSpillFixtures(t, first, 2, 30*24*time.Hour)
	writeSpillFixtures(t, second, 2, 30*24*time.Hour)
	for _, storage := range []string{first, second} {
		planBashSpill(sessionfs.NativeBashSpillSettings{Enabled: true, CapBytes: 1024, RetentionDays: 14, PruneIntervalSeconds: 3600, StorageDir: storage}, workspace, workspace, "call")
		if got := countSpillFiles(t, storage); got != 0 {
			t.Fatalf("%s left %d expired files, want 0", storage, got)
		}
	}
}

func BenchmarkPlanBashSpillLargeDirectory(b *testing.B) {
	for _, interval := range []int64{0, 3600} {
		b.Run(fmt.Sprintf("interval_%ds", interval), func(b *testing.B) {
			workspace := b.TempDir()
			storage := filepath.Join(workspace, "spill")
			writeSpillFixtures(b, storage, 3000, time.Hour)
			policy := sessionfs.NativeBashSpillSettings{Enabled: true, CapBytes: 1024, RetentionDays: 14, PruneIntervalSeconds: interval, StorageDir: storage}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				planBashSpill(policy, workspace, workspace, "call")
			}
		})
	}
}
