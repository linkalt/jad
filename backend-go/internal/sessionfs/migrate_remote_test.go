package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const migrateTokenSecret = "MIGRATESECRET_0123456789abcdef0123456789ab"

// remotesFixture is the migrate fixture with a remotes folder holding a remote that reads its
// token from a file (relative path) and one that reads it from the environment and has a
// default session.
func remotesFixture(t *testing.T) *migrateFixture {
	t.Helper()
	f := newMigrateFixture(t)
	f.setting["catalogs"] = map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": "state", "remotes": "remotes"}
	f.writeSetting(t)
	f.put(t, "remotes/home.json", map[string]any{"schema_version": 1, "kind": "remote", "remote": map[string]any{"url": "http://127.0.0.1:1", "token_file": "tokens/home"}})
	f.put(t, "remotes/work.json", map[string]any{"schema_version": 1, "kind": "remote", "remote": map[string]any{"url": "http://10.0.0.2:8787", "token_env": "JAD_REMOTE_WORK", "default_session": "main", "timeout_ms": 3000}})
	f.write(t, "remotes/tokens/home", migrateTokenSecret)
	return f
}

// v1 remotes are never copied: each is a manual step with what to do for the keys it has, a
// token file's content is not read, and no remote group file is in the plan. The plan is not
// blocked by a remote, even one that names a place in the workspace (it is not copied to G).
func TestMigrationPlanDoesNotCopyRemotesAndListsManualSteps(t *testing.T) {
	f := remotesFixture(t)
	before := snapshotTree(t, f.base)
	plan := f.plan(t, "")
	if plan.Blocked() {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
	for _, file := range plan.Files {
		if file.Group == "remotes" {
			t.Fatalf("a remote is in the plan: %+v", file)
		}
	}
	notes := map[string]string{}
	for _, note := range plan.NotMigrated {
		if strings.Contains(note.Reason, "v1 remote") {
			notes[filepath.Base(note.Path)] = note.Reason
		}
	}
	for file, want := range map[string][]string{
		"home.json": {"v1 remote home is not copied", filepath.Join(f.global, "remotes", "home.json"), filepath.Join(f.global, "auth", "keys", "home.json"), "token_file is tokens/home", "the path itself is not carried over", "remote_keys", "remote.workspace"},
		"work.json": {"v1 remote work is not copied", "token_env JAD_REMOTE_WORK", "default_session is main", "remote:work/<session>/<category>/<name>"},
	} {
		for _, text := range want {
			if !strings.Contains(notes[file], text) {
				t.Errorf("%s: missing %q in\n%s", file, text, notes[file])
			}
		}
	}
	data, _ := json.Marshal(plan)
	if strings.Contains(string(data), migrateTokenSecret) {
		t.Fatal("the plan holds the content of a token file")
	}
	for _, secret := range plan.Secrets {
		if strings.Contains(secret.File, "remotes") {
			t.Errorf("a remote file is scanned as a secret holder: %+v", secret)
		}
	}
	if len(snapshotTree(t, f.base)) != len(before) {
		t.Fatal("the plan changed the tree")
	}
}

// Applying with v1 remotes writes nothing for them: G has no remotes folder entry, the v1 files
// stay, and v2 loads.
func TestApplyLeavesV1RemotesAloneAndWritesNoBindingOrKey(t *testing.T) {
	f := remotesFixture(t)
	before := hashes(t, f.base)
	result, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	unchanged(t, before, hashes(t, f.base))
	for _, path := range []string{filepath.Join(f.global, "remotes"), filepath.Join(f.global, "auth")} {
		if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
			t.Errorf("%s was written: %v", path, entries)
		}
	}
	for path := range hashes(t, f.global) {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), migrateTokenSecret) {
			t.Errorf("%s holds the token", path)
		}
	}
}

func profileMessageToolset(extra map[string]any) map[string]any {
	section := map[string]any{"timeout_ms": 60000}
	for key, value := range extra {
		section[key] = value
	}
	return map[string]any{"native": map[string]any{"async": map[string]any{}, "profile_message": section}, "mcp": map[string]any{"web/search": map[string]any{}}}
}

