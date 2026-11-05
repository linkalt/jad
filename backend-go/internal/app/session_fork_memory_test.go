package app

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/imagestore"
)

// growTranscript appends turns user turns of about lineBytes each to the session's
// transcript, each answered, and returns the transcript's size.
func growTranscript(t *testing.T, f *heldResetFixture, session string, turns, lineBytes int) int64 {
	t.Helper()
	path := filepath.Join(f.fixture.StateDir, session, "dev", "coder", "messages.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("x", lineBytes)
	var buffer bytes.Buffer
	for turn := range turns {
		buffer.Reset()
		fmt.Fprintf(&buffer, `{"type":"message","role":"user","content":[{"type":"input_text","text":"turn %d %s"}],"__agent_session_input_batch_id":"batch%028d","__agent_session_input_batch_index":0}`+"\n", turn, text, turn)
		fmt.Fprintf(&buffer, `{"type":"response.completed","session_turn":%d,"session_step":1,"response":{"id":"resp-big-%d","status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`+"\n", turn+2, turn)
		if _, err := file.Write(buffer.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// allocatedBy reports the highest live heap above the starting one while call ran, with the
// collector run after every 5% of growth so garbage does not count.
func allocatedBy(call func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	base, peak := stats.HeapAlloc, stats.HeapAlloc
	done := make(chan struct{})
	sampled := make(chan uint64)
	go func() {
		for {
			select {
			case <-done:
				sampled <- peak
				return
			default:
			}
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			peak = max(peak, stats.HeapAlloc)
			time.Sleep(100 * time.Microsecond)
		}
	}()
	call()
	close(done)
	return <-sampled - base
}

// A fork streams the transcript it copies and searches a message one turn at a time, so its
// live memory follows the largest turn, not the transcript: with 32 turns of 512 KiB, the
// peak stays under half the transcript (it was about twice the transcript before).
func TestForkHoldsATurnInMemoryNotTheTranscript(t *testing.T) {
	f := newHeldResetFixture(t)
	close(f.release)
	f.runTurn("local:one", "first")
	size := growTranscript(t, f, "one", 32, 512*1024)
	source := f.transcript("one")
	ids, _ := f.userMessages("local:one")
	whole := allocatedBy(func() {
		if status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:copy"}`); status != http.StatusCreated || !strings.Contains(body, `"turns":33`) {
			t.Fatalf("whole fork: %d %s", status, body)
		}
	})
	byMessage := allocatedBy(func() {
		if status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:msg","message_id":"`+ids[len(ids)-1]+`","keep_turn":false}`); status != http.StatusCreated || !strings.Contains(body, `"turns":32`) {
			t.Fatalf("message fork: %d %s", status, body)
		}
	})
	t.Logf("transcript %d bytes; peak live heap of the whole fork %d, of the message fork %d", size, whole, byMessage)
	if limit := uint64(size / 2); whole > limit || byMessage > limit {
		t.Fatalf("fork peak heap %d (whole) and %d (by message) for a %d byte transcript; want both under %d", whole, byMessage, size, limit)
	}
	if !bytes.Equal(f.transcript("copy"), source) {
		t.Fatal("the whole fork is not a byte copy of the source transcript")
	}
	if forked := f.transcript("msg"); !bytes.HasPrefix(source, forked) || len(forked) >= len(source) {
		t.Fatalf("the message fork is not a prefix of the source: %d of %d bytes", len(forked), len(source))
	}
}

// The history block is copied byte for byte, a last line without a line break included,
// and the images its lines refer to are collected; a missing block copies nothing.
func TestCopyHistoryBlockStreamsTheBlockAndItsImages(t *testing.T) {
	dir := t.TempDir()
	first, second := strings.Repeat("a", 64)+".png", strings.Repeat("b", 64)+".jpg"
	block := `{"image_url":"` + imagestore.RefPrefix + first + `"}` + "\n" + `{"text":"plain"}` + "\n" + `{"image_url":"` + imagestore.RefPrefix + second + `"}`
	source, destination := filepath.Join(dir, "backup.jsonl"), filepath.Join(dir, "out", "backup.jsonl")
	if err := os.WriteFile(source, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	images := map[string]struct{}{}
	if err := copyHistoryBlock(source, destination, images); err != nil {
		t.Fatal(err)
	}
	if copied, err := os.ReadFile(destination); err != nil || string(copied) != block {
		t.Fatalf("copy = %q, %v", copied, err)
	}
	if _, ok := images[first]; !ok || len(images) != 2 {
		t.Fatalf("images = %v", images)
	}
	missing := filepath.Join(dir, "none", "backup.jsonl")
	if err := copyHistoryBlock(filepath.Join(dir, "absent.jsonl"), missing, images); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("a missing block wrote %s: %v", missing, err)
	}
}
