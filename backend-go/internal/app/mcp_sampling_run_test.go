package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/tools"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
)

// samplingFixture gives review/reviewer the models openai/large (default) and openai/small, a
// toolset with a sub-agent finder and the fixture MCP server with its ask tool and sampling on.
// Every run, listing and remote start of review/reviewer uses the profile's one shared manager.
func samplingFixture(t *testing.T) *layoutFixture {
	t.Helper()
	f := newLayoutFixture(t)
	body, _ := json.Marshal(appStdioMCPServerFile(t, "", func(file map[string]any) {
		file["transport"].(map[string]any)["env"] = map[string]string{appStdioMCPEnvironment: "1", appStdioMCPAskEnvironment: "1"}
		file["sampling"] = map[string]any{"enabled": true, "max_requests": 50}
	}))
	writeFile(t, filepath.Join(f.root, "mcp", "m", "stdio.json"), body)
	writeFile(t, filepath.Join(f.root, "tools", "review", "stdio.json"), []byte(`{"native":{"async":{},"subagent":{"background_writers":true,"max_background_writers":2,`+
		`"agents":{"finder":{"description":"Uses the server."}}}},"mcp":{"m/stdio":{}}}`))
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large","openai/small"],"model_default":"openai/large","toolset":"review/stdio"}`)
	return f
}

// samplingScript answers the fixture's requests by what their last input item holds. A request
// whose input is a key of asks calls ask under that key's call id (after waiting on its hold,
// when it has one); the output of that call is sent on answers under the key. The sampling
// request itself ("say hi") and anything else get the plain "ok". then, when set for a key,
// is the call the run makes after its ask was answered.
type samplingScript struct {
	t       *testing.T
	holds   map[string]chan struct{}
	entered map[string]chan struct{}
	then    map[string]string
	answers chan [2]string
}

func newSamplingScript(t *testing.T, keys ...string) *samplingScript {
	s := &samplingScript{t: t, holds: map[string]chan struct{}{}, entered: map[string]chan struct{}{}, then: map[string]string{}, answers: make(chan [2]string, 16)}
	for _, key := range keys {
		s.entered[key] = make(chan struct{}, 1)
	}
	return s
}

func (s *samplingScript) answer(model, body string) string {
	if strings.Contains(body, "say hi") {
		return ""
	}
	last := lastInputItem(s.t, body)
	ask := mcpToolNameFor("stdio", "ask")
	if strings.Contains(last, "function_call_output") {
		for key := range s.entered {
			if strings.Contains(last, `"call-`+key+`"`) {
				s.answers <- [2]string{key, last}
				return s.then[key]
			}
		}
		return ""
	}
	for key, entered := range s.entered {
		if strings.Contains(last, "go-"+key) {
			entered <- struct{}{}
			if hold := s.holds[key]; hold != nil {
				<-hold
			}
			return layoutCall("call-"+key, ask, map[string]any{})
		}
	}
	return ""
}

// await returns what the ask of key answered.
func (s *samplingScript) await(key string) string {
	s.t.Helper()
	deadline := time.After(time.Minute)
	for {
		select {
		case got := <-s.answers:
			if got[0] == key {
				return got[1]
			}
			s.answers <- got
			time.Sleep(5 * time.Millisecond)
		case <-deadline:
			s.t.Fatalf("the ask of %s was not answered", key)
			return ""
		}
	}
}

// sharedAsk calls ask on a listing of the shared manager made without a sampler: it shows what
// the manager itself would answer, which a catalog build must not have set.
func sharedAsk(t *testing.T, f *layoutFixture) string {
	t.Helper()
	config := boundRunnerOf(t, f, "review/reviewer").config
	manager, err := f.service.resolver.sharedMCPManager(config)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.LoadTools(context.Background(), config.MCPServers, config.WorkingDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range loaded {
		if tool.Definition.Name == mcpToolNameFor("stdio", "ask") {
			output, err := tool.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{})
			if err != nil {
				t.Fatal(err)
			}
			return string(output)
		}
	}
	t.Fatalf("no ask tool in %d tools", len(loaded))
	return ""
}

func wantSampledBy(t *testing.T, what, got, model string) {
	t.Helper()
	if !strings.Contains(got, "sampled by "+model) {
		t.Fatalf("%s: %s, want sampled by %s", what, got, model)
	}
}

// T-008: a run of review/reviewer with openai/small is held after it listed its tools. Meanwhile
// a tool listing (the default model) and a remote sub-agent start (the default model, whose
// child calls ask itself) build catalogs on the same shared manager. The held run's ask is then
// answered by its own model-small, the remote child's by model-large, and the shared manager
// is left with no sampler of its own.
func TestMCPSamplingOfARunIsNotTakenOverByAListingOrARemoteStart(t *testing.T) {
	f := samplingFixture(t)
	script := newSamplingScript(t, "own", "remote")
	script.holds["own"] = make(chan struct{})
	release := sync.OnceFunc(func() { close(script.holds["own"]) })
	t.Cleanup(release)
	f.script = script.answer

	ran := make(chan string, 1)
	go func() {
		status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile=review%2Freviewer", `{"input":"go-own","model":"openai/small"}`)
		if status != 200 || !strings.Contains(body, "event: completed") {
			ran <- body
			return
		}
		ran <- ""
	}()
	awaitSignal(t, script.entered["own"], "the held run's first request")

	if status, body := f.do(t, http.MethodGet, "/api/sessions/local:demo/tools?profile=review%2Freviewer", ""); status != 200 {
		t.Fatalf("tool listing: %d %s", status, body)
	}
	backend := remoteBackend{resolver: f.service.resolver, runTimeout: time.Minute, runs: newRemoteRunIndex()}
	run, err := backend.StartSubagent(context.Background(), remoteapi.SubagentRequest{Session: "demo", Profile: "review/reviewer", Agent: "finder", Task: "go-remote", RunID: "rr_sampling", Caller: "remote:peer"})
	if err != nil {
		t.Fatal(err)
	}
	wantSampledBy(t, "the remote child's ask", script.await("remote"), "model-large")
	select {
	case <-run.Done():
	case <-time.After(time.Minute):
		t.Fatal("the remote sub-agent did not end")
	}

	release()
	wantSampledBy(t, "the held run's ask after the listing and the remote start", script.await("own"), "model-small")
	if failed := <-ran; failed != "" {
		t.Fatalf("the run: %s", failed)
	}
	if got := sharedAsk(t, f); strings.Contains(got, "sampled by") || !strings.Contains(got, `\"status\":\"error\"`) {
		t.Fatalf("the shared manager's own sampler answered %s, want none set", got)
	}
}

