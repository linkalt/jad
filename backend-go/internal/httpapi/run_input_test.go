package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewNRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

var testLimits = RunInputLimits{MaxImages: 2, MaxImageBytes: 4096}

func requireBadInput(t *testing.T, input string, limits RunInputLimits, wantCode int, wantText string) {
	t.Helper()
	got, err := parseRunInput(json.RawMessage(input), limits)
	var status *StatusError
	if !errors.As(err, &status) {
		t.Fatalf("%.80s: err = %v, result %s; want a StatusError", input, err, got)
	}
	if status.Code != wantCode || !strings.Contains(status.Message, wantText) {
		t.Fatalf("%.80s: %d %q, want %d containing %q", input, status.Code, status.Message, wantCode, wantText)
	}
}

// A string input stays exactly what it was: the runner gets the JSON string, so the
// transcript record is the same as before images existed.
func TestParseRunInputKeepsAStringInputAsAString(t *testing.T) {
	got, err := parseRunInput(json.RawMessage(`"hello \"there\""`), testLimits)
	if err != nil || string(got) != `"hello \"there\""` {
		t.Fatalf("%s, %v", got, err)
	}
	// With image input off the string path is unchanged.
	if got, err := parseRunInput(json.RawMessage(`"x"`), RunInputLimits{}); err != nil || string(got) != `"x"` {
		t.Fatalf("%s, %v", got, err)
	}
}

func TestParseRunInputRefusesWhatTheStringPathAlwaysRefused(t *testing.T) {
	for _, input := range []string{`""`, `"   "`, `5`, `null`, `true`, `{}`, `{"type":"text"}`} {
		requireBadInput(t, input, testLimits, http.StatusBadRequest, "input must be a non-empty string")
	}
}

func TestParseRunInputBuildsOneUserMessageFromParts(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testPNG(t))
	got, err := parseRunInput(json.RawMessage(`[{"type":"text","text":"look"},{"type":"image","data":"`+data+`"},{"type":"text","text":" and this "},{"type":"image","data":"data:image/jpeg;base64,`+data+`"}]`), testLimits)
	if err != nil {
		t.Fatal(err)
	}
	var message struct {
		Type, Role string
		Content    []map[string]string
	}
	if err := json.Unmarshal(got, &message); err != nil {
		t.Fatal(err)
	}
	if message.Type != "message" || message.Role != "user" || len(message.Content) != 4 {
		t.Fatalf("%s", got)
	}
	want := "data:image/png;base64," + data
	for index, expected := range []map[string]string{
		{"type": "input_text", "text": "look"},
		{"type": "input_image", "image_url": want, "detail": "auto"},
		{"type": "input_text", "text": " and this "},
		// The declared image/jpeg is ignored: the bytes are a png.
		{"type": "input_image", "image_url": want, "detail": "auto"},
	} {
		for key, value := range expected {
			if message.Content[index][key] != value {
				t.Errorf("part %d %s = %q, want %q", index, key, message.Content[index][key], value)
			}
		}
	}
}

func TestParseRunInputAcceptsAnImageWithoutText(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testPNG(t))
	got, err := parseRunInput(json.RawMessage(`[{"type":"image","data":"`+data+`"}]`), testLimits)
	if err != nil || !strings.Contains(string(got), `"input_image"`) || strings.Contains(string(got), `"input_text"`) {
		t.Fatalf("%s, %v", got, err)
	}
}

