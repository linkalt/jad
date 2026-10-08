package openaiauth

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"
)

// TokenStatus describes one JWT in the file, from its claims only.
type TokenStatus struct {
	Present bool `json:"present"`
	// Readable is false when the token is not a JWT with a payload, so no times are known.
	Readable  bool       `json:"readable"`
	IssuedAt  *time.Time `json:"issued_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Expired is true when the expiry claim is within the margin of now or past.
	Expired bool `json:"expired"`
}

// Status is a report on the credential file. It holds no token text.
type Status struct {
	Path           string      `json:"path"`
	Exists         bool        `json:"exists"`
	Mode           string      `json:"mode,omitempty"`
	ModeTooOpen    bool        `json:"mode_too_open,omitempty"`
	AuthMode       string      `json:"auth_mode,omitempty"`
	AccountID      string      `json:"account_id,omitempty"`
	FedRAMP        bool        `json:"fedramp,omitempty"`
	RefreshToken   bool        `json:"refresh_token_present"`
	LastRefresh    *time.Time  `json:"last_refresh,omitempty"`
	SinceRefresh   string      `json:"since_last_refresh,omitempty"`
	AccessToken    TokenStatus `json:"access_token"`
	IDToken        TokenStatus `json:"id_token"`
	BackupExists   bool        `json:"backup_exists"`
	Problem        string      `json:"problem,omitempty"`
	UsableByClaims bool        `json:"usable_by_claims"`
}

// ExitCode maps the report to the exit status of the status command: 0 when the access token
// looks usable by its claims, 1 when the file is missing or unusable, 2 when the access token
// is expired by its own expiry claim.
func (s *Status) ExitCode() int {
	switch {
	case s.UsableByClaims:
		return 0
	case s.AccessToken.Expired && s.Problem == "":
		return 2
	default:
		return 1
	}
}

// Inspect builds the report. margin is how long before its expiry a token already counts as
// expired. The result says what the claims say: it cannot tell whether the server accepts the
// token (the server has rejected tokens whose claims were still valid for days).
func Inspect(store *Store, margin time.Duration, now time.Time) *Status {
	report := &Status{Path: store.Path}
	if _, err := os.Stat(store.BackupPath()); err == nil {
		report.BackupExists = true
	}
	info, err := os.Stat(store.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			report.Problem = "file not found; run the login command"
		} else {
			report.Problem = "cannot be read: " + errors.Unwrap(err).Error()
		}
		return report
	}
	report.Exists = true
	report.Mode = info.Mode().Perm().String()
	report.ModeTooOpen = info.Mode().Perm()&0o077 != 0
	file, err := store.Read()
	if err != nil {
		report.Problem = strings.TrimPrefix(err.Error(), store.Path+": ")
		return report
	}
	report.AuthMode = file.AuthMode
	report.RefreshToken = file.RefreshToken != ""
	if stored, err := time.Parse(time.RFC3339, file.LastRefresh); err == nil {
		report.LastRefresh = &stored
		report.SinceRefresh = now.Sub(stored).Round(time.Second).String()
	}
	report.AccessToken = tokenStatus(file.AccessToken, margin, now)
	report.IDToken = tokenStatus(file.IDToken, margin, now)
	report.AccountID = firstNonEmpty(file.AccountID, accountIDFrom(file.IDToken, file.AccessToken))
	report.FedRAMP = parseClaims(file.IDToken).fedRAMP() || parseClaims(file.AccessToken).fedRAMP()
	switch {
	case !report.AccessToken.Present:
		report.Problem = "has no access token; run the login command"
	case report.AccountID == "":
		report.Problem = "has no account id and the tokens carry none; run the login command"
	}
	// An unreadable access token has no expiry to check: the backend sends it as it is and lets
	// the server decide, so it counts as usable here too.
	report.UsableByClaims = report.Problem == "" && !report.AccessToken.Expired
	return report
}

func tokenStatus(token string, margin time.Duration, now time.Time) TokenStatus {
	status := TokenStatus{Present: token != ""}
	if !status.Present {
		return status
	}
	payload := parseClaims(token)
	status.Readable = payload != nil
	if issued, ok := payload.time("iat"); ok {
		status.IssuedAt = &issued
	}
	if expires, ok := payload.time("exp"); ok {
		status.ExpiresAt = &expires
		status.Expired = !expires.After(now.Add(margin))
	}
	return status
}
