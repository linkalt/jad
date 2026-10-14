package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

type listedSession struct {
	ID           string           `json:"id"`
	Turns        int64            `json:"turns"`
	ProfileTurns map[string]int64 `json:"profile_turns"`
}

func (f *layoutFixture) listSessions(t *testing.T, ifNoneMatch string) (int, string, []listedSession) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.api.URL+"/api/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Sessions []listedSession `json:"sessions"`
	}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, response.Header.Get("ETag"), body.Sessions
}

func TestSessionListCountsTheTurnsOfEveryProfile(t *testing.T) {
	f := newLayoutFixture(t)
	const session = "/api/sessions/local:demo"
	reviewerState := filepath.Join(f.cwd, ".jad", "state", "demo", "review")

	run := func(profile, input string) {
		t.Helper()
		if status, body := f.do(t, http.MethodPost, session+"/run?profile="+url.QueryEscape(profile), `{"input":"`+input+`"}`); status != http.StatusOK {
			t.Fatalf("run on %s: %d %s", profile, status, body)
		}
	}
	run("dev/coder", "one")
	run("dev/coder", "two")

	status, etag, sessions := f.listSessions(t, "")
	if status != http.StatusOK || len(sessions) != 1 {
		t.Fatalf("list = %d %v", status, sessions)
	}
	if got := sessions[0].ProfileTurns; got["dev/coder"] != 2 || got["review/reviewer"] != 0 || len(got) != 2 {
		t.Fatalf("profile_turns = %v, want dev/coder 2 and review/reviewer 0", got)
	}
	if sessions[0].Turns != 2 {
		t.Fatalf("turns = %d, want the default profile's 2", sessions[0].Turns)
	}
	if _, err := os.Stat(reviewerState); !os.IsNotExist(err) {
		t.Fatalf("listing created state for a profile that never ran: %v", err)
	}

	if status, _, _ := f.listSessions(t, etag); status != http.StatusNotModified {
		t.Fatalf("an unchanged list = %d, want 304", status)
	}

	// Only the other profile's transcript changes, and no run is recorded; a list keyed
	// on the default's transcript alone would answer 304 with the old count.
	reviewerDir := filepath.Join(reviewerState, "reviewer")
	if err := os.MkdirAll(reviewerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reviewerDir, "messages.jsonl"), []byte(`{"role":"user","content":"three"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, _, sessions = f.listSessions(t, etag)
	if status != http.StatusOK {
		t.Fatalf("list after a change in another profile = %d, want 200", status)
	}
	if got := sessions[0].ProfileTurns; got["dev/coder"] != 2 || got["review/reviewer"] != 1 {
		t.Fatalf("profile_turns = %v, want dev/coder 2 and review/reviewer 1", got)
	}
	if sessions[0].Turns != 2 {
		t.Fatalf("turns = %d, want the default profile's 2", sessions[0].Turns)
	}
}

func TestSessionListSurvivesAnUnreadableTranscriptOfAnotherProfile(t *testing.T) {
	f := newLayoutFixture(t)
	reviewerDir := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer")
	if err := os.MkdirAll(reviewerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory where the transcript file belongs: it exists but cannot be read.
	if err := os.Mkdir(filepath.Join(reviewerDir, "messages.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, _, sessions := f.listSessions(t, "")
	if status != http.StatusOK || len(sessions) != 1 {
		t.Fatalf("list = %d %v, want 200 with the session", status, sessions)
	}
	got := sessions[0].ProfileTurns
	if _, present := got["review/reviewer"]; present || got["dev/coder"] != 0 {
		t.Fatalf("profile_turns = %v, want only dev/coder", got)
	}
}

func TestProfileTurnsAreCountedAgainOnlyWhenTheTranscriptChanges(t *testing.T) {
	resolver := newProfileTurnsBenchmarkResolver(t, 2, 3)
	ctx := context.Background()
	turnsOf := func(profile string) int64 {
		t.Helper()
		response, err := resolver.ListSessions(ctx, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Sessions []listedSession `json:"sessions"`
		}
		if err := json.Unmarshal(response.Body, &body); err != nil || len(body.Sessions) != 1 {
			t.Fatalf("list body: %v %s", err, response.Body)
		}
		return body.Sessions[0].ProfileTurns[profile]
	}
	if got := turnsOf("dev/p01"); got != 3 {
		t.Fatalf("first count = %d, want 3", got)
	}

	resolver.profileTurnsMu.Lock()
	if len(resolver.profileTurnCounts) != 1 {
		t.Fatalf("cache entries = %d, want one for the one other profile", len(resolver.profileTurnCounts))
	}
	for path, entry := range resolver.profileTurnCounts {
		entry.turns = 99
		resolver.profileTurnCounts[path] = entry
	}
	resolver.profileTurnsMu.Unlock()
	if got := turnsOf("dev/p01"); got != 99 {
		t.Fatalf("an unchanged transcript was counted again: %d, want the cached 99", got)
	}

	var transcriptPath string
	resolver.profileTurnsMu.Lock()
	for path := range resolver.profileTurnCounts {
		transcriptPath = path
	}
	resolver.profileTurnsMu.Unlock()
	file, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"another"}]}` + "\n")
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatalf("append: %v %v", err, closeErr)
	}
	if got := turnsOf("dev/p01"); got != 4 {
		t.Fatalf("count after the transcript grew = %d, want 4", got)
	}
}
