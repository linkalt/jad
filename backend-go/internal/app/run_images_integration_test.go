package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"bytes"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// runImagesFixture is a server with a fake Responses provider that records every
// request body, so a test can see what the model would receive.
type runImagesFixture struct {
	apiURL       string
	messagesPath string
	mu           sync.Mutex
	bodies       []string
}

func newRunImagesFixture(t *testing.T, server, session map[string]any) *runImagesFixture {
	t.Helper()
	f := &runImagesFixture{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		message := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"seen"}]}`
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":` + message + `}`,
			`{"type":"response.completed","response":{"id":"r1","status":"completed","model":"m","output":[` + message + `]}}`,
			"[DONE]",
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	t.Cleanup(provider.Close)
	environment := map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "k"}
	config, _ := layoutConfig(t, layouttest.Spec{
		SessionID: "demo", Settings: session, Server: server,
		Models: map[string]map[string]any{"openai/default": {"provider": "openai", "model": "m", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, environment)
	location, err := sessionfs.Resolve(config.Roots, "local:demo")
	if err != nil {
		t.Fatal(err)
	}
	sessionSettings, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.messagesPath = sessionSettings.MessagesPath
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(service.Handler)
	t.Cleanup(func() { service.Close(); api.Close() })
	f.apiURL = api.URL
	return f
}

func (f *runImagesFixture) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, f.apiURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(raw)
}

func (f *runImagesFixture) transcript(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(f.messagesPath)
	return string(data)
}

func (f *runImagesFixture) lastProviderBody(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("the provider got no request")
	}
	return f.bodies[len(f.bodies)-1]
}

