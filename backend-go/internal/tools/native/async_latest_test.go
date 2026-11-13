package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// latestBlocksOf decodes the latest line of an async response.
func latestBlocksOf(t *testing.T, out string) []latestBlock {
	t.Helper()
	_, block, _ := strings.Cut(out, "--- INFORMATION ---\n")
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "latest: [") {
			var blocks []latestBlock
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "latest: ")), &blocks); err != nil {
				t.Fatalf("latest line: %v\n%s", err, line)
			}
			return blocks
		}
	}
	return nil
}

func latestTextOf(t *testing.T, out, id string) string {
	t.Helper()
	for _, block := range latestBlocksOf(t, out) {
		if block.ID == id {
			return block.Text
		}
	}
	return ""
}

// latestFixture runs bash through exec and hands each run off as an async handle at once,
// so its output is read through the async tool's latest blocks.
type latestFixture struct{ execAsyncFixture }

func newLatestFixture(t *testing.T) latestFixture {
	t.Helper()
	f := newExecAsyncFixture(t)
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, f.root)
	settings.Child.TimeoutMs, settings.Child.MaxTimeoutMs = 20_000, 30_000
	settings.Child.Stdout = sessionfs.NativeBashStreamBudget{HeadBytes: 1024, TailBytes: 4096}
	settings.Child.Stderr = sessionfs.NativeBashStreamBudget{HeadBytes: 1024, TailBytes: 4096}
	execTool, err := NewExecTool(settings, "", f.host)
	if err != nil {
		t.Fatal(err)
	}
	f.exec = execTool
	return latestFixture{f}
}

// start runs command and returns the id of its handle.
func (f latestFixture) start(t *testing.T, owner, command string) string {
	t.Helper()
	arguments, _ := json.Marshal(map[string]any{"lang": "bash", "code": command, "block_ms": 0})
	return execRunIDOf(t, f.run(t, owner, string(arguments)))
}

func (f latestFixture) status(t *testing.T, id string) string {
	t.Helper()
	return callAsync(t, f.async, "s1", map[string]any{"action": "status", "id": id})
}

