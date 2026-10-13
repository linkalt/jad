package native

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/imageresize"
	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func decidePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// A cheap hash gives noise, which a png cannot compress, so size follows pixels.
			h := uint32(x)*2654435761 ^ uint32(y)*40503 ^ 0x9e3779b9
			h ^= h >> 15
			h *= 2246822519
			picture.Set(x, y, color.RGBA{R: uint8(h), G: uint8(h >> 8), B: uint8(h >> 16), A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, picture); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// imageRig is a decide tool with a workspace, wired to a fake provider.
type imageRig struct {
	catalog   *tools.Catalog
	workspace string
	requests  []map[string]any
}

func newImageRig(t *testing.T, backend string, images DecideImages, adjust func(*sessionfs.NativeDecideSettings)) *imageRig {
	t.Helper()
	rig := &imageRig{workspace: t.TempDir()}
	if resolved, err := filepath.EvalSymlinks(rig.workspace); err == nil {
		rig.workspace = resolved
	}
	reply := openRouterReply
	if backend == "openai" {
		reply = openAIReply
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rig.requests = append(rig.requests, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)
	settings := sessionfs.NativeDecideSettings{Backend: backend, BaseURL: server.URL, APIKeyEnv: "K", Model: "m", TimeoutMS: 5000, MaxQuestions: 4, MaxInputBytes: 1000, MaxResponseBytes: 4096, MaxImages: 2, MaxImageBytes: 4000}
	if adjust != nil {
		adjust(&settings)
	}
	tool, err := newDecideTool(settings, images, func(string) string { return "key" }, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if rig.catalog, err = tools.NewCatalog([]tools.Tool{tool}); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *imageRig) call(t *testing.T, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": "decide", "arguments": arguments})
	raw, err := r.catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: r.workspace})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}

func (r *imageRig) write(t *testing.T, name string, data []byte) {
	t.Helper()
	path := filepath.Join(r.workspace, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

const imageQuestion = `"questions":[{"name":"billing","instructions":"Is this a billing problem?"}]`

func requireOK(t *testing.T, output string) {
	t.Helper()
	if !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("not ok:\n%s", output)
	}
}

func requireRefused(t *testing.T, rig *imageRig, output, code string) {
	t.Helper()
	if !strings.Contains(output, code) || strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("want %s:\n%s", code, output)
	}
	if len(rig.requests) != 0 {
		t.Fatalf("a refused image still sent %d requests", len(rig.requests))
	}
}

func TestDecideImageFileGoesToOpenAIAsInputImage(t *testing.T) {
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	data := decidePNG(t, 8, 8)
	rig.write(t, "shots/a.png", data)
	requireOK(t, rig.call(t, `{"input":["Look at this:",{"type":"image","path":"shots/a.png"}],`+imageQuestion+`}`))
	if len(rig.requests) != 1 {
		t.Fatalf("%d requests", len(rig.requests))
	}
	messages := rig.requests[0]["input"].([]any)
	parts := messages[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %#v", parts)
	}
	if text := parts[0].(map[string]any); text["type"] != "input_text" || text["text"] != "Look at this:" {
		t.Fatalf("text part = %#v", text)
	}
	picture := parts[1].(map[string]any)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	if picture["type"] != "input_image" || picture["image_url"] != want {
		t.Fatalf("image part = %#v", picture)
	}
}

func TestDecideImageGoesToOpenRouterAsImageURLInState(t *testing.T) {
	rig := newImageRig(t, "openrouter", DecideImages{}, nil)
	data := decidePNG(t, 8, 8)
	rig.write(t, "a.png", data)
	requireOK(t, rig.call(t, `{"input":["note",{"type":"json","value":{"k":1}},{"type":"image","path":"a.png"}],`+imageQuestion+`}`))
	state := rig.requests[0]["state"].([]any)
	if len(state) != 3 || state[0] != "note" {
		t.Fatalf("state = %#v", state)
	}
	if object, ok := state[1].(map[string]any); !ok || object["k"] != float64(1) {
		t.Fatalf("json part = %#v", state[1])
	}
	picture := state[2].(map[string]any)
	url := picture["image_url"].(map[string]any)["url"]
	if picture["type"] != "image_url" || url != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("image part = %#v", picture)
	}
}

func TestDecideImageOnlyInputIsNotSentAsText(t *testing.T) {
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	rig.write(t, "a.png", decidePNG(t, 8, 8))
	requireOK(t, rig.call(t, `{"input":[{"type":"image","path":"a.png"}],`+imageQuestion+`}`))
	if _, isString := rig.requests[0]["input"].(string); isString {
		t.Fatal("an image-only input was sent as a string")
	}
}