// noisyPNG is a png that does not compress, so its size is about size*size*4 bytes.
func noisyPNG(t *testing.T, size int) []byte {
	t.Helper()
	picture := image.NewNRGBA(image.Rect(0, 0, size, size))
	state := uint32(2463534242)
	for i := range picture.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		picture.Pix[i] = byte(state)
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func squarePNG(t *testing.T, size int) []byte {
	t.Helper()
	picture := image.NewNRGBA(image.Rect(0, 0, size, size))
	for i := range picture.Pix {
		picture.Pix[i] = byte(i * 7)
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func imageRunBody(data []byte, text string) string {
	parts := []map[string]string{}
	if text != "" {
		parts = append(parts, map[string]string{"type": "text", "text": text})
	}
	parts = append(parts, map[string]string{"type": "image", "data": base64.StdEncoding.EncodeToString(data)})
	encoded, _ := json.Marshal(map[string]any{"input": parts})
	return string(encoded)
}

// With the store off, the image stays inline in messages.jsonl and the model gets it.
func TestRunRequestImageReachesTheModelAndStaysInlineWithoutAStore(t *testing.T) {
	f := newRunImagesFixture(t, nil, nil)
	data := squarePNG(t, 40)
	status, stream := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", imageRunBody(data, "what is this"))
	if status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("status %d: %.300s", status, stream)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	if !strings.Contains(f.lastProviderBody(t), want) || !strings.Contains(f.lastProviderBody(t), "what is this") {
		t.Fatalf("the provider body lacks the image or the text: %.400s", f.lastProviderBody(t))
	}
	if !strings.Contains(f.transcript(t), want) || strings.Contains(f.transcript(t), "stored-image://") {
		t.Fatal("messages.jsonl must hold the data URL when no store is configured")
	}
}

// With the store on, the transcript keeps a reference, the model still gets the pixels,
// the snapshot lists the file, and the route serves the same bytes.
func TestRunRequestImageIsStoredAndServed(t *testing.T) {
	f := newRunImagesFixture(t, nil, map[string]any{"image_store": map[string]any{"dir": "images", "min_bytes": 100}})
	data := squarePNG(t, 60)
	status, stream := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", imageRunBody(data, ""))
	if status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("status %d: %.300s", status, stream)
	}
	if !strings.Contains(f.lastProviderBody(t), base64.StdEncoding.EncodeToString(data)) {
		t.Fatal("the model must get the full image back from the store")
	}
	transcript := f.transcript(t)
	if strings.Contains(transcript, base64.StdEncoding.EncodeToString(data)) || strings.Count(transcript, "stored-image://") != 1 {
		t.Fatalf("messages.jsonl must hold one reference and no pixels: %.300s", transcript)
	}

	_, snapshot := f.do(t, http.MethodGet, "/api/sessions/local:demo", "")
	var page struct {
		Messages []struct {
			Sender string `json:"sender"`
			Text   string `json:"text"`
			Parts  []struct {
				Images []string `json:"images"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(snapshot), &page); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, message := range page.Messages {
		if message.Sender == "user" {
			if message.Text != "[image]" {
				t.Errorf("an image-only message shows %q, want [image]", message.Text)
			}
			for _, part := range message.Parts {
				names = append(names, part.Images...)
			}
		}
	}
	if len(names) != 1 || strings.Contains(snapshot, "base64") {
		t.Fatalf("snapshot lists %v; pixels leaked: %v", names, strings.Contains(snapshot, "base64"))
	}
	status, served := f.do(t, http.MethodGet, "/api/sessions/local:demo/images/"+names[0], "")
	if status != http.StatusOK || served != string(data) {
		t.Fatalf("route: status %d, %d bytes (want %d, identical)", status, len(served), len(data))
	}
	// A second run in the same session still sends the stored image to the model.
	f.do(t, http.MethodPost, "/api/sessions/local:demo/run", `{"input":"and again?"}`)
	if !strings.Contains(f.lastProviderBody(t), base64.StdEncoding.EncodeToString(data)) {
		t.Fatal("the next turn must still carry the image")
	}
}

// image_resize applies to the user's image only when applies_to lists "user".
func TestRunRequestImageIsResizedOnlyWhenUserIsListed(t *testing.T) {
	data := squarePNG(t, 400)
	for _, tc := range []struct {
		name      string
		appliesTo []string
		resized   bool
	}{{"default (mcp only)", nil, false}, {"user listed", []string{"user"}, true}} {
		resize := map[string]any{"enabled": true, "max_edge_px": 100}
		if tc.appliesTo != nil {
			resize["applies_to"] = tc.appliesTo
		}
		f := newRunImagesFixture(t, nil, map[string]any{"image_resize": resize})
		f.do(t, http.MethodPost, "/api/sessions/local:demo/run", imageRunBody(data, "x"))
		body := f.lastProviderBody(t)
		original := strings.Contains(body, base64.StdEncoding.EncodeToString(data))
		if original == tc.resized {
			t.Errorf("%s: the provider got the original = %v, want %v", tc.name, original, !tc.resized)
		}
		// Whatever was sent, messages.jsonl keeps the original.
		if !strings.Contains(f.transcript(t), base64.StdEncoding.EncodeToString(data)) {
			t.Errorf("%s: messages.jsonl must keep the original", tc.name)
		}
	}
}

// The body limit is max_body_bytes for everything but the images a run may carry.
func TestRunRequestBodyLimitAllowsAllowedImagesOnly(t *testing.T) {
	data := noisyPNG(t, 30) // about 3.6 KB, far over the 512-byte body limit
	if len(data) < 3000 {
		t.Fatalf("the test image is %d bytes; it must be over the plain body limit", len(data))
	}
	server := map[string]any{"max_body_bytes": 512, "max_request_images": 1, "max_request_image_bytes": len(data)}
	f := newRunImagesFixture(t, server, nil)
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", imageRunBody(data, "")); status != http.StatusOK {
		t.Fatalf("an image within the limits: status %d: %.200s", status, body)
	}
	// The same body is refused when there is no allowance at all: no images, and no files
	// either (a file allowance also widens the run body limit, and then the parser answers
	// 400 "image input is off" instead of the reader's 413).
	none := newRunImagesFixture(t, map[string]any{"max_body_bytes": 512, "max_request_images": 0, "max_request_files": 0}, nil)
	if status, _ := none.do(t, http.MethodPost, "/api/sessions/local:demo/run", imageRunBody(data, "")); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("image input off and a body over max_body_bytes: status %d, want 413", status)
	}
	// Text over max_body_bytes but inside the image allowance is refused too: the allowance
	// is for image data only (this was a regression the differential harness found).
	middle, _ := json.Marshal(map[string]string{"input": strings.Repeat("x", 3000)})
	if status, _ := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", string(middle)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a text of 3000 bytes with max_body_bytes 512 and room for one image: status %d, want 413", status)
	}
	// The same text next to an image is refused as well.
	withText, _ := json.Marshal(map[string]any{"input": []map[string]string{{"type": "text", "text": strings.Repeat("x", 3000)}, {"type": "image", "data": base64.StdEncoding.EncodeToString(data)}}})
	if status, _ := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", string(withText)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("long text next to an image: status %d, want 413", status)
	}
	// Text over max_body_bytes plus the image allowance (about 4.9 KB here) is refused.
	huge, _ := json.Marshal(map[string]string{"input": strings.Repeat("x", 20000)})
	if status, _ := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", string(huge)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a long text: status %d, want 413", status)
	}
	// One byte over the image limit is refused with 413.
	small := newRunImagesFixture(t, map[string]any{"max_body_bytes": 512, "max_request_images": 1, "max_request_image_bytes": len(data) - 1}, nil)
	if status, _ := small.do(t, http.MethodPost, "/api/sessions/local:demo/run", imageRunBody(data, "")); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an image over the limit: status %d, want 413", status)
	}
	// Other routes keep max_body_bytes: a body bigger than it is refused with 413.
	if status, _ := f.do(t, http.MethodPost, "/api/sessions/local:demo/profile", `{"active_profile":"`+strings.Repeat("p", 2000)+`"}`); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("another route with a long body: status %d, want 413", status)
	}
}

// A refused request leaves nothing in the transcript and starts no run.
func TestRunRequestRefusedImagesWriteNothing(t *testing.T) {
	f := newRunImagesFixture(t, map[string]any{"max_request_images": 0}, nil)
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run", imageRunBody(squarePNG(t, 8), "x"))
	if status != http.StatusBadRequest || !strings.Contains(body, "max_request_images") {
		t.Fatalf("status %d: %.200s", status, body)
	}
	g := newRunImagesFixture(t, nil, nil)
	for name, input := range map[string]string{
		"not an image": `{"input":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("hello hello hello")) + `"}]}`,
		"a path":       `{"input":[{"type":"image","path":"/etc/hostname"}]}`,
		"bad base64":   `{"input":[{"type":"image","data":"%%%"}]}`,
	} {
		if status, _ := g.do(t, http.MethodPost, "/api/sessions/local:demo/run", input); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, status)
		}
	}
	for _, fixture := range []*runImagesFixture{f, g} {
		if got := fixture.transcript(t); got != "" {
			t.Errorf("a refused request wrote to messages.jsonl: %.200s", got)
		}
		fixture.mu.Lock()
		count := len(fixture.bodies)
		fixture.mu.Unlock()
		if count != 0 {
			t.Errorf("a refused request reached the provider %d times", count)
		}
	}
}