// waitForOutput waits until the run's live output holds want, without reading a latest
// block (which would move its cursor).
func (f latestFixture) waitForOutput(t *testing.T, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.host.mu.Lock()
		run := f.host.shellRuns[id]
		f.host.mu.Unlock()
		if run != nil {
			if collector := run.watch.get(); collector != nil {
				rendered := collector.render()
				if strings.Contains(rendered.stdout.text+rendered.stderr.text, want) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never printed %q", id, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLatestShowsOnlyNewOutputOnTheSecondReturn(t *testing.T) {
	f := newLatestFixture(t)
	id := f.start(t, "s1", "sleep 0.3; echo first-line; sleep 0.5; echo second-line; sleep 30")
	f.waitForOutput(t, id, "first-line")
	first := latestTextOf(t, f.status(t, id), id)
	if !strings.Contains(first, "first-line") || !strings.Contains(first, "running") {
		t.Fatalf("first block:\n%s", first)
	}
	f.waitForOutput(t, id, "second-line")
	second := latestTextOf(t, f.status(t, id), id)
	if !strings.Contains(second, "second-line") || strings.Contains(second, "first-line") {
		t.Fatalf("second block must hold only the new line:\n%s", second)
	}
	third := latestTextOf(t, f.status(t, id), id)
	if !strings.Contains(third, "no new output since") || strings.Contains(third, "second-line") {
		t.Fatalf("third block must say nothing is new:\n%s", third)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
}

func TestLatestKeepsStdoutAndStderrApart(t *testing.T) {
	f := newLatestFixture(t)
	id := f.start(t, "s1", "sleep 0.3; echo to-out; echo to-err >&2; sleep 30")
	f.waitForOutput(t, id, "to-out")
	f.waitForOutput(t, id, "to-err")
	text := latestTextOf(t, f.status(t, id), id)
	out, err, found := strings.Cut(text, "[stderr]\n")
	if !found || !strings.Contains(out, "[stdout]\nto-out") || strings.Contains(out, "to-err") || !strings.HasPrefix(err, "to-err") {
		t.Fatalf("streams mixed:\n%s", text)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
}

func TestLatestCutsTheFrontWithAMarkerAndStripsControlCodes(t *testing.T) {
	f := newLatestFixture(t)
	id := f.start(t, "s1", `sleep 0.3; printf '\033[31m'; seq 1 600; printf 'END-MARK\033[0m\n'; sleep 30`)
	f.waitForOutput(t, id, "END-MARK")
	text := latestTextOf(t, f.status(t, id), id)
	if !strings.Contains(text, "…+") || !strings.Contains(text, " bytes earlier") || !strings.HasSuffix(strings.TrimSpace(text), "END-MARK") {
		t.Fatalf("want a front cut with a marker and the end kept:\n%s", text)
	}
	if _, body, _ := strings.Cut(text, "[stdout]\n"); strings.Contains(text, "\x1b") || len(body) > 500+len("…+99999 bytes earlier\n") {
		t.Fatalf("escape codes or too many bytes (cap 500 per stream):\n%q", text)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
}

func TestLatestStartToolReturnsTheBlockAndWaitContinuesFromIt(t *testing.T) {
	f := newLatestFixture(t)
	out := f.run(t, "s1", `{"lang":"bash","code":"echo early; sleep 30","block_ms":300}`)
	id := execRunIDOf(t, out)
	if !strings.Contains(execField(out, "latest"), "early") {
		t.Fatalf("the still-running start result lacks the latest block:\n%s", out)
	}
	if later := latestTextOf(t, f.status(t, id), id); strings.Contains(later, "early") || !strings.Contains(later, "no new output") {
		t.Fatalf("what the start call showed was shown again:\n%s", later)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
}

func TestLatestOfSeveralHandlesInOneWaitAndFinishedOnesAreUnchanged(t *testing.T) {
	f := newLatestFixture(t)
	a := f.start(t, "s1", "sleep 0.3; echo from-a; sleep 30")
	b := f.start(t, "s1", "sleep 0.3; echo from-b; sleep 30")
	done := f.start(t, "s1", "sleep 1; echo finished-text")
	f.waitForOutput(t, a, "from-a")
	f.waitForOutput(t, b, "from-b")
	out := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("5000")})
	if !strings.Contains(out, done) || !strings.Contains(out, "finished-text") || !strings.Contains(out, `"state":"done"`) {
		t.Fatalf("the finished handle must come back in full:\n%s", out)
	}
	blocks := latestBlocksOf(t, out)
	if len(blocks) != 2 {
		t.Fatalf("want one block per running handle, got %d:\n%s", len(blocks), out)
	}
	for _, block := range blocks {
		if block.ID == done {
			t.Fatalf("a finished handle got a latest block: %+v", block)
		}
	}
	if !strings.Contains(latestTextOf(t, out, a), "from-a") || !strings.Contains(latestTextOf(t, out, b), "from-b") {
		t.Fatalf("blocks:\n%+v", blocks)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": a})
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": b})
}

func TestLatestTotalIsCappedAcrossHandles(t *testing.T) {
	cfg := asyncmgr.DefaultConfig()
	cfg.LatestBytes, cfg.LatestTotalBytes, cfg.LatestValueBytes = 100, 150, 40
	host, err := NewAsyncHost(filepath.Join(t.TempDir(), ".async"), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	// Oldest 60 bytes, then the newest 40: the ends are told apart, which all-x text cannot do.
	driver := &fakeLatestDriver{text: strings.Repeat("x", 60) + strings.Repeat("Z", 40)}
	_ = host.manager.AddDriver(driver)
	for _, id := range []string{"h1", "h2", "h3"} {
		if err := host.manager.Register(asyncmgr.Registration{ID: id, Owner: "s1", Kind: "fake"}); err != nil {
			t.Fatal(err)
		}
	}
	blocks := host.latestBlocks("s1", []string{"h1", "h2", "h3"})
	total := 0
	for _, block := range blocks {
		total += len(block.Text)
	}
	if total > 150 || len(blocks) != 2 || len(blocks[1].Text) != 50 {
		t.Fatalf("blocks = %d, total = %d, want 2 blocks within 150 bytes", len(blocks), total)
	}
	// The block that fits only in part keeps what is new (the Z tail) and says what it dropped,
	// marker included in the budget (P6BM-05).
	if second := blocks[1].Text; !strings.HasPrefix(second, "…+71 bytes earlier\n") || !strings.HasSuffix(second, strings.Repeat("Z", 29)) || strings.Contains(second, "x") {
		t.Fatalf("the cut block must keep its newest bytes and mark the rest:\n%q", second)
	}
	if driver.calls["h3"] != 0 {
		t.Fatalf("a handle past the cap must not move its cursor or be asked: %v", driver.calls)
	}
}

type fakeLatestDriver struct {
	text  string
	calls map[string]int
}

func (d *fakeLatestDriver) Kind() asyncmgr.Kind    { return "fake" }
func (d *fakeLatestDriver) Stop(id, reason string) {}
func (d *fakeLatestDriver) latest(id string, _ latestInfo, prev any, _ asyncmgr.Config, _ time.Time) (string, any) {
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[id]++
	return d.text, prev
}

func TestLatestForExecAndToolSmithHandles(t *testing.T) {
	f := newExecAsyncFixture(t)
	started := f.run(t, "s1", `{"code":"echo exec-early; sleep 30","block_ms":400}`)
	id := execRunIDOf(t, started)
	if !strings.Contains(execField(started, "latest"), "exec-early") {
		t.Fatalf("exec still-running result lacks the block:\n%s", started)
	}
	status := callAsync(t, f.async, "s1", map[string]any{"action": "status", "id": id})
	if text := latestTextOf(t, status, id); strings.Contains(text, "exec-early") || !strings.Contains(text, "no new output") {
		t.Fatalf("exec delta:\n%s", text)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
}

func appendActivity(t *testing.T, path string, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func writeActivity(t *testing.T, path string, lines ...string) {
	t.Helper()
	writeTestTranscript(t, path, lines...)
}

func TestLatestActivityShowsCallsAndRepliesNeverReasoningAndOnlyNewSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeActivity(t, path,
		userLine("do the task"),
		answerLine(1, 1, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "PRIVATE-MUSING"}}}, callItem("c1", "read", `{"path":"/a/very/long/path/that/goes/on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/file.go"}`)),
		resultLine("c1", "package a"),
	)
	cfg := asyncmgr.DefaultConfig()
	now := time.Now()
	info := latestInfo{state: asyncmgr.StateRunning, started: now.Add(-3 * time.Second)}
	text, next := activityLatest(path, info, nil, cfg, now, true)
	if !strings.Contains(text, "call read") || !strings.Contains(text, "transcript: "+path) || !strings.Contains(text, "last step 1.") || !strings.Contains(text, "last event") {
		t.Fatalf("first block:\n%s", text)
	}
	if strings.Contains(text, "PRIVATE-MUSING") || strings.Contains(text, "reasoning") {
		t.Fatalf("reasoning leaked:\n%s", text)
	}
	if strings.Contains(text, "on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/and/on/file.go") {
		t.Fatalf("argument values must be shortened:\n%s", text)
	}
	appendActivity(t, path, answerLine(1, 2, textItem("the preview of my answer")))
	text, next = activityLatest(path, info, next, cfg, now, true)
	if !strings.Contains(text, "the preview of my answer") || strings.Contains(text, "call read") || !strings.Contains(text, "1 new step") {
		t.Fatalf("second block must show only the new reply:\n%s", text)
	}
	text, _ = activityLatest(path, info, next, cfg, now, true)
	if !strings.Contains(text, "no new activity since") || strings.Contains(text, "the preview of my answer") || !strings.Contains(text, "transcript: "+path) {
		t.Fatalf("third block:\n%s", text)
	}
}

func TestLatestActivityHonoursTheLineCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	lines := []string{userLine("go")}
	for i := 1; i <= 6; i++ {
		lines = append(lines, answerLine(1, i, callItem(fmt.Sprintf("c%d", i), "read", fmt.Sprintf(`{"path":"f%d.go"}`, i))), resultLine(fmt.Sprintf("c%d", i), "ok"))
	}
	writeActivity(t, path, lines...)
	cfg := asyncmgr.DefaultConfig()
	text, _ := activityLatest(path, latestInfo{state: asyncmgr.StateRunning, started: time.Now()}, nil, cfg, time.Now(), true)
	if strings.Count(text, "call read") != int(cfg.LatestLines) || !strings.Contains(text, "f6.go") || strings.Contains(text, "f3.go") {
		t.Fatalf("want the last %d calls only:\n%s", cfg.LatestLines, text)
	}
}

func TestSubagentHandleReportsItsActivityThroughAsyncStatus(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "child")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "messages.jsonl")
	writeActivity(t, transcript, userLine("task"), answerLine(1, 1, callItem("c1", "read", `{"path":"one.go"}`)), resultLine("c1", "x"))
	release := make(chan struct{})
	started := make(chan struct{})
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(dir)
		close(started)
		select {
		case <-release:
			return SubagentResult{Text: "done"}, nil
		case <-ctx.Done():
			return SubagentResult{}, ctx.Err()
		}
	}}
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), subagentSettings(agentNamed("explore")), launcher)
	out := f.start(t, "s1", map[string]any{"task": "look", "block_ms": int64(400)})
	<-started
	id := runIDOf(t, out)
	// The result names the transcript once, as its transcript item; the latest block keeps the
	// news and the last step but not the path.
	latest := execField(out, "latest")
	if !strings.Contains(latest, "call read") || !strings.Contains(latest, "last transcript step 1.1") || strings.Contains(latest, transcript) || strings.Contains(latest, "(last step") {
		t.Fatalf("a sub-agent's start result must keep the last step without repeating the path:\n%s", out)
	}
	if execField(out, "transcript") != transcript || strings.Count(out, transcript) != 1 {
		t.Fatalf("the transcript path appears %d times in the running result, want once (as transcript):\n%s", strings.Count(out, transcript), out)
	}
	appendActivity(t, transcript, answerLine(1, 2, textItem("now halfway through")))
	status := callAsync(t, f.async, "s1", map[string]any{"action": "status", "id": id})
	if text := latestTextOf(t, status, id); !strings.Contains(text, "now halfway through") || strings.Contains(text, "call read") || strings.Count(text, transcript) != 1 || !strings.Contains(text, "(last step 1.2)") {
		t.Fatalf("a poll shows only the delta and still names the transcript once:\n%s", text)
	}
	if again := latestTextOf(t, callAsync(t, f.async, "s1", map[string]any{"action": "status", "id": id}), id); !strings.Contains(again, "no new activity") || strings.Count(again, transcript) != 1 {
		t.Fatalf("a quiet poll holds only the delta and the transcript:\n%s", again)
	}
	close(release)
}