// A shared toolset that is copied to G has deny_profiles rewritten to deny rules that refuse the
// profile in every session and workspace, loses sessions (with a behaviour-change warning) and is
// warned about for its remotes; the source stays as it was, and the workspace layout loads the
// copy.
func TestMigrationRewritesProfileMessageKeysOfACopiedToolset(t *testing.T) {
	f := newMigrateFixture(t)
	// Only the tools are elsewhere; state, profiles and sessions stay in W/.jad (in place).
	v1 := filepath.Join(f.base, "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.jad, "tools"), filepath.Join(v1, "tools")); err != nil {
		t.Fatal(err)
	}
	f.setting["catalogs"] = map[string]any{"profiles": "profiles", "models": "models", "tools": filepath.Join(v1, "tools"), "mcp": "mcp", "state": "state"}
	f.writeSetting(t)
	f.put(t, filepath.Join(v1, "tools/coding/default.json"), profileMessageToolset(map[string]any{"deny_profiles": []string{"review/security", "dev/other"}, "sessions": []string{"*"}, "remotes": []string{"box"}}))
	plan := f.plan(t, "")
	if plan.Blocked() {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
	target := filepath.Join(f.global, "tools/coding/default.json")
	rewrites := map[string]MigrationRewrite{}
	for _, rewrite := range plan.Rewrites {
		if rewrite.File == target {
			rewrites[rewrite.Key] = rewrite
		}
	}
	if got := rewrites["native.profile_message.deny_profiles"]; got.From != `["review/security","dev/other"]` || got.To != `deny [{"to":"local:*/*/review/security"},{"to":"local:*/*/dev/other"}]` {
		t.Errorf("deny_profiles rewrite: %+v", got)
	}
	if got := rewrites["native.profile_message.sessions"]; got.To != "(removed)" {
		t.Errorf("sessions rewrite: %+v", got)
	}
	warnings := strings.Join(plan.Warnings, "\n")
	for _, want := range []string{"behaviour change: " + target + " drops native.profile_message.sessions", "lists remotes box in tools.native.profile_message"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("missing warning %q in\n%s", want, warnings)
		}
	}
	before := hashes(t, f.base)
	result, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	unchanged(t, before, hashes(t, f.base))
	moved, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var toolset struct {
		Native map[string]map[string]json.RawMessage `json:"native"`
	}
	if err := json.Unmarshal(moved, &toolset); err != nil {
		t.Fatal(err)
	}
	section := toolset.Native["profile_message"]
	if string(section["deny"]) != `[{"to":"local:*/*/review/security"},{"to":"local:*/*/dev/other"}]` || section["sessions"] != nil || section["deny_profiles"] != nil || string(section["timeout_ms"]) != "60000" || compactJSON(section["remotes"]) != `["box"]` {
		t.Fatalf("the copied toolset's profile_message: %s", moved)
	}
	source, _ := os.ReadFile(filepath.Join(v1, "tools/coding/default.json"))
	if !strings.Contains(string(source), "deny_profiles") || !strings.Contains(string(source), `"sessions"`) {
		t.Fatalf("the v1 source was changed: %s", source)
	}
	if result.Compared < 1 {
		t.Fatalf("compared %d", result.Compared)
	}
}

// A toolset that stays where it is (here the workspace's own folder) and holds a removed key is a
// blocker: v1 and v2 would read the same file and v2 would not load the profiles that use it.
func TestMigrationBlocksAToolsetInPlaceThatHoldsRemovedProfileMessageKeys(t *testing.T) {
	for _, key := range []string{"deny_profiles", "sessions"} {
		f := newMigrateFixture(t)
		value := any([]string{"review/security"})
		if key == "sessions" {
			value = []string{"*"}
		}
		f.put(t, "tools/coding/default.json", profileMessageToolset(map[string]any{key: value}))
		plan := f.plan(t, "")
		found := false
		for _, blocker := range plan.Blockers {
			found = found || blocker.Kind == migrationBlockRemovedKeys && strings.Contains(blocker.Message, "native.profile_message."+key) && blocker.Path == filepath.Join(f.jad, "tools/coding/default.json")
		}
		if !found {
			t.Errorf("%s: want %s: %+v", key, migrationBlockRemovedKeys, plan.Blockers)
		}
	}
	// A toolset without them stays unblocked.
	f := newMigrateFixture(t)
	f.put(t, "tools/coding/default.json", profileMessageToolset(map[string]any{"deny": nil}))
	if plan := f.plan(t, ""); hasBlocker(plan, migrationBlockRemovedKeys) {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
}

// An enabled remote API is told about: its token table is copied by nothing and the v2 server
// needs a workspace in every row.
func TestMigrationWarnsOfTheTokenTableOfAnEnabledRemoteAPI(t *testing.T) {
	f := newMigrateFixture(t)
	f.setting["server"] = map[string]any{"port": 4100, "remote_api": map[string]any{"enabled": true, "tokens_file": "/abs/tokens.json"}}
	f.writeSetting(t)
	plan := f.plan(t, "")
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "every row of its token table names the workspace it reaches") {
		t.Fatalf("warnings: %q", plan.Warnings)
	}
	f = newMigrateFixture(t)
	f.setting["server"] = map[string]any{"port": 4100, "remote_api": map[string]any{"enabled": false, "tokens_file": "/abs/tokens.json"}}
	f.writeSetting(t)
	if plan := f.plan(t, ""); strings.Contains(strings.Join(plan.Warnings, "\n"), "token table") {
		t.Fatalf("warned for a disabled remote API: %q", plan.Warnings)
	}
}

func TestWorkspaceProfileMessageIsIdempotentAndKeepsOtherKeys(t *testing.T) {
	raw := json.RawMessage(`{"timeout_ms":5,"deny_profiles":["a/b"],"remotes":["x"]}`)
	out, rewrites, err := workspaceProfileMessage(raw)
	if err != nil || len(rewrites) != 1 || string(out) != `{"timeout_ms":5,"remotes":["x"],"deny":[{"to":"local:*/*/a/b"}]}` {
		t.Fatalf("%s %+v %v", out, rewrites, err)
	}
	if again, more, err := workspaceProfileMessage(out); err != nil || len(more) != 0 || string(again) != string(out) {
		t.Fatalf("second pass: %s %+v %v", again, more, err)
	}
	if _, _, err := workspaceProfileMessage(json.RawMessage(`{"deny_profiles":"a/b"}`)); err == nil {
		t.Fatal("a v1 value that is not a list was rewritten")
	}
}
