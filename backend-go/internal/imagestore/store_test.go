package imagestore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func noisyPNG(t testing.TB, size int, seed int64) []byte {
	t.Helper()
	random := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, color.RGBA{uint8(random.Intn(256)), uint8(random.Intn(256)), uint8(random.Intn(256)), 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func newStore(t *testing.T, mutate func(*Options)) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "images")
	options := Options{Dir: dir, MinBytes: 1000, MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20}
	if mutate != nil {
		mutate(&options)
	}
	return New(options), dir
}

func toolOutput(callID string, parts ...string) []byte {
	return []byte(`{"type":"function_call_output","call_id":"` + callID + `","output":[` + strings.Join(parts, ",") + `]}`)
}

func imagePart(url string) string {
	return `{"type":"input_image","image_url":"` + url + `","detail":"auto"}`
}

func TestRoundTripRestoresTheExactItem(t *testing.T) {
	store, dir := newStore(t, nil)
	original := toolOutput("c1", `{"type":"input_text","text":"before <b> & \"quoted\""}`, imagePart(dataURL("image/png", noisyPNG(t, 60, 1))), `{"type":"input_text","text":"after"}`)
	stored, stats := store.Externalize(original)
	if stats.Stored != 1 || len(stored) >= len(original)/4 {
		t.Fatalf("stats %+v, stored %d bytes vs %d", stats, len(stored), len(original))
	}
	if !bytes.Contains(stored, []byte(RefPrefix)) || bytes.Contains(stored, []byte("base64")) {
		t.Fatalf("stored item = %s", stored)
	}
	if !json.Valid(stored) {
		t.Fatalf("stored item is not JSON: %s", stored)
	}
	restored, restoredStats := store.Rehydrate([][]byte{stored})
	if restoredStats.Restored != 1 || restoredStats.Missing != 0 || !bytes.Equal(restored[0], original) {
		t.Fatalf("restored %+v differs from the original:\n%s\n%s", restoredStats, restored[0], original)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want 1", len(entries))
	}
}

func TestNilStoreAndEmptyDirDoNothing(t *testing.T) {
	if New(Options{}) != nil {
		t.Fatal("an empty directory must give a nil store")
	}
	var store *Store
	item := toolOutput("c1", imagePart(dataURL("image/png", noisyPNG(t, 60, 1))))
	if out, stats := store.Externalize(item); !bytes.Equal(out, item) || stats.Stored != 0 {
		t.Fatal("nil store changed an item")
	}
	history := [][]byte{item}
	if out, _ := store.Rehydrate(history); &out[0] != &history[0] {
		t.Fatal("nil store must return the history itself")
	}
	if out, _ := store.ExternalizeAll([]json.RawMessage{item}); !bytes.Equal(out[0], item) {
		t.Fatal("nil store changed outputs")
	}
}

func TestSameImageIsStoredOnceAndByContent(t *testing.T) {
	store, dir := newStore(t, nil)
	png := noisyPNG(t, 60, 2)
	first, _ := store.Externalize(toolOutput("a", imagePart(dataURL("image/png", png))))
	second, _ := store.Externalize(toolOutput("b", imagePart(dataURL("image/png", png))))
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("%d files for one image", len(entries))
	}
	name := entries[0].Name()
	if _, ok := ValidName(name); !ok || !bytes.Contains(first, []byte(name)) || !bytes.Contains(second, []byte(name)) {
		t.Fatalf("file %q, items %s %s", name, first, second)
	}
}

func TestSeveralImagesInOneItemAndInOneHistory(t *testing.T) {
	store, _ := newStore(t, nil)
	a, b := dataURL("image/png", noisyPNG(t, 60, 3)), dataURL("image/png", noisyPNG(t, 61, 4))
	item := toolOutput("c", imagePart(a), imagePart(b), imagePart(a))
	stored, stats := store.Externalize(item)
	if stats.Stored != 3 {
		t.Fatalf("stored %d, want 3", stats.Stored)
	}
	plain := []byte(`{"type":"function_call","call_id":"c","name":"x","arguments":"{}"}`)
	history := [][]byte{plain, stored, plain}
	restored, restoredStats := store.Rehydrate(history)
	if restoredStats.Restored != 3 || !bytes.Equal(restored[1], item) {
		t.Fatalf("restored %+v", restoredStats)
	}
	if &restored[0] == &history[0] {
		t.Fatal("a changed history must be a new slice")
	}
	if !bytes.Equal(history[1], stored) {
		t.Fatal("the input history was modified")
	}
	if &restored[0] == nil || !bytes.Equal(restored[0], plain) || !bytes.Equal(restored[2], plain) {
		t.Fatal("unchanged items must be kept")
	}
}

