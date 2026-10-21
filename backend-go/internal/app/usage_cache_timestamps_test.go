package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

type usageTimestampFixture struct {
	resolver *Resolver
	location sessionfs.Location
	config   sessionfs.SessionConfig
}

func newUsageTimestampFixture(t *testing.T) usageTimestampFixture {
	t.Helper()
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	roots := loaded.Roots
	location, err := sessionfs.Resolve(roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.MessagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	user := `{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`
	response := `{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"usage-response-1","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"output":[]}}`
	if err := os.WriteFile(config.MessagesPath, []byte(user+"\n"+response+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	return usageTimestampFixture{resolver: resolver, location: location, config: config}
}

func (fixture usageTimestampFixture) readSnapshot(t *testing.T) {
	t.Helper()
	if _, err := fixture.resolver.ReadSessionSnapshot(context.Background(), fixture.location.Reference, "", httpapi.SnapshotOptions{}, "", nil); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
}

// The TypeScript backend parses usage-cache.json with Number.isSafeInteger and
// rejects the whole file otherwise, so the timestamps must be written as whole
// numbers: no fraction and no exponent.
func TestUsageCacheTimestampsAreWrittenAsWholeMilliseconds(t *testing.T) {
	fixture := newUsageTimestampFixture(t)
	fixture.readSnapshot(t)

	encoded, err := os.ReadFile(fixture.config.UsageCachePath)
	if err != nil {
		t.Fatalf("read usage cache: %v", err)
	}
	var raw struct {
		Transcript map[string]json.RawMessage `json:"transcript"`
	}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("decode usage cache: %v", err)
	}
	whole := regexp.MustCompile(`^[0-9]+$`)
	for _, field := range []string{"device", "inode", "offset", "modified_at_ms", "changed_at_ms", "checkpoint_start"} {
		value, ok := raw.Transcript[field]
		if !ok {
			t.Fatalf("usage cache has no transcript.%s: %s", field, encoded)
		}
		if !whole.Match(value) {
			t.Errorf("transcript.%s = %s, want a whole number", field, value)
		}
	}
}

func TestUsageCacheWithTypeScriptStyleTimestampsIsAccepted(t *testing.T) {
	fixture := newUsageTimestampFixture(t)
	fixture.readSnapshot(t)

	info, err := os.Stat(fixture.config.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := statFile(fixture.config.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	changedAt, ok := stat.changeMS()
	if !ok {
		t.Skip("platform does not report a change time")
	}
	cache, valid := readUsageCacheState(fixture.config)
	if !valid {
		t.Fatal("cache written by Go is not valid for the unchanged transcript")
	}
	if got, want := fmt.Sprint(cache.Transcript.ModifiedAtMS), fmt.Sprint(info.ModTime().UnixMilli()); got != want {
		t.Errorf("modified_at_ms = %s, want %s (Math.trunc of mtimeMs)", got, want)
	}
	if got, want := fmt.Sprint(cache.Transcript.ChangedAtMS), fmt.Sprint(changedAt); got != want {
		t.Errorf("changed_at_ms = %s, want %s", got, want)
	}
}

// A usage cache written by an earlier Go build holds fractional milliseconds.
// It must not be trusted or crash the reader: it is ignored, rebuilt from the
// transcript, and rewritten with whole numbers.
func TestUsageCacheWithFractionalTimestampsIsRebuiltAsWholeNumbers(t *testing.T) {
	fixture := newUsageTimestampFixture(t)
	fixture.readSnapshot(t)

	encoded, err := os.ReadFile(fixture.config.UsageCachePath)
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`("(?:modified_at_ms|changed_at_ms)":)([0-9]+)`)
	legacy := pattern.ReplaceAll(encoded, []byte("${1}${2}.4158"))
	if bytes.Equal(legacy, encoded) {
		t.Fatal("test did not turn the timestamps into fractions")
	}
	if err := os.WriteFile(fixture.config.UsageCachePath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, valid := readUsageCacheState(fixture.config); valid {
		t.Fatal("a cache with fractional timestamps was accepted")
	}

	fixture.readSnapshot(t)
	rewritten, err := os.ReadFile(fixture.config.UsageCachePath)
	if err != nil {
		t.Fatal(err)
	}
	if fractional := regexp.MustCompile(`"(?:modified_at_ms|changed_at_ms)":[0-9]*[.eE]`); fractional.Match(rewritten) {
		t.Fatalf("usage cache still has fractional timestamps: %s", rewritten)
	}
	if _, valid := readUsageCacheState(fixture.config); !valid {
		t.Fatalf("rebuilt usage cache is not valid: %s", rewritten)
	}
}
