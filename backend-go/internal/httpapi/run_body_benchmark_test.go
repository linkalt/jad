package httpapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// maxRunBody builds a request at the default image and file allowance: 8 images of 5 MiB and
// 4 PDFs of 10 MiB. The bytes after the magic number are zeros; only the magic is checked.
func maxRunBody(tb testing.TB, limits RunInputLimits) []byte {
	tb.Helper()
	image := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, limits.MaxImageBytes-8)...)
	file := append([]byte("%PDF-1.4\n"), make([]byte, limits.MaxFileBytes-9)...)
	imageText := base64.StdEncoding.EncodeToString(image)
	fileText := base64.StdEncoding.EncodeToString(file)
	var body strings.Builder
	body.WriteString(`{"input":[{"type":"text","text":"look"}`)
	for i := 0; i < limits.MaxImages; i++ {
		body.WriteString(`,{"type":"image","data":"` + imageText + `"}`)
	}
	for i := 0; i < limits.MaxFiles; i++ {
		body.WriteString(`,{"type":"file","name":"a.pdf","data":"` + fileText + `"}`)
	}
	body.WriteString(`]}`)
	return []byte(body.String())
}

var maxRunLimits = RunInputLimits{MaxImages: 8, MaxImageBytes: 5 << 20, MaxFiles: 4, MaxFileBytes: 10 << 20}

func TestReadRunBodyAcceptsTheMaximumRequest(t *testing.T) {
	payload := maxRunBody(t, maxRunLimits)
	h := &Handler{maxBodyBytes: 1 << 20, runInputLimits: maxRunLimits}
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
	body, err := readRunBody(httptest.NewRecorder(), request, h.runBodyLimit(), h.maxBodyBytes, maxRunLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body.Input, []byte(`"type":"input_file"`)) || !bytes.Contains(body.Input, []byte(`"type":"input_image"`)) {
		t.Fatalf("input lost its parts: %.200s", body.Input)
	}
	if got := strings.Count(string(body.Input), `"type":"input_`); got != 1+8+4 {
		t.Fatalf("parts = %d, want 13", got)
	}
}

func BenchmarkReadRunBodyMaximum(b *testing.B) {
	payload := maxRunBody(b, maxRunLimits)
	h := &Handler{maxBodyBytes: 1 << 20, runInputLimits: maxRunLimits}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
		if _, err := readRunBody(httptest.NewRecorder(), request, h.runBodyLimit(), h.maxBodyBytes, maxRunLimits); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReadRunBodyPeak reports the heap above the baseline while one maximum request is read.
func BenchmarkReadRunBodyPeak(b *testing.B) {
	payload := maxRunBody(b, maxRunLimits)
	h := &Handler{maxBodyBytes: 1 << 20, runInputLimits: maxRunLimits}
	var peak uint64
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)
		done, stopped := make(chan struct{}), make(chan uint64)
		go func() {
			var high uint64
			for {
				select {
				case <-done:
					stopped <- high
					return
				default:
				}
				time.Sleep(2 * time.Millisecond)
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > high {
					high = m.HeapAlloc
				}
			}
		}()
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
		if _, err := readRunBody(httptest.NewRecorder(), request, h.runBodyLimit(), h.maxBodyBytes, maxRunLimits); err != nil {
			b.Fatal(err)
		}
		close(done)
		if high := <-stopped; high > base.HeapAlloc && high-base.HeapAlloc > peak {
			peak = high - base.HeapAlloc
		}
	}
	b.ReportMetric(float64(peak)/(1<<20), "peakMiB")
}

func TestReadRunBodyRefusals(t *testing.T) {
	limits := RunInputLimits{MaxImages: 1, MaxImageBytes: 4096}
	for _, tc := range []struct {
		name, body string
		max        int64
		code       int
		message    string
	}{
		{"empty", ``, 1000, 400, "invalid JSON body"},
		{"truncated", `{"input":`, 1000, 400, "invalid JSON body"},
		{"garbage", `not json`, 1000, 400, "invalid JSON body"},
		{"two objects", `{"input":"a"}{"input":"b"}`, 1000, 400, "request body must contain one JSON object"},
		{"trailing text", `{"input":"a"} x`, 1000, 400, "request body must contain one JSON object"},
		{"number", `5`, 1000, 400, "input must be a non-empty string"},
		{"array", `[]`, 1000, 400, "input must be a non-empty string"},
		{"null", `null`, 1000, 400, "input must be a non-empty string"},
		{"no input", `{"model":"m"}`, 1000, 400, "input must be a non-empty string"},
		{"too large", `{"input":"` + strings.Repeat("a", 2000) + `"}`, 1000, 413, "request body is too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			_, err := readRunBody(httptest.NewRecorder(), request, tc.max, tc.max, limits)
			var status *StatusError
			if !errors.As(err, &status) || status.Code != tc.code || status.Message != tc.message {
				t.Fatalf("err = %v, want %d %q", err, tc.code, tc.message)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"input":"hi","model":" m ","supports_interaction":true}`))
	request.ContentLength = -1
	body, err := readRunBody(httptest.NewRecorder(), request, 1000, 1000, limits)
	if err != nil || string(body.Input) != `"hi"` || body.Model != "m" || !body.SupportsInteraction {
		t.Fatalf("%+v, %v", body, err)
	}
}

func TestReadRunBodyDoesNotReserveMemoryForADeclaredLength(t *testing.T) {
	limits := RunInputLimits{MaxImages: 8, MaxImageBytes: 5 << 20, MaxFiles: 4, MaxFileBytes: 10 << 20}
	h := &Handler{maxBodyBytes: 1 << 20, runInputLimits: limits}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"input":"hi"}`))
	request.ContentLength = h.runBodyLimit()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body, err := readRunBody(httptest.NewRecorder(), request, h.runBodyLimit(), h.maxBodyBytes, limits)
	runtime.ReadMemStats(&after)
	if err != nil || string(body.Input) != `"hi"` {
		t.Fatalf("%+v, %v", body, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("a 14-byte body that declared %d bytes allocated %d bytes", request.ContentLength, allocated)
	}
}

func TestReadAllChunkedJoinsEverySize(t *testing.T) {
	for _, size := range []int{0, 1, bodyFirstChunk - 1, bodyFirstChunk, bodyFirstChunk + 1, 3*bodyFirstChunk + 7, 5 << 20} {
		want := bytes.Repeat([]byte("abcdefghij"), size/10+1)[:size]
		got, err := readAllChunked(bytes.NewReader(want))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("size %d: %d bytes back, %v", size, len(got), err)
		}
	}
}
