package evalrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/headless"
)

// A case with output_schema sends it with the run (strict unless output_strict is false), counts
// as a case that can fail, and fails an answer the run reports as invalid_output.
func TestRunOutputSchemaSendsTheSchemaAndFailsAMismatch(t *testing.T) {
	dir := t.TempDir()
	schema := map[string]any{"type": "object", "required": []string{"a"}}
	suite := writeSuite(t, dir, map[string]any{
		"schema_version": 1, "name": "s", "keep_workspaces": "never",
		"cases": []any{
			map[string]any{"name": "good", "prompt": "p", "output_schema": schema},
			map[string]any{"name": "bad", "prompt": "p", "output_schema": schema, "output_strict": false},
		},
	})
	runner := &fakeRunner{root: dir, act: func(_ string, request headless.Request) headless.Result {
		if strings.Contains(request.Session, "-good-") {
			return headless.Result{Status: headless.StatusCompleted, Text: `{"a":1}`, Output: json.RawMessage(`{"a":1}`)}
		}
		return headless.Result{Status: headless.StatusInvalidOutput, Text: `{}`, Error: "the final answer does not match the output schema: /: missing a"}
	}}
	report, err := Run(context.Background(), suite, Options{Runner: runner, Roots: []Root{{ID: "r", Directory: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range runner.requests {
		if request.Output == nil || string(request.Output.Schema) != `{"required":["a"],"type":"object"}` {
			t.Fatalf("the run was not sent the schema: %+v", request.Output)
		}
		if wantStrict := strings.Contains(request.Session, "-good-"); request.Output.Strict != wantStrict {
			t.Fatalf("%s: strict = %v", request.Session, request.Output.Strict)
		}
	}
	byName := map[string]Attempt{}
	for _, result := range report.Results {
		byName[result.Case] = result
	}
	if byName["good"].Outcome != OutcomePass {
		t.Fatalf("good = %+v", byName["good"])
	}
	if bad := byName["bad"]; bad.Outcome != OutcomeFail || !strings.Contains(bad.Reason, "does not match the output schema") {
		t.Fatalf("bad = %+v", bad)
	}
}

func TestParseSuiteRefusesABadOutputSchema(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"not an object":     {"output_schema": []int{1}, "checks": []string{"true"}},
		"does not compile":  {"output_schema": map[string]any{"type": 7}},
		"strict, no schema": {"output_strict": true, "checks": []string{"true"}},
	} {
		value := map[string]any{"name": "a", "prompt": "p"}
		for key, item := range extra {
			value[key] = item
		}
		raw, _ := json.Marshal(map[string]any{"schema_version": 1, "name": "s", "cases": []any{value}})
		if _, err := ParseSuite(raw, t.TempDir()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
