package openaiauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// claims is the payload of a JWT. The signature is not checked: the tool only reads times
// and the account id from tokens the issuer just handed to it, and the backend does the same.
// The backend has its own reader (internal/openaioauth) so that package stays read-only.
type claims map[string]any

const authClaim = "https://api.openai.com/auth"

// parseClaims returns the payload of a JWT, or nil when the token is not a readable JWT.
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

func (c claims) time(key string) (time.Time, bool) {
	seconds, ok := c[key].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0), true
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

// accountIDFrom returns the first account id the tokens carry.
func accountIDFrom(tokens ...string) string {
	for _, token := range tokens {
		if id := parseClaims(token).accountID(); id != "" {
			return id
		}
	}
	return ""
}
