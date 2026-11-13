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

// A session written <workspace>:<session> is matched only by a glob that names a workspace, and
// a bare session only by a glob that does not (design §29): a bare "*" keeps meaning the calling
// workspace.
func TestProfileMessageAddressesASessionOfAnotherWorkspace(t *testing.T) {
	settings := crossSessionSettings()
	settings.Sessions = []string{"b:work-*", "*:shared", "own-?"}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "from b", nil }}
	output := callProfileMessage(t, settings, messenger, `{"profile":"b:work-1/review/reviewer","message":"hi"}`)
	if messenger.request.Session != "b:work-1" || messenger.target != "review/reviewer" || !strings.Contains(output, "from b") {
		t.Fatalf("session %q target %q output %s", messenger.request.Session, messenger.target, output)
	}
	messenger = &fakeMessenger{reply: func(context.Context) (string, error) { return "from c", nil }}
	callProfileMessage(t, settings, messenger, `{"profile":"c:shared/review/reviewer","message":"hi"}`)
	if messenger.request.Session != "c:shared" {
		t.Fatalf("session %q", messenger.request.Session)
	}
	// The bare form of an allowed bare glob still means the caller's workspace.
	messenger = &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	callProfileMessage(t, settings, messenger, `{"profile":"own-1/review/reviewer","message":"hi"}`)
	if messenger.request.Session != "own-1" {
		t.Fatalf("session %q", messenger.request.Session)
	}
}

func TestProfileMessageGlobsKeepBareAndWorkspaceFormsApart(t *testing.T) {
	missing := callProfileMessage(t, crossSessionSettings(), &fakeMessenger{prepareErr: ErrProfileUnknown}, `{"profile":"shared/review/ghost","message":"hi"}`)
	for name, test := range map[string]struct {
		sessions []string
		profile  string
	}{
		"star is not every workspace":        {[]string{"*"}, "b:s1/review/reviewer"},
		"bare glob does not match qualified": {[]string{"b-*", "s1"}, "b:s1/review/reviewer"},
		"workspace glob for another":         {[]string{"c:*"}, "b:s1/review/reviewer"},
		"session glob of the wrong session":  {[]string{"b:work-*"}, "b:other/review/reviewer"},
		"workspace glob does not match bare": {[]string{"*:*", "b:*"}, "s1/review/reviewer"},
		"denied profile":                     {[]string{"*:*"}, "b:s1/team/secret"},
		"two colons":                         {[]string{"*:*"}, "a:b:s1/review/reviewer"},
		"empty workspace":                    {[]string{"*:*"}, ":s1/review/reviewer"},
		"empty session":                      {[]string{"*:*"}, "b:/review/reviewer"},
		"traversal in the session":           {[]string{"*:*"}, "b:../review/reviewer"},
		"dot session":                        {[]string{"*:*"}, "b:./review/reviewer"},
		"glob character in the reference":    {[]string{"*:*"}, "b:s*/review/reviewer"},
	} {
		t.Run(name, func(t *testing.T) {
			settings := crossSessionSettings()
			settings.Sessions = test.sessions
			messenger := &fakeMessenger{reply: func(context.Context) (string, error) { t.Fatal("messenger called"); return "", nil }}
			denied := callProfileMessage(t, settings, messenger, `{"profile":"`+test.profile+`","message":"hi"}`)
			want := strings.ReplaceAll(missing, "shared/review/ghost", test.profile)
			if strings.ReplaceAll(denied, "call_1", "") != strings.ReplaceAll(want, "call_1", "") {
				t.Errorf("got %s\nwant %s", denied, want)
			}
		})
	}
}

func TestProfileMessageListTextNamesTheWorkspaceForm(t *testing.T) {
	text := profileMessageListText(nil, []string{"s1", "b:*"}, nil)
	for _, want := range []string{"s1, b:*", "<workspace>:<session>/<category>/<profile>", "accept"} {
		if !strings.Contains(text, want) {
			t.Errorf("list text lacks %q: %s", want, text)
		}
	}
}

// remote: always means a remote server, so a workspace named remote is written
// local:remote:<session>; the prefix is only read before a <workspace>:<session> (two colons,
// which were never valid), and local:<session> stays the session of a workspace named local.
func TestProfileMessageExplicitLocalForm(t *testing.T) {
	for name, test := range map[string]struct {
		sessions []string
		profile  string
		session  string // "" means refused
	}{
		"workspace named remote":               {[]string{"remote:*"}, "local:remote:s1/review/reviewer", "remote:s1"},
		"explicit form of another workspace":   {[]string{"b:*"}, "local:b:s1/review/reviewer", "b:s1"},
		"workspace named local, plain":         {[]string{"local:*"}, "local:s1/review/reviewer", "local:s1"},
		"workspace named local, explicit":      {[]string{"local:*"}, "local:local:s1/review/reviewer", "local:s1"},
		"plain remote form is not local":       {[]string{"remote:*"}, "remote:s1/review/reviewer", ""},
		"glob of another workspace":            {[]string{"b:*"}, "local:remote:s1/review/reviewer", ""},
		"bare glob does not match":             {[]string{"s1"}, "local:remote:s1/review/reviewer", ""},
		"local:<session> is workspace local":   {[]string{"remote:*"}, "local:s1/review/reviewer", ""},
		"three colons":                         {[]string{"*:*"}, "local:a:b:s1/review/reviewer", ""},
		"nothing after the prefix":             {[]string{"*:*"}, "local::s1/review/reviewer", ""},
		"explicit form with traversal":         {[]string{"*:*"}, "local:remote:../review/reviewer", ""},
		"prefix in the middle is not a prefix": {[]string{"*:*"}, "b:local:s1/review/reviewer", ""},
	} {
		t.Run(name, func(t *testing.T) {
			settings := crossSessionSettings()
			settings.Sessions = test.sessions
			called := false
			messenger := &fakeMessenger{reply: func(context.Context) (string, error) { called = true; return "ok", nil }}
			output := callProfileMessage(t, settings, messenger, `{"profile":"`+test.profile+`","message":"hi"}`)
			if test.session == "" {
				if called || messenger.request.Session != "" || messenger.target != "" {
					t.Fatalf("the messenger was reached (session %q, target %q): %s", messenger.request.Session, messenger.target, output)
				}
				return
			}
			if !called || messenger.request.Session != test.session || messenger.target != "review/reviewer" {
				t.Fatalf("session %q target %q, want %q: %s", messenger.request.Session, messenger.target, test.session, output)
			}
		})
	}
}

// The listing tells the model how to write a workspace named remote.
func TestProfileMessageListTextNamesTheExplicitLocalForm(t *testing.T) {
	if text := profileMessageListText(nil, []string{"remote:*"}, nil); !strings.Contains(text, "local:remote:<session>/<category>/<profile>") {
		t.Errorf("list text: %s", text)
	}
}

// A refused cross-workspace reset is its own error code, not a failed reset that "may need
// attention", and it says nothing was sent or changed.
func TestProfileMessageForeignResetRefusalIsItsOwnAnswer(t *testing.T) {
	settings := crossSessionSettings()
	settings.Sessions = []string{"b:*"}
	messenger := &fakeMessenger{prepareErr: ErrContextTriggerForeignReset}
	output := callProfileMessage(t, settings, messenger, `{"profile":"b:s1/review/reviewer","message":"hi"}`)
	for _, want := range []string{"error_code: context_trigger_refused", "Nothing was sent or changed"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q: %s", want, output)
		}
	}
	if strings.Contains(output, "may need attention") {
		t.Errorf("a refusal is not a failed reset: %s", output)
	}
}