// T-008 (replaces the pin of the overwrite): a profile_message with openai/small is answered by
// model-small for its own run's ask and for a background child it started, also when that
// child asks only after the target's own next run (model-large) listed and asked in between.
func TestProfileMessageModelChoiceSamplesOnlyForItsOwnRunAndItsChildren(t *testing.T) {
	f := samplingFixture(t)
	script := newSamplingScript(t, "own", "look", "bg", "again")
	script.holds["bg"] = make(chan struct{})
	release := sync.OnceFunc(func() { close(script.holds["bg"]) })
	t.Cleanup(release)
	script.then["look"] = layoutCall("call-sub", "subagent", map[string]any{"agent": "finder", "task": "go-bg", "block_ms": 0})
	f.script = script.answer
	ownRun := func(key string) {
		t.Helper()
		if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile=review%2Freviewer", `{"input":"go-`+key+`"}`); status != 200 || !strings.Contains(body, "event: completed") {
			t.Fatalf("own run: %d %s", status, body)
		}
	}

	ownRun("own")
	wantSampledBy(t, "the target's own run", script.await("own"), "model-large")

	messenger := profileMessenger{resolver: f.service.resolver}
	ctx, cancel := context.WithTimeout(callerContext(t, f, context.Background()), time.Minute)
	defer cancel()
	plan, err := messenger.Prepare(ctx, native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "go-look", Model: "openai/small"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantSampledBy(t, "the message's run", script.await("look"), "model-small")
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal("the message did not end")
	}
	if outcome := run.Outcome(); outcome.Err != nil {
		t.Fatalf("message outcome = %+v", outcome)
	}
	awaitSignal(t, script.entered["bg"], "the background child's first request")

	ownRun("again")
	wantSampledBy(t, "the target's next own run, while the message's child still goes", script.await("again"), "model-large")
	release()
	wantSampledBy(t, "the message's background child, after the target's own run", script.await("bg"), "model-small")
	if got := sharedAsk(t, f); strings.Contains(got, "sampled by") {
		t.Fatalf("the shared manager's own sampler answered %s, want none set", got)
	}
}

// T-008 follow-up: a run's read of a resource is a request of the run, so a sampling input
// request in its result is answered by the run's model (openai/small here, not the default),
// while the user's own read of the same resource (no run) is refused and samples nothing.
func TestMCPResourceReadSamplesForTheRunThatReadsAndNotForTheUser(t *testing.T) {
	f := samplingFixture(t)
	writeFile(t, filepath.Join(f.root, "tools", "review", "stdio.json"), []byte(`{"native":{"async":{}},"mcp":{"m/stdio":{"resources":true}}}`))
	readTool := strings.Replace(mcpToolNameFor("stdio", "resources/read"), "resources/read", "resources_read", 1)
	outputs := make(chan string, 4)
	f.script = func(_ string, body string) string {
		if strings.Contains(body, "say hi") {
			return ""
		}
		last := lastInputItem(t, body)
		if strings.Contains(last, "function_call_output") {
			outputs <- last
			return ""
		}
		if strings.Contains(last, "go-read") {
			return layoutCall("call-read", readTool, map[string]any{"uri": "doc://ask"})
		}
		return ""
	}
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile=review%2Freviewer", `{"input":"go-read","model":"openai/small"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("the run: %d %s", status, body)
	}
	select {
	case got := <-outputs:
		wantSampledBy(t, "the run's resource read", got, "model-small")
	case <-time.After(time.Minute):
		t.Fatal("the run's resource read was not answered")
	}

	read, err := f.service.resolver.ReadMCPResource(context.Background(), "local:demo", "review/reviewer", httpapi.MCPResourceRequest{Server: "stdio", URI: "doc://ask"})
	if err == nil || strings.Contains(read.Text, "sampled by") {
		t.Fatalf("the user's own read = %+v %v, want it refused with no sampling", read, err)
	}
	// Control: the refusal is the missing run; a sampler set on the shared manager answers it.
	manager, err := f.service.resolver.sharedMCPManager(boundRunnerOf(t, f, "review/reviewer").config)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSampler(func(context.Context, mcpclient.SamplingRequest) (string, string, error) {
		return "hi", "model-control", nil
	})
	read, err = f.service.resolver.ReadMCPResource(context.Background(), "local:demo", "review/reviewer", httpapi.MCPResourceRequest{Server: "stdio", URI: "doc://ask"})
	manager.SetSampler(nil)
	if err != nil || !strings.Contains(read.Text, "sampled by model-control") {
		t.Fatalf("the user's read with a manager sampler = %+v %v, want sampled by model-control", read, err)
	}
}
