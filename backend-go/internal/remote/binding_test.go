package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const bindingSecret = "BINDSECRET_0123456789abcdef0123456789ab"

func bindingJSON(url, extra string) []byte {
	return []byte(`{"kind":"remote","remote":{"url":"` + url + `","workspace":"proj","timeout_ms":2000` + extra + `}}`)
}

func TestParseBindingNeedsAWorkspaceAndNoCredential(t *testing.T) {
	entry, err := ParseBinding("box", "/g/remotes/box.json", bindingJSON("http://10.0.0.2:8787", `,"profiles":["review/*"]`))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Workspace != "proj" || entry.Keyed() || entry.WireSession("main") != "proj:main" {
		t.Fatalf("%+v", entry)
	}
	for name, c := range map[string]struct{ data, want string }{
		"no workspace":    {`{"kind":"remote","remote":{"url":"http://10.0.0.2:8787"}}`, "remote.workspace is required"},
		"colon workspace": {`{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"a:b"}}`, "remote.workspace is required"},
		"token_env":       {`{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj","token_env":"JAD_REMOTE_X"}}`, "remote.token_env was removed with workspace layout v1; the secret moves to a key file"},
		"token_file":      {`{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj","token_file":"t"}}`, "remote.token_file was removed with workspace layout v1; move the secret to a key file G/auth/keys"},
		"default_session": {`{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj","default_session":"main"}}`, "remote.default_session was removed with workspace layout v1; write the session in the address"},
		"inline token":    {`{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj","token":"` + bindingSecret + `"}}`, "token"},
		"unknown key":     {`{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj","bogus":1}}`, "bogus"},
		"wrong kind":      {`{"kind":"other","remote":{"url":"http://10.0.0.2:8787","workspace":"proj"}}`, "kind"},
	} {
		_, err := ParseBinding("box", "/g/remotes/box.json", []byte(c.data))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		} else if strings.Contains(err.Error(), bindingSecret) {
			t.Errorf("%s: the error repeats the secret: %v", name, err)
		}
	}
}

func TestParseBindingProfileFormsAndSessionQualification(t *testing.T) {
	entry := func(name string) (Entry, error) {
		return Entry{Name: name, Workspace: "proj"}, nil
	}
	target, err := ParseBindingProfile("remote:box/main/review/security", entry)
	if err != nil || target.Remote != "box" || target.Session != "main" || target.Profile != "review/security" || target.WireSession() != "proj:main" {
		t.Fatalf("%+v %v", target, err)
	}
	sub, err := ParseBindingSubagent("remote:box/main/review/security/explore", entry)
	if err != nil || sub.Agent != "explore" || sub.WireSession() != "proj:main" {
		t.Fatalf("%+v %v", sub, err)
	}
	for _, bad := range []string{
		"remote:box/review/security", "remote:box/main/review", "remote:box/main/review/security/extra", "remote:box",
		"remote:box/proj:main/review/security", "remote:box/main/../security", "remote:box/main//security", "remote:/main/review/security", "box/main/review/security",
	} {
		if got, err := ParseBindingProfile(bad, entry); err == nil {
			t.Errorf("%q parsed as %+v", bad, got)
		}
	}
	if _, err := ParseBindingSubagent("remote:box/main/review/security", entry); err == nil {
		t.Error("an agent reference needs its agent")
	}
	// An unknown binding is the entry reader's error.
	if _, err := ParseBindingProfile("remote:none/main/review/security", func(string) (Entry, error) { return Entry{}, ErrNotFound }); err != ErrNotFound {
		t.Errorf("unknown binding: %v", err)
	}
}