// Before the child has a transcript folder the result has no transcript item, so nothing may be
// left out of the latest block either: it says there is no transcript yet, and the path shows
// up in a later poll once the folder exists.
func TestSubagentStartResultWithoutFolderThenPollNamesTranscript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "child")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "messages.jsonl")
	writeActivity(t, transcript, userLine("task"), answerLine(1, 1, callItem("c1", "read", `{"path":"one.go"}`)), resultLine("c1", "x"))
	release, folder := make(chan struct{}), make(chan struct{})
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		select {
		case <-folder:
			request.OnTranscript(dir)
		case <-ctx.Done():
			return SubagentResult{}, ctx.Err()
		}
		select {
		case <-release:
			return SubagentResult{Text: "done"}, nil
		case <-ctx.Done():
			return SubagentResult{}, ctx.Err()
		}
	}}
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), subagentSettings(agentNamed("explore")), launcher)
	out := f.start(t, "s1", map[string]any{"task": "look", "block_ms": int64(100)})
	id := runIDOf(t, out)
	if execField(out, "transcript") != "" || strings.Contains(out, "messages.jsonl") || !strings.Contains(execField(out, "latest"), "no transcript yet") {
		t.Fatalf("a run without a folder names no transcript:\n%s", out)
	}
	close(folder)
	deadline := time.Now().Add(5 * time.Second)
	for {
		text := latestTextOf(t, callAsync(t, f.async, "s1", map[string]any{"action": "status", "id": id}), id)
		if strings.Count(text, transcript) == 1 && strings.Contains(text, "call read") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a poll after the folder exists must name the transcript once with the news:\n%s", text)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
}

