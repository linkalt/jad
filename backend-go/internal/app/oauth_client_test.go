package app

import (
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

func TestCheckOAuthCredentialAnswersWithoutThePathAndLogsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "auth.json")
	config := sessionfs.SessionConfig{Model: sessionfs.Model{OAuthAuthFile: path}}
	var logs lockedBuffer
	err := checkOAuthCredential(config, slog.New(slog.NewTextHandler(&logs, nil)))
	var statusErr *httpapi.StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want a 422 StatusError", err)
	}
	if strings.Contains(statusErr.Message, path) || strings.Contains(statusErr.Message, "/") {
		t.Fatalf("the client message shows the path: %q", statusErr.Message)
	}
	if statusErr.Message != "OpenAI OAuth credential file not found; sign in with the auth tool" {
		t.Fatalf("message = %q", statusErr.Message)
	}
	if !strings.Contains(logs.String(), path) {
		t.Fatalf("the log should name the file:\n%s", logs.String())
	}
}
