package claudeauth

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"

	"jad/backend-go/internal/fserr"
)

// Status is a report on the credential file. It holds no token text.
type Status struct {
	Path         string     `json:"path"`
	Exists       bool       `json:"exists"`
	Mode         string     `json:"mode,omitempty"`
	ModeTooOpen  bool       `json:"mode_too_open,omitempty"`
	Version      int        `json:"version,omitempty"`
	Scope        string     `json:"scope,omitempty"`
	AccessToken  bool       `json:"access_token_present"`
	RefreshToken bool       `json:"refresh_token_present"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	// Expired is true when the stored expiry is within the margin of now or past.
	Expired      bool       `json:"expired"`
	LastRefresh  *time.Time `json:"last_refresh,omitempty"`
	SinceRefresh string     `json:"since_last_refresh,omitempty"`
	BackupExists bool       `json:"backup_exists"`
	Problem      string     `json:"problem,omitempty"`
	// UsableByExpiry is true when the file is complete and the stored expiry is still ahead.
	UsableByExpiry bool `json:"usable_by_expiry"`
}

// ExitCode maps the report to the exit status of the status command: 0 when the access token
// looks usable by its stored expiry, 1 when the file is missing or unusable, 2 when the access
// token is expired by its stored expiry.
func (s *Status) ExitCode() int {
	switch {
	case s.UsableByExpiry:
		return 0
	case s.Expired && s.Problem == "":
		return 2
	default:
		return 1
	}
}

// Inspect builds the report. margin is how long before its expiry a token already counts as
// expired. The result says what the file says: it cannot tell whether the server accepts the
// token (OpenAI's server rejected tokens whose claims were valid for days; nothing shows
// Anthropic behaves differently).
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
			report.Problem = "cannot be read: " + fserr.UnwrapPathError(err).Error()
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
	report.Version = file.Version
	report.Scope = file.Scope
	report.AccessToken = file.AccessToken != ""
	report.RefreshToken = file.RefreshToken != ""
	if stored, err := time.Parse(ExpiresAtLayout, file.LastRefresh); err == nil {
		report.LastRefresh = &stored
		report.SinceRefresh = now.Sub(stored).Round(time.Second).String()
	}
	expires, expiryErr := time.Parse(ExpiresAtLayout, file.ExpiresAt)
	if expiryErr == nil {
		report.ExpiresAt = &expires
		report.Expired = !expires.After(now.Add(margin))
	}
	switch {
	case !report.AccessToken:
		report.Problem = "has no access token; run the login command"
	case expiryErr != nil:
		report.Problem = "has no valid expires_at; run the login command"
	}
	report.UsableByExpiry = report.Problem == "" && !report.Expired
	return report
}
