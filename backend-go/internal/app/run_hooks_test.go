package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func runHookEntry(name, script string) map[string]any {
	return map[string]any{"name": name, "argv": []string{"sh", "-c", script}}
}

func readHookLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func newRunHooksFixture(t *testing.T, hooks map[string]any) (*sessionStartFixture, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	triggers := map[string]any{"session_start": map[string]any{"tools": []string{"process"}, "max_bytes": 4096}}
	return newStateContextFixtureWith(t, false, triggers, map[string]any{"hooks": hooks}, logger), logs
}

func TestRunHooksFireOncePerRunInOrderAndOnlyTheLogSeesThem(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	atRequest := filepath.Join(dir, "at-request")
	fixture, logs := newRunHooksFixture(t, map[string]any{"enabled": true,
		"run_start": []any{runHookEntry("begin", "cat >> "+events+"; echo >> "+events+"; echo HOOKMARKER-START")},
		"run_end":   []any{runHookEntry("finish", "cat >> "+events+"; echo >> "+events+"; echo HOOKMARKER-END")},
	})
	fixture.onProviderRequest = func() {
		lines := readHookLines(t, events)
		if err := os.WriteFile(atRequest, []byte(strings.Join(lines, "|")), 0o600); err != nil {
			t.Error(err)
		}
	}

	fixture.run("first")
	seen, err := os.ReadFile(atRequest)
	if err != nil || !strings.Contains(string(seen), `"event":"run_start"`) || strings.Contains(string(seen), `"event":"run_end"`) {
		t.Fatalf("at the first provider request the events were %q (%v); want run_start only", seen, err)
	}
	fixture.run("second")

	var lines []string
	for _, line := range readHookLines(t, events) {
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("got %d events, want run_start and run_end for each of 2 runs:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for index, want := range []string{"run_start", "run_end", "run_start", "run_end"} {
		if !strings.Contains(lines[index], `"event":"`+want+`"`) {
			t.Errorf("event %d = %s, want %s", index, lines[index], want)
		}
	}
	for _, line := range []string{lines[0], lines[1]} {
		if !strings.Contains(line, `"session":"one"`) || !strings.Contains(line, `"turn_id":"`) || strings.Contains(line, `"turn_id":""`) {
			t.Errorf("payload lacks the session or a turn id: %s", line)
		}
	}
	if !strings.Contains(lines[1], `"outcome":"ok"`) {
		t.Errorf("run_end payload = %s, want outcome ok", lines[1])
	}
	startTurn := between(lines[0], `"turn_id":"`, `"`)
	if startTurn == "" || startTurn != between(lines[1], `"turn_id":"`, `"`) {
		t.Errorf("run_start and run_end name different turns: %s / %s", lines[0], lines[1])
	}

	output := logs.String()
	for _, want := range []string{"hook run_start", "hook run_end", "HOOKMARKER-START", "HOOKMARKER-END"} {
		if !strings.Contains(output, want) {
			t.Errorf("the server log lacks %q:\n%s", want, output)
		}
	}
	messages, err := os.ReadFile(fixture.messagesPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(messages), "HOOKMARKER") || strings.Contains(string(messages), "hook run_") {
		t.Errorf("hook output reached messages.jsonl:\n%s", messages)
	}
}

func TestRunHooksReportAFailedRunAsAnError(t *testing.T) {
	events := filepath.Join(t.TempDir(), "end.json")
	fixture, _ := newRunHooksFixture(t, map[string]any{"enabled": true,
		"run_end": []any{runHookEntry("finish", "cat > "+events)},
	})
	fixture.failProvider.Store(true)
	fixture.run("will fail")
	lines := readHookLines(t, events)
	if len(lines) != 1 || !strings.Contains(lines[0], `"outcome":"error"`) || !strings.Contains(lines[0], `"error":"`) {
		t.Fatalf("run_end after a provider failure = %q, want outcome error with the error text", lines)
	}
}

func TestRunHooksStartNothingWhenDisabled(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events")
	fixture, logs := newRunHooksFixture(t, map[string]any{"enabled": false,
		"run_start": []any{runHookEntry("begin", "touch "+events)},
		"run_end":   []any{runHookEntry("finish", "touch "+events)},
	})
	fixture.run("quiet")
	if _, err := os.Stat(events); err == nil {
		t.Fatal("a run hook ran although hooks.enabled is false")
	}
	if strings.Contains(logs.String(), "hook run_") {
		t.Errorf("the log mentions run hooks:\n%s", logs.String())
	}
}

func TestRunEndHookReportsACancelledRunOnAContextTheStopCannotCancel(t *testing.T) {
	events := filepath.Join(t.TempDir(), "end.json")
	logs := &lockedBuffer{}
	runner := boundRunner{
		resolver:  &Resolver{logger: slog.New(slog.NewTextHandler(logs, nil))},
		reference: "local:one",
		config: sessionfs.SessionConfig{Profile: "default", WorkingDir: t.TempDir(), Hooks: sessionfs.HooksSettings{
			Enabled: true, TimeoutMS: 10_000, MaxOutputBytes: 4096, MaxPayloadBytes: 4096, KillGraceMs: 100, KillProcessGroup: "always", MaxLineChars: 2000,
			RunEnd: []sessionfs.RunEventHook{{Name: "finish", Argv: []string{"sh", "-c", "cat > " + events}}},
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	finish := runner.runEndHook(ctx, nil)
	cancel()
	finish("turn-9", context.Canceled)
	lines := readHookLines(t, events)
	if len(lines) != 1 || !strings.Contains(lines[0], `"outcome":"cancelled"`) || !strings.Contains(lines[0], `"turn_id":"turn-9"`) || strings.Contains(lines[0], `"error":`) {
		t.Fatalf("run_end after a stop = %q, want outcome cancelled with no error text", lines)
	}
}

func TestRunHooksDoNotFireForSubAgentRuns(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events")
	f := hooksFixture(t, map[string]any{"enabled": true,
		"run_start": []any{runHookEntry("begin", "echo start >> "+events)},
		"run_end":   []any{runHookEntry("finish", "echo end >> "+events)},
	})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if got := readHookLines(t, events); strings.Join(got, ",") != "start,end" {
		t.Fatalf("events = %q; the parent run fires one start and one end and its child none", got)
	}
}

func between(text, before, after string) string {
	_, rest, found := strings.Cut(text, before)
	if !found {
		return ""
	}
	value, _, _ := strings.Cut(rest, after)
	return value
}

func TestRunHookReportsReachTheRunStreamLiveAndAreNotStored(t *testing.T) {
	fixture, _ := newRunHooksFixture(t, map[string]any{"enabled": true,
		"run_start": []any{runHookEntry("begin", "echo STREAM-START")},
		"run_end":   []any{runHookEntry("finish", "echo STREAM-END")},
	})
	body := fixture.runBody("hello")
	frames := map[string]string{}
	for _, block := range strings.Split(body, "\n\n") {
		event, data, found := strings.Cut(block, "\ndata: ")
		if found && strings.HasSuffix(event, "event: hook_report") {
			switch {
			case strings.Contains(data, "STREAM-START"):
				frames["run_start"] = data
			case strings.Contains(data, "STREAM-END"):
				frames["run_end"] = data
			}
		}
	}
	for event, data := range frames {
		for _, want := range []string{`"event":"` + event + `"`, `"turn_id":"`, `"report":"`} {
			if !strings.Contains(data, want) {
				t.Errorf("%s frame lacks %s: %s", event, want, data)
			}
		}
	}
	if len(frames) != 2 {
		t.Fatalf("want a hook_report frame for run_start and run_end, got %v in:\n%s", frames, body)
	}
	if strings.Index(body, "STREAM-START") > strings.Index(body, "event: completed") {
		t.Errorf("run_start's report must arrive before the run completes:\n%s", body)
	}
	messages, err := os.ReadFile(fixture.messagesPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(messages), "STREAM-") || strings.Contains(string(messages), "hook_report") {
		t.Errorf("a hook report reached messages.jsonl:\n%s", messages)
	}
}
