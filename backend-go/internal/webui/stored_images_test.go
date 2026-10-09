package webui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestToolResultListsStoredImageNames(t *testing.T) {
	a := strings.Repeat("a", 64) + ".png"
	b := strings.Repeat("b", 64) + ".jpg"
	tests := []struct {
		name   string
		output string
		want   []string
	}{
		{"one stored image", `[{"type":"input_text","text":"shot"},{"type":"input_image","image_url":"stored-image://` + a + `"}]`, []string{a}},
		{"order kept, repeat dropped", `[{"type":"input_image","image_url":"stored-image://` + b + `"},{"type":"input_image","image_url":"stored-image://` + a + `"},{"type":"input_image","image_url":"stored-image://` + b + `"}]`, []string{b, a}},
		{"inline data url not listed", `[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]`, nil},
		{"http url not listed", `[{"type":"input_image","image_url":"https://example.com/a.png"}]`, nil},
		{"bad name not listed", `[{"type":"input_image","image_url":"stored-image://../../etc/passwd"},{"type":"input_image","image_url":"stored-image://` + strings.Repeat("A", 64) + `.png"},{"type":"input_image","image_url":"stored-image://` + strings.Repeat("a", 64) + `.svg"}]`, nil},
		{"reference on a non-image part ignored", `[{"type":"input_text","image_url":"stored-image://` + a + `"}]`, nil},
		{"plain string output", `"just text"`, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"type":"function_call_output","call_id":"c1","output":` + test.output + `}`
			part, ok := ToolResultPart(json.RawMessage(raw), 0)
			if !ok {
				t.Fatal("not a tool result")
			}
			if !reflect.DeepEqual(part.Images, test.want) {
				t.Fatalf("Images = %v, want %v", part.Images, test.want)
			}
		})
	}
}

func TestStoredImagesReachTranscriptAndJSON(t *testing.T) {
	a := strings.Repeat("c", 64) + ".webp"
	records := []TranscriptRecord{
		{Offset: 0, Raw: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}`)},
		{Offset: 1, Raw: json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"stored-image://` + a + `"}]}`)},
	}
	messages, err := NormalizeTranscript(records)
	if err != nil {
		t.Fatal(err)
	}
	var found *Part
	for i := range messages {
		for j := range messages[i].Parts {
			if messages[i].Parts[j].Type == "tool_result" {
				found = &messages[i].Parts[j]
			}
		}
	}
	if found == nil || !reflect.DeepEqual(found.Images, []string{a}) || found.Output != "[MCP image content]" {
		t.Fatalf("tool result part = %+v", found)
	}
	encoded, _ := json.Marshal(found)
	if !strings.Contains(string(encoded), `"images":["`+a+`"]`) {
		t.Fatalf("json = %s", encoded)
	}
	plain, _ := json.Marshal(Part{Type: "tool_result", CallID: "x"})
	if strings.Contains(string(plain), "images") {
		t.Fatalf("empty Images must be omitted: %s", plain)
	}
}

func TestHostedToolListsGeneratedImagesBeforeTheRawIsCapped(t *testing.T) {
	a := strings.Repeat("a", 64) + ".png"
	b := strings.Repeat("b", 64) + ".webp"
	item := `{"type":"openrouter:image_generation","id":"st_1","status":"completed","result":"stored-image-b64://` + a + `","imageUrl":"stored-image://` + a + `","extra":"stored-image://` + b + `","prompt":"` + strings.Repeat("x", 4000) + `"}`
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(item), &fields); err != nil {
		t.Fatal(err)
	}
	part, ok := hostedToolPart(fields, 200)
	if !ok {
		t.Fatal("not a hosted tool")
	}
	// The raw is compact JSON with sorted keys, so "extra" (b) comes before "imageUrl" (a).
	if want := []string{b, a}; !reflect.DeepEqual(part.Images, want) {
		t.Fatalf("Images = %v, want %v", part.Images, want)
	}
	if part.Truncated == nil || !json.Valid([]byte(part.Raw)) {
		t.Fatalf("raw should be capped and valid: truncated=%v", part.Truncated)
	}
	encoded, _ := json.Marshal(part)
	if !strings.Contains(string(encoded), `"images":["`+b+`","`+a+`"]`) {
		t.Fatalf("images missing in JSON: %s", encoded)
	}
	plain := `{"type":"web_search_call","id":"w","status":"completed","action":{"query":"x"}}`
	var searchFields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(plain), &searchFields)
	if part, _ := hostedToolPart(searchFields, 0); part.Images != nil {
		t.Fatalf("a search item got images: %v", part.Images)
	}
}
