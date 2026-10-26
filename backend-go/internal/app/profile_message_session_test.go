package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/tools/native"
)

func addOtherSession(t *testing.T, f *layoutFixture, id string, profiles ...string) {
	t.Helper()
	list := `"` + strings.Join(profiles, `","`) + `"`
	f.overwrite(t, "sessions/"+id+".json", `{"schema_version":1,"cwd":`+quoteJSON(f.cwd)+`,"category":"work","scope":{"profiles":[`+list+`]}}`)
}

func quoteJSON(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func TestProfileMessageReachesAProfileOfAnotherSession(t *testing.T) {
	f := newLayoutFixture(t)
	addOtherSession(t, f, "other", "review")
	messenger := profileMessenger{resolver: f.service.resolver}
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Session: "other", Target: "review/reviewer", Message: "cross hello"}
	plan, err := messenger.Prepare(callerContext(t, f, context.Background()), request)
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-run.Done()
	if outcome := run.Outcome(); outcome.Err != nil || !strings.Contains(outcome.Text, "ok") {
		t.Fatalf("outcome = %+v", outcome)
	}
	data, err := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", "other", "review", "reviewer", "messages.jsonl"))
	if err != nil || !strings.Contains(string(data), "cross hello") {
		t.Fatalf("transcript of the other session: %v %s", err, data)
	}
	if data, err := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "messages.jsonl")); err == nil && strings.Contains(string(data), "cross hello") {
		t.Fatalf("the calling session's profile got the message: %s", data)
	}
}

func TestProfileMessageToTheSameNameInAnotherSessionIsNotACycle(t *testing.T) {
	f := newLayoutFixture(t)
	addOtherSession(t, f, "other", "dev")
	messenger := profileMessenger{resolver: f.service.resolver}
	if _, err := messenger.Prepare(callerContext(t, f, context.Background()), native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Session: "other", Target: "dev/coder", Message: "hi"}); err != nil {
		t.Fatalf("dev/coder of another session: %v", err)
	}
	if _, err := messenger.Prepare(callerContext(t, f, context.Background()), native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Session: "demo", Target: "dev/coder", Message: "hi"}); !errors.Is(err, native.ErrProfileCycle) {
		t.Fatalf("dev/coder of the calling session by name: error = %v, want ErrProfileCycle", err)
	}
}

func TestProfileMessageUnknownSessionOrProfileIsErrProfileUnknown(t *testing.T) {
	f := newLayoutFixture(t)
	addOtherSession(t, f, "other", "review")
	messenger := profileMessenger{resolver: f.service.resolver}
	for _, request := range []native.ProfileMessageRequest{
		{Session: "ghost", Target: "review/reviewer"},
		{Session: "other", Target: "dev/coder"},
		{Session: "other", Target: "review/ghost"},
		{Session: "..", Target: "review/reviewer"},
		{Session: "other/..", Target: "review/reviewer"},
	} {
		request.OwnerKey, request.Message = profileMessageOwner, "hi"
		if _, err := messenger.Prepare(callerContext(t, f, context.Background()), request); !errors.Is(err, native.ErrProfileUnknown) {
			t.Errorf("%s %s: error = %v, want ErrProfileUnknown", request.Session, request.Target, err)
		}
	}
}
