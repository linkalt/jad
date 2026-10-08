package openaiauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Random sizes in bytes, before base64 encoding. A 48 byte verifier encodes to 64
// characters, inside the 43..128 that RFC 7636 allows.
const (
	stateBytes    = 24
	verifierBytes = 48
)

// AuthRequest is one authorization request: the URL to open and the secrets needed to finish it.
type AuthRequest struct {
	URL         string
	State       string
	Verifier    string
	RedirectURI string
}

// NewAuthRequest builds an authorization-code request with PKCE (S256). random is the source of
// the state and the verifier; nil uses crypto/rand.
func NewAuthRequest(config *Config, redirectURI string, random io.Reader) (*AuthRequest, error) {
	if random == nil {
		random = rand.Reader
	}
	state, err := randomToken(random, stateBytes)
	if err != nil {
		return nil, err
	}
	verifier, err := randomToken(random, verifierBytes)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for key, value := range config.AuthorizeParams {
		query.Set(key, value)
	}
	query.Set("response_type", "code")
	query.Set("client_id", config.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", config.Scope)
	query.Set("state", state)
	query.Set("code_challenge", codeChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	return &AuthRequest{
		URL:         strings.TrimSuffix(config.Issuer, "/") + "/oauth/authorize?" + query.Encode(),
		State:       state,
		Verifier:    verifier,
		RedirectURI: redirectURI,
	}, nil
}

func randomToken(random io.Reader, size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := io.ReadFull(random, buffer); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// codeChallenge is BASE64URL(SHA256(verifier)) without padding (RFC 7636, section 4.2).
func codeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