func TestProfileMessageHandleReportsTheTargetsActivity(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	transcript := filepath.Join(t.TempDir(), "messages.jsonl")
	writeActivity(t, transcript, userLine("hi"), answerLine(1, 1, textItem("working on the review")))
	messenger := &pathMessenger{controlMessenger: f.messenger, path: transcript}
	message, err := NewProfileMessageTool(sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024, MaxQueued: 4}, profileMessageTargets, messenger, f.host)
	if err != nil {
		t.Fatal(err)
	}
	f.message = message
	out := f.send(t, tools.ExecutionContext{SessionKey: "s1:main"}, 50)
	id := handleIDPattern.FindString(out)
	if id == "" || !strings.Contains(execField(out, "latest"), "last transcript step 1.") {
		t.Fatalf("still-running result lacks the activity block:\n%s", out)
	}
	// The result already names the transcript, so the latest block must not repeat it.
	if latest := execField(out, "latest"); strings.Contains(latest, transcript) || strings.Contains(latest, "(last step") {
		t.Fatalf("latest repeats the transcript path:\n%s", out)
	}
	if got := strings.Count(out, transcript); got != 1 {
		t.Fatalf("the transcript path appears %d times in the running result, want once (as transcript):\n%s", got, out)
	}
	appendActivity(t, transcript, answerLine(1, 2, textItem("now halfway through")))
	status := callAsync(t, f.async, "s1:main", map[string]any{"action": "status", "id": id})
	if text := latestTextOf(t, status, id); !strings.Contains(text, "now halfway through") || strings.Contains(text, "working on the review") || strings.Count(text, transcript) != 1 {
		t.Fatalf("what ran before the handle started or was shown must not come back, and the poll names the transcript once:\n%s", text)
	}
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "ok"})
}

