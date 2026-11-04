package headless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseCommand(t *testing.T) {
	cases := map[string]*Command{
		"/compact":                    {Kind: CommandCompact, Word: "compact"},
		"  /reset  ":                  {Kind: CommandReset, Word: "reset"},
		"/state keep the plan\n":      {Kind: CommandState, Word: "state", Text: "keep the plan"},
		"/prompt:bug-finding fix it":  {Kind: CommandPrompt, Word: "prompt:bug-finding", Item: "bug-finding", Text: "fix it"},
		"/template:t\n\nmore\nlines":  {Kind: CommandTemplate, Word: "template:t", Item: "t", Text: "more\nlines"},
		"/skill:review":               {Kind: CommandSkill, Word: "skill:review", Item: "review"},
		"/mcp:git_hub:review pr=4 go": {Kind: CommandMCP, Word: "mcp:git_hub:review", Server: "git_hub", Item: "review", Text: "pr=4 go"},
		"/mcp:s:a:b":                  {Kind: CommandMCP, Word: "mcp:s:a:b", Server: "s", Item: "a:b"},
		"/plan fix the bug":           {Kind: CommandPlan, Word: "plan", Text: "fix the bug"},
		"/planning":                   {Kind: CommandCustom, Word: "planning", Item: "planning"},
		"/etc/hosts is wrong":         nil,
		"/compacted":                  {Kind: CommandCustom, Word: "compacted", Item: "compacted"},
		"/prompt:":                    nil,
		"/unknown:x":                  {Kind: CommandCustom, Word: "unknown:x", Item: "unknown:x"},
		"/git:commit-all fix it":      {Kind: CommandCustom, Word: "git:commit-all", Item: "git:commit-all", Text: "fix it"},
		"/Review":                     nil,
		"/a--b":                       nil,
		"/a::b":                       nil,
		"/resource:x":                 nil,
		"/mcp:server":                 nil,
		"/mcp:bad.server:x":           nil,
		"/mcp:s:":                     nil,
		"say /compact":                nil,
		"":                            nil,
	}
	for input, want := range cases {
		got, ok := ParseCommand(input)
		if want == nil {
			if ok {
				t.Errorf("ParseCommand(%q) = %+v, want no command", input, got)
			}
			continue
		}
		if !ok || got != *want {
			t.Errorf("ParseCommand(%q) = %+v %v, want %+v", input, got, ok, *want)
		}
	}
}

func TestCommandCheck(t *testing.T) {
	compact, _ := ParseCommand("/compact now")
	if err := compact.Check("", false, false); err == nil {
		t.Fatal("/compact with text was accepted")
	}
	state, _ := ParseCommand("/state text")
	if err := state.Check("", false, false); err != nil {
		t.Fatalf("/state with text: %v", err)
	}
	if err := state.Check("openai/m", false, false); err == nil {
		t.Fatal("/state with a model was accepted")
	}
	if err := state.Check("", true, false); err == nil {
		t.Fatal("/state with an output schema was accepted")
	}
	prompt, _ := ParseCommand("/prompt:x text")
	if err := prompt.Check("openai/m", true, false); err != nil {
		t.Fatalf("a library command with a model and schema: %v", err)
	}
}

// The same cases as frontend_new/src/slash-commands.test.mjs.
func TestLibrarySlugMatchesTheWebClient(t *testing.T) {
	for name, want := range map[string]string{
		"Software engineering": "software-engineering",
		"  A -- B / C!  ":      "a-b-c",
		"日本語 テスト":              "日本語-テスト",
		"***":                  "untitled",
		"Ünïcode 2 Ways":       "ünïcode-2-ways",
	} {
		if got := LibrarySlug(name); got != want {
			t.Errorf("LibrarySlug(%q) = %q, want %q", name, got, want)
		}
	}
	keys := libraryKeys([][]librarySummary{
		{{ID: "p1", Kind: "prompt", Name: "Review"}, {ID: "p2", Kind: "prompt", Name: "review!"}, {ID: "p3", Kind: "prompt", Name: "Review 2"}},
		{{ID: "t1", Kind: "template", Name: "Review"}},
	})
	var got []string
	for _, key := range keys {
		got = append(got, key.key)
	}
	if strings.Join(got, " ") != "prompt:review prompt:review-2 prompt:review-2-2 template:review" {
		t.Fatalf("keys = %v", got)
	}
}

