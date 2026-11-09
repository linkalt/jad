package remoteapi

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// getRaw reads a plain-bytes answer: status, X-Result-Bytes and the body.
func (f *fixture) getRaw(path, secret string) (int, string, string) {
	f.t.Helper()
	request, _ := http.NewRequest("GET", f.http.URL+path, nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, response.Header.Get("X-Result-Bytes"), string(body)
}

func startedRun(t *testing.T, f *fixture, id string, text string) string {
	t.Helper()
	_, started := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", id))
	f.backend.runs[len(f.backend.runs)-1].finish(RunResult{State: "done", Text: text})
	return started["id"].(string)
}

// The result route hands out the whole text in parts that, joined, are the text: a part may end
// inside a character, and the size is told with every part.
func TestResultRouteServesTheWholeTextInParts(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) { o.ResultMaxBytes = 10 })
	text := strings.Repeat("é日", 40) + "!"
	id := startedRun(t, f, "req-result-0001", text)
	_, view := f.do("GET", Prefix+"/runs/"+id, secretA, nil)
	if got := view["text"].(string); len(got) > 10 || view["cut"] != true || !strings.HasPrefix(text, got) {
		t.Fatalf("view: %v", view)
	}
	var joined strings.Builder
	for offset := 0; ; {
		status, total, part := f.getRaw(Prefix+"/runs/"+id+"/result?limit=7&offset="+strconv.Itoa(offset), secretA)
		if status != 200 || total != strconv.Itoa(len(text)) || len(part) > 7 {
			t.Fatalf("part at %d: %d total=%q len=%d", offset, status, total, len(part))
		}
		joined.WriteString(part)
		offset += len(part)
		if offset == len(text) {
			break
		}
		if len(part) == 0 {
			t.Fatal("an empty part before the end")
		}
	}
	if joined.String() != text {
		t.Fatalf("joined parts differ from the text: %d bytes vs %d", joined.Len(), len(text))
	}
}

func TestResultRouteBoundaries(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) { o.ResultMaxBytes = 8 })
	id := startedRun(t, f, "req-result-0002", "0123456789abcdef")
	path := Prefix + "/runs/" + id + "/result"
	// No limit: the server's result_max_bytes is the part size.
	if status, total, part := f.getRaw(path, secretA); status != 200 || total != "16" || part != "01234567" {
		t.Fatalf("default part: %d %q %q", status, total, part)
	}
	// A limit above the server's own is not cut by it: the reader chose what it can take.
	if _, _, part := f.getRaw(path+"?limit=100", secretA); part != "0123456789abcdef" {
		t.Fatalf("a large limit: %q", part)
	}
	// The exact end gives an empty part with the size; past it is refused.
	if status, total, part := f.getRaw(path+"?offset=16", secretA); status != 200 || total != "16" || part != "" {
		t.Fatalf("at the end: %d %q %q", status, total, part)
	}
	for _, query := range []string{"?offset=17", "?offset=-1", "?offset=x", "?limit=0", "?limit=x"} {
		if status, _, _ := f.getRaw(path+query, secretA); status != 400 {
			t.Errorf("%s: status %d, want 400", query, status)
		}
	}
}

func TestResultRouteNeedsAnEndedRunTheTokensScopeAndStatus(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	_, started := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-result-0003"))
	id := started["id"].(string)
	path := Prefix + "/runs/" + id + "/result"
	if status, _, _ := f.getRaw(path, secretA); status != 409 {
		t.Fatalf("a running run: %d, want 409", status)
	}
	f.backend.runs[0].finish(RunResult{State: "done", Text: "answer"})
	if status, _, _ := f.getRaw(path, secretB); status != 404 {
		t.Fatalf("another token's run: %d, want 404", status)
	}
	if status, _, _ := f.getRaw(Prefix+"/runs/rr_0000000000000000/result", secretA); status != 404 {
		t.Fatalf("an unknown run: %d, want 404", status)
	}
	if status, _, part := f.getRaw(path, secretA); status != 200 || part != "answer" {
		t.Fatalf("the owner: %d %q", status, part)
	}
}

// text_max_bytes lowers the view's text below the server's own limit and never raises it.
func TestViewTextMaxBytesLowersTheViewAndNeverRaisesIt(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) { o.ResultMaxBytes = 10 })
	id := startedRun(t, f, "req-result-0004", strings.Repeat("é", 20))
	for query, want := range map[string]int{"": 10, "?text_max_bytes=4": 4, "?text_max_bytes=0": 0, "?text_max_bytes=100": 10} {
		_, view := f.do("GET", Prefix+"/runs/"+id+query, secretA, nil)
		text, _ := view["text"].(string)
		if len(text) != want || view["cut"] != true {
			t.Errorf("%q: text %d bytes, cut=%v; want %d bytes, cut", query, len(text), view["cut"], want)
		}
	}
	if status, _ := f.do("GET", Prefix+"/runs/"+id+"?text_max_bytes=-1", secretA, nil); status != 400 {
		t.Errorf("a negative text_max_bytes: %d, want 400", status)
	}
}