type pathMessenger struct {
	*controlMessenger
	path string
}

func (m *pathMessenger) Prepare(ctx context.Context, request ProfileMessageRequest) (ProfileMessagePlan, error) {
	plan, err := m.controlMessenger.Prepare(ctx, request)
	return pathPlan{plan: plan, path: m.path}, err
}

type pathPlan struct {
	plan ProfileMessagePlan
	path string
}

func (p pathPlan) Start(ctx context.Context) (ProfileMessageRun, error) { return p.plan.Start(ctx) }
func (p pathPlan) Transcripts() (string, string)                        { return p.path, "" }

func TestRemoteRunLatestNamesNoPaths(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	out := f.start(t, remoteAgent, 100, remoteCaller)
	id := remoteRunHandle.FindString(out)
	latest := execField(out, "latest")
	if id == "" || !strings.Contains(latest, "remote agent run rr_") || strings.Contains(latest, "/") {
		t.Fatalf("remote latest:\n%s", out)
	}
	status := f.asyncCall(t, map[string]any{"action": "status", "id": id})
	text := latestTextOf(t, status, id)
	if !strings.Contains(text, "remote agent run rr_") || strings.Contains(text, "transcript") || strings.Contains(text, "messages.jsonl") || strings.Contains(text, string(filepath.Separator)+"tmp") {
		t.Fatalf("a remote run must show its id only, no files:\n%s", text)
	}
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "late"})
}

func TestLatestIsNotShownForAFinishedHandle(t *testing.T) {
	f := newLatestFixture(t)
	id := f.start(t, "s1", "sleep 0.2; echo all-done")
	f.waitState(t, "s1", id, asyncmgr.StateDone)
	if status := f.status(t, id); len(latestBlocksOf(t, status)) != 0 {
		t.Fatalf("finished handle shows a latest block:\n%s", status)
	}
}