func TestDecideImageDataAcceptsBase64DataURLAndWhitespace(t *testing.T) {
	data := decidePNG(t, 8, 8)
	encoded := base64.StdEncoding.EncodeToString(data)
	want := "data:image/png;base64," + encoded
	forms := map[string]string{
		"base64":        encoded,
		"data url":      want,
		"with newlines": encoded[:20] + "\\n" + encoded[20:],
		"unpadded":      strings.TrimRight(encoded, "="),
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			rig := newImageRig(t, "openai", DecideImages{}, nil)
			requireOK(t, rig.call(t, `{"input":[{"type":"image","data":"`+form+`"}],`+imageQuestion+`}`))
			parts := rig.requests[0]["input"].([]any)[0].(map[string]any)["content"].([]any)
			if got := parts[0].(map[string]any)["image_url"]; got != want {
				t.Fatalf("sent %v", got)
			}
		})
	}
}

func TestDecideImageTypeComesFromTheBytesNotTheName(t *testing.T) {
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	rig.write(t, "photo.txt", decidePNG(t, 8, 8))
	requireOK(t, rig.call(t, `{"input":[{"type":"image","path":"photo.txt"}],`+imageQuestion+`}`))

	rig = newImageRig(t, "openai", DecideImages{}, nil)
	rig.write(t, "fake.png", []byte("this is only text"))
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","path":"fake.png"}],`+imageQuestion+`}`), "unsupported_image")
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","data":"`+base64.StdEncoding.EncodeToString([]byte("<svg></svg>"))+`"}],`+imageQuestion+`}`), "unsupported_image")
}

func TestDecideImagePartNeedsExactlyOneOfPathAndData(t *testing.T) {
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	rig.write(t, "a.png", decidePNG(t, 8, 8))
	for name, part := range map[string]string{
		"neither": `{"type":"image"}`,
		"both":    `{"type":"image","path":"a.png","data":"AAAA"}`,
		"blank":   `{"type":"image","path":"   "}`,
	} {
		output := rig.call(t, `{"input":[`+part+`],`+imageQuestion+`}`)
		if strings.Contains(output, `"status":"ok"`) || len(rig.requests) != 0 {
			t.Fatalf("%s accepted:\n%s", name, output)
		}
	}
}

func TestDecideImageTypeIsCaseInsensitive(t *testing.T) {
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	rig.write(t, "a.png", decidePNG(t, 8, 8))
	requireOK(t, rig.call(t, `{"input":[{"type":" IMAGE ","path":"a.png"}],`+imageQuestion+`}`))
}

func TestDecideImagePathStaysInTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.png"), decidePNG(t, 8, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	if err := os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(rig.workspace, "link.png")); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"absolute outside": filepath.Join(outside, "secret.png"),
		"dot dot":          "../" + filepath.Base(outside) + "/secret.png",
		"symlink out":      "link.png",
	} {
		before := len(rig.requests)
		output := rig.call(t, `{"input":[{"type":"image","path":`+quote(path)+`}],`+imageQuestion+`}`)
		if strings.Contains(output, `"status":"ok"`) || len(rig.requests) != before {
			t.Fatalf("%s was read:\n%s", name, output)
		}
	}
}

func TestDecideImagePathHonoursItsOwnDenyRules(t *testing.T) {
	workspace := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(workspace, "private")}}}
	rig := newImageRig(t, "openai", DecideImages{}, func(s *sessionfs.NativeDecideSettings) { s.Policy = policy })
	rig.workspace = workspace
	rig.write(t, "private/a.png", decidePNG(t, 8, 8))
	rig.write(t, "public/a.png", decidePNG(t, 8, 8))
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","path":"private/a.png"}],`+imageQuestion+`}`), "path_denied")
	requireOK(t, rig.call(t, `{"input":[{"type":"image","path":"public/a.png"}],`+imageQuestion+`}`))
}

func TestDecideImageMissingFileAndDirectory(t *testing.T) {
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	rig.write(t, "dir/x.png", decidePNG(t, 8, 8))
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","path":"nope.png"}],`+imageQuestion+`}`), "not_found")
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","path":"dir"}],`+imageQuestion+`}`), "not_a_file")
}

func TestDecideImageSizeLimits(t *testing.T) {
	big := decidePNG(t, 120, 120)
	if len(big) < 4000 {
		t.Fatalf("test image too small: %d", len(big))
	}
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	rig.write(t, "big.png", big)
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","path":"big.png"}],`+imageQuestion+`}`), "image_too_large")
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","data":"`+base64.StdEncoding.EncodeToString(big)+`"}],`+imageQuestion+`}`), "image_too_large")
	// A string far over the limit is refused from its length, before it is decoded.
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","data":"`+strings.Repeat("A", 100000)+`"}],`+imageQuestion+`}`), "image_too_large")
}

