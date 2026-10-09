package webui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const searchResponse = `{"id":"resp-1","output":[
 {"type":"reasoning","summary":[{"text":"Plan."}]},
 {"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"go release"}},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"Go 1.27.","annotations":[
   {"type":"url_citation","url":"https://go.dev/dl","title":"Downloads","start_index":0,"end_index":3},
   {"type":"url_citation","url":"https://go.dev/dl","title":"Downloads again"},
   {"type":"file_citation","file_id":"f1"},
   {"type":"container_file_citation","url":"https://not-a-web-source.test/file","title":"file"},
   {"type":"url_citation","url":"","title":"no url"},
   {"type":"url_citation","url":"https://go.dev/doc"}]}]}]}`

func TestNormalizeTranscriptKeepsWebSearchAndCitations(t *testing.T) {
	messages, err := NormalizeTranscript([]TranscriptRecord{
		{Offset: 0, Raw: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}`)},
		{Offset: 50, Raw: json.RawMessage(`{"type":"response.completed","response":` + searchResponse + `}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
	parts := messages[1].Parts
	if len(parts) != 3 || parts[0].Type != "reasoning" || parts[1].Type != "hosted_tool" || parts[2].Type != "text" {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[1].CallID != "ws_1" || parts[1].Target != "go release" || parts[1].Name != "web_search" || parts[1].Status != "completed" {
		t.Fatalf("web_search part = %+v", parts[1])
	}
	want := []Citation{{URL: "https://go.dev/dl", Title: "Downloads"}, {URL: "https://go.dev/doc"}}
	if !reflect.DeepEqual(parts[2].Citations, want) {
		t.Fatalf("citations = %+v, want %+v", parts[2].Citations, want)
	}
	if messages[1].Text != "Go 1.27." {
		t.Fatalf("message text = %q", messages[1].Text)
	}
}

func TestWebSearchPartFallsBackFromQueryToQueriesURLAndPattern(t *testing.T) {
	cases := map[string]string{
		`{"type":"web_search_call","id":"a","status":"completed","action":{"type":"search","query":"q1"}}`:                  "q1",
		`{"type":"web_search_call","id":"a","action":{"type":"search","queries":["q1","q2"]}}`:                              "q1; q2",
		`{"type":"web_search_call","id":"a","action":{"type":"open_page","url":"https://x.test"}}`:                          "https://x.test",
		`{"type":"web_search_call","id":"a","action":{"type":"find_in_page","url":"https://x.test","pattern":"needle"}}`:    "https://x.test",
		`{"type":"web_search_call","id":"a","action":{"type":"find_in_page","pattern":"needle"}}`:                           "needle",
		`{"type":"web_search_call","id":"a","status":"in_progress"}`:                                                        "",
		`{"type":"web_search_call","id":"a","action":"not an object"}`:                                                      "",
		`{"type":"web_search_call","id":"a","action":{"type":"search","query":"q1","queries":["ignored"],"url":"ignored"}}`: "q1",
	}
	for input, want := range cases {
		var item map[string]json.RawMessage
		if err := json.Unmarshal([]byte(input), &item); err != nil {
			t.Fatal(err)
		}
		part, ok := hostedToolPart(item, 0)
		if !ok || part.Type != "hosted_tool" || part.Target != want {
			t.Fatalf("%s -> %+v ok=%v, want query %q", input, part, ok, want)
		}
	}
	if _, ok := hostedToolPart(map[string]json.RawMessage{"type": json.RawMessage(`"web_search_call"`), "status": json.RawMessage(`"completed"`)}, 0); ok {
		t.Fatal("an item without id produced a part")
	}
}

func TestStreamingRunShowsSearchWhileItRunsThenKeepsOnePart(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"resp-1"}}`))

	message := updateStreamingRun(t, run, `{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}`)
	if len(message.Parts) != 1 || message.Parts[0].Type != "hosted_tool" || message.Parts[0].Status != "in_progress" || message.Parts[0].Target != "" {
		t.Fatalf("announced search = %+v", message.Parts)
	}
	if len(message.Steps) != 0 {
		t.Fatalf("steps = %+v, want none for a single step", message.Steps)
	}

	message = updateStreamingRun(t, run, `{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"go release"}}}`)
	if len(message.Parts) != 1 || message.Parts[0].Status != "completed" || message.Parts[0].Target != "go release" {
		t.Fatalf("finished search = %+v", message.Parts)
	}

	// A later event with less detail must not erase what is known.
	message = updateStreamingRun(t, run, `{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1"}}`)
	if message.Parts[0].Status != "completed" || message.Parts[0].Target != "go release" {
		t.Fatalf("sparse update erased fields: %+v", message.Parts[0])
	}

	updateStreamingRun(t, run, `{"type":"response.output_text.delta","delta":"Go 1.27."}`)
	final := run.Complete(json.RawMessage(`{"type":"response.completed","response":` + searchResponse + `}`))
	if len(final.Parts) != 3 {
		t.Fatalf("final parts = %+v, want reasoning + one web_search + text", final.Parts)
	}
	// The reasoning item was never streamed, so the existing merge appends it
	// after the streamed parts; only the counts per type are asserted here.
	counts := map[string]int{}
	var text Part
	for _, part := range final.Parts {
		counts[part.Type]++
		switch part.Type {
		case "hosted_tool":
			if part.CallID != "ws_1" || part.Target != "go release" {
				t.Fatalf("final search = %+v", part)
			}
		case "text":
			text = part
		}
	}
	if !reflect.DeepEqual(counts, map[string]int{"hosted_tool": 1, "reasoning": 1, "text": 1}) {
		t.Fatalf("part counts = %v", counts)
	}
	if text.Text != "Go 1.27." || len(text.Citations) != 2 {
		t.Fatalf("final text part = %+v", text)
	}
	for _, step := range final.Steps {
		if step.HasTools {
			t.Fatalf("a hosted search marked a step as tool step: %+v", final.Steps)
		}
	}
}

