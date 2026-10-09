package imageresize

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func sourceByName(names map[string]string) SourceOf {
	return func(call json.RawMessage) string {
		var fields struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(call, &fields)
		return names[fields.Name]
	}
}

func functionCall(name, callID string) []byte {
	return mustJSON(map[string]any{"type": "function_call", "name": name, "call_id": callID, "arguments": "{}"})
}

func toolOutput(callID string, parts ...any) []byte {
	return mustJSON(map[string]any{"type": "function_call_output", "call_id": callID, "output": parts})
}

func imagePart(url string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": url, "detail": "auto"}
}

func textPart(text string) map[string]any {
	return map[string]any{"type": "input_text", "text": text}
}

func userMessage(parts ...any) []byte {
	return mustJSON(map[string]any{"type": "message", "role": "user", "content": parts})
}

func outputParts(t *testing.T, item []byte) []map[string]any {
	t.Helper()
	var fields struct {
		Output  []map[string]any `json:"output"`
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(item, &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Output != nil {
		return fields.Output
	}
	return fields.Content
}

func TestApplyResizesOnlyTheConfiguredSources(t *testing.T) {
	big := pngURL(t, noisy(300, 300, 255))
	history := [][]byte{
		functionCall("mcp_shot", "c1"), toolOutput("c1", textPart("shot"), imagePart(big)),
		functionCall("read", "c2"), toolOutput("c2", imagePart(big)),
		userMessage(textPart("look"), imagePart(big)),
	}
	sources := sourceByName(map[string]string{"mcp_shot": SourceMCP, "read": SourceNative})
	for _, test := range []struct {
		name      string
		appliesTo []string
		changed   []int
	}{
		{"mcp only", []string{"mcp"}, []int{1}},
		{"native only", []string{"native"}, []int{3}},
		{"user only", []string{"user"}, []int{4}},
		{"all three", []string{"mcp", "native", "user"}, []int{1, 3, 4}},
		{"none", nil, nil},
	} {
		sent, stats := New(testOptions(), test.appliesTo).Apply(history, sources)
		var changed []int
		for index := range history {
			if !bytes.Equal(sent[index], history[index]) {
				changed = append(changed, index)
			}
		}
		if len(changed) != len(test.changed) || stats.Resized != len(test.changed) {
			t.Errorf("%s: changed %v (stats %+v), want %v", test.name, changed, stats, test.changed)
			continue
		}
		for position, index := range changed {
			if index != test.changed[position] {
				t.Errorf("%s: changed %v, want %v", test.name, changed, test.changed)
			}
		}
	}
}

func TestApplyNeverChangesTheStoredHistory(t *testing.T) {
	big := pngURL(t, noisy(300, 300, 255))
	history := [][]byte{functionCall("mcp_shot", "c1"), toolOutput("c1", imagePart(big))}
	before := [][]byte{append([]byte(nil), history[0]...), append([]byte(nil), history[1]...)}
	sent, stats := New(testOptions(), []string{"mcp"}).Apply(history, sourceByName(map[string]string{"mcp_shot": SourceMCP}))
	for index := range history {
		if !bytes.Equal(history[index], before[index]) {
			t.Fatalf("item %d of the input history was modified in place", index)
		}
	}
	if stats.Resized != 1 || len(sent[1]) >= len(history[1]) {
		t.Fatalf("stats %+v, sent %d bytes vs stored %d", stats, len(sent[1]), len(history[1]))
	}
	if &sent[0] == &history[0] {
		t.Fatalf("the result must be a new slice when something changed")
	}
}

func TestApplyReturnsTheSameSliceWhenNothingChanges(t *testing.T) {
	small := pngURL(t, noisy(20, 20, 255))
	history := [][]byte{functionCall("mcp_shot", "c1"), toolOutput("c1", imagePart(small)), userMessage(textPart("hi"))}
	sent, stats := New(testOptions(), []string{"mcp", "user"}).Apply(history, sourceByName(map[string]string{"mcp_shot": SourceMCP}))
	if &sent[0] != &history[0] || stats.Resized != 0 || stats.Images != 1 {
		t.Fatalf("sent should be the input slice; stats = %+v", stats)
	}
	var disabled *Resizer
	if again, _ := disabled.Apply(history, nil); &again[0] != &history[0] {
		t.Fatalf("a nil resizer must return the history itself")
	}
}

func TestApplyKeepsEveryOtherFieldAndPartOfAnItem(t *testing.T) {
	big := pngURL(t, noisy(300, 300, 255))
	item := mustJSON(map[string]any{
		"type": "function_call_output", "call_id": "c1", "id": "fco_1", "status": "completed",
		"output": []any{textPart("before"), imagePart(big), map[string]any{"type": "input_file", "filename": "a.bin", "file_data": "QUJD"}},
	})
	sent, _ := New(testOptions(), []string{"mcp"}).Apply([][]byte{functionCall("mcp_shot", "c1"), item}, sourceByName(map[string]string{"mcp_shot": SourceMCP}))
	var fields map[string]any
	if err := json.Unmarshal(sent[1], &fields); err != nil {
		t.Fatal(err)
	}
	if fields["id"] != "fco_1" || fields["status"] != "completed" || fields["call_id"] != "c1" {
		t.Fatalf("fields lost: %v", fields)
	}
	parts := outputParts(t, sent[1])
	if len(parts) != 3 || parts[0]["text"] != "before" || parts[1]["detail"] != "auto" || parts[2]["file_data"] != "QUJD" {
		t.Fatalf("parts = %v", parts)
	}
	if url, _ := parts[1]["image_url"].(string); !strings.HasPrefix(url, "data:image/") || url == big {
		t.Fatalf("the image was not replaced: %.40s", url)
	}
}

func TestApplyLeavesAStringOutputAndForeignItemsAlone(t *testing.T) {
	big := pngURL(t, noisy(300, 300, 255))
	history := [][]byte{
		functionCall("mcp_shot", "c1"),
		mustJSON(map[string]any{"type": "function_call_output", "call_id": "c1", "output": "text mentioning input_image"}),
		mustJSON(map[string]any{"type": "message", "role": "assistant", "content": []any{imagePart(big)}}),
		mustJSON(map[string]any{"type": "function_call_output", "call_id": "missing", "output": []any{imagePart(big)}}),
		[]byte(`{"type":"reasoning","summary":[]}`),
	}
	sent, stats := New(testOptions(), []string{"mcp", "native", "user"}).Apply(history, sourceByName(map[string]string{"mcp_shot": SourceMCP}))
	for index := range history {
		if !bytes.Equal(sent[index], history[index]) {
			t.Errorf("item %d changed", index)
		}
	}
	if stats.Resized != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestApplyResizesEachImageOfOneItemAndCountsBytes(t *testing.T) {
	first, second := pngURL(t, noisy(300, 300, 255)), pngURL(t, noisy(250, 250, 255))
	history := [][]byte{functionCall("mcp_shot", "c1"), toolOutput("c1", imagePart(first), imagePart(second))}
	sent, stats := New(testOptions(), []string{"mcp"}).Apply(history, sourceByName(map[string]string{"mcp_shot": SourceMCP}))
	if stats.Images != 2 || stats.Resized != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if want := len(first) + len(second); stats.BytesBefore != want {
		t.Fatalf("BytesBefore = %d, want %d", stats.BytesBefore, want)
	}
	var after int
	for _, part := range outputParts(t, sent[1]) {
		after += len(part["image_url"].(string))
	}
	if stats.BytesAfter != after {
		t.Fatalf("BytesAfter = %d, want %d", stats.BytesAfter, after)
	}
}

func TestResultsAreMemoisedSoEveryRoundSendsTheSameBytes(t *testing.T) {
	big := pngURL(t, noisy(300, 300, 255))
	history := [][]byte{functionCall("mcp_shot", "c1"), toolOutput("c1", imagePart(big)), functionCall("mcp_shot", "c2"), toolOutput("c2", imagePart(big))}
	resizer := New(testOptions(), []string{"mcp"})
	sources := sourceByName(map[string]string{"mcp_shot": SourceMCP})
	first, _ := resizer.Apply(history, sources)
	second, _ := resizer.Apply(history, sources)
	for index := range first {
		if !bytes.Equal(first[index], second[index]) {
			t.Fatalf("item %d differs between two rounds", index)
		}
	}
	if len(resizer.cache) != 1 {
		t.Fatalf("cache entries = %d, want 1 (the same image twice)", len(resizer.cache))
	}
}

func TestMemoIsUsedInsteadOfResizingAgain(t *testing.T) {
	// A planted entry is returned as it is: the image is not decoded a second time.
	big := pngURL(t, noisy(300, 300, 255))
	resizer := New(testOptions(), []string{"user"})
	key := sha256.Sum256([]byte(big))
	resizer.cache[key] = "data:image/png;base64,PLANTED"
	sent, stats := resizer.Apply([][]byte{userMessage(imagePart(big))}, nil)
	if stats.Resized != 1 || !strings.Contains(string(sent[0]), "PLANTED") {
		t.Fatalf("the memo was not used: stats %+v, item %.120s", stats, sent[0])
	}
}

func TestMemoRemembersAnImageThatStaysAsItIs(t *testing.T) {
	// A negative entry (empty string) keeps even an image that would shrink untouched.
	big := pngURL(t, noisy(300, 300, 255))
	resizer := New(testOptions(), []string{"user"})
	resizer.cache[sha256.Sum256([]byte(big))] = ""
	history := [][]byte{userMessage(imagePart(big))}
	sent, stats := resizer.Apply(history, nil)
	if stats.Resized != 0 || &sent[0] != &history[0] {
		t.Fatalf("a negative memo entry was ignored: %+v", stats)
	}
	small := pngURL(t, noisy(20, 20, 255))
	resizer = New(testOptions(), []string{"user"})
	resizer.Apply([][]byte{userMessage(imagePart(small))}, nil)
	if got, found := resizer.cache[sha256.Sum256([]byte(small))]; !found || got != "" {
		t.Fatalf("an image that fits must leave a negative entry, got %q found=%v", got, found)
	}
}

func TestMemoIsBoundedAndEvictsTheOldest(t *testing.T) {
	options := testOptions()
	options.CacheEntries = 2
	resizer := New(options, []string{"user"})
	for size := 300; size < 305; size++ {
		resizer.Apply([][]byte{userMessage(imagePart(pngURL(t, noisy(size, size, 255))))}, nil)
	}
	if len(resizer.cache) != 2 || len(resizer.order) != 2 {
		t.Fatalf("cache %d, order %d, want 2 and 2", len(resizer.cache), len(resizer.order))
	}
}

func TestApplyIsSafeForConcurrentUse(t *testing.T) {
	big := pngURL(t, noisy(300, 300, 255))
	history := [][]byte{functionCall("mcp_shot", "c1"), toolOutput("c1", imagePart(big))}
	resizer := New(testOptions(), []string{"mcp"})
	sources := sourceByName(map[string]string{"mcp_shot": SourceMCP})
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, stats := resizer.Apply(history, sources); stats.Resized != 1 {
				t.Errorf("stats = %+v", stats)
			}
		}()
	}
	group.Wait()
}
