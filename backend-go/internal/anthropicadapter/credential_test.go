package anthropicadapter

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCredential writes a claude-auth file. expires is relative to the real clock because the
// handler reads the clock through h.now, which these tests leave at time.Now.
func writeCredential(t *testing.T, keys map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-oauth.json")
	raw, _ := json.Marshal(keys)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validCredential(token string, expiresIn time.Duration) map[string]any {
	return map[string]any{
		"version": 1, "access_token": token, "refresh_token": "REFRESH-SECRET",
		"expires_at": time.Now().Add(expiresIn).UTC().Format(time.RFC3339), "future_key": "ignored",
	}
}

func adapterWithCredential(t *testing.T, upstreamURL, path, extra string) *httptest.Server {
	t.Helper()
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstreamURL+`","credential_file":"`+path+`"`+extra+`}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	return server
}

const simpleRequest = `{"model":"claude-x","input":"hi"}`

func TestCredentialFileTokenReplacesTheCallersToken(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	path := writeCredential(t, validCredential("sk-ant-oat01-FILE", time.Hour))
	adapter := adapterWithCredential(t, upstream.URL, path, "")
	// The caller sends no credential at all: the file supplies it.
	if status, _, raw := post(t, adapter.URL+"/v1/responses", simpleRequest, nil); status != 200 {
		t.Fatalf("status %d body %s", status, raw)
	}
	// A caller token is ignored, not forwarded.
	if status, _, raw := post(t, adapter.URL+"/v1/responses", simpleRequest, map[string]string{"Authorization": "Bearer caller-token"}); status != 200 {
		t.Fatalf("status %d body %s", status, raw)
	}
	for index, call := range *calls {
		if call.Header.Get("Authorization") != "Bearer sk-ant-oat01-FILE" || call.Header.Get("x-api-key") != "" {
			t.Errorf("call %d: Authorization=%q x-api-key=%q", index, call.Header.Get("Authorization"), call.Header.Get("x-api-key"))
		}
	}
}

func TestCredentialFileHonoursAuthHeader(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	path := writeCredential(t, validCredential("tok-file", time.Hour))
	adapter := adapterWithCredential(t, upstream.URL, path, `,"auth_header":"x-api-key"`)
	post(t, adapter.URL+"/v1/responses", simpleRequest, nil)
	if call := (*calls)[0]; call.Header.Get("x-api-key") != "tok-file" || call.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization=%q x-api-key=%q", call.Header.Get("Authorization"), call.Header.Get("x-api-key"))
	}
}

func TestCredentialFileIsReadOnEveryRequest(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	path := writeCredential(t, validCredential("first", time.Hour))
	adapter := adapterWithCredential(t, upstream.URL, path, "")
	post(t, adapter.URL+"/v1/responses", simpleRequest, nil)
	raw, _ := json.Marshal(validCredential("second", time.Hour))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	post(t, adapter.URL+"/v1/responses", simpleRequest, nil)
	if (*calls)[0].Header.Get("Authorization") != "Bearer first" || (*calls)[1].Header.Get("Authorization") != "Bearer second" {
		t.Fatalf("a refreshed file was not picked up: %q then %q", (*calls)[0].Header.Get("Authorization"), (*calls)[1].Header.Get("Authorization"))
	}
}

func TestCredentialFileProblemsAnswer401WithoutCallingUpstream(t *testing.T) {
	cases := map[string]struct {
		keys map[string]any
		want string
	}{
		"expired":           {validCredential("T", -time.Hour), "expired at"},
		"inside the margin": {validCredential("T", time.Minute), "expired at"}, // default margin is 5 minutes
		"no access token":   {map[string]any{"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, "no access token"},
		"no expiry":         {map[string]any{"access_token": "T"}, "no valid expires_at"},
		"bad expiry":        {map[string]any{"access_token": "T", "expires_at": "soon"}, "no valid expires_at"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
			adapter := adapterWithCredential(t, upstream.URL, writeCredential(t, c.keys), "")
			status, _, raw := post(t, adapter.URL+"/v1/responses", simpleRequest, nil)
			if status != 401 || !strings.Contains(raw, c.want) || !strings.Contains(raw, `"code":"credential_unusable"`) ||
				!strings.Contains(raw, `"type":"authentication_error"`) || len(*calls) != 0 {
				t.Fatalf("status %d, upstream calls %d, body %s", status, len(*calls), raw)
			}
			if strings.Contains(raw, "REFRESH-SECRET") || strings.Contains(raw, `"T"`) {
				t.Fatalf("body shows a secret: %s", raw)
			}
		})
	}
}

func TestCredentialFileMissingOrCorrupt(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	missing := filepath.Join(t.TempDir(), "nope.json")
	adapter := adapterWithCredential(t, upstream.URL, missing, "")
	if status, _, raw := post(t, adapter.URL+"/v1/responses", simpleRequest, nil); status != 401 || !strings.Contains(raw, "file not found") {
		t.Fatalf("missing: %d %s", status, raw)
	}
	corrupt := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(corrupt, []byte(`{"access_token":"SECRET-IN-FILE"`), 0o600)
	adapter = adapterWithCredential(t, upstream.URL, corrupt, "")
	status, _, raw := post(t, adapter.URL+"/v1/responses", simpleRequest, nil)
	if status != 401 || strings.Contains(raw, "SECRET-IN-FILE") || !strings.Contains(raw, "not valid JSON") {
		t.Fatalf("corrupt: %d %s", status, raw)
	}
	if len(*calls) != 0 {
		t.Fatal("upstream was called")
	}
}

func TestExpiryMarginIsConfigurable(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	path := writeCredential(t, validCredential("T", time.Minute))
	adapter := adapterWithCredential(t, upstream.URL, path, `,"expiry_margin_ms":0`)
	if status, _, raw := post(t, adapter.URL+"/v1/responses", simpleRequest, nil); status != 200 || len(*calls) != 1 {
		t.Fatalf("margin 0 must accept a token with a minute left: %d %s", status, raw)
	}
}

func TestStreamingUsesTheCredentialFileToo(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", nil)
	path := writeCredential(t, validCredential("tok-stream", time.Hour))
	adapter := adapterWithCredential(t, upstream.URL, path, "")
	post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi","stream":true}`, nil)
	if len(*calls) == 0 || (*calls)[0].Header.Get("Authorization") != "Bearer tok-stream" {
		t.Fatalf("calls %+v", *calls)
	}
	// An unusable file must fail a streaming request before any stream starts.
	expired := adapterWithCredential(t, upstream.URL, writeCredential(t, validCredential("T", -time.Hour)), "")
	status, header, raw := post(t, expired.URL+"/v1/responses", `{"model":"claude-x","input":"hi","stream":true}`, nil)
	if status != 401 || strings.HasPrefix(header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("streaming with an expired credential: %d %s", status, raw)
	}
}

func TestWithoutCredentialFileTheCallersTokenIsRequired(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	if status, _, _ := post(t, adapter.URL+"/v1/responses", simpleRequest, nil); status != 401 || len(*calls) != 0 {
		t.Fatalf("status %d calls %d", status, len(*calls))
	}
}

func TestExtraHeadersAreSentOnEveryRequest(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","extra_headers":{"X-App":"cli","User-Agent":"ua/1"}}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	post(t, server.URL+"/v1/responses", simpleRequest, map[string]string{"Authorization": "Bearer t"})
	call := (*calls)[len(*calls)-1]
	if call.Header.Get("X-App") != "cli" || call.Header.Get("User-Agent") != "ua/1" || call.Header.Get("Authorization") != "Bearer t" {
		t.Fatalf("headers %v", call.Header)
	}
}

func TestExpandCredentialPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got, err := ExpandCredentialPath("~/.jad/auth/x.json"); err != nil || got != filepath.Join(home, ".jad", "auth", "x.json") {
		t.Errorf("tilde: %q %v", got, err)
	}
	for _, bad := range []string{"", "  ", "rel/x.json", "~user/x"} {
		if _, err := ExpandCredentialPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

const claudeCodeSentence = "You are Claude Code, Anthropic's official CLI for Claude."

func sentSystem(t *testing.T, body string) []struct {
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control"`
} {
	t.Helper()
	var sent struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	return sent.System
}

func TestSystemPrefixIsTheFirstBlockAndTheCallersInstructionsFollow(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","system_prefix":`+strconvQuote(claudeCodeSentence)+`},"cache":{"system":true}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	post(t, server.URL+"/v1/responses", `{"model":"claude-x","instructions":"Be brief.","input":[{"role":"developer","content":"also this"},{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer t"})
	system := sentSystem(t, (*calls)[0].Body)
	if len(system) != 3 || system[0].Text != claudeCodeSentence || system[1].Text != "Be brief." || system[2].Text != "also this" {
		t.Fatalf("system blocks wrong: %+v", system)
	}
	// The prefix is a block of its own, never merged into the caller's text.
	if strings.Contains(system[1].Text, "Claude Code") {
		t.Fatal("the prefix was merged into the instructions")
	}
	// The cache marker still sits on the last block, so the cached prefix covers all three.
	if len(system[0].CacheControl) != 0 || len(system[2].CacheControl) == 0 {
		t.Fatalf("cache marker misplaced: %+v", system)
	}
}

