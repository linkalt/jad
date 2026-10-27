package checkpoint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// loggingGit returns a git wrapper that appends the subcommand of every call to a log.
func loggingGit(t *testing.T) (binary, logPath string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binary = filepath.Join(dir, "git")
	script := "#!/bin/sh\nwhile [ \"$1\" = \"-c\" ]; do shift 2; done\necho \"$1\" >> '" + logPath + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, logPath
}

func countCalls(t *testing.T, logPath, subcommand string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == subcommand {
			count++
		}
	}
	return count
}

func loggedFixture(t *testing.T, change func(*sessionfs.CheckpointSettings)) (*fixture, string) {
	binary, logPath := loggingGit(t)
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) {
		s.GitBinary = binary
		if change != nil {
			change(s)
		}
	})
	return f, logPath
}

func TestARunMeasuresTheRepositoryOncePerCaptureAndNotBeforeEachOne(t *testing.T) {
	f, logPath := loggedFixture(t, nil)
	f.write(t, "a.txt", "one")
	for run := 0; run < 3; run++ {
		id := f.service.Begin(context.Background(), "turn", int64(run))
		if id == "" {
			t.Fatal("Begin took no checkpoint")
		}
		f.write(t, "a.txt", fmt.Sprintf("run %d", run))
		f.service.Finish(id)
	}
	// 3 runs = 6 captures; the first one also measures before gating.
	if got := countCalls(t, logPath, "count-objects"); got != 7 {
		t.Fatalf("count-objects ran %d times for 3 runs, want 7 (one per capture plus the first gate)", got)
	}
}

func TestACachedSizeStillEnforcesMaxTotalBytes(t *testing.T) {
	f, _ := loggedFixture(t, func(s *sessionfs.CheckpointSettings) { s.MaxTotalBytes = 1 << 40 })
	f.write(t, "a.txt", "one")
	if id := f.service.Begin(context.Background(), "turn", 1); id == "" {
		t.Fatal("first Begin took no checkpoint")
	}
	f.service.options.Settings.MaxTotalBytes = 1
	f.write(t, "a.txt", "two")
	if id := f.service.Begin(context.Background(), "turn", 2); id != "" {
		t.Fatal("Begin took a checkpoint with the repository over max_total_bytes")
	}
	var record Record
	for _, candidate := range f.service.List() {
		if candidate.SessionTurn == 2 {
			record = candidate
		}
	}
	if record.State != StateSkipped || !strings.Contains(record.Warning, "max_total_bytes") {
		t.Fatalf("record = %s / %q, want skipped over max_total_bytes", record.State, record.Warning)
	}
}

func TestTheCachedSizeGrowsByTheCommitAllowanceAndStaysWithinOneBlockPerKeptCommit(t *testing.T) {
	f, _ := loggedFixture(t, nil)
	f.write(t, "a.txt", "one")
	id := f.service.Begin(context.Background(), "turn", 1)
	cached := f.service.size.bytes
	measured, err := f.service.git.repositoryBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cached < 0 || measured-cached > commitObjectAllowance || cached-measured > commitObjectAllowance {
		t.Fatalf("cached %d vs measured %d differ by more than one commit allowance (%d)", cached, measured, commitObjectAllowance)
	}
	f.service.Finish(id)
}

func TestAFirstCaptureOfANewServiceMeasuresTheRepositoryAgain(t *testing.T) {
	f, logPath := loggedFixture(t, nil)
	f.write(t, "a.txt", "one")
	f.service.Begin(context.Background(), "turn", 1)
	before := countCalls(t, logPath, "count-objects")
	repoSizes.Delete(filepath.Clean(f.staging)) // a restart: no remembered size
	restarted := NewService(f.service.options)
	f.write(t, "a.txt", "two")
	restarted.Begin(context.Background(), "turn", 2)
	if got := countCalls(t, logPath, "count-objects") - before; got != 2 {
		t.Fatalf("a restarted service ran count-objects %d times in its first capture, want 2 (gate and record)", got)
	}
}

func TestARemovedRepositoryIsMeasuredAgainAndRecreated(t *testing.T) {
	f, logPath := loggedFixture(t, nil)
	f.write(t, "a.txt", "one")
	f.service.Begin(context.Background(), "turn", 1)
	if err := os.RemoveAll(f.service.git.gitDir); err != nil {
		t.Fatal(err)
	}
	before := countCalls(t, logPath, "count-objects")
	if id := f.service.Begin(context.Background(), "turn", 2); id == "" {
		t.Fatal("Begin did not recover from a removed repository")
	}
	if got := countCalls(t, logPath, "count-objects") - before; got != 2 {
		t.Fatalf("count-objects ran %d times after the repository was removed, want 2", got)
	}
}

