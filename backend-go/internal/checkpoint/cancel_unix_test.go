//go:build unix

package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// M05: cancelling the caller's context kills a running git command.
func TestBeginReturnsPromptlyWhenTheCallerCancelsDuringSlowGit(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	binary := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nwhile [ \"$1\" = \"-c\" ]; do shift 2; done\n" +
		"if [ \"$1\" = \"add\" ]; then exec sleep 30; fi\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(settings *sessionfs.CheckpointSettings) { settings.GitBinary = binary })
	f.seed(t)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	started := time.Now()
	if id := f.service.Begin(ctx, "turn", 1); id != "" {
		t.Fatalf("Begin returned checkpoint %q although its git call was cancelled", id)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Begin took %v after the context was cancelled", elapsed)
	}
}
