package openaiauth

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
)

// RFC 7636, appendix B: the verifier and challenge of the specification's own example.
func TestCodeChallengeMatchesRFC7636Example(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := codeChallenge(verifier); got != want {
		t.Fatalf("challenge %q, want %q", got, want)
	}
}

func TestNewAuthRequestBuildsTheURL(t *testing.T) {
	config, _ := LoadConfig("")
	request, err := NewAuthRequest(config, "http://localhost:1455/auth/callback", bytes.NewReader(bytes.Repeat([]byte{7}, 200)))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(request.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != "https://auth.openai.com/oauth/authorize" {
		t.Fatalf("endpoint wrong: %s", request.URL)
	}
	query := parsed.Query()
	want := map[string]string{
		"response_type": "code", "client_id": DefaultClientID, "redirect_uri": "http://localhost:1455/auth/callback",
		"scope": DefaultScope, "state": request.State, "code_challenge": codeChallenge(request.Verifier),
		"code_challenge_method": "S256", "id_token_add_organizations": "true", "codex_cli_simplified_flow": "true",
	}
	for key, value := range want {
		if query.Get(key) != value {
			t.Errorf("%s = %q, want %q", key, query.Get(key), value)
		}
	}
	if len(request.Verifier) < 43 || len(request.Verifier) > 128 {
		t.Errorf("verifier length %d is outside RFC 7636 43..128", len(request.Verifier))
	}
	if strings.ContainsAny(request.Verifier+request.State, "+/=") {
		t.Error("state and verifier must be base64url without padding")
	}
}

func TestNewAuthRequestIsRandomAndReportsFailure(t *testing.T) {
	config, _ := LoadConfig("")
	first, err := NewAuthRequest(config, "http://localhost/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := NewAuthRequest(config, "http://localhost/x", nil)
	if first.State == second.State || first.Verifier == second.Verifier {
		t.Fatal("two requests must not share a state or a verifier")
	}
	if _, err := NewAuthRequest(config, "http://localhost/x", bytes.NewReader([]byte{1, 2})); err == nil {
		t.Fatal("a short random source must be an error, not a weak verifier")
	}
}

// A custom authorize parameter reaches the URL, and a custom issuer path is kept.
func TestNewAuthRequestCustomParamsAndIssuer(t *testing.T) {
	config, _ := LoadConfig(writeConfig(t, `{"issuer":"https://id.example/base/","authorize_params":{"audience":"a b"}}`))
	request, err := NewAuthRequest(config, "http://localhost:1/c", nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(request.URL)
	if parsed.Path != "/base/oauth/authorize" || parsed.Query().Get("audience") != "a b" || parsed.Query().Get("codex_cli_simplified_flow") != "" {
		t.Fatalf("custom config not applied: %s", request.URL)
	}
}
