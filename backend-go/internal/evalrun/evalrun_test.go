package evalrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/headless"
)

// fakeRunner answers each run by calling act with the session's workspace, so a test can
// play the agent: change files and choose the answer.
type fakeRunner struct {
	mu       sync.Mutex
	root     string
	requests []headless.Request
	act      func(workspace string, request headless.Request) headless.Result
}

func (f *fakeRunner) Run(_ context.Context, request headless.Request) (headless.Result, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	_, id, _ := strings.Cut(request.Session, ":")
	raw, err := os.ReadFile(filepath.Join(f.root, id+".json"))
	if err != nil {
		return headless.Result{}, err
	}
	var session struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return headless.Result{}, err
	}
	result := f.act(session.CWD, request)
	result.Session = request.Session
	return result, nil
}

// writeSuite parses suite with dir as its folder. Workspaces go under a test temp dir
// unless the suite names its own work_dir, so a kept workspace never outlives the test.
func writeSuite(t *testing.T, dir string, suite map[string]any) *Suite {
	t.Helper()
	if _, set := suite["work_dir"]; !set {
		suite["work_dir"] = t.TempDir()
	}
	raw, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSuite(raw, dir)
	if err != nil {
		t.Fatalf("ParseSuite: %v", err)
	}
	return parsed
}

