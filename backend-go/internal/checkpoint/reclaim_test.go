package checkpoint

import (
	"bytes"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// gitRepository runs git in the fixture's private repository, outside the
// service's own wrapper.
func gitRepository(t *testing.T, staging string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"--git-dir", filepath.Join(staging, GitDirectoryName)}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(output)
}

// objectCounts is `git count-objects -v` as name to value.
func objectCounts(t *testing.T, staging string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, line := range strings.Split(gitRepository(t, staging, "count-objects", "-v"), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		number, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			t.Fatalf("count-objects line %q: %v", line, err)
		}
		counts[name] = number
	}
	return counts
}

// reachableObjectCount is how many objects the kept checkpoints need.
func reachableObjectCount(t *testing.T, staging string) int {
	t.Helper()
	listed := strings.TrimSpace(gitRepository(t, staging, "rev-list", "--objects", "--all"))
	if listed == "" {
		return 0
	}
	return len(strings.Split(listed, "\n"))
}

// incompressible returns n bytes a pack cannot shrink, so one loose object occupies
// one block whatever the filesystem compresses.
func incompressible(seed int64, n int) string {
	source := rand.New(rand.NewSource(seed))
	data := make([]byte, n)
	_, _ = source.Read(data)
	return string(data)
}

// refusingGCGit returns a git wrapper that logs the subcommand of every call and
// fails a gc.
func refusingGCGit(t *testing.T) (binary, logPath string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binary = filepath.Join(dir, "git")
	script := "#!/bin/sh\nwhile [ \"$1\" = \"-c\" ]; do shift 2; done\n" +
		"echo \"$1\" >> '" + logPath + "'\n" +
		"if [ \"$1\" = \"gc\" ]; then echo 'gc is not allowed here' >&2; exit 1; fi\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, logPath
}

// M09: deleting the refs of pruned checkpoints frees nothing. The gc that follows
// the prune is what gives the disk back.
func TestPruningReclaimsTheDiskTheDroppedCheckpointsHeld(t *testing.T) {
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) {
		s.MaxCheckpoints = 1
		s.MaxTotalBytes = 512 * 1024
	})
	f.write(t, "a.txt", "a0\n")
	peak, previous, reclaimed := int64(0), int64(0), int64(-1)
	for turn := int64(1); turn <= 20; turn++ {
		id := f.begin(t, "turn", turn)
		f.write(t, "a.txt", incompressible(turn, 16*1024))
		f.service.Finish(id)
		f.service.reclaiming.Wait()
		measured, err := f.service.git.repositoryBytes()
		if err != nil {
			t.Fatal(err)
		}
		if measured > peak {
			peak = measured
		}
		if measured < previous {
			// This run's prune was followed by a gc: the repository must hold
			// exactly what the kept record needs, all of it packed.
			counts := objectCounts(t, f.staging)
			reachable := reachableObjectCount(t, f.staging)
			if counts["count"] != 0 || counts["in-pack"] != reachable {
				t.Fatalf("after the gc of run %d the repository holds %d loose and %d packed objects, want none loose and only the %d the kept records need",
					turn, counts["count"], counts["in-pack"], reachable)
			}
			reclaimed = measured
		}
		previous = measured
	}
	if reclaimed < 0 {
		t.Fatal("no prune was followed by a gc in 20 runs")
	}
	if reclaimed >= peak {
		t.Fatalf("the gc left %d bytes, the repository peaked at %d: the reclaim freed nothing", reclaimed, peak)
	}
}