func TestLatestForARunningToolSmithShowsNewOutputOnlyThenNothingNew(t *testing.T) {
	f := newToolSmithAsyncFixture(t, `echo smith-early; sleep 30`)
	started := f.run(t, "s1", `{"code":"return 1;","block_ms":600}`)
	id := smithRunIDOf(t, started)
	if !strings.Contains(execField(started, "latest"), "smith-early") {
		t.Fatalf("the still-running start result lacks the latest block:\n%s", started)
	}
	status := callAsync(t, f.async, "s1", map[string]any{"action": "status", "id": id})
	if text := latestTextOf(t, status, id); strings.Contains(text, "smith-early") || !strings.Contains(text, "no new output since") {
		t.Fatalf("what the start call showed was shown again:\n%s", text)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
}

func TestIndexTranscriptTurnsFromResumesAtATurnStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeActivity(t, path,
		userLine("one"), answerLine(1, 1, textItem("a")),
		userLine("two"), answerLine(2, 1, textItem("b")),
		userLine("three"), answerLine(3, 1, textItem("c")),
	)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ctx, maxLine := context.Background(), asyncmgr.DefaultConfig().LatestScanMaxBytes
	all, complete, err := indexTranscriptTurns(ctx, file, stat.Size(), maxLine)
	if err != nil || len(all) != 3 || all[2].number != 3 {
		t.Fatalf("whole index = %+v, complete %d, err %v", all, complete, err)
	}
	part, partComplete, err := indexTranscriptTurnsFrom(ctx, file, all[2].off, stat.Size(), maxLine, all[2].number)
	if err != nil || partComplete != complete {
		t.Fatalf("resumed index: complete %d, err %v, want complete %d", partComplete, err, complete)
	}
	if len(part) != 1 || part[0] != all[2] {
		t.Fatalf("resumed turns = %+v, want the tail of %+v", part, all)
	}
	// Nothing appended since: the resumed table is the turn it starts at, and the offset a
	// cursor already holds comes back unchanged rather than falling to 0.
	empty, emptyComplete, err := indexTranscriptTurnsFrom(ctx, file, stat.Size(), stat.Size(), maxLine, all[2].number)
	if err != nil || len(empty) != 1 || empty[0].off != stat.Size() || emptyComplete != stat.Size() {
		t.Fatalf("resume at the end = %+v, complete %d, err %v", empty, emptyComplete, err)
	}
	// A line the run is still writing is neither indexed nor reported as complete.
	half, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := half.WriteString(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"c`); err != nil {
		t.Fatal(err)
	}
	if err := half.Close(); err != nil {
		t.Fatal(err)
	}
	unfinished, unfinishedComplete, err := indexTranscriptTurnsFrom(ctx, file, all[2].off, stat.Size(), maxLine, all[2].number)
	if err != nil || unfinishedComplete != complete {
		t.Fatalf("half-written line: complete %d, err %v, want %d", unfinishedComplete, err, complete)
	}
	if len(unfinished) != 1 || unfinished[0] != all[2] {
		t.Fatalf("turns over a half-written line = %+v, want just %+v", unfinished, all[2])
	}
}

func TestResumeTranscriptTurnsFallsBackFromAStaleCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeActivity(t, path, userLine("one"), answerLine(1, 1, textItem("a")))
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	turn := int64(len(userLine("one")) + 1)
	if start, number := resumeTranscriptTurns(file, activityProgress{turnOff: turn, turn: 2}, stat.Size()); start != turn || number != 2 {
		t.Fatalf("resume = %d/%d, want %d/2", start, number, turn)
	}
	// A cursor whose turn start is not a line start any more, one that points past the end, and
	// one that never carried a turn all index the transcript from its beginning.
	for _, cursor := range []activityProgress{
		{turnOff: turn - 1, turn: 2},
		{turnOff: stat.Size() + 1, turn: 2},
		{turnOff: turn, turn: 0},
		{turnOff: 0, turn: 2},
	} {
		if start, number := resumeTranscriptTurns(file, cursor, stat.Size()); start != 0 || number != 0 {
			t.Fatalf("resume from %+v = %d/%d, want a whole index", cursor, start, number)
		}
	}
}

