package native

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// otherProcess starts a program that sleeps and returns its identity. The process is
// killed when the test ends.
func otherProcess(t *testing.T) (serverIdentity, *exec.Cmd) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process liveness is not checked on Windows")
	}
	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return serverIdentity{PID: cmd.Process.Pid, Start: processStartTime(cmd.Process.Pid)}, cmd
}

// writeForeignManifest stores a manifest as another server would have written it.
func writeForeignManifest(t *testing.T, root, id, state string, server *serverIdentity) {
	t.Helper()
	store := newSubagentStore(root, nil)
	err := store.write(subagentManifest{
		Version: subagentManifestVersion, ID: id, Owner: "alpha", Agent: "explore", Task: "other server's task",
		State: state, StartedAt: time.Now().Add(-time.Minute), Server: server,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func recoveredState(t *testing.T, root, id string, keep int) (state string, known bool) {
	t.Helper()
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: keep})
	t.Cleanup(func() { _ = manager.Close() })
	for _, view := range manager.list("alpha") {
		if view.id == id {
			return view.state, true
		}
	}
	return "", false
}

func TestSubagentRunOfAnotherLiveServerIsNotMarkedLost(t *testing.T) {
	root := t.TempDir()
	other, _ := otherProcess(t)
	writeForeignManifest(t, root, "sub_0000aaaa", subagentRunRunning, &other)

	if state, known := recoveredState(t, root, "sub_0000aaaa", 10); known {
		t.Fatalf("this server took over the run of a live server, state %q", state)
	}
	if got := manifestOf(t, root, "sub_0000aaaa").State; got != subagentRunRunning {
		t.Errorf("manifest state = %q, want running: the other server's run was rewritten", got)
	}
}

func TestSubagentRunOfAServerThatIsGoneIsMarkedLost(t *testing.T) {
	root := t.TempDir()
	other, cmd := otherProcess(t)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	writeForeignManifest(t, root, "sub_0000bbbb", subagentRunRunning, &other)

	state, known := recoveredState(t, root, "sub_0000bbbb", 10)
	if !known || state != subagentRunLost {
		t.Fatalf("run of a dead server: known=%v state=%q, want lost", known, state)
	}
	if got := manifestOf(t, root, "sub_0000bbbb").State; got != subagentRunLost {
		t.Errorf("manifest state = %q, want lost", got)
	}
}

func TestSubagentRunOfAReusedPidIsMarkedLost(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the start time is read from /proc")
	}
	root := t.TempDir()
	other, _ := otherProcess(t)
	reused := serverIdentity{PID: other.PID, Start: other.Start + 1}
	writeForeignManifest(t, root, "sub_0000cccc", subagentRunRunning, &reused)

	if state, known := recoveredState(t, root, "sub_0000cccc", 10); !known || state != subagentRunLost {
		t.Fatalf("run whose pid belongs to another program: known=%v state=%q, want lost", known, state)
	}
}

func TestSubagentManifestWithoutAServerIsMarkedLost(t *testing.T) {
	root := t.TempDir()
	writeForeignManifest(t, root, "sub_0000dddd", subagentRunRunning, nil)

	if state, known := recoveredState(t, root, "sub_0000dddd", 10); !known || state != subagentRunLost {
		t.Fatalf("manifest from before servers were recorded: known=%v state=%q, want lost", known, state)
	}
}

func TestSubagentRunOfThisProcessIsMarkedLostAfterARestart(t *testing.T) {
	root := t.TempDir()
	self := currentServerIdentity()
	writeForeignManifest(t, root, "sub_0000eeee", subagentRunRunning, &self)

	if state, known := recoveredState(t, root, "sub_0000eeee", 10); !known || state != subagentRunLost {
		t.Fatalf("run of an earlier manager of this process: known=%v state=%q, want lost", known, state)
	}
}

func TestSubagentPruningLeavesRunsOfAnotherLiveServer(t *testing.T) {
	root := t.TempDir()
	other, _ := otherProcess(t)
	writeForeignManifest(t, root, "sub_0000ffff", subagentRunDone, &other)
	for _, id := range []string{"sub_00000001", "sub_00000002", "sub_00000003"} {
		writeForeignManifest(t, root, id, subagentRunDone, nil)
	}

	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 1})
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := os.Stat(root + "/" + subagentManifestDir + "/sub_0000ffff.json"); err != nil {
		t.Errorf("the other server's finished run was pruned: %v", err)
	}
	if got := len(manager.list("alpha")); got != 1 {
		t.Errorf("this server keeps %d runs, want 1", got)
	}
}

func TestSubagentManifestRecordsTheServerThatWroteIt(t *testing.T) {
	root := t.TempDir()
	first, firstManager := persistedCatalog(t, root, &fakeLauncher{}, 10)
	id := startedRunID(t, first, firstManager, "alpha", `{"task":"x"}`)
	if got := manifestOf(t, root, id).Server; got == nil || got.PID != os.Getpid() {
		t.Fatalf("manifest server = %+v, want this process", got)
	}
}

func TestParseProcStatStartCountsFromTheLastParenthesis(t *testing.T) {
	const tail = " S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 23 24"
	cases := map[string]string{
		"plain":       "123 (sleep)" + tail,
		"with space":  "123 (my prog)" + tail,
		"parentheses": "123 (a) b (c))" + tail,
	}
	for name, line := range cases {
		if got := parseProcStatStart(line); got != 424242 {
			t.Errorf("%s: start = %d, want 424242", name, got)
		}
	}
	for _, bad := range []string{"", "123 sleep S 1", "123 (sleep) S 1 2", "123 (x) S " + strings.Repeat("a ", 30)} {
		if got := parseProcStatStart(bad); got != 0 {
			t.Errorf("%q: start = %d, want 0", bad, got)
		}
	}
}
