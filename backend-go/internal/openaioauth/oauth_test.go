package openaioauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// jwt builds an unsigned token with the given claims; only the payload is read.
func jwt(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func writeAuth(t *testing.T, tokens map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	body, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": tokens, "last_refresh": "x", "other": 1})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func source(path string) *Source {
	return &Source{Path: path, ExpiryMargin: 5 * time.Minute, Now: func() time.Time { return testNow }}
}

func loadError(t *testing.T, path string) *CredentialError {
	t.Helper()
	_, err := source(path).Load()
	var credentialError *CredentialError
	if !errors.As(err, &credentialError) {
		t.Fatalf("Load error = %T %v, want *CredentialError", err, err)
	}
	return credentialError
}

func TestLoadReadsTokenAndAccountIDAndIgnoresUnknownKeys(t *testing.T) {
	access := jwt(map[string]any{"exp": testNow.Add(time.Hour).Unix()})
	credential, err := source(writeAuth(t, map[string]any{"access_token": access, "account_id": "acct-1", "refresh_token": "r"})).Load()
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != access || credential.AccountID != "acct-1" || credential.FedRAMP {
		t.Errorf("credential fields differ: account %q fedramp %v", credential.AccountID, credential.FedRAMP)
	}
}

func TestLoadTakesTheAccountIDFromTheTokensWhenTheFileHasNone(t *testing.T) {
	exp := testNow.Add(time.Hour).Unix()
	cases := map[string]map[string]any{
		"id token auth claim":    {"access_token": jwt(map[string]any{"exp": exp}), "id_token": jwt(map[string]any{authClaim: map[string]any{"chatgpt_account_id": "from-auth-claim"}})},
		"id token top level":     {"access_token": jwt(map[string]any{"exp": exp}), "id_token": jwt(map[string]any{"chatgpt_account_id": "from-auth-claim"})},
		"id token organizations": {"access_token": jwt(map[string]any{"exp": exp}), "id_token": jwt(map[string]any{"organizations": []any{map[string]any{"id": "from-auth-claim"}}})},
		"access token claim":     {"access_token": jwt(map[string]any{"exp": exp, authClaim: map[string]any{"chatgpt_account_id": "from-auth-claim"}})},
	}
	for name, tokens := range cases {
		t.Run(name, func(t *testing.T) {
			credential, err := source(writeAuth(t, tokens)).Load()
			if err != nil || credential.AccountID != "from-auth-claim" {
				t.Fatalf("account = %q, error %v", credential.AccountID, err)
			}
		})
	}
}

func TestLoadFlagsFedRAMPFromEitherToken(t *testing.T) {
	exp := testNow.Add(time.Hour).Unix()
	fed := map[string]any{authClaim: map[string]any{"chatgpt_account_is_fedramp": true}}
	plain := map[string]any{"exp": exp}
	access := map[string]any{"exp": exp, authClaim: map[string]any{"chatgpt_account_is_fedramp": true}}
	for name, tokens := range map[string]map[string]any{
		"id token":     {"access_token": jwt(plain), "id_token": jwt(fed), "account_id": "a"},
		"access token": {"access_token": jwt(access), "account_id": "a"},
	} {
		credential, err := source(writeAuth(t, tokens)).Load()
		if err != nil || !credential.FedRAMP {
			t.Errorf("%s: fedramp = %v, error %v", name, credential.FedRAMP, err)
		}
	}
}

func TestLoadRejectsUnusableFilesWithAPermanentErrorThatNamesTheFile(t *testing.T) {
	exp := testNow.Add(time.Hour).Unix()
	dir := t.TempDir()
	notJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(notJSON, []byte("secret-looking-text {"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		path string
		want string
	}{
		"missing file":    {filepath.Join(dir, "none.json"), "file not found"},
		"directory":       {dir, "not a regular file"},
		"not JSON":        {notJSON, "not valid JSON"},
		"no access token": {writeAuth(t, map[string]any{"account_id": "a"}), "no access token"},
		"expired":         {writeAuth(t, map[string]any{"access_token": jwt(map[string]any{"exp": testNow.Add(-time.Hour).Unix()}), "account_id": "a"}), "expired at 2026-10-08T11:00:00Z"},
		"no account id":   {writeAuth(t, map[string]any{"access_token": jwt(map[string]any{"exp": exp})}), "no account id"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			credentialError := loadError(t, c.path)
			if !strings.Contains(credentialError.Error(), c.want) || !strings.Contains(credentialError.Error(), c.path) {
				t.Errorf("error = %q, want it to name %q and %q", credentialError.Error(), c.want, c.path)
			}
			if !credentialError.Permanent() {
				t.Error("an unusable credential must be permanent so the retry loop skips it")
			}
			if strings.Contains(credentialError.Error(), "secret-looking") {
				t.Error("the error quotes file content")
			}
		})
	}
}

func TestLoadTreatsATokenInsideTheExpiryMarginAsExpired(t *testing.T) {
	soon := writeAuth(t, map[string]any{"access_token": jwt(map[string]any{"exp": testNow.Add(4 * time.Minute).Unix()}), "account_id": "a"})
	if got := loadError(t, soon); !strings.Contains(got.Error(), "expired") {
		t.Errorf("error = %q", got)
	}
	exactly := writeAuth(t, map[string]any{"access_token": jwt(map[string]any{"exp": testNow.Add(5 * time.Minute).Unix()}), "account_id": "a"})
	if got := loadError(t, exactly); !strings.Contains(got.Error(), "expired") {
		t.Errorf("a token expiring exactly at the margin must count as expired: %q", got)
	}
	later := writeAuth(t, map[string]any{"access_token": jwt(map[string]any{"exp": testNow.Add(5*time.Minute + time.Second).Unix()}), "account_id": "a"})
	if _, err := source(later).Load(); err != nil {
		t.Errorf("a token one second beyond the margin must pass: %v", err)
	}
	zero := &Source{Path: soon, Now: func() time.Time { return testNow }}
	if _, err := zero.Load(); err != nil {
		t.Errorf("margin 0 must accept a token that is still valid: %v", err)
	}
}

func TestLoadSendsATokenWithoutReadableExpiryAsIs(t *testing.T) {
	credential, err := source(writeAuth(t, map[string]any{"access_token": "opaque-token", "account_id": "a"})).Load()
	if err != nil || credential.AccessToken != "opaque-token" {
		t.Fatalf("opaque token: %v", err)
	}
}

func TestLoadPicksUpARefreshedFileOnTheNextCall(t *testing.T) {
	exp := testNow.Add(time.Hour).Unix()
	path := writeAuth(t, map[string]any{"access_token": jwt(map[string]any{"exp": testNow.Add(-time.Hour).Unix()}), "account_id": "a"})
	s := source(path)
	if _, err := s.Load(); err == nil {
		t.Fatal("expired token loaded")
	}
	fresh := jwt(map[string]any{"exp": exp})
	body, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": fresh, "account_id": "a"}})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	credential, err := s.Load()
	if err != nil || credential.AccessToken != fresh {
		t.Fatalf("after the file was refreshed: %v", err)
	}
}

func TestCredentialNeverPrintsTheToken(t *testing.T) {
	credential := Credential{AccessToken: "TOKEN-VALUE", AccountID: "ACCOUNT"}
	for _, text := range []string{fmt.Sprint(credential), fmt.Sprintf("%v", credential), fmt.Sprintf("%+v", credential), fmt.Sprintf("%#v", credential)} {
		if strings.Contains(text, "TOKEN-VALUE") {
			t.Errorf("formatted credential shows the token: %s", text)
		}
	}
}

func TestTransportSetsOnlyTheCredentialHeadersAndKeepsTheRequest(t *testing.T) {
	access := jwt(map[string]any{"exp": testNow.Add(time.Hour).Unix()})
	var got *http.Request
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	transport := &Transport{BaseURL: server.URL, Source: source(writeAuth(t, map[string]any{"access_token": access, "account_id": "acct-9"}))}

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/codex/responses?x=1", strings.NewReader(`{"model":"m","stream":true}`))
	request.Header.Set("Authorization", "Bearer placeholder-key")
	request.Header.Set("OpenAI-Beta", "responses=experimental")
	request.Header.Set("X-OpenAI-Fedramp", "true")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Custom", "keep")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	if got.Header.Get("Authorization") != "Bearer "+access || got.Header.Get("chatgpt-account-id") != "acct-9" {
		t.Errorf("credential headers wrong: auth set %v, account %q", got.Header.Get("Authorization") == "Bearer "+access, got.Header.Get("chatgpt-account-id"))
	}
	if got.Header.Get("OpenAI-Beta") != "" || got.Header.Get("X-OpenAI-Fedramp") != "" {
		t.Errorf("headers the transport owns survived: beta %q fedramp %q", got.Header.Get("OpenAI-Beta"), got.Header.Get("X-OpenAI-Fedramp"))
	}
	if got.Header.Get("Content-Type") != "application/json" || got.Header.Get("X-Custom") != "keep" {
		t.Error("the caller's other headers were changed")
	}
	if got.Method != http.MethodPost || got.URL.RequestURI() != "/codex/responses?x=1" || gotBody != `{"model":"m","stream":true}` {
		t.Errorf("request changed: %s %s body %q", got.Method, got.URL.RequestURI(), gotBody)
	}
	if request.Header.Get("Authorization") != "Bearer placeholder-key" {
		t.Error("the caller's request was modified")
	}
}

