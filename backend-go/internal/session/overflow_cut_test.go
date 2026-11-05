package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/tools"
)

// bigOutputCatalog has one lookup tool whose output is 3000 bytes that end with "END".
func bigOutputCatalog(t *testing.T) *tools.Catalog {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{{
		Definition: tools.Definition{ID: "native.lookup", Source: "native", Name: "lookup", Description: "Look up.", InputSchema: schema},
		Validator:  validator,
		Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
			return "START" + strings.Repeat("x", 2992) + "END", nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

type cutCall struct{ attempt, maxBytes, cut int }

func cutOverflowSettings(attempts, maxBytes int, compactions *[]overflowCall, cuts *[]cutCall) *AutomaticCompactionSettings {
	settings := overflowCompaction(true, attempts, compactions)
	settings.KeepTurns = 0
	settings.Overflow.ToolOutputMaxBytes = maxBytes
	settings.Overflow.OnToolOutputsCut = func(attempt, maxBytes, cut int) {
		*cuts = append(*cuts, cutCall{attempt, maxBytes, cut})
	}
	return settings
}

// An overflow inside the run's own turn, which compaction cannot shrink, is sent again with
// the tool outputs cut; the cut lasts for the later rounds of the run, and the transcript
// keeps the outputs whole.
func TestOverflowInsideTheRunsTurnCutsItsToolOutputs(t *testing.T) {
	store, _, _ := newAutomaticCompactionStore(t, "")
	streamer := &scriptedStreamer{
		errs:   []error{nil, errOverflow},
		rounds: [][]responses.Event{tokenRound(0, 1, ""), nil, tokenRound(1, 1, ""), finalAnswerRound("recovered", 10)},
	}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var compactions []overflowCall
	var cuts []cutCall
	if _, err := runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		ToolCatalog: bigOutputCatalog(t), MaxToolRounds: 8, AutomaticCompaction: cutOverflowSettings(1, 1000, &compactions, &cuts),
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(streamer.requests) != 4 || len(compactions) != 1 || compactions[0] != (overflowCall{1, false}) || len(cuts) != 1 || cuts[0] != (cutCall{1, 1000, 1}) {
		t.Fatalf("requests = %d, compactions = %+v, cuts = %+v", len(streamer.requests), compactions, cuts)
	}
	full := strings.Repeat("x", 2992)
	if !strings.Contains(streamer.requests[1], full) {
		t.Fatal("the first request was already cut")
	}
	for index, request := range streamer.requests[2:] {
		if strings.Contains(request, full) || !strings.Contains(request, "START") || !strings.Contains(request, "END") || !strings.Contains(request, "run the tool again with a narrower request") || strings.Contains(request, "transcript keeps") {
			t.Fatalf("request %d was not cut: %.400s", index+2, request)
		}
	}
	if strings.Count(streamer.requests[3], "were left out of the request") != 2 {
		t.Fatalf("the later round did not cut both outputs: %.600s", streamer.requests[3])
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	whole := 0
	for _, item := range history {
		if strings.Contains(string(item), full) {
			whole++
		}
		if strings.Contains(string(item), "left out of the request") {
			t.Fatalf("the cut reached the transcript: %.300s", item)
		}
	}
	if whole != 2 {
		t.Fatalf("whole outputs in the transcript = %d, want 2", whole)
	}
}

// A second overflow halves the cap.
func TestOverflowCutHalvesOnTheNextAttempt(t *testing.T) {
	store, _, _ := newAutomaticCompactionStore(t, "")
	streamer := &scriptedStreamer{
		errs:   []error{nil, errOverflow, errOverflow},
		rounds: [][]responses.Event{tokenRound(0, 1, ""), nil, nil, finalAnswerRound("recovered", 10)},
	}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var compactions []overflowCall
	var cuts []cutCall
	if _, err := runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		ToolCatalog: bigOutputCatalog(t), MaxToolRounds: 8, AutomaticCompaction: cutOverflowSettings(2, 1000, &compactions, &cuts),
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(cuts) != 2 || cuts[0] != (cutCall{1, 1000, 1}) || cuts[1] != (cutCall{2, 500, 1}) || len(streamer.requests) != 4 {
		t.Fatalf("cuts = %+v, requests = %d", cuts, len(streamer.requests))
	}
	if len(streamer.requests[3]) >= len(streamer.requests[2]) {
		t.Fatalf("the second cut is not smaller: %d, then %d bytes", len(streamer.requests[2]), len(streamer.requests[3]))
	}
}

// Outputs already shorter than the cap leave the request as it was, so it is not sent again:
// the provider's error stands. With the cut off it stands too.
func TestOverflowCutThatCutsNothingKeepsTheProviderError(t *testing.T) {
	for name, maxBytes := range map[string]int{"nothing to cut": 4096, "cut off": 0} {
		store, _, _ := newAutomaticCompactionStore(t, "")
		streamer := &scriptedStreamer{errs: []error{nil, errOverflow}, rounds: [][]responses.Event{tokenRound(0, 1, "")}}
		runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
		if err != nil {
			t.Fatal(err)
		}
		var compactions []overflowCall
		var cuts []cutCall
		_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
			ToolCatalog: bigOutputCatalog(t), MaxToolRounds: 8, AutomaticCompaction: cutOverflowSettings(1, maxBytes, &compactions, &cuts),
		})
		if err == nil || !strings.Contains(err.Error(), errOverflow.Error()) || len(streamer.requests) != 2 {
			t.Fatalf("%s: err = %v, requests = %d", name, err, len(streamer.requests))
		}
		if maxBytes == 0 && len(cuts) != 0 || maxBytes != 0 && (len(cuts) != 1 || cuts[0].cut != 0) {
			t.Fatalf("%s: cuts = %+v", name, cuts)
		}
	}
}

func TestCutToolOutputsLeavesTheHistoryItemsAlone(t *testing.T) {
	long := strings.Repeat("é", 600) // 1200 bytes
	stringOutput := []byte(`{"type":"function_call_output","call_id":"c1","output":"` + long + `"}`)
	listOutput := []byte(`{"type":"custom_tool_call_output","call_id":"c2","output":[{"type":"input_text","text":"` + long + `"},{"type":"input_image","image_url":"data:x"}]}`)
	message := []byte(`{"type":"message","role":"user","content":"` + long + `"}`)
	history := [][]byte{stringOutput, listOutput, message}
	originals := make([]string, len(history))
	for index, item := range history {
		originals[index] = string(item)
	}
	sent, cut := cutToolOutputs(history, 300)
	if cut != 2 || string(sent[2]) != string(message) {
		t.Fatalf("cut = %d, message = %s", cut, sent[2])
	}
	for index, item := range history {
		if string(item) != originals[index] {
			t.Fatalf("history item %d was written", index)
		}
	}
	var first struct{ Output string }
	if err := json.Unmarshal(sent[0], &first); err != nil || !utf8.ValidString(first.Output) || len(first.Output) > 300+300 || !strings.HasPrefix(first.Output, "éé") || !strings.HasSuffix(first.Output, "éé") {
		t.Fatalf("string output = %q, %v", first.Output, err)
	}
	var second struct {
		Output []map[string]string `json:"output"`
	}
	if err := json.Unmarshal(sent[1], &second); err != nil || len(second.Output) != 2 || !strings.Contains(second.Output[0]["text"], "left out of the request") || second.Output[1]["image_url"] != "data:x" {
		t.Fatalf("list output = %s, %v", sent[1], err)
	}
	if same, none := cutToolOutputs(history, 0); none != 0 || &same[0] != &history[0] {
		t.Fatal("a zero cap changed the history")
	}
}
