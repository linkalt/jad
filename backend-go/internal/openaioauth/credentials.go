// Package openaioauth sends model requests with an OpenAI (ChatGPT account) OAuth
// access token. It only reads a credential file that another tool writes: sign-in,
// token refresh and status are not part of the backend, so there is never a second
// writer to race with.
package openaioauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Credential is what a request needs. Its text forms never show the token.
type Credential struct {
	AccessToken string
	AccountID   string
	// FedRAMP is true when a token claims a FedRAMP account.
	FedRAMP bool
}

func (Credential) String() string   { return "openaioauth.Credential{redacted}" }
func (Credential) GoString() string { return "openaioauth.Credential{redacted}" }

// CredentialError is a credential that cannot be used. It is permanent: repeating the
// request cannot help, so the retry loop must not wait and try again. The message names
// the file and the reason and never contains token text; it is for the server log. Message
// omits the path and is the only text a client may see.
type CredentialError struct {
	Path   string
	Reason string
}

func (e *CredentialError) Error() string {
	return fmt.Sprintf("OpenAI OAuth credential %s: %s", e.Path, e.Reason)
}

// Message is the client-facing text: the reason without the credential file path.
func (e *CredentialError) Message() string {
	return "OpenAI OAuth credential " + e.Reason
}

// Permanent marks the error as one a retry cannot fix.
func (e *CredentialError) Permanent() bool { return true }

// Source reads the credential file. It reads the file on every Load, so a token that
// the sign-in tool refreshed is used by the next request without a restart.
type Source struct {
	// Path is the credential file.
	Path string
	// ExpiryMargin is how long before its expiry an access token already counts as
	// expired, so a request is not sent with a token that dies in flight.
	ExpiryMargin time.Duration
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// authFile is the part of the file that is read. Unknown keys are ignored because the
// tool that writes the file owns its layout.
type authFile struct {
	Tokens struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// Load returns a usable credential or a *CredentialError.
func (s *Source) Load() (Credential, error) {
	fail := func(reason string) (Credential, error) {
		return Credential{}, &CredentialError{Path: s.Path, Reason: reason}
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fail("file not found; sign in with the auth tool")
		}
		return fail("cannot be read: " + errors.Unwrap(err).Error())
	}
	if !info.Mode().IsRegular() {
		return fail("is not a regular file")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return fail("cannot be read: " + errors.Unwrap(err).Error())
	}
	var file authFile
	if err := json.Unmarshal(data, &file); err != nil {
		// The decoder's text can quote file content, so it is not repeated.
		return fail("is not valid JSON of the expected shape")
	}
	tokens := file.Tokens
	if tokens.AccessToken == "" {
		return fail("has no access token; sign in with the auth tool")
	}
	idClaims := parseClaims(tokens.IDToken)
	accessClaims := parseClaims(tokens.AccessToken)
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	// A token without a readable expiry is sent as it is: the provider decides.
	if exp, ok := accessClaims.expiry(); ok && !exp.After(now().Add(s.ExpiryMargin)) {
		return fail(fmt.Sprintf("access token expired at %s; refresh it with the auth tool", exp.UTC().Format(time.RFC3339)))
	}
	accountID := tokens.AccountID
	if accountID == "" {
		accountID = idClaims.accountID()
	}
	if accountID == "" {
		accountID = accessClaims.accountID()
	}
	if accountID == "" {
		return fail("has no account id and the tokens carry none; sign in with the auth tool")
	}
	return Credential{
		AccessToken: tokens.AccessToken,
		AccountID:   accountID,
		FedRAMP:     idClaims.fedRAMP() || accessClaims.fedRAMP(),
	}, nil
}

// claims is the payload of a JWT. A token that is not a readable JWT gives no claims.
type claims map[string]any

const authClaim = "https://api.openai.com/auth"

func parseClaims(token string) claims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var out claims
	if json.Unmarshal(payload, &out) != nil {
		return nil
	}
	return out
}

func (c claims) expiry() (time.Time, bool) {
	exp, ok := c["exp"].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

func (c claims) authObject() map[string]any {
	object, _ := c[authClaim].(map[string]any)
	return object
}

func (c claims) accountID() string {
	if id, _ := c.authObject()["chatgpt_account_id"].(string); id != "" {
		return id
	}
	if id, _ := c["chatgpt_account_id"].(string); id != "" {
		return id
	}
	if organizations, _ := c["organizations"].([]any); len(organizations) > 0 {
		first, _ := organizations[0].(map[string]any)
		if id, _ := first["id"].(string); id != "" {
			return id
		}
	}
	return ""
}

func (c claims) fedRAMP() bool {
	flag, _ := c.authObject()["chatgpt_account_is_fedramp"].(bool)
	return flag
}
