package claudeauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestExchangeSendsJSONWithStateAndVerifier(t *testing.T) {
	issuer := newFakeIssuer(t, reply{200, tokenBody("acc-1", "ref-1", 3600)})
	tokens, err := clientFor(t, issuer).Exchange(context.Background(), "the-code", "the-state", "the-verifier", "http://localhost:1/callback")
	if err != nil {
		t.Fatal(err)
	}
	call := issuer.Calls[0]
	want := map[string]string{
		"grant_type": "authorization_code", "client_id": DefaultClientID, "code": "the-code", "state": "the-state",
		"redirect_uri": "http://localhost:1/callback", "code_verifier": "the-verifier",
	}
	if call.ContentType != "application/json" || call.Path != "/v1/oauth/token" || len(call.JSON) != len(want) {
		t.Fatalf("request wrong: %+v", call)
	}
	for key, value := range want {
		if call.JSON[key] != value {
			t.Errorf("%s = %v, want %s", key, call.JSON[key], value)
		}
	}
	if tokens.AccessToken != "acc-1" || tokens.RefreshToken != "ref-1" || tokens.ExpiresIn != 3600 || tokens.Scope != "user:inference" {
		t.Fatalf("tokens wrong: %d %d", len(tokens.AccessToken), tokens.ExpiresIn)
	}
}

func TestRefreshSendsOnlyTheGrantAndKeepsTheOldRefreshToken(t *testing.T) {
	issuer := newFakeIssuer(t, reply{200, tokenBody("acc-2", "", 3600)})
	tokens, err := clientFor(t, issuer).Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	call := issuer.Calls[0]
	if len(call.JSON) != 3 || call.JSON["grant_type"] != "refresh_token" || call.JSON["refresh_token"] != "old-refresh" || call.JSON["client_id"] != DefaultClientID {
		t.Fatalf("refresh body wrong: %v", keys(call.JSON))
	}
	if tokens.RefreshToken != "old-refresh" {
		t.Fatal("a refresh response without a refresh token must keep the old one")
	}
}

func TestParseTokensRequiresAnExpiry(t *testing.T) {
	for name, body := range map[string]string{
		"no access token": `{"expires_in":3600}`,
		"no expiry":       `{"access_token":"a"}`,
		"zero expiry":     `{"access_token":"a","expires_in":0}`,
		"not json":        `<html>`,
	} {
		if _, err := parseTokens([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTokenErrorShapesAndRejection(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		contains string
		rejected bool
	}{
		{400, `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`, "invalid_grant (Refresh token not found or invalid)", true},
		{401, `{"type":"error","error":{"type":"authentication_error","message":"bad"}}`, "authentication_error (bad)", true},
		{429, `{"error":"rate_limited"}`, "rate_limited", false},
		{500, `oops`, "HTTP 500", false},
		{408, ``, "HTTP 408", false},
	}
	for _, c := range cases {
		issuer := newFakeIssuer(t, reply{c.status, c.body})
		_, err := clientFor(t, issuer).Refresh(context.Background(), "r")
		var tokenErr *TokenError
		if !errors.As(err, &tokenErr) || !strings.Contains(err.Error(), c.contains) || tokenErr.Rejected() != c.rejected {
			t.Errorf("status %d: err %v rejected=%v, want %q rejected=%v", c.status, err, tokenErr != nil && tokenErr.Rejected(), c.contains, c.rejected)
		}
	}
}

func TestTokenErrorNeverEchoesTheBodyOrTheRefreshToken(t *testing.T) {
	issuer := newFakeIssuer(t, reply{400, `{"error":"x","debug":"SECRET-BODY-TEXT sk-ant-oat01-LEAK"}`})
	_, err := clientFor(t, issuer).Refresh(context.Background(), "REFRESH-SECRET")
	if err == nil || strings.Contains(err.Error(), "SECRET-BODY-TEXT") || strings.Contains(err.Error(), "REFRESH-SECRET") {
		t.Fatalf("error leaks: %v", err)
	}
}

func TestLongDescriptionIsTruncated(t *testing.T) {
	issuer := newFakeIssuer(t, reply{400, fmt.Sprintf(`{"error":"x","error_description":%q}`, strings.Repeat("a", 5000))})
	_, err := clientFor(t, issuer).Refresh(context.Background(), "r")
	if err == nil || len(err.Error()) > 400 {
		t.Fatalf("description not bounded: %d bytes", len(err.Error()))
	}
}

func TestStringFormsHideTokens(t *testing.T) {
	tokens := Tokens{AccessToken: "TOP-SECRET", RefreshToken: "TOP-SECRET-2"}
	file := File{AccessToken: "TOP-SECRET", RefreshToken: "TOP-SECRET-2"}
	for _, text := range []string{
		fmt.Sprintf("%v %+v %#v %s", tokens, tokens, tokens, tokens),
		fmt.Sprintf("%v %+v %#v %s", file, file, file, file),
	} {
		if strings.Contains(text, "TOP-SECRET") {
			t.Fatalf("a token is printed: %s", text)
		}
	}
}

func TestPKCEMatchesRFC7636Example(t *testing.T) {
	// RFC 7636 appendix B.
	if got := codeChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge %s", got)
	}
}

func TestAuthRequestURL(t *testing.T) {
	config := DefaultConfig()
	if err := config.finish(); err != nil {
		t.Fatal(err)
	}
	request, err := NewAuthRequest(&config, "http://localhost:53692/callback", strings.NewReader(strings.Repeat("x", 200)))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(request.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != DefaultAuthorizeURL {
		t.Fatalf("base wrong: %s", parsed.Path)
	}
	for key, want := range map[string]string{
		"code": "true", "response_type": "code", "client_id": DefaultClientID, "redirect_uri": "http://localhost:53692/callback",
		"scope": DefaultScope, "state": request.State, "code_challenge": codeChallenge(request.Verifier), "code_challenge_method": "S256",
	} {
		if query.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, query.Get(key), want)
		}
	}
	if len(request.Verifier) < 43 || len(request.Verifier) > 128 || request.State == request.Verifier {
		t.Errorf("verifier %d chars, state equals verifier: %v", len(request.Verifier), request.State == request.Verifier)
	}
}

func TestAuthRequestFailsWhenRandomRunsOut(t *testing.T) {
	config := DefaultConfig()
	_ = config.finish()
	if _, err := NewAuthRequest(&config, "http://localhost/cb", strings.NewReader("short")); err == nil {
		t.Fatal("a short random source must fail")
	}
}

func TestParsePasted(t *testing.T) {
	cases := map[string][2]string{
		"abc#xyz":     {"abc", "xyz"},
		"  abc#xyz\n": {"abc", "xyz"},
		"abc":         {"abc", ""},
		"http://localhost:53692/callback?code=c1&state=s1": {"c1", "s1"},
		"code=c2&state=s2": {"c2", "s2"},
		"?code=c3":         {"c3", ""},
		"":                 {"", ""},
	}
	for input, want := range cases {
		code, state := ParsePasted(input)
		if code != want[0] || state != want[1] {
			t.Errorf("ParsePasted(%q) = %q, %q; want %q, %q", input, code, state, want[0], want[1])
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