// routeStub answers the routes the commands call. Each answer is a status and a body; a run
// request is recorded and answered with one completed turn.
type routeStub struct {
	mu      sync.Mutex
	answers map[string]routeAnswer
	bodies  map[string]string
	block   chan struct{}
}

type routeAnswer struct {
	status int
	body   string
}

func newRouteStub(answers map[string]routeAnswer) *routeStub {
	return &routeStub{answers: answers, bodies: map[string]string{}}
}

func (s *routeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.RequestURI()
	s.mu.Lock()
	s.bodies[key] = string(body)
	answer, ok := s.answers[key]
	s.mu.Unlock()
	if s.block != nil && strings.HasSuffix(r.URL.Path, "/compact") {
		select {
		case <-s.block:
		case <-r.Context().Done():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"compaction stopped"}`)
			return
		}
	}
	if strings.HasSuffix(r.URL.Path, "/run") && r.Method == http.MethodPost {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, frame("completed", `{"message":{"text":"answer"}}`)+frame("done", `{}`))
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
		return
	}
	w.WriteHeader(answer.status)
	_, _ = io.WriteString(w, answer.body)
}

func (s *routeStub) runInput(t *testing.T, path string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.bodies["POST "+path]
	if !ok {
		t.Fatalf("no run was sent to %s; requests: %v", path, s.bodies)
	}
	var body struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return body.Input
}

func TestActionsCallTheirRoutesAndRunNoTurn(t *testing.T) {
	stub := newRouteStub(map[string]routeAnswer{
		"POST /api/sessions/demo/compact?profile=dev%2Fcoder": {200, `{"compaction":{"compacted":true,"compacted_items":12,"retained_items":4},"usage":{"input_tokens":9}}`},
		"POST /api/sessions/demo/reset":                       {200, `{"reset":true}`},
		"POST /api/sessions/demo/statecontext":                {200, `{"statecontext":{"written":true,"preview":false,"role":"user","text":"x","bytes":120}}`},
	})
	runner := NewRunner(stub, "t")

	result, err := runner.Run(context.Background(), Request{Session: "demo", Profile: "dev/coder", Input: "/compact", Commands: true})
	if err != nil || result.Status != StatusCompleted || result.Command != "compact" || result.Text != "Compacted 12 items; 4 kept." || string(result.Usage) != `{"input_tokens":9}` || !strings.Contains(string(result.CommandResult), `"compacted_items":12`) {
		t.Fatalf("compact: %+v %v", result, err)
	}
	result, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/reset", Commands: true})
	if err != nil || result.Text != "The session was reset." {
		t.Fatalf("reset: %+v %v", result, err)
	}
	result, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/state remember the plan", Commands: true})
	if err != nil || result.Text != "Wrote a user state-context block (120 bytes)." {
		t.Fatalf("state: %+v %v", result, err)
	}
	if body := stub.bodies["POST /api/sessions/demo/statecontext"]; body != `{"preview":false,"prompt":"remember the plan"}` {
		t.Fatalf("state body = %s", body)
	}
	for key := range stub.bodies {
		if strings.HasSuffix(key, "/run") {
			t.Fatalf("an action started a turn: %s", key)
		}
	}

	// Without Commands the word is a prompt.
	result, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/compact"})
	if err != nil || result.Text != "answer" || result.Command != "" || stub.runInput(t, "/api/sessions/demo/run") != "/compact" {
		t.Fatalf("no commands: %+v %v", result, err)
	}

	// A refusal is the server's message.
	stub.answers["POST /api/sessions/demo/reset"] = routeAnswer{409, `{"error":"a run is active"}`}
	_, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/reset", Commands: true})
	var refused *RequestError
	if !errors.As(err, &refused) || refused.Status != 409 || refused.Message != "a run is active" {
		t.Fatalf("refused reset: %v", err)
	}
}

func TestActionStopsAtTheTimeout(t *testing.T) {
	stub := newRouteStub(nil)
	stub.block = make(chan struct{})
	defer close(stub.block)
	result, err := NewRunner(stub, "t").Run(context.Background(), Request{Session: "demo", Input: "/compact", Commands: true, Timeout: 50 * time.Millisecond})
	if err != nil || result.Status != StatusTimedOut || result.Command != "compact" {
		t.Fatalf("result %+v err %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	result, err = NewRunner(stub, "t").Run(ctx, Request{Session: "demo", Input: "/compact", Commands: true})
	if err != nil || result.Status != StatusAborted {
		t.Fatalf("interrupted: %+v %v", result, err)
	}
}

const libraryList = `{"prompts":[{"id":"a1","kind":"prompt","name":"Bug finding"},{"id":"a2","kind":"prompt","name":"Bug-finding"}],"templates":[],"skills":[{"id":"s1","kind":"skill","name":"review"}]}`

func TestLibraryCommandsSendTheItemText(t *testing.T) {
	stub := newRouteStub(map[string]routeAnswer{
		"GET /api/library":           {200, libraryList},
		"GET /api/library/prompt/a2": {200, `{"asset":{"id":"a2","kind":"prompt","content":"Find the bugs.\n"}}`},
		"GET /api/library/skill/s1":  {200, `{"asset":{"id":"s1","kind":"skill","content":"Use the review skill."}}`},
	})
	runner := NewRunner(stub, "t")
	result, err := runner.Run(context.Background(), Request{Session: "demo", Input: "/prompt:bug-finding-2 in main.go\nplease", Commands: true})
	if err != nil || result.Status != StatusCompleted || result.Command != "prompt:bug-finding-2" {
		t.Fatalf("prompt: %+v %v", result, err)
	}
	if input := stub.runInput(t, "/api/sessions/demo/run"); input != "Find the bugs.\n\nin main.go\nplease" {
		t.Fatalf("run input = %q", input)
	}
	if _, err := runner.Run(context.Background(), Request{Session: "demo", Input: "/skill:review", Commands: true}); err != nil {
		t.Fatal(err)
	}
	if input := stub.runInput(t, "/api/sessions/demo/run"); input != "Use the review skill." {
		t.Fatalf("skill input = %q", input)
	}
	_, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/prompt:missing", Commands: true})
	if err == nil || !strings.Contains(err.Error(), "/prompt:bug-finding, /prompt:bug-finding-2") {
		t.Fatalf("unknown item: %v", err)
	}
	_, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/template:x", Commands: true})
	if err == nil || !strings.Contains(err.Error(), "it has no template items") {
		t.Fatalf("empty kind: %v", err)
	}
}

const mcpPromptList = `{"prompts":[
 {"server":"gh","name":"review_pr","arguments":[{"name":"pr","required":true},{"name":"focus"}]},
 {"server":"gh","name":"Summarize Issue","arguments":[{"name":"issue","required":true}]},
 {"server":"gh","name":"hello","arguments":[]}
],"failures":[{"server":"down","error":"failed to connect MCP server down (Error)"}]}`

func TestMCPCommandsFillThePrompt(t *testing.T) {
	stub := newRouteStub(map[string]routeAnswer{
		"GET /api/sessions/demo/mcp-prompts":      {200, mcpPromptList},
		"POST /api/sessions/demo/mcp-prompts/get": {200, `{"prompt":{"server":"gh","name":"x","text":"PROMPT TEXT"}}`},
	})
	runner := NewRunner(stub, "t")
	getBody := func() map[string]any {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal([]byte(stub.bodies["POST /api/sessions/demo/mcp-prompts/get"]), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	_, err := runner.Run(context.Background(), Request{Session: "demo", Input: `/mcp:gh:review_pr pr=42 focus="error \"paths\"" and tests`, Commands: true})
	if err != nil {
		t.Fatal(err)
	}
	body := getBody()
	arguments, _ := body["arguments"].(map[string]any)
	if body["name"] != "review_pr" || arguments["pr"] != "42" || arguments["focus"] != `error "paths"` || len(arguments) != 2 {
		t.Fatalf("get body = %v", body)
	}
	if input := stub.runInput(t, "/api/sessions/demo/run"); input != "PROMPT TEXT\n\nand tests" {
		t.Fatalf("run input = %q", input)
	}

	// One argument takes the whole text; the prompt is found by the slug of its name.
	if _, err := runner.Run(context.Background(), Request{Session: "demo", Input: "/mcp:gh:summarize-issue the login bug", Commands: true}); err != nil {
		t.Fatal(err)
	}
	body = getBody()
	arguments, _ = body["arguments"].(map[string]any)
	if body["name"] != "Summarize Issue" || arguments["issue"] != "the login bug" {
		t.Fatalf("one-argument body = %v", body)
	}
	if input := stub.runInput(t, "/api/sessions/demo/run"); input != "PROMPT TEXT" {
		t.Fatalf("one-argument input = %q", input)
	}

	for input, want := range map[string]string{
		"/mcp:gh:review_pr focus=x":      "needs pr",
		"/mcp:gh:review_pr pr=1 pr=2":    "given twice",
		`/mcp:gh:review_pr pr="unclosed`: "needs pr",
		"/mcp:gh:nope":                   "its prompts are Summarize Issue, hello, review_pr",
		"/mcp:down:x":                    "failed to connect MCP server down",
		"/mcp:other:x":                   "prompts: true",
	} {
		_, err := runner.Run(context.Background(), Request{Session: "demo", Input: input, Commands: true})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", input, err, want)
		}
	}
}