func TestTransportSendsFedRAMPOnlyForAFedRAMPAccount(t *testing.T) {
	exp := testNow.Add(time.Hour).Unix()
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Get("X-OpenAI-Fedramp") }))
	defer server.Close()
	access := jwt(map[string]any{"exp": exp, authClaim: map[string]any{"chatgpt_account_is_fedramp": true}})
	transport := &Transport{BaseURL: server.URL, Source: source(writeAuth(t, map[string]any{"access_token": access, "account_id": "a"}))}
	request, _ := http.NewRequest(http.MethodPost, server.URL, nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if seen != "true" {
		t.Errorf("X-OpenAI-Fedramp = %q, want true", seen)
	}
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

func TestTransportSendsNothingAndClosesTheBodyWhenTheCredentialIsUnusable(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer server.Close()
	transport := &Transport{BaseURL: server.URL, Source: source(filepath.Join(t.TempDir(), "missing.json"))}
	body := &closeTracker{Reader: strings.NewReader("{}")}
	request, _ := http.NewRequest(http.MethodPost, server.URL, body)
	_, err := transport.RoundTrip(request)
	var credentialError *CredentialError
	if !errors.As(err, &credentialError) {
		t.Fatalf("error = %T %v", err, err)
	}
	if hits != 0 {
		t.Errorf("%d requests reached the server", hits)
	}
	if !body.closed {
		t.Error("the request body was not closed")
	}
	if _, err := (&Transport{}).RoundTrip(request); err == nil {
		t.Error("a transport without a source must fail")
	}
}

func redirectFixture(t *testing.T, access string) (first *httptest.Server, hits *[]http.Header) {
	t.Helper()
	var seen []http.Header
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
	}))
	t.Cleanup(second.Close)
	first = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/landing", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(first.Close)
	return first, &seen
}

