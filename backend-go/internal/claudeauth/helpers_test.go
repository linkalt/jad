package claudeauth

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type issuerCall struct {
	Path        string
	ContentType string
	JSON        map[string]any
}

type reply struct {
	status int
	body   string
}

// fakeIssuer answers every POST with the next queued reply and records each call.
type fakeIssuer struct {
	*httptest.Server
	mutex   sync.Mutex
	Calls   []issuerCall
	replies []reply
}

func newFakeIssuer(t *testing.T, replies ...reply) *fakeIssuer {
	t.Helper()
	issuer := &fakeIssuer{replies: replies}
	issuer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := issuerCall{Path: r.URL.Path, ContentType: r.Header.Get("Content-Type")}
		_ = json.Unmarshal(raw, &call.JSON)
		issuer.mutex.Lock()
		issuer.Calls = append(issuer.Calls, call)
		next := reply{status: 500, body: `{"error":"no reply queued"}`}
		if len(issuer.replies) > 0 {
			next, issuer.replies = issuer.replies[0], issuer.replies[1:]
		}
		issuer.mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(next.status)
		_, _ = io.WriteString(w, next.body)
	}))
	t.Cleanup(issuer.Close)
	return issuer
}

func (f *fakeIssuer) callCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.Calls)
}

func tokenBody(access, refresh string, expiresIn int) string {
	body := map[string]any{"token_type": "Bearer", "access_token": access, "expires_in": expiresIn, "scope": "user:inference"}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// clientFor returns a client whose token endpoint is the fake server.
func clientFor(t *testing.T, issuer *fakeIssuer) *Client {
	t.Helper()
	config := DefaultConfig()
	config.TokenURL = issuer.URL + "/v1/oauth/token"
	config.LoginPort = 0
	if err := config.finish(); err != nil {
		t.Fatal(err)
	}
	return &Client{Config: &config}
}

func storeIn(t *testing.T) *Store {
	t.Helper()
	return &Store{Path: filepath.Join(t.TempDir(), "auth", "claude-oauth.json"), KeepBackup: true}
}

// writeFile writes a credential file with the given keys.
func writeFile(t *testing.T, store *Store, keys map[string]any) {
	t.Helper()
	raw, _ := json.MarshalIndent(keys, "", "  ")
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func oldFile(expires time.Time) map[string]any {
	return map[string]any{
		"version": 1, "access_token": "old-access", "refresh_token": "old-refresh",
		"expires_at": expires.UTC().Format(time.RFC3339), "scope": "old-scope",
		"last_refresh": fixedNow.Add(-time.Hour).UTC().Format(time.RFC3339),
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-auth.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// portHolder holds one free loopback port, to make a port busy.
type portHolder struct{ l net.Listener }

func (n *portHolder) listen() (*portHolder, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	n.l = l
	return n, err
}
func (n *portHolder) port() int { return n.l.Addr().(*net.TCPAddr).Port }
func (n *portHolder) Close()    { _ = n.l.Close() }
