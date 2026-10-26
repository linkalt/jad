package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type readImageRig struct {
	catalog   *tools.Catalog
	workspace string
}

func newReadImageRig(t *testing.T, settings sessionfs.NativeReadImageSettings, store *imagestore.Store) *readImageRig {
	t.Helper()
	tool, err := NewReadImageTool(settings, store)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	return &readImageRig{catalog: catalog, workspace: t.TempDir()}
}

func (r *readImageRig) write(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.workspace, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// call returns the output value of the function_call_output the model would receive.
func (r *readImageRig) call(t *testing.T, arguments string) json.RawMessage {
	t.Helper()
	raw, err := r.catalog.ExecuteFunctionCall(context.Background(),
		json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"read_image","arguments":`+quoteJSON(arguments)+`}`),
		tools.ExecutionContext{CallID: "call-1", WorkspaceRoot: r.workspace})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || item.Type != "function_call_output" || item.CallID != "call-1" {
		t.Fatalf("function_call_output = %s, err=%v", raw, err)
	}
	return item.Output
}

func quoteJSON(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}

// requireImagePart checks that the output is exactly one input_image part and returns its decoded bytes.
func requireImagePart(t *testing.T, output json.RawMessage) (string, []byte) {
	t.Helper()
	var parts []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(output, &parts); err != nil || len(parts) != 1 || parts[0].Type != "input_image" || parts[0].Detail != "auto" {
		t.Fatalf("output is not one input_image part: %s (err=%v)", output, err)
	}
	header, payload, found := strings.Cut(parts[0].ImageURL, ";base64,")
	if !found {
		t.Fatalf("image_url is not a base64 data URL: %.60s", parts[0].ImageURL)
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(header, "data:"), data
}

// requireReadImageRefused checks the output is a text result with the status and code.
func requireReadImageRefused(t *testing.T, output json.RawMessage, status, code string) {
	t.Helper()
	var text string
	if err := json.Unmarshal(output, &text); err != nil {
		t.Fatalf("a refusal must be a text result, got %s", output)
	}
	if !strings.Contains(text, status) || !strings.Contains(text, code) {
		t.Fatalf("refusal %q lacks status %q or code %q", text, status, code)
	}
}

var defaultReadImageSettings = sessionfs.NativeReadImageSettings{MaxImageBytes: 5 * 1024 * 1024}

func TestReadImageSmallestCallReturnsOneImagePart(t *testing.T) {
	rig := newReadImageRig(t, defaultReadImageSettings, nil)
	picture := decideSystemonePNG(t, 8, 8)
	rig.write(t, "shot.png", picture)
	mime, data := requireImagePart(t, rig.call(t, `{"path":"shot.png"}`))
	if mime != "image/png" || string(data) != string(picture) {
		t.Fatalf("mime %q, bytes equal %t", mime, string(data) == string(picture))
	}
}

func TestReadImagePathIsTrimmed(t *testing.T) {
	rig := newReadImageRig(t, defaultReadImageSettings, nil)
	rig.write(t, "shot.png", decideSystemonePNG(t, 8, 8))
	requireImagePart(t, rig.call(t, `{"path":"  shot.png\n"}`))
}

func TestReadImageTypeComesFromTheBytesNotTheName(t *testing.T) {
	rig := newReadImageRig(t, defaultReadImageSettings, nil)
	rig.write(t, "really-a-png.txt", decideSystemonePNG(t, 8, 8))
	rig.write(t, "fake.png", []byte("this is plain text, not an image"))
	mime, _ := requireImagePart(t, rig.call(t, `{"path":"really-a-png.txt"}`))
	if mime != "image/png" {
		t.Fatalf("mime = %q", mime)
	}
	requireReadImageRefused(t, rig.call(t, `{"path":"fake.png"}`), "invalid_input", "unsupported_image")
}

func TestReadImageRefusesWhatIsNotAnImageFile(t *testing.T) {
	rig := newReadImageRig(t, defaultReadImageSettings, nil)
	if err := os.Mkdir(filepath.Join(rig.workspace, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	requireReadImageRefused(t, rig.call(t, `{"path":"missing.png"}`), "not_found", "not_found")
	requireReadImageRefused(t, rig.call(t, `{"path":"dir"}`), "invalid_input", "not_a_file")
}

func TestReadImageHonoursTheSizeLimit(t *testing.T) {
	picture := decideSystemonePNG(t, 16, 16)
	rig := newReadImageRig(t, sessionfs.NativeReadImageSettings{MaxImageBytes: int64(len(picture)) - 1}, nil)
	rig.write(t, "big.png", picture)
	requireReadImageRefused(t, rig.call(t, `{"path":"big.png"}`), "invalid_input", "image_too_large")

	exact := newReadImageRig(t, sessionfs.NativeReadImageSettings{MaxImageBytes: int64(len(picture))}, nil)
	exact.write(t, "big.png", picture)
	requireImagePart(t, exact.call(t, `{"path":"big.png"}`))
}

func TestReadImageUsesTheReadPathPolicy(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.png"), decideSystemonePNG(t, 8, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	path := quoteJSON(filepath.Join(outside, "secret.png"))

	closed := newReadImageRig(t, defaultReadImageSettings, nil)
	var refused string
	if err := json.Unmarshal(closed.call(t, `{"path":`+path+`}`), &refused); err != nil || !strings.Contains(refused, "forbidden") {
		t.Fatalf("a path outside the workspace was not refused: %q (err=%v)", refused, err)
	}

	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "allow", Path: outside}}}
	open := newReadImageRig(t, sessionfs.NativeReadImageSettings{Policy: policy, MaxImageBytes: 1 << 20}, nil)
	requireImagePart(t, open.call(t, `{"path":`+path+`}`))
}

func TestReadImageOpensAStoredReference(t *testing.T) {
	store := imagestore.New(imagestore.Options{Dir: t.TempDir(), MinBytes: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 24})
	picture := decideSystemonePNG(t, 8, 8)
	item := []byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(picture) + `"}]}`)
	stored, _ := store.Externalize(item)
	names := imagestore.RefNames(stored)
	if len(names) != 1 {
		t.Fatalf("setup: names %v", names)
	}
	rig := newReadImageRig(t, defaultReadImageSettings, store)
	_, data := requireImagePart(t, rig.call(t, `{"path":"stored-image://`+names[0]+`"}`))
	if string(data) != string(picture) {
		t.Fatal("the stored image came back different")
	}
	missing := "stored-image://" + strings.Repeat("0", 64) + ".png"
	requireReadImageRefused(t, rig.call(t, `{"path":"`+missing+`"}`), "not_found", "image_not_found")

	off := newReadImageRig(t, defaultReadImageSettings, nil)
	requireReadImageRefused(t, off.call(t, `{"path":"stored-image://`+names[0]+`"}`), "not_found", "image_not_found")
}

func TestReadImageRequiresAPath(t *testing.T) {
	rig := newReadImageRig(t, defaultReadImageSettings, nil)
	var text string
	if err := json.Unmarshal(rig.call(t, `{}`), &text); err != nil || !strings.Contains(text, "invalid_input") {
		t.Fatalf("a call without a path = %q (err=%v)", text, err)
	}
}

func TestReadImageToolIsReadOnlyAndDescribed(t *testing.T) {
	tool, err := NewReadImageTool(defaultReadImageSettings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !tool.ReadOnly || tool.Definition.ID != "native.read_image" || tool.Definition.Name != "read_image" {
		t.Fatalf("tool = %+v", tool.Definition)
	}
	if got := string(tool.Definition.InputSchema); !strings.Contains(got, `"required":["path"]`) {
		t.Fatalf("schema = %s", got)
	}
}
