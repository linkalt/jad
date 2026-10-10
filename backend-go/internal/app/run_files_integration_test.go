package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

var integrationPDF = []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n")

func fileRunBody(name string, data []byte, text string) string {
	parts := []map[string]string{}
	if text != "" {
		parts = append(parts, map[string]string{"type": "text", "text": text})
	}
	parts = append(parts, map[string]string{"type": "file", "name": name, "data": base64.StdEncoding.EncodeToString(data)})
	encoded, _ := json.Marshal(map[string]any{"input": parts})
	return string(encoded)
}

// A PDF reaches the model as an input_file with its name, stays in messages.jsonl as the
// same part, and the snapshot lists only the name: the bytes never reach the browser.
func TestRunRequestFileReachesTheModelAndOnlyItsNameReachesTheBrowser(t *testing.T) {
	f := newRunImagesFixture(t, nil, nil)
	status, stream := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", fileRunBody("report.pdf", integrationPDF, "summarize"))
	if status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("status %d: %.300s", status, stream)
	}
	encoded := base64.StdEncoding.EncodeToString(integrationPDF)
	body := f.lastProviderBody(t)
	for _, want := range []string{`"type":"input_file"`, `"filename":"report.pdf"`, "data:application/pdf;base64," + encoded, "summarize"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the provider body lacks %q: %.500s", want, body)
		}
	}
	transcript := f.transcript(t)
	if !strings.Contains(transcript, `"filename":"report.pdf"`) || !strings.Contains(transcript, encoded) {
		t.Fatalf("messages.jsonl must hold the file part: %.300s", transcript)
	}

	_, snapshot := f.do(t, http.MethodGet, "/api/sessions/local:demo", "")
	var page struct {
		Messages []struct {
			Sender string `json:"sender"`
			Text   string `json:"text"`
			Parts  []struct {
				Files []string `json:"files"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(snapshot), &page); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, message := range page.Messages {
		if message.Sender == "user" {
			if message.Text != "summarize" {
				t.Errorf("the user text = %q, want summarize", message.Text)
			}
			for _, part := range message.Parts {
				names = append(names, part.Files...)
			}
		}
	}
	if len(names) != 1 || names[0] != "report.pdf" {
		t.Fatalf("snapshot lists %v, want [report.pdf]", names)
	}
	if strings.Contains(snapshot, encoded) || strings.Contains(snapshot, "base64") {
		t.Fatal("the file bytes leaked into the snapshot")
	}
	// A later turn still carries the file.
	f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"and the second page?"}`)
	if !strings.Contains(f.lastProviderBody(t), encoded) {
		t.Fatal("the next turn must still carry the file")
	}
}

func TestRunRequestFileAloneIsAMessageWithItsName(t *testing.T) {
	f := newRunImagesFixture(t, nil, nil)
	f.do(t, http.MethodPost, "/api/sessions/local:demo/run", fileRunBody("a.pdf", integrationPDF, ""))
	_, snapshot := f.do(t, http.MethodGet, "/api/sessions/local:demo", "")
	if !strings.Contains(snapshot, `"text":"[file: a.pdf]"`) {
		t.Fatalf("a file-only message is not listed with its name: %.400s", snapshot)
	}
}

// Refused files write nothing: not a PDF, too many, too large, file input off.
func TestRunRequestRefusedFilesWriteNothing(t *testing.T) {
	notPDF := []byte("hello, not a pdf")
	for _, tc := range []struct {
		name   string
		server map[string]any
		body   string
		status int
		want   string
	}{
		{"not a pdf", nil, fileRunBody("x.pdf", notPDF, ""), http.StatusBadRequest, "not a PDF"},
		{"file input off", map[string]any{"max_request_files": 0}, fileRunBody("x.pdf", integrationPDF, ""), http.StatusBadRequest, "max_request_files"},
		{"too large", map[string]any{"max_request_file_bytes": len(integrationPDF) - 1}, fileRunBody("x.pdf", integrationPDF, ""), http.StatusRequestEntityTooLarge, "larger than"},
	} {
		f := newRunImagesFixture(t, tc.server, nil)
		status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", tc.body)
		if status != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%s: status %d %.200s, want %d containing %q", tc.name, status, body, tc.status, tc.want)
		}
		if got := f.transcript(t); got != "" {
			t.Errorf("%s: messages.jsonl is not empty: %.200s", tc.name, got)
		}
		f.mu.Lock()
		requests := len(f.bodies)
		f.mu.Unlock()
		if requests != 0 {
			t.Errorf("%s: the provider got %d requests", tc.name, requests)
		}
	}
}