func writeKey(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKeyFilesAreStrictAndNeverNamedInErrors(t *testing.T) {
	dir := t.TempDir()
	good := writeKey(t, dir, "k", `{"schema_version":1,"kind":"remote_key","binding":"box","token":"`+bindingSecret+`"}`, 0o600)
	if binding, err := KeyBinding("k", good); err != nil || binding != "box" {
		t.Fatalf("%q %v", binding, err)
	}
	for name, c := range map[string]struct {
		body string
		mode os.FileMode
		want string
	}{
		"group readable": {`{"kind":"remote_key","binding":"box","token":"` + bindingSecret + `"}`, 0o640, "chmod 600"},
		"both sources":   {`{"kind":"remote_key","binding":"box","token":"` + bindingSecret + `","token_env":"JAD_REMOTE_X"}`, 0o600, "alternatives"},
		"no source":      {`{"kind":"remote_key","binding":"box"}`, 0o600, "needs token or token_env"},
		"provider env":   {`{"kind":"remote_key","binding":"box","token_env":"OPENAI_API_KEY"}`, 0o600, "JAD_REMOTE_"},
		"wrong kind":     {`{"kind":"remote","binding":"box","token":"` + bindingSecret + `"}`, 0o600, "remote_key"},
		"no binding":     {`{"kind":"remote_key","token":"` + bindingSecret + `"}`, 0o600, "binding"},
		"unknown key":    {`{"kind":"remote_key","binding":"box","token":"` + bindingSecret + `","extra":1}`, 0o600, "not a valid remote_key file"},
		"bad version":    {`{"schema_version":2,"kind":"remote_key","binding":"box","token":"` + bindingSecret + `"}`, 0o600, "schema_version"},
	} {
		path := writeKey(t, dir, "bad", c.body, c.mode)
		_, err := KeyBinding("bad", path)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		} else if strings.Contains(err.Error(), bindingSecret) {
			t.Errorf("%s: the error repeats the secret: %v", name, err)
		}
	}
	if _, err := KeyBinding("gone", filepath.Join(dir, "gone.json")); err == nil || strings.Contains(err.Error(), dir) {
		t.Errorf("a missing key file: %v (the message names the key, not the path)", err)
	}
}

