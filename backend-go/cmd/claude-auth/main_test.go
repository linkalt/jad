package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func now() time.Time { return fixedNow }

// safeBuffer is a buffer that a command goroutine writes and a test reads at the same time.
type safeBuffer struct {
	mutex sync.Mutex
	text  strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.text.Write(p)
}

func (b *safeBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.text.String()
}

var urlPattern = regexp.MustCompile(`https?://\S+/oauth/authorize\?\S+`)

func waitForURL(t *testing.T, out *safeBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if found := urlPattern.FindString(out.String()); found != "" {
			return found
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no authorization URL printed; output: %q", out.String())
	return ""
}

type env struct {
	dir        string
	authFile   string
	configFile string
	issuer     *httptest.Server
	calls      *[]map[string]any
}

// newEnv writes a config whose token endpoint is a fake issuer answering every request with reply.
func newEnv(t *testing.T, status int, reply string) *env {
	t.Helper()
	var mutex sync.Mutex
	var calls []map[string]any
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mutex.Lock()
		calls = append(calls, body)
		mutex.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(issuer.Close)
	dir := t.TempDir()
	e := &env{dir: dir, authFile: filepath.Join(dir, "auth", "claude-oauth.json"), configFile: filepath.Join(dir, "claude-auth.json"), issuer: issuer, calls: &calls}
	config := fmt.Sprintf(`{"auth_file":%q,"token_url":%q,"authorize_url":%q,"login_port":0,"login_timeout_ms":2000,"open_browser":false}`,
		e.authFile, issuer.URL+"/v1/oauth/token", issuer.URL+"/oauth/authorize")
	if err := os.WriteFile(e.configFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) writeCredentials(t *testing.T, expires time.Time, refresh string) {
	t.Helper()
	keys := map[string]any{
		"version": 1, "access_token": "ACCESS-SECRET", "expires_at": expires.UTC().Format(time.RFC3339),
		"scope": "user:inference", "last_refresh": fixedNow.Add(-time.Hour).UTC().Format(time.RFC3339),
	}
	if refresh != "" {
		keys["refresh_token"] = refresh
	}
	raw, _ := json.Marshal(keys)
	if err := os.MkdirAll(filepath.Dir(e.authFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.authFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, now)
	return code, stdout.String(), stderr.String()
}

func TestUsageAndUnknownCommand(t *testing.T) {
	if code, _, stderr := runCommand(t); code != exitUsage || !strings.Contains(stderr, "Usage: claude-auth") {
		t.Errorf("no args: %d %q", code, stderr)
	}
	if code, stdout, _ := runCommand(t, "help"); code != exitOK || !strings.Contains(stdout, "login") {
		t.Errorf("help: %d", code)
	}
	if code, _, stderr := runCommand(t, "nope"); code != exitUsage || !strings.Contains(stderr, `unknown command "nope"`) {
		t.Errorf("unknown: %d %q", code, stderr)
	}
	if code, _, _ := runCommand(t, "status", "extra"); code != exitUsage {
		t.Errorf("extra argument: %d", code)
	}
}

func TestBadConfigFailsBeforeAnyWork(t *testing.T) {
	e := newEnv(t, 200, `{}`)
	_ = os.WriteFile(e.configFile, []byte(`{"bogus":1}`), 0o600)
	for _, command := range []string{"login", "refresh", "status"} {
		code, _, stderr := runCommand(t, command, "--config", e.configFile)
		if code != exitFailed || !strings.Contains(stderr, "unknown field") {
			t.Errorf("%s: %d %q", command, code, stderr)
		}
	}
	if len(*e.calls) != 0 {
		t.Fatal("the issuer was called")
	}
}

func TestStatusCommand(t *testing.T) {
	e := newEnv(t, 200, `{}`)
	code, stdout, _ := runCommand(t, "status", "--config", e.configFile)
	if code != 1 || !strings.Contains(stdout, "not found") {
		t.Errorf("missing file: %d %q", code, stdout)
	}
	e.writeCredentials(t, fixedNow.Add(2*time.Hour), "REFRESH-SECRET")
	code, stdout, _ = runCommand(t, "status", "--config", e.configFile)
	if code != 0 || !strings.Contains(stdout, "expires in 2h0m0s") || !strings.Contains(stdout, "usable by expiry: yes") {
		t.Errorf("valid: %d %q", code, stdout)
	}
	if strings.Contains(stdout, "ACCESS-SECRET") || strings.Contains(stdout, "REFRESH-SECRET") {
		t.Fatal("status printed a token")
	}
	code, stdout, _ = runCommand(t, "status", "--json", "--config", e.configFile)
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || code != 0 || report["usable_by_expiry"] != true {
		t.Errorf("json: %d %v %q", code, err, stdout)
	}
	if strings.Contains(stdout, "SECRET") {
		t.Fatal("status --json printed a token")
	}
	e.writeCredentials(t, fixedNow.Add(-time.Hour), "REFRESH-SECRET")
	if code, stdout, _ = runCommand(t, "status", "--config", e.configFile); code != 2 || !strings.Contains(stdout, "EXPIRED") {
		t.Errorf("expired: %d %q", code, stdout)
	}
}

func TestRefreshCommand(t *testing.T) {
	e := newEnv(t, 200, `{"access_token":"NEW-ACCESS","refresh_token":"NEW-REFRESH","expires_in":28800}`)
	e.writeCredentials(t, fixedNow.Add(-time.Minute), "REFRESH-SECRET")
	code, stdout, stderr := runCommand(t, "refresh", "--config", e.configFile)
	if code != 0 || !strings.Contains(stdout, "Refreshed") || stderr != "" {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, "NEW-ACCESS") {
		t.Fatal("refresh printed a token")
	}
	var stored map[string]any
	raw, _ := os.ReadFile(e.authFile)
	_ = json.Unmarshal(raw, &stored)
	if stored["access_token"] != "NEW-ACCESS" || stored["refresh_token"] != "NEW-REFRESH" || (*e.calls)[0]["refresh_token"] != "REFRESH-SECRET" {
		t.Fatalf("stored wrong, issuer saw %v", (*e.calls)[0]["grant_type"])
	}
}

func TestRefreshSkipFlags(t *testing.T) {
	e := newEnv(t, 200, `{"access_token":"NEW","expires_in":60}`)
	e.writeCredentials(t, fixedNow.Add(3*time.Hour), "R")
	code, stdout, _ := runCommand(t, "refresh", "--if-older-than", "2h", "--config", e.configFile)
	if code != 0 || !strings.Contains(stdout, "Not refreshed: the tokens are 1h0m0s old, less than 2h0m0s") {
		t.Errorf("if-older-than: %d %q", code, stdout)
	}
	code, stdout, _ = runCommand(t, "refresh", "--if-expires-within", "30m", "--config", e.configFile)
	if code != 0 || !strings.Contains(stdout, "Not refreshed: the access token has 3h0m0s left, more than 30m0s") {
		t.Errorf("if-expires-within: %d %q", code, stdout)
	}
	if len(*e.calls) != 0 {
		t.Fatal("the issuer was called for a skipped refresh")
	}
	if code, _, _ := runCommand(t, "refresh", "--if-older-than", "-1m", "--config", e.configFile); code != exitUsage {
		t.Errorf("negative flag: %d", code)
	}
}

func TestRefreshExitCodes(t *testing.T) {
	e := newEnv(t, 400, `{"error":"invalid_grant"}`)
	if code, _, stderr := runCommand(t, "refresh", "--config", e.configFile); code != exitNeedLogin || !strings.Contains(stderr, "sign in again") {
		t.Errorf("missing file: %d %q", code, stderr)
	}
	e.writeCredentials(t, fixedNow, "R")
	if code, _, stderr := runCommand(t, "refresh", "--config", e.configFile); code != exitNeedLogin || !strings.Contains(stderr, "invalid_grant") {
		t.Errorf("rejected: %d %q", code, stderr)
	}
	e2 := newEnv(t, 503, `{}`)
	e2.writeCredentials(t, fixedNow, "R")
	if code, _, _ := runCommand(t, "refresh", "--config", e2.configFile); code != exitFailed {
		t.Errorf("server error: %d", code)
	}
}

func TestLoginCommandCallbackFlow(t *testing.T) {
	e := newEnv(t, 200, `{"access_token":"LOGIN-ACCESS","refresh_token":"LOGIN-REFRESH","expires_in":3600}`)
	var stdout, stderr safeBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"login", "--config", e.configFile}, strings.NewReader(""), &stdout, &stderr, now)
	}()
	authorize := waitForURL(t, &stderr)
	parsed := mustParse(t, authorize)
	response, err := http.Get(parsed.Query().Get("redirect_uri") + "?code=the-code&state=" + parsed.Query().Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Signed in") || strings.Contains(stdout.String()+stderr.String(), "LOGIN-ACCESS") {
		t.Fatalf("output %q / %q", stdout.String(), stderr.String())
	}
	raw, _ := os.ReadFile(e.authFile)
	if !strings.Contains(string(raw), "LOGIN-ACCESS") {
		t.Fatal("credentials not stored")
	}
	if code, _, stderr2 := runCommand(t, "login", "--config", e.configFile); code != exitFailed || !strings.Contains(stderr2, "--force") {
		t.Errorf("second login without --force: %d %q", code, stderr2)
	}
}