func TestPlanCommandCheck(t *testing.T) {
	for input, want := range map[string]string{
		"/plan":                         "/plan needs a prompt after it",
		"/plan /compact":                "/plan cannot be followed by /compact",
		"/plan /plan again":             "/plan cannot be followed by /plan",
		"/plan fix it":                  "",
		"/plan /prompt:review fix it":   "",
		"/plan /etc/hosts is the issue": "",
	} {
		command, _ := ParseCommand(input)
		err := command.Check("", false, false)
		if (want == "") != (err == nil) || (err != nil && err.Error() != want) {
			t.Errorf("%q: %v, want %q", input, err, want)
		}
	}
	state, _ := ParseCommand("/state text")
	if err := state.Check("", false, true); err == nil || !strings.Contains(err.Error(), "cannot run in plan mode") {
		t.Fatalf("/state in plan mode: %v", err)
	}
	inner, _ := ParseCommand("/plan /mcp:s:p x")
	if err := inner.Check("openai/m", true, false); err != nil {
		t.Fatalf("/plan with an MCP command, a model and a schema: %v", err)
	}
}

func TestPlanSendsTheRunInPlanMode(t *testing.T) {
	stub := newRouteStub(map[string]routeAnswer{
		"GET /api/library":           {200, libraryList},
		"GET /api/library/prompt/a2": {200, `{"asset":{"id":"a2","kind":"prompt","content":"Find the bugs."}}`},
	})
	runner := NewRunner(stub, "t")
	plan := func() (string, bool) {
		t.Helper()
		var body struct {
			Input string `json:"input"`
			Plan  *bool  `json:"plan"`
		}
		if err := json.Unmarshal([]byte(stub.bodies["POST /api/sessions/demo/run"]), &body); err != nil {
			t.Fatal(err)
		}
		return body.Input, body.Plan != nil && *body.Plan
	}
	result, err := runner.Run(context.Background(), Request{Session: "demo", Input: "/plan add a cache", Commands: true})
	if err != nil || !result.Plan || result.Command != "plan" {
		t.Fatalf("/plan: %+v %v", result, err)
	}
	if input, planned := plan(); input != "add a cache" || !planned {
		t.Fatalf("/plan body: %q %v", input, planned)
	}
	result, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/plan /prompt:bug-finding-2 in main.go", Commands: true})
	if err != nil || !result.Plan || result.Command != "plan" {
		t.Fatalf("/plan with a library item: %+v %v", result, err)
	}
	if input, planned := plan(); input != "Find the bugs.\n\nin main.go" || !planned {
		t.Fatalf("/plan with a library item body: %q %v", input, planned)
	}
	result, err = runner.Run(context.Background(), Request{Session: "demo", Input: "add a cache", Plan: true})
	if err != nil || !result.Plan || result.Command != "" {
		t.Fatalf("Plan: %+v %v", result, err)
	}
	if _, planned := plan(); !planned {
		t.Fatal("Plan was not sent")
	}
	if _, err := runner.Run(context.Background(), Request{Session: "demo", Input: "add a cache"}); err != nil {
		t.Fatal(err)
	}
	if raw := stub.bodies["POST /api/sessions/demo/run"]; strings.Contains(raw, "plan") {
		t.Fatalf("a run without plan mode sent %s", raw)
	}
	if _, err := runner.Run(context.Background(), Request{Session: "demo", Input: "/reset", Commands: true, Plan: true}); err == nil {
		t.Fatal("/reset in plan mode was accepted")
	}
}

