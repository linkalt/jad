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
	if !strings.Contains(stdout.String(), idle+": would strip 2 marked records") || !strings.Contains(stdout.String(), "2 transcripts read, would strip 2 marked records in 1") {
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
	if !strings.Contains(stdout.String(), "3 transcripts read, stripped 3 marked records in 2") {
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