func TestDecideImageCountLimitAndTextLimitAreSeparate(t *testing.T) {
	rig := newImageRig(t, "openai", DecideImages{}, nil)
	rig.write(t, "a.png", decidePNG(t, 8, 8))
	three := `{"type":"image","path":"a.png"},{"type":"image","path":"a.png"},{"type":"image","path":"a.png"}`
	requireRefused(t, rig, rig.call(t, `{"input":[`+three+`],`+imageQuestion+`}`), "too_many_images")
	// Two images are within max_images, and the text limit is 1000 bytes while one image is not text.
	requireOK(t, rig.call(t, `{"input":["x",{"type":"image","path":"a.png"},{"type":"image","path":"a.png"}],`+imageQuestion+`}`))
	requireRefused(t, newImageRig(t, "openai", DecideImages{}, nil), rig.call(t, `{"input":["`+strings.Repeat("x", 1001)+`",{"type":"image","path":"a.png"}],`+imageQuestion+`}`), "input_too_large")
}

func TestDecideImageReadsStoredReference(t *testing.T) {
	dir := t.TempDir()
	store := imagestore.New(imagestore.Options{Dir: dir, MinBytes: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 24})
	data := decidePNG(t, 8, 8)
	item := []byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(data) + `"}]}`)
	stored, stats := store.Externalize(item)
	names := imagestore.RefNames(stored)
	if stats.Stored != 1 || len(names) != 1 {
		t.Fatalf("setup: stats %+v names %v", stats, names)
	}
	rig := newImageRig(t, "openai", DecideImages{Store: store}, nil)
	requireOK(t, rig.call(t, `{"input":[{"type":"image","path":"stored-image://`+names[0]+`"}],`+imageQuestion+`}`))
	parts := rig.requests[0]["input"].([]any)[0].(map[string]any)["content"].([]any)
	if got := parts[0].(map[string]any)["image_url"]; got != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("sent %v", got)
	}

	rig.requests = nil
	missing := "stored-image://" + strings.Repeat("0", 64) + ".png"
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","path":"`+missing+`"}],`+imageQuestion+`}`), "image_not_found")
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","path":"stored-image://../../etc/passwd"}],`+imageQuestion+`}`), "image_not_found")

	off := newImageRig(t, "openai", DecideImages{}, nil)
	requireRefused(t, off, off.call(t, `{"input":[{"type":"image","path":"stored-image://`+names[0]+`"}],`+imageQuestion+`}`), "image_not_found")
}

