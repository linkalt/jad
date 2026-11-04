package evalrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/headless"
)

func TestCasesReadCommandsAndPlanMode(t *testing.T) {
	dir := t.TempDir()
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "cmds", "plan": true,
		"cases": []any{
			map[string]any{"name": "library", "prompt": "/prompt:review main.go", "expect_text": []string{"ok"}},
			map[string]any{"name": "written", "prompt": "/prompt:review", "commands": false, "plan": false, "expect_text": []string{"ok"}},
			map[string]any{"name": "planned", "prompt": "/plan design it", "plan": false, "expect_text": []string{"ok"}},
		},
	})
	runner := &fakeRunner{root: dir, act: func(string, headless.Request) headless.Result {
		return headless.Result{Status: headless.StatusCompleted, Text: "ok"}
	}}
	report, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}})
	if err != nil || report.Summary.Passed != 3 {
		t.Fatalf("report %+v err %v", report.Summary, err)
	}
	got := map[string]headless.Request{}
	for _, request := range runner.requests {
		got[request.Input] = request
	}
	for input, want := range map[string][2]bool{
		"/prompt:review main.go": {true, true},
		"/prompt:review":         {false, false},
		"/plan design it":        {true, false},
	} {
		request, ok := got[input]
		if !ok || request.Commands != want[0] || request.Plan != want[1] {
			t.Errorf("%q: commands %v plan %v, want %v", input, request.Commands, request.Plan, want)
		}
	}
}

func TestSuiteRefusesActionPromptsUnlessCommandsAreOff(t *testing.T) {
	dir := t.TempDir()
	suite := func(extra string, prompt string) []byte {
		encoded, _ := json.Marshal(prompt)
		return []byte(`{"schema_version":1,"name":"s"` + extra + `,"cases":[{"name":"a","prompt":` + string(encoded) + `,"expect_text":["x"]}]}`)
	}
	for prompt, want := range map[string]string{
		"/reset":         "starts with /reset, which runs no turn",
		"/compact":       "starts with /compact, which runs no turn",
		"/state x":       "starts with /state, which runs no turn",
		"/plan":          "/plan needs a prompt after it",
		"/plan /compact": "/plan cannot be followed by /compact",
	} {
		_, err := ParseSuite(suite("", prompt), dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", prompt, err, want)
		}
		if _, err := ParseSuite(suite(`,"commands":false`, prompt), dir); err != nil {
			t.Errorf("%q with commands false: %v", prompt, err)
		}
		parsed, err := ParseSuiteWith(suite("", prompt), dir, LoadOptions{NoCommands: true})
		if err != nil || parsed.commandsOf(&parsed.Cases[0]) {
			t.Errorf("%q with NoCommands: %v", prompt, err)
		}
	}
	// A case's commands: true is overruled by NoCommands too.
	raw := []byte(`{"schema_version":1,"name":"s","cases":[{"name":"a","prompt":"/reset","commands":true,"expect_text":["x"]}]}`)
	if _, err := ParseSuite(raw, dir); err == nil {
		t.Error("a case with commands true and /reset was accepted")
	}
	if _, err := ParseSuiteWith(raw, dir, LoadOptions{NoCommands: true}); err != nil {
		t.Errorf("NoCommands did not overrule the case: %v", err)
	}
}
