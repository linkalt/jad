package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodToken = "tok_0123456789abcdef0123456789abcdef"

func parse(t *testing.T, body string) (Entry, error) {
	t.Helper()
	return ParseEntry("box", "/etc/jad/remotes/box.json", []byte(body))
}

func TestParseEntryHTTP(t *testing.T) {
	entry, err := parse(t, `{"kind":"remote","remote":{"url":"http://host.example:8787/","token_env":"JAD_REMOTE_BOX","default_session":"main","profiles":["review/*"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if entry.URL != "http://host.example:8787" || entry.Host != "host.example:8787" || entry.Unix != "" {
		t.Fatalf("entry: %+v", entry)
	}
	if entry.Timeout != DefaultTimeoutMs || entry.Retries != DefaultRetries || entry.MaxResponseBytes != DefaultMaxResponseBytes {
		t.Fatalf("defaults: %+v", entry)
	}
	if !entry.Allows("review/a") || entry.Allows("dev/coder") {
		t.Fatal("profiles glob")
	}
}

func TestParseEntryUnix(t *testing.T) {
	entry, err := parse(t, `{"kind":"remote","remote":{"url":"unix:///run/jad/api.sock","token_file":"t"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Unix != "/run/jad/api.sock" || entry.TokenFile != "/etc/jad/remotes/t" || !entry.Allows("any/thing") {
		t.Fatalf("entry: %+v", entry)
	}
}

func TestParseEntryRefusals(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"https":           {`{"kind":"remote","remote":{"url":"https://h:1","token_env":"JAD_REMOTE_T"}}`, "https was removed"},
		"ftp":             {`{"kind":"remote","remote":{"url":"ftp://h","token_env":"JAD_REMOTE_T"}}`, "must be http://host[:port]"},
		"userinfo":        {`{"kind":"remote","remote":{"url":"http://u:p@h","token_env":"JAD_REMOTE_T"}}`, "credentials"},
		"query":           {`{"kind":"remote","remote":{"url":"http://h/?a=b","token_env":"JAD_REMOTE_T"}}`, "credentials, a query"},
		"path":            {`{"kind":"remote","remote":{"url":"http://h/api","token_env":"JAD_REMOTE_T"}}`, "no path"},
		"no host":         {`{"kind":"remote","remote":{"url":"http://","token_env":"JAD_REMOTE_T"}}`, "must be http://host[:port]"},
		"relative unix":   {`{"kind":"remote","remote":{"url":"unix://rel.sock","token_env":"JAD_REMOTE_T"}}`, "unix:///absolute"},
		"no token":        {`{"kind":"remote","remote":{"url":"http://h"}}`, "needs token_env or token_file"},
		"both tokens":     {`{"kind":"remote","remote":{"url":"http://h","token_env":"JAD_REMOTE_T","token_file":"f"}}`, "alternatives"},
		"inline token":    {`{"kind":"remote","remote":{"url":"http://h","token":"abc"}}`, `"token" is not allowed`},
		"top-level token": {`{"kind":"remote","token":"abc","remote":{"url":"http://h","token_env":"JAD_REMOTE_T"}}`, `"token" is not allowed`},
		"proxy":           {`{"kind":"remote","remote":{"url":"http://h","token_env":"JAD_REMOTE_T","proxy":"http://p"}}`, "no proxy"},
		"wrong kind":      {`{"kind":"profile","remote":{"url":"http://h","token_env":"JAD_REMOTE_T"}}`, `kind must be "remote"`},
		"model":           {`{"kind":"remote","model":"x","remote":{"url":"http://h","token_env":"JAD_REMOTE_T"}}`, "unknown field"},
		"bad env name":    {`{"kind":"remote","remote":{"url":"http://h","token_env":"A B"}}`, "JAD_REMOTE_"},
		"provider key":    {`{"kind":"remote","remote":{"url":"http://h","token_env":"OPENAI_API_KEY"}}`, "may not read other variables"},
		"timeout cap":     {`{"kind":"remote","remote":{"url":"http://h","token_env":"JAD_REMOTE_T","timeout_ms":999999999}}`, "timeout_ms must be between"},
		"retries cap":     {`{"kind":"remote","remote":{"url":"http://h","token_env":"JAD_REMOTE_T","retries":99}}`, "retries must be between"},
		"bad session":     {`{"kind":"remote","remote":{"url":"http://h","token_env":"JAD_REMOTE_T","default_session":"a/b"}}`, "default_session"},
		"no remote":       {`{"kind":"remote"}`, "remote is required"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parse(t, test.body)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
		})
	}
}

// The https era of remote files is gone: its TLS keys stay refused by name, so an old file
// never silently reaches a server unverified.
func TestParseEntryRefusesRemovedTLSKeys(t *testing.T) {
	for _, key := range []string{"ca_file", "insecure", "insecure_skip_verify", "skip_verify", "tls"} {
		body := `{"kind":"remote","remote":{"url":"http://h:1","token_env":"JAD_REMOTE_T","` + key + `":"x"}}`
		if _, err := parse(t, body); err == nil || !strings.Contains(err.Error(), `"`+key+`" was removed`) || !strings.Contains(err.Error(), "no TLS") {
			t.Errorf("%s: want a named removed-key error, got %v", key, err)
		}
	}
	if _, err := parse(t, `{"kind":"remote","ca_file":"ca.pem","remote":{"url":"unix:///a.sock","token_env":"JAD_REMOTE_T"}}`); err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Errorf("a top-level ca_file must be refused by name: %v", err)
	}
}