func TestCustomCommandsSendTheirFilledText(t *testing.T) {
	stub := newRouteStub(map[string]routeAnswer{
		"GET /api/library":                    {200, `{"prompts":[],"templates":[],"skills":[],"commands":[{"id":"c1","kind":"command","command":"git:commit","name":"commit"}]}`},
		"POST /api/library/command/c1/expand": {200, `{"command":"git:commit","text":"Commit with the message: fix the build"}`},
	})
	runner := NewRunner(stub, "t")
	result, err := runner.Run(context.Background(), Request{Session: "demo", Input: "/git:commit fix the build", Commands: true})
	if err != nil || result.Command != "git:commit" {
		t.Fatalf("custom command: %+v %v", result, err)
	}
	if body := stub.bodies["POST /api/library/command/c1/expand"]; body != `{"arguments":"fix the build"}` {
		t.Fatalf("expand body = %s", body)
	}
	if input := stub.runInput(t, "/api/sessions/demo/run"); input != "Commit with the message: fix the build" {
		t.Fatalf("run input = %q", input)
	}

	// A word the library has no command for is a prompt, sent as written.
	result, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/deploy now", Commands: true})
	if err != nil || result.Command != "" || stub.runInput(t, "/api/sessions/demo/run") != "/deploy now" {
		t.Fatalf("unknown word: %+v %v", result, err)
	}
	// In plan mode too.
	result, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/plan /git:commit x", Commands: true})
	if err != nil || result.Command != "plan" || !result.Plan || stub.runInput(t, "/api/sessions/demo/run") != "Commit with the message: fix the build" {
		t.Fatalf("plan with a custom command: %+v %v", result, err)
	}
	result, err = runner.Run(context.Background(), Request{Session: "demo", Input: "/plan /deploy now", Commands: true})
	if err != nil || result.Command != "plan" || stub.runInput(t, "/api/sessions/demo/run") != "/deploy now" {
		t.Fatalf("plan with an unknown word: %+v %v", result, err)
	}
	// A refused expansion is the server's message.
	stub.answers["POST /api/library/command/c1/expand"] = routeAnswer{422, `{"error":"the command's text is empty"}`}
	if _, err := runner.Run(context.Background(), Request{Session: "demo", Input: "/git:commit", Commands: true}); err == nil || !strings.Contains(err.Error(), "the command's text is empty") {
		t.Fatalf("refused expansion: %v", err)
	}
}

