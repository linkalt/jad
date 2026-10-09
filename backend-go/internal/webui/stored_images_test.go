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
