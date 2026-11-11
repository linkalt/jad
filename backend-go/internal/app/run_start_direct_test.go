package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// startRunFixture is a resolver and a session configuration whose agent model is a fake provider
// that answers every request with one final message, so StartRun can be driven to its end.
type startRunFixture struct {
	resolver *Resolver
	config   sessionfs.SessionConfig
	agent    sessionfs.NativeSubagentAgent
	storage  string
	requests *atomic.Int32
}

func newStartRunFixture(t *testing.T, answer string) startRunFixture {
	t.Helper()
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range messageEvents(answer, 7) {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(event), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, event)
		}
	}))
	t.Cleanup(server.Close)
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	location, err := sessionfs.Resolve(loaded.Roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(loaded.Roots, map[string]string{"FIXTURE_KEY": "key"}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	storage := t.TempDir()
	config.NativeSubagent = &sessionfs.NativeSubagentSettings{StorageDir: storage}
	model := filepath.Join(t.TempDir(), "child.json")
	body, _ := json.Marshal(map[string]any{"provider": "openai", "model": "child-model", "api_key_env": "FIXTURE_KEY", "base_url": server.URL + "/v1"})
	if err := os.WriteFile(model, body, 0o644); err != nil {
		t.Fatal(err)
	}
	agent := sessionfs.NativeSubagentAgent{Name: "explore", ModelPath: model, MaxToolRounds: 2}
	return startRunFixture{resolver: resolver, config: config, agent: agent, storage: storage, requests: requests}
}

// TestStartRunRunsAnEphemeralRunToItsEnd drives StartRun directly (R2 deferred test): an
// ephemeral run with its own model makes one provider request, returns the final answer with its
// rounds and tokens, names its folder once, and leaves the task and the answer in that folder's
// messages.jsonl.
func TestStartRunRunsAnEphemeralRunToItsEnd(t *testing.T) {
	f := newStartRunFixture(t, "the answer is 42")
	catalog, err := tools.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	handle, err := f.resolver.StartRun(context.Background(), RunSpec{
		Kind:         RunEphemeral,
		Template:     RunTemplate{Base: f.config, Agent: f.agent},
		Input:        json.RawMessage(`"find the answer"`),
		CallID:       "call_7",
		Tools:        catalog,
		OnTranscript: func(dir string) { dirs = append(dirs, dir) },
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if result := handle.Result(); result != (RunResult{Text: "the answer is 42", Rounds: 1, Tokens: 7}) {
		t.Fatalf("result = %+v", result)
	}
	if got := f.requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
	if len(dirs) != 1 || filepath.Dir(dirs[0]) != f.storage || !strings.HasPrefix(filepath.Base(dirs[0]), "call_7-") {
		t.Fatalf("OnTranscript got %v, want one folder call_7-<hex> under %s", dirs, f.storage)
	}
	transcript, err := os.ReadFile(filepath.Join(dirs[0], "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"find the answer", "the answer is 42"} {
		if !strings.Contains(string(transcript), want) {
			t.Errorf("messages.jsonl lacks %q:\n%s", want, transcript)
		}
	}
}

// TestStartRunPanicIsRecoveredOnTheManagersGoroutine (R2 deferred test, needed before R5): a panic
// raised inside StartRun's body, through the real launcher adapter, is recovered by the sub-agent
// manager's runLauncher and the call reports a failed run. The panic is only recoverable there
// because StartRun runs on the goroutine that runLauncher deferred its recover on: if StartRun
// ever ran its body on a goroutine of its own, this panic would end the test binary.
func TestStartRunPanicIsRecoveredOnTheManagersGoroutine(t *testing.T) {
	f := newStartRunFixture(t, "never read")
	real := f.resolver.subagentLauncherFor(f.config)
	var reached atomic.Bool
	launcher := subagentLauncherFunc(func(ctx context.Context, request native.SubagentRequest) (native.SubagentResult, error) {
		next := request.OnTranscript
		request.OnTranscript = func(dir string) {
			if next != nil {
				next(dir)
			}
			reached.Store(true)
			panic("injected panic inside StartRun")
		}
		return real.Run(ctx, request)
	})
	settings := subagentLifecycleSettings(f.storage)
	agent := f.agent
	agent.TimeoutMS = 60_000
	settings.Agents = []sessionfs.NativeSubagentAgent{agent}
	manager := native.NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := native.NewSubagentToolWithManager(settings, subagentLifecycleParent(t), launcher, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	output := subagentOutput(t, tool)
	if !reached.Load() {
		t.Fatalf("the run never reached the injected panic:\n%s", output)
	}
	if !strings.Contains(output, `"status":"error"`) || !strings.Contains(output, "sub-agent launcher panicked") {
		t.Fatalf("output = %s, want a failed run naming the recovered panic", output)
	}
	if f.requests.Load() != 0 {
		t.Fatalf("provider requests = %d, want 0: the panic came before the first request", f.requests.Load())
	}
}
