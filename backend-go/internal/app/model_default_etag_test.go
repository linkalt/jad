package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The session snapshot names the profile file's entity tag, the model-default route takes it
// back as If-Match, and a tag that no longer matches (the file was edited meanwhile) is 412 and
// writes nothing. A request without If-Match is accepted as before.
func TestModelDefaultTakesTheProfileETagAsIfMatch(t *testing.T) {
	f := newLayoutFixture(t)
	const session = "/api/sessions/local:demo"
	profile := "?profile=" + url.QueryEscape("dev/coder")
	post := func(model, ifMatch string) (int, string, string) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, f.api.URL+session+"/model-default"+profile, strings.NewReader(`{"model_default":"`+model+`"}`))
		request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
		request.Header.Set("Content-Type", "application/json")
		if ifMatch != "" {
			request.Header.Set("If-Match", ifMatch)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body), response.Header.Get("ETag")
	}
	etagOf := func(body string) string {
		t.Helper()
		var fields struct {
			ProfileETag string `json:"profile_etag"`
		}
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return fields.ProfileETag
	}

	status, body := f.do(t, http.MethodGet, session+profile, "")
	listed := etagOf(body)
	if status != http.StatusOK || !strings.HasPrefix(listed, `"`) {
		t.Fatalf("snapshot: %d profile_etag %q", status, listed)
	}
	status, body, header := post("openai/large", listed)
	updated := etagOf(body)
	if status != http.StatusOK || updated == "" || updated == listed || header != updated {
		t.Fatalf("update with the listed tag: %d %s etag header %q", status, body, header)
	}
	if _, body = f.do(t, http.MethodGet, session+profile, ""); etagOf(body) != updated {
		t.Fatalf("the snapshot did not follow the write: %s", body)
	}

	profilePath := filepath.Join(f.root, "profiles", "dev", "coder.json")
	before, _ := os.ReadFile(profilePath)
	if status, body, _ = post("openai/small", listed); status != http.StatusPreconditionFailed || !strings.Contains(body, "changed since it was read") {
		t.Fatalf("a stale tag: %d %s", status, body)
	}
	if after, _ := os.ReadFile(profilePath); string(after) != string(before) {
		t.Fatal("a stale tag still wrote the profile")
	}
	if status, body, _ = post("openai/small", ""); status != http.StatusOK || etagOf(body) == updated {
		t.Fatalf("no If-Match: %d %s", status, body)
	}
}
