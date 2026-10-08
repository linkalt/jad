package openaiauth

import (
	"bytes"
	"encoding/base64"
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

// jwt builds an unsigned token with the given payload. The package never checks signatures.
func jwt(t *testing.T, payload map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "none"}) + "." + encode(payload) + ".sig"
}

// accessJWT is an access token issued at issued and valid for lifetime, with an account id.
func accessJWT(t *testing.T, issued time.Time, lifetime time.Duration, account string) string {
	return jwt(t, map[string]any{
		"iat": issued.Unix(), "exp": issued.Add(lifetime).Unix(),
		authClaim: map[string]any{"chatgpt_account_id": account},
	})
}

type issuerCall struct {
	Path        string
	ContentType string
	Form        map[string][]string
	JSON        map[string]any
}

// fakeIssuer answers /oauth/token with the next queued reply and records each call.
type fakeIssuer struct {
	*httptest.Server
	mutex   sync.Mutex
	Calls   []issuerCall
	replies []reply
}

type reply struct {
	status int
	body   string
}

func newFakeIssuer(t *testing.T, replies ...reply) *fakeIssuer {
	t.Helper()
	issuer := &fakeIssuer{replies: replies}
	issuer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := issuerCall{Path: r.URL.Path, ContentType: r.Header.Get("Content-Type")}
		if call.ContentType == "application/json" {
			_ = json.Unmarshal(raw, &call.JSON)
		} else {
			r.Body = io.NopCloser(bytes.NewReader(raw))
			_ = r.ParseForm()
			call.Form = r.PostForm
		}
		issuer.mutex.Lock()
		issuer.Calls = append(issuer.Calls, call)
		var next reply
		if len(issuer.replies) > 0 {
			next, issuer.replies = issuer.replies[0], issuer.replies[1:]
		} else {
			next = reply{status: 500, body: `{"error":"no reply queued"}`}
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

func tokenBody(t *testing.T, access, id, refresh string) string {
	t.Helper()
	body := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600}
	if id != "" {
		body["id_token"] = id
	}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// clientFor returns a client whose issuer is the fake server.
func clientFor(t *testing.T, issuer *fakeIssuer) *Client {
	t.Helper()
	config := DefaultConfig()
	config.Issuer = issuer.URL
	if err := config.finish(); err != nil {
		t.Fatal(err)
	}
	return &Client{Config: &config}
}

// storeIn returns a store for a new file in a private temp directory.
func storeIn(t *testing.T) *Store {
	t.Helper()
	return &Store{Path: filepath.Join(t.TempDir(), "auth", "openai-codex.json"), KeepBackup: true}
}

// writeFile writes a credential file with the given tokens and extra top-level keys.
func writeFile(t *testing.T, store *Store, tokens map[string]any, extra map[string]any) {
	t.Helper()
	top := map[string]any{"auth_mode": "chatgpt", "tokens": tokens, "last_refresh": "2026-10-01T00:00:00.000Z"}
	for key, value := range extra {
		top[key] = value
	}
	raw, _ := json.MarshalIndent(top, "", "  ")
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, raw, 0o600); err != nil {
		t.Fatal(err)
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
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return out
}

func tokensOf(object map[string]any) map[string]any {
	tokens, _ := object["tokens"].(map[string]any)
	return tokens
}

// newCountingIssuer answers /oauth/token by calling decide with the refresh token of a JSON request.
func newCountingIssuer(t *testing.T, decide func(refreshToken string) (int, string)) *fakeIssuer {
	t.Helper()
	issuer := &fakeIssuer{}
	issuer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		refresh, _ := body["refresh_token"].(string)
		issuer.mutex.Lock()
		issuer.Calls = append(issuer.Calls, issuerCall{Path: r.URL.Path, JSON: body})
		issuer.mutex.Unlock()
		status, reply := decide(refresh)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(issuer.Close)
	return issuer
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
