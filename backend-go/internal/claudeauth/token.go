package claudeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Tokens is what the issuer returned. Its text forms never show a token.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	// ExpiresIn is the lifetime in seconds the issuer reported. It is always positive.
	ExpiresIn int64
	// Scope is the scope the issuer granted, or empty when it sent none.
	Scope string
}

func (Tokens) String() string   { return "claudeauth.Tokens{redacted}" }
func (Tokens) GoString() string { return "claudeauth.Tokens{redacted}" }

// TokenError is a refusal or failure of a token request. The message holds the status and the
// issuer's error code and description, never the response body or any token.
type TokenError struct {
	Status      int
	Code        string
	Description string
}

func (e *TokenError) Error() string {
	message := fmt.Sprintf("token request failed with HTTP %d", e.Status)
	if e.Code != "" {
		message += ": " + e.Code
	}
	if e.Description != "" {
		message += " (" + e.Description + ")"
	}
	return message
}

// Rejected is true when the issuer refused the credential itself (a 4xx other than a rate
// limit or timeout). Repeating the request cannot help; signing in again can.
func (e *TokenError) Rejected() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests && e.Status != http.StatusRequestTimeout
}

// maxDescription bounds the issuer text that is copied into an error message.
const maxDescription = 200

// Client talks to the issuer's token endpoint.
type Client struct {
	Config *Config
	// HTTP is the client used; nil builds one with the configured timeout.
	HTTP *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: c.Config.HTTPTimeout()}
}

// Exchange trades an authorization code for tokens. The body is JSON and carries the state, as
// Claude Code's token endpoint expects.
func (c *Client) Exchange(ctx context.Context, code, state, verifier, redirectURI string) (*Tokens, error) {
	return c.post(ctx, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     c.Config.ClientID,
		"code":          code,
		"state":         state,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
}

// Refresh trades a refresh token for new tokens. The result keeps the old refresh token when the
// issuer sends none.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	tokens, err := c.post(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     c.Config.ClientID,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

func (c *Client) post(ctx context.Context, fields map[string]string) (*Tokens, error) {
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Config.TokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient().Do(request)
	if err != nil {
		// The URL has no secret in it, and the body is not part of the error text.
		return nil, fmt.Errorf("token request to %s failed: %w", c.Config.TokenURL, unwrapURLError(err))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxTokenResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if len(data) > MaxTokenResponseBytes {
		return nil, fmt.Errorf("token response exceeds %d bytes", MaxTokenResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, tokenErrorFrom(response.StatusCode, data)
	}
	return parseTokens(data)
}

// unwrapURLError drops the *url.Error wrapper, which repeats the URL, so the message is short.
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func tokenErrorFrom(status int, data []byte) *TokenError {
	result := &TokenError{Status: status}
	var body struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
		Message          string          `json:"message"`
	}
	if json.Unmarshal(data, &body) != nil {
		return result
	}
	// "error" is a string in OAuth (RFC 6749) but an object on Anthropic's API endpoints:
	// {"type":"error","error":{"type":"...","message":"..."}}.
	var code string
	if json.Unmarshal(body.Error, &code) == nil {
		result.Code = code
	} else {
		var object struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body.Error, &object) == nil {
			result.Code = object.Type
			body.Message = firstNonEmpty(body.Message, object.Message)
		}
	}
	result.Description = truncate(firstNonEmpty(body.ErrorDescription, body.Message), maxDescription)
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func truncate(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

func parseTokens(data []byte) (*Tokens, error) {
	var body struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
		Scope        string  `json:"scope"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, errors.New("token response is not valid JSON of the expected shape")
	}
	if body.AccessToken == "" {
		return nil, errors.New("token response has no access_token")
	}
	// Access tokens are opaque, so the expiry can only come from expires_in. Without it the
	// stored file could never say when to refresh.
	if body.ExpiresIn <= 0 {
		return nil, errors.New("token response has no positive expires_in")
	}
	return &Tokens{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresIn:    int64(body.ExpiresIn),
		Scope:        body.Scope,
	}, nil
}