func TestStreamingRunIgnoresSearchItemWithoutID(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	if _, changed := run.Update(json.RawMessage(`{"type":"response.output_item.added","item":{"type":"web_search_call","status":"in_progress"}}`)); changed {
		t.Fatal("an item without id changed the message")
	}
}

func TestIncrementsSendsChangedCitationsAsPartNotDelta(t *testing.T) {
	previous := Message{ID: "m", Parts: []Part{{Type: "text", Text: "Go"}}}
	next := Message{ID: "m", Parts: []Part{{Type: "text", Text: "Go 1.27", Citations: []Citation{{URL: "https://go.dev"}}}}}
	events := Increments(previous, next)
	if len(events) != 1 || events[0].Name != "ui.part" {
		t.Fatalf("events = %+v, want one ui.part", events)
	}
	plain := Increments(Message{ID: "m", Parts: []Part{{Type: "text", Text: "Go"}}}, Message{ID: "m", Parts: []Part{{Type: "text", Text: "Go 1"}}})
	if len(plain) != 1 || plain[0].Name != "ui.delta" {
		t.Fatalf("plain text events = %+v, want one ui.delta", plain)
	}
}

func TestPartJSONOmitsSearchFieldsForOtherParts(t *testing.T) {
	raw, err := json.Marshal(Part{Type: "text", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"type":"text","text":"hi"}` {
		t.Fatalf("text part JSON = %s", raw)
	}
	raw, _ = json.Marshal(Part{Type: "hosted_tool", CallID: "ws_1", Name: "web_search", Target: "q", Status: "completed"})
	if string(raw) != `{"type":"hosted_tool","callId":"ws_1","name":"web_search","status":"completed","target":"q"}` {
		t.Fatalf("web_search part JSON = %s", raw)
	}
}

func TestCompleteAddsCitedTextWhenNoTextWasStreamed(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	final := run.Complete(json.RawMessage(`{"type":"response.completed","response":` + searchResponse + `}`))
	var text *Part
	for index := range final.Parts {
		if final.Parts[index].Type == "text" {
			text = &final.Parts[index]
		}
	}
	if text == nil || text.Text != "Go 1.27." || len(text.Citations) != 2 {
		t.Fatalf("text part = %+v, parts %+v", text, final.Parts)
	}
}

func TestSearchDoesNotMarkItsStepAsAToolStep(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"resp-1"}}`))
	updateStreamingRun(t, run, `{"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}`)
	updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"q"}}}`)
	run.Update(json.RawMessage(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"q"}}]}}`))
	run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"resp-2"}}`))
	updateStreamingRun(t, run, `{"type":"response.output_text.delta","delta":"Done."}`)
	final := run.Complete(json.RawMessage(`{"type":"response.completed","response":{"id":"resp-2","output":[{"type":"message","content":[{"type":"output_text","text":"Done."}]}]}}`))
	if len(final.Steps) != 2 {
		t.Fatalf("steps = %+v, want 2", final.Steps)
	}
	for _, step := range final.Steps {
		if step.HasTools {
			t.Fatalf("a hosted search marked step %q as a tool step: %+v", step.ID, final.Steps)
		}
	}
	if final.Parts[0].Type != "hosted_tool" || final.Parts[0].Status != "completed" || len(final.Parts) != 2 {
		t.Fatalf("parts = %+v", final.Parts)
	}
}

const interleavedResponse = `{"id":"resp-1","output":[
 {"type":"reasoning","summary":[{"text":"R1"}]},
 {"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"first"}},
 {"type":"reasoning","summary":[{"text":"R2"}]},
 {"type":"web_search_call","id":"ws_2","status":"completed","action":{"query":"second"}},
 {"type":"message","content":[{"type":"output_text","text":"A","annotations":[{"type":"url_citation","url":"https://a.test"}]}]},
 {"type":"web_search_call","id":"ws_3","status":"completed","action":{"query":"third"}},
 {"type":"message","content":[{"type":"output_text","text":"B","annotations":[{"type":"url_citation","url":"https://b.test"}]}]}]}`

func partSummary(parts []Part) []string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "hosted_tool":
			out = append(out, "search:"+part.CallID+":"+part.Target)
		case "text", "reasoning":
			summary := part.Type + ":" + part.Text
			for _, citation := range part.Citations {
				summary += "|" + citation.URL
			}
			out = append(out, summary)
		default:
			out = append(out, part.Type)
		}
	}
	return out
}

var interleavedWant = []string{
	"reasoning:R1", "search:ws_1:first", "reasoning:R2", "search:ws_2:second",
	"text:A|https://a.test", "search:ws_3:third", "text:B|https://b.test",
}

func TestSnapshotKeepsInterleavedSearchTextAndReasoningInOrder(t *testing.T) {
	messages, err := NormalizeTranscript([]TranscriptRecord{
		{Offset: 0, Raw: json.RawMessage(`{"type":"response.completed","response":` + interleavedResponse + `}`)},
	})
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages = %+v, err %v", messages, err)
	}
	if got := partSummary(messages[0].Parts); !reflect.DeepEqual(got, interleavedWant) {
		t.Fatalf("snapshot parts = %v\nwant %v", got, interleavedWant)
	}
}

func TestLiveRunMatchesSnapshotForInterleavedSearch(t *testing.T) {
	// Streamed: every item arrives as it happens, then response.completed repeats them.
	run := NewStreamingRun("turn-1", "10:10 AM")
	run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"resp-1"}}`))
	updateStreamingRun(t, run, `{"type":"response.reasoning_summary_text.delta","delta":"R1"}`)
	updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"first"}}}`)
	updateStreamingRun(t, run, `{"type":"response.reasoning_summary_text.delta","delta":"R2"}`)
	updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_2","status":"completed","action":{"query":"second"}}}`)
	updateStreamingRun(t, run, `{"type":"response.output_text.delta","delta":"A"}`)
	updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_3","status":"completed","action":{"query":"third"}}}`)
	updateStreamingRun(t, run, `{"type":"response.output_text.delta","delta":"B"}`)
	final := run.Complete(json.RawMessage(`{"type":"response.completed","response":` + interleavedResponse + `}`))
	if got := partSummary(final.Parts); !reflect.DeepEqual(got, interleavedWant) {
		t.Fatalf("streamed parts = %v\nwant %v", got, interleavedWant)
	}
}

