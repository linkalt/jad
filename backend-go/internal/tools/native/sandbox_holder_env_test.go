package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/worktree"
)

// The git of a collection in the overlay holder runs with the host's environment, but a remote key
// may read its token from a JAD_REMOTE_* variable, and git there runs hooks and filters the run
// could have left in the repository: those variables are not passed on.
func TestHolderLauncherDropsTheRemoteKeyVariables(t *testing.T) {
	for _, program := range []string{"nsenter", "git"} {
		if _, err := exec.LookPath(program); err != nil {
			t.Skip(program + " is not installed")
		}
	}
	t.Setenv("JAD_REMOTE_BOX", "REMOTETOKEN_0123456789abcdef0123456789abcdef")
	t.Setenv("JAD_KEEP_THIS", "kept")
	self, err := identify(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(self.close)
	tree := &worktree.Tree{Path: filepath.Join(t.TempDir(), "run")}
	tree.Workspace = tree.Path
	path := filepath.Clean(tree.Path)
	isolatedRuns.Lock()
	isolatedRuns.byPath[path] = isolatedRun{path: path, holder: &sandboxHolder{pid: os.Getpid(), identity: self}}
	isolatedRuns.Unlock()
	t.Cleanup(func() {
		isolatedRuns.Lock()
		delete(isolatedRuns.byPath, path)
		isolatedRuns.Unlock()
	})
	launch, err := holderLauncher(tree)("git", []string{"status"}, tree.Path)
	if err != nil {
		t.Fatal(err)
	}
	environment := strings.Join(launch.Env, "\n")
	if strings.Contains(environment, "JAD_REMOTE_") || strings.Contains(environment, "REMOTETOKEN_") || !strings.Contains(environment, "JAD_KEEP_THIS=kept") {
		t.Fatalf("the environment of the holder's git:\n%s", environment)
	}
}

func TestWithoutRemoteKeysKeepsEverythingElse(t *testing.T) {
	got := withoutRemoteKeys([]string{"PATH=/bin", "JAD_REMOTE_X=a", "JAD_REMOTEX=b", "JAD_REMOTE_=c", "HOME=/h"})
	if strings.Join(got, ",") != "PATH=/bin,JAD_REMOTEX=b,HOME=/h" {
		t.Fatalf("%q", got)
	}
}