func TestSystemPrefixAloneWhenTheCallerSendsNoInstructions(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","system_prefix":`+strconvQuote(claudeCodeSentence)+`}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	post(t, server.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer t"})
	if system := sentSystem(t, (*calls)[0].Body); len(system) != 1 || system[0].Text != claudeCodeSentence {
		t.Fatalf("system blocks wrong: %+v", system)
	}
}

func TestNoSystemPrefixByDefault(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer t"})
	if strings.Contains((*calls)[0].Body, `"system"`) {
		t.Fatalf("a system field was sent without instructions or a prefix: %s", (*calls)[0].Body)
	}
}

func strconvQuote(s string) string { raw, _ := json.Marshal(s); return string(raw) }

const callerToken = "caller-token-0123456789abcdef0123456789"

func adapterWithCallerToken(t *testing.T) (*httptest.Server, *[]upstreamCall) {
	t.Helper()
	t.Setenv("ADAPTER_CALLER_TOKEN", callerToken)
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	path := writeCredential(t, validCredential("sk-ant-oat01-FILE", time.Hour))
	config := mustConfig(t, `{"caller_token_env":"ADAPTER_CALLER_TOKEN","upstream":{"base_url":"`+upstream.URL+`","credential_file":"`+path+`"}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	return server, calls
}

func TestCallerTokenGuardsTheCredentialFile(t *testing.T) {
	adapter, calls := adapterWithCallerToken(t)
	url := adapter.URL + "/v1/responses"
	for name, headers := range map[string]map[string]string{
		"missing":       nil,
		"wrong bearer":  {"Authorization": "Bearer wrong-token"},
		"wrong api key": {"x-api-key": "wrong-token"},
		"prefix":        {"Authorization": "Bearer " + callerToken[:10]},
	} {
		if status, _, raw := post(t, url, simpleRequest, headers); status != 401 {
			t.Errorf("%s: status %d body %s", name, status, raw)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("upstream called %d times for refused callers", len(*calls))
	}
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer " + callerToken},
		{"x-api-key": callerToken},
	} {
		if status, _, raw := post(t, url, simpleRequest, headers); status != 200 {
			t.Fatalf("right token: status %d body %s", status, raw)
		}
	}
	if got := (*calls)[0].Header.Get("Authorization"); got != "Bearer sk-ant-oat01-FILE" {
		t.Errorf("upstream got %q: the caller token must not be forwarded", got)
	}
}

func TestBrowserOriginIsRefusedWhenTheAdapterHoldsACredential(t *testing.T) {
	adapter, calls := adapterWithCallerToken(t)
	headers := map[string]string{"Authorization": "Bearer " + callerToken, "Origin": "https://evil.example"}
	if status, _, _ := post(t, adapter.URL+"/v1/responses", simpleRequest, headers); status != 403 {
		t.Fatalf("status %d, want 403", status)
	}
	if len(*calls) != 0 {
		t.Fatal("upstream was called")
	}
}

func TestCallerTokenIsOptionalWithoutIt(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 200, messageBody, nil)
	path := writeCredential(t, validCredential("tok", time.Hour))
	adapter := adapterWithCredential(t, upstream.URL, path, "")
	if status, _, raw := post(t, adapter.URL+"/v1/responses", simpleRequest, nil); status != 200 {
		t.Fatalf("status %d body %s", status, raw)
	}
}

func TestCallerTokenConfigValidation(t *testing.T) {
	path := writeCredential(t, validCredential("tok", time.Hour))
	t.Setenv("ADAPTER_SHORT", "short")
	t.Setenv("ADAPTER_OK", callerToken)
	cases := map[string]string{
		"unset variable":          `{"caller_token_env":"ADAPTER_UNSET_XYZ","upstream":{"credential_file":"` + path + `"}}`,
		"short token":             `{"caller_token_env":"ADAPTER_SHORT","upstream":{"credential_file":"` + path + `"}}`,
		"bad name":                `{"caller_token_env":"1 bad","upstream":{"credential_file":"` + path + `"}}`,
		"without credential file": `{"caller_token_env":"ADAPTER_OK"}`,
	}
	for name, text := range cases {
		file := filepath.Join(t.TempDir(), "c.json")
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(file); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCredentialErrorBodyOmitsThePathAndTheLogKeepsIt(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 200, messageBody, nil)
	path := filepath.Join(t.TempDir(), "missing", "claude-oauth.json")
	handler := NewHandler(mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","credential_file":"`+path+`"}}`), nil)
	var logs bytes.Buffer
	handler.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	status, _, raw := post(t, server.URL+"/v1/responses", simpleRequest, nil)
	if status != 401 || strings.Contains(raw, path) || strings.Contains(raw, filepath.Dir(path)) {
		t.Fatalf("status %d, body shows the path: %s", status, raw)
	}
	if !strings.Contains(raw, "file not found; sign in with claude-auth") || !strings.Contains(raw, `"code":"credential_unusable"`) {
		t.Fatalf("message is not the stable one: %s", raw)
	}
	if !strings.Contains(logs.String(), path) {
		t.Fatalf("the log should name the file:\n%s", logs.String())
	}
}