const mcpResourceList = `{"resources":[
 {"server":"docs","uri":"docs://readme","name":"README","mime_type":"text/markdown"},
 {"server":"docs","uri_template":"docs://pages/{page}{?lang}","variables":["page","lang"],"name":"page"},
 {"server":"docs","uri":"docs://a","name":"Same Name"},
 {"server":"docs","uri":"docs://b","name":"same-name"}
],"failures":[{"server":"down","error":"failed to connect MCP server down (Error)"}]}`

func TestResourceCommandsReadTheResource(t *testing.T) {
	stub := newRouteStub(map[string]routeAnswer{
		"GET /api/sessions/demo/mcp-resources":       {200, mcpResourceList},
		"POST /api/sessions/demo/mcp-resources/read": {200, `{"resource":{"server":"docs","uri":"x","text":"Resource text."}}`},
	})
	runner := NewRunner(stub, "t")
	read := func(input string) (string, string, error) {
		t.Helper()
		_, err := runner.Run(context.Background(), Request{Session: "demo", Input: input, Commands: true})
		if err != nil {
			return "", "", err
		}
		return stub.bodies["POST /api/sessions/demo/mcp-resources/read"], stub.runInput(t, "/api/sessions/demo/run"), nil
	}
	for input, want := range map[string][2]string{
		"/resource:docs:README summarize":                 {`{"server":"docs","uri":"docs://readme"}`, "Resource text.\n\nsummarize"},
		"/resource:docs:readme":                           {`{"server":"docs","uri":"docs://readme"}`, "Resource text."},
		"/resource:docs:docs://readme":                    {`{"server":"docs","uri":"docs://readme"}`, "Resource text."},
		"/resource:docs:docs://unlisted x":                {`{"server":"docs","uri":"docs://unlisted"}`, "Resource text.\n\nx"},
		"/resource:docs:same-name":                        {`{"server":"docs","uri":"docs://b"}`, "Resource text."},
		`/resource:docs:page page=intro lang="en us" now`: {`{"server":"docs","uri_template":"docs://pages/{page}{?lang}","variables":{"lang":"en us","page":"intro"}}`, "Resource text.\n\nnow"},
	} {
		body, input2, err := read(input)
		if err != nil || body != want[0] || input2 != want[1] {
			t.Errorf("%q: body %s input %q err %v", input, body, input2, err)
		}
	}
	for input, want := range map[string]string{
		"/resource:docs:missing":            `MCP server docs has no resource "missing"; its resources are README, Same Name, page, same-name`,
		"/resource:docs:page":               "needs its variables as name=value",
		"/resource:docs:page page=a page=b": "variable page of MCP resource page is given twice",
		"/resource:down:x":                  "list the resources of MCP server down",
		"/resource:other:x":                 "MCP server other lists no resources",
	} {
		if _, _, err := read(input); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", input, err, want)
		}
	}
	stub.answers["POST /api/sessions/demo/mcp-resources/read"] = routeAnswer{502, `{"error":"MCP server docs: resource docs://readme has no text to insert"}`}
	if _, _, err := read("/resource:docs:README"); err == nil || !strings.Contains(err.Error(), "has no text to insert") {
		t.Fatalf("refused read: %v", err)
	}
}

