package headless

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOutputJSONTextRemovesOnlyAWholeJSONFence(t *testing.T) {
	for text, want := range map[string]string{
		` {"a":1} `:                          `{"a":1}`,
		"```json\n{\"a\":1}\n```":            `{"a":1}`,
		"```JSON\n{\"a\":1}\n```":            `{"a":1}`,
		"```\n[1]\n```":                      `[1]`,
		"```go\nx\n```":                      "```go\nx\n```",
		"```json\n{}\n```\n```json\n{}\n```": "```json\n{}\n```\n```json\n{}\n```",
		"see ```json\n{}\n```":               "see ```json\n{}\n```",
		"``````":                             "``````",
	} {
		if got := outputJSONText(text); got != want {
			t.Errorf("outputJSONText(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestOutputCheckBoundsTheReportedIssues(t *testing.T) {
	schema, err := NewOutputSchema(json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"},"c":{"type":"integer"},"d":{"type":"integer"},"e":{"type":"integer"},"f":{"type":"integer"},"g":{"type":"integer"}}}`), "", true)
	if err != nil {
		t.Fatal(err)
	}
	result := Result{Status: StatusCompleted, Text: `{"a":"x","b":"x","c":"x","d":"x","e":"x","f":"x","g":"x"}`}
	schema.check(&result)
	if result.Status != StatusInvalidOutput || len(result.Output) != 0 || !strings.Contains(result.Error, "and 2 more") || strings.Count(result.Error, "; ") != maxReportedOutputIssues {
		t.Fatalf("seven issues should be reported as five and a count: %+v", result)
	}
	if got := cutText(strings.Repeat("é", 200), 201); got != strings.Repeat("é", 100)+"..." {
		t.Fatalf("cutText split a rune: %q", got)
	}
	if _, err := NewOutputSchema(json.RawMessage(`[]`), "", true); err == nil {
		t.Fatal("an array schema was accepted")
	}
}
