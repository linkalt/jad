//go:build !windows

package native

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHooksKillLeftoverProcessesOnlyWhenTheirOwnPolicySaysSo(t *testing.T) {
	leftover := `sleep 30 >/dev/null 2>&1 & echo $! > "$0.pid"`
	for _, test := range []struct {
		policy    string
		wantAlive bool
	}{{"always", false}, {"timeout_only", true}} {
		hooks := hookTestSettings(shHook("bg", leftover, ".txt"))
		hooks.KillProcessGroup = test.policy
		write, _, root := hookedTools(t, hooks)
		callTool(t, write, root, map[string]any{"path": "docs/k.txt", "content": "x\n"}, nil)
		data, err := os.ReadFile(filepath.Join(root, "docs", "k.txt.pid"))
		if err != nil {
			t.Fatalf("%s: the check did not record the background pid: %v", test.policy, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		alive := false
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			alive = syscall.Kill(pid, 0) == nil
			if alive == test.wantAlive && !test.wantAlive {
				break
			}
		}
		if alive != test.wantAlive {
			t.Errorf("kill_process_group %q: leftover alive = %v, want %v", test.policy, alive, test.wantAlive)
		}
	}
}
