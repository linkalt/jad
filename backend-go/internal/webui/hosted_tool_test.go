package webui

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func rawItem(t *testing.T, text string) map[string]json.RawMessage {
	t.Helper()
	var item map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestHostedToolNameDropsOnlyATrailingCall(t *testing.T) {
	cases := map[string]string{
		"web_search_call":      "web_search",
		"openrouter:web_fetch": "openrouter:web_fetch",
		"file_search_call":     "file_search",
		"image_generation":     "image_generation",
		"call":                 "call",
		"_call":                "_call",
		"x_call_y":             "x_call_y",
	}
	for typeName, want := range cases {
		if got := hostedToolName(typeName); got != want {
			t.Errorf("hostedToolName(%q) = %q, want %q", typeName, got, want)
		}
	}
}

func TestAnyUnhandledItemWithAnIDIsAHostedTool(t *testing.T) {
	for _, text := range []string{
		`{"type":"web_search_call","id":"a"}`,
		`{"type":"openrouter:web_fetch","id":"a"}`,
		`{"type":"some_future_tool","id":"a"}`,
	} {
		if !isHostedToolItem(rawItem(t, text)) {
			t.Errorf("%s was not a hosted tool", text)
		}
	}
	for _, text := range []string{
		`{"type":"message","id":"a"}`,
		`{"type":"reasoning","id":"a"}`,
		`{"type":"function_call","id":"a"}`,
		`{"type":"function_call_output","id":"a"}`,
		`{"type":"web_search_call"}`,
		`{"type":"web_search_call","id":""}`,
		`{"id":"a"}`,
		`{}`,
	} {
		if isHostedToolItem(rawItem(t, text)) {
			t.Errorf("%s was taken as a hosted tool", text)
		}
	}
}

func TestHostedToolTargetUsesCommonFieldsInOrder(t *testing.T) {
	cases := map[string]string{
		`{"action":{"query":"q","url":"u"},"url":"top"}`:          "q",
		`{"action":{"queries":["a","b"],"url":"u"}}`:              "a; b",
		`{"action":{"url":"u","pattern":"p"},"url":"top"}`:        "u",
		`{"action":{"pattern":"p"},"url":"top"}`:                  "p",
		`{"url":"https://go.dev/dl/","title":"All releases"}`:     "https://go.dev/dl/",
		`{"query":"direct","title":"t"}`:                          "direct",
		`{"title":"only a title"}`:                                "only a title",
		`{"action":"not an object","url":"top"}`:                  "top",
		`{"status":"in_progress"}`:                                "",
		`{"action":{"query":""},"url":"","query":"","title":"x"}`: "x",
	}
	for body, want := range cases {
		item := rawItem(t, body)
		if got := hostedToolTarget(item); got != want {
			t.Errorf("target of %s = %q, want %q", body, got, want)
		}
	}
}

func TestHostedToolPartKeepsTheItemAsRawJSON(t *testing.T) {
	item := rawItem(t, `{"type":"openrouter:web_fetch","id":"st_1","status":"completed","url":"https://go.dev/dl/","title":"All releases","httpStatus":200,"content":"page"}`)
	part, ok := hostedToolPart(item, 0)
	if !ok {
		t.Fatal("not a hosted tool")
	}
	if part.Type != "hosted_tool" || part.CallID != "st_1" || part.Name != "openrouter:web_fetch" || part.Status != "completed" || part.Target != "https://go.dev/dl/" || part.Truncated != nil {
		t.Fatalf("part = %+v", part)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(part.Raw), &back); err != nil {
		t.Fatalf("raw is not JSON: %v: %s", err, part.Raw)
	}
	if back["content"] != "page" || back["httpStatus"].(float64) != 200 || back["type"] != "openrouter:web_fetch" {
		t.Fatalf("raw = %s", part.Raw)
	}
}

func bigFetchItem(t *testing.T, size int) map[string]json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"type": "openrouter:web_fetch", "id": "st_1", "status": "completed",
		"url": "https://go.dev/dl/", "title": "All releases", "httpStatus": 200,
		"content": strings.Repeat("é", size/2),
	})
	return rawItem(t, string(body))
}

func TestHostedToolRawIsCappedByShorteningTheLongestStringAndStaysJSON(t *testing.T) {
	item := bigFetchItem(t, 54000)
	full, _ := json.Marshal(item)
	part, _ := hostedToolPart(item, 2000)
	if len(part.Raw) > 2000 {
		t.Fatalf("raw is %d bytes, over the 2000 cap", len(part.Raw))
	}
	if part.Truncated == nil || part.Truncated.Bytes != len(full) {
		t.Fatalf("truncated = %+v, want the full size %d", part.Truncated, len(full))
	}
	if !utf8.ValidString(part.Raw) {
		t.Fatal("raw is not valid UTF-8 after the cut")
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(part.Raw), &back); err != nil {
		t.Fatalf("capped raw is not JSON: %v", err)
	}
	if back["url"] != "https://go.dev/dl/" || back["title"] != "All releases" || back["status"] != "completed" {
		t.Fatalf("short fields were damaged: %s", part.Raw[:200])
	}
	if content, _ := back["content"].(string); content == "" || len(content) >= 54000 {
		t.Fatalf("content = %d bytes, want shortened but not empty", len(content))
	}
}

