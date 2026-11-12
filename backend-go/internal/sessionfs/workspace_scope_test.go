package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wsCatalogKind(t *testing.T, err error) CatalogErrorKind {
	t.Helper()
	var catalogErr *CatalogError
	if !errors.As(err, &catalogErr) {
		t.Fatalf("not a catalog error: %v", err)
	}
	return catalogErr.Kind
}

// Decider fix D: catalog routes follow the scope. global is G's folders, workspace:<id> that
// workspace's profiles, models and toolsets, so a write for workspace b lands in b; without a
// scope profiles are the start workspace's and the rest global; a global delete checks every
// workspace's profiles.
func TestScopedCatalogLayoutFollowsTheScope(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	b := f.workspace(t, "b", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	wsPut(t, filepath.Join(b, ".jad", "models", "local", "llama.json"), `{"provider":"openai","model":"llama","api_key_env":"FIXTURE_KEY"}`)
	config := wsMustLoad(t, f, a)
	roots := config.Roots

	list := func(scope string, kind CatalogKind) []string {
		t.Helper()
		layout, _, err := ScopedCatalogLayout(roots, scope, kind)
		if err != nil {
			t.Fatalf("%s %s: %v", scope, kind, err)
		}
		files, err := ListCatalogFiles(layout, kind)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, file := range files {
			ids = append(ids, file.ID)
		}
		return ids
	}
	if got := strings.Join(list("global", CatalogModels), ","); got != "openai/default" {
		t.Errorf("global models = %s", got)
	}
	if got := strings.Join(list("workspace:b", CatalogModels), ","); got != "local/llama" {
		t.Errorf("b models = %s", got)
	}
	if got := strings.Join(list("", CatalogModels), ","); got != "openai/default" {
		t.Errorf("unscoped models = %s", got)
	}
	for scope, kind := range map[string]CatalogKind{"global": CatalogProfiles, "workspace:a": CatalogMCP, "workspace:nope": CatalogModels} {
		if _, _, err := ScopedCatalogLayout(roots, scope, kind); err == nil || wsCatalogKind(t, err) != CatalogNotFound {
			if err == nil {
				layout, _, _ := ScopedCatalogLayout(roots, scope, kind)
				_, err = ListCatalogFiles(layout, kind)
			}
			if err == nil || wsCatalogKind(t, err) != CatalogNotFound {
				t.Errorf("%s %s: %v, want not found", scope, kind, err)
			}
		}
	}
	if _, _, err := ScopedCatalogLayout(roots, "a", CatalogModels); err == nil || wsCatalogKind(t, err) != CatalogBadID {
		t.Errorf("a bad scope: %v", err)
	}

	// A profile written for b lands in b, not in the start workspace.
	layout, scopeRoots, err := ScopedCatalogLayout(roots, "workspace:b", CatalogProfiles)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteCatalogFile(layout, scopeRoots, CatalogProfiles, "dev/llama", []byte(`{"models":["local/llama"]}`), WriteCondition{IfNoneMatch: "*"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, ".jad", "profiles", "dev", "llama.json")); err != nil {
		t.Fatalf("the profile is not in b: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a, ".jad", "profiles", "dev", "llama.json")); err == nil {
		t.Fatal("the profile landed in the start workspace")
	}
	// b's profile names a model only b has: a's sessions are not b's, so a's profiles list is untouched.
	if got := strings.Join(list("", CatalogProfiles), ","); got != "dev/coder" {
		t.Errorf("unscoped profiles = %s (the start workspace's)", got)
	}

	// The global model is used by profiles of a and b: a global delete is refused and names them.
	global, globalRoots, err := ScopedCatalogLayout(roots, "global", CatalogModels)
	if err != nil {
		t.Fatal(err)
	}
	file, err := ReadCatalogFile(global, CatalogModels, "openai/default")
	if err != nil {
		t.Fatal(err)
	}
	err = DeleteCatalogFile(global, globalRoots, CatalogModels, "openai/default", file.ETag)
	if err == nil || wsCatalogKind(t, err) != CatalogInUse || !strings.Contains(err.Error(), "dev/coder") {
		t.Fatalf("global delete = %v", err)
	}
	// b's own model is used by b's new profile.
	own, ownRoots, _ := ScopedCatalogLayout(roots, "workspace:b", CatalogModels)
	file, _ = ReadCatalogFile(own, CatalogModels, "local/llama")
	if err := DeleteCatalogFile(own, ownRoots, CatalogModels, "local/llama", file.ETag); err == nil || wsCatalogKind(t, err) != CatalogInUse || !strings.Contains(err.Error(), "dev/llama") {
		t.Fatalf("workspace delete = %v", err)
	}
}

// Outside the workspace layout a scope is refused and the first layout root is used.
func TestScopedCatalogLayoutInV1(t *testing.T) {
	roots := []Root{{ID: "r", Layout: &Layout{Models: "/m"}}}
	if _, _, err := ScopedCatalogLayout(roots, "global", CatalogModels); err == nil || wsCatalogKind(t, err) != CatalogNotFound {
		t.Fatalf("v1 scope = %v", err)
	}
	layout, got, err := ScopedCatalogLayout(roots, "", CatalogModels)
	if err != nil || layout.Models != "/m" || len(got) != 1 {
		t.Fatalf("v1 = %+v %v %v", layout, got, err)
	}
}

// A scope prefix picks a model or toolset found in both scopes; {workspace} in a prompt
// document means W even when the session works in another cwd (design D1, D2).
func TestScopePrefixesAndTheWorkspacePlaceholder(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	other := filepath.Join(f.base, "elsewhere")
	wsPut(t, filepath.Join(other, "doc.md"), "from the cwd")
	wsPut(t, filepath.Join(a, "doc.md"), "from the workspace")
	wsPut(t, filepath.Join(a, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-own","api_key_env":"FIXTURE_KEY"}`)
	wsPut(t, filepath.Join(f.global, "tools", "dev", "t.json"), `{"native":{"exec":{}}}`)
	wsPut(t, filepath.Join(a, ".jad", "tools", "dev", "t.json"), `{"native":{"code_context":{}}}`)
	wsPut(t, filepath.Join(f.global, "models", "openai", "mini.json"), `{"provider":"openai","model":"gpt-mini","api_key_env":"FIXTURE_KEY"}`)
	wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", "pick.json"), `{"models":["workspace:openai/default","global:openai/mini"],"model_default":"global:openai/mini","toolset":["global:dev/t"],
		"prompt_documents":["{workspace}/doc.md","doc.md"],"settings":{"cwd":"`+other+`"}}`)
	wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", "bare.json"), `{"models":["openai/default"]}`)
	config := wsMustLoad(t, f, a)
	session, err := wsSession(t, config, "s1", "dev/pick")
	if err != nil {
		t.Fatal(err)
	}
	if session.ModelDefault != "openai/mini" || session.ModelPath != filepath.Join(f.global, "models", "openai", "mini.json") {
		t.Fatalf("model = %s %s", session.ModelDefault, session.ModelPath)
	}
	if picked, err := wsSession(t, config, "s1", "dev/pick"); err != nil || picked.ModelPaths["openai/default"] != filepath.Join(a, ".jad", "models", "openai", "default.json") {
		t.Fatalf("workspace:openai/default = %v %v", picked.ModelPaths, err)
	}
	if session.NativeExec == nil || session.NativeCodeContext != nil {
		t.Fatal("global:dev/t must pick the global toolset")
	}
	if session.Model.PromptText != "from the workspace\n\nfrom the cwd" {
		t.Fatalf("prompt = %q", session.Model.PromptText)
	}
	if _, err := wsSession(t, config, "s1", "dev/bare"); err == nil || !strings.Contains(err.Error(), "global:openai/default or workspace:openai/default") {
		t.Fatalf("a bare ambiguous name = %v", err)
	}
	wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", "pick.json"), `{"models":["workspace:openai/default"],"prompt_documents":["{workspace}doc.md"]}`)
	if _, err := wsSession(t, config, "s1", "dev/pick"); err == nil || !strings.Contains(err.Error(), "may only be followed by a /") {
		t.Fatalf("{workspace} glued to a name = %v", err)
	}
}

// The profile schema takes the scope prefixes the loader takes, and refuses others.
func TestProfileSchemaTakesScopePrefixes(t *testing.T) {
	schema := compileSchema(t, "profile.schema.json")
	if !schemaAccepts(t, schema, `{"schema_version":1,"models":["workspace:openai/default","global:openai","global:*"],"model_default":"global:openai/mini@high","toolset":["global:dev/t","dev/u"]}`) {
		t.Error("a profile with scope prefixes is refused")
	}
	// A profile's own models take profile: (phase 3c.1); own toolsets do not exist yet.
	if !schemaAccepts(t, schema, `{"schema_version":1,"models":["profile:openai/fast","profile:local"],"model_default":"profile:openai/fast@high"}`) {
		t.Error("a profile naming its own models is refused")
	}
	for _, refused := range []string{`{"schema_version":1,"toolset":"profile:dev/t"}`, `{"schema_version":1,"model_default":"other:openai/x"}`, `{"schema_version":1,"toolset":"global:workspace:dev/t"}`, `{"schema_version":1,"models":["profile:global:openai/x"]}`} {
		if schemaAccepts(t, schema, refused) {
			t.Errorf("%s is accepted", refused)
		}
	}
}
