package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/serverconfig"
)

// failingCache returns a cache whose partial file cannot be created (the directory is
// missing), so every append goes to the in-memory fallback.
func failingCache(t *testing.T, fallbackMax int) (*runContentCache, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	cache := newRunContentCache(8, serverconfig.DefaultRunContentWriteBatchBytes, time.Millisecond)
	cache.fallbackMax = fallbackMax
	cache.logger = slog.New(slog.NewTextHandler(&logs, nil))
	cache.SetPartialPath(filepath.Join(t.TempDir(), "missing", "responses.partial"))
	t.Cleanup(func() { cache.Close(true) })
	return cache, &logs
}

// A switch to the fallback reads no more of the partial file than the cap can keep, so a large
// partial file does not allocate its whole length in memory first.
func TestFallbackSwitchReadsNoMoreThanTheCap(t *testing.T) {
	cache := newRunContentCache(8, serverconfig.DefaultRunContentWriteBatchBytes, time.Millisecond)
	cache.fallbackMax = 100
	cache.logger = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	cache.SetPartialPath(filepath.Join(t.TempDir(), "responses.partial"))
	t.Cleanup(func() { cache.Close(true) })
	cache.Append(strings.Repeat("a", 4<<20))
	cache.mu.Lock()
	defer cache.mu.Unlock()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cache.switchToFallbackLocked(nil, errors.New("forced"))
	runtime.ReadMemStats(&after)
	if got := len(cache.memory); got != 100 {
		t.Fatalf("memory holds %d bytes, want the cap 100", got)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("the switch allocated %d bytes for a 100 byte cap", allocated)
	}
}