func TestDecideImageIsResizedLikeANativeImage(t *testing.T) {
	big := decidePNG(t, 400, 300)
	options := imageresize.Options{MaxEdgePx: 100, MaxBytes: 1 << 20, MaxPixels: 1 << 26, JPEGQuality: 85, CacheEntries: 4}
	sentWidth := func(t *testing.T, appliesTo []string) int {
		t.Helper()
		var resizer *imageresize.Resizer
		if appliesTo != nil {
			resizer = imageresize.New(options, appliesTo)
		}
		rig := newImageRig(t, "openai", DecideImages{Resizer: resizer}, func(s *sessionfs.NativeDecideSettings) { s.MaxImageBytes = 1 << 20 })
		rig.write(t, "big.png", big)
		requireOK(t, rig.call(t, `{"input":[{"type":"image","path":"big.png"}],`+imageQuestion+`}`))
		url := rig.requests[0]["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["image_url"].(string)
		_, encoded, _ := strings.Cut(url, ",")
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		return config.Width
	}
	if width := sentWidth(t, []string{"native"}); width != 100 {
		t.Fatalf("native resize sent width %d, want 100", width)
	}
	if width := sentWidth(t, []string{"mcp"}); width != 400 {
		t.Fatalf("mcp-only resize changed a decide image: width %d", width)
	}
	if width := sentWidth(t, nil); width != 400 {
		t.Fatalf("no resizer changed the image: width %d", width)
	}
}

// The size limit applies to what was read, so an image that is resized to fit is still
// refused when the file itself is over the limit: the limit protects memory, not the
// provider request.
func TestDecideImageFileOverTheLimitIsRefusedEvenIfResizeWouldShrinkIt(t *testing.T) {
	big := decidePNG(t, 400, 300)
	options := imageresize.Options{MaxEdgePx: 50, MaxBytes: 1 << 20, MaxPixels: 1 << 26, JPEGQuality: 85, CacheEntries: 4}
	rig := newImageRig(t, "openai", DecideImages{Resizer: imageresize.New(options, []string{"native"})}, func(s *sessionfs.NativeDecideSettings) { s.MaxImageBytes = int64(len(big)) - 1 })
	rig.write(t, "big.png", big)
	requireRefused(t, rig, rig.call(t, `{"input":[{"type":"image","path":"big.png"}],`+imageQuestion+`}`), "image_too_large")
}

func quote(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}

// BuildCatalog must give decide its own path policy and the session's image store, and
// must not lend it the read tool's policy: read's deny rule must not apply to decide.
func TestBuildCatalogWiresOwnPolicyAndImageStoreIntoDecide(t *testing.T) {
	workspace := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	var received int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIReply))
	}))
	t.Cleanup(server.Close)

	sample := filepath.Join("..", "..", "..", "..", "sample", "tools", "native")
	storeDir := filepath.Join(t.TempDir(), "images")
	store := imagestore.New(imagestore.Options{Dir: storeDir, MinBytes: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 24})
	data := decidePNG(t, 8, 8)
	stored, _ := store.Externalize([]byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(data) + `"}]}`))
	names := imagestore.RefNames(stored)
	if len(names) != 1 {
		t.Fatalf("setup: %v", names)
	}
	for _, name := range []string{"private/a.png", "public/a.png"} {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(workspace, "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join(sample, "decide.json"), filepath.Join(sample, "read.json")},
		NativeRead:          &sessionfs.NativeReadSettings{Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(workspace, "public")}}}, MaxBytes: 1000, MaxFileBytes: 1000, DefaultLimit: 10},
		NativeDecide:        &sessionfs.NativeDecideSettings{Backend: "openai", BaseURL: server.URL, APIKeyEnv: "PATH", Model: "m", TimeoutMS: 5000, MaxQuestions: 4, MaxInputBytes: 1000, MaxResponseBytes: 4096, MaxImages: 2, MaxImageBytes: 4000, Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(workspace, "private")}}}},
		ImageStore:          sessionfs.ImageStoreSettings{Dir: storeDir, MinBytes: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) string {
		arguments := `{"input":[{"type":"image","path":` + quote(path) + `}],` + imageQuestion + `}`
		raw, err := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": "decide", "arguments": arguments})
		if err != nil {
			t.Fatal(err)
		}
		result, err := catalog.ExecuteFunctionCall(context.Background(), raw, tools.ExecutionContext{WorkspaceRoot: workspace})
		if err != nil {
			t.Fatal(err)
		}
		return string(result)
	}
	if output := call("private/a.png"); !strings.Contains(output, "path_denied") || received != 0 {
		t.Fatalf("a path decide denies was read (requests %d):\n%s", received, output)
	}
	if output := call("public/a.png"); !strings.Contains(output, `status\":\"ok`) && !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("a path only the read tool denies was refused for decide:\n%s", output)
	}
	if output := call("stored-image://" + names[0]); !strings.Contains(output, `status\":\"ok`) && !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("a stored image was refused:\n%s", output)
	}
	if received != 2 {
		t.Fatalf("%d requests, want 2", received)
	}
}

// A source over the limit must stop being read one byte past it, whatever its size.
func TestReadWithinStopsOneByteOverTheLimit(t *testing.T) {
	endless := &countingReader{}
	data, within, err := readWithin(endless, 1000)
	if err != nil || within || len(data) != 1001 || endless.read != 1001 {
		t.Fatalf("within=%v len=%d read=%d err=%v", within, len(data), endless.read, err)
	}
	exact, within, err := readWithin(strings.NewReader(strings.Repeat("a", 1000)), 1000)
	if err != nil || !within || len(exact) != 1000 {
		t.Fatalf("a source of exactly the limit: within=%v len=%d err=%v", within, len(exact), err)
	}
}

type countingReader struct{ read int }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	c.read += len(p)
	return len(p), nil
}

// A file over the limit is refused by the read itself, with no request sent.
func TestDecideImageFileOverTheLimitIsRefusedByTheRead(t *testing.T) {
	big := decidePNG(t, 120, 120)
	rig := newImageRig(t, "openai", DecideImages{}, func(s *sessionfs.NativeDecideSettings) { s.MaxImageBytes = int64(len(big)) - 1 })
	rig.write(t, "grown.png", big)
	data, failure := readDecideImageFile("grown.png", DecideImages{}, sessionfs.NativeDecideSettings{MaxImageBytes: int64(len(big)) - 1}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: rig.workspace}, "input part 1")
	if failure == "" || data != nil || !strings.Contains(failure, "image_too_large") {
		t.Fatalf("data %d bytes, failure %q", len(data), failure)
	}
}

// The base64 text is judged by its length first, so a very long string is not decoded.
func TestDecideImageDataIsRefusedFromItsLengthBeforeDecoding(t *testing.T) {
	// Not valid base64 and far over the limit: the length check must answer first, so the
	// reason is the size and not an invalid-data message.
	_, failure := decodeDecideImageData(strings.Repeat("!", 100000), sessionfs.NativeDecideSettings{MaxImageBytes: 1000}, "c", "input part 1")
	if !strings.Contains(failure, "image_too_large") {
		t.Fatalf("failure = %q", failure)
	}
}