func TestLoginCommandCopyCodeFlow(t *testing.T) {
	e := newEnv(t, 200, `{"access_token":"CC-ACCESS","refresh_token":"CC-REFRESH","expires_in":3600}`)
	reader, writer := io.Pipe()
	var stdout, stderr safeBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"login", "--copy-code", "--config", e.configFile}, reader, &stdout, &stderr, now)
	}()
	state := mustParse(t, waitForURL(t, &stderr)).Query().Get("state")
	_, _ = io.WriteString(writer, "pasted-code#"+state+"\n")
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got := (*e.calls)[0]; got["code"] != "pasted-code" || got["redirect_uri"] != "https://platform.claude.com/oauth/code/callback" {
		t.Fatalf("exchange %v", got)
	}
}

func TestLoginTimeoutExit(t *testing.T) {
	e := newEnv(t, 200, `{}`)
	_ = os.WriteFile(e.configFile, []byte(fmt.Sprintf(`{"auth_file":%q,"login_port":0,"login_timeout_ms":50,"open_browser":false}`, e.authFile)), 0o600)
	if code, _, stderr := runCommand(t, "login", "--config", e.configFile); code != exitFailed || !strings.Contains(stderr, "timed out") {
		t.Errorf("%d %q", code, stderr)
	}
}