func TestParseSuiteDefaultsAndRefusals(t *testing.T) {
	dir := t.TempDir()
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "smoke",
		"cases": []any{map[string]any{"name": "a", "prompt": "p", "checks": []string{"true"}}},
	})
	if suite.Repeat != DefaultRepeat || suite.Parallel != DefaultParallel || suite.TimeoutMS != DefaultRunTimeoutMS ||
		suite.CheckTimeoutMS != DefaultCheckTimeoutMS || suite.CheckOutputMaxBytes != DefaultCheckOutputMaxBytes ||
		suite.KeepWorkspaces != DefaultKeepWorkspaces || strings.Join(suite.Shell, " ") != "sh -c" {
		t.Fatalf("defaults = %+v", suite)
	}
	base := func(extra map[string]any, cases ...map[string]any) []byte {
		value := map[string]any{"schema_version": 1, "name": "s"}
		for key, item := range extra {
			value[key] = item
		}
		if cases == nil {
			cases = []map[string]any{{"name": "a", "prompt": "p", "checks": []string{"true"}}}
		}
		value["cases"] = cases
		raw, _ := json.Marshal(value)
		return raw
	}
	for name, raw := range map[string][]byte{
		"unknown key":       base(map[string]any{"bogus": 1}),
		"schema version":    base(map[string]any{"schema_version": 2}),
		"bad name":          base(map[string]any{"name": "has space"}),
		"runner's key":      base(map[string]any{"session": map[string]any{"cwd": "/x"}}),
		"repeat too large":  base(map[string]any{"repeat": maxRepeat + 1}),
		"parallel negative": base(map[string]any{"parallel": -1}),
		"keep value":        base(map[string]any{"keep_workspaces": "sometimes"}),
		"no cases":          []byte(`{"schema_version":1,"name":"s","cases":[]}`),
		"duplicate case":    base(nil, map[string]any{"name": "a", "prompt": "p", "checks": []string{"true"}}, map[string]any{"name": "a", "prompt": "p", "checks": []string{"true"}}),
		"empty prompt":      base(nil, map[string]any{"name": "a", "prompt": " ", "checks": []string{"true"}}),
		"cannot fail":       base(nil, map[string]any{"name": "a", "prompt": "p"}),
		"bad pattern":       base(nil, map[string]any{"name": "a", "prompt": "p", "expect_text": []string{"("}}),
		"missing workspace": base(nil, map[string]any{"name": "a", "prompt": "p", "checks": []string{"true"}, "workspace": "nope"}),
		"two values":        append(base(nil), []byte(" {}")...),
	} {
		if _, err := ParseSuite(raw, dir); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRunCopiesTheWorkspaceRunsChecksAndKeepsFailures(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture")
	if err := os.MkdirAll(filepath.Join(fixture, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "sub", "value.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "sessions")
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "smoke", "profile": "dev/coder", "work_dir": "work",
		"session": map[string]any{"scope": map[string]any{"profiles": []string{"dev"}}},
		"cases": []any{
			map[string]any{
				"name": "fix", "prompt": "make it 2", "workspace": "fixture", "setup": []string{"echo set > setup.txt"},
				"checks":      []string{`test "$(cat sub/value.txt)" = 2`, `test -x run.sh`, `grep -q done "$JAD_EVAL_ANSWER_FILE"`},
				"expect_text": []string{"^done"},
			},
			map[string]any{"name": "wrong", "prompt": "make it 3", "workspace": "fixture", "checks": []string{`test "$(cat sub/value.txt)" = 3`}, "model": "openai/large"},
		},
	})
	runner := &fakeRunner{root: sessions, act: func(workspace string, request headless.Request) headless.Result {
		if _, err := os.Stat(filepath.Join(workspace, "setup.txt")); err != nil && strings.Contains(request.Input, "2") {
			t.Errorf("setup did not run before the agent: %v", err)
		}
		_ = os.WriteFile(filepath.Join(workspace, "sub", "value.txt"), []byte("2"), 0o644)
		return headless.Result{Status: headless.StatusCompleted, Text: "done", Usage: json.RawMessage(`{"input_tokens":10,"output_tokens":4}`)}
	}}
	report, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "local", Directory: sessions}}, RunID: "20260102T030405"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Total != 2 || report.Summary.Passed != 1 || report.Summary.Failed != 1 || report.Summary.InputTokens != 20 || report.Summary.OutputTokens != 8 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	pass, fail := report.Results[0], report.Results[1]
	if pass.Outcome != OutcomePass || pass.Kept || len(pass.Checks) != 3 || !pass.Expect[0].Matched {
		t.Fatalf("passing attempt = %+v", pass)
	}
	if _, err := os.Stat(pass.Workspace); !os.IsNotExist(err) {
		t.Fatalf("passing workspace kept: %v", err)
	}
	if fail.Outcome != OutcomeFail || !fail.Kept || !strings.Contains(fail.Reason, "check exited 1") {
		t.Fatalf("failing attempt = %+v", fail)
	}
	if _, err := os.Stat(fail.Workspace); err != nil || !strings.HasPrefix(fail.Workspace, work) {
		t.Fatalf("failing workspace %s: %v", fail.Workspace, err)
	}
	if pass.Session != "local:eval-smoke-20260102T030405-fix-1" || report.RunID != "20260102T030405" {
		t.Fatalf("session = %q", pass.Session)
	}
	raw, err := os.ReadFile(filepath.Join(sessions, "eval-smoke-20260102T030405-fix-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var session map[string]any
	if err := json.Unmarshal(raw, &session); err != nil {
		t.Fatal(err)
	}
	if session["category"] != DefaultSessionCategory || session["cwd"] != pass.Workspace || session["schema_version"] != float64(1) || session["scope"] == nil {
		t.Fatalf("session file = %s", raw)
	}
	if runner.requests[0].Profile != "dev/coder" || runner.requests[1].Model != "openai/large" || runner.requests[0].Timeout != DefaultRunTimeoutMS*time.Millisecond {
		t.Fatalf("requests = %+v", runner.requests)
	}
}

func TestRunOutcomesForSetupFailureAndUnfinishedRun(t *testing.T) {
	dir := t.TempDir()
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "s", "keep_workspaces": "never",
		"cases": []any{
			map[string]any{"name": "setup", "prompt": "p", "setup": []string{"exit 3"}, "checks": []string{"true"}},
			map[string]any{"name": "slow", "prompt": "p", "checks": []string{"true"}},
		},
	})
	runner := &fakeRunner{root: dir, act: func(string, headless.Request) headless.Result {
		return headless.Result{Status: headless.StatusTimedOut, Error: "stopped"}
	}}
	report, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	setup, slow := report.Results[0], report.Results[1]
	if setup.Outcome != OutcomeError || setup.Run != nil || setup.Setup[0].ExitCode != 3 || setup.Kept {
		t.Fatalf("setup attempt = %+v", setup)
	}
	if slow.Outcome != OutcomeFail || !strings.Contains(slow.Reason, "run timed_out") || slow.Checks[0].ExitCode != 0 {
		t.Fatalf("unfinished attempt = %+v, want fail with the checks still run", slow)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runs = %d, want only the case whose setup passed", len(runner.requests))
	}
}