func TestHostedToolRawBelowTheCapIsLeftAlone(t *testing.T) {
	item := bigFetchItem(t, 400)
	part, _ := hostedToolPart(item, 4000)
	if part.Truncated != nil {
		t.Fatalf("truncated = %+v for a small item", part.Truncated)
	}
	exact, _ := hostedToolPart(item, len(part.Raw))
	if exact.Truncated != nil {
		t.Fatalf("an item exactly at the cap was cut: %+v", exact.Truncated)
	}
	over, _ := hostedToolPart(item, len(part.Raw)-1)
	if over.Truncated == nil || len(over.Raw) > len(part.Raw)-1 {
		t.Fatalf("an item one byte over the cap was not cut: %d bytes, %+v", len(over.Raw), over.Truncated)
	}
}

func TestShrinkJSONHandlesNestedStringsArraysAndTinyLimits(t *testing.T) {
	raw := `{"a":{"b":["` + strings.Repeat("x", 500) + `","y"]},"c":"` + strings.Repeat("z", 100) + `"}`
	got := shrinkJSON(raw, 300)
	if len(got) > 300 {
		t.Fatalf("len = %d", len(got))
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("not JSON: %v: %s", err, got)
	}
	for _, tiny := range []int{0, 1, 2, 5} {
		got := shrinkJSON(raw, tiny)
		if len(got) > tiny {
			t.Fatalf("limit %d gave %d bytes", tiny, len(got))
		}
		if got != "" {
			var any1 any
			if err := json.Unmarshal([]byte(got), &any1); err != nil {
				t.Fatalf("limit %d gave invalid JSON %q", tiny, got)
			}
		}
	}
	if got := shrinkJSON("not json at all, a long text", 8); got != "not json" {
		t.Fatalf("invalid JSON fallback = %q", got)
	}
}

func TestCapMessagesShortensHostedRawAndReportsTheSize(t *testing.T) {
	item := bigFetchItem(t, 54000)
	part, _ := hostedToolPart(item, 0)
	full := len(part.Raw)
	messages := CapMessages([]Message{{ID: "m", Parts: []Part{part, {Type: "text", Text: "ok"}}}}, 4096, nil)
	capped := messages[0].Parts[0]
	if capped.Truncated == nil || capped.Truncated.Bytes != full || len(capped.Raw) > 4096 {
		t.Fatalf("capped = %d bytes, truncated %+v, full %d", len(capped.Raw), capped.Truncated, full)
	}
	if capped.Target != "https://go.dev/dl/" || capped.Name != "openrouter:web_fetch" || capped.CallID != "st_1" {
		t.Fatalf("metadata lost: %+v", capped)
	}
	uncapped := CapMessages([]Message{{ID: "m", Parts: []Part{part}}}, 0, nil)
	if uncapped[0].Parts[0].Truncated != nil || len(uncapped[0].Parts[0].Raw) != full {
		t.Fatal("a zero limit must send parts uncut")
	}
	again := CapMessages(messages, 4096, nil)
	if again[0].Parts[0].Truncated.Bytes != full {
		t.Fatalf("capping twice changed the reported size: %+v", again[0].Parts[0].Truncated)
	}
}

func fetchResponse(size int) string {
	body, _ := json.Marshal(map[string]any{"id": "resp-1", "output": []any{
		map[string]any{"type": "openrouter:web_fetch", "id": "st_1", "status": "completed", "url": "https://go.dev/dl/", "title": "All releases", "content": strings.Repeat("a", size)},
		map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "Go 1.26.5"}}},
	}})
	return string(body)
}

func TestStoredFetchBecomesAHostedToolPartThenText(t *testing.T) {
	messages, err := NormalizeTranscript([]TranscriptRecord{
		{Offset: 0, Raw: json.RawMessage(`{"type":"response.completed","response":` + fetchResponse(3000) + `}`)},
	})
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages = %+v, err %v", messages, err)
	}
	parts := messages[0].Parts
	if len(parts) != 2 || parts[0].Type != "hosted_tool" || parts[0].Name != "openrouter:web_fetch" || parts[1].Type != "text" || parts[1].Text != "Go 1.26.5" {
		t.Fatalf("parts = %+v", parts)
	}
	for _, step := range messages[0].Steps {
		if step.HasTools {
			t.Fatalf("a hosted fetch marked a step as a tool step: %+v", messages[0].Steps)
		}
	}
}