func TestLiveRunAddsSearchesThatWereNotStreamed(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	final := run.Complete(json.RawMessage(`{"type":"response.completed","response":` + searchResponse + `}`))
	got := partSummary(final.Parts)
	if len(got) != 3 || got[0] != "reasoning:Plan." || got[1] != "search:ws_1:go release" || !strings.HasPrefix(got[2], "text:Go 1.27.|https://go.dev/dl") {
		t.Fatalf("parts = %v", got)
	}
}

func TestTwoSearchesKeepTheirOwnIDsAndAToolCallIDDoesNotCollide(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"resp-1"}}`))
	updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"function_call","call_id":"ws_1","name":"read","arguments":"{}"}}`)
	updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"one"}}}`)
	message := updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_2","status":"completed","action":{"query":"two"}}}`)
	if got := partSummary(message.Parts); !reflect.DeepEqual(got, []string{"tool_call", "search:ws_1:one", "search:ws_2:two"}) {
		t.Fatalf("parts = %v", got)
	}
	if message.Parts[0].Target != "" || message.Parts[0].Status != "" {
		t.Fatalf("the tool call was overwritten by a search: %+v", message.Parts[0])
	}
}

func TestSeveralOutputTextItemsMergeCitationsWithoutDuplicates(t *testing.T) {
	response := `{"id":"r","output":[{"type":"message","content":[
	 {"type":"output_text","text":"one ","annotations":[{"type":"url_citation","url":"https://a.test"},{"type":"url_citation","url":"https://b.test"}]},
	 {"type":"output_text","text":"","annotations":[{"type":"url_citation","url":"https://only-sources.test"}]},
	 {"type":"output_text","text":"two","annotations":[{"type":"url_citation","url":"https://b.test"},{"type":"url_citation","url":"https://c.test"}]}]}]}`
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(response), &fields); err != nil {
		t.Fatal(err)
	}
	parts := responseParts(fields)
	if len(parts) != 1 || parts[0].Text != "one two" {
		t.Fatalf("parts = %+v", parts)
	}
	var urls []string
	for _, citation := range parts[0].Citations {
		urls = append(urls, citation.URL)
	}
	if !reflect.DeepEqual(urls, []string{"https://a.test", "https://b.test", "https://c.test"}) {
		t.Fatalf("citations = %v (empty text must not contribute sources, duplicates must collapse)", urls)
	}

	onlyAnnotations := `{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"","annotations":[{"type":"url_citation","url":"https://x.test"}]}]}]}`
	_ = json.Unmarshal([]byte(onlyAnnotations), &fields)
	if got := responseParts(fields); len(got) != 0 {
		t.Fatalf("empty text with annotations produced parts: %+v", got)
	}
}

