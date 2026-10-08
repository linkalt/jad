package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func jwt(t *testing.T, payload map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "none"}) + "." + encode(payload) + ".sig"
}

func accessJWT(t *testing.T, issued time.Time, lifetime time.Duration, account string) string {
	return jwt(t, map[string]any{
		"iat": issued.Unix(), "exp": issued.Add(lifetime).Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account},
	})
}

// issuer answers /oauth/token with status and body, and counts the requests.
func issuer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// configFor writes an openai-auth config that uses a credential file in a temp dir and the issuer.
func configFor(t *testing.T, issuerURL string, extra string) (configPath, authPath string) {
	t.Helper()
	dir := t.TempDir()
	authPath = filepath.Join(dir, "auth", "openai-codex.json")
	configPath = filepath.Join(dir, "openai-auth.json")
	body := fmt.Sprintf(`{"auth_file":%q,"issuer":%q,"login_port":0,"login_timeout_ms":3000,"open_browser":false%s}`, authPath, issuerURL, extra)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, authPath
}

func seed(t *testing.T, authPath string, tokens map[string]any, lastRefresh string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": tokens, "last_refresh": lastRefresh})
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func execute(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut, func() time.Time { return fixedNow })
	return code, out.String(), errOut.String()
}

func TestUsageAndUnknownCommand(t *testing.T) {
	if code, _, stderr := execute(t); code != exitUsage || !strings.Contains(stderr, "Usage: openai-auth") {
		t.Fatalf("no args: code %d %q", code, stderr)
	}
	if code, stdout, _ := execute(t, "help"); code != exitOK || !strings.Contains(stdout, "login") || !strings.Contains(stdout, "refresh") || !strings.Contains(stdout, "status") {
		t.Fatalf("help: code %d %q", code, stdout)
	}
	if code, _, stderr := execute(t, "logout"); code != exitUsage || !strings.Contains(stderr, `unknown command "logout"`) {
		t.Fatalf("unknown: code %d %q", code, stderr)
	}
	if code, _, stderr := execute(t, "status", "extra"); code != exitUsage || !strings.Contains(stderr, "unexpected argument") {
		t.Fatalf("stray arg: code %d %q", code, stderr)
	}
	if code, _, _ := execute(t, "status", "--nope"); code != exitUsage {
		t.Fatalf("unknown flag: code %d", code)
	}
	if code, _, _ := execute(t, "refresh", "-h"); code != exitOK {
		t.Fatalf("-h on a command: code %d, want 0", code)
	}
}

