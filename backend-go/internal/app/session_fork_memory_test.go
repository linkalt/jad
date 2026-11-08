package app

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
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

// retainedBy reports the highest retained heap above the starting one while call ran.
// A sample is the lowest HeapAlloc of three forced collections in a row: what a collection
// leaves is what is reachable plus the garbage the running fork allocated while it marked
// (floating garbage), and the lowest of three drops that, whose size follows the CPU the
// collector gets and so the load of the machine. The collector runs at GOGC=1 meanwhile,
// so the fork cannot out-allocate a collection by more than a sliver. A fork that buffers
// a transcript keeps it reachable until it returns, so every sample sees it; a line buffer
// a streaming fork drops at once may fall between samples, which is fine: it is not held.
func retainedBy(call func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	return sampleRetained(call, liveHeap)
}

// liveHeap is one sample of retainedBy: the lowest HeapAlloc of three forced collections in a row.
func liveHeap() uint64 {
	low := uint64(math.MaxUint64)
	for range 3 {
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		low = min(low, stats.HeapAlloc)
	}
	return low
}

// sampleRetained runs call while a goroutine takes samples one after another, and returns the
// highest sample above the one taken before call. The sampler is stopped, and waited for, however
// call ends: a t.Fatal inside it (runtime.Goexit) or a panic included. Otherwise a failed fork would
// leave it forcing collections for the rest of the package's tests.
func sampleRetained(call func(), sample func() uint64) uint64 {
	base := sample()
	done := make(chan struct{})
	sampled := make(chan uint64, 1)
	go func() {
		peak := base
		for {
			select {
			case <-done:
				sampled <- peak
				return
			default:
			}
			peak = max(peak, sample())
		}
	}()
	var once sync.Once
	var highest uint64
	stop := func() {
		once.Do(func() {
			close(done)
			highest = <-sampled
		})
	}
	defer stop()
	call()
	stop()
	return highest - base
}

// A measured call that ends with t.Fatal (runtime.Goexit) still stops the sampler: once
// sampleRetained has returned, no sample is taken any more.
func TestSampleRetainedStopsTheSamplerWhenTheCallExits(t *testing.T) {
	var samples atomic.Int64
	sample := func() uint64 {
		samples.Add(1)
		time.Sleep(time.Millisecond)
		return 0
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		sampleRetained(func() {
			time.Sleep(5 * time.Millisecond)
			runtime.Goexit()
		}, sample)
	}()
	<-exited
	after := samples.Load()
	time.Sleep(50 * time.Millisecond)
	if more := samples.Load() - after; more != 0 {
		t.Fatalf("the sampler took %d samples after the measured call had exited", more)
	}
	if got := sampleRetained(func() {}, func() uint64 { return 7 }); got != 0 {
		t.Fatalf("a flat sample gave %d, want 0", got)
	}
}

// A fork streams the transcript it copies and searches a message one turn at a time, so its
// live memory follows the largest turn, not the transcript: with 32 turns of 512 KiB, the
// peak stays under half the transcript (it was about twice the transcript before).
func TestForkHoldsATurnInMemoryNotTheTranscript(t *testing.T) {
	f := newHeldResetFixture(t)
	close(f.release)
	f.runTurn("local:one", "first")
	size := growTranscript(t, f, "one", 32, 512*1024)
	ids, _ := f.userMessages("local:one")
	whole := retainedBy(func() {
		if status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:copy"}`); status != http.StatusCreated || !strings.Contains(body, `"turns":33`) {
			t.Fatalf("whole fork: %d %s", status, body)
		}
	})
	byMessage := retainedBy(func() {
		if status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:msg","message_id":"`+ids[len(ids)-1]+`","keep_turn":false}`); status != http.StatusCreated || !strings.Contains(body, `"turns":32`) {
			t.Fatalf("message fork: %d %s", status, body)
		}
	})
	t.Logf("transcript %d bytes; peak retained heap of the whole fork %d, of the message fork %d", size, whole, byMessage)
	if limit := uint64(size / 2); whole > limit || byMessage > limit {
		t.Fatalf("fork peak heap %d (whole) and %d (by message) for a %d byte transcript; want both under %d", whole, byMessage, size, limit)
	}
	source := f.transcript("one")
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
