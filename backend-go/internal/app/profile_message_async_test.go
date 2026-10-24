package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

var pmHandle = regexp.MustCompile(`pm_[0-9a-f]{8}`)

type pmFixture struct {
	f        *layoutFixture
	message  tools.Tool
	async    tools.Tool
	owner    string
	order    *orderLog
	settings sessionfs.NativeProfileMessageSettings
}

// orderLog records, per target run, which marker of the prompt was new: the order the
// target served its messages in.
type orderLog struct {
	mu   sync.Mutex
	seen map[string]bool
	list []string
}

func (o *orderLog) note(body string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	for _, marker := range []string{"alpha-1", "alpha-2", "alpha-3", "alpha-4", "alpha-5"} {
		if strings.Contains(body, marker) && !o.seen[marker] {
			o.seen[marker] = true
			o.list = append(o.list, marker)
			return marker
		}
	}
	return ""
}

func (o *orderLog) served() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.list...)
}

func newPMFixture(t *testing.T, mutate func(*sessionfs.NativeProfileMessageSettings)) *pmFixture {
	t.Helper()
	f := newLayoutFixture(t)
	f.overwrite(t, "profiles/review/helper.json", `{"models":["openai/large"],"toolset":"coding/none"}`)
	order := &orderLog{}
	f.script = func(_, body string) string {
		marker := order.note(body)
		if marker == "" {
			return ""
		}
		return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer for ` + marker + `"}]}`
	}
	host, err := f.service.resolver.AsyncHost(native.AsyncHostConfig{Dir: native.AsyncDir(filepath.Join(f.cwd, "pm")), Config: asyncmgr.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	settings := sessionfs.NativeProfileMessageSettings{TimeoutMs: 60000, ResultMaxBytes: 4096, MaxChain: 2, MaxQueued: 4}
	if mutate != nil {
		mutate(&settings)
	}
	message, err := native.NewProfileMessageTool(settings, []string{"review/reviewer", "dev/coder"}, profileMessenger{resolver: f.service.resolver}, host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := native.NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := f.service.resolver.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	return &pmFixture{f: f, message: message, async: async, owner: sessionKey(reference, "dev/coder"), order: order, settings: settings}
}

func (p *pmFixture) callerCtx(t *testing.T, profile string) context.Context {
	t.Helper()
	return context.WithValue(context.Background(), callerMessagesPathKey{}, boundRunnerOf(t, p.f, profile).config.MessagesPath)
}

func (p *pmFixture) sendAs(t *testing.T, ctx context.Context, execution tools.ExecutionContext, text string) string {
	t.Helper()
	execution.CallID = "call"
	out, err := p.message.Execute(ctx, map[string]any{"profile": "review/reviewer", "message": text, "block_ms": json.Number("0")}, execution)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (p *pmFixture) send(t *testing.T, text string) string {
	t.Helper()
	return p.sendAs(t, p.callerCtx(t, "dev/coder"), tools.ExecutionContext{SessionKey: p.owner}, text)
}

func (p *pmFixture) asyncAs(t *testing.T, owner string, arguments map[string]any) string {
	t.Helper()
	out, err := p.async.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "async", SessionKey: owner})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (p *pmFixture) wait(t *testing.T, id string) string {
	t.Helper()
	return p.asyncAs(t, p.owner, map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("30000")})
}

func (p *pmFixture) list(t *testing.T) string {
	t.Helper()
	return p.asyncAs(t, p.owner, map[string]any{"action": "list", "state": "all"})
}

func (p *pmFixture) handle(t *testing.T, out string) string {
	t.Helper()
	id := pmHandle.FindString(out)
	if id == "" {
		t.Fatalf("no handle in %s", out)
	}
	return id
}

func (p *pmFixture) holdTranscript(t *testing.T) func() {
	t.Helper()
	release, err := p.f.service.resolver.LockSession(context.Background(), "local:demo", "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func pollFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProfileMessageQueuesFirstComeFirstServedAndEachHandleKeepsItsOwnAnswerAfterTheTargetMovedOn(t *testing.T) {
	p := newPMFixture(t, nil)
	helperOwner := sessionKey("local:demo", "review/helper")
	release := p.holdTranscript(t)
	first := p.handle(t, p.send(t, "alpha-1 please"))
	second := p.handle(t, p.send(t, "alpha-2 please"))
	helperOut := p.sendAs(t, p.callerCtx(t, "review/helper"), tools.ExecutionContext{SessionKey: helperOwner}, "alpha-3 please")
	third := p.handle(t, helperOut)
	if !strings.Contains(helperOut, "state: queued") {
		t.Fatalf("a message to a busy target must queue:\n%s", helperOut)
	}
	if list := p.list(t); strings.Count(list, `"state":"queued"`) != 2 || strings.Contains(list, third) {
		t.Fatalf("the owner's list must show its two queued messages and not the other caller's:\n%s", list)
	}
	release()

	// The third caller's wait returns only after the target served all three in arrival order.
	if got := p.asyncAs(t, helperOwner, map[string]any{"action": "wait", "ids": []any{third}, "wait_ms": json.Number("30000")}); !strings.Contains(got, "answer for alpha-3") {
		t.Fatalf("the helper's answer:\n%s", got)
	}
	if served := p.order.served(); strings.Join(served, ",") != "alpha-1,alpha-2,alpha-3" {
		t.Fatalf("the target served %v, want first come, first served", served)
	}
	// The target has long moved on; each earlier caller still gets exactly its own answer, once.
	for id, want := range map[string]string{first: "answer for alpha-1", second: "answer for alpha-2"} {
		got := p.wait(t, id)
		if !strings.Contains(got, want) || !strings.Contains(got, id) {
			t.Errorf("handle %s: want %q:\n%s", id, want, got)
		}
	}
	if again := p.asyncAs(t, p.owner, map[string]any{"action": "wait", "wait_ms": json.Number("0")}); strings.Contains(again, "answer for") {
		t.Fatalf("an answer was delivered twice:\n%s", again)
	}
	status := p.asyncAs(t, p.owner, map[string]any{"action": "status", "id": first})
	if !strings.Contains(status, "answer for alpha-1") || strings.Contains(status, "#") {
		t.Fatalf("status must show the stored answer and no transcript reference:\n%s", status)
	}
}

func TestProfileMessageQueueCapFailsFastAndLeavesNoHandle(t *testing.T) {
	p := newPMFixture(t, func(s *sessionfs.NativeProfileMessageSettings) { s.MaxQueued = 2 })
	release := p.holdTranscript(t)
	first := p.handle(t, p.send(t, "alpha-1"))
	second := p.handle(t, p.send(t, "alpha-2"))
	out := p.send(t, "alpha-3")
	if !strings.Contains(out, "profile_busy") || !strings.Contains(out, "2 messages are already waiting") || pmHandle.MatchString(out) {
		t.Fatalf("a message over the cap must fail at once:\n%s", out)
	}
	if list := p.list(t); !strings.Contains(list, "counts: {\"active\":2,\"unread\":0,") {
		t.Fatalf("the refused message left a live or unread handle:\n%s", list)
	}
	release()
	p.wait(t, second)
	p.wait(t, first)
	if served := p.order.served(); strings.Join(served, ",") != "alpha-1,alpha-2" {
		t.Fatalf("served %v: the refused message must never run", served)
	}
	// With the cap at 0 a busy target fails at once.
	none := newPMFixture(t, func(s *sessionfs.NativeProfileMessageSettings) { s.MaxQueued = 0 })
	hold := none.holdTranscript(t)
	defer hold()
	if out := none.send(t, "alpha-1"); !strings.Contains(out, "profile_busy") || pmHandle.MatchString(out) {
		t.Fatalf("max_queued_per_profile 0 must not queue:\n%s", out)
	}
}

func TestProfileMessageBusyWaitMsIsTheLongestAQueuedMessageWaits(t *testing.T) {
	p := newPMFixture(t, func(s *sessionfs.NativeProfileMessageSettings) { s.BusyWaitMs = 150 })
	release := p.holdTranscript(t)
	defer release()
	id := p.handle(t, p.send(t, "alpha-1"))
	got := p.wait(t, id)
	for _, want := range []string{id, "failed", "profile_busy", "still busy after waiting 150 ms"} {
		if !strings.Contains(got, want) {
			t.Errorf("wait lacks %q:\n%s", want, got)
		}
	}
	if served := p.order.served(); len(served) != 0 {
		t.Fatalf("the expired message ran: %v", served)
	}
}

func TestProfileMessageStopOfQueuedMessagesRemovesThemFromTheLine(t *testing.T) {
	p := newPMFixture(t, nil)
	release := p.holdTranscript(t)
	first := p.handle(t, p.send(t, "alpha-1"))
	second := p.handle(t, p.send(t, "alpha-2"))
	third := p.handle(t, p.send(t, "alpha-3"))
	for _, id := range []string{first, second} {
		if out := p.asyncAs(t, p.owner, map[string]any{"action": "stop", "id": id}); !strings.Contains(out, "stopped") {
			t.Fatalf("stop of %s:\n%s", id, out)
		}
	}
	release()
	if got := p.wait(t, third); !strings.Contains(got, "answer for alpha-3") {
		t.Fatalf("the message behind the stopped ones:\n%s", got)
	}
	if served := p.order.served(); strings.Join(served, ",") != "alpha-3" {
		t.Fatalf("served %v, want only the message that was not stopped", served)
	}
	// A freed place can be used again.
	fourth := p.handle(t, p.send(t, "alpha-4"))
	if got := p.wait(t, fourth); !strings.Contains(got, "answer for alpha-4") {
		t.Fatalf("a new message after the stops:\n%s", got)
	}
}

func TestProfileMessageStopOfARunningMessageStopsTheTargetAndKeepsNoLiveHandle(t *testing.T) {
	p := newPMFixture(t, nil)
	p.f.cancelled = make(chan struct{}, 4)
	p.f.mu.Lock()
	p.f.hold, p.f.reached = true, make(chan struct{}, 4)
	p.f.mu.Unlock()
	id := p.handle(t, p.send(t, "alpha-1"))
	awaitSignal(t, p.f.reached, "the target's provider request")
	if list := p.list(t); !strings.Contains(list, `"state":"running"`) {
		t.Fatalf("a message the target is working on must be running:\n%s", list)
	}
	if out := p.asyncAs(t, p.owner, map[string]any{"action": "stop", "id": id}); !strings.Contains(out, "stopped") {
		t.Fatalf("stop:\n%s", out)
	}
	awaitSignal(t, p.f.cancelled, "the target's request to be cancelled")
	awaitTargetSessionFree(t, p.f)
	if run, ok := targetRunInList(t, p.f); ok && run.Status == "running" {
		t.Fatalf("the target is still running: %+v", run)
	}
}

func TestProfileMessageResetAndDeleteReachQueuedAndRunningMessages(t *testing.T) {
	p := newPMFixture(t, nil)
	p.f.cancelled = make(chan struct{}, 8)
	p.f.mu.Lock()
	p.f.hold, p.f.reached = true, make(chan struct{}, 8)
	p.f.mu.Unlock()
	running := p.handle(t, p.send(t, "alpha-1"))
	awaitSignal(t, p.f.reached, "the target's provider request")
	queued := p.handle(t, p.send(t, "alpha-2"))
	if !strings.Contains(p.list(t), queued) {
		t.Fatal("the queued message is not listed")
	}
	if status, body := p.f.do(t, http.MethodPost, "/api/sessions/local:demo/reset", ""); status != http.StatusOK {
		t.Fatalf("reset: %d %s", status, body)
	}
	awaitSignal(t, p.f.cancelled, "the running target request to be cancelled")
	pollFor(t, "both messages to end after the reset", func() bool {
		list := p.list(t)
		return !strings.Contains(list, `"state":"queued"`) && !strings.Contains(list, `"state":"running"`)
	})
	if got := p.asyncAs(t, p.owner, map[string]any{"action": "wait", "wait_ms": json.Number("0")}); strings.Contains(got, running) || strings.Contains(got, queued) {
		t.Fatalf("a reset must drop the signals of the history it ended:\n%s", got)
	}
	awaitTargetSessionFree(t, p.f)

	// A delete takes the records away too.
	p.f.mu.Lock()
	p.f.hold = true
	p.f.mu.Unlock()
	live := p.handle(t, p.send(t, "alpha-3"))
	awaitSignal(t, p.f.reached, "the second held request")
	deleteDemoSession(t, p.f)
	awaitSignal(t, p.f.cancelled, "the held request to be cancelled by the delete")
	pollFor(t, "the deleted session's handle to be forgotten", func() bool {
		return strings.Contains(p.asyncAs(t, p.owner, map[string]any{"action": "status", "id": live}), "unknown_handle")
	})
}

func TestProfileMessageFromANestedChildIsOwnedByTheChildAndKeepsTheChainGuard(t *testing.T) {
	p := newPMFixture(t, nil)
	child := tools.ExecutionContext{SessionKey: p.owner, ProcessOwner: "sub_00000001", ProcessAsync: true, AgentDepth: 1}
	ctx := context.Background()
	out := p.sendAs(t, ctx, child, "alpha-1 from a child")
	id := p.handle(t, out)
	if list := p.list(t); strings.Contains(list, id) {
		t.Fatalf("the session sees its child's handle:\n%s", list)
	}
	got := p.asyncAs(t, "sub_00000001", map[string]any{"action": "wait", "ids": []any{id}, "wait_ms": json.Number("30000")})
	_ = got
	childOwned := p.asyncAs(t, "sub_00000001", map[string]any{"action": "status", "id": id})
	if !strings.Contains(childOwned, "answer for alpha-1") {
		t.Fatalf("the child's own status:\n%s", childOwned)
	}
	// The child acts as the session's profile: it cannot message that profile back.
	cycle := p.message
	out, err := cycle.Execute(ctx, map[string]any{"profile": "dev/coder", "message": "hi", "block_ms": json.Number("0")}, tools.ExecutionContext{CallID: "c", SessionKey: p.owner, ProcessOwner: "sub_00000001", ProcessAsync: true, AgentDepth: 1})
	if err != nil || !strings.Contains(out, "profile_cycle") {
		t.Fatalf("a child of dev/coder messaging dev/coder: %s, %v", out, err)
	}
	// Without the async tool the child waits for the end of the message itself.
	plain := tools.ExecutionContext{SessionKey: p.owner, ProcessOwner: "sub_00000002", AgentDepth: 1}
	inline := p.sendAs(t, ctx, plain, "alpha-2 inline")
	if pmHandle.MatchString(inline) || !strings.Contains(inline, "answer for alpha-2") {
		t.Fatalf("a child without the async tool must get the answer inline:\n%s", inline)
	}
}

func TestProfileMessageHandleCyclesLeaveNoGoroutines(t *testing.T) {
	p := newPMFixture(t, nil)
	cycle := func() {
		release := p.holdTranscript(t)
		queued := p.handle(t, p.send(t, "alpha-1"))
		other := p.handle(t, p.send(t, "alpha-1"))
		p.asyncAs(t, p.owner, map[string]any{"action": "stop", "id": other})
		p.asyncAs(t, p.owner, map[string]any{"action": "stop", "id": queued})
		release()
		awaitTargetSessionFree(t, p.f)
		p.order.mu.Lock()
		p.order.seen = map[string]bool{}
		p.order.mu.Unlock()
		done := p.handle(t, p.send(t, "alpha-1"))
		p.wait(t, done)
	}
	cycle()
	time.Sleep(200 * time.Millisecond)
	before := runtimeGoroutines()
	for range 10 {
		cycle()
	}
	if after := settledGoroutineCount(before); after > before {
		t.Fatalf("goroutines %d -> %d after 10 cycles of queued, stopped and finished messages", before, after)
	}
}

func runtimeGoroutines() int { return runtime.NumGoroutine() }
