package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/worktree"
)

// startSleeper starts a process that does nothing for a minute and returns it with its
// identity; the test's cleanup ends it.
func startSleeper(t *testing.T) (*exec.Cmd, processIdentity) {
	t.Helper()
	command := exec.Command("sleep", "60")
	if err := command.Start(); err != nil {
		t.Skipf("no sleep: %v", err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	identity, err := identify(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(identity.close)
	return command, identity
}

// endedWithin reports whether the process with pid ends (is gone or a zombie) within d.
func endedWithin(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if _, state, err := readProcessStart(pid); err != nil || state == 'Z' {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const grace = 300 * time.Millisecond

// F4: the start time is field 22 of /proc/<pid>/stat, counted from the last ")" because the
// command name may hold spaces and parentheses.
func TestParseProcessStatCountsFromTheLastParenthesis(t *testing.T) {
	for _, comm := range []string{"cat", "a b", "we ird) name", "x) S 1 2 3 (y"} {
		stat := "123 (" + comm + ") S 1 123 123 0 -1 4194304 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 200 18446744073709551615 1 1 0"
		start, state, err := parseProcessStat(stat)
		if err != nil || start != 987654 || state != 'S' {
			t.Fatalf("%q: start %d state %c err %v", comm, start, state, err)
		}
	}
	for _, bad := range []string{"", "123 cat S", "123 (cat) S 1 2", "123 (cat) S 1 123 123 0 -1 4194304 100 0 0 0 1 2 0 0 20 0 1 0 notanumber 1"} {
		if _, _, err := parseProcessStat(bad); err == nil {
			t.Fatalf("%q was read", bad)
		}
	}
	// This process's own status is read the same way.
	if start, state, err := readProcessStart(os.Getpid()); err != nil || start == 0 || state == 'Z' {
		t.Fatalf("own start %d state %c err %v", start, state, err)
	}
}

// F4: a process is the holder only with its start time. A pid that another process has (the
// start time differs) is gone: it is not alive, kill leaves it, and with or without a pidfd.
func TestIdentityNamesAProcessByItsStartTime(t *testing.T) {
	victim, identity := startSleeper(t)
	if !identity.alive() {
		t.Fatal("a live process is not alive")
	}
	reused := processIdentity{pid: identity.pid, start: identity.start + 1, pidfd: -1}
	if reused.alive() {
		t.Fatal("a pid with another start time counts as the process")
	}
	if err := reused.kill(); err != nil || endedWithin(victim.Process.Pid, grace) {
		t.Fatalf("kill of a reused pid: %v (or the other process was ended)", err)
	}
	if _, err := identityOf(identity.pid, identity.start+1); err == nil {
		t.Fatal("a recorded identity with another start time was accepted")
	}
	noPidfd := processIdentity{pid: identity.pid, start: identity.start, pidfd: -1}
	if !noPidfd.alive() {
		t.Fatal("without a pidfd the start time alone must still name the process")
	}
	if err := identity.kill(); err != nil {
		t.Fatal(err)
	}
	if !endedWithin(victim.Process.Pid, 10*time.Second) {
		t.Fatal("kill did not end the process it names")
	}
	if identity.alive() || noPidfd.alive() {
		t.Fatal("an ended process is still alive")
	}
	if err := identity.kill(); err != nil {
		t.Fatalf("kill of a gone process: %v", err)
	}
}

// F4: a join, the root of the in-process readers and a kill each check the holder's identity:
// with a pid another process has, a join fails, the root is one no read can succeed in, and the
// process that has the pid is not killed (jad's own bwrap child is).
func TestHolderChecksItsIdentityBeforeJoinRootAndKill(t *testing.T) {
	other, identity := startSleeper(t)
	staleIdentity := processIdentity{pid: identity.pid, start: identity.start + 1, pidfd: -1}
	holder := &sandboxHolder{pid: identity.pid, identity: staleIdentity, timeout: time.Second}
	if holder.alive() {
		t.Fatal("a holder whose pid another process has is alive")
	}
	if _, err := joinHolder(holder, "/", sandboxLaunch{configured: "true", resolved: "/bin/true"}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("joinHolder = %v", err)
	}
	if root := holder.root(); root != holderGoneRoot {
		t.Fatalf("root = %q, want %q", root, holderGoneRoot)
	}
	if _, err := os.Stat(holder.root() + "/etc"); err == nil {
		t.Fatal("a read through the root of a gone holder succeeded")
	}
	// kill and stop leave the process that has the pid alone.
	bwrap := exec.Command("sleep", "60")
	if err := bwrap.Start(); err != nil {
		t.Fatal(err)
	}
	input, writer, _ := os.Pipe()
	_ = input.Close()
	holder.command, holder.input = bwrap, writer
	holder.stop()
	if endedWithin(other.Process.Pid, grace) {
		t.Fatal("the process that reused the holder's pid was killed")
	}
	if bwrap.ProcessState == nil {
		t.Fatal("stop did not end jad's own bwrap child")
	}
	// A holder that is what it was started as joins, shows its root and is killed.
	live := &sandboxHolder{pid: identity.pid, identity: identity, timeout: time.Second}
	if !live.alive() || live.root() != "/proc/"+strconv.Itoa(identity.pid)+"/root" {
		t.Fatalf("live holder: alive %v root %q", live.alive(), live.root())
	}
	if launch, err := joinHolder(live, "/", sandboxLaunch{configured: "true", resolved: "/bin/true"}); err != nil || !strings.Contains(strings.Join(launch.args, " "), "-t "+strconv.Itoa(identity.pid)+" ") {
		t.Fatalf("join of a live holder: %+v %v", launch, err)
	}
	live.command = exec.Command("sleep", "60")
	if err := live.command.Start(); err != nil {
		t.Fatal(err)
	}
	live.kill()
	if !endedWithin(other.Process.Pid, 10*time.Second) {
		t.Fatal("kill did not end the holder it names")
	}
	_ = live.command.Wait()
}

// F4, recovery: the state file names the holder by pid and start time. A lost server's holder
// that is still running is ended before the run is collected; a pid that another process has
// is left alone. Both clear the record. No mount is needed: the run is an overlay worktree
// whose state file the test fills in.
func TestRecoveryEndsOnlyTheHolderItsRecordNames(t *testing.T) {
	root := gitRepository(t)
	options := isolatedTestOptions()
	options.Materialize = worktree.MaterializeOverlay
	tree, err := worktree.Create(context.Background(), options, root, "recorded", "")
	if err != nil {
		t.Fatal(err)
	}
	// The record's own holder is checked only through the state file.
	for name, test := range map[string]struct {
		skew   uint64
		wanted bool
	}{"the recorded holder": {0, true}, "another process with that pid": {1, false}} {
		process, identity := startSleeper(t)
		if err := tree.SetHolder(worktree.HolderRef{PID: identity.pid, Start: identity.start + test.skew}); err != nil {
			t.Fatal(err)
		}
		if err := reapStaleHolder(tree, 10*time.Second); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ended := endedWithin(process.Process.Pid, grace); ended != test.wanted {
			t.Fatalf("%s: process ended = %v, want %v", name, ended, test.wanted)
		}
		if ref := tree.Holder(); ref != (worktree.HolderRef{}) {
			t.Fatalf("%s: the record was not cleared: %+v", name, ref)
		}
	}
	// No record, a gone process: nothing to do.
	if err := reapStaleHolder(tree, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := tree.SetHolder(worktree.HolderRef{PID: 1 << 30, Start: 5}); err != nil {
		t.Fatal(err)
	}
	if err := reapStaleHolder(tree, time.Second); err != nil || tree.Holder() != (worktree.HolderRef{}) {
		t.Fatalf("a gone holder: %v %+v", err, tree.Holder())
	}
}

// F4: a holder that outlived its server (the lock went with the server, the holder did not) is
// found through the state file and ended by recovery, which then collects the run and leaves no
// second mount of the upper layer.
func TestRecoveryEndsAHolderThatOutlivedItsServer(t *testing.T) {
	useOwnTempDir(t)
	root, tree := overlayRun(t, "overlay-survivor", nil)
	survivor := holderFor(tree.Workspace)
	holderBash(t, tree, nil, false, "echo survived > survived.txt")
	if ref := tree.Holder(); ref.PID != survivor.pid || ref.Start != survivor.identity.start || ref.Start == 0 {
		t.Fatalf("the state file names %+v, the holder is %d/%d", ref, survivor.pid, survivor.identity.start)
	}
	// The server dies: its lock goes (Finish without a launcher lets it go), its registry with
	// it, but the holder keeps running.
	isolatedRuns.Lock()
	delete(isolatedRuns.byPath, filepath.Clean(tree.Path))
	isolatedRuns.Unlock()
	if outcome, err := tree.Finish(context.Background(), "x", nil); err == nil || !outcome.Kept {
		t.Fatalf("Finish without a launcher: %+v %v", outcome, err)
	}
	if !survivor.alive() {
		t.Fatal("the holder did not outlive the server")
	}
	reopened, err := worktree.Open(context.Background(), isolatedTestOptions(), tree.State())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := recoverIsolatedTree(context.Background(), reopened, "recovered", 30*time.Second)
	if err != nil || !outcome.Changed {
		t.Fatalf("outcome %+v, err %v", outcome, err)
	}
	if survivor.alive() {
		t.Fatal("the holder that outlived its server is still running after recovery")
	}
	if got := gitCommand(t, root, "show", tree.Branch+":survived.txt"); got != "survived" {
		t.Fatalf("survived.txt on the branch = %q", got)
	}
}
