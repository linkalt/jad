package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// sandboxedBashIn runs command in sandbox as a call of workspace root with sessionDir as the
// session's state folder.
func sandboxedBashIn(t *testing.T, root, sessionDir string, sandbox *sessionfs.SandboxSettings, command string) string {
	t.Helper()
	settings := bashTestSettings(t, root)
	settings.Sandbox = sandbox
	return executeNativeBash(context.Background(), map[string]any{"command": command}, settings, "", tools.ExecutionContext{
		CallID: "read-call", WorkspaceRoot: root, SessionDir: sessionDir, SessionKey: "read",
	})
}

// read.paths: a child sees only the listed paths ({system} for the programs) and the
// writable ones; everything else does not exist in its environment.
func TestSandboxWithReadPathsSeesOnlyThem(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mine.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sandbox := bwrapSandbox()
	sandbox.WritablePaths = []string{"{workspace}", "{tmp}"}
	sandbox.ReadPaths = []string{sessionfs.SandboxSystemPlaceholder, "{workspace}", "{tmp}"}
	output := sandboxedBashIn(t, root, t.TempDir(), sandbox, `cat mine.txt; cat '`+outside+`/secret.txt' 2>&1 || echo outside-missing; ls /opt /root 2>&1 | head -2; touch new.txt && echo wrote`)
	requireOutput(t, output, "mine", "outside-missing", "wrote")
	if strings.Contains(output, "secret\n") {
		t.Fatalf("a path outside read.paths was readable:\n%s", output)
	}
	// The same call with the whole file system readable (the default) reads it.
	sandbox.ReadPaths = []string{"/"}
	output = sandboxedBashIn(t, root, t.TempDir(), sandbox, `cat '`+outside+`/secret.txt'`)
	requireOutput(t, output, "secret")
}

// D27/D28: the session's configuration under a writable path stays read-only; a writable
// path inside it is writable again; the session's transcripts stay read-only even when
// {session_dir} is listed, while a folder inside it can be written.
func TestSandboxKeepsTheSessionConfigurationReadOnly(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	root := t.TempDir()
	templates := filepath.Join(root, ".jad", "sandbox")
	state := filepath.Join(root, ".jad", "state")
	sessionDir := filepath.Join(state, "session-1")
	notes := filepath.Join(sessionDir, "notes")
	for _, dir := range []string{templates, notes, filepath.Join(sessionDir, "dev", "coder")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	transcript := filepath.Join(sessionDir, "dev", "coder", "messages.jsonl")
	for _, file := range []string{filepath.Join(templates, "writer.json"), transcript} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sandbox := bwrapSandbox()
	sandbox.WritablePaths = []string{"{workspace}", "{session_dir}", "{session_dir}/notes", "{tmp}"}
	sandbox.ReadOnlyPaths = []string{templates, state}
	output := sandboxedBashIn(t, root, sessionDir, sandbox, `echo '{"enabled":false}' > .jad/sandbox/writer.json 2>&1 || echo template-refused
echo x >> '`+transcript+`' 2>&1 || echo transcript-refused
touch '`+notes+`/n.txt' && echo notes-writable
touch other.txt && echo workspace-writable`)
	requireOutput(t, output, "template-refused", "transcript-refused", "notes-writable", "workspace-writable")
	if data, _ := os.ReadFile(filepath.Join(templates, "writer.json")); string(data) != "{}\n" {
		t.Fatalf("the template changed: %q", data)
	}
	if data, _ := os.ReadFile(transcript); string(data) != "{}\n" {
		t.Fatalf("the transcript changed: %q", data)
	}
}

// {session_dir} names the session's state folder; without one its paths are left out.
func TestSandboxExpanderFillsSessionDir(t *testing.T) {
	expand := sandboxExpander(sandboxPlace{workspace: "/w", sessionDir: "/state/s1"})
	if got := expand("{session_dir}/data"); got != "/state/s1/data" {
		t.Fatalf("got %q", got)
	}
	if got := sandboxExpander(sandboxPlace{workspace: "/w"})("{session_dir}/data"); got != "" {
		t.Fatalf("no session: %q", got)
	}
}

// D5: the default hidden paths cover the credential stores in the home folder.
func TestSandboxHidesTheDefaultCredentialPaths(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{".ssh", ".config/gh"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{".ssh/id_ed25519", ".config/gh/hosts.yml", ".netrc"} {
		if err := os.WriteFile(filepath.Join(home, file), []byte("key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := sessionfs.ParseSandboxSetting([]byte(`{"enabled":true}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	output := sandboxedBashIn(t, root, t.TempDir(), loaded, `cat ~/.ssh/id_ed25519 ~/.config/gh/hosts.yml ~/.netrc 2>&1; echo done`)
	requireOutput(t, output, "done")
	if strings.Contains(output, "key") {
		t.Fatalf("a credential file was readable:\n%s", output)
	}
}

func TestSeatbeltProfileWithReadPaths(t *testing.T) {
	useOwnTempDir(t)
	root := t.TempDir()
	real, _ := filepath.EvalSymlinks(root)
	sandbox := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendSeatbelt, Executable: "sandbox-exec", ReadPaths: []string{"{workspace}"}, WritablePaths: []string{"{tmp}"}}
	place := sandboxPlace{workspace: root, cwd: root}
	profile := seatbeltProfile(sandbox, place, sandboxExpander(place))
	order := []string{"(allow default)", "(deny file-read*)", "(allow file-read* (subpath \"/dev\") (subpath " + seatbeltString(real) + ")", "(deny file-write*)"}
	at := 0
	for _, want := range order {
		index := strings.Index(profile[at:], want)
		if index < 0 {
			t.Fatalf("missing, or out of order, %q in:\n%s", want, profile)
		}
		at += index + len(want)
	}
	sandbox.ReadPaths = []string{"/"}
	if profile := seatbeltProfile(sandbox, place, sandboxExpander(place)); strings.Contains(profile, "(deny file-read*)\n") {
		t.Fatalf("the whole file system readable denies reads:\n%s", profile)
	}
}