func TestLatestActivityIndexesOnlyTheTailAppendedSinceItsCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	head := []string{
		userLine("do the task"),
		answerLine(1, 1, callItem("c1", "read", `{"path":"one.go"}`)),
		resultLine("c1", "ok"),
	}
	// The first poll of a handle indexes the whole file, so turn 2 has to exist before it for
	// there to be a turn start to resume at; that is what a run's second turn looks like.
	writeActivity(t, path, append(head, userLine("and more"))...)
	appendActivity(t, path, messageLine("FIRST-NEWS"))
	cfg := asyncmgr.DefaultConfig()
	now := time.Now()
	info := latestInfo{state: asyncmgr.StateRunning, started: now.Add(-time.Minute)}
	text, next := activityLatest(path, info, nil, cfg, now, true)
	if !strings.Contains(text, "FIRST-NEWS") {
		t.Fatalf("first block:\n%s", text)
	}
	cursor, ok := next.(*activityProgress)
	if !ok || cursor.turnOff != int64(len(strings.Join(head, "\n")+"\n")) || cursor.turn != 2 {
		t.Fatalf("cursor = %+v, want a resume at the start of turn 2", cursor)
	}
	// Turn 1 now says its response was turn 8, which a poll that indexed from the beginning
	// would carry into turn 2 (9) while a poll that resumes at the cursor keeps the number it
	// already had (2). The head is rewritten in place, same length, so nothing else moves.
	before := strings.Join(head, "\n") + "\n"
	mangled := strings.Replace(before, `"session_turn":1`, `"session_turn":8`, 1)
	if len(mangled) != len(before) || mangled == before {
		t.Fatalf("the fixture did not rewrite the head: %q", mangled)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte(mangled), 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	appendActivity(t, path, messageLine("SECOND-NEWS"))
	text, next = activityLatest(path, info, cursor, cfg, now, true)
	if !strings.Contains(text, "SECOND-NEWS") || strings.Contains(text, "FIRST-NEWS") {
		t.Fatalf("second block:\n%s", text)
	}
	var step string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "SECOND-NEWS") {
			step = line
		}
	}
	if !strings.HasPrefix(step, "2.") {
		t.Fatalf("the resumed index lost the turn numbers its cursor held (a re-index from 0 numbers it 9):\n%s", text)
	}
	if again, _ := next.(*activityProgress); again == nil || again.turn != 2 || again.turnOff != cursor.turnOff {
		t.Fatalf("cursor after the second poll = %+v", next)
	}
}

// messageLine is an assistant reply as a plain message record: unlike answerLine it carries no
// session_turn, so it takes the turn number of the index around it.
func messageLine(text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
	return string(raw)
}