func TestLiveFetchIsAnnouncedThenFilledAndCappedAndNeverDuplicated(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	run.SetHostedRawLimit(1500)
	run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"resp-1"}}`))

	message := updateStreamingRun(t, run, `{"type":"response.output_item.added","item":{"type":"openrouter:web_fetch","id":"st_1","status":"in_progress"}}`)
	if len(message.Parts) != 1 || message.Parts[0].Type != "hosted_tool" || message.Parts[0].Status != "in_progress" || message.Parts[0].Target != "" {
		t.Fatalf("announced = %+v", message.Parts)
	}

	done, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "item": map[string]any{
		"type": "openrouter:web_fetch", "id": "st_1", "status": "completed", "url": "https://go.dev/dl/", "content": strings.Repeat("b", 50000)}})
	message = updateStreamingRun(t, run, string(done))
	if len(message.Parts) != 1 {
		t.Fatalf("parts = %+v, want one", message.Parts)
	}
	part := message.Parts[0]
	if part.Status != "completed" || part.Target != "https://go.dev/dl/" || len(part.Raw) > 1500 || part.Truncated == nil || part.Truncated.Bytes < 50000 {
		t.Fatalf("filled part: status %q target %q raw %d truncated %+v", part.Status, part.Target, len(part.Raw), part.Truncated)
	}

	final := run.Complete(json.RawMessage(`{"type":"response.completed","response":` + fetchResponse(50000) + `}`))
	if got := partSummary(final.Parts); len(got) != 2 {
		t.Fatalf("final parts = %v, want the fetch once plus the text", got)
	}
	if final.Parts[0].Truncated == nil || len(final.Parts[0].Raw) > 1500 {
		t.Fatalf("the completed response uncapped the part: raw %d", len(final.Parts[0].Raw))
	}
}

func TestLiveSparseUpdateDoesNotEraseAHostedToolsDetails(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"q"}}}`)
	message := updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1"}}`)
	part := message.Parts[0]
	if part.Status != "completed" || part.Target != "q" || !strings.Contains(part.Raw, `"query":"q"`) {
		t.Fatalf("sparse update erased fields: %+v", part)
	}
}

func TestSetHostedRawLimitIgnoresNegativeAndNilRuns(t *testing.T) {
	var none *StreamingRun
	none.SetHostedRawLimit(10)
	run := NewStreamingRun("t", "")
	run.SetHostedRawLimit(100)
	run.SetHostedRawLimit(-1)
	if run.hostedRawLimit != 100 {
		t.Fatalf("limit = %d, want 100", run.hostedRawLimit)
	}
}

func TestIncrementsSendsAChangedHostedToolAsAPart(t *testing.T) {
	previous := Message{ID: "m", Parts: []Part{{Type: "hosted_tool", CallID: "a", Name: "web_search", Status: "in_progress"}}}
	next := Message{ID: "m", Parts: []Part{{Type: "hosted_tool", CallID: "a", Name: "web_search", Status: "completed", Target: "q", Raw: `{"id":"a"}`}}}
	events := Increments(previous, next)
	if len(events) != 1 || events[0].Name != "ui.part" {
		t.Fatalf("events = %+v, want one ui.part", events)
	}
	if Increments(next, next) != nil {
		t.Fatal("an unchanged hosted tool produced events")
	}
}

func TestHostedToolPartJSONHasItsFieldsAndNoSearchLeftovers(t *testing.T) {
	raw, _ := json.Marshal(Part{Type: "hosted_tool", CallID: "a", Name: "web_search", Status: "completed", Target: "q", Raw: "{}"})
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "hosted_tool", "callId": "a", "name": "web_search", "status": "completed", "target": "q", "raw": "{}"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("JSON = %s", raw)
	}
}

func TestShrinkJSONFitsAnItemWithManyMediumStrings(t *testing.T) {
	var sources []string
	for i := 0; i < 120; i++ {
		sources = append(sources, strings.Repeat("s", 400))
	}
	body, _ := json.Marshal(map[string]any{"type": "x_call", "id": "a", "sources": sources})
	got := shrinkJSON(string(body), 4096)
	if len(got) > 4096 {
		t.Fatalf("len = %d, over the limit", len(got))
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("capped raw is not JSON: %v: %.120s", err, got)
	}
	if back["id"] != "a" || back["type"] != "x_call" {
		t.Fatalf("short fields were damaged: %.200s", got)
	}
}

