package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionReadmeRoute(t *testing.T) {
	f := newLayoutFixture(t)
	const route = "/api/sessions/local:demo/readme"
	readme := filepath.Join(f.cwd, "README.md")

	if status, body := f.do(t, http.MethodGet, route, ""); status != http.StatusNotFound || !strings.Contains(body, "no README") {
		t.Fatalf("without a README = %d %s, want 404", status, body)
	}

	if err := os.WriteFile(readme, []byte("# Demo\n\ntext"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body := f.do(t, http.MethodGet, route, "")
	var got struct {
		Name      string `json:"name"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got.Name != "README.md" || got.Content != "# Demo\n\ntext" || got.Truncated {
		t.Fatalf("readme = %d %s", status, body)
	}
	if status, _ := f.do(t, http.MethodGet, "/api/sessions/local:demo/readme?profile=review%2Freviewer", ""); status != http.StatusOK {
		t.Fatalf("readme of another profile = %d", status)
	}
	if status, _ := f.do(t, http.MethodPost, route, `{}`); status == http.StatusOK {
		t.Fatal("the README route accepted a POST")
	}
	filesStatus, _ := f.do(t, http.MethodGet, "/api/sessions/local:nope/files", "")
	if status, _ := f.do(t, http.MethodGet, "/api/sessions/local:nope/readme", ""); status == http.StatusOK || status != filesStatus {
		t.Fatalf("unknown session = %d, want the file listing's %d", status, filesStatus)
	}

	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, readme); err == nil {
		if status, body := f.do(t, http.MethodGet, route, ""); status != http.StatusNotFound || strings.Contains(body, "secret") {
			t.Fatalf("a README linked out of the folder = %d %s, want 404", status, body)
		}
	}
}

func TestSessionReadmeFollowsTheFileListingSwitch(t *testing.T) {
	f := newLayoutFixture(t)
	if err := os.WriteFile(filepath.Join(f.cwd, "README.md"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(f.root, "sessions", "demo.json")
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	var session map[string]any
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	session["settings"] = map[string]any{"session_files": map[string]any{"enabled": false}}
	data, _ = json.Marshal(session)
	if err := os.WriteFile(sessionFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.do(t, http.MethodGet, "/api/sessions/local:demo/readme", ""); status != http.StatusForbidden {
		t.Fatalf("README with the listing disabled = %d, want 403", status)
	}
}
