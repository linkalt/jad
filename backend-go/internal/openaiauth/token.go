package openaiauth

import (
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
	IDToken      string
	RefreshToken string
	// AccountID is derived from the tokens' claims; it is empty when they carry none.
	AccountID string
	// ExpiresIn is the lifetime in seconds the issuer reported, or 0.
	ExpiresIn int64
}

func (Tokens) String() string   { return "openaiauth.Tokens{redacted}" }
func (Tokens) GoString() string { return "openaiauth.Tokens{redacted}" }

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

func (c *Client) tokenURL() string { return strings.TrimSuffix(c.Config.Issuer, "/") + "/oauth/token" }

// Exchange trades an authorization code for tokens (form-encoded, as the Codex CLI does).
func (c *Client) Exchange(ctx context.Context, code, verifier, redirectURI string) (*Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {c.Config.ClientID},
		"code_verifier": {verifier},
	}
	return c.post(ctx, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

// Refresh trades a refresh token for new tokens (JSON, as the Codex CLI does). The result keeps
// the old refresh token when the issuer sends none.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     c.Config.ClientID,
	})
	if err != nil {
		return nil, err
	}
	tokens, err := c.post(ctx, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

func (c *Client) post(ctx context.Context, contentType string, body io.Reader) (*Tokens, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), body)
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient().Do(request)
	if err != nil {
		// The URL has no secret in it, and the body is not part of the error text.
		return nil, fmt.Errorf("token request to %s failed: %w", c.tokenURL(), unwrapURLError(err))
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
		Code             string          `json:"code"`
	}
	if json.Unmarshal(data, &body) != nil {
		return result
	}
	// "error" is a string in OAuth (RFC 6749) but an object on some OpenAI endpoints.
	var code string
	if json.Unmarshal(body.Error, &code) == nil {
		result.Code = code
	} else {
		var object struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body.Error, &object) == nil {
			result.Code = object.Code
			body.Message = firstNonEmpty(body.Message, object.Message)
		}
	}
	result.Code = firstNonEmpty(result.Code, body.Code)
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
		IDToken      string  `json:"id_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, errors.New("token response is not valid JSON of the expected shape")
	}
	if body.AccessToken == "" {
		return nil, errors.New("token response has no access_token")
	}
	return &Tokens{
		AccessToken:  body.AccessToken,
		IDToken:      body.IDToken,
		RefreshToken: body.RefreshToken,
		AccountID:    accountIDFrom(body.IDToken, body.AccessToken),
		ExpiresIn:    int64(body.ExpiresIn),
	}, nil
}