// M09b: a gc the service runs itself must make the size gate let snapshots through
// again, in the service that pruned and in a fresh one for the same staging
// directory, because the cached size is shared by staging path. The 200 files of
// 2 KiB are about 800 KiB of loose objects (one 4 KiB block each), so a cap of
// 1 MiB sits above the live set a gc keeps and below the garbage that accumulates.
func TestAReclaimLetsTheSizeGateRecoverInTheSameAndInAFreshService(t *testing.T) {
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) {
		s.MaxTotalBytes = 1024 * 1024
	})
	for index := 0; index < 200; index++ {
		f.write(t, fmt.Sprintf("file-%03d.bin", index), incompressible(int64(index), 2048))
	}
	kept := int(f.service.options.Settings.MaxCheckpoints)
	turn := int64(0)
	for ; turn < 100; turn++ {
		id := f.service.Begin("turn", turn)
		if id == "" {
			break
		}
		f.write(t, "file-000.bin", incompressible(1000+turn, 2048))
		f.service.Finish(id)
	}
	if turn >= 100 {
		t.Fatal("the repository never reached max_total_bytes")
	}
	if records := len(f.service.List()); records >= kept {
		t.Fatalf("the gate tripped with %d records, at or over max_checkpoints (%d): a prune and its gc would have kept the repository under the cap", records, kept)
	}
	// Records keep coming while the gate refuses snapshots. The Begin past
	// max_checkpoints prunes, and the gc that follows is what lets the gate recover.
	for len(f.service.List()) < kept {
		if id := f.service.Begin("turn", turn); id != "" {
			t.Fatalf("the size gate let a snapshot through with %d bytes, over checkpoint.max_total_bytes (%d)", f.service.size.bytes, f.service.options.Settings.MaxTotalBytes)
		}
		turn++
	}
	if id := f.service.Begin("turn", turn); id != "" {
		t.Fatal("the size gate let a snapshot through before the prune")
	}
	f.service.reclaiming.Wait()
	if id := f.service.Begin("turn", turn+1); id == "" {
		t.Fatalf("the size gate still refuses after the gc, with %d bytes cached", f.service.size.bytes)
	}
	fresh := NewService(f.service.options)
	if id := fresh.Begin("turn", turn+2); id == "" {
		t.Fatalf("the size gate still refuses a fresh service after the gc, with %d bytes cached", fresh.size.bytes)
	}
}

// A prune that drops nothing asks for no gc, even when the repository is over the
// share of max_total_bytes that makes one worth it.
func TestNoReclaimWithoutADroppedRecord(t *testing.T) {
	f, logPath := loggedFixture(t, func(s *sessionfs.CheckpointSettings) {
		s.MaxCheckpoints = 20
		s.MaxTotalBytes = 8 * 1024
	})
	f.write(t, "a.txt", "a0\n")
	for turn := int64(1); turn <= 5; turn++ {
		if id := f.service.Begin("turn", turn); id != "" {
			f.write(t, "a.txt", fmt.Sprintf("a%d\n", turn))
			f.service.Finish(id)
		}
	}
	f.service.reclaiming.Wait()
	if share := f.service.options.Settings.MaxTotalBytes / reclaimSizeDivisor; f.service.size.bytes <= share {
		t.Fatalf("the repository holds %d bytes, not over the %d that make a gc worth it: the test proves nothing", f.service.size.bytes, share)
	}
	if calls := countCalls(t, logPath, "gc"); calls != 0 {
		t.Fatalf("gc ran %d times with all %d records kept, want none", calls, len(f.service.List()))
	}
}

// A gc that fails must not fail the run: the record is complete and the failure is
// logged.
func TestAFailedReclaimDoesNotFailTheRun(t *testing.T) {
	binary, logPath := refusingGCGit(t)
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) {
		s.GitBinary = binary
		s.MaxCheckpoints = 2
		s.MaxTotalBytes = 64 * 1024
	})
	var logged bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logged, nil))
	f.write(t, "a.txt", "a0\n")
	for turn := int64(1); turn <= 3; turn++ {
		id := f.begin(t, "turn", turn)
		f.write(t, "a.txt", fmt.Sprintf("a%d\n", turn))
		f.service.Finish(id)
	}
	f.service.reclaiming.Wait()
	if calls := countCalls(t, logPath, "gc"); calls == 0 {
		t.Fatal("no gc was attempted after the prune")
	}
	if !strings.Contains(logged.String(), "cannot reclaim") {
		t.Fatalf("log = %q, want the failed gc", logged.String())
	}
	records := f.service.List()
	if len(records) == 0 || records[0].State != StateComplete {
		t.Fatalf("records = %+v, want the last run recorded as complete", records)
	}
}