func TestHostedToolNameFallsBackToTheTypeWhenEmpty(t *testing.T) {
	part, ok := hostedToolPart(rawItem(t, `{"type":"_call","id":"a"}`), 0)
	if !ok || part.Name != "_call" {
		t.Fatalf("part = %+v ok=%v, want the full type as the name", part, ok)
	}
}

func TestHTMLInsideAnItemIsNotMarkedTruncatedWhenNothingWasCut(t *testing.T) {
	page := strings.Repeat("<a>&</a>", 200)
	item := rawItem(t, `{"type":"openrouter:web_fetch","id":"a","content":`+strconvQuote(page)+`}`)
	plain, _ := hostedToolPart(item, 0)
	// A limit between the unescaped and the HTML-escaped size.
	var back map[string]any
	_ = json.Unmarshal([]byte(plain.Raw), &back)
	unescaped, _ := encodeCompact(back)
	if len(unescaped) >= len(plain.Raw) {
		t.Fatalf("test setup: unescaped %d is not smaller than escaped %d", len(unescaped), len(plain.Raw))
	}
	part, _ := hostedToolPart(item, len(unescaped))
	if part.Truncated != nil && part.Raw == plain.Raw {
		t.Fatalf("marked truncated although nothing was cut: %+v", part.Truncated)
	}
	if len(part.Raw) > len(unescaped) {
		t.Fatalf("raw is %d bytes, over the %d limit", len(part.Raw), len(unescaped))
	}
}

func TestCappingTheSameItemTwiceGivesTheSameBytes(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"type": "t", "id": "a", "a": strings.Repeat("x", 3000), "b": strings.Repeat("y", 3000), "c": strings.Repeat("z", 3000)})
	first := shrinkJSON(string(body), 4000)
	for i := 0; i < 40; i++ {
		if again := shrinkJSON(string(body), 4000); again != first {
			t.Fatalf("run %d gave different bytes for equal-length strings", i)
		}
	}
}

func strconvQuote(text string) string {
	body, _ := json.Marshal(text)
	return string(body)
}

func TestShrinkJSONRandomDocumentsFitStayValidAndAreDeterministic(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	letters := []string{"a", "é", "漢", "😀", "<", "&", "\n", "\""}
	text := func(n int) string {
		var builder strings.Builder
		for builder.Len() < n {
			builder.WriteString(letters[random.Intn(len(letters))])
		}
		return builder.String()
	}
	var build func(depth int) any
	build = func(depth int) any {
		switch kind := random.Intn(6); {
		case kind == 0 && depth < 3:
			object := map[string]any{}
			for i, count := 0, random.Intn(5); i < count; i++ {
				object[text(1+random.Intn(6))] = build(depth + 1)
			}
			return object
		case kind == 1 && depth < 3:
			list := make([]any, random.Intn(40))
			for i := range list {
				list[i] = build(depth + 1)
			}
			return list
		case kind == 2:
			return random.Intn(1000000)
		case kind == 3:
			return nil
		default:
			return text(random.Intn(1200))
		}
	}
	for round := 0; round < 400; round++ {
		document := map[string]any{"type": "t", "id": "a", "body": build(0), "more": build(0)}
		body, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		limit := []int{2, 40, 300, 2048, 16384}[random.Intn(5)]
		got := shrinkJSON(string(body), limit)
		if len(got) > limit {
			t.Fatalf("round %d: %d bytes over limit %d", round, len(got), limit)
		}
		if got != "" {
			var back any
			if err := json.Unmarshal([]byte(got), &back); err != nil {
				t.Fatalf("round %d: invalid JSON at limit %d: %v: %.80s", round, limit, err, got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("round %d: invalid UTF-8", round)
			}
		}
		if again := shrinkJSON(string(body), limit); again != got {
			t.Fatalf("round %d: not deterministic", round)
		}
		if len(body) <= limit {
			if compact, _ := encodeCompact(mustDecode(t, string(body))); got != compact {
				t.Fatalf("round %d: an item within the limit was changed", round)
			}
		}
	}
}

func mustDecode(t *testing.T, text string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestLiveHostedToolRawFollowsTheLatestReportedState(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	big := strings.Repeat("p", 500)
	updateStreamingRun(t, run, `{"type":"response.output_item.added","item":{"type":"x_call","id":"a","status":"in_progress","note":"`+big+`"}}`)
	message := updateStreamingRun(t, run, `{"type":"response.output_item.done","item":{"type":"x_call","id":"a","status":"completed"}}`)
	part := message.Parts[0]
	if part.Status != "completed" || !strings.Contains(part.Raw, `"status":"completed"`) || strings.Contains(part.Raw, "in_progress") {
		t.Fatalf("raw disagrees with status: status %q raw %.120s", part.Status, part.Raw)
	}
}