func TestRunRepeatsFiltersAndRunsInParallel(t *testing.T) {
	dir := t.TempDir()
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "s", "repeat": 2, "parallel": 3,
		"cases": []any{
			map[string]any{"name": "keep-a", "prompt": "p", "expect_text": []string{"x"}},
			map[string]any{"name": "keep-b", "prompt": "p", "expect_text": []string{"x"}},
			map[string]any{"name": "skip", "prompt": "p", "expect_text": []string{"x"}},
		},
	})
	var mu sync.Mutex
	active, peak := 0, 0
	runner := &fakeRunner{root: dir, act: func(string, headless.Request) headless.Result {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return headless.Result{Status: headless.StatusCompleted, Text: "x"}
	}}
	var progress strings.Builder
	report, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}, Cases: regexp.MustCompile("^keep"), Repeat: 3, Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Total != 6 || report.Summary.Passed != 6 {
		t.Fatalf("summary = %+v, want 2 cases x 3 attempts", report.Summary)
	}
	for index, want := range []string{"keep-a#1", "keep-a#2", "keep-a#3", "keep-b#1", "keep-b#2", "keep-b#3"} {
		if got := report.Results[index].Case + "#" + string(rune('0'+report.Results[index].Attempt)); got != want {
			t.Fatalf("results[%d] = %s, want %s (job order)", index, got, want)
		}
	}
	if peak < 2 || peak > 3 {
		t.Fatalf("peak concurrency = %d, want between 2 and the suite's 3", peak)
	}
	if strings.Count(progress.String(), "PASS") != 6 {
		t.Fatalf("progress = %q", progress.String())
	}
	if _, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}, Cases: regexp.MustCompile("^none$")}); err == nil {
		t.Fatal("a filter matching nothing: no error")
	}
}

func TestRunCheckTimeoutAndOutputTail(t *testing.T) {
	dir := t.TempDir()
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "s", "check_timeout_ms": 300, "check_output_max_bytes": 8,
		"cases": []any{map[string]any{"name": "a", "prompt": "p", "checks": []string{"printf 0123456789abcdef", "sleep 30 & sleep 30"}}},
	})
	runner := &fakeRunner{root: dir, act: func(string, headless.Request) headless.Result {
		return headless.Result{Status: headless.StatusCompleted}
	}}
	began := time.Now()
	report, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed > 10*time.Second {
		t.Fatalf("took %s: the timed-out check was not killed with its children", elapsed)
	}
	checks := report.Results[0].Checks
	if checks[0].Output != "89abcdef" || !checks[0].Truncated || checks[0].ExitCode != 0 {
		t.Fatalf("tail = %+v", checks[0])
	}
	if !checks[1].TimedOut || checks[1].ExitCode != -1 || report.Results[0].Outcome != OutcomeFail {
		t.Fatalf("timed-out check = %+v, outcome %s", checks[1], report.Results[0].Outcome)
	}
}

func TestRunStopsSchedulingWhenInterrupted(t *testing.T) {
	dir := t.TempDir()
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "s", "repeat": 5,
		"cases": []any{map[string]any{"name": "a", "prompt": "p", "expect_text": []string{"x"}}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeRunner{root: dir, act: func(string, headless.Request) headless.Result {
		cancel()
		return headless.Result{Status: headless.StatusAborted}
	}}
	report, err := Run(ctx, suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Interrupted || report.Summary.Total >= 5 {
		t.Fatalf("report = %+v, want interrupted before every attempt ran", report.Summary)
	}
}

func TestSessionNameFitsTheSessionIDLimit(t *testing.T) {
	name := sessionName(strings.Repeat("s", 64), "20260102T030405", strings.Repeat("c", 64), 1000)
	if len(name) > 128 || !strings.HasSuffix(name, strings.Repeat("c", 64)+"-1000") {
		t.Fatalf("name = %q (%d)", name, len(name))
	}
}

func TestRunIDsDifferWithinOneSecond(t *testing.T) {
	dir := t.TempDir()
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "s",
		"cases": []any{map[string]any{"name": "a", "prompt": "p", "expect_text": []string{"x"}}},
	})
	runner := &fakeRunner{root: dir, act: func(string, headless.Request) headless.Result {
		return headless.Result{Status: headless.StatusCompleted, Text: "x"}
	}}
	fixed := func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	first, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}, Now: fixed})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}, Now: fixed})
	if err != nil {
		t.Fatal(err)
	}
	if first.RunID == second.RunID || first.Summary.Passed != 1 || second.Summary.Passed != 1 || !strings.HasPrefix(first.RunID, "20260102T030405-") {
		t.Fatalf("run ids %q and %q, summaries %+v %+v", first.RunID, second.RunID, first.Summary, second.Summary)
	}
}
