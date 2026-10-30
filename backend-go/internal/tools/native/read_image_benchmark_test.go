package native

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// BenchmarkReadImageStoredTooLarge reads a stored reference whose object is over the tool's
// limit. The check must refuse it before the whole object is read into memory.
func BenchmarkReadImageStoredTooLarge(b *testing.B) {
	dir := b.TempDir()
	store := imagestore.New(imagestore.Options{Dir: dir, MinBytes: 1, MaxFileBytes: 128 << 20, MaxTotalBytes: 256 << 20})
	var small bytes.Buffer
	if err := png.Encode(&small, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		b.Fatal(err)
	}
	item := []byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(small.Bytes()) + `"}]}`)
	stored, _ := store.Externalize(item)
	names := imagestore.RefNames(stored)
	if len(names) != 1 {
		b.Fatalf("setup: names %v", names)
	}
	// Grow the stored object past the tool limit. The PNG header stays first, so the content
	// type still matches the name.
	file, err := os.OpenFile(filepath.Join(dir, names[0]), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		b.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	tool, err := NewReadImageTool(sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20}, store)
	if err != nil {
		b.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		b.Fatal(err)
	}
	call, err := json.Marshal(map[string]any{"type": "function_call", "call_id": "call-1", "name": "read_image", "arguments": `{"path":"stored-image://` + names[0] + `"}`})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{CallID: "call-1", WorkspaceRoot: b.TempDir()}); err != nil {
			b.Fatal(err)
		}
	}
}
