package sessionfs

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// twoMessageWorkspaces lists a and b in global.json, with a's and b's workspace.json given.
func twoMessageWorkspaces(t *testing.T, listA, listB bool, aJSON, bJSON string) (wsFixture, Config) {
	t.Helper()
	f := newWSFixture(t)
	a := f.workspace(t, "a", aJSON)
	b := f.workspace(t, "b", bJSON)
	wsPut(t, filepath.Join(b, ".jad", "sessions", "only-b.json"), `{"schema_version":1}`)
	var entries []string
	if listA {
		entries = append(entries, `{"id":"a","path":"`+a+`"}`)
	}
	if listB {
		entries = append(entries, `{"id":"b","path":"`+b+`"}`)
	}
	f.globalJSON(t, `{"workspaces":[`+strings.Join(entries, ",")+`]}`)
	return f, wsMustLoad(t, f, a)
}

// A workspace takes messages from another only when both are listed and its accept_from has a
// glob for the caller's id (design §29); the answer is the one of a session that does not exist.
func TestResolveMessageTargetNeedsBothWorkspacesToAgree(t *testing.T) {
	accept := `{"profile_message":{"accept_from":["a"]}}`
	for name, test := range map[string]struct {
		listA, listB bool
		a, b         string
		reference    string
		want         string // "" means refused
		foreign      bool
	}{
		"accepted":                  {true, true, `{}`, accept, "b:s1", "b:s1", true},
		"a session only b has":      {true, true, `{}`, accept, "b:only-b", "b:only-b", true},
		"glob":                      {true, true, `{}`, `{"profile_message":{"accept_from":["?"]}}`, "b:s1", "b:s1", true},
		"star":                      {true, true, `{}`, `{"profile_message":{"accept_from":["*"]}}`, "b:s1", "b:s1", true},
		"nothing configured":        {true, true, `{}`, `{}`, "b:s1", "", false},
		"empty list":                {true, true, `{}`, `{"profile_message":{"accept_from":[]}}`, "b:s1", "", false},
		"another workspace's glob":  {true, true, `{}`, `{"profile_message":{"accept_from":["c","aa"]}}`, "b:s1", "", false},
		"caller not listed":         {false, true, `{}`, accept, "b:s1", "", false},
		"target not listed":         {true, false, `{}`, accept, "b:s1", "", false},
		"missing session":           {true, true, `{}`, accept, "b:nope", "", false},
		"missing workspace":         {true, true, `{}`, accept, "c:s1", "", false},
		"own workspace, qualified":  {true, true, `{}`, `{}`, "a:s1", "a:s1", false},
		"own workspace, bare":       {true, true, `{}`, accept, "s1", "a:s1", false},
		"bare id of b only":         {true, true, `{}`, accept, "only-b", "", false},
		"bare id held by both is a": {true, true, `{}`, accept, "s1", "a:s1", false},
	} {
		t.Run(name, func(t *testing.T) {
			_, config := twoMessageWorkspaces(t, test.listA, test.listB, test.a, test.b)
			// A workspace that is not listed is not served (it is not the start directory).
			if want := map[bool]int{true: 2, false: 1}[test.listB]; len(config.Roots) != want {
				t.Fatalf("roots = %d, want %d", len(config.Roots), want)
			}
			location, foreign, err := ResolveMessageTarget(config.Roots, "a:s1", test.reference)
			if test.want == "" {
				if err == nil {
					t.Fatalf("%s resolved to %s", test.reference, location.Reference)
				}
				// The same error as a session that does not exist in a workspace that does.
				_, _, missing := ResolveMessageTarget(config.Roots, "a:s1", "b:never-existed")
				if err.Error() != missing.Error() && !strings.Contains(err.Error(), "invalid") {
					t.Fatalf("error %q differs from the one of a missing session %q", err, missing)
				}
				return
			}
			if err != nil || location.Reference != test.want || foreign != test.foreign {
				t.Fatalf("%s = %q foreign %v, %v; want %q foreign %v", test.reference, location.Reference, foreign, err, test.want, test.foreign)
			}
		})
	}
}

