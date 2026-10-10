package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A tiny but real-looking PDF: the magic bytes decide, nothing else is parsed.
var testPDF = []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n")

var fileLimits = RunInputLimits{MaxImages: 2, MaxImageBytes: 4096, MaxFiles: 2, MaxFileBytes: 4096}

func filePart(name string, raw []byte) string {
	return `{"type":"file","name":` + quoted(name) + `,"data":"` + base64.StdEncoding.EncodeToString(raw) + `"}`
}

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestParseRunInputBuildsAnInputFilePart(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testPDF)
	got, err := parseRunInput(json.RawMessage(`[{"type":"text","text":"read it"},`+filePart("report.pdf", testPDF)+`]`), fileLimits)
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
	if message.Type != "message" || message.Role != "user" || len(message.Content) != 2 {
		t.Fatalf("%s", got)
	}
	want := map[string]string{"type": "input_file", "filename": "report.pdf", "file_data": "data:application/pdf;base64," + data}
	for key, value := range want {
		if message.Content[1][key] != value {
			t.Errorf("%s = %q, want %q", key, message.Content[1][key], value)
		}
	}
}

func TestParseRunInputAcceptsAFileWithoutText(t *testing.T) {
	got, err := parseRunInput(json.RawMessage(`[`+filePart("a.pdf", testPDF)+`]`), fileLimits)
	if err != nil || !strings.Contains(string(got), `"input_file"`) || strings.Contains(string(got), `"input_text"`) {
		t.Fatalf("%s, %v", got, err)
	}
}

// A data URL is accepted too, and a declared type is never trusted: the bytes decide.
func TestParseRunInputFileTakesADataURLAndJudgesTheBytes(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testPDF)
	got, err := parseRunInput(json.RawMessage(`[{"type":"file","name":"a.pdf","data":"data:text/plain;base64,`+data+`"}]`), fileLimits)
	if err != nil || !strings.Contains(string(got), "data:application/pdf;base64,"+data) {
		t.Fatalf("%s, %v", got, err)
	}
	notPDF := base64.StdEncoding.EncodeToString([]byte("hello, not a pdf"))
	requireBadInput(t, `[{"type":"file","name":"a.pdf","data":"data:application/pdf;base64,`+notPDF+`"}]`, fileLimits, http.StatusBadRequest, "input part 1: the file is not a PDF")
	// A PNG renamed to .pdf is still refused.
	requireBadInput(t, `[`+filePart("fake.pdf", testPNG(t))+`]`, fileLimits, http.StatusBadRequest, "only PDF files are supported")
}

func TestParseRunInputRefusesBadFileParts(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testPDF)
	for _, tc := range []struct {
		input string
		want  string
	}{
		{`[{"type":"file","name":"a.pdf"}]`, "needs data"},
		{`[{"type":"file","data":"` + data + `"}]`, "needs a name"},
		{`[{"type":"file","name":"","data":"` + data + `"}]`, "needs a file name"},
		{`[{"type":"file","name":"  ","data":"` + data + `"}]`, "needs a file name"},
		{`[{"type":"file","name":"/","data":"` + data + `"}]`, "needs a file name"},
		{`[{"type":"file","name":"a.pdf","data":"not base64!"}]`, "input part 1: the data is not valid base64"},
		{`[{"type":"file","name":"a.pdf","path":"/etc/passwd","data":"` + data + `"}]`, `unknown field "path"`},
		{`[{"type":"file","name":"a.pdf","url":"http://x/y.pdf","data":"` + data + `"}]`, `unknown field "url"`},
		{`[{"type":"file","name":5,"data":"` + data + `"}]`, "must be an object"},
		{`[{"type":"image","name":"a.png","data":"` + base64.StdEncoding.EncodeToString(testPNG(t)) + `"}]`, `unknown field "name"`},
		{`[{"type":"text","text":"a","name":"x"}]`, `unknown field "name"`},
		{`[{"type":"pdf","data":"x"}]`, "use text, image or file"},
	} {
		requireBadInput(t, tc.input, fileLimits, http.StatusBadRequest, tc.want)
	}
}