func TestARestoreMeasuresTheRepositoryAgain(t *testing.T) {
	f, logPath := loggedFixture(t, nil)
	f.write(t, "a.txt", "one")
	id := f.service.Begin(context.Background(), "turn", 1)
	f.write(t, "a.txt", "two")
	f.service.Finish(id)
	before := countCalls(t, logPath, "count-objects")
	if _, err := f.service.Restore(context.Background(), id, RestoreOptions{Mode: ModeFull}); err != nil {
		t.Fatal(err)
	}
	if got := countCalls(t, logPath, "count-objects") - before; got < 3 {
		t.Fatalf("a restore ran count-objects %d times, want a fresh gate measure plus the two captures", got)
	}
}

func TestSkippedCaptureOverMaxFilesKeepsTheCachedSize(t *testing.T) {
	f, logPath := loggedFixture(t, func(s *sessionfs.CheckpointSettings) { s.MaxFiles = 1 })
	f.write(t, "a.txt", "one")
	f.write(t, "b.txt", "two")
	f.service.Begin(context.Background(), "turn", 1)
	before := countCalls(t, logPath, "count-objects")
	f.service.Begin(context.Background(), "turn", 2)
	if got := countCalls(t, logPath, "count-objects") - before; got != 0 {
		t.Fatalf("count-objects ran %d times after a skipped capture, want 0 (nothing was staged, so the cached size is still exact)", got)
	}
}

func TestConcurrentCapturesShareOneCachedSizeSafely(t *testing.T) {
	f, _ := loggedFixture(t, nil)
	f.write(t, "a.txt", "one")
	var group sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		other := NewService(f.service.options)
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for run := 0; run < 3; run++ {
				if id := other.Begin(context.Background(), fmt.Sprintf("t%d-%d", worker, run), int64(run)); id != "" {
					other.Finish(id)
				}
			}
		}(worker)
	}
	group.Wait()
	if got := len(f.service.List()); got == 0 {
		t.Fatal("no checkpoint was recorded")
	}
}

func TestOverMaxFilesWorkspaceIsSkippedWithoutStaging(t *testing.T) {
	f, logPath := loggedFixture(t, func(s *sessionfs.CheckpointSettings) { s.MaxFiles = 3 })
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		f.write(t, name+".txt", name)
	}
	if id := f.service.Begin(context.Background(), "turn", 1); id != "" {
		t.Fatalf("Begin returned %q for a skipped snapshot", id)
	}
	records := f.service.List()
	if len(records) != 1 || records[0].State != StateSkipped || !strings.Contains(records[0].Warning, "max_files") {
		t.Fatalf("records = %+v", records)
	}
	if got := countCalls(t, logPath, "add"); got != 0 {
		t.Fatalf("add ran %d times for an over-limit workspace, want 0", got)
	}
}

func TestWorkspaceAtMaxFilesStillCaptures(t *testing.T) {
	f, logPath := loggedFixture(t, func(s *sessionfs.CheckpointSettings) { s.MaxFiles = 3 })
	for _, name := range []string{"a", "b", "c"} {
		f.write(t, name+".txt", name)
	}
	if id := f.service.Begin(context.Background(), "turn", 1); id == "" {
		t.Fatalf("Begin skipped a workspace at the limit: %+v", f.service.List())
	}
	if got := countCalls(t, logPath, "add"); got != 1 {
		t.Fatalf("add ran %d times, want 1", got)
	}
}

func TestLimitedBufferStopsTheCommandPastTheEntryLimit(t *testing.T) {
	cancelled := 0
	buffer := &limitedBuffer{max: 1 << 20, cancel: func() { cancelled++ }, stopAfter: 2}
	_, _ = buffer.Write([]byte("a\x00b\x00"))
	if buffer.stopped || cancelled != 0 {
		t.Fatalf("stopped at the limit: stopped=%v cancelled=%d", buffer.stopped, cancelled)
	}
	_, _ = buffer.Write([]byte("c\x00"))
	_, _ = buffer.Write([]byte("d\x00"))
	if !buffer.stopped || cancelled != 1 {
		t.Fatalf("not stopped past the limit: stopped=%v cancelled=%d", buffer.stopped, cancelled)
	}
}
