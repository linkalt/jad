package app

import (
	"net/http"
	"strings"
	"testing"
)

// The remote profile info route answers the real context tokens of a profile the token reaches,
// with no file path, and refuses a profile outside the token's globs as if it did not exist.
func TestRemoteProfileInfoAnswersWithoutPathsAndHidesOtherProfiles(t *testing.T) {
	server, f := e2eServer(t)
	writeRemoteTranscript(t, server, "dev/coder", remoteTranscriptJSONL)
	status, body := server.call(t, http.MethodGet, "/api/remote/v1/profile?session=demo&profile=dev/coder", e2eToken, nil)
	output, _ := body["output"].(string)
	if status != http.StatusOK || !strings.Contains(output, "profile dev/coder | context tokens:") {
		t.Fatalf("info: status %d body %v", status, body)
	}
	if strings.Contains(output, "messages.jsonl") || strings.Contains(output, f.workspace) || strings.Contains(output, "| file ") {
		t.Fatalf("the remote info leaks a path:\n%s", output)
	}
	if status, body = server.call(t, http.MethodGet, "/api/remote/v1/profile?session=demo&profile=ops/never", e2eToken, nil); status != http.StatusNotFound {
		t.Fatalf("a profile that does not exist: status %d body %v", status, body)
	}
}

// A token without the profile action is refused the info route.
func TestRemoteProfileInfoNeedsTheProfileAction(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("client", e2eToken, `"profiles":["*/*/*"],"actions":["transcripts"]`)))
	if status, _ := server.call(t, http.MethodGet, "/api/remote/v1/profile?session=demo&profile=dev/coder", e2eToken, nil); status != http.StatusForbidden {
		t.Fatalf("no profile action: status %d, want 403", status)
	}
}