func TestBadConfigFailsBeforeAnyWork(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"issuer":"ftp://x"}`), 0o600)
	for _, command := range []string{"login", "refresh", "status"} {
		code, _, stderr := execute(t, command, "--config", bad)
		if code != exitFailed || !strings.Contains(stderr, "issuer") {
			t.Errorf("%s: code %d %q", command, code, stderr)
		}
	}
	if code, _, stderr := execute(t, "status", "--config", filepath.Join(t.TempDir(), "missing.json")); code != exitFailed || !strings.Contains(stderr, "read config") {
		t.Errorf("missing config file: code %d %q", code, stderr)
	}
}

func TestStatusCommand(t *testing.T) {
	server, calls := issuer(t, 500, `{}`)
	config, auth := configFor(t, server.URL, "")

	code, stdout, _ := execute(t, "status", "--config", config)
	if code != 1 || !strings.Contains(stdout, "file not found") {
		t.Fatalf("missing file: code %d %q", code, stdout)
	}

	access := accessJWT(t, fixedNow.Add(-2*time.Hour), 10*24*time.Hour, "acct-1234567890")
	seed(t, auth, map[string]any{"access_token": access, "id_token": "opaque-id", "refresh_token": "SECRET-REFRESH", "account_id": "acct-1234567890"}, fixedNow.Add(-2*time.Hour).Format(time.RFC3339))
	code, stdout, stderr := execute(t, "status", "--config", config)
	if code != 0 || stderr != "" {
		t.Fatalf("usable file: code %d stderr %q", code, stderr)
	}
	for _, want := range []string{"mode:           -rw-------", "refresh token:  yes", "usable by claims: yes", "expires in", "id token:       present, not a JWT", "acct-123…7890"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output lacks %q:\n%s", want, stdout)
		}
	}
	for _, secret := range []string{access, "SECRET-REFRESH", "opaque-id", "acct-1234567890"} {
		if strings.Contains(stdout, secret) {
			t.Errorf("status output contains %q", secret)
		}
	}

	code, stdout, _ = execute(t, "status", "--config", config, "--json")
	var report map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &report) != nil || report["usable_by_claims"] != true || report["refresh_token_present"] != true {
		t.Fatalf("--json: code %d %s", code, stdout)
	}
	if strings.Contains(stdout, access) || strings.Contains(stdout, "SECRET-REFRESH") {
		t.Fatal("--json output contains token text")
	}

	seed(t, auth, map[string]any{"access_token": accessJWT(t, fixedNow.Add(-5*time.Hour), time.Hour, "a"), "account_id": "a", "refresh_token": "r"}, fixedNow.Format(time.RFC3339))
	if code, stdout, _ = execute(t, "status", "--config", config); code != 2 || !strings.Contains(stdout, "EXPIRED") {
		t.Fatalf("expired: code %d %q", code, stdout)
	}
	// Scripts read the exit code of --json too.
	if code, stdout, _ = execute(t, "status", "--config", config, "--json"); code != 2 || !strings.Contains(stdout, `"usable_by_claims": false`) {
		t.Fatalf("expired with --json: code %d %q", code, stdout)
	}
	if err := os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	if code, _, _ = execute(t, "status", "--config", config, "--json"); code != 1 {
		t.Fatalf("missing file with --json: code %d, want 1", code)
	}
	if calls.Load() != 0 {
		t.Fatal("status must never contact the issuer")
	}
}

func TestRefreshCommand(t *testing.T) {
	newAccess := accessJWT(t, fixedNow, 10*24*time.Hour, "acct-1")
	body, _ := json.Marshal(map[string]any{"access_token": newAccess, "refresh_token": "rotated"})
	server, calls := issuer(t, 200, string(body))
	config, auth := configFor(t, server.URL, "")

	code, _, stderr := execute(t, "refresh", "--config", config)
	if code != exitNeedLogin || !strings.Contains(stderr, "login") || calls.Load() != 0 {
		t.Fatalf("no file: code %d %q calls %d", code, stderr, calls.Load())
	}

	seed(t, auth, map[string]any{"access_token": "old", "refresh_token": "old-refresh", "account_id": "acct-1"}, fixedNow.Add(-10*time.Minute).Format(time.RFC3339))
	code, stdout, _ := execute(t, "refresh", "--config", config, "--if-older-than", "1h")
	if code != 0 || !strings.Contains(stdout, "Not refreshed") || calls.Load() != 0 {
		t.Fatalf("recent: code %d %q calls %d", code, stdout, calls.Load())
	}
	code, stdout, stderr = execute(t, "refresh", "--config", config)
	if code != 0 || !strings.Contains(stdout, "Refreshed") || calls.Load() != 1 {
		t.Fatalf("refresh: code %d %q %q calls %d", code, stdout, stderr, calls.Load())
	}
	raw, _ := os.ReadFile(auth)
	if !strings.Contains(string(raw), "rotated") || strings.Contains(stdout+stderr, newAccess) || strings.Contains(stdout+stderr, "rotated") {
		t.Fatal("tokens must be stored and never printed")
	}
	if _, err := os.Stat(auth + ".bak"); err != nil {
		t.Fatal("the previous file must be kept as a backup")
	}
	if code, _, _ = execute(t, "refresh", "--config", config, "--if-older-than", "-1h"); code != exitUsage {
		t.Fatalf("negative duration: code %d", code)
	}
}

func TestRefreshExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"refresh token rejected", 400, `{"error":"invalid_grant"}`, exitNeedLogin},
		{"issuer down", 503, `{}`, exitFailed},
		{"rate limited", 429, `{"error":"slow_down"}`, exitFailed},
	}
	for _, c := range cases {
		server, _ := issuer(t, c.status, c.body)
		config, auth := configFor(t, server.URL, "")
		seed(t, auth, map[string]any{"access_token": "a", "refresh_token": "r", "account_id": "x"}, fixedNow.Format(time.RFC3339))
		before, _ := os.ReadFile(auth)
		code, _, stderr := execute(t, "refresh", "--config", config)
		if code != c.want {
			t.Errorf("%s: code %d, want %d (%q)", c.name, code, c.want, stderr)
		}
		if after, _ := os.ReadFile(auth); string(after) != string(before) {
			t.Errorf("%s: the file changed after a failed refresh", c.name)
		}
	}
}

func TestLoginCommandFlow(t *testing.T) {
	access := accessJWT(t, fixedNow, 10*24*time.Hour, "acct-login")
	body, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": "r1"})
	server, calls := issuer(t, 200, string(body))
	config, auth := configFor(t, server.URL, "")

	// No browser opens in a test: read the URL from stderr, play the user, then check the result.
	var out, errOut safeBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"login", "--config", config, "--no-browser"}, &out, &errOut, func() time.Time { return fixedNow })
	}()
	authorizeURL := waitForURL(t, &errOut)
	parsed, _ := url.Parse(authorizeURL)
	redirect := parsed.Query().Get("redirect_uri") + "?code=the-code&state=" + url.QueryEscape(parsed.Query().Get("state"))
	response, err := http.Get(redirect)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if code := <-done; code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut.String())
	}
	if calls.Load() != 1 || !strings.Contains(out.String(), "Signed in") {
		t.Fatalf("calls %d out %q", calls.Load(), out.String())
	}
	raw, _ := os.ReadFile(auth)
	if !strings.Contains(string(raw), access) {
		t.Fatal("the access token was not stored")
	}
	if strings.Contains(out.String()+errOut.String(), access) || strings.Contains(out.String()+errOut.String(), "the-code") {
		t.Fatal("tokens or the code appeared in the output")
	}

	// A second login needs --force.
	code, _, stderr := execute(t, "login", "--config", config, "--no-browser")
	if code != exitFailed || !strings.Contains(stderr, "--force") {
		t.Fatalf("second login: code %d %q", code, stderr)
	}
}

func TestLoginTimeoutExit(t *testing.T) {
	server, _ := issuer(t, 200, `{}`)
	config, _ := configFor(t, server.URL, `,"login_timeout_ms":100`)
	// configFor already sets login_timeout_ms; the later key wins only if the JSON decoder keeps
	// the last duplicate, which encoding/json does.
	code, _, stderr := execute(t, "login", "--config", config, "--no-browser")
	if code != exitFailed || !strings.Contains(stderr, "timed out after 100 ms") {
		t.Fatalf("code %d %q", code, stderr)
	}
}
