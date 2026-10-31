package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewHonorsLevelAndFormat(t *testing.T) {
	cases := []struct {
		name, level, format string
		debugLogged         bool
		infoLogged          bool
		warnLogged          bool
	}{
		{"default is info text", "", "", false, true, true},
		{"debug", "debug", "text", true, true, true},
		{"warn drops info", "warn", "text", false, false, true},
		{"error drops warn", "error", "text", false, false, false},
		{"off drops everything", "off", "text", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger, closer, err := New(Settings{Level: tc.level, Format: tc.format}, &output)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			logger.Debug("debug-line")
			logger.Info("info-line")
			logger.Warn("warn-line")
			text := output.String()
			if got := strings.Contains(text, "debug-line"); got != tc.debugLogged {
				t.Errorf("debug logged = %v, want %v; output %q", got, tc.debugLogged, text)
			}
			if got := strings.Contains(text, "info-line"); got != tc.infoLogged {
				t.Errorf("info logged = %v, want %v", got, tc.infoLogged)
			}
			if got := strings.Contains(text, "warn-line"); got != tc.warnLogged {
				t.Errorf("warn logged = %v, want %v", got, tc.warnLogged)
			}
		})
	}
}

func TestNewJSONFormatIsOneObjectPerLine(t *testing.T) {
	var output bytes.Buffer
	logger, closer, err := New(Settings{Format: "json"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	logger.Info("hello", slog.String("session", "s1"))
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("log line is not JSON: %v: %q", err, output.String())
	}
	if record["msg"] != "hello" || record["session"] != "s1" || record["level"] != "INFO" {
		t.Errorf("record = %v", record)
	}
}

func TestNewRejectsUnknownLevelAndFormat(t *testing.T) {
	if _, _, err := New(Settings{Level: "verbose"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), `"verbose"`) {
		t.Errorf("unknown level error = %v", err)
	}
	if _, _, err := New(Settings{Format: "xml"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), `"xml"`) {
		t.Errorf("unknown format error = %v", err)
	}
}

func TestNewWritesToFileAppendsAndCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "logs", "agent.log")
	var stderr bytes.Buffer
	for _, line := range []string{"first", "second"} {
		logger, closer, err := New(Settings{File: path}, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		logger.Info(line)
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Errorf("file must keep both runs, got %q", data)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr must stay empty when a file is configured, got %q", stderr.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("log file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestNewTightensAnExistingLogFileToPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, closer, err := New(Settings{File: path}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("an existing 0644 log must become 0600, got %v", info.Mode().Perm())
	}
}

func TestNewFailsWhenTheLogFileCannotBeOpened(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(Settings{File: filepath.Join(blocker, "agent.log")}, &bytes.Buffer{}); err == nil {
		t.Fatal("a log file below a regular file must be an error, not a silent drop")
	}
}

func TestOffDoesNotOpenTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	logger, closer, err := New(Settings{Level: "off", File: path}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	logger.Error("dropped")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("off must not create the log file, stat error = %v", err)
	}
}

func TestOrAndDiscardNeverPanic(t *testing.T) {
	Or(nil).Error("nothing", slog.Any("error", os.ErrNotExist))
	Discard().With("a", "b").Info("nothing")
	if Or(nil) != Discard() {
		t.Error("Or(nil) should return the shared discard logger")
	}
	custom := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if Or(custom) != custom {
		t.Error("Or must return a non-nil logger unchanged")
	}
}

// TestJSONRecordsNeverRepeatAKey guards the shape of the startup record: slog
// owns "level", "time" and "msg", so a caller attribute with one of those names
// would produce a duplicate JSON key.
func TestJSONRecordsNeverRepeatAKey(t *testing.T) {
	var output bytes.Buffer
	logger, closer, err := New(Settings{Format: "json"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	logger.Info("Go agent API listening", slog.String("address", "127.0.0.1:1"), slog.String("log_level", "info"), slog.String("log_file", ""))
	decoder := json.NewDecoder(&output)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		t.Fatalf("not an object: %v %v", token, err)
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		name := key.(string)
		if seen[name] {
			t.Errorf("duplicate key %q in %s", name, output.String())
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
}
