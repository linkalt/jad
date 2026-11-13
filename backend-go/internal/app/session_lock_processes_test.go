package app

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// These tests put the second server on the other side of a process boundary.
// The settings lock is no longer held for a whole run, so what keeps two
// servers on one session directory apart is the per-profile transcript lock. A
// helper process runs the real
// Resolver against the same directory.

const (
	lockHelperRootEnv = "JAD_APPLOCK_HELPER_ROOT"
	lockHelperModeEnv = "JAD_APPLOCK_HELPER_MODE"
)

// newSharedSessionRoot writes the lock tests' layout session and returns the workspace and the
// global folder ("<workspace>:<global>"), which the helper process opens too.
func newSharedSessionRoot(t *testing.T) string {
	t.Helper()
	_, fixture := layoutConfig(t, lockSpec(), map[string]string{})
	return fixture.Workspace + string(os.PathListSeparator) + fixture.GlobalDir
}

func resolverForRoot(rootSetting string) (*Resolver, error) {
	workspace, global, _ := strings.Cut(rootSetting, string(os.PathListSeparator))
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: workspace, GlobalDir: global, Environment: map[string]string{}})
	if err != nil {
		return nil, err
	}
	return NewResolver(config.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
}

// TestAppLockHelperProcess is the second "server". Modes:
//
//	hold:<profile> take LockSession, print "held", keep it until stdin closes
//	models         flip model_default 300 times under LockSessionSettings, print "done"
func TestAppLockHelperProcess(t *testing.T) {
	rootSetting := os.Getenv(lockHelperRootEnv)
	if rootSetting == "" {
		t.Skip("helper process only")
	}
	resolver, err := resolverForRoot(rootSetting)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(2)
	}
	mode, arg, _ := strings.Cut(os.Getenv(lockHelperModeEnv), ":")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch mode {
	case "hold":
		release, err := resolver.LockSession(ctx, "session-1", arg, "")
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(2)
		}
		fmt.Println("held")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		release()
	case "models":
		for i := 0; i < 300; i++ {
			model := "openai/model"
			if i%2 == 1 {
				model = "openai/model2"
			}
			release, err := resolver.LockSessionSettings(ctx, "session-1")
			if err != nil {
				fmt.Println("error:", err)
				os.Exit(2)
			}
			_, err = resolver.UpdateModelDefault(ctx, "session-1", "dev/green", model, "")
			release()
			if err != nil {
				fmt.Println("error:", err)
				os.Exit(2)
			}
		}
		fmt.Println("done")
	default:
		fmt.Println("error: unknown mode", mode)
		os.Exit(2)
	}
}

type lockHelper struct {
	cmd   *exec.Cmd
	stdin interface{ Close() error }
	lines *bufio.Scanner
}

func startLockHelper(t *testing.T, rootSetting, mode string) *lockHelper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAppLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperRootEnv+"="+rootSetting, lockHelperModeEnv+"="+mode)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &lockHelper{cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout)}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return h
}

// await reads helper output until the wanted line shows up. It does not touch
// testing.T, so it is safe to call from a goroutine.
func (h *lockHelper) await(want string) error {
	done := make(chan string, 1)
	go func() {
		for h.lines.Scan() {
			line := strings.TrimSpace(h.lines.Text())
			if line == want || strings.HasPrefix(line, "error:") {
				done <- line
				return
			}
		}
		done <- "eof"
	}()
	select {
	case got := <-done:
		if got != want {
			return fmt.Errorf("helper said %q, want %q", got, want)
		}
		return nil
	case <-time.After(15 * time.Second):
		return fmt.Errorf("helper did not say %q within 15s", want)
	}
}

func (h *lockHelper) expect(t *testing.T, want string) {
	t.Helper()
	if err := h.await(want); err != nil {
		t.Fatal(err)
	}
}

func (h *lockHelper) finish(t *testing.T) {
	t.Helper()
	_ = h.stdin.Close()
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("helper exited with an error: %v", err)
	}
}

func lockInRoot(resolver *Resolver, profile string, wait time.Duration) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return resolver.LockSession(ctx, "session-1", profile, "")
}

// A run in another server process holds one profile. A second run of that
// profile here must wait; another profile must start at once.
func TestOtherProcessRunBlocksOnlyItsOwnProfile(t *testing.T) {
	rootSetting := newSharedSessionRoot(t)
	other := startLockHelper(t, rootSetting, "hold:dev/blue")
	other.expect(t, "held")
	resolver, err := resolverForRoot(rootSetting)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := lockInRoot(resolver, "dev/blue", 300*time.Millisecond); err == nil {
		release()
		t.Fatal("this process started a second run of blue while another process runs it")
	}
	start := time.Now()
	release, err := lockInRoot(resolver, "dev/green", time.Second)
	if err != nil {
		t.Fatalf("green could not start while another process runs blue (waited %v): %v", time.Since(start), err)
	}
	release()
	other.finish(t)
	release, err = lockInRoot(resolver, "dev/blue", time.Second)
	if err != nil {
		t.Fatalf("blue stayed locked after the other process released it: %v", err)
	}
	release()
}

// Run starts read setting.json while another process rewrites model_default.
// The write is an atomic replace, so a start must never see a torn file.
func TestRunStartsWhileOtherProcessWritesModelDefault(t *testing.T) {
	rootSetting := newSharedSessionRoot(t)
	writer := startLockHelper(t, rootSetting, "models")
	resolver, err := resolverForRoot(rootSetting)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- writer.await("done") }()
	starts := 0
	for {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
			if starts == 0 {
				t.Fatal("no run start happened while the other process wrote")
			}
			t.Logf("%d run starts overlapped the writes", starts)
			writer.finish(t)
			return
		default:
		}
		release, err := lockInRoot(resolver, "dev/blue", 5*time.Second)
		if err != nil {
			t.Fatalf("run start %d failed while another process wrote model_default: %v", starts, err)
		}
		release()
		starts++
	}
}
