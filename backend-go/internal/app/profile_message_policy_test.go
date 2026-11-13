package app

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// Reset across workspaces is allowed only by the target workspace's allow_reset, and then it is
// really carried out: the target's history before the message is gone from its transcript.
func TestProfileMessageCrossWorkspaceResetNeedsAllowReset(t *testing.T) {
	allow := `{"profile_message":{"allow_reset":[{"from":"local:a/*/dev/*","to":"local:b/*/review/*"}]}}`
	for name, test := range map[string]struct {
		workspace string
		denied    bool
	}{
		"matching rule":        {allow, false},
		"rule of another from": {`{"profile_message":{"allow_reset":[{"from":"local:a/*/ops/*"}]}}`, true},
		"rule of another to":   {`{"profile_message":{"allow_reset":[{"to":"local:b/*/admin/*"}]}}`, true},
		"rule without keys":    {`{"profile_message":{"allow_reset":[{}]}}`, false},
		"no rule":              {`{}`, true},
		"an empty list":        {`{"profile_message":{"allow_reset":[]}}`, true},
		"a deny beats it":      {`{"profile_message":{"allow_reset":[{}],"deny":[{"to":"local:b/*/review/*","actions":["reset"]}]}}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			w := startMessageWorkspaces(t, messageWorkspaceOptions{bWorkspace: test.workspace})
			first, err := w.prepare(t, ownerA, "b:s1", "review/reviewer", func(r *native.ProfileMessageRequest) { r.Message = "first-message" })
			if err != nil {
				t.Fatal(err)
			}
			w.send(t, first)
			before := w.transcript(w.b, "s1", "review/reviewer")
			plan, err := w.prepare(t, ownerA, "b:s1", "review/reviewer", withTrigger(sessionfs.ProfileMessageContextReset, 0), func(r *native.ProfileMessageRequest) { r.Message = "second-message" })
			if test.denied {
				if !isDenied(err, "workspace:b", "reset") || plan != nil {
					t.Fatalf("plan %v, err %v", plan, err)
				}
				if after := w.transcript(w.b, "s1", "review/reviewer"); after != before {
					t.Fatalf("the target changed although the reset was refused:\n%s", after)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if outcome := w.send(t, plan); outcome.Err != nil {
				t.Fatalf("outcome = %+v", outcome)
			}
			got := w.transcript(w.b, "s1", "review/reviewer")
			if strings.Contains(got, "first-message") || !strings.Contains(got, "second-message") {
				t.Fatalf("the target was not reset before the message:\n%s", got)
			}
		})
	}
}

// `{}` in a workspace means it neither sends nor receives; the first refusal names the workspace
// whose rule it was, whichever side it is on.
func TestProfileMessageEmptyWorkspaceDenyClosesBothDirections(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{bWorkspace: `{"profile_message":{"deny":[{}]}}`})
	if _, err := w.prepare(t, ownerA, "b:s1", "review/reviewer"); !isDenied(err, "workspace:b", "message") {
		t.Fatalf("a to b: %v", err)
	}
	if _, err := w.prepare(t, ownerB, "a:s1", "dev/base"); !isDenied(err, "workspace:b", "message") {
		t.Fatalf("b to a: %v", err)
	}
	// Inside b the same rule refuses too: it is one list for every call touching b.
	if _, err := w.prepare(t, ownerB, "", "review/reviewer"); !isDenied(err, "workspace:b", "message") {
		t.Fatalf("b to itself: %v", err)
	}
	// A call that touches neither side is unaffected by b's rule.
	if _, err := w.prepare(t, "a:s1\x00dev/base", "", "dev/base"); !errors.Is(err, native.ErrProfileCycle) {
		t.Fatalf("a to a: %v", err)
	}
}

// The rules match the canonical address, so a short form, the session spelled out and the
// qualified form are all one profile: no spelling bypasses a rule.
func TestProfileMessageDenyMatchesTheCanonicalAddressWhateverTheSpelling(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{aReviewer: true, aWorkspace: `{"profile_message":{"deny":[{"to":"local:a/*/review/*"}]}}`})
	for _, session := range []string{"", "s1", "a:s1"} {
		if _, err := w.prepare(t, ownerA, session, "review/reviewer"); !isDenied(err, "workspace:a", "message") {
			t.Errorf("session %q: %v", session, err)
		}
	}
	// A profile outside the pattern is unaffected.
	if _, err := w.prepare(t, ownerA, "b:s1", "review/reviewer"); err != nil {
		t.Fatalf("a to b: %v", err)
	}
}

// Each action is judged on its own: a reset-only rule leaves the message, a message rule
// refuses the reset and the model choice with it, and compact and model_choice have their own.
func TestProfileMessageDenyActions(t *testing.T) {
	model := func(r *native.ProfileMessageRequest) { r.Model = "openai/default" }
	for name, test := range map[string]struct {
		rule   string
		change []func(*native.ProfileMessageRequest)
		denied string // action, "" when the call goes through
	}{
		"reset rule, plain message":   {`{"to":"local:a/*/review/*","actions":["reset"]}`, nil, ""},
		"reset rule, reset":           {`{"to":"local:a/*/review/*","actions":["reset"]}`, []func(*native.ProfileMessageRequest){withTrigger(sessionfs.ProfileMessageContextReset, 0)}, "reset"},
		"reset rule, compact":         {`{"to":"local:a/*/review/*","actions":["reset"]}`, []func(*native.ProfileMessageRequest){withTrigger(sessionfs.ProfileMessageContextCompact, 0)}, ""},
		"compact rule, compact":       {`{"to":"local:a/*/review/*","actions":["compact"]}`, []func(*native.ProfileMessageRequest){withTrigger(sessionfs.ProfileMessageContextCompact, 0)}, "compact"},
		"compact rule, plain message": {`{"to":"local:a/*/review/*","actions":["compact"]}`, nil, ""},
		"model rule, a named model":   {`{"to":"local:a/*/review/*","actions":["model_choice"]}`, []func(*native.ProfileMessageRequest){model}, "model_choice"},
		"model rule, no model":        {`{"to":"local:a/*/review/*","actions":["model_choice"]}`, nil, ""},
		"message rule, plain":         {`{"to":"local:a/*/review/*"}`, nil, "message"},
		"message rule, reset":         {`{"to":"local:a/*/review/*"}`, []func(*native.ProfileMessageRequest){withTrigger(sessionfs.ProfileMessageContextReset, 0)}, "message"},
		"explicit message action":     {`{"to":"local:a/*/review/*","actions":["message"]}`, nil, "message"},
		"from another profile only":   {`{"from":"local:a/*/ops/*","to":"local:a/*/review/*"}`, nil, ""},
		"from this caller":            {`{"from":"local:a/s1/dev/base","to":"local:a/*/review/*"}`, nil, "message"},
	} {
		t.Run(name, func(t *testing.T) {
			w := startMessageWorkspaces(t, messageWorkspaceOptions{aReviewer: true, aWorkspace: `{"profile_message":{"deny":[` + test.rule + `]}}`})
			plan, err := w.prepare(t, ownerA, "", "review/reviewer", test.change...)
			if test.denied == "" {
				if err != nil || plan == nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !isDenied(err, "workspace:a", test.denied) {
				t.Fatalf("err = %v, want a refusal of %s", err, test.denied)
			}
		})
	}
}

// global.json rules apply to every call and name "global"; they come before the workspace's.
func TestProfileMessageGlobalDeny(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{aReviewer: true, global: `{"deny":[{"from":"local:a/*/*/*","to":"local:b/*/*/*"}]}`, aWorkspace: `{"profile_message":{"deny":[{}]}}`})
	if _, err := w.prepare(t, ownerA, "b:s1", "review/reviewer"); !isDenied(err, "global", "message") {
		t.Fatalf("a to b: %v", err)
	}
	// Only the pair the rule names: a to a is the workspace's own rule, b to a is not matched by it.
	if _, err := w.prepare(t, ownerA, "", "review/reviewer"); !isDenied(err, "workspace:a", "message") {
		t.Fatalf("a to a: %v", err)
	}
	if _, err := w.prepare(t, ownerB, "a:s1", "dev/base"); !isDenied(err, "workspace:a", "message") {
		t.Fatalf("b to a: %v", err)
	}
	open := startMessageWorkspaces(t, messageWorkspaceOptions{global: `{"deny":[{"from":"local:scratch/*/*/*"}]}`})
	if _, err := open.prepare(t, ownerA, "b:s1", "review/reviewer"); err != nil {
		t.Fatalf("a rule about another workspace refused: %v", err)
	}
}

// Through the tool: the caller's own toolset deny answers before anything is looked up, so a
// profile that does not exist is refused the same way as one that does; the target is never
// touched. A workspace rule gives the same answer from the messenger.
func TestProfileMessageToolAnswersProfileDenied(t *testing.T) {
	for name, options := range map[string]messageWorkspaceOptions{
		"toolset deny":   {aToolset: "coding/talk", talk: `{"timeout_ms":60000,"deny":[{"to":"local:b/*/review/*"}]}`},
		"workspace deny": {aToolset: "coding/talk", aWorkspace: `{"profile_message":{"deny":[{"to":"local:b/*/review/*"}]}}`},
		"global deny":    {aToolset: "coding/talk", global: `{"deny":[{"to":"local:b/*/review/*"}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			w := startMessageWorkspaces(t, options)
			reply := w.runAsk(t, "local:b/s1/review/reviewer")
			if !strings.Contains(reply, "profile_denied") || !strings.Contains(reply, "local:b/s1/review/reviewer") {
				t.Fatalf("a's transcript lacks the refusal:\n%s", reply)
			}
			by := map[string]string{"toolset deny": "caller", "workspace deny": "workspace:a", "global deny": "global"}[name]
			if !strings.Contains(reply, "by: "+by) {
				t.Errorf("the refusal does not say %q:\n%s", by, reply)
			}
			if got := w.transcript(w.b, "s1", "review/reviewer"); got != "" {
				t.Fatalf("b was messaged: %s", got)
			}
			if strings.Contains(reply, w.a) || strings.Contains(reply, w.b) {
				t.Fatalf("the refusal shows a path:\n%s", reply)
			}
		})
	}
	// A toolset deny answers before the lookup: a profile that does not exist reads the same.
	w := startMessageWorkspaces(t, messageWorkspaceOptions{aToolset: "coding/talk", talk: `{"timeout_ms":60000,"deny":[{"to":"local:b/*/review/*"}]}`})
	if reply := w.runAsk(t, "local:b/nope/review/ghost"); !strings.Contains(reply, "profile_denied") {
		t.Fatalf("a missing target of a denied pattern:\n%s", reply)
	}
}

// runAsk has a's dev/base call profile_message with profile and returns its transcript.
func (w *messageWorkspaces) runAsk(t *testing.T, profile string) string {
	t.Helper()
	call := scriptFunctionCall("profile_message", map[string]any{"profile": profile, "message": "question-for-b"})
	w.script = func(model, body string) string {
		if strings.Contains(body, "ask-b") && !strings.Contains(body, "function_call_output") {
			return call(model, body)
		}
		return ""
	}
	request, err := http.NewRequest(http.MethodPost, w.api.URL+"/api/sessions/"+url.PathEscape("a:s1")+"/run?profile="+url.QueryEscape("dev/base"), strings.NewReader(`{"input":"ask-b"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("run: %d %s", response.StatusCode, body)
	}
	return w.transcript(w.a, "s1", "dev/base")
}

// The refusal comes from the messenger for a rule the tool cannot see (the target workspace's),
// and the tool description keeps a refused profile of the caller's own session out of its list.
func TestProfileMessageDeniedTargetsAreNotAdvertised(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{aToolset: "coding/talk", aReviewer: true, aWorkspace: `{"profile_message":{"deny":[{"to":"local:a/*/review/*"}]}}`})
	config, err := w.service.resolver.Resolve(t.Context(), "a:s1", "dev/base", "")
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := config.Runner.(boundRunner)
	if !ok {
		t.Fatalf("runner %T", config.Runner)
	}
	settings := bound.config.NativeProfileMessage
	if settings == nil || len(settings.WorkspaceDeny) != 1 {
		t.Fatalf("settings = %+v", settings)
	}
	targets := sessionfs.ProfileMessageTargets(bound.config.Profiles, bound.config.Profile, nil)
	if got := settings.ShownTargets(targets); len(got) != 0 {
		t.Fatalf("shown = %q, want none (review/reviewer is refused as a whole)", got)
	}
	if len(targets) != 1 || filepath.Base(targets[0]) != "reviewer" {
		t.Fatalf("targets = %q", targets)
	}
}