func TestImagesThatStayInline(t *testing.T) {
	big := noisyPNG(t, 60, 5)
	cases := map[string]struct {
		mutate func(*Options)
		url    string
	}{
		"below min_bytes":       {func(o *Options) { o.MinBytes = int64(len(big)) + 1 }, dataURL("image/png", big)},
		"above max_file_bytes":  {func(o *Options) { o.MaxFileBytes = int64(len(big)) - 1 }, dataURL("image/png", big)},
		"svg":                   {nil, dataURL("image/svg+xml", []byte(strings.Repeat("<svg/>", 400)))},
		"not base64":            {nil, "data:image/png;charset=utf-8,hello"},
		"invalid base64":        {nil, "data:image/png;base64,!!!!" + strings.Repeat("A", 2000)},
		"declared jpeg is png":  {nil, dataURL("image/jpeg", big)},
		"declared png is bytes": {nil, dataURL("image/png", bytes.Repeat([]byte("x"), 3000))},
		"http url":              {nil, "https://example.com/a.png"},
	}
	for name, test := range cases {
		store, dir := newStore(t, test.mutate)
		item := toolOutput("c", imagePart(test.url))
		out, stats := store.Externalize(item)
		if stats.Stored != 0 || !bytes.Equal(out, item) {
			t.Errorf("%s: item changed (%+v)", name, stats)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: %d files written", name, len(entries))
		}
	}
}

func TestMaxTotalBytesKeepsLaterImagesInlineButNotKnownOnes(t *testing.T) {
	first, second := noisyPNG(t, 60, 6), noisyPNG(t, 61, 7)
	store, dir := newStore(t, func(o *Options) { o.MaxTotalBytes = int64(len(first)) + 10 })
	storedFirst, a := store.Externalize(toolOutput("a", imagePart(dataURL("image/png", first))))
	_, b := store.Externalize(toolOutput("b", imagePart(dataURL("image/png", second))))
	again, c := store.Externalize(toolOutput("c", imagePart(dataURL("image/png", first))))
	if a.Stored != 1 || b.Stored != 0 || c.Stored != 1 || !bytes.Contains(again, []byte(RefPrefix)) {
		t.Fatalf("stored %d %d %d", a.Stored, b.Stored, c.Stored)
	}
	_ = storedFirst
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%d files, want 1", len(entries))
	}
}

func TestTotalIsReadFromTheDirectoryOfAnEarlierRun(t *testing.T) {
	first, second := noisyPNG(t, 60, 8), noisyPNG(t, 61, 9)
	dir := filepath.Join(t.TempDir(), "images")
	options := Options{Dir: dir, MinBytes: 100, MaxFileBytes: 1 << 20, MaxTotalBytes: int64(len(first)) + 10}
	New(options).Externalize(toolOutput("a", imagePart(dataURL("image/png", first))))
	_, stats := New(options).Externalize(toolOutput("b", imagePart(dataURL("image/png", second))))
	if stats.Stored != 0 {
		t.Fatal("a new store must count the files already in the directory")
	}
}

func TestMissingAndDamagedFilesBecomeTextNotErrors(t *testing.T) {
	store, dir := newStore(t, nil)
	item := toolOutput("c", `{"type":"input_text","text":"hi"}`, imagePart(dataURL("image/png", noisyPNG(t, 60, 10))))
	stored, _ := store.Externalize(item)
	entries, _ := os.ReadDir(dir)
	path := filepath.Join(dir, entries[0].Name())
	check := func(label string) {
		t.Helper()
		out, stats := store.Rehydrate([][]byte{stored})
		if stats.Missing != 1 || stats.Restored != 0 || !json.Valid(out[0]) {
			t.Fatalf("%s: stats %+v item %s", label, stats, out[0])
		}
		var parsed struct {
			Output []map[string]any `json:"output"`
		}
		if err := json.Unmarshal(out[0], &parsed); err != nil || len(parsed.Output) != 2 || parsed.Output[1]["type"] != "input_text" || parsed.Output[1]["text"] != "[stored image unavailable]" {
			t.Fatalf("%s: output = %s (%v)", label, out[0], err)
		}
	}
	damaged := []byte("not a png at all")
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	check("damaged")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check("missing")
}

func TestUnavailableValueInAnUnexpectedShapeStaysValidJSON(t *testing.T) {
	store, _ := newStore(t, nil)
	name := strings.Repeat("a", 64) + ".png"
	item := []byte(`{"type":"message","role":"user","content":[{"detail":"auto","type":"input_image","image_url":"` + RefPrefix + name + `"}]}`)
	out, stats := store.Rehydrate([][]byte{item})
	if stats.Missing != 1 || !json.Valid(out[0]) {
		t.Fatalf("stats %+v item %s", stats, out[0])
	}
}

func TestOpenRefusesBadNames(t *testing.T) {
	store, dir := newStore(t, nil)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(dir), "secret.png")
	if err := os.WriteFile(outside, noisyPNG(t, 40, 11), 0o600); err != nil {
		t.Fatal(err)
	}
	hex := strings.Repeat("0", 64)
	for _, name := range []string{
		"", "..", "../secret.png", "../images/" + hex + ".png", hex, hex + ".svg", hex + ".PNG", hex + ".png/", hex + ".png.png",
		strings.Repeat("A", 64) + ".png", hex[:63] + ".png", hex + "x.png", "/etc/passwd", hex + ".png\x00", strings.Repeat("g", 64) + ".png",
	} {
		if _, _, err := store.Open(name); err == nil {
			t.Errorf("Open(%q) succeeded", name)
		}
		if _, ok := ValidName(name); ok {
			t.Errorf("ValidName(%q) = true", name)
		}
	}
	// A symlink in the directory pointing at a file outside is not a regular file.
	link := filepath.Join(dir, hex+".png")
	if err := os.Symlink(outside, link); err == nil {
		if _, _, err := store.Open(hex + ".png"); err == nil {
			t.Error("Open followed a symlink to a file outside the directory")
		}
	}
}

