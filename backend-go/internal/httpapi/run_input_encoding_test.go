package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// referenceMessageContent and referenceMessage are the structs the user message was marshalled
// from before parseRunInputSized wrote it directly; the stored bytes must not change.
type referenceMessageContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

func TestParseRunInputEncodesTheSameBytesAsMarshallingTheMessage(t *testing.T) {
	png := base64.StdEncoding.EncodeToString(testPNG(t))
	pdf := base64.StdEncoding.EncodeToString(testPDF)
	texts := []string{"plain", "<b>&amp;</b>", "line\nbreak\ttab \"quoted\" \\ back", "unicode é 漢字 😀 \u2028\u2029", "  padded  "}
	var parts []string
	var want []referenceMessageContent
	for _, text := range texts {
		encoded, _ := json.Marshal(text)
		parts = append(parts, `{"type":"text","text":`+string(encoded)+`}`)
		want = append(want, referenceMessageContent{Type: "input_text", Text: text})
	}
	parts = append(parts, `{"type":"text","text":"   "}`)
	parts = append(parts, `{"type":"image","data":"`+png+`"}`, `{"type":"image","data":"data:image/jpeg;base64,`+png+`"}`)
	want = append(want,
		referenceMessageContent{Type: "input_image", ImageURL: "data:image/png;base64," + png, Detail: "auto"},
		referenceMessageContent{Type: "input_image", ImageURL: "data:image/png;base64," + png, Detail: "auto"})
	for _, name := range []string{"report.pdf", `a<b>&"c".pdf`, "dir/évidence 漢.pdf"} {
		encoded, _ := json.Marshal(name)
		parts = append(parts, `{"type":"file","name":`+string(encoded)+`,"data":"`+pdf+`"}`)
		clean, err := cleanFilename("x", name)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, referenceMessageContent{Type: "input_file", Filename: clean, FileData: "data:application/pdf;base64," + pdf})
	}
	reference, err := json.Marshal(struct {
		Type    string                    `json:"type"`
		Role    string                    `json:"role"`
		Content []referenceMessageContent `json:"content"`
	}{Type: "message", Role: "user", Content: want})
	if err != nil {
		t.Fatal(err)
	}
	limits := RunInputLimits{MaxImages: 4, MaxImageBytes: 4096, MaxFiles: 4, MaxFileBytes: 4096}
	input := "["
	for index, part := range parts {
		if index > 0 {
			input += ","
		}
		input += part
	}
	got, err := parseRunInput(json.RawMessage(input+"]"), limits)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(reference) {
		t.Fatalf("encoded message differs\n got: %s\nwant: %s", got, reference)
	}
}