// The body limit grows by the files a run may carry and no more.
func TestRunRequestBodyLimitAllowsAllowedFilesOnly(t *testing.T) {
	big := append([]byte("%PDF-1.4\n"), []byte(strings.Repeat("x", 3000))...)
	server := map[string]any{"max_body_bytes": 512, "max_request_files": 1, "max_request_file_bytes": len(big)}
	ok := newRunImagesFixture(t, server, nil)
	if status, body := ok.do(t, http.MethodPost, "/api/sessions/local:demo/run", fileRunBody("big.pdf", big, "")); status != http.StatusOK {
		t.Fatalf("an allowed file over max_body_bytes: %d %.200s", status, body)
	}
	none := newRunImagesFixture(t, map[string]any{"max_body_bytes": 512, "max_request_files": 0}, nil)
	if status, _ := none.do(t, http.MethodPost, "/api/sessions/local:demo/run", fileRunBody("big.pdf", big, "")); status == http.StatusOK {
		t.Fatal("a file over max_body_bytes with file input off must be refused")
	}
	// Text padding in the same request is still judged by max_body_bytes.
	padded := fileRunBody("big.pdf", big, strings.Repeat("t", 600))
	if status, _ := ok.do(t, http.MethodPost, "/api/sessions/local:demo/run", padded); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("600 bytes of text over a 512 byte body limit: status %d, want 413", status)
	}
}

// image_store and image_resize look at images only: with both on, a PDF in the same
// message is kept whole in messages.jsonl and reaches the model byte for byte, while the
// image next to it is stored and resized as before.
func TestRunRequestFileIsLeftAloneByTheImageStoreAndResize(t *testing.T) {
	session := map[string]any{
		"image_store":  map[string]any{"dir": "images", "min_bytes": 100},
		"image_resize": map[string]any{"enabled": true, "max_edge_px": 100, "applies_to": []string{"user"}},
	}
	f := newRunImagesFixture(t, nil, session)
	pdf := append([]byte("%PDF-1.4\n"), []byte(strings.Repeat("0123456789abcdef", 300))...)
	picture := squarePNG(t, 400)
	body, _ := json.Marshal(map[string]any{"input": []map[string]string{
		{"type": "text", "text": "both"},
		{"type": "file", "name": "doc.pdf", "data": base64.StdEncoding.EncodeToString(pdf)},
		{"type": "image", "data": base64.StdEncoding.EncodeToString(picture)},
	}})
	status, stream := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", string(body))
	if status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("status %d: %.300s", status, stream)
	}
	encodedPDF := base64.StdEncoding.EncodeToString(pdf)
	transcript := f.transcript(t)
	if !strings.Contains(transcript, encodedPDF) || !strings.Contains(transcript, `"filename":"doc.pdf"`) {
		t.Fatal("messages.jsonl must keep the PDF whole")
	}
	if strings.Count(transcript, "stored-image://") != 1 || strings.Contains(transcript, base64.StdEncoding.EncodeToString(picture)) {
		t.Fatalf("the image must be stored as one reference: %.300s", transcript)
	}
	sent := f.lastProviderBody(t)
	if !strings.Contains(sent, "data:application/pdf;base64,"+encodedPDF) {
		t.Fatal("the model must get the PDF byte for byte")
	}
	if strings.Contains(sent, base64.StdEncoding.EncodeToString(picture)) {
		t.Fatal("the image must be resized for the model (user is listed)")
	}
}
