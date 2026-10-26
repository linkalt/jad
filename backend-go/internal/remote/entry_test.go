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

func TestParseEntryHTTPS(t *testing.T) {
	entry, err := parse(t, `{"kind":"remote","remote":{"url":"https://host.example:8443/","token_env":"BOX_TOKEN","ca_file":"ca.pem","default_session":"main","profiles":["review/*"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if entry.URL != "https://host.example:8443" || entry.Host != "host.example:8443" || entry.Unix != "" {
		t.Fatalf("entry: %+v", entry)
	}
	if entry.CAFile != "/etc/jad/remotes/ca.pem" || entry.Timeout != DefaultTimeoutMs || entry.Retries != DefaultRetries || entry.MaxResponseBytes != DefaultMaxResponseBytes {
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
		"plain http":      {`{"kind":"remote","remote":{"url":"http://h:1","token_env":"T"}}`, "plain http is not allowed"},
		"ftp":             {`{"kind":"remote","remote":{"url":"ftp://h","token_env":"T"}}`, "must be https"},
		"userinfo":        {`{"kind":"remote","remote":{"url":"https://u:p@h","token_env":"T"}}`, "credentials"},
		"query":           {`{"kind":"remote","remote":{"url":"https://h/?a=b","token_env":"T"}}`, "credentials, a query"},
		"path":            {`{"kind":"remote","remote":{"url":"https://h/api","token_env":"T"}}`, "no path"},
		"relative unix":   {`{"kind":"remote","remote":{"url":"unix://rel.sock","token_env":"T"}}`, "unix:///absolute"},
		"no token":        {`{"kind":"remote","remote":{"url":"https://h"}}`, "needs token_env or token_file"},
		"both tokens":     {`{"kind":"remote","remote":{"url":"https://h","token_env":"T","token_file":"f"}}`, "alternatives"},
		"inline token":    {`{"kind":"remote","remote":{"url":"https://h","token":"abc"}}`, `"token" is not allowed`},
		"top-level token": {`{"kind":"remote","token":"abc","remote":{"url":"https://h","token_env":"T"}}`, `"token" is not allowed`},
		"insecure":        {`{"kind":"remote","remote":{"url":"https://h","token_env":"T","insecure_skip_verify":true}}`, "TLS is always verified"},
		"proxy":           {`{"kind":"remote","remote":{"url":"https://h","token_env":"T","proxy":"http://p"}}`, "no proxy"},
		"wrong kind":      {`{"kind":"profile","remote":{"url":"https://h","token_env":"T"}}`, `kind must be "remote"`},
		"model":           {`{"kind":"remote","model":"x","remote":{"url":"https://h","token_env":"T"}}`, "unknown field"},
		"bad env name":    {`{"kind":"remote","remote":{"url":"https://h","token_env":"A B"}}`, "environment variable name"},
		"ca on unix":      {`{"kind":"remote","remote":{"url":"unix:///a.sock","token_env":"T","ca_file":"x"}}`, "ca_file only applies"},
		"timeout cap":     {`{"kind":"remote","remote":{"url":"https://h","token_env":"T","timeout_ms":999999999}}`, "timeout_ms must be between"},
		"retries cap":     {`{"kind":"remote","remote":{"url":"https://h","token_env":"T","retries":99}}`, "retries must be between"},
		"bad session":     {`{"kind":"remote","remote":{"url":"https://h","token_env":"T","default_session":"a/b"}}`, "default_session"},
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
	fromEnv := Entry{Name: "box", TokenEnv: "BOX_TOKEN"}
	if got, err := fromEnv.Token(func(string) string { return " " + goodToken + " " }); err != nil || got != goodToken {
		t.Fatalf("env token: %q %v", got, err)
	}
	if _, err := fromEnv.Token(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "BOX_TOKEN is not set") {
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
	_, err = Entry{Name: "box", TokenEnv: "T"}.Token(func(string) string { return secret })
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

func TestListAndLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.json", `{"kind":"remote","remote":{"url":"https://b:1","token_env":"T"}}`)
	write("a.json", `{"kind":"remote","remote":{"url":"unix:///a.sock","token_env":"T"}}`)
	write("broken.json", `{"kind":"remote","remote":{"url":"http://x","token_env":"T"}}`)
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