func TestSnapshotWithSearchAndToolCallBuildsToolStepAndPlainSearchStep(t *testing.T) {
	messages, err := NormalizeTranscript([]TranscriptRecord{
		{Offset: 0, Raw: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}`)},
		{Offset: 10, Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"r1","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"q"}},{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}]}}`)},
		{Offset: 20, Raw: json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":"{\"status\":\"ok\"}\nbody"}`)},
		{Offset: 30, Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"r2","output":[{"type":"web_search_call","id":"ws_2","status":"completed","action":{"query":"q2"}},{"type":"message","content":[{"type":"output_text","text":"done","annotations":[{"type":"url_citation","url":"https://d.test"}]}]}]}}`)},
	})
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages = %d, err %v", len(messages), err)
	}
	assistant := messages[1]
	want := []string{"search:ws_1:q", "tool_call", "tool_result", "search:ws_2:q2", "text:done|https://d.test"}
	if got := partSummary(assistant.Parts); !reflect.DeepEqual(got, want) {
		t.Fatalf("parts = %v\nwant %v", got, want)
	}
	if len(assistant.Steps) != 2 || !assistant.Steps[0].HasTools || assistant.Steps[1].HasTools {
		t.Fatalf("steps = %+v, want a tool step then a plain step", assistant.Steps)
	}
}

func TestLiveRunWithNothingStreamedKeepsInterleavedOrder(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	final := run.Complete(json.RawMessage(`{"type":"response.completed","response":` + interleavedResponse + `}`))
	if got := partSummary(final.Parts); !reflect.DeepEqual(got, interleavedWant) {
		t.Fatalf("parts = %v\nwant %v", got, interleavedWant)
	}
}

func TestSecondResponseTextKeepsTheSourcesOfTheFirst(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	first := `{"type":"response.completed","response":{"id":"r1","output":[{"type":"message","content":[{"type":"output_text","text":"X","annotations":[{"type":"url_citation","url":"https://first.test"}]}]}]}}`
	second := `{"type":"response.completed","response":{"id":"r2","output":[{"type":"message","content":[{"type":"output_text","text":"Y","annotations":[{"type":"url_citation","url":"https://second.test"}]}]}]}}`
	run.Update(json.RawMessage(first))
	run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"r2"}}`))
	final := run.Complete(json.RawMessage(second))
	got := partSummary(final.Parts)
	if len(got) != 1 || got[0] != "text:XY|https://first.test|https://second.test" {
		t.Fatalf("parts = %v, want the text of both responses with the sources of both", got)
	}
}