// A key serves its own binding only, is read at each request (a rotation applies to the next
// one without a restart), and nothing the client returns holds the secret.
func TestScopedDirectoryUsesTheKeyOfTheBindingAndReadsItEachTime(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if strings.Contains(r.Header.Get("Authorization"), "rejected") {
			http.Error(w, `{"code":"unauthorized","error":"no"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	remotes, keys := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remotes, "box.json"), bindingJSON(server.URL, ""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remotes, "other.json"), bindingJSON(server.URL, ""), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := writeKey(t, keys, "k", `{"kind":"remote_key","binding":"box","token":"`+bindingSecret+`"}`, 0o600)
	hub := NewHub(func(name string) string {
		if name == "JAD_REMOTE_BOX" {
			return "ENVTOKEN_0123456789abcdef0123456789abcdefab"
		}
		return ""
	})
	defer hub.Close()
	directory := hub.Directory(remotes).Scoped(Scope{Keys: map[string]Key{"box": {Name: "k", File: keyPath}, "other": {Name: "k", File: keyPath}}})
	call := func(name string) error {
		client, err := directory.Client(name)
		if err != nil {
			return err
		}
		return client.Do(context.Background(), Request{Method: "GET", Path: "/api/remote/v1/profiles"}, nil)
	}
	if err := call("box"); err != nil {
		t.Fatal(err)
	}
	// The same key on another binding is refused before any request: a key never works elsewhere.
	if err := call("other"); err == nil || !strings.Contains(err.Error(), "key k is for another binding") || strings.Contains(err.Error(), bindingSecret) {
		t.Fatalf("a key for another binding: %v", err)
	}
	// Rotation: the next request uses the new token.
	writeKey(t, keys, "k", `{"kind":"remote_key","binding":"box","token":"ROTATED_0123456789abcdef0123456789abcdefab"}`, 0o600)
	if err := call("box"); err != nil {
		t.Fatal(err)
	}
	// A key file that turns bad fails the next request closed, naming the key and never the secret.
	writeKey(t, keys, "k", `{"kind":"remote_key","binding":"box","token":"`+bindingSecret+`"}`, 0o644)
	if err := call("box"); err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), bindingSecret) {
		t.Fatalf("an open key file: %v", err)
	}
	writeKey(t, keys, "k", `{"kind":"remote_key","binding":"box","token":"rejected_0123456789abcdef0123456789abcdefab"}`, 0o600)
	if err := call("box"); err == nil || strings.Contains(err.Error(), "rejected_0123") {
		t.Fatalf("a rejected token: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer " + bindingSecret, "Bearer ROTATED_0123456789abcdef0123456789abcdefab", "Bearer rejected_0123456789abcdef0123456789abcdefab"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("requests sent: %q", seen)
	}
}

// A key from the environment is read when the request is made, only JAD_REMOTE_* names are
// accepted, and an unset or short variable fails closed without its value in the message.
func TestKeyTokenFromTheEnvironment(t *testing.T) {
	keys := t.TempDir()
	path := writeKey(t, keys, "k", `{"kind":"remote_key","binding":"box","token_env":"JAD_REMOTE_BOX"}`, 0o600)
	entry := Entry{Name: "box", Workspace: "proj", KeyName: "k", KeyFile: path}
	if _, err := entry.Token(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "JAD_REMOTE_BOX is not set") {
		t.Fatalf("unset: %v", err)
	}
	if _, err := entry.Token(func(string) string { return "short" }); err == nil || strings.Contains(err.Error(), "short") {
		t.Fatalf("short: %v", err)
	}
	if token, err := entry.Token(func(name string) string {
		if name != "JAD_REMOTE_BOX" {
			t.Errorf("read %s", name)
		}
		return "  " + bindingSecret + "\n"
	}); err != nil || token != bindingSecret {
		t.Fatalf("%q %v", token, err)
	}
	// An entry without a key has no credential, and no environment variable stands in for one.
	if _, err := (Entry{Name: "box", Workspace: "proj"}).Token(func(string) string { return bindingSecret }); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("unkeyed: %v", err)
	}
}

// Two sessions with different keys for one binding never share a client, so never a credential.
func TestScopedDirectoriesKeepClientsPerKeyFile(t *testing.T) {
	remotes, keys := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remotes, "box.json"), bindingJSON("http://127.0.0.1:1", ""), 0o600); err != nil {
		t.Fatal(err)
	}
	one := writeKey(t, keys, "one", `{"kind":"remote_key","binding":"box","token":"`+bindingSecret+`"}`, 0o600)
	two := writeKey(t, keys, "two", `{"kind":"remote_key","binding":"box","token":"TWOTWOTWO_0123456789abcdef0123456789abcd"}`, 0o600)
	hub := NewHub(nil)
	defer hub.Close()
	base := hub.Directory(remotes)
	a, _ := base.Scoped(Scope{Keys: map[string]Key{"box": {Name: "one", File: one}}}).Client("box")
	b, _ := base.Scoped(Scope{Keys: map[string]Key{"box": {Name: "two", File: two}}}).Client("box")
	again, _ := base.Scoped(Scope{Keys: map[string]Key{"box": {Name: "one", File: one}}}).Client("box")
	if a == b || a != again {
		t.Fatalf("clients: one=%p two=%p one-again=%p", a, b, again)
	}
	unkeyed, err := base.Scoped(Scope{}).Entry("box")
	if err != nil || unkeyed.Keyed() {
		t.Fatalf("an unkeyed binding: %+v %v", unkeyed, err)
	}
}

// PointsAt is the warning aid of jad check: a binding on the server's own socket, or on its own
// host and port, reaches the server itself.
func TestEntryPointsAtTheServerItself(t *testing.T) {
	parse := func(url string) Entry {
		entry, err := ParseBinding("self", "/g/remotes/self.json", bindingJSON(url, ""))
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	for _, c := range []struct {
		url        string
		listenUnix string
		host       string
		port       int
		want       bool
	}{
		{"http://127.0.0.1:8787", "", "127.0.0.1", 8787, true},
		{"http://localhost:8787", "", "127.0.0.1", 8787, true},
		{"http://127.0.0.1:8787", "", "0.0.0.0", 8787, true},
		{"http://127.0.0.1:8787", "", "", 8787, true},
		{"http://127.0.0.1:9999", "", "127.0.0.1", 8787, false},
		{"http://10.0.0.2:8787", "", "10.0.0.2", 8787, true},
		{"http://10.0.0.3:8787", "", "10.0.0.2", 8787, false},
		{"http://10.0.0.2:8787", "", "127.0.0.1", 8787, false},
		{"unix:///run/jad.sock", "/run/jad.sock", "127.0.0.1", 8787, true},
		{"unix:///run/other.sock", "/run/jad.sock", "127.0.0.1", 8787, false},
		{"http://127.0.0.1:8787", "/run/jad.sock", "127.0.0.1", 8787, false},
	} {
		if got := parse(c.url).PointsAt(c.listenUnix, c.host, c.port); got != c.want {
			t.Errorf("%s vs unix %q host %q port %d: %v, want %v", c.url, c.listenUnix, c.host, c.port, got, c.want)
		}
	}
}
