package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type fakeMessenger struct {
	owner, target, message string
	reply                  func(ctx context.Context) (string, error)
	onCall                 func(busyWait time.Duration)
}

func (f *fakeMessenger) Message(ctx context.Context, owner, target, message string, busyWait time.Duration) (string, error) {
	if f.onCall != nil {
		f.onCall(busyWait)
	}
	f.owner, f.target, f.message = owner, target, message
	return f.reply(ctx)
}

var profileMessageTargets = []string{"reviewer", "team/coder"}

func profileMessageSettings() sessionfs.NativeProfileMessageSettings {
	return sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024}
}

func callProfileMessage(t *testing.T, settings sessionfs.NativeProfileMessageSettings, messenger ProfileMessenger, arguments string) string {
	t.Helper()
	tool, err := NewProfileMessageTool(settings, profileMessageTargets, messenger)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": "profile_message", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{SessionKey: "s1:main"})
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

func TestProfileMessageSuccessPassesOwnerTargetAndMessage(t *testing.T) {
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "  looks fine\n", nil }}
	output := callProfileMessage(t, profileMessageSettings(), messenger, `{"profile":"reviewer","message":"check it"}`)
	if messenger.owner != "s1:main" || messenger.target != "reviewer" || messenger.message != "check it" {
		t.Fatalf("messenger got %q %q %q", messenger.owner, messenger.target, messenger.message)
	}
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "looks fine") || strings.Contains(output, "truncated") {
		t.Fatalf("output = %s", output)
	}
}

func TestProfileMessageUnknownProfile(t *testing.T) {
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { t.Fatal("messenger called"); return "", nil }}
	output := callProfileMessage(t, profileMessageSettings(), messenger, `{"profile":"nobody","message":"hi"}`)
	if !strings.Contains(output, "error_code: unknown_profile") || !strings.Contains(output, "available: reviewer, team/coder") {
		t.Fatalf("output = %s", output)
	}
}

func TestProfileMessageSentinelErrorsAndFailure(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{ErrProfileBusy, "profile_busy"},
		{ErrProfileStopped, "profile_stopped"},
		{ErrProfileCycle, "profile_cycle"},
		{ErrProfileSameTranscript, "profile_same_transcript"},
		{ErrProfileNoAnswer, "profile_no_answer"},
		{errors.New("disk exploded"), "profile_failed"},
	}
	for _, c := range cases {
		messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "", c.err }}
		output := callProfileMessage(t, profileMessageSettings(), messenger, `{"profile":"reviewer","message":"hi"}`)
		if !strings.Contains(output, "error_code: "+c.code) {
			t.Errorf("%v: output = %s", c.err, output)
		}
		if c.code == "profile_failed" && strings.Contains(output, "disk exploded") {
			t.Errorf("internal error text leaked: %s", output)
		}
		if c.code == "profile_stopped" && !strings.Contains(output, "Profile reviewer was stopped") {
			t.Errorf("stopped text missing: %s", output)
		}
		if c.code == "profile_same_transcript" && !strings.Contains(output, "shares this conversation and cannot be called") {
			t.Errorf("same-transcript text missing: %s", output)
		}
		if c.code == "profile_no_answer" && (strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "Profile reviewer finished without an answer")) {
			t.Errorf("no-answer result wrong: %s", output)
		}
	}
}

func TestProfileMessageCallerCancelReturnsACancelledResult(t *testing.T) {
	messenger := &fakeMessenger{reply: func(ctx context.Context) (string, error) { return "", ctx.Err() }}
	tool, err := NewProfileMessageTool(profileMessageSettings(), profileMessageTargets, messenger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output, err := tool.Execute(ctx, map[string]any{"profile": "reviewer", "message": "hi"}, tools.ExecutionContext{CallID: "call_1", SessionKey: "s1:main"})
	if err != nil {
		t.Fatalf("error = %v, want a rendered result", err)
	}
	if !strings.Contains(output, "error_code: cancelled") {
		t.Fatalf("output = %s", output)
	}
}

func TestProfileMessageTimeoutCancelsTheMessenger(t *testing.T) {
	settings := profileMessageSettings()
	settings.TimeoutMs = 20
	messenger := &fakeMessenger{reply: func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			return "late", nil
		}
	}}
	output := callProfileMessage(t, settings, messenger, `{"profile":"reviewer","message":"hi"}`)
	if !strings.Contains(output, "error_code: profile_timeout") {
		t.Fatalf("output = %s", output)
	}
}

func TestProfileMessageBoundsTheReplyOnACharacterBoundary(t *testing.T) {
	settings := profileMessageSettings()
	settings.ResultMaxBytes = 11
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return strings.Repeat("é", 20), nil }}
	output := callProfileMessage(t, settings, messenger, `{"profile":"reviewer","message":"hi"}`)
	if !strings.Contains(output, "truncated: true") || !strings.Contains(output, strings.Repeat("é", 5)) || strings.Contains(output, strings.Repeat("é", 6)) {
		t.Fatalf("output = %s", output)
	}
}

func TestProfileMessageRejectsMissingOrBlankArguments(t *testing.T) {
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { t.Fatal("messenger called"); return "", nil }}
	for _, arguments := range []string{`{}`, `{"profile":"reviewer"}`, `{"message":"hi"}`, `{"profile":"reviewer","message":"  "}`, `{"profile":"","message":"hi"}`} {
		output := callProfileMessage(t, profileMessageSettings(), messenger, arguments)
		if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, "validation_failed") {
			t.Errorf("%s: output = %s", arguments, output)
		}
	}
}