func TestParseRunInputRefusesBadArrays(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testPNG(t))
	for _, tc := range []struct {
		input string
		want  string
	}{
		{`[]`, "non-empty array"},
		{`["hi"]`, "input part 1 must be an object"},
		{`[{"type":"text","text":"   "}]`, "some text or an image"},
		{`[{"type":"text"}]`, "input part 1 needs a text string"},
		{`[{"type":"text","text":5}]`, "input part 1 must be an object"},
		{`[{"type":"text","text":"a","extra":1}]`, `unknown field "extra"`},
		{`[{"type":"audio","data":"x"}]`, "unknown type"},
		{`[{"data":"x"}]`, "unknown type"},
		{`[{"type":"image"}]`, "needs data"},
		{`[{"type":"image","path":"/etc/passwd"}]`, `unknown field "path"`},
		{`[{"type":"image","url":"http://x/y.png","data":"` + data + `"}]`, `unknown field "url"`},
		{`[{"type":"image","data":"not base64!"}]`, "input part 1: the data is not valid base64"},
		{`[{"type":"text","text":"a"},{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("hello, not an image")) + `"}]`, "input part 2: the image is text/plain"},
		{`[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("<svg></svg>")) + `"}]`, "png, jpeg, webp and gif"},
	} {
		requireBadInput(t, tc.input, testLimits, http.StatusBadRequest, tc.want)
	}
}

func TestParseRunInputLimits(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testPNG(t))
	image := `{"type":"image","data":"` + data + `"}`
	if _, err := parseRunInput(json.RawMessage(`[`+image+`,`+image+`]`), testLimits); err != nil {
		t.Fatalf("two images at the limit of two: %v", err)
	}
	requireBadInput(t, `[`+image+`,`+image+`,`+image+`]`, testLimits, http.StatusBadRequest, "more than 2 images")
	// A request with images while image input is off says so, and names the setting.
	requireBadInput(t, `[`+image+`]`, RunInputLimits{}, http.StatusBadRequest, "server.max_request_images")
	// A text-only array still works with image input off.
	if _, err := parseRunInput(json.RawMessage(`[{"type":"text","text":"hi"}]`), RunInputLimits{}); err != nil {
		t.Fatal(err)
	}
	size := int64(len(testPNG(t)))
	if _, err := parseRunInput(json.RawMessage(`[`+image+`]`), RunInputLimits{MaxImages: 1, MaxImageBytes: size}); err != nil {
		t.Fatalf("exactly at the byte limit: %v", err)
	}
	requireBadInput(t, `[`+image+`]`, RunInputLimits{MaxImages: 1, MaxImageBytes: size - 1}, http.StatusRequestEntityTooLarge, "larger than")
}

func TestRunBodyLimitGrowsOnlyWhenImagesAreAllowed(t *testing.T) {
	h := &Handler{maxBodyBytes: 1000}
	if got := h.runBodyLimit(); got != 1000 {
		t.Fatalf("images off: %d, want 1000", got)
	}
	h.runInputLimits = RunInputLimits{MaxImages: 2, MaxImageBytes: 300}
	// Each image: 300 bytes as base64 is 400 characters, plus a little for the JSON around it.
	if got := h.runBodyLimit(); got < 1000+2*400 || got > 1000+2*500 {
		t.Fatalf("images on: %d, want about %d", got, 1000+2*400)
	}
}

func TestUserMessageFromTranscriptRecordCarriesImages(t *testing.T) {
	name := strings.Repeat("e", 64) + ".png"
	record := json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"stored-image://` + name + `"}]}`)
	encoded, visible, err := UserMessageFromTranscriptRecord(record, 42)
	if err != nil || !visible {
		t.Fatalf("%v, visible %v", err, visible)
	}
	var message struct {
		ID    string
		Text  string
		Parts []struct {
			Type   string
			Images []string
		}
	}
	if err := json.Unmarshal(encoded, &message); err != nil {
		t.Fatal(err)
	}
	if message.ID != "session-user-o42" || message.Text != "[image]" || len(message.Parts) != 1 || len(message.Parts[0].Images) != 1 || message.Parts[0].Images[0] != name {
		t.Fatalf("%s", encoded)
	}
	// A message with no text and no image is still not a message.
	if _, visible, _ := UserMessageFromTranscriptRecord(json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_file"}]}`), 1); visible {
		t.Fatal("a message without text or image must stay hidden")
	}
}
