package native

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
)

// A stored reference over the tool's limit is refused as image_too_large.
func TestReadImageStoredReferenceOverLimitIsRefused(t *testing.T) {
	dir := t.TempDir()
	store := imagestore.New(imagestore.Options{Dir: dir, MinBytes: 1, MaxFileBytes: 128 << 20, MaxTotalBytes: 256 << 20})
	var small bytes.Buffer
	if err := png.Encode(&small, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	item := []byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(small.Bytes()) + `"}]}`)
	stored, _ := store.Externalize(item)
	names := imagestore.RefNames(stored)
	if len(names) != 1 {
		t.Fatalf("setup: names %v", names)
	}
	file, err := os.OpenFile(filepath.Join(dir, names[0]), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	rig := newReadImageRig(t, sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20}, store)
	requireReadImageRefused(t, rig.call(t, `{"path":"stored-image://`+names[0]+`"}`), "invalid_input", "image_too_large")
}

// A refused stored object is not read in full: a 64 MiB object over a 5 MiB limit costs a few
// KiB, not 64 MiB.
func TestReadImageStoredReferenceOverLimitDoesNotReadTheObject(t *testing.T) {
	dir := t.TempDir()
	store := imagestore.New(imagestore.Options{Dir: dir, MinBytes: 1, MaxFileBytes: 128 << 20, MaxTotalBytes: 256 << 20})
	var small bytes.Buffer
	if err := png.Encode(&small, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	item := []byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(small.Bytes()) + `"}]}`)
	stored, _ := store.Externalize(item)
	names := imagestore.RefNames(stored)
	if len(names) != 1 {
		t.Fatalf("setup: names %v", names)
	}
	file, err := os.OpenFile(filepath.Join(dir, names[0]), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	rig := newReadImageRig(t, sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20}, store)
	call := `{"path":"stored-image://` + names[0] + `"}`
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rig.call(t, call)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("refusing a 64 MiB stored image allocated %d bytes", allocated)
	}
}
