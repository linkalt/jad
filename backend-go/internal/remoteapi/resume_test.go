package remoteapi

import (
	"fmt"
	"net/http"
	"testing"
)

// A subagent start may carry resume: the backend gets the source run and the caller the token
// authenticated as. The body has no caller key at all: a caller in it is an unknown field.
func TestSubagentStartForwardsResumeAndTheTokensCaller(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	status, body := f.do("POST", Prefix+"/subagents", secretA, map[string]any{
		"session": "main", "profile": "review/a", "agent": "explore", "task": "more",
		"resume": "rr_0123456789abcdef", "request_id": "req-resume-1",
	})
	if status != http.StatusAccepted {
		t.Fatalf("status %d: %v", status, body)
	}
	if len(f.backend.subs) != 1 {
		t.Fatalf("the backend got %d subagent starts, want 1", len(f.backend.subs))
	}
	if got := f.backend.subs[0].Resume; got != "rr_0123456789abcdef" {
		t.Fatalf("the backend got resume %q", got)
	}
	if got := f.backend.subs[0].Caller; got != "remote:build" {
		t.Fatalf("the backend got caller %q, want the token's remote:build (never the body's)", got)
	}
}

// resume is for subagents only: a message that names it is refused before the backend.
func TestMessageWithResumeIsRefusedBeforeTheBackend(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	status, _ := f.do("POST", Prefix+"/messages", secretA, map[string]any{
		"session": "main", "profile": "review/a", "message": "hi", "resume": "a/rr_0123456789abcdef", "request_id": "req-resume-2",
	})
	if status != http.StatusBadRequest || len(f.backend.messages) != 0 {
		t.Fatalf("status %d, backend messages %d; a message with resume must be refused before the backend", status, len(f.backend.messages))
	}
}

// A resume that is not a remote run id is refused before the backend: the wire takes rr_ ids only.
func TestSubagentResumeMustBeARemoteRunID(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	status, _ := f.do("POST", Prefix+"/subagents", secretA, map[string]any{
		"session": "main", "profile": "review/a", "agent": "explore", "task": "more",
		"resume": "sub_00000001", "request_id": "req-resume-3",
	})
	if status != http.StatusBadRequest || len(f.backend.subs) != 0 {
		t.Fatalf("status %d, backend starts %d; a non-rr_ resume must be refused before the backend", status, len(f.backend.subs))
	}
}

// A token without the subagent action is refused before the backend, resume or not. The token
// matches the profile and the agent, so only the action check can refuse the start.
func TestResumeWithoutTheSubagentActionIsRefusedBeforeTheBackend(t *testing.T) {
	table := tokens(t, fmt.Sprintf(`{"tokens":[
 {"id":"messenger","sha256":%q,"profiles":["main/review/*"],"agents":["main/review/*/explore"],"actions":["message","status"]}]}`, hashOf(secretA)))
	f := newFixture(t, table, nil)
	status, _ := f.do("POST", Prefix+"/subagents", secretA, map[string]any{
		"session": "main", "profile": "review/a", "agent": "explore", "task": "more",
		"resume": "rr_0123456789abcdef", "request_id": "req-resume-4",
	})
	if status != http.StatusForbidden || len(f.backend.subs) != 0 {
		t.Fatalf("status %d, backend starts %d; a token without subagent must be refused before the backend", status, len(f.backend.subs))
	}
}

// The request fingerprint includes the resume id: a retry with the same request_id and a different
// source is refused, not answered with the first run.
func TestResumeIsPartOfTheRequestFingerprint(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	start := func(resume string) (int, map[string]any) {
		return f.do("POST", Prefix+"/subagents", secretA, map[string]any{
			"session": "main", "profile": "review/a", "agent": "explore", "task": "more",
			"resume": resume, "request_id": "req-resume-fp",
		})
	}
	if status, body := start("rr_0123456789abcdef"); status != http.StatusAccepted {
		t.Fatalf("first start: %d %v", status, body)
	}
	status, body := start("rr_fedcba9876543210")
	if status != http.StatusConflict || body["code"] != "request_id_reused" {
		t.Fatalf("a different source under the same request_id: %d %v, want request_id_reused", status, body)
	}
	if len(f.backend.subs) != 1 {
		t.Fatalf("the backend got %d starts, want 1", len(f.backend.subs))
	}
}

// The backend's unknown_run refusal reaches the client as a 409 with that code and nothing else.
func TestResumeUnknownRunIsMappedToItsCode(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	f.backend.startErr = &Refusal{Code: "unknown_run", Message: "No sub-agent run rr_0123456789abcdef in this session"}
	status, body := f.do("POST", Prefix+"/subagents", secretA, map[string]any{
		"session": "main", "profile": "review/a", "agent": "explore", "task": "more",
		"resume": "rr_0123456789abcdef", "request_id": "req-resume-unknown",
	})
	if status != http.StatusConflict || body["code"] != "unknown_run" {
		t.Fatalf("status %d %v, want 409 unknown_run", status, body)
	}
}