// Acceptance is judged on the caller's own workspace, not on anything the reference says, and
// is not symmetric: b accepting a does not let b message a.
func TestResolveMessageTargetIsNotSymmetric(t *testing.T) {
	_, config := twoMessageWorkspaces(t, true, true, `{}`, `{"profile_message":{"accept_from":["a"]}}`)
	if _, _, err := ResolveMessageTarget(config.Roots, "a:s1", "b:s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveMessageTarget(config.Roots, "b:s1", "a:s1"); err == nil {
		t.Fatal("a accepts nobody, yet b reached it")
	}
	// A caller that does not resolve reaches nothing.
	if _, _, err := ResolveMessageTarget(config.Roots, "zzz:s1", "b:s1"); err == nil {
		t.Fatal("an unknown caller reached b")
	}
}

// Outside the workspace layout a reference naming a root is not a way to another root.
func TestResolveMessageTargetRefusesTheColonFormInV1(t *testing.T) {
	roots := []Root{{ID: "one", Directory: t.TempDir()}, {ID: "two", Directory: t.TempDir()}}
	for _, root := range roots {
		wsPut(t, filepath.Join(root.Directory, "s1.json"), `{"schema_version":1}`)
	}
	if _, foreign, err := ResolveMessageTarget(roots, "one:s1", "two:s1"); err == nil || foreign {
		t.Fatalf("v1 reached another root: %v", err)
	}
	var ambiguous *ResolveError
	if _, _, err := ResolveMessageTarget(roots, "one:s1", "s1"); !errors.As(err, &ambiguous) || ambiguous.Kind != ResolveAmbiguous {
		t.Fatalf("v1 bare id held by both roots: %v", err)
	}
}

func TestWorkspaceJSONProfileMessageAcceptFrom(t *testing.T) {
	for name, text := range map[string]string{
		"not an object":    `{"profile_message":[]}`,
		"unknown key":      `{"profile_message":{"accept":["a"]}}`,
		"not a list":       `{"profile_message":{"accept_from":"a"}}`,
		"colon":            `{"profile_message":{"accept_from":["a:b"]}}`,
		"slash":            `{"profile_message":{"accept_from":["a/b"]}}`,
		"empty entry":      `{"profile_message":{"accept_from":[""]}}`,
		"class":            `{"profile_message":{"accept_from":["a[b]"]}}`,
		"duplicate":        `{"profile_message":{"accept_from":["a","a"]}}`,
		"null section":     `{"profile_message":null}`,
		"entry not string": `{"profile_message":{"accept_from":[1]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newWSFixture(t)
			a := f.workspace(t, "a", text)
			wsMustRefuse(t, f, a, "profile_message")
		})
	}
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{"profile_message":{"accept_from":["x","y-*"]}}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"}]}`)
	config := wsMustLoad(t, f, a)
	layout := config.Roots[0].Layout
	if !layout.Listed || len(layout.AcceptProfileMessagesFrom) != 2 {
		t.Fatalf("layout: listed %v accept %q", layout.Listed, layout.AcceptProfileMessagesFrom)
	}
	// Unlisted start workspace: not Listed, so it takes part in no cross-workspace message.
	g := newWSFixture(t)
	b := g.workspace(t, "b", `{"profile_message":{"accept_from":["*"]}}`)
	config = wsMustLoad(t, g, b)
	if config.Roots[0].Layout.Listed {
		t.Fatal("an unlisted start workspace counts as listed")
	}
	if !strings.Contains(strings.Join(config.Warnings, "\n"), "accept_from is ignored") {
		t.Fatalf("no warning that accept_from of an unlisted workspace does nothing: %q", config.Warnings)
	}
	if config.Roots[0].Layout.AcceptsProfileMessagesFrom(config.Roots[0].Layout, "b") {
		t.Fatal("an unlisted workspace accepts")
	}
}

func TestAcceptsProfileMessagesFromIsClosedOnNil(t *testing.T) {
	var layout *Layout
	if layout.AcceptsProfileMessagesFrom(&Layout{Listed: true}, "a") {
		t.Fatal("nil layout accepts")
	}
	listed := &Layout{Listed: true, AcceptProfileMessagesFrom: []string{"a"}}
	if listed.AcceptsProfileMessagesFrom(nil, "a") || !listed.AcceptsProfileMessagesFrom(&Layout{Listed: true}, "a") {
		t.Fatal("caller layout nil must refuse and a listed one with a matching id must be accepted")
	}
	if listed.AcceptsProfileMessagesFrom(&Layout{Listed: true}, "b") {
		t.Fatal("glob a matched b")
	}
}
