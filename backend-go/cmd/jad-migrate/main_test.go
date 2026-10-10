package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// oneSession builds a legacy root with one session and returns the root setting path.
func oneSession(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cwd := t.TempDir()
	writeJSON(t, filepath.Join(root, "setting.json"), map[string]any{
		"schema_version": 1,
		"session_roots":  []any{map[string]any{"id": "local", "path": "sessions"}},
	})
	session := filepath.Join(root, "sessions", "alpha")
	writeJSON(t, filepath.Join(session, "model.json"), map[string]any{"provider": "openai", "model": "m", "api_key_env": "K", "base_url": "http://127.0.0.1:1/v1"})
	writeJSON(t, filepath.Join(session, "setting.json"), map[string]any{
		"cwd":   cwd,
		"paths": map[string]any{"active_profile": "p", "profiles": map[string]any{"p": map[string]any{"model_default": "model.json", "messages": "p/messages.jsonl"}}},
	})
	return filepath.Join(root, "setting.json")
}

func TestRunExitCodes(t *testing.T) {
	setting := oneSession(t)
	out := filepath.Join(t.TempDir(), "new")
	cases := []struct {
		name string
		args []string
		want int
		in   string // text the output must hold
	}{
		{"help is not an error", []string{"-h"}, exitOK, "Usage"},
		{"no flags", nil, exitUsage, "Usage"},
		{"missing out", []string{"--setting", setting}, exitUsage, "Usage"},
		{"extra argument", []string{"--setting", setting, "--out", out, "more"}, exitUsage, "Usage"},
		{"unknown flag", []string{"--nope"}, exitUsage, ""},
		{"a missing setting file", []string{"--setting", filepath.Join(t.TempDir(), "none.json"), "--out", out}, exitFailed, "jad-migrate:"},
		{"success", []string{"--setting", setting, "--out", out}, exitOK, "every profile loads to the same settings"},
		{"the output is no longer empty", []string{"--setting", setting, "--out", out}, exitFailed, "not empty"},
	}
	for _, test := range cases {
		var stdout, stderr bytes.Buffer
		got := run(test.args, &stdout, &stderr)
		if got != test.want {
			t.Errorf("%s: exit = %d, want %d\nstdout: %s\nstderr: %s", test.name, got, test.want, stdout.String(), stderr.String())
		}
		if text := stdout.String() + stderr.String(); !strings.Contains(text, test.in) {
			t.Errorf("%s: output does not hold %q: %s", test.name, test.in, text)
		}
	}
}
