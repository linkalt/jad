package webui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestUserContentReadsTextAndStoredImages(t *testing.T) {
	a := strings.Repeat("a", 64) + ".png"
	b := strings.Repeat("b", 64) + ".jpg"
	ref := func(name string) string { return "stored-image://" + name }
	for _, tc := range []struct {
		name    string
		content []UserContentPart
		text    string
		images  []string
		visible bool
	}{
		{"text only", []UserContentPart{{Type: "input_text", Text: "hi"}}, "hi", nil, true},
		{"text parts joined", []UserContentPart{{Type: "input_text", Text: "a"}, {Type: "input_text", Text: "b"}}, "ab", nil, true},
		{"text and a stored image", []UserContentPart{{Type: "input_text", Text: "look"}, {Type: "input_image", ImageURL: ref(a)}}, "look", []string{a}, true},
		{"image only is visible", []UserContentPart{{Type: "input_image", ImageURL: ref(a)}}, "[image]", []string{a}, true},
		{"two images only", []UserContentPart{{Type: "input_image", ImageURL: ref(a)}, {Type: "input_image", ImageURL: ref(b)}}, "[image] [image]", []string{a, b}, true},
		{"repeat listed once, both counted", []UserContentPart{{Type: "input_image", ImageURL: ref(a)}, {Type: "input_image", ImageURL: ref(a)}}, "[image] [image]", []string{a}, true},
		{"inline image is not listed but is visible", []UserContentPart{{Type: "input_image", ImageURL: "data:image/png;base64,AAAA"}}, "[image]", nil, true},
		{"bad name is not listed", []UserContentPart{{Type: "input_text", Text: "x"}, {Type: "input_image", ImageURL: ref("../../etc/passwd")}}, "x", nil, true},
		{"http url is not listed", []UserContentPart{{Type: "input_text", Text: "x"}, {Type: "input_image", ImageURL: "https://example.com/a.png"}}, "x", nil, true},
		{"empty", nil, "", nil, false},
		{"empty text, no image", []UserContentPart{{Type: "input_text", Text: ""}}, "", nil, false},
		{"unknown part", []UserContentPart{{Type: "input_file"}}, "", nil, false},
	} {
		text, images, visible := UserContent(tc.content)
		if text != tc.text || !reflect.DeepEqual(images, tc.images) || visible != tc.visible {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, %v)", tc.name, text, images, visible, tc.text, tc.images, tc.visible)
		}
	}
}

func TestUserMessageCarriesStoredImagesAndKeepsPlainOnesUnchanged(t *testing.T) {
	a := strings.Repeat("d", 64) + ".webp"
	records := []TranscriptRecord{
		{Offset: 0, Raw: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"plain"}]}`)},
		{Offset: 100, Raw: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"see"},{"type":"input_image","image_url":"stored-image://` + a + `","detail":"auto"}]}`)},
		{Offset: 200, Raw: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`)},
	}
	messages, err := NormalizeTranscript(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("%d messages, want 3", len(messages))
	}
	plain, _ := json.Marshal(messages[0])
	if strings.Contains(string(plain), "images") {
		t.Fatalf("a text message must not gain an images field: %s", plain)
	}
	if got := messages[1].Parts[0].Images; !reflect.DeepEqual(got, []string{a}) || messages[1].Text != "see" {
		t.Fatalf("message 2 = %+v", messages[1])
	}
	// The data URL itself never reaches the browser.
	encoded, _ := json.Marshal(messages[2])
	if strings.Contains(string(encoded), "base64") || messages[2].Text != "[image]" {
		t.Fatalf("message 3 = %s", encoded)
	}
}
