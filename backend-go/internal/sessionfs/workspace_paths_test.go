package sessionfs

import (
	"path/filepath"
	"slices"
	"testing"
)

// A relative path in a workspace file resolves from W/.jad and one in a global file from G
// (decider ruling on phase 1a): two workspaces that both name env_file ".env" read their own
// file, never G/.env, and a global toolset still reads its own paths from G.
func TestWorkspacePathsResolveFromTheFolderOfTheirFile(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	b := f.workspace(t, "b", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}],"libraries":{"tool_definition_paths":["defs"]},"defaults":{"session_files":{"paths":{"deny":["gfiles"]}}}}`)
	wsPut(t, filepath.Join(f.global, ".env"), "FROM=global\n")
	wsPut(t, filepath.Join(f.global, "g.env"), "FROM=global-toolset\n")
	wsPut(t, filepath.Join(f.global, "tools", "dev", "shell.json"), `{"native":{"exec":{"paths":{"deny":["gdeny"]}}}}`)
	wsPut(t, filepath.Join(f.global, "tools", "dev", "own.json"), `{"native":{"exec":{"env_file":"g.env"}}}`)
	for _, dir := range []string{a, b} {
		wsPut(t, filepath.Join(dir, ".jad", ".env"), "FROM="+filepath.Base(dir)+"\n")
		wsPut(t, filepath.Join(dir, ".jad", "w.env"), "FROM=workspace-toolset\n")
		wsPut(t, filepath.Join(dir, ".jad", "tools", "ws", "shell.json"), `{"native":{"exec":{"env_file":"w.env","paths":{"deny":["wdeny"]}}}}`)
		wsPut(t, filepath.Join(dir, ".jad", "profiles", "dev", "env.json"), `{"models":["openai/default"],"toolset":["dev/shell"],"settings":{"env_file":".env","hooks":{"env_file":".env"}}}`)
		wsPut(t, filepath.Join(dir, ".jad", "profiles", "dev", "global.json"), `{"models":["openai/default"],"toolset":["dev/own"]}`)
		wsPut(t, filepath.Join(dir, ".jad", "profiles", "dev", "ws.json"), `{"models":["openai/default"],"toolset":["ws/shell"]}`)
	}
	config := wsMustLoad(t, f, a)
	for _, dir := range []string{a, b} {
		id := filepath.Base(dir)
		session, err := wsSession(t, config, id+":s1", "dev/env")
		if err != nil {
			t.Fatal(err)
		}
		own := filepath.Join(dir, ".jad", ".env")
		if session.NativeExec == nil || session.NativeExec.Child.EnvFile != own || session.NativeExec.Child.Env["FROM"] != id {
			t.Fatalf("%s: exec env_file = %+v, want %s", id, session.NativeExec, own)
		}
		if session.Hooks.EnvFile != own {
			t.Fatalf("%s: hooks env_file = %s, want %s", id, session.Hooks.EnvFile, own)
		}
		if !hasDenyRule(session.NativeExec.Child.Policy, filepath.Join(f.global, "gdeny")) {
			t.Fatalf("%s: a global toolset's path rule must resolve from G: %+v", id, session.NativeExec.Child.Policy)
		}
		if !hasDenyRule(session.FilePolicy, filepath.Join(f.global, "gfiles")) {
			t.Fatalf("%s: a global default's path rule must resolve from G: %+v", id, session.FilePolicy)
		}
		if !slices.Equal(session.ToolDefinitionPaths, []string{filepath.Join(f.global, "defs")}) {
			t.Fatalf("%s: tool_definition_paths of global.json = %q, want them from G", id, session.ToolDefinitionPaths)
		}
		global, err := wsSession(t, config, id+":s1", "dev/global")
		if err != nil {
			t.Fatal(err)
		}
		if global.NativeExec.Child.EnvFile != filepath.Join(f.global, "g.env") {
			t.Fatalf("%s: a global toolset's env_file = %s", id, global.NativeExec.Child.EnvFile)
		}
		ws, err := wsSession(t, config, id+":s1", "dev/ws")
		if err != nil {
			t.Fatal(err)
		}
		if ws.NativeExec.Child.EnvFile != filepath.Join(dir, ".jad", "w.env") || !hasDenyRule(ws.NativeExec.Child.Policy, filepath.Join(dir, ".jad", "wdeny")) {
			t.Fatalf("%s: a workspace toolset must resolve from W/.jad: %s %+v", id, ws.NativeExec.Child.EnvFile, ws.NativeExec.Child.Policy)
		}
	}
}

func hasDenyRule(policy NativePathPolicy, path string) bool {
	return slices.ContainsFunc(policy.Paths, func(rule NativePathRule) bool { return rule.Action == "deny" && rule.Path == path })
}

// A narrowed sandbox keeps every sealed folder of both, and a helper folder only where the
// parent lets it stay writable too.
func TestNarrowedSandboxKeepsSealedFoldersAndOnlySharedHelpers(t *testing.T) {
	jad := "/w/.jad"
	sandbox := func(sealed, inner []string) *SandboxSettings {
		return &SandboxSettings{Tools: []string{"exec"}, Backend: SandboxBackendBwrap, Executable: "bwrap", WritablePaths: []string{"/w"}, SealedPaths: sealed, SealedInner: inner}
	}
	helper := filepath.Join(jad, "functions")
	narrowed, err := NarrowSandbox(sandbox([]string{jad}, nil), sandbox([]string{"/g"}, []string{helper, "/other/exec"}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(narrowed.SealedPaths, []string{"/g", jad}) || !slices.Equal(narrowed.SealedInner, []string{"/other/exec"}) {
		t.Fatalf("sealed %q inner %q", narrowed.SealedPaths, narrowed.SealedInner)
	}
	narrowed, err = NarrowSandbox(sandbox([]string{jad}, []string{helper}), sandbox(nil, []string{helper}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(narrowed.SealedInner, []string{helper}) {
		t.Fatalf("inner %q", narrowed.SealedInner)
	}
}

// The in-process readers of a workspace session get the fixed hidden paths (decider fix C).
func TestWorkspaceReadersGetTheFixedHiddenPaths(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	b := f.workspace(t, "b", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	wsPut(t, filepath.Join(f.global, "tools", "dev", "read.json"), `{"native":{"transcript":{"allow_path":true},"transcripts":{},"code_context":{},"exec":{}}}`)
	wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", "reader.json"), `{"models":["openai/default"],"toolset":["dev/read"]}`)
	session, err := wsSession(t, wsMustLoad(t, f, a), "a:s1", "dev/reader")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(f.global, "auth"), filepath.Join(b, ".jad")}
	for name, policy := range map[string]NativePathPolicy{"transcript": session.NativeTranscript.Policy, "transcripts": session.NativeTranscripts.Policy,
		"code_context": session.NativeCodeContext.Policy, "exec": session.NativeExec.Child.Policy, "files": session.FilePolicy} {
		if !slices.Equal(policy.Hidden, want) {
			t.Errorf("%s hidden = %q, want %q", name, policy.Hidden, want)
		}
	}
}