func TestParseRunInputFileLimits(t *testing.T) {
	one := filePart("a.pdf", testPDF)
	if _, err := parseRunInput(json.RawMessage(`[`+one+`,`+one+`]`), fileLimits); err != nil {
		t.Fatalf("two files at the limit of two: %v", err)
	}
	requireBadInput(t, `[`+one+`,`+one+`,`+one+`]`, fileLimits, http.StatusBadRequest, "more than 2 files")
	// File input off says so, and names the setting.
	off := fileLimits
	off.MaxFiles = 0
	requireBadInput(t, `[`+one+`]`, off, http.StatusBadRequest, "server.max_request_files")
	// A text-only or image-only array still works with file input off.
	if _, err := parseRunInput(json.RawMessage(`[{"type":"text","text":"hi"}]`), off); err != nil {
		t.Fatal(err)
	}
	size := int64(len(testPDF))
	exact := fileLimits
	exact.MaxFileBytes = size
	if _, err := parseRunInput(json.RawMessage(`[`+one+`]`), exact); err != nil {
		t.Fatalf("exactly at the byte limit: %v", err)
	}
	exact.MaxFileBytes = size - 1
	requireBadInput(t, `[`+one+`]`, exact, http.StatusRequestEntityTooLarge, "the file is larger than")
	// Images and files count on their own: two images and two files together are fine.
	image := `{"type":"image","data":"` + base64.StdEncoding.EncodeToString(testPNG(t)) + `"}`
	if _, err := parseRunInput(json.RawMessage(`[`+image+`,`+image+`,`+one+`,`+one+`]`), fileLimits); err != nil {
		t.Fatalf("2 images and 2 files: %v", err)
	}
}

func TestParseRunInputCleansTheFileName(t *testing.T) {
	for name, want := range map[string]string{
		"report.pdf":                    "report.pdf",
		"../../etc/passwd.pdf":          "passwd.pdf",
		`C:\Users\me\Docs\Q3 plan.pdf`:  "Q3 plan.pdf",
		"  spaced.pdf  ":                "spaced.pdf",
		"line\nbreak\x00.pdf":           "linebreak.pdf",
		"日本語の資料.pdf":                    "日本語の資料.pdf",
		strings.Repeat("a", 400) + ".p": strings.Repeat("a", maxFilenameRunes),
	} {
		got, err := parseRunInput(json.RawMessage(`[`+filePart(name, testPDF)+`]`), fileLimits)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		var message struct{ Content []map[string]string }
		if err := json.Unmarshal(got, &message); err != nil {
			t.Fatal(err)
		}
		if message.Content[0]["filename"] != want {
			t.Errorf("%q -> %q, want %q", name, message.Content[0]["filename"], want)
		}
	}
}

func TestRunBodyLimitCountsFilesToo(t *testing.T) {
	h := &Handler{maxBodyBytes: 1000}
	h.runInputLimits = RunInputLimits{MaxFiles: 2, MaxFileBytes: 300}
	if got := h.runBodyLimit(); got < 1000+2*400 || got > 1000+2*500 {
		t.Fatalf("files on: %d, want about %d", got, 1000+2*400)
	}
	h.runInputLimits = RunInputLimits{MaxImages: 1, MaxImageBytes: 300, MaxFiles: 1, MaxFileBytes: 300}
	if got := h.runBodyLimit(); got < 1000+2*400 || got > 1000+2*500 {
		t.Fatalf("images and files on: %d, want about %d", got, 1000+2*400)
	}
}

// The data of a file is not part of the body that max_body_bytes judges, like an image.
func TestParseRunInputSizedCountsFileDataAsBulk(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testPDF)
	_, chars, err := parseRunInputSized(json.RawMessage(`[`+filePart("a.pdf", testPDF)+`]`), fileLimits)
	if err != nil || chars != int64(len(data)) {
		t.Fatalf("chars = %d, want %d (err %v)", chars, len(data), err)
	}
}
