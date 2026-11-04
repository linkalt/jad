package sessionfs

import (
	"strings"
	"testing"
)

func TestNativeSubagentWorktreeDefaultsAndValues(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"plain":{"tools":["write"]}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Worktree != DefaultNativeSubagentWorktreeSettings() || got.Agents[0].Isolation != "" || got.Agents[0].Isolated() || !got.Agents[0].ChangesWorkspace() {
		t.Fatalf("defaults: %+v, agent %+v", got.Worktree, got.Agents[0])
	}
	want := NativeSubagentWorktreeSettings{Dir: "", BranchPrefix: "jad/", Git: "git", AuthorName: "jad", AuthorEmail: "jad@localhost", TimeoutMS: 60_000}
	if got.Worktree != want {
		t.Fatalf("default values = %+v", got.Worktree)
	}

	custom, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{
		"worktree": {"dir": "trees", "branch_prefix": "agents/", "git": "/usr/bin/git", "author_name": "Bot", "author_email": "bot@example.com", "commit_hooks": true, "timeout_ms": 5000},
		"agents": {"iso": {"tools": ["read","write"], "isolation": "worktree"}, "none": {"tools": ["bash"], "isolation": "none"}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	want = NativeSubagentWorktreeSettings{Dir: "/w/trees", BranchPrefix: "agents/", Git: "/usr/bin/git", AuthorName: "Bot", AuthorEmail: "bot@example.com", CommitHooks: true, TimeoutMS: 5000}
	if custom.Worktree != want {
		t.Fatalf("values = %+v", custom.Worktree)
	}
	iso, _ := custom.Agent("iso")
	none, _ := custom.Agent("none")
	if !iso.Isolated() || iso.ChangesWorkspace() || !iso.ChangesFiles() || none.Isolated() || none.Isolation != "" {
		t.Fatalf("agents: iso %+v, none %+v", iso, none)
	}
}

func TestNativeSubagentWorktreeRefusals(t *testing.T) {
	for name, test := range map[string]struct{ section, want string }{
		"unknown isolation":    {`{"agents":{"a":{"tools":["write"],"isolation":"container"}}}`, `isolation must be "none" or "worktree"`},
		"isolation not text":   {`{"agents":{"a":{"tools":["write"],"isolation":true}}}`, "isolation must be a non-empty string"},
		"read-only isolated":   {`{"agents":{"a":{"isolation":"worktree"}}}`, "isolation worktree needs a tool that changes files"},
		"read tools isolated":  {`{"agents":{"a":{"tools":["read"],"isolation":"worktree"}}}`, "isolation worktree needs a tool that changes files"},
		"worktree not object":  {`{"worktree":[],"agents":{}}`, "worktree must be an object"},
		"unknown worktree key": {`{"worktree":{"keep":true},"agents":{}}`, "keep"},
		"bad prefix":           {`{"worktree":{"branch_prefix":"a b/"},"agents":{}}`, "cannot start a git branch name"},
		"dash prefix":          {`{"worktree":{"branch_prefix":"-x/"},"agents":{}}`, "cannot start a git branch name"},
		"dots prefix":          {`{"worktree":{"branch_prefix":"a..b/"},"agents":{}}`, "cannot start a git branch name"},
		"blank git":            {`{"worktree":{"git":" "},"agents":{}}`, "git must be a non-empty string"},
		"timeout too small":    {`{"worktree":{"timeout_ms":10},"agents":{}}`, "timeout_ms"},
		"timeout too large":    {`{"worktree":{"timeout_ms":600001},"agents":{}}`, "timeout_ms"},
		"hooks not a boolean":  {`{"worktree":{"commit_hooks":"yes"},"agents":{}}`, "commit_hooks"},
	} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(test.section), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}
