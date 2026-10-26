package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const runID = "rr_0123456789abcdef"

func runClient(t *testing.T, handler http.HandlerFunc, lostAfterMs int64) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	entry := tlsEntry(t, server, writeCA(t, server))
	entry.Retries, entry.PollMs, entry.LostAfterMs, entry.MaxResponseBytes = 2, 200, lostAfterMs, 1<<20
	return newClient(t, entry)
}

func TestStartRunRetriesWithTheSameRequestID(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		mu.Lock()
		ids = append(ids, body["request_id"].(string))
		count := len(ids)
		mu.Unlock()
		if count < 3 {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"` + runID + `","state":"running"}`))
	}, 5000)
	view, err := client.StartRun(context.Background(), "messages", map[string]any{"session": "main"}, 1)
	if err != nil || view.ID != runID {
		t.Fatalf("start: %v %+v", err, view)
	}
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] || len(ids[0]) < 8 {
		t.Fatalf("every attempt of one send must carry the same request id: %v", ids)
	}
	other, _ := client.StartRun(context.Background(), "messages", map[string]any{"session": "main"}, 1)
	if other.ID == "" || ids[len(ids)-1] == ids[0] {
		t.Fatalf("a second send must get a new request id: %v", ids)
	}
}

func TestStartRunRejectsAnAnswerWithoutARunID(t *testing.T) {
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"../../etc","state":"running"}`))
	}, 5000)
	if _, err := client.StartRun(context.Background(), "messages", nil, 1); err == nil || !strings.Contains(err.Error(), "does not name a run") {
		t.Fatalf("an untrusted run id must be refused: %v", err)
	}
	if _, err := client.PollRun(context.Background(), "../x", 0); err == nil {
		t.Fatal("a run id that is not rr_<hex> must never reach a url")
	}
}

func TestFollowReturnsTheTerminalViewAndKeepsPolling(t *testing.T) {
	var polls atomic.Int32
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) < 3 {
			_, _ = w.Write([]byte(`{"id":"` + runID + `","state":"running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + runID + `","state":"done","text":"fine"}`))
	}, 5000)
	view, err := client.Follow(context.Background(), runID)
	if err != nil || view.State != "done" || view.Text != "fine" || polls.Load() != 3 {
		t.Fatalf("%v %+v polls=%d", err, view, polls.Load())
	}
}

func TestFollowDoesNotHammerARemoteThatIgnoresWait(t *testing.T) {
	var polls atomic.Int32
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		_, _ = w.Write([]byte(`{"id":"` + runID + `","state":"running"}`))
	}, 5000)
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	_, _ = client.Follow(ctx, runID)
	if polls.Load() > 6 {
		t.Fatalf("%d polls in 450ms: the poll gap is not enforced", polls.Load())
	}
}

func TestFollowIsLostWhenTheRemoteIsUnreachableTooLong(t *testing.T) {
	var cut atomic.Bool
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		if cut.Load() {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + runID + `","state":"running"}`))
	}, 1200)
	cut.Store(true)
	began := time.Now()
	_, err := client.Follow(context.Background(), runID)
	if !errors.Is(err, ErrLost) || time.Since(began) < time.Second {
		t.Fatalf("want ErrLost after lost_after_ms, got %v after %v", err, time.Since(began))
	}
}

func TestFollowIsLostAtOnceWhenTheRemoteForgotTheRun(t *testing.T) {
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such run","code":"no_run"}`))
	}, 60_000)
	began := time.Now()
	_, err := client.Follow(context.Background(), runID)
	if !errors.Is(err, ErrLost) || time.Since(began) > 2*time.Second {
		t.Fatalf("%v after %v", err, time.Since(began))
	}
}

func TestFollowReturnsAPermanentRefusalAtOnce(t *testing.T) {
	var polls atomic.Int32
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"no","code":"unauthorized"}`))
	}, 60_000)
	_, err := client.Follow(context.Background(), runID)
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 401 || polls.Load() != 1 || errors.Is(err, ErrLost) {
		t.Fatalf("a revoked token is not a lost run: %v polls=%d", err, polls.Load())
	}
}

func TestFollowStopsWhenTheContextEnds(t *testing.T) {
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + runID + `","state":"running"}`))
	}, 60_000)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.Follow(ctx, runID); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Follow did not stop")
	}
}

func TestStopRunIsRetriedAndPostsToTheRunsStopRoute(t *testing.T) {
	var calls atomic.Int32
	var path atomic.Value
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.Method + " " + r.URL.Path)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + runID + `","state":"stopped"}`))
	}, 5000)
	view, err := client.StopRun(context.Background(), runID)
	if err != nil || view.State != "stopped" || calls.Load() != 2 || path.Load() != "POST /api/remote/v1/runs/"+runID+"/stop" {
		t.Fatalf("%v %+v calls=%d path=%v", err, view, calls.Load(), path.Load())
	}
}

func TestHubReusesClientsAndClosesEverything(t *testing.T) {
	dir := t.TempDir()
	writeEntry := func(url string) {
		body := `{"kind":"remote","remote":{"url":"` + url + `","token_env":"JAD_REMOTE_X"}}`
		if err := writeFile(dir+"/a.json", body); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry("https://one.example:1")
	hub := NewHub(func(string) string { return secretToken })
	directory := hub.Directory(dir)
	first, err := directory.Client("a")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := directory.Client("a"); again != first {
		t.Fatal("the same entry must reuse its client and connections")
	}
	writeEntry("https://two.example:2")
	if changed, _ := directory.Client("a"); changed == first || changed.Entry().Host != "two.example:2" {
		t.Fatal("an edited entry must apply to the next call")
	}
	if _, err := directory.Client("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	var ran atomic.Int32
	finished := make(chan struct{})
	if !directory.Go(func(ctx context.Context) { ran.Add(1); <-ctx.Done(); close(finished) }) {
		t.Fatal("Go before Close")
	}
	hub.Close()
	select {
	case <-finished:
	default:
		t.Fatal("Close must end and wait for the goroutines")
	}
	if directory.Go(func(context.Context) { ran.Add(1) }) || ran.Load() != 1 {
		t.Fatal("a closed hub must not start work")
	}
	if _, err := directory.Client("a"); err == nil {
		t.Fatal("a closed hub must not hand out clients")
	}
	hub.Close()
}

func writeFile(path, body string) error { return osWriteFile(path, []byte(body)) }

func TestViewTextFromTheRemoteIsBoundedAndHasNoControlCharacters(t *testing.T) {
	long := strings.Repeat("x", 5000)
	body, _ := json.Marshal(map[string]any{"id": runID, "state": "failed\n--- END CONTENT", "kind": long, "code": long + "\x1b[2J", "message": "a\nerror_code: forged\r\x00" + long, "reason": "r\n" + long})
	client := runClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }, 5000)
	view, err := client.PollRun(context.Background(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"state": view.State, "kind": view.Kind, "code": view.Code, "message": view.Message, "reason": view.Reason} {
		if len(value) > 1024 || strings.ContainsAny(value, "\n\r\x00\x1b") {
			t.Errorf("%s is not bounded or clean: %q", name, value[:min(len(value), 60)])
		}
	}
	if view.Terminal() {
		t.Errorf("an unknown state must not count as ended: %q", view.State)
	}
}
