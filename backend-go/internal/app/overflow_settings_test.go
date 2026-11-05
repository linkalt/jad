package app

import (
	"log/slog"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestOverflowSettingsCarryTheToolOutputCapAndLogTheCut(t *testing.T) {
	var config sessionfs.SessionConfig
	config.Compaction.Overflow = sessionfs.DefaultCompactionOverflow()
	if overflowSettings(config, nil, "s") != nil {
		t.Fatal("the recovery is off by default")
	}
	config.Compaction.Overflow.Enabled = true
	config.Compaction.Overflow.ToolOutputMaxBytes = 4096
	logs := &lockedBuffer{}
	settings := overflowSettings(config, slog.New(slog.NewTextHandler(logs, nil)), "s")
	if settings == nil || settings.ToolOutputMaxBytes != 4096 || settings.OnToolOutputsCut == nil {
		t.Fatalf("settings = %+v", settings)
	}
	settings.OnToolOutputsCut(1, 4096, 3)
	if line := logs.String(); !strings.Contains(line, "context overflow tool outputs cut") || !strings.Contains(line, "max_bytes=4096") || !strings.Contains(line, "cut=3") {
		t.Fatalf("log = %q", line)
	}
}
