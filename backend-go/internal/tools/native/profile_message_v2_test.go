package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// v2Settings are the profile_message settings of the session release of workspace app, profile
// dev/coder, which may reach the workspaces reports and remote.
func v2Settings() sessionfs.NativeProfileMessageSettings {
	settings := profileMessageSettings()
	settings.Caller = sessionfs.MessageCaller{Workspace: "app", Session: "release", Profile: "dev/coder"}
	settings.Workspaces = []string{"reports", "remote"}
	return settings
}

var v2Targets = []string{"dev/reviewer", "review/security"}

func callV2(t *testing.T, settings sessionfs.NativeProfileMessageSettings, messenger ProfileMessenger, profile string) string {
	t.Helper()
	tool, err := NewProfileMessageTool(settings, v2Targets, messenger, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(map[string]string{"profile": profile, "message": "hi"})
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": "profile_message", "arguments": string(arguments)})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{SessionKey: "release\x00dev/coder"})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}

// The five address forms of the workspace layout reach the messenger as a session reference
// (empty for the calling session, bare for another session of the workspace, qualified for another
// workspace) and a <category>/<profile>; the answer names the profile by its canonical address.
func TestProfileMessageV2AddressForms(t *testing.T) {
	for name, test := range map[string]struct {
		profile, session, target, label string
	}{
		"name only":                  {"reviewer", "", "dev/reviewer", "local:app/release/dev/reviewer"},
		"category and name":          {"review/security", "", "review/security", "local:app/release/review/security"},
		"own session spelled out":    {"release/review/security", "", "review/security", "local:app/release/review/security"},
		"own workspace and session":  {"local:app/release/dev/reviewer", "", "dev/reviewer", "local:app/release/dev/reviewer"},
		"another session":            {"nightly/review/security", "nightly", "review/security", "local:app/nightly/review/security"},
		"own workspace, other":       {"local:app/nightly/review/security", "nightly", "review/security", "local:app/nightly/review/security"},
		"another workspace":          {"local:reports/daily/review/auditor", "reports:daily", "review/auditor", "local:reports/daily/review/auditor"},
		"workspace named remote":     {"local:remote/main/dev/x", "remote:main", "dev/x", "local:remote/main/dev/x"},
		"session named like a scope": {"local/review/security", "local", "review/security", "local:app/local/review/security"},
	} {
		t.Run(name, func(t *testing.T) {
			messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "answer", nil }}
			output := callV2(t, v2Settings(), messenger, test.profile)
			if messenger.request.Session != test.session || messenger.target != test.target {
				t.Fatalf("session %q target %q, want %q %q: %s", messenger.request.Session, messenger.target, test.session, test.target, output)
			}
			if !strings.Contains(output, "Profile "+test.label+" answered") || !strings.Contains(output, "profile: "+test.label) {
				t.Fatalf("result does not carry the canonical label %s: %s", test.label, output)
			}
		})
	}
}

// An address that is none of the five forms is invalid_profile_ref, whose text lists the forms;
// the messenger is never reached. This includes every old colon form.
func TestProfileMessageV2InvalidAddresses(t *testing.T) {
	for _, profile := range []string{
		"reports:daily/review/auditor",
		"local:reports:daily/review/auditor",
		"local:remote:main/dev/x",
		"local:reports/daily/auditor",
		"local:reports/daily/review/auditor/x",
		"a/b/c/d",
		"../review/security",
		"release/../security",
		"dev/coder\x00",
		"dev//coder",
	} {
		messenger := &fakeMessenger{reply: func(context.Context) (string, error) { t.Fatal("messenger called"); return "", nil }}
		output := callV2(t, v2Settings(), messenger, profile)
		for _, want := range []string{"error_code: invalid_profile_ref", "local:<workspace>/<session>/<category>/<name>", "remote:<binding>/<session>/<category>/<name>"} {
			if !strings.Contains(output, want) {
				t.Errorf("%q: output lacks %q: %s", profile, want, output)
			}
		}
	}
}

// A profile of the calling session that the session does not hold, and the caller itself, read
// like an unknown profile; any other session, workspace or profile is the messenger's to judge.
func TestProfileMessageV2OwnSessionTargetsAreTheSessionsProfiles(t *testing.T) {
	for _, profile := range []string{"ghost", "dev/ghost", "release/dev/ghost", "local:app/release/dev/ghost", "coder", "dev/coder", "local:app/release/dev/coder"} {
		messenger := &fakeMessenger{reply: func(context.Context) (string, error) { t.Fatal("messenger called"); return "", nil }}
		if output := callV2(t, v2Settings(), messenger, profile); !strings.Contains(output, "error_code: unknown_profile") {
			t.Errorf("%s: %s", profile, output)
		}
	}
	// Another session or workspace goes to the messenger, which says whether it exists.
	for _, profile := range []string{"nightly/dev/ghost", "local:reports/daily/dev/ghost"} {
		messenger := &fakeMessenger{prepareErr: ErrProfileUnknown}
		if output := callV2(t, v2Settings(), messenger, profile); !strings.Contains(output, "error_code: unknown_profile") || messenger.target != "dev/ghost" {
			t.Errorf("%s: target %q: %s", profile, messenger.target, output)
		}
	}
}

// The description lists the five forms, the profiles of the session and the reachable
// workspaces; a profile with no other profile still has a tool (it reaches by address), and v1
// keeps its error and its text.
func TestProfileMessageV2Description(t *testing.T) {
	tool, err := NewProfileMessageTool(v2Settings(), v2Targets, &fakeMessenger{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := tool.Definition.Description
	for _, want := range []string{"Profiles: dev/reviewer, review/security.", sessionfs.MessageRefForms, "Workspaces you can reach: reports, remote."} {
		if !strings.Contains(text, want) {
			t.Errorf("description lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "accept") || strings.Contains(text, "sessions:") {
		t.Errorf("description names the removed consent: %s", text)
	}
	if _, err := NewProfileMessageTool(v2Settings(), nil, &fakeMessenger{}, nil); err != nil {
		t.Errorf("v2 tool without session targets: %v", err)
	}
	if _, err := NewProfileMessageTool(profileMessageSettings(), nil, &fakeMessenger{}, nil); err == nil {
		t.Error("v1 tool without targets was built")
	}
	v1, err := NewProfileMessageTool(profileMessageSettings(), v2Targets, &fakeMessenger{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(v1.Definition.Description, "local:") {
		t.Errorf("v1 description shows v2 forms: %s", v1.Definition.Description)
	}
}