func TestOpenChecksTheContentAgainstTheName(t *testing.T) {
	store, dir := newStore(t, nil)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("b", 64) + ".png"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("<html><script>alert(1)</script></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Open(name); err == nil {
		t.Fatal("Open served html under a png name")
	}
}

func TestFilesAreOwnerOnlyAndNoTempFilesRemain(t *testing.T) {
	store, dir := newStore(t, nil)
	store.Externalize(toolOutput("c", imagePart(dataURL("image/png", noisyPNG(t, 60, 12)))))
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		info, _ := entry.Info()
		if strings.HasPrefix(entry.Name(), ".tmp") || info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", entry.Name(), info.Mode())
		}
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", info.Mode().Perm())
	}
}

func TestUnwritableDirectoryKeepsTheImageInline(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(Options{Dir: filepath.Join(blocker, "images"), MinBytes: 100, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 30})
	item := toolOutput("c", imagePart(dataURL("image/png", noisyPNG(t, 60, 13))))
	if out, stats := store.Externalize(item); stats.Stored != 0 || !bytes.Equal(out, item) {
		t.Fatal("a failed write must keep the image inline")
	}
}

func TestOtherFieldsWithTheAnchorTextAreNotTouched(t *testing.T) {
	store, _ := newStore(t, nil)
	text := `the model wrote "image_url":"data:image/png;base64,AAAA" in a message`
	encoded, _ := json.Marshal(text)
	item := []byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":` + string(encoded) + `}]}`)
	if out, stats := store.Externalize(item); stats.Stored != 0 || !bytes.Equal(out, item) {
		t.Fatalf("text content was changed: %s", out)
	}
	refText, _ := json.Marshal(`"image_url":"` + RefPrefix + strings.Repeat("a", 64) + `.png"`)
	item = []byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":` + string(refText) + `}]}`)
	if out, stats := store.Rehydrate([][]byte{item}); stats.Missing != 0 || stats.Restored != 0 || !bytes.Equal(out[0], item) {
		t.Fatalf("text content was changed on the way back: %s", out[0])
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	store, dir := newStore(t, nil)
	images := [][]byte{noisyPNG(t, 60, 14), noisyPNG(t, 61, 15), noisyPNG(t, 62, 16)}
	var wait sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			item := toolOutput("c", imagePart(dataURL("image/png", images[worker%3])))
			stored, stats := store.Externalize(item)
			if stats.Stored != 1 {
				t.Errorf("worker %d stored %d", worker, stats.Stored)
				return
			}
			if restored, _ := store.Rehydrate([][]byte{stored}); !bytes.Equal(restored[0], item) {
				t.Errorf("worker %d round trip differs", worker)
			}
		}(worker)
	}
	wait.Wait()
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Fatalf("%d files, want 3", len(entries))
	}
}

func TestTotalCountsEveryImageWrittenByThisStore(t *testing.T) {
	a, b := noisyPNG(t, 60, 20), noisyPNG(t, 61, 21)
	store, dir := newStore(t, func(o *Options) { o.MaxTotalBytes = int64(len(a)+len(b)) - 1 })
	_, first := store.Externalize(toolOutput("a", imagePart(dataURL("image/png", a))))
	_, second := store.Externalize(toolOutput("b", imagePart(dataURL("image/png", b))))
	if first.Stored != 1 || second.Stored != 0 {
		t.Fatalf("stored %d then %d: the second image fits alone but not together with the first", first.Stored, second.Stored)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%d files, want 1", len(entries))
	}
}

func TestDirectorySizeCountsStoredFilesOnly(t *testing.T) {
	dir := t.TempDir()
	hex := strings.Repeat("c", 64)
	for name, size := range map[string]int{hex + ".png": 300, ".tmp-123": 5000, "notes.txt": 5000, strings.Repeat("d", 64) + ".svg": 5000} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, strings.Repeat("e", 64)+".png"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := directorySize(dir); got != 300 {
		t.Fatalf("directorySize = %d, want 300 (only the valid stored file)", got)
	}
	if got := directorySize(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("missing directory = %d, want 0", got)
	}
}

func TestOpenRefusesALinkEvenWhenTheTargetIsAValidImage(t *testing.T) {
	store, dir := newStore(t, nil)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "real.png")
	if err := os.WriteFile(outside, noisyPNG(t, 40, 22), 0o600); err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("f", 64) + ".png"
	if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, _, err := store.Open(name); err == nil {
		t.Fatal("Open served a file through a symlink")
	}
}