// A custom command's front matter fills the run's model, profile, plan mode and tools,
// unless the request sets them itself.
func TestCustomCommandRunOptions(t *testing.T) {
	stub := newRouteStub(map[string]routeAnswer{
		"GET /api/library":                    {200, `{"prompts":[],"templates":[],"skills":[],"commands":[{"id":"c1","kind":"command","command":"review","name":"review"}]}`},
		"POST /api/library/command/c1/expand": {200, `{"command":"review","text":"Review main.go.","model":"openai/fast","profile":"dev/reviewer","plan":true,"allowed_tools":["read","grep"]}`},
	})
	runner := NewRunner(stub, "t")
	sent := func(path string) map[string]any {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal([]byte(stub.bodies["POST "+path]), &body); err != nil {
			t.Fatalf("no run at %s: %v", path, stub.bodies)
		}
		return body
	}
	result, err := runner.Run(context.Background(), Request{Session: "demo", Input: "/review main.go", Commands: true})
	if err != nil || !result.Plan || result.Profile != "dev/reviewer" || result.Model != "openai/fast" {
		t.Fatalf("command with run options: %+v %v", result, err)
	}
	body := sent("/api/sessions/demo/run?profile=dev%2Freviewer")
	if body["model"] != "openai/fast" || body["plan"] != true || fmt.Sprint(body["tools"]) != "[read grep]" {
		t.Fatalf("run body = %v", body)
	}

	// What the request sets wins.
	result, err = runner.Run(context.Background(), Request{Session: "demo", Profile: "dev/coder", Model: "openai/slow", Tools: []string{"glob"}, Input: "/review main.go", Commands: true})
	if err != nil || result.Profile != "dev/coder" || result.Model != "openai/slow" {
		t.Fatalf("request options: %+v %v", result, err)
	}
	body = sent("/api/sessions/demo/run?profile=dev%2Fcoder")
	if body["model"] != "openai/slow" || body["plan"] != true || fmt.Sprint(body["tools"]) != "[glob]" {
		t.Fatalf("run body with the request's options = %v", body)
	}
}

// ExecutePlan sends execute_plan with the input as written, which may be empty, and does not
// read it as a command; it cannot be combined with plan mode.
func TestExecutePlan(t *testing.T) {
	stub := newRouteStub(map[string]routeAnswer{})
	runner := NewRunner(stub, "t")
	result, err := runner.Run(context.Background(), Request{Session: "demo", ExecutePlan: true, Commands: true})
	if err != nil || !result.ExecutePlan || result.Status != StatusCompleted {
		t.Fatalf("execute plan: %+v %v", result, err)
	}
	if raw := stub.bodies["POST /api/sessions/demo/run"]; raw != `{"input":"","execute_plan":true}` {
		t.Fatalf("run body = %s", raw)
	}
	if _, err := runner.Run(context.Background(), Request{Session: "demo", ExecutePlan: true, Commands: true, Input: "/compact skip the tests"}); err != nil {
		t.Fatal(err)
	}
	if input := stub.runInput(t, "/api/sessions/demo/run"); input != "/compact skip the tests" {
		t.Fatalf("execute plan input = %q", input)
	}
	if _, err := runner.Run(context.Background(), Request{Session: "demo", ExecutePlan: true, Plan: true}); err == nil {
		t.Fatal("execute plan in plan mode was accepted")
	}
	if _, err := runner.Run(context.Background(), Request{Session: "demo"}); err == nil {
		t.Fatal("an empty prompt was accepted")
	}
}
