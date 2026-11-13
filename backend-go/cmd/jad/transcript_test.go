package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionlock"
	"jad/backend-go/internal/transcript"
)

func TestTranscriptStripSteerWalksFoldersAndSkipsATranscriptInUse(t *testing.T) {
	root := t.TempDir()
	marked := `{"type":"message","role":"user","content":"x","` + transcript.SessionInputSteerField + `":true}` + "\n"
	plain := `{"type":"message","role":"user","content":"y"}` + "\n"
	idle := filepath.Join(root, "sessions", "a", "messages.jsonl")
	busy := filepath.Join(root, "sessions", "b", "messages.jsonl")
	clean := filepath.Join(root, "sessions", "c", "messages.jsonl")
	for path, data := range map[string]string{idle: plain + marked + marked, busy: marked, clean: plain, filepath.Join(root, "notes.jsonl"): marked} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lockPath, err := sessionlock.PathFor(busy)
	if err != nil {
		t.Fatal(err)
	}
	held, err := sessionlock.Acquire(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	var stdout, stderr bytes.Buffer
	streams := commandStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	if err := dispatchCommand([]string{"transcript", "strip-steer", "--dry-run", root}, streams); !isExit(err, exitFailed) {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(stdout.String(), idle+": would strip 2 records (2 steering markers, 0 __agent_meta)") || !strings.Contains(stdout.String(), "2 transcripts read, would strip 2 records in 1 (2 steering markers, 0 __agent_meta)") {
		t.Fatalf("dry run stdout:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), busy+" is in use by a run") {
		t.Fatalf("dry run stderr:\n%s", stderr.String())
	}
	if data, _ := os.ReadFile(idle); !strings.Contains(string(data), transcript.SessionInputSteerField) {
		t.Fatal("the dry run changed a transcript")
	}

	held.Close()
	stdout.Reset()
	stderr.Reset()
	if err := dispatchCommand([]string{"transcript", "strip-steer", root}, streams); err != nil {
		t.Fatalf("strip: %v (stderr %s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "3 transcripts read, stripped 3 records in 2 (3 steering markers, 0 __agent_meta)") {
		t.Fatalf("strip stdout:\n%s", stdout.String())
	}
	for _, path := range []string{idle, busy} {
		if data, _ := os.ReadFile(path); strings.Contains(string(data), transcript.SessionInputSteerField) {
			t.Fatalf("%s still marked: %s", path, data)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, "notes.jsonl")); !strings.Contains(string(data), transcript.SessionInputSteerField) {
		t.Fatal("a file not named messages.jsonl in a folder was changed")
	}
	if err := dispatchCommand([]string{"transcript"}, streams); !isExit(err, exitFailed) {
		t.Fatalf("no subcommand: %v", err)
	}
}

func isExit(err error, code int) bool {
	var exit *exitError
	return errors.As(err, &exit) && exit.code == code
}

// strip-local --meta-only blanks __agent_meta and keeps the steering marker (for a build that
// knows the marker); strip-local without it, like its alias strip-steer, blanks both. A
// strip-steer with --meta-only is a usage error, and a busy transcript is skipped.
func TestTranscriptStripLocalBlanksMetaOnlyOrBoth(t *testing.T) {
	steer := `,"` + transcript.SessionInputSteerField + `":true`
	meta := `,"` + transcript.MetaField + `":{"` + transcript.MetaStoredAt + `":"2026-11-13T07:01:02.345Z"}`
	data := `{"type":"message","role":"user","content":"x"` + meta + "}\n" +
		`{"type":"message","role":"user","content":"y"` + steer + meta + "}\n" +
		`{"type":"message","role":"user","content":"z"` + steer + "}\n"
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	streams := commandStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	if err := dispatchCommand([]string{"transcript", "strip-steer", "--meta-only", path}, streams); !isExit(err, exitFailed) {
		t.Fatalf("strip-steer --meta-only: %v", err)
	}
	lockPath, err := sessionlock.PathFor(path)
	if err != nil {
		t.Fatal(err)
	}
	held, err := sessionlock.Acquire(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchCommand([]string{"transcript", "strip-local", "--meta-only", path}, streams); !isExit(err, exitFailed) || !strings.Contains(stderr.String(), path+" is in use by a run") {
		t.Fatalf("busy: %v (stderr %s)", err, stderr.String())
	}
	held.Close()
	if unchanged, _ := os.ReadFile(path); string(unchanged) != data {
		t.Fatal("a refused command changed the transcript")
	}

	stdout.Reset()
	if err := dispatchCommand([]string{"transcript", "strip-local", "--meta-only", path}, streams); err != nil {
		t.Fatalf("meta-only: %v", err)
	}
	if !strings.Contains(stdout.String(), "1 transcripts read, stripped 2 records in 1 (0 steering markers, 2 __agent_meta)") {
		t.Fatalf("meta-only stdout:\n%s", stdout.String())
	}
	after, _ := os.ReadFile(path)
	if len(after) != len(data) || strings.Contains(string(after), transcript.MetaField) || strings.Count(string(after), transcript.SessionInputSteerField) != 2 {
		t.Fatalf("meta-only left:\n%s", after)
	}

	stdout.Reset()
	if err := dispatchCommand([]string{"transcript", "strip-local", path}, streams); err != nil {
		t.Fatalf("strip-local: %v", err)
	}
	if !strings.Contains(stdout.String(), "stripped 2 records in 1 (2 steering markers, 0 __agent_meta)") {
		t.Fatalf("strip-local stdout:\n%s", stdout.String())
	}
	after, _ = os.ReadFile(path)
	if len(after) != len(data) || strings.Contains(string(after), "__agent_") {
		t.Fatalf("strip-local left:\n%s", after)
	}
}
