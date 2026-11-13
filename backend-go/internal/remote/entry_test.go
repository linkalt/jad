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
	return ParseBinding("box", "/etc/jad/remotes/box.json", []byte(body))
}

func TestParseEntryHTTP(t *testing.T) {
	entry, err := parse(t, `{"kind":"remote","remote":{"workspace":"proj","url":"http://host.example:8787/","profiles":["review/*"]}}`)
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
	entry, err := parse(t, `{"kind":"remote","remote":{"workspace":"proj","url":"unix:///run/jad/api.sock"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Unix != "/run/jad/api.sock" || !entry.Allows("any/thing") {
		t.Fatalf("entry: %+v", entry)
	}
}

func TestParseEntryRefusals(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"https":           {`{"kind":"remote","remote":{"workspace":"proj","url":"https://h:1"}}`, "https was removed"},
		"ftp":             {`{"kind":"remote","remote":{"workspace":"proj","url":"ftp://h"}}`, "must be http://host[:port]"},
		"userinfo":        {`{"kind":"remote","remote":{"workspace":"proj","url":"http://u:p@h"}}`, "credentials"},
		"query":           {`{"kind":"remote","remote":{"workspace":"proj","url":"http://h/?a=b"}}`, "credentials, a query"},
		"path":            {`{"kind":"remote","remote":{"workspace":"proj","url":"http://h/api"}}`, "no path"},
		"no host":         {`{"kind":"remote","remote":{"workspace":"proj","url":"http://"}}`, "must be http://host[:port]"},
		"relative unix":   {`{"kind":"remote","remote":{"workspace":"proj","url":"unix://rel.sock"}}`, "unix:///absolute"},
		"inline token":    {`{"kind":"remote","remote":{"workspace":"proj","url":"http://h","token":"abc"}}`, `"token" is not allowed`},
		"top-level token": {`{"kind":"remote","token":"abc","remote":{"workspace":"proj","url":"http://h"}}`, `"token" is not allowed`},
		"proxy":           {`{"kind":"remote","remote":{"workspace":"proj","url":"http://h","proxy":"http://p"}}`, "no proxy"},
		"wrong kind":      {`{"kind":"profile","remote":{"workspace":"proj","url":"http://h"}}`, `kind must be "remote"`},
		"model":           {`{"kind":"remote","model":"x","remote":{"workspace":"proj","url":"http://h"}}`, "unknown field"},
		"timeout cap":     {`{"kind":"remote","remote":{"workspace":"proj","url":"http://h","timeout_ms":999999999}}`, "timeout_ms must be between"},
		"retries cap":     {`{"kind":"remote","remote":{"workspace":"proj","url":"http://h","retries":99}}`, "retries must be between"},
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
		body := `{"kind":"remote","remote":{"workspace":"proj","url":"http://h:1","` + key + `":"x"}}`
		if _, err := parse(t, body); err == nil || !strings.Contains(err.Error(), `"`+key+`" was removed`) || !strings.Contains(err.Error(), "no TLS") {
			t.Errorf("%s: want a named removed-key error, got %v", key, err)
		}
	}
	if _, err := parse(t, `{"kind":"remote","ca_file":"ca.pem","remote":{"workspace":"proj","url":"unix:///a.sock"}}`); err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Errorf("a top-level ca_file must be refused by name: %v", err)
	}
}

func TestListAndLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.json", `{"kind":"remote","remote":{"url":"http://b:1","workspace":"proj"}}`)
	write("a.json", `{"kind":"remote","remote":{"url":"unix:///a.sock","workspace":"proj"}}`)
	write("broken.json", `{"kind":"remote","remote":{"url":"https://x","workspace":"proj"}}`)
	write("notes.txt", "x")
	entries, problems := ListBindings(dir)
	if len(entries) != 2 || entries[0].Name != "a" || entries[1].Name != "b" || len(problems) != 1 {
		t.Fatalf("entries %+v problems %v", entries, problems)
	}
	if _, err := LoadBinding(dir, "missing"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	if _, err := LoadBinding(dir, "../a"); err != ErrNotFound {
		t.Fatalf("traversal name: %v", err)
	}
	if entries, problems := ListBindings(filepath.Join(dir, "none")); entries != nil || problems != nil {
		t.Fatalf("missing folder must list as empty: %v %v", entries, problems)
	}
}

func TestListIncludesASymlinkedEntryAsLoadDoes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte(`{"kind":"remote","remote":{"url":"http://b:1","workspace":"proj"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBinding(dir, "linked"); err != nil {
		t.Fatalf("Load follows the symlink: %v", err)
	}
	entries, problems := ListBindings(dir)
	if len(entries) != 1 || entries[0].Name != "linked" || len(problems) != 0 {
		t.Fatalf("List must include the symlinked entry that Load reads: %+v %v", entries, problems)
	}
}

func TestParseEntryRefusesCharacterClassGlobs(t *testing.T) {
	for _, glob := range []string{"review[^x]*", "[a-z]*/x"} {
		body := `{"kind":"remote","remote":{"url":"unix:///run/jad/api.sock","workspace":"proj","profiles":["` + glob + `"]}}`
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
