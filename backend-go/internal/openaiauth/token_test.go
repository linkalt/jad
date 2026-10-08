package openaiauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestExchangeSendsAFormAndParsesTokens(t *testing.T) {
	now := time.Now()
	access := accessJWT(t, now, time.Hour, "acct-1")
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "", "refresh-1")})
	tokens, err := clientFor(t, issuer).Exchange(context.Background(), "the-code", "the-verifier", "http://localhost:1455/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	call := issuer.Calls[0]
	if call.Path != "/oauth/token" || call.ContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("request wrong: %+v", call)
	}
	want := map[string]string{
		"grant_type": "authorization_code", "code": "the-code", "code_verifier": "the-verifier",
		"redirect_uri": "http://localhost:1455/auth/callback", "client_id": DefaultClientID,
	}
	for key, value := range want {
		if got := call.Form[key]; len(got) != 1 || got[0] != value {
			t.Errorf("form %s = %v, want %q", key, got, value)
		}
	}
	if tokens.AccessToken != access || tokens.RefreshToken != "refresh-1" || tokens.AccountID != "acct-1" || tokens.ExpiresIn != 3600 {
		t.Fatalf("tokens wrong (access token text not shown)")
	}
}

func TestRefreshSendsJSONAndKeepsTheOldRefreshTokenWhenNoneComesBack(t *testing.T) {
	access := accessJWT(t, time.Now(), time.Hour, "acct-1")
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "", "")}, reply{200, tokenBody(t, access, "", "rotated")})
	client := clientFor(t, issuer)
	tokens, err := client.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	call := issuer.Calls[0]
	if call.ContentType != "application/json" || call.JSON["grant_type"] != "refresh_token" ||
		call.JSON["refresh_token"] != "old-refresh" || call.JSON["client_id"] != DefaultClientID {
		t.Fatalf("request wrong: %+v", call)
	}
	if tokens.RefreshToken != "old-refresh" {
		t.Fatal("a response without a refresh token must keep the old one")
	}
	if tokens, _ = client.Refresh(context.Background(), "old-refresh"); tokens.RefreshToken != "rotated" {
		t.Fatal("a rotated refresh token must replace the old one")
	}
}

func TestTokenErrors(t *testing.T) {
	cases := []struct {
		name     string
		reply    reply
		want     string
		rejected bool
	}{
		{"oauth error", reply{400, `{"error":"invalid_grant","error_description":"Refresh token reused"}`}, "invalid_grant (Refresh token reused)", true},
		{"error object", reply{401, `{"error":{"code":"token_expired","message":"expired"}}`}, "token_expired (expired)", true},
		{"message only", reply{403, `{"message":"blocked"}`}, "HTTP 403 (blocked)", true},
		{"rate limited", reply{429, `{"error":"slow_down"}`}, "slow_down", false},
		{"server error", reply{503, `<html>oops</html>`}, "HTTP 503", false},
		{"timeout status", reply{408, ``}, "HTTP 408", false},
	}
	for _, c := range cases {
		issuer := newFakeIssuer(t, c.reply)
		_, err := clientFor(t, issuer).Refresh(context.Background(), "secret-refresh-token")
		var tokenErr *TokenError
		if !errors.As(err, &tokenErr) {
			t.Errorf("%s: want *TokenError, got %v", c.name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.want) || tokenErr.Rejected() != c.rejected {
			t.Errorf("%s: error %q rejected=%v, want %q rejected=%v", c.name, err, tokenErr.Rejected(), c.want, c.rejected)
		}
		if strings.Contains(err.Error(), "secret-refresh-token") {
			t.Errorf("%s: the error text repeats the refresh token", c.name)
		}
	}
}

func TestTokenErrorTextIsBoundedAndSingleLine(t *testing.T) {
	long := strings.Repeat("word ", 200)
	issuer := newFakeIssuer(t, reply{400, fmt.Sprintf(`{"error":"x","error_description":%q}`, "line one\n\n"+long)})
	_, err := clientFor(t, issuer).Refresh(context.Background(), "r")
	if err == nil || strings.Contains(err.Error(), "\n") || len(err.Error()) > 400 {
		t.Fatalf("error text not bounded: %d bytes %q", len(err.Error()), err)
	}
}

func TestMalformedSuccessResponses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        `not json`,
		"no access token": `{"refresh_token":"r"}`,
		"empty object":    `{}`,
	} {
		issuer := newFakeIssuer(t, reply{200, body})
		_, err := clientFor(t, issuer).Refresh(context.Background(), "r")
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
		var tokenErr *TokenError
		if errors.As(err, &tokenErr) {
			t.Errorf("%s: a malformed success body is not a *TokenError", name)
		}
	}
}

func TestOversizedResponseIsRefused(t *testing.T) {
	issuer := newFakeIssuer(t, reply{200, `{"access_token":"` + strings.Repeat("a", MaxTokenResponseBytes) + `"}`})
	if _, err := clientFor(t, issuer).Refresh(context.Background(), "r"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want an oversize error, got %v", err)
	}
}

func TestUnreachableIssuerAndCancel(t *testing.T) {
	issuer := newFakeIssuer(t)
	client := clientFor(t, issuer)
	issuer.Close()
	if _, err := client.Refresh(context.Background(), "r"); err == nil || strings.Contains(err.Error(), "&client") {
		t.Fatalf("want a short connection error, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Refresh(ctx, "r"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestTokensNeverPrint(t *testing.T) {
	tokens := Tokens{AccessToken: "SECRET-A", IDToken: "SECRET-I", RefreshToken: "SECRET-R"}
	for _, text := range []string{fmt.Sprint(tokens), fmt.Sprintf("%v %+v %#v %s", tokens, tokens, tokens, tokens), fmt.Sprint(&tokens)} {
		if strings.Contains(text, "SECRET") {
			t.Fatalf("a token printed: %s", text)
		}
	}
}