func TestTokenSources(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tok")
	if err := os.WriteFile(file, []byte(goodToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile := Entry{Name: "box", TokenFile: file}
	if got, err := fromFile.Token(nil); err != nil || got != goodToken {
		t.Fatalf("file token: %q %v", got, err)
	}
	fromEnv := Entry{Name: "box", TokenEnv: "JAD_REMOTE_BOX"}
	if got, err := fromEnv.Token(func(string) string { return " " + goodToken + " " }); err != nil || got != goodToken {
		t.Fatalf("env token: %q %v", got, err)
	}
	if _, err := fromEnv.Token(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "JAD_REMOTE_BOX is not set") {
		t.Fatalf("unset env: %v", err)
	}
}

func TestTokenErrorsNeverContainTheToken(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tok")
	secret := "short secret with spaces"
	if err := os.WriteFile(file, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Entry{Name: "box", TokenFile: file}.Token(nil)
	if err == nil || strings.Contains(err.Error(), "short") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a short token must be refused without echoing it: %v", err)
	}
	_, err = Entry{Name: "box", TokenEnv: "JAD_REMOTE_T"}.Token(func(string) string { return secret })
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("env token echoed: %v", err)
	}
}

func TestTokenFileMustBePrivate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(file, []byte(goodToken), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Entry{Name: "box", TokenFile: file}).Token(nil); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable token file accepted: %v", err)
	}
}

func TestTokenFileIsReadFromTheCheckedFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(file, []byte(goodToken), 0o600); err != nil {
		t.Fatal(err)
	}
	original := tokenFileChecked
	t.Cleanup(func() { tokenFileChecked = original })
	tokenFileChecked = func() {
		// A swap after the check: the path now names a different token, which must not be read.
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("tok_ffffffffffffffffffffffffffffffff"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	token, err := (Entry{Name: "box", TokenFile: file}).Token(nil)
	if err != nil || token != goodToken {
		t.Fatalf("token must come from the file that was checked, got %q, %v", token, err)
	}
}

func TestListAndLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.json", `{"kind":"remote","remote":{"url":"http://b:1","token_env":"JAD_REMOTE_T"}}`)
	write("a.json", `{"kind":"remote","remote":{"url":"unix:///a.sock","token_env":"JAD_REMOTE_T"}}`)
	write("broken.json", `{"kind":"remote","remote":{"url":"https://x","token_env":"JAD_REMOTE_T"}}`)
	write("notes.txt", "x")
	entries, problems := List(dir)
	if len(entries) != 2 || entries[0].Name != "a" || entries[1].Name != "b" || len(problems) != 1 {
		t.Fatalf("entries %+v problems %v", entries, problems)
	}
	if _, err := Load(dir, "missing"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	if _, err := Load(dir, "../a"); err != ErrNotFound {
		t.Fatalf("traversal name: %v", err)
	}
	if entries, problems := List(filepath.Join(dir, "none")); entries != nil || problems != nil {
		t.Fatalf("missing folder must list as empty: %v %v", entries, problems)
	}
}

func TestListIncludesASymlinkedEntryAsLoadDoes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte(`{"kind":"remote","remote":{"url":"http://b:1","token_env":"JAD_REMOTE_T"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "linked"); err != nil {
		t.Fatalf("Load follows the symlink: %v", err)
	}
	entries, problems := List(dir)
	if len(entries) != 1 || entries[0].Name != "linked" || len(problems) != 0 {
		t.Fatalf("List must include the symlinked entry that Load reads: %+v %v", entries, problems)
	}
}

func TestParseEntryRefusesCharacterClassGlobs(t *testing.T) {
	for _, glob := range []string{"review[^x]*", "[a-z]*/x"} {
		body := `{"kind":"remote","remote":{"url":"unix:///run/jad/api.sock","token_file":"t","profiles":["` + glob + `"]}}`
		if _, err := parse(t, body); err == nil || !strings.Contains(err.Error(), "[classes]") {
			t.Errorf("%q: a class can match '/' and must be refused: %v", glob, err)
		}
	}
}

// SameHost is true for the entries that reach a server on this machine: a unix socket and a loopback
// or unspecified address. An address of another machine is not.
func TestEntrySameHost(t *testing.T) {
	for url, want := range map[string]bool{
		"unix:///run/jad/peer.sock": true,
		"http://localhost:8080":     true,
		"http://LOCALHOST":          true,
		"http://peer.localhost:80":  true,
		"http://127.0.0.1:8080":     true,
		"http://127.1.2.3:8080":     true,
		"http://[::1]:8080":         true,
		"http://0.0.0.0:8080":       true,
		"http://192.0.2.7:8080":     false,
		"http://example.com":        false,
		"http://localhost.example":  false,
		"http://[2001:db8::1]:80":   false,
	} {
		entry := Entry{}
		if err := entry.setURL("peer.json", url); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		if got := entry.SameHost(); got != want {
			t.Errorf("%s: SameHost = %v, want %v", url, got, want)
		}
	}
}
