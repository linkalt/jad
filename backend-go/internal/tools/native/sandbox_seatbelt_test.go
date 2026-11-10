package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestSeatbeltProfileFollowsTheSettings(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "work")
	secret := filepath.Join(root, "secret")
	key := filepath.Join(root, "key.pem")
	for _, directory := range []string{workspace, secret} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	sandbox := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendSeatbelt, WritablePaths: []string{"{workspace}", "/does/not/matter", "{home}"}, HidePaths: []string{secret, key, filepath.Join(root, "missing")}}
	place := sandboxPlace{workspace: link, cwd: link}
	profile := seatbeltProfile(sandbox, place, sandboxExpander(place))
	resolvedWorkspace, _ := filepath.EvalSymlinks(workspace)
	resolvedSecret, _ := filepath.EvalSymlinks(secret)
	resolvedKey, _ := filepath.EvalSymlinks(key)
	for _, want := range []string{
		"(version 1)\n(allow default)\n(deny file-write*)\n",
		`(subpath "` + resolvedWorkspace + `")`, `(subpath "/does/not/matter")`,
		`(deny file-read* file-write* (subpath "` + resolvedSecret + `"))`,
		`(deny file-read* file-write* (literal "` + resolvedKey + `"))`,
		"(deny network*)\n(allow network* (local unix-socket) (remote unix-socket))",
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile misses %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "missing") {
		t.Errorf("a hidden path that does not exist is in the profile:\n%s", profile)
	}
	sandbox.Network = true
	if strings.Contains(seatbeltProfile(sandbox, place, sandboxExpander(place)), "network") {
		t.Error("network true still denies the network")
	}
	if got := seatbeltString(`a "b" \c`); got != `"a \"b\" \\c"` {
		t.Errorf("seatbeltString = %s", got)
	}
}

func TestSeatbeltSandboxStartsTheChildThroughSandboxExec(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "sandbox-exec")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	sandbox := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendSeatbelt, Executable: "sandbox-exec", WritablePaths: []string{"{workspace}"}}
	file, argv0, args, _, err := SandboxCommand(sandbox, t.TempDir(), t.TempDir(), "sh", []string{"-c", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if file != fake || argv0 != "sandbox-exec" || args[0] != "-p" || !strings.HasPrefix(args[1], "(version 1)") || args[2] != "--" || !strings.HasSuffix(args[3], "/sh") || args[4] != "-c" {
		t.Fatalf("launch = %s %s %q", file, argv0, args)
	}
}