func TestFallbackCacheStopsGrowingAtTheCap(t *testing.T) {
	cache, logs := failingCache(t, 100)
	chunk := strings.Repeat("a", 30)
	for index := 0; index < 10; index++ {
		cache.Append(chunk)
	}
	if !cache.Truncated() {
		t.Fatal("cache is not marked truncated after 300 bytes into a 100 byte cap")
	}
	if got := len(cache.memory); got != 100 {
		t.Fatalf("memory holds %d bytes, want exactly the cap 100", got)
	}
	if got := cache.Length(); got != 100 {
		t.Fatalf("Length = %d, want 100 (what can be read back)", got)
	}
	if got := strings.Count(logs.String(), "run content cache truncated"); got != 1 {
		t.Fatalf("truncation logged %d times, want once\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "limit_bytes=100") {
		t.Fatalf("log does not carry the limit: %s", logs.String())
	}
}

func TestFallbackCacheSnapshotEndsWithTheTruncationMarker(t *testing.T) {
	cache, _ := failingCache(t, 64)
	cache.Append(strings.Repeat("b", 200))
	for name, got := range map[string]string{
		"Snapshot":       cache.Snapshot(),
		"SnapshotPrefix": cache.SnapshotPrefix(cache.Length()),
	} {
		if want := strings.Repeat("b", 64) + runContentTruncationMarker; got != want {
			t.Errorf("%s = %q, want 64 bytes then the marker", name, got)
		}
	}
	if got := cache.SnapshotPrefix(10); got != strings.Repeat("b", 10) {
		t.Errorf("a prefix shorter than the cut text = %q, want 10 plain bytes", got)
	}
}

func TestFallbackCacheBelowTheCapIsUnchanged(t *testing.T) {
	cache, logs := failingCache(t, 1024)
	cache.Append("hello ")
	cache.Append("world")
	if cache.Truncated() || cache.Snapshot() != "hello world" || cache.Length() != 11 {
		t.Fatalf("below the cap: truncated=%v snapshot=%q length=%d", cache.Truncated(), cache.Snapshot(), cache.Length())
	}
	if logs.Len() != 0 && strings.Contains(logs.String(), "truncated") {
		t.Fatalf("unexpected truncation log: %s", logs.String())
	}
}

func TestFallbackCacheCutsOnAUTF8Boundary(t *testing.T) {
	cache, _ := failingCache(t, 10)
	cache.Append(strings.Repeat("é", 20))
	text := cache.Snapshot()
	body := strings.TrimSuffix(text, runContentTruncationMarker)
	if body != strings.Repeat("é", 5) {
		t.Fatalf("cut text = %q (%d bytes), want 5 whole runes", body, len(body))
	}
	if cache.Length() != 10 {
		t.Fatalf("Length = %d, want 10", cache.Length())
	}
}

func TestFallbackCacheDropsTextAfterTheCutEvenIfItWouldFit(t *testing.T) {
	cache, _ := failingCache(t, 10)
	cache.Append("0123456789ABC")
	cache.Append("x")
	if got := strings.TrimSuffix(cache.Snapshot(), runContentTruncationMarker); got != "0123456789" {
		t.Fatalf("text after the cut was kept: %q", got)
	}
}

func TestSpilledFileThatFailsLaterFallsBackWithinTheCap(t *testing.T) {
	var logs bytes.Buffer
	cache := newRunContentCache(4, 4, 0)
	cache.fallbackMax = 32
	cache.logger = slog.New(slog.NewTextHandler(&logs, nil))
	path := filepath.Join(t.TempDir(), "responses.partial")
	cache.SetPartialPath(path)
	defer cache.Close(true)
	cache.Append(strings.Repeat("c", 20))
	if cache.fallback {
		t.Fatal("the file should still work")
	}
	readOnly, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	_ = cache.file.Close()
	cache.file = readOnly
	cache.mu.Unlock()
	cache.Append(strings.Repeat("d", 40))
	if !cache.fallback || !cache.Truncated() {
		t.Fatalf("fallback=%v truncated=%v after the file failed with 60 bytes against a cap of 32", cache.fallback, cache.Truncated())
	}
	if len(cache.memory) != 32 || cache.Length() != 32 {
		t.Fatalf("memory=%d length=%d, want 32", len(cache.memory), cache.Length())
	}
	if !strings.HasPrefix(cache.Snapshot(), strings.Repeat("c", 20)+"dddd") {
		t.Fatalf("the text before the failure was lost: %q", cache.Snapshot())
	}
}

func TestCommittedMessageNeverTakesTheTruncatedText(t *testing.T) {
	registry := newRunRegistry(time.Minute, 1<<20, 0)
	defer registry.close(time.Second)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := failingCache(t, 16)
	run.setContentCache(cache)
	run.cacheContent(strings.Repeat("z", 100))
	run.setCommittedMessage(webUIMessage{ID: "m", Sender: "assistant", Status: "streaming"})
	message := run.currentMessage()
	if message == nil {
		t.Fatal("no message")
	}
	if strings.Contains(message.Text, "truncated") || message.Text != "" {
		t.Fatalf("committed message took the cut text: %q", message.Text)
	}
}

func TestLateSubscriberSeesTheCutTextAndLiveSubscribersAreUnaffected(t *testing.T) {
	registry := newRunRegistry(time.Minute, 1<<20, 0)
	defer registry.close(time.Second)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := failingCache(t, 16)
	run.setContentCache(cache)
	if err := run.publishMessage(webUIMessage{ID: "m", Sender: "assistant", Status: "streaming"}); err != nil {
		t.Fatal(err)
	}
	run.cacheContent(strings.Repeat("q", 100))
	late := run.subscribe()
	defer run.detach(late)
	var snapshot string
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for snapshot == "" {
		event, ok := late.next(ctx)
		if !ok {
			t.Fatal("the queue ended before a ui.message snapshot")
		}
		if event.name == "ui.message" {
			snapshot = string(event.frame)
		}
	}
	if !strings.Contains(snapshot, strings.Repeat("q", 16)) || !strings.Contains(snapshot, "output truncated") {
		t.Fatalf("late snapshot = %q, want the first 16 bytes and the marker", snapshot)
	}
	if strings.Contains(snapshot, strings.Repeat("q", 17)) {
		t.Fatalf("late snapshot is longer than the cap: %q", snapshot)
	}
}

func TestNewHandlerDefaultsAndValidatesTheFallbackCap(t *testing.T) {
	base := Config{APIToken: testAPIToken, MaxBodyBytes: 1024, SSEMaxBufferedBytes: 1024, RunStateRetention: time.Minute, MaxPageTurns: 50}
	handler, err := NewHandler(base, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if handler.runContentFallbackMax != serverconfig.DefaultRunContentFallbackMaxBytes || serverconfig.DefaultRunContentFallbackMaxBytes != 64<<20 {
		t.Fatalf("default fallback cap = %d, want 64 MiB", handler.runContentFallbackMax)
	}
	if handler.sseWriteTimeout != 0 {
		t.Fatalf("an unset Config write timeout = %v, want 0 (the app layer supplies the 30 s default)", handler.sseWriteTimeout)
	}
	for _, bad := range []int{-1, 1023, serverconfig.MaxRunContentFallbackMaxBytes + 1} {
		config := base
		config.RunContentFallbackMaxBytes = bad
		if _, err := NewHandler(config, &fakeResolver{}); err == nil {
			t.Errorf("RunContentFallbackMaxBytes=%d was accepted", bad)
		}
	}
	for _, bad := range []time.Duration{-time.Second, time.Duration(serverconfig.MaxSSEWriteTimeoutMS+1) * time.Millisecond} {
		config := base
		config.SSEWriteTimeout = bad
		if _, err := NewHandler(config, &fakeResolver{}); err == nil {
			t.Errorf("SSEWriteTimeout=%v was accepted", bad)
		}
	}
	good := base
	good.RunContentFallbackMaxBytes = 2048
	good.SSEWriteTimeout = time.Second
	if handler, err := NewHandler(good, &fakeResolver{}); err != nil || handler.runContentFallbackMax != 2048 || handler.sseWriteTimeout != time.Second {
		t.Fatalf("valid values: handler=%+v err=%v", handler, err)
	}
}
