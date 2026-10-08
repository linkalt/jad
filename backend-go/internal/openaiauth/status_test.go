package openaiauth

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const margin = 5 * time.Minute

func TestStatusOfAUsableFile(t *testing.T) {
	store := storeIn(t)
	access := accessJWT(t, fixedNow.Add(-2*time.Hour), 10*24*time.Hour, "acct-1")
	id := jwt(t, map[string]any{"iat": fixedNow.Add(-2 * time.Hour).Unix(), "exp": fixedNow.Add(-time.Hour).Unix(),
		authClaim: map[string]any{"chatgpt_account_is_fedramp": true}})
	writeFile(t, store, map[string]any{"access_token": access, "id_token": id, "refresh_token": "r", "account_id": "acct-1"},
		map[string]any{"last_refresh": fixedNow.Add(-2 * time.Hour).Format(time.RFC3339)})
	report := Inspect(store, margin, fixedNow)
	if !report.Exists || !report.UsableByClaims || report.ExitCode() != 0 || report.Problem != "" {
		t.Fatalf("usable file reported wrong: %+v", report)
	}
	if report.AuthMode != "chatgpt" || report.AccountID != "acct-1" || !report.RefreshToken || !report.FedRAMP || report.Mode != "-rw-------" || report.ModeTooOpen {
		t.Fatalf("fields wrong: %+v", report)
	}
	if report.SinceRefresh != "2h0m0s" || report.AccessToken.Expired || !report.IDToken.Expired {
		t.Fatalf("times wrong: since=%s access.expired=%v id.expired=%v", report.SinceRefresh, report.AccessToken.Expired, report.IDToken.Expired)
	}
	// An expired id_token does not make the file unusable: the backend sends only the access token.
	if report.ExitCode() != 0 {
		t.Fatal("an expired id_token must not change the exit code")
	}
}

func TestStatusExitCodes(t *testing.T) {
	store := storeIn(t)
	if report := Inspect(store, margin, fixedNow); report.Exists || report.ExitCode() != 1 || !strings.Contains(report.Problem, "login") {
		t.Fatalf("missing file: %+v", report)
	}
	expired := accessJWT(t, fixedNow.Add(-3*time.Hour), time.Hour, "a")
	writeFile(t, store, map[string]any{"access_token": expired, "refresh_token": "r", "account_id": "a"}, nil)
	if report := Inspect(store, margin, fixedNow); report.ExitCode() != 2 || report.UsableByClaims || report.Problem != "" {
		t.Fatalf("expired access token: want exit 2, got %d %+v", report.ExitCode(), report)
	}
	writeFile(t, store, map[string]any{"refresh_token": "r", "account_id": "a"}, nil)
	if report := Inspect(store, margin, fixedNow); report.ExitCode() != 1 || !strings.Contains(report.Problem, "no access token") {
		t.Fatalf("no access token: %+v", report)
	}
	writeFile(t, store, map[string]any{"access_token": "opaque-not-a-jwt"}, nil)
	if report := Inspect(store, margin, fixedNow); report.ExitCode() != 1 || !strings.Contains(report.Problem, "account id") {
		t.Fatalf("no account id: %+v", report)
	}
	if err := os.WriteFile(store.Path, []byte(`{"tokens": SECRET-GARBAGE`), 0o600); err != nil {
		t.Fatal(err)
	}
	if report := Inspect(store, margin, fixedNow); report.ExitCode() != 1 || strings.Contains(report.Problem, "SECRET") || report.Problem == "" {
		t.Fatalf("invalid JSON: %+v", report)
	}
}

// A token that is not a JWT has no expiry to check. The backend sends it as it is, so status agrees.
func TestStatusOpaqueAccessTokenCountsAsUsable(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, map[string]any{"access_token": "opaque", "account_id": "a", "refresh_token": "r"}, nil)
	report := Inspect(store, margin, fixedNow)
	if !report.UsableByClaims || report.AccessToken.Readable || report.AccessToken.ExpiresAt != nil || report.ExitCode() != 0 {
		t.Fatalf("opaque token: %+v", report)
	}
}

func TestStatusMarginAndLooseMode(t *testing.T) {
	store := storeIn(t)
	access := accessJWT(t, fixedNow.Add(-time.Hour), time.Hour+3*time.Minute, "a") // expires in 3 min
	writeFile(t, store, map[string]any{"access_token": access, "account_id": "a", "refresh_token": "r"}, nil)
	if report := Inspect(store, margin, fixedNow); !report.AccessToken.Expired || report.ExitCode() != 2 {
		t.Fatalf("3 min left with a 5 min margin must count as expired: %+v", report.AccessToken)
	}
	if report := Inspect(store, 0, fixedNow); report.AccessToken.Expired || report.ExitCode() != 0 {
		t.Fatalf("3 min left with no margin must count as usable: %+v", report.AccessToken)
	}
	if err := os.Chmod(store.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if report := Inspect(store, margin, fixedNow); !report.ModeTooOpen || report.Mode != "-rw-r--r--" {
		t.Fatalf("a world-readable file must be flagged: %+v", report)
	}
}

// The report is printed and may be sent to a log; no token text may be in it.
func TestStatusJSONHasNoTokenText(t *testing.T) {
	store := storeIn(t)
	access := accessJWT(t, fixedNow, time.Hour, "a")
	writeFile(t, store, map[string]any{"access_token": access, "id_token": "SECRET-ID", "refresh_token": "SECRET-REFRESH", "account_id": "a"}, nil)
	raw, err := json.Marshal(Inspect(store, margin, fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{access, "SECRET-ID", "SECRET-REFRESH"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the status JSON contains token text: %s", raw)
		}
	}
}

func TestStatusReportsTheBackup(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), nil)
	if Inspect(store, margin, fixedNow).BackupExists {
		t.Fatal("no backup yet")
	}
	if err := os.WriteFile(store.BackupPath(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !Inspect(store, margin, fixedNow).BackupExists {
		t.Fatal("backup not reported")
	}
}
