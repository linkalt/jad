package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// heldRemoteProvider answers every request to the parent model only after the current gate is
// closed. Each arrival is sent on reached, so a test can wait until a run is held at the provider.
// newGate arms a fresh gate for the next round, so one provider serves several rounds.
type heldRemoteProvider struct {
	mu      sync.Mutex
	gate    chan struct{}
	reached chan struct{}
	server  *httptest.Server
}

func newHeldRemoteProvider(n int) *heldRemoteProvider {
	return &heldRemoteProvider{gate: make(chan struct{}), reached: make(chan struct{}, n)}
}

// newGate arms a fresh gate and returns a func that opens it.
func (p *heldRemoteProvider) newGate() func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gate = make(chan struct{})
	gate := p.gate
	return sync.OnceFunc(func() { close(gate) })
}

func (p *heldRemoteProvider) currentGate() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gate
}

func (p *heldRemoteProvider) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		gate := p.currentGate()
		p.reached <- struct{}{}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-1","status":"completed","model":"parent-model","output":[]}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

// newHeldRemoteFixture builds one session with n profiles, dev/p0 to dev/p<n-1>, all on the
// parent model that heldRemoteProvider holds. Each profile has its own transcript, so each
// remote message is its own run.
// newHeldRemoteConfig returns the config of the held fixture and its provider.
func newHeldRemoteConfig(t *testing.T, n int) (Config, *heldRemoteProvider) {
	t.Helper()
	provider := newHeldRemoteProvider(n)
	server := httptest.NewServer(provider.handler())
	provider.server = server
	t.Cleanup(server.Close)
	open := provider.newGate()
	t.Cleanup(open)
	profiles := map[string]map[string]any{}
	for i := 0; i < n; i++ {
		profiles[fmt.Sprintf("dev/p%d", i)] = map[string]any{"models": []string{"openai/parent"}, "model_default": "openai/parent"}
	}
	spec := maintenanceSpec(map[string]any{"max_tool_rounds": 4}, "")
	spec.Server = map[string]any{"max_body_bytes": 4096, "sse_max_buffered_bytes": 4096}
	spec.SessionID = "demo"
	spec.Models = map[string]map[string]any{"openai/parent": {"provider": "openai", "model": "parent-model", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": server.URL + "/v1"}}
	spec.Profiles = profiles
	config, _ := layoutConfig(t, spec, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	return config, provider
}

// newHeldRemoteFixture builds the held server with a run timeout of runTimeoutMs.
func newHeldRemoteFixture(t *testing.T, n int, runTimeoutMs int64) (*remoteServer, *heldRemoteProvider) {
	t.Helper()
	config, provider := newHeldRemoteConfig(t, n)
	return startHeldRemoteServer(t, config, n, runTimeoutMs), provider
}

// TestRemoteMessageHeldSettle checks the held state before anything is counted: n remote
// message runs each reach the provider and stay running until release is closed.
func TestRemoteMessageHeldSettle(t *testing.T) {
	const n = 3
	server, provider := newHeldRemoteFixture(t, n, 60_000)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		status, body := server.call(t, "POST", "/api/remote/v1/messages", remoteTestSecret, map[string]any{
			"session": "demo", "profile": fmt.Sprintf("dev/p%d", i), "message": "hold", "request_id": fmt.Sprintf("req-held-%d", i)})
		if status != 202 {
			t.Fatalf("start %d: %d %v", i, status, body)
		}
		ids = append(ids, body["id"].(string))
	}
	for i := 0; i < n; i++ {
		select {
		case <-provider.reached:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d runs reached the provider", i, n)
		}
	}
	for _, id := range ids {
		status, body := server.call(t, "GET", "/api/remote/v1/runs/"+id, remoteTestSecret, nil)
		if status != 200 || body["state"] != "running" {
			t.Fatalf("run %s: %d %v, want running", id, status, body)
		}
	}
}

// startHeldRemoteServer is startRemoteServer with the run caps raised to n, so n live runs
// from one token are not refused. startRemoteServer keeps its own caps for the other tests.
func startHeldRemoteServer(t *testing.T, config Config, n int, runTimeoutMs int64) *remoteServer {
	t.Helper()
	table := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(table, []byte(tokenTable(tokenRow("peer", remoteTestSecret, fullGrant))), 0o600); err != nil {
		t.Fatal(err)
	}
	config.RemoteAPI = sessionfs.RemoteAPISettings{Enabled: true, TokensFile: table, MaxHops: 3, MaxRuns: n + 16, MaxRunsPerToken: n + 16, RunTimeoutMs: runTimeoutMs, RunRetentionMs: 60_000, WaitMaxMs: 5_000, BodyMaxBytes: 1 << 20, ResultMaxBytes: 64 << 10}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	handlers := service.HandlersFor(config)
	gate := &outageGate{next: handlers.Main}
	server := httptest.NewServer(gate)
	t.Cleanup(server.Close)
	return &remoteServer{f: subagentFixture{config: config}, config: config, service: service, url: server.URL, table: table, handlers: handlers, server: server, gate: gate}
}

// TestRemoteMessageTimesOutWhenHeld checks the timeout path: a held run past RunTimeoutMs is stopped
// and reported as timed_out, with the timer stopped on end.
func TestRemoteMessageTimesOutWhenHeld(t *testing.T) {
	server, _ := newHeldRemoteFixture(t, 1, 200)
	status, body := server.call(t, "POST", "/api/remote/v1/messages", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/p0", "message": "hold", "request_id": "req-timeout-0"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	done := server.waitDone(t, body["id"].(string))
	if done["state"] != "timed_out" {
		t.Fatalf("outcome: %v, want timed_out", done)
	}
}

// TestRemoteMessageGoroutinesSettleAfterRunsEnd checks the goroutine delta: after a warm-up round,
// n held remote runs end, and the goroutine count settles back to its count before them. Idle
// HTTP connections are closed first, so only run goroutines count.
func TestRemoteMessageGoroutinesSettleAfterRunsEnd(t *testing.T) {
	const n = 20
	server, provider := newHeldRemoteFixture(t, n, 60_000)
	round := func(prefix string) {
		open := provider.newGate()
		ids := make([]string, 0, n)
		for i := 0; i < n; i++ {
			status, body := server.call(t, "POST", "/api/remote/v1/messages", remoteTestSecret, map[string]any{
				"session": "demo", "profile": fmt.Sprintf("dev/p%d", i), "message": "hold", "request_id": fmt.Sprintf("%s-req-%04d", prefix, i)})
			if status != 202 {
				t.Fatalf("start %d: %d %v", i, status, body)
			}
			ids = append(ids, body["id"].(string))
		}
		for i := 0; i < n; i++ {
			<-provider.reached
		}
		open()
		for _, id := range ids {
			if done := server.waitDone(t, id); done["state"] == "running" {
				t.Fatalf("run %s still running", id)
			}
		}
	}
	round("warmup")
	time.Sleep(200 * time.Millisecond)
	server.server.Client().CloseIdleConnections()
	before := runtime.NumGoroutine()
	round("measured")
	server.server.Client().CloseIdleConnections()
	if after := settledGoroutineCount(before); after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d after %d held remote runs\n%s", before, after, n, buffer[:runtime.Stack(buffer, true)])
	}
}

// TestScaleRemoteMessageHeldPeak measures the goroutines held by n remote message runs while all of
// them are running, then after they end. Labelled (remote watcher): the count includes the
// remote watcher goroutine per run, which the seam B change removes. Gated by JAD_SCALE_MEASURE=1.
func TestScaleRemoteMessageHeldPeak(t *testing.T) {
	if os.Getenv("JAD_SCALE_MEASURE") != "1" {
		t.Skip("set JAD_SCALE_MEASURE=1 to measure the remote message held peak")
	}
	for _, n := range []int{100, 500, 1000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			server, provider := newHeldRemoteFixture(t, n, 60_000)
			baseline := runtime.NumGoroutine()
			open := provider.newGate()
			ids := make([]string, 0, n)
			for i := 0; i < n; i++ {
				status, body := server.call(t, "POST", "/api/remote/v1/messages", remoteTestSecret, map[string]any{
					"session": "demo", "profile": fmt.Sprintf("dev/p%d", i), "message": "hold", "request_id": fmt.Sprintf("peak-req-%04d", i)})
				if status != 202 {
					t.Fatalf("start %d: %d %v", i, status, body)
				}
				ids = append(ids, body["id"].(string))
			}
			for i := 0; i < n; i++ {
				<-provider.reached
			}
			peak := runtime.NumGoroutine()
			open()
			for _, id := range ids {
				if done := server.waitDone(t, id); done["state"] == "running" {
					t.Fatalf("run %s still running", id)
				}
			}
			server.server.Client().CloseIdleConnections()
			provider.server.CloseClientConnections()
			settled := settledGoroutineCount(baseline)
			t.Logf("label=remote_watcher n=%d baseline=%d peak_running=%d peak_per_run=%.2f settled=%d", n, baseline, peak, float64(peak-baseline)/float64(n), settled)
			// Measured at c21d26e (goroutine watcher): 6.02 per run at n=100. After the change: 5.02 at n=100,
			// 5.00 at n=500, 3.75 at n=1000. The bound sits between them, so the watcher fails it.
			if perRun := float64(peak-baseline) / float64(n); perRun > 5.5 {
				t.Fatalf("n=%d: %.2f goroutines held per remote run, want at most 5.5", n, perRun)
			}
			if settled > baseline {
				t.Fatalf("n=%d: %d goroutines after the runs ended, baseline %d", n, settled, baseline)
			}
		})
	}
}

