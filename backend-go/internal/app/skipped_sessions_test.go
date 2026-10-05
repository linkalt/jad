package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSkippedSessionsLogsEachReasonOnce(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	var skipped skippedSessions

	for i := 0; i < 50; i++ { // 50 polls of the session list
		skipped.report(logger, "root:broken", "tools.native.skill is not supported")
	}
	if count := strings.Count(output.String(), "skipping invalid session"); count != 1 {
		t.Fatalf("one unchanged skip logged %d times, want 1:\n%s", count, output.String())
	}

	skipped.report(logger, "root:broken", "a different problem")
	if count := strings.Count(output.String(), "skipping invalid session"); count != 2 {
		t.Fatalf("a changed reason must be logged again, got %d lines", count)
	}

	skipped.resolved("root:broken")
	skipped.report(logger, "root:broken", "a different problem")
	if count := strings.Count(output.String(), "skipping invalid session"); count != 3 {
		t.Fatalf("a session that became valid and broke again must be logged again, got %d lines", count)
	}

	skipped.report(logger, "root:other", "a different problem")
	if count := strings.Count(output.String(), "skipping invalid session"); count != 4 {
		t.Fatalf("another session with the same reason must be logged, got %d lines", count)
	}
	if !strings.Contains(output.String(), "session=root:broken") || !strings.Contains(output.String(), "level=WARN") {
		t.Errorf("record lacks the session or level:\n%s", output.String())
	}
}
