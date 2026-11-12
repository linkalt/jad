package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// edit, revert and history reach the port with the caller's own owner key only, and the
// contents keep the order the model wrote them in (the raw arguments).
func TestProfileSelfEditReachesThePortForTheOwnProfile(t *testing.T) {
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	edit := `{"action":"edit","path":"self","if_match":"\"r1\"","contents":{"name":"x","description":"y"}}`
	execution := tools.ExecutionContext{SessionKey: listCaller, RawArguments: []byte(edit)}
	if out := callTranscripts(t, tool, edit, execution); !strings.Contains(out, `"action": "edit"`) {
		t.Fatalf("edit = %s", out)
	}
	// Raw text that says something else than the parsed arguments is not used.
	stale := tools.ExecutionContext{SessionKey: listCaller, RawArguments: []byte(`{"action":"edit","if_match":"r1","contents":{"name":"other"}}`)}
	callTranscripts(t, tool, `{"action":"edit","if_match":"r1","contents":{"name":"x"}}`, stale)
	if last := actions.calls[len(actions.calls)-1]; last != `edit demo dev/coder r1  {"name":"x"}` {
		t.Fatalf("edit with stale raw arguments = %q", last)
	}
	actions.calls = actions.calls[:len(actions.calls)-1]
	if out := callTranscripts(t, tool, `{"action":"revert","if_match":"r1","history_id":"000001-0123456789ab"}`, tools.ExecutionContext{SessionKey: listCaller}); !strings.Contains(out, `"action": "revert"`) {
		t.Fatalf("revert = %s", out)
	}
	if out := callTranscripts(t, tool, `{"action":"history","limit":3}`, tools.ExecutionContext{SessionKey: listCaller, PlanMode: true}); !strings.Contains(out, `"profile": "own"`) {
		t.Fatalf("history in plan mode = %s", out)
	}
	want := []string{
		`edit demo dev/coder "r1"  {"name":"x","description":"y"}`,
		"revert demo dev/coder r1 000001-0123456789ab ",
		"history demo dev/coder 3",
	}
	if strings.Join(actions.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q", actions.calls)
	}
}

// Each refusal of decider S2 holds whatever allow_children, allow_other_profiles and plan mode
// say, and none reaches the port.
func TestProfileSelfEditRefusals(t *testing.T) {
	settings := profileTestSettings()
	settings.AllowOtherProfiles, settings.AllowOtherSessions, settings.AllowChildren = true, true, true
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, settings, actions)
	edit := `{"action":"edit","if_match":"r","contents":{}`
	revert := `{"action":"revert","if_match":"r","history_id":"000001-0123456789ab"`
	cases := []struct {
		name, arguments, code string
		execution             tools.ExecutionContext
	}{
		{"sub-agent edit", edit + `}`, "profile_children_denied", tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1}},
		{"sub-agent history", `{"action":"history"}`, "profile_children_denied", tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 2}},
		{"other profile", edit + `,"path":"profile:dev/other"}`, "profile_self_only", tools.ExecutionContext{SessionKey: listCaller}},
		{"other session", revert + `,"path":"profile:other/dev/coder"}`, "profile_self_only", tools.ExecutionContext{SessionKey: listCaller}},
		{"other profile history", `{"action":"history","path":"profile:dev/other"}`, "profile_self_only", tools.ExecutionContext{SessionKey: listCaller}},
		{"plan mode", edit + `}`, "profile_plan_mode_denied", tools.ExecutionContext{SessionKey: listCaller, PlanMode: true}},
		{"plan guarded", revert + `}`, "profile_plan_mode_denied", tools.ExecutionContext{SessionKey: listCaller, PlanGuarded: true}},
		{"profile_message target", edit + `}`, "profile_started_run_denied", tools.ExecutionContext{SessionKey: listCaller, RunNode: "node-1", ChainDepth: 1}},
		{"chain only", revert + `}`, "profile_started_run_denied", tools.ExecutionContext{SessionKey: listCaller, ChainDepth: 1}},
		{"remote caller", edit + `}`, "profile_started_run_denied", tools.ExecutionContext{SessionKey: listCaller, RemoteHops: 1}},
		{"remote path", edit + `,"path":"remote:r/profile:s/dev/coder"}`, "profile_remote_denied", tools.ExecutionContext{SessionKey: listCaller}},
		{"no caller", edit + `}`, "profile_caller_unknown", tools.ExecutionContext{}},
		{"no if_match", `{"action":"edit","contents":{}}`, "if_match_required", tools.ExecutionContext{SessionKey: listCaller}},
		{"no contents", `{"action":"edit","if_match":"r"}`, "contents_required", tools.ExecutionContext{SessionKey: listCaller}},
		{"no history_id", `{"action":"revert","if_match":"r"}`, "history_id_required", tools.ExecutionContext{SessionKey: listCaller}},
	}
	for _, c := range cases {
		if out := callTranscripts(t, tool, c.arguments, c.execution); !strings.Contains(out, c.code) {
			t.Errorf("%s: %s", c.name, out)
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a refused change reached the port: %v", actions.calls)
	}
}

// A refusal of the store is reported with a code that says why.
func TestProfileSelfEditFailureCodes(t *testing.T) {
	for err, code := range map[error]string{
		&sessionfs.CatalogError{Kind: sessionfs.CatalogPreconditionFailed, Message: "changed"}: "profile_revision_changed",
		&sessionfs.ConflictError{Message: "busy"}:                                              "profile_edit_conflict",
		&sessionfs.ClientError{Message: "no"}:                                                  "profile_edit_refused",
	} {
		actions := &fakeProfileActions{failErr: err}
		tool := newProfileActionsTool(t, profileTestSettings(), actions)
		if out := callTranscripts(t, tool, `{"action":"edit","if_match":"r","contents":{}}`, tools.ExecutionContext{SessionKey: listCaller}); !strings.Contains(out, code) {
			t.Errorf("%v: %s", err, out)
		}
	}
}

// A profile definition copied before edit, history and revert (an old local
// tools_defintion/native/profile.json) is refused when the catalog is built, naming the sample
// to copy again; the current schema is accepted.
func TestAnOlderProfileDefinitionIsRefused(t *testing.T) {
	old := strings.Replace(string(profileInputSchema), `"config","edit","history","revert"]`, `"config"]`, 1)
	if old == string(profileInputSchema) {
		t.Fatal("the action enum was not found in the schema")
	}
	definitions := map[string]tools.Definition{profileToolID: {ID: profileToolID, Source: "native", Name: "profile", InputSchema: []byte(old)}}
	_, err := requireNativeDefinition(sessionfs.SessionConfig{SettingPath: "setting.json"}, definitions, "profile", profileToolID, profileInputSchema)
	if err == nil || !strings.Contains(err.Error(), "sample/tools/native/profile.json") {
		t.Fatalf("an older profile definition: %v", err)
	}
	definitions[profileToolID] = tools.Definition{ID: profileToolID, Source: "native", Name: "profile", InputSchema: profileInputSchema}
	if _, err := requireNativeDefinition(sessionfs.SessionConfig{}, definitions, "profile", profileToolID, profileInputSchema); err != nil {
		t.Fatal(err)
	}
}
