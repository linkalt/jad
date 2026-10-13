package native

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

func executeBashWithStdin(t *testing.T, root, callID, command, stdin string, settingsLimit int64) string {
	t.Helper()
	settings := bashTestSettings(t, root)
	if settingsLimit > 0 {
		settings.StdinMaxBytes = settingsLimit
	}
	return executeNativeBash(context.Background(), map[string]any{"command": command, "stdin": stdin}, settings, "", tools.ExecutionContext{
		CallID: callID, WorkspaceRoot: root, SessionDir: root,
	})
}

func TestNativeBashPipesStdinToTheCommand(t *testing.T) {
	root := t.TempDir()
	output := executeBashWithStdin(t, root, "stdin-cat", "cat", "piped-text\nsecond line\n", 0)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "piped-text\nsecond line") {
		t.Fatalf("cat did not echo stdin: %s", output)
	}
	// A pipeline can end in another program.
	output = executeBashWithStdin(t, root, "stdin-pipe", "tr a-z A-Z | rev", "abc", 0)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "CBA") {
		t.Fatalf("pipeline result = %s", output)
	}
	// Multi-byte text arrives intact.
	output = executeBashWithStdin(t, root, "stdin-utf8", "cat", "héllo ✓", 0)
	if !strings.Contains(output, "héllo ✓") {
		t.Fatalf("utf-8 stdin was altered: %s", output)
	}
}

func TestNativeBashRunsAnInlineScriptFedOnStdin(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	root := t.TempDir()
	output := executeBashWithStdin(t, root, "stdin-python", python+" -", "import sys\nprint('sum', 2 + 3)\nprint('err', file=sys.stderr)\n", 0)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "sum 5") {
		t.Fatalf("python script result = %s", output)
	}
}

func TestNativeBashWithoutStdinSeesAClosedInput(t *testing.T) {
	root := t.TempDir()
	started := time.Now()
	// `cat` returns at once with nothing to read instead of waiting for input.
	output := executeBashWithStdin(t, root, "no-stdin", "cat; echo done", "", 0)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "done") {
		t.Fatalf("result without stdin = %s", output)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("a command without stdin took %s", time.Since(started))
	}
}

func TestNativeBashIgnoredStdinIsNotAnError(t *testing.T) {
	root := t.TempDir()
	// 200 KiB into a process that never reads it: the closed pipe must not fail the call.
	output := executeBashWithStdin(t, root, "stdin-ignored", "echo ignored-input", strings.Repeat("x", 200*1024), 1024*1024)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "ignored-input") {
		t.Fatalf("ignored stdin result = %s", output)
	}
}

func TestNativeBashStdinOverTheLimitIsRefusedWhole(t *testing.T) {
	root := t.TempDir()
	output := executeBashWithStdin(t, root, "stdin-limit", "wc -c", "0123456789", 10)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "10") {
		t.Fatalf("stdin at the limit = %s", output)
	}
	output = executeBashWithStdin(t, root, "stdin-over", "cat", "01234567890", 10)
	if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, "error_code: stdin_too_large") ||
		!strings.Contains(output, "11 bytes, over the limit of 10") || strings.Contains(output, "--- CONTENT token=") {
		t.Fatalf("stdin over the limit = %s", output)
	}
	// Three-byte characters: 4 of them are 12 bytes, not 4 characters.
	output = executeBashWithStdin(t, root, "stdin-wide", "cat", "✓✓✓✓", 10)
	if !strings.Contains(output, "error_code: stdin_too_large") || !strings.Contains(output, "12 bytes, over the limit of 10") {
		t.Fatalf("multi-byte stdin over the limit = %s", output)
	}
}

func TestNormalizeBashArgumentsAcceptsInputAsStdin(t *testing.T) {
	normalized := normalizeBashArguments(map[string]any{"command": "cat", "input": "via-input"})
	if normalized["stdin"] != "via-input" {
		t.Fatalf("input was not mapped to stdin: %#v", normalized)
	}
	if _, exists := normalized["input"]; exists {
		t.Fatalf("normalized arguments retained input: %#v", normalized)
	}
	both := normalizeBashArguments(map[string]any{"command": "cat", "stdin": "named", "input": "alias"})
	if both["stdin"] != "named" {
		t.Fatalf("the named field did not win: %#v", both)
	}
}
