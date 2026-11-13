package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// messageWorkspaces is two workspaces, a (where the callers are) and b, with a scripted
// provider: sessions s1 in both and only-b in b; a has dev/base, b has dev/base and
// review/reviewer (design §30).
type messageWorkspaces struct {
	a, b    string
	service *Service
	api     *httptest.Server
	mu      sync.Mutex
	// script returns the provider's output item for a request, or "" for the default answer.
	script func(model, body string) string
	// holdOn makes a request whose body has this text wait on gate; reached is told first.
	holdOn  string
	gate    chan struct{}
	reached chan struct{}
}

type messageWorkspaceOptions struct {
	aWorkspace, bWorkspace string // workspace.json texts; {} by default
	unlistedA              bool   // a is only the start directory, not listed in global.json
	unlistedB              bool   // b exists but global.json does not list it
	aToolset               string // the toolset of a's dev/base, coding/none by default
	bReviewer              string // b's review/reviewer file, a plain profile by default
	bToolset               string // the toolset of b's dev/base, coding/none by default
	aProfile               string // a's dev/base file when the toolset alone is not enough
	exec                   bool   // the tool definition of exec is loaded
	bID                    string // the workspace id of b, "b" by default
}

func startMessageWorkspaces(t *testing.T, options messageWorkspaceOptions) *messageWorkspaces {
	t.Helper()
	w := &messageWorkspaces{}
	provider := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &request)
		w.mu.Lock()
		script, holdOn, gate, reached := w.script, w.holdOn, w.gate, w.reached
		w.mu.Unlock()
		if holdOn != "" && strings.Contains(string(raw), holdOn) {
			reached <- struct{}{}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
		if script != nil {
			if scripted := script(request.Model, string(raw)); scripted != "" {
				item = scripted
			}
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			`{"type":"response.completed","response":{"id":"r1","status":"completed","model":"gpt-test","output":[]}}`,
			"[DONE]",
		} {
			_, _ = fmt.Fprintf(rw, "data: %s\n\n", event)
		}
	}))
	t.Cleanup(provider.Close)
	base := t.TempDir()
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	home := filepath.Join(base, "home")
	global := filepath.Join(home, ".jad")
	w.a, w.b = filepath.Join(base, "a"), filepath.Join(base, "b")
	put := func(path, text string) { writeWorkspaceTestFile(t, path, text) }
	orDefault := func(value, fallback string) string {
		if value == "" {
			return fallback
		}
		return value
	}
	definitions := []string{"profile_message", "async"}
	if options.exec {
		definitions = append(definitions, "exec")
	}
	var paths []string
	for _, name := range definitions {
		definition, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, `"`+definition+`"`)
	}
	bID := orDefault(options.bID, "b")
	var entries []string
	if !options.unlistedA {
		entries = append(entries, `{"id":"a","path":"`+w.a+`"}`)
	}
	if !options.unlistedB {
		entries = append(entries, `{"id":"`+bID+`","path":"`+w.b+`"}`)
	}
	workspaces := strings.Join(entries, ",")
	put(filepath.Join(global, "global.json"), `{"workspaces":[`+workspaces+`],"libraries":{"tool_definition_paths":[`+strings.Join(paths, ",")+`]}}`)
	put(filepath.Join(global, "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY","base_url":"`+provider.URL+`/v1"}`)
	put(filepath.Join(global, "tools", "coding", "none.json"), `{"native":{},"mcp":{}}`)
	put(filepath.Join(global, "tools", "coding", "talk.json"), `{"native":{"profile_message":{"timeout_ms":60000}},"mcp":{}}`)
	put(filepath.Join(global, "tools", "coding", "exec.json"), `{"native":{"exec":{"spill":{"enabled":false}}},"mcp":{}}`)
	put(filepath.Join(w.a, ".jad", "workspace.json"), orDefault(options.aWorkspace, `{}`))
	put(filepath.Join(w.b, ".jad", "workspace.json"), orDefault(options.bWorkspace, `{}`))
	for _, dir := range []string{w.a, w.b} {
		put(filepath.Join(dir, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	}
	put(filepath.Join(w.b, ".jad", "sessions", "only-b.json"), `{"schema_version":1}`)
	put(filepath.Join(w.a, ".jad", "profiles", "dev", "base.json"), orDefault(options.aProfile, `{"models":["openai/default"],"model_default":"openai/default","toolset":"`+orDefault(options.aToolset, "coding/none")+`"}`))
	put(filepath.Join(w.b, ".jad", "profiles", "dev", "base.json"), `{"models":["openai/default"],"model_default":"openai/default","toolset":"`+orDefault(options.bToolset, "coding/none")+`"}`)
	put(filepath.Join(w.b, ".jad", "profiles", "review", "reviewer.json"), orDefault(options.bReviewer, `{"models":["openai/default"],"model_default":"openai/default"}`))
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: w.a, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "HOME": home, "FIXTURE_KEY": "key"}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := newServiceForLayout(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.service = service
	w.api = httptest.NewServer(service.Handler)
	t.Cleanup(func() { service.Close(); w.api.Close() })
	return w
}

func (w *messageWorkspaces) messenger() profileMessenger {
	return profileMessenger{resolver: w.service.resolver}
}

// callerCtx is a context of the run of a caller whose transcript is path.
func callerCtx(path string) context.Context {
	return context.WithValue(context.Background(), callerMessagesPathKey{}, path)
}

const (
	ownerA = "a:s1\x00dev/base"
	ownerB = "b:s1\x00dev/base"
)

func (w *messageWorkspaces) prepare(t *testing.T, owner, session, target string, change ...func(*native.ProfileMessageRequest)) (native.ProfileMessagePlan, error) {
	t.Helper()
	request := native.ProfileMessageRequest{OwnerKey: owner, Session: session, Target: target, Message: "hello"}
	for _, apply := range change {
		apply(&request)
	}
	return w.messenger().Prepare(callerCtx(filepath.Join(t.TempDir(), "caller.jsonl")), request)
}

func (w *messageWorkspaces) send(t *testing.T, plan native.ProfileMessagePlan) native.ProfileMessageOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run, err := plan.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal("the message did not end")
	}
	return run.Outcome()
}

func (w *messageWorkspaces) transcript(dir, session, profile string) string {
	data, _ := os.ReadFile(filepath.Join(dir, ".jad", "state", session, profile, "messages.jsonl"))
	return string(data)
}

// Both workspaces are listed: the message runs the profile of b in b's own state (its transcript is in
// b's state folder, the caller's workspace gets nothing), and the plan is keyed by the
// qualified session, so the same name in a is another queue and another node of the chain.
func TestProfileMessageReachesAProfileOfAnotherListedWorkspace(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{})
	plan, err := w.prepare(t, ownerA, "b:s1", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if key := plan.(*profileMessagePlan).key; key != "b:s1\x00review/reviewer" {
		t.Fatalf("queue key = %q", key)
	}
	if outcome := w.send(t, plan); outcome.Err != nil || !strings.Contains(outcome.Text, "ok") {
		t.Fatalf("outcome = %+v", outcome)
	}
	if got := w.transcript(w.b, "s1", "review/reviewer"); !strings.Contains(got, "hello") {
		t.Fatalf("b's transcript: %q", got)
	}
	for _, dir := range []string{w.a} {
		if _, err := os.Stat(filepath.Join(dir, ".jad", "state", "s1", "review")); err == nil {
			t.Fatal("the caller's workspace got a state folder for the target")
		}
	}
	// The same profile name in the caller's own workspace is not a cycle of the chain, and its queue is another.
	plan, err = w.prepare(t, ownerA, "b:s1", "dev/base")
	if err != nil {
		t.Fatalf("dev/base of b from dev/base of a: %v", err)
	}
	if key := plan.(*profileMessagePlan).key; key != "b:s1\x00dev/base" {
		t.Fatalf("queue key = %q", key)
	}
	// A session only b holds is reached with its workspace, never bare.
	if _, err := w.prepare(t, ownerA, "b:only-b", "review/reviewer"); err != nil {
		t.Fatalf("b:only-b: %v", err)
	}
	if _, err := w.prepare(t, ownerA, "only-b", "review/reviewer"); !errors.Is(err, native.ErrProfileUnknown) {
		t.Fatalf("bare only-b: %v", err)
	}
}

// A workspace that is not listed, one that does not exist and a session that does not exist are
// the same refusal; an unlisted caller reaches no other workspace; a bare id stays in the
// caller's workspace even when b holds the id too; listing is the only opt-in, so the target
// needs nothing in its own workspace.json and the direction does not matter.
func TestProfileMessageToAnotherWorkspaceNeedsBothListed(t *testing.T) {
	for name, options := range map[string]messageWorkspaceOptions{
		"the caller is not listed": {unlistedA: true},
		"the target is not listed": {unlistedB: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := startMessageWorkspaces(t, options)
			for _, session := range []string{"b:s1", "b:only-b", "b:nope", "c:s1"} {
				if _, err := w.prepare(t, ownerA, session, "review/reviewer"); !errors.Is(err, native.ErrProfileUnknown) {
					t.Errorf("%s: %v", session, err)
				}
			}
		})
	}
	w := startMessageWorkspaces(t, messageWorkspaceOptions{})
	// s1 is held by both: bare means a's, which has no review/reviewer (b's has), so it is unknown.
	if _, err := w.prepare(t, ownerA, "s1", "review/reviewer"); !errors.Is(err, native.ErrProfileUnknown) {
		t.Fatalf("bare s1: %v", err)
	}
	// a's own profile by the bare and the qualified own form is the same session.
	for _, session := range []string{"s1", "a:s1"} {
		if _, err := w.prepare(t, "a:s1\x00dev/base", session, "dev/base"); !errors.Is(err, native.ErrProfileCycle) {
			t.Errorf("own session as %s: %v, want a cycle", session, err)
		}
	}
	// Symmetric: b reaches a's dev/base as a reaches b's.
	if _, err := w.prepare(t, ownerB, "a:s1", "dev/base"); err != nil {
		t.Fatalf("b to a: %v", err)
	}
}

// A message that comes back to a profile the chain already holds is a cycle across workspaces:
// the nodes are qualified, so the running b profile cannot message a's dev/base again.
func TestProfileMessageCycleAcrossWorkspacesIsRefused(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{})
	w.mu.Lock()
	w.holdOn, w.gate, w.reached = "hello", make(chan struct{}), make(chan struct{}, 4)
	w.mu.Unlock()
	plan, err := w.prepare(t, ownerA, "b:s1", "dev/base", func(r *native.ProfileMessageRequest) { r.Message = "hello" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run, err := plan.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.reached:
	case <-ctx.Done():
		t.Fatal("the run of b did not start")
	}
	// b's dev/base, running for a's dev/base, messages a's dev/base: the chain holds it.
	if _, err := w.prepare(t, ownerB, "a:s1", "dev/base"); !errors.Is(err, native.ErrProfileCycle) {
		t.Errorf("back to the caller: %v, want a cycle", err)
	}
	// Another profile of a would be a new node (a's chain limit decides, not a cycle).
	close(w.gate)
	<-run.Done()
}

// A caller with a sandbox cannot take a target of another workspace that has none: the caller's
// sandbox names its own workspace's folders, which are not the target's. One with a sandbox of
// its own is narrowed by the caller's, with each side's own paths, and refused when it reaches
// more.
func TestProfileMessageToAnotherWorkspaceNeedsASandboxOfItsOwnUnderASandboxedCaller(t *testing.T) {
	sandbox := func(write string) *sessionfs.SandboxSettings {
		return &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", Tools: []string{"exec"}, ReadPaths: []string{"/"}, WritablePaths: []string{write, "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}}
	}
	own := `{"models":["openai/default"],"model_default":"openai/default","settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"` + wsTrueExecutable() + `","writable_paths":["{workspace}","{tmp}"],"plan_writable_paths":["{tmp}"]}}}`
	for name, test := range map[string]struct {
		reviewer string
		caller   *sessionfs.SandboxSettings
		wantErr  string
	}{
		"no sandbox on the target":        {"", sandbox("{workspace}"), "has no sandbox of its own"},
		"target writes beyond":            {own, sandbox("{workspace}"), "widens"},
		"target has its own, caller none": {own, nil, ""},
		"no caller sandbox":               {"", nil, ""},
		"target inside the callers":       {own, sandbox("/"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			w := startMessageWorkspaces(t, messageWorkspaceOptions{bReviewer: test.reviewer})
			_, err := w.prepare(t, ownerA, "b:s1", "review/reviewer", func(r *native.ProfileMessageRequest) {
				if test.caller != nil {
					r.Environment = &tools.Environment{Sandbox: test.caller}
					r.CallerPlace = sessionfs.SandboxPlace{Workspace: w.a, CWD: w.a}
				}
			})
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var refused *native.ProfileSandboxError
			if !errors.As(err, &refused) || !strings.Contains(refused.Message, test.wantErr) {
				t.Fatalf("error = %v, want a sandbox refusal with %q", err, test.wantErr)
			}
		})
	}
	// The same sandboxed caller with a target of its own workspace is unchanged: the target with
	// no sandbox takes the caller's (existing behaviour).
	w := startMessageWorkspaces(t, messageWorkspaceOptions{})
	writeWorkspaceTestFile(t, filepath.Join(w.a, ".jad", "profiles", "review", "reviewer.json"), `{"models":["openai/default"],"model_default":"openai/default"}`)
	if _, err := w.prepare(t, ownerA, "s1", "review/reviewer", func(r *native.ProfileMessageRequest) {
		r.Environment = &tools.Environment{Sandbox: sandbox("{workspace}")}
		r.CallerPlace = sessionfs.SandboxPlace{Workspace: w.a, CWD: w.a}
	}); err != nil {
		t.Fatalf("own workspace under a sandbox: %v", err)
	}
}

// wsTrueExecutable is an executable that exists, standing in for bwrap where a test only loads
// the settings.
func wsTrueExecutable() string {
	for _, path := range []string{"/usr/bin/true", "/bin/true"} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "true"
}

// The whole path through the tool: a run of a's dev/base calls profile_message with
// local:b/s1/review/reviewer, b answers in its own session, and the answer comes back as the
// tool's result. Listing both workspaces is all it takes; a target that is not listed reads like
// an unknown profile.
func TestProfileMessageToolCrossesWorkspacesEndToEnd(t *testing.T) {
	for name, test := range map[string]struct {
		unlistedB bool
		want      string
	}{
		"listed":     {false, "answer-from-b"},
		"not listed": {true, "unknown_profile"},
	} {
		t.Run(name, func(t *testing.T) {
			w := startMessageWorkspaces(t, messageWorkspaceOptions{unlistedB: test.unlistedB, aToolset: "coding/talk"})
			call := scriptFunctionCall("profile_message", map[string]any{"profile": "local:b/s1/review/reviewer", "message": "question-for-b"})
			w.script = func(model, body string) string {
				switch {
				case strings.Contains(body, "question-for-b") && !strings.Contains(body, "ask-b"):
					return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer-from-b"}]}`
				case strings.Contains(body, "ask-b"):
					return call(model, body)
				}
				return ""
			}
			request, err := http.NewRequest(http.MethodPost, w.api.URL+"/api/sessions/"+url.PathEscape("a:s1")+"/run?profile="+url.QueryEscape("dev/base"), strings.NewReader(`{"input":"ask-b"}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("run: %d %s", response.StatusCode, body)
			}
			callerTranscript := w.transcript(w.a, "s1", "dev/base")
			targetTranscript := w.transcript(w.b, "s1", "review/reviewer")
			if !strings.Contains(callerTranscript, test.want) {
				t.Fatalf("a's transcript lacks %q:\n%s", test.want, callerTranscript)
			}
			if test.want == "answer-from-b" {
				if !strings.Contains(targetTranscript, "question-for-b") {
					t.Fatalf("b's transcript: %q", targetTranscript)
				}
			} else if targetTranscript != "" {
				t.Fatalf("b was messaged without being listed: %s", targetTranscript)
			}
		})
	}
}

// With real bwrap: a sandboxed caller of a covers b's folder, b's profile has its own sandbox,
// and the narrowed run of the target in b works in b (writes there) but sees nothing of a's
// .jad, cannot write a, and cannot see b's own .jad (a hides it from its own sandbox, and the
// target is never outside the caller's).
func TestProfileMessageTargetOfAnotherWorkspaceRunsInItsOwnNarrowedSandbox(t *testing.T) {
	requireBwrap(t)
	own := `{"models":["openai/default"],"model_default":"openai/default","toolset":"coding/exec","settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap","writable_paths":["{workspace}","{tmp}"],"plan_writable_paths":["{tmp}"]}}}`
	w := startMessageWorkspaces(t, messageWorkspaceOptions{bReviewer: own, exec: true})
	command := "printf 'by b' > made.txt; " +
		"( if cat " + filepath.Join(w.a, ".jad", "workspace.json") + " >/dev/null 2>&1; then echo reada=yes; else echo reada=no; fi; " +
		"if echo x > " + filepath.Join(w.a, "planted.txt") + " 2>/dev/null; then echo writea=yes; else echo writea=no; fi; " +
		"if cat .jad/workspace.json >/dev/null 2>&1; then echo readbjad=yes; else echo readbjad=no; fi ) > result.txt"
	call := scriptFunctionCall("exec", map[string]any{"code": command})
	w.script = func(model, body string) string {
		if strings.Contains(body, "do-it") {
			return call(model, body)
		}
		return ""
	}
	caller := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", Tools: []string{"exec"}, ReadPaths: []string{"/"},
		WritablePaths: []string{w.a, w.b, "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}, HidePaths: []string{filepath.Join(w.b, ".jad")}}
	plan, err := w.prepare(t, ownerA, "b:s1", "review/reviewer", func(r *native.ProfileMessageRequest) {
		r.Message = "do-it"
		r.Environment = &tools.Environment{Sandbox: caller}
		r.CallerPlace = sessionfs.SandboxPlace{Workspace: w.a, CWD: w.a}
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome := w.send(t, plan); outcome.Err != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if data, err := os.ReadFile(filepath.Join(w.b, "made.txt")); err != nil || string(data) != "by b" {
		t.Fatalf("the target did not write its workspace: %v %q\n%s", err, data, w.transcript(w.b, "s1", "review/reviewer"))
	}
	result, _ := os.ReadFile(filepath.Join(w.b, "result.txt"))
	for _, want := range []string{"reada=no", "writea=no", "readbjad=no"} {
		if !strings.Contains(string(result), want) {
			t.Errorf("result lacks %s:\n%s", want, result)
		}
	}
	if _, err := os.Stat(filepath.Join(w.a, "planted.txt")); err == nil {
		t.Fatal("the target planted a file in the caller's workspace")
	}
}

// The hook that confines the run at its start refuses the same target the plan refused: a run of
// another workspace without a sandbox of its own takes nothing from the caller's.
func TestTargetConfinerRefusesAnUnsandboxedTargetOfAnotherWorkspace(t *testing.T) {
	caller := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", ReadPaths: []string{"/"}, WritablePaths: []string{"/work/a"}}
	place := sessionfs.SandboxPlace{Workspace: "/work/a", CWD: "/work/a"}
	target := boundRunner{config: sessionfs.SessionConfig{WorkingDir: "/work/b"}}
	if _, err := targetConfiner(caller, place, nil, true)(target); err == nil || !strings.Contains(err.Error(), "another workspace") {
		t.Fatalf("other workspace: %v", err)
	}
	confined, err := targetConfiner(caller, place, nil, false)(target)
	if err != nil {
		t.Fatalf("same workspace: %v", err)
	}
	if got := confined.(boundRunner).config.Sandbox; got == nil || got.WritablePaths[0] != "/work/a" {
		t.Fatalf("a target without a sandbox of the caller's workspace takes the caller's: %+v", got)
	}
}

// The run applies the check again to the settings it starts with: a target that lost its sandbox
// between the plan and the start is refused, not given the caller's.
func TestProfileMessageToAnotherWorkspaceChecksTheSandboxAgainAtTheStart(t *testing.T) {
	own := `{"models":["openai/default"],"model_default":"openai/default","settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"` + wsTrueExecutable() + `","writable_paths":["{workspace}","{tmp}"],"plan_writable_paths":["{tmp}"]}}}`
	w := startMessageWorkspaces(t, messageWorkspaceOptions{bReviewer: own})
	caller := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: wsTrueExecutable(), ReadPaths: []string{"/"}, WritablePaths: []string{"/", "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}}
	plan, err := w.prepare(t, ownerA, "b:s1", "review/reviewer", func(r *native.ProfileMessageRequest) {
		r.Environment = &tools.Environment{Sandbox: caller}
		r.CallerPlace = sessionfs.SandboxPlace{Workspace: w.a, CWD: w.a}
	})
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(w.b, ".jad", "profiles", "review", "reviewer.json"), `{"models":["openai/default"],"model_default":"openai/default"}`)
	run, err := plan.Start(context.Background())
	if err == nil {
		<-run.Done()
		err = run.Outcome().Err
	}
	if err == nil || !strings.Contains(err.Error(), "another workspace") {
		t.Fatalf("start: %v", err)
	}
}
