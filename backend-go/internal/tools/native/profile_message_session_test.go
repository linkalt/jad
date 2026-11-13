package native

import (
	"context"
	"errors"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func crossSessionSettings() sessionfs.NativeProfileMessageSettings {
	settings := profileMessageSettings()
	settings.Sessions = []string{"work-*", "shared"}
	settings.DenyProfiles = []string{"team/secret"}
	return settings
}

func TestProfileMessageAddressesAnotherSession(t *testing.T) {
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "from afar", nil }}
	output := callProfileMessage(t, crossSessionSettings(), messenger, `{"profile":"work-b/review/reviewer","message":"hi"}`)
	if messenger.request.Session != "work-b" || messenger.target != "review/reviewer" || !strings.Contains(output, "from afar") {
		t.Fatalf("session %q target %q output %s", messenger.request.Session, messenger.target, output)
	}
}

func TestProfileMessageShortFormKeepsTheCallingSession(t *testing.T) {
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	callProfileMessage(t, crossSessionSettings(), messenger, `{"profile":"team/coder","message":"hi"}`)
	if messenger.request.Session != "" || messenger.target != "team/coder" {
		t.Fatalf("session %q target %q, want the calling session and team/coder", messenger.request.Session, messenger.target)
	}
}

func TestProfileMessageSessionOutsideTheAllowRulesReadsLikeAMissingOne(t *testing.T) {
	failing := &fakeMessenger{prepareErr: ErrProfileUnknown}
	missing := callProfileMessage(t, crossSessionSettings(), failing, `{"profile":"shared/review/ghost","message":"hi"}`)
	if !strings.Contains(missing, "error_code: unknown_profile") {
		t.Fatalf("a missing profile of an allowed session: %s", missing)
	}
	for _, profile := range []string{"other/review/reviewer", "team/secret", "shared/team/secret", "work/review/reviewer"} {
		messenger := &fakeMessenger{reply: func(context.Context) (string, error) { t.Fatal("messenger called"); return "", errors.New("x") }}
		denied := callProfileMessage(t, crossSessionSettings(), messenger, `{"profile":"`+profile+`","message":"hi"}`)
		want := strings.ReplaceAll(missing, "shared/review/ghost", profile)
		if strings.ReplaceAll(denied, "call_1", "") != strings.ReplaceAll(want, "call_1", "") {
			t.Errorf("%s:\n got %s\nwant %s", profile, denied, want)
		}
	}
}

func TestProfileMessageWithoutSessionsRefusesAnotherSession(t *testing.T) {
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { t.Fatal("messenger called"); return "", nil }}
	output := callProfileMessage(t, profileMessageSettings(), messenger, `{"profile":"shared/review/reviewer","message":"hi"}`)
	if !strings.Contains(output, "error_code: unknown_profile") {
		t.Fatalf("output = %s", output)
	}
}

func TestProfileMessageRejectsTraversalInASessionTarget(t *testing.T) {
	settings := crossSessionSettings()
	settings.Sessions = []string{"*"}
	for _, profile := range []string{"../review/reviewer", "./review/reviewer", "shared/../reviewer", "shared/review/..", "shared/review/a/b", "shared//reviewer", "sha red/review/reviewer", "local:shared/review/reviewer", "shared/.review/x"} {
		messenger := &fakeMessenger{reply: func(context.Context) (string, error) { t.Fatal("messenger called"); return "", nil }}
		output := callProfileMessage(t, settings, messenger, `{"profile":"`+profile+`","message":"hi"}`)
		if !strings.Contains(output, "error_code: unknown_profile") {
			t.Errorf("%s: output = %s", profile, output)
		}
	}
}

// A refused cross-workspace reset is its own error code, not a failed reset that "may need
// attention", and it says nothing was sent or changed.
func TestProfileMessageForeignResetRefusalIsItsOwnAnswer(t *testing.T) {
	settings := crossSessionSettings()
	messenger := &fakeMessenger{prepareErr: ErrContextTriggerForeignReset}
	output := callProfileMessage(t, settings, messenger, `{"profile":"shared/review/reviewer","message":"hi"}`)
	for _, want := range []string{"error_code: context_trigger_refused", "Nothing was sent or changed"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q: %s", want, output)
		}
	}
	if strings.Contains(output, "may need attention") {
		t.Errorf("a refusal is not a failed reset: %s", output)
	}
}