// TestRemoteMessageBusyStartLeavesNoRun checks the synchronous busy outcome: while dev/p0's
// transcript is held by a remote run, further messages to dev/p0 are refused with profile_busy and
// return no run id. SendMessage returns ErrBusy before StartProfileRun can register OnEnd, so no
// hook or done channel is created, and the refusals leave no goroutines behind.
func TestRemoteMessageBusyStartLeavesNoRun(t *testing.T) {
	const refusals = 50
	server, provider := newHeldRemoteFixture(t, 1, 60_000)
	open := provider.newGate()
	t.Cleanup(open)
	status, body := server.call(t, "POST", "/api/remote/v1/messages", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/p0", "message": "hold", "request_id": "req-busy-held"})
	if status != 202 {
		t.Fatalf("start held run: %d %v", status, body)
	}
	held := body["id"].(string)
	<-provider.reached
	server.server.Client().CloseIdleConnections()
	before := runtime.NumGoroutine()
	for i := 0; i < refusals; i++ {
		status, body := server.call(t, "POST", "/api/remote/v1/messages", remoteTestSecret, map[string]any{
			"session": "demo", "profile": "dev/p0", "message": "again", "request_id": fmt.Sprintf("req-busy-%03d", i)})
		if status != 409 || body["code"] != "profile_busy" {
			t.Fatalf("refusal %d: %d %v, want 409 profile_busy", i, status, body)
		}
		if _, ok := body["id"]; ok {
			t.Fatalf("refusal %d returned a run id: %v", i, body)
		}
	}
	server.server.Client().CloseIdleConnections()
	if after := settledGoroutineCount(before); after > before {
		t.Fatalf("goroutines %d -> %d after %d busy refusals", before, after, refusals)
	}
	open()
	if done := server.waitDone(t, held); done["state"] == "running" {
		t.Fatalf("held run still running")
	}
}