func TestTransportDoesNotSendTheCredentialToAnotherOriginAfterARedirect(t *testing.T) {
	access := jwt(map[string]any{"exp": testNow.Add(time.Hour).Unix()})
	first, seen := redirectFixture(t, access)
	transport := &Transport{BaseURL: first.URL, Source: source(writeAuth(t, map[string]any{"access_token": access, "account_id": "acct-1"}))}
	client := &http.Client{Transport: transport}
	response, err := client.Post(first.URL+"/responses", "application/json", strings.NewReader("{}"))
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("the redirect to another origin succeeded")
	}
	for _, header := range *seen {
		t.Errorf("second origin received Authorization %q account %q", header.Get("Authorization"), header.Get("Chatgpt-Account-Id"))
	}
	if len(*seen) != 0 {
		t.Errorf("second origin was reached %d times", len(*seen))
	}
}

func TestTransportSendsTheCredentialToTheConfiguredOrigin(t *testing.T) {
	access := jwt(map[string]any{"exp": testNow.Add(time.Hour).Unix()})
	var auth, account string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, account = r.Header.Get("Authorization"), r.Header.Get("Chatgpt-Account-Id")
	}))
	defer server.Close()
	transport := &Transport{BaseURL: server.URL + "/codex", Source: source(writeAuth(t, map[string]any{"access_token": access, "account_id": "acct-1"}))}
	response, err := (&http.Client{Transport: transport}).Post(server.URL+"/codex/responses", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if auth != "Bearer "+access || account != "acct-1" {
		t.Errorf("credential headers missing on the configured origin: %q %q", auth, account)
	}
}