// A caller whose result already names the transcript asks activityLatest not to repeat the
// path: every branch keeps its news and the last step, as "last transcript step", and none
// names the file. The same branches with namePath keep "transcript: <path>" as before.
func TestLatestActivityWithoutPathNamesOnlyTheLastTranscriptStep(t *testing.T) {
	cfg := asyncmgr.DefaultConfig()
	now := time.Now()
	info := latestInfo{state: asyncmgr.StateRunning, started: now.Add(-3 * time.Second)}

	t.Run("steps", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "messages.jsonl")
		writeActivity(t, path, userLine("do the task"), answerLine(1, 1, callItem("c1", "read", `{"path":"a.go"}`)), resultLine("c1", "package a"))
		text, next := activityLatest(path, info, nil, cfg, now, false)
		if !strings.Contains(text, "call read") || !strings.HasSuffix(text, "\nlast transcript step 1.1") || strings.Contains(text, path) || strings.Contains(text, "(last step") {
			t.Fatalf("news block:\n%s", text)
		}
		text, _ = activityLatest(path, info, next, cfg, now, false)
		if !strings.Contains(text, "no new activity since") || !strings.HasSuffix(text, "\nlast transcript step 1.1") || strings.Contains(text, path) {
			t.Fatalf("no-news block:\n%s", text)
		}
		text, _ = activityLatest(path, info, nil, cfg, now, true)
		if !strings.Contains(text, "transcript: "+path+" (last step 1.1)") || strings.Contains(text, "last transcript step") {
			t.Fatalf("with the path:\n%s", text)
		}
	})
	t.Run("no steps yet", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "messages.jsonl")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		text, _ := activityLatest(path, info, nil, cfg, now, false)
		if !strings.HasSuffix(text, "; no steps yet") || strings.Contains(text, path) {
			t.Fatalf("no steps, no path:\n%s", text)
		}
		text, _ = activityLatest(path, info, nil, cfg, now, true)
		if !strings.HasSuffix(text, "; no steps yet\ntranscript: "+path) {
			t.Fatalf("no steps, with the path:\n%s", text)
		}
	})
	t.Run("too large", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "messages.jsonl")
		writeActivity(t, path, userLine("do the task"), answerLine(1, 1, textItem("working")))
		small := cfg
		small.LatestScanMaxBytes = 10
		text, _ := activityLatest(path, info, nil, small, now, false)
		if !strings.HasSuffix(text, "\ntranscript too large to summarize (latest_scan_max_bytes)") || strings.Contains(text, path) {
			t.Fatalf("too large, no path:\n%s", text)
		}
		text, _ = activityLatest(path, info, nil, small, now, true)
		if !strings.HasSuffix(text, "(latest_scan_max_bytes); transcript: "+path) {
			t.Fatalf("too large, with the path:\n%s", text)
		}
	})
}

type labelledMessenger struct {
	*pathMessenger
	label string
}

func (m *labelledMessenger) Prepare(ctx context.Context, request ProfileMessageRequest) (ProfileMessagePlan, error) {
	plan, err := m.pathMessenger.Prepare(ctx, request)
	return labelledPlan{pathPlan: plan.(pathPlan), label: m.label}, err
}

// labelledPlan is a target of another workspace: Transcripts is the file, TranscriptLabels is
// what the model is shown instead.
type labelledPlan struct {
	pathPlan
	label string
}

func (p labelledPlan) TranscriptLabels() (string, string) { return p.label, "" }

// A message to a profile of another workspace is shown by its name, never by the file of the
// other workspace: not in the running result, not in the polls of its handle, not in the final
// result's ref_transcript. The host still follows the file, so the activity is reported.
func TestProfileMessageHandleOfAnotherWorkspaceNamesNoPath(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	transcript := filepath.Join(t.TempDir(), "messages.jsonl")
	writeActivity(t, transcript, userLine("hi"), answerLine(1, 1, textItem("working on the review")))
	const label = "b:s1/review/reviewer"
	messenger := &labelledMessenger{pathMessenger: &pathMessenger{controlMessenger: f.messenger, path: transcript}, label: label}
	message, err := NewProfileMessageTool(sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024, MaxQueued: 4}, profileMessageTargets, messenger, f.host)
	if err != nil {
		t.Fatal(err)
	}
	f.message = message
	out := f.send(t, tools.ExecutionContext{SessionKey: "s1:main"}, 50)
	id := handleIDPattern.FindString(out)
	if id == "" || strings.Contains(out, transcript) || strings.Count(out, label) != 1 || !strings.Contains(execField(out, "latest"), "last transcript step 1.") {
		t.Fatalf("the running result names the label once, no path, and has the activity:\n%s", out)
	}
	appendActivity(t, transcript, answerLine(1, 2, textItem("now halfway through")))
	status := callAsync(t, f.async, "s1:main", map[string]any{"action": "status", "id": id})
	if text := latestTextOf(t, status, id); !strings.Contains(text, "now halfway through") || strings.Contains(status, transcript) || strings.Count(text, label) != 1 {
		t.Fatalf("a poll shows the news and names the label, never the file:\n%s", status)
	}
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "ok"})
	waitForState(t, f.host, "s1:main", id, asyncmgr.StateDone)
	final := callAsync(t, f.async, "s1:main", map[string]any{"action": "status", "id": id})
	if strings.Contains(final, transcript) || !strings.Contains(final, label) {
		t.Fatalf("the finished handle's ref_transcript is the label:\n%s", final)
	}
}