func TestProfileMessageToolIsNotReadOnlyAndListsProfiles(t *testing.T) {
	tool, err := NewProfileMessageTool(profileMessageSettings(), profileMessageTargets, &fakeMessenger{})
	if err != nil {
		t.Fatal(err)
	}
	if tool.ReadOnly || tool.MayChangeFiles == nil || !tool.MayChangeFiles(map[string]any{}) {
		t.Fatalf("ReadOnly = %v, MayChangeFiles set = %v", tool.ReadOnly, tool.MayChangeFiles != nil)
	}
	if !strings.Contains(tool.Definition.Description, "reviewer, team/coder") {
		t.Fatalf("description = %q", tool.Definition.Description)
	}
	if _, err := NewProfileMessageTool(profileMessageSettings(), profileMessageTargets, nil); err == nil {
		t.Fatal("nil messenger accepted")
	}
}

func TestProfileMessageManifestMatchesTheInCodeSchemaAndNeedsAMessenger(t *testing.T) {
	manifest := filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile_message.json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []tools.Definition `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("manifest: %v, %d tools", err, len(document.Tools))
	}
	same, err := sameJSON(document.Tools[0].InputSchema, profileMessageInputSchema)
	if err != nil || !same {
		t.Fatalf("manifest inputSchema differs from the in-code schema (err %v)", err)
	}
	settings := profileMessageSettings()
	config := sessionfs.SessionConfig{SettingPath: filepath.Join(t.TempDir(), "setting.json"), ToolDefinitionPaths: []string{manifest}, NativeProfileMessage: &settings, Profile: "main", Profiles: []string{"main", "reviewer", "team/coder"}}
	catalog, err := BuildCatalog(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Definitions()) != 0 {
		t.Fatalf("offered without a messenger: %#v", catalog.Definitions())
	}
	catalog, err = BuildCatalogWithMessenger(config, nil, nil, nil, nil, nil, nil, nil, nil, nil, &fakeMessenger{})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != profileMessageToolID || !strings.Contains(definitions[0].Description, "reviewer, team/coder") {
		t.Fatalf("definitions = %#v", definitions)
	}
}

func TestProfileMessageToolApprovalFollowsSetting(t *testing.T) {
	for approval, want := range map[string]string{"": "", sessionfs.ApprovalOff: "", sessionfs.ApprovalAsk: tools.ApprovalAsk} {
		settings := profileMessageSettings()
		settings.Approval = approval
		tool, err := NewProfileMessageTool(settings, profileMessageTargets, &fakeMessenger{})
		if err != nil {
			t.Fatal(err)
		}
		if tool.Approval != want {
			t.Errorf("approval %q: Tool.Approval = %q, want %q", approval, tool.Approval, want)
		}
	}
}

func TestProfileMessageToolWithoutTargetsIsAConfigError(t *testing.T) {
	_, err := NewProfileMessageTool(profileMessageSettings(), nil, &fakeMessenger{})
	if err == nil || !strings.Contains(err.Error(), "no profile to message") {
		t.Fatalf("error = %v", err)
	}
}

func TestProfileMessageCatalogAllowsSessionProfilesExceptDeniedAndCaller(t *testing.T) {
	settings := profileMessageSettings()
	settings.DenyProfiles = []string{"team/coder"}
	config := sessionfs.SessionConfig{
		SettingPath:          filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths:  []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile_message.json")},
		NativeProfileMessage: &settings,
		Profile:              "main",
		Profiles:             []string{"main", "reviewer", "team/coder"},
	}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithMessenger(config, nil, nil, nil, nil, nil, nil, nil, nil, nil, messenger)
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || !strings.HasSuffix(definitions[0].Description, " Profiles: reviewer.") {
		t.Fatalf("definitions = %#v", definitions)
	}
	call := func(profile string) string {
		raw, err := catalog.ExecuteFunctionCall(context.Background(), json.RawMessage(`{"type":"function_call","call_id":"c","name":"profile_message","arguments":"{\"profile\":\"`+profile+`\",\"message\":\"hi\"}"}`), tools.ExecutionContext{SessionKey: "s1:main"})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if out := call("reviewer"); !strings.Contains(out, "answered") {
		t.Fatalf("reviewer output = %s", out)
	}
	for _, denied := range []string{"team/coder", "main"} {
		if out := call(denied); !strings.Contains(out, "unknown_profile") {
			t.Fatalf("%s output = %s", denied, out)
		}
	}

	config.Profiles = []string{"main", "team/coder"}
	if _, err := BuildCatalogWithMessenger(config, nil, nil, nil, nil, nil, nil, nil, nil, nil, messenger); err == nil || !strings.Contains(err.Error(), "no profile to message") {
		t.Fatalf("empty effective list error = %v", err)
	}
}

func TestProfileMessagePassesTheBusyWaitAndNamesWhatHoldsTheTarget(t *testing.T) {
	settings := profileMessageSettings()
	settings.BusyWaitMs = 250
	var waited time.Duration
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "", &ProfileBusyError{HeldBy: "team/coder"} }}
	messenger.onCall = func(busyWait time.Duration) { waited = busyWait }
	output := callProfileMessage(t, settings, messenger, `{"profile":"reviewer","message":"hi"}`)
	if waited != 250*time.Millisecond {
		t.Fatalf("busy wait = %v, want 250ms", waited)
	}
	for _, want := range []string{"error_code: profile_busy", "a message from profile team/coder", "still busy after waiting 250 ms"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q: %s", want, output)
		}
	}

	settings.BusyWaitMs = 0
	plain := callProfileMessage(t, settings, &fakeMessenger{reply: func(context.Context) (string, error) { return "", ErrProfileBusy }}, `{"profile":"reviewer","message":"hi"}`)
	if !strings.Contains(plain, "with a user run or a maintenance action") || strings.Contains(plain, "waiting") {
		t.Errorf("plain busy text wrong: %s", plain)
	}
}
