package sessionfs

import (
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

// A profile reaches another workspace when both are listed in global.json (design §30); there
// is no consent of the target beyond that. The answer for a refusal is the one of a session that
// does not exist.
func TestResolveMessageTargetNeedsBothWorkspacesListed(t *testing.T) {
	for name, test := range map[string]struct {
		listA, listB bool
		reference    string
		want         string // "" means refused
		foreign      bool
	}{
		"listed both":               {true, true, "b:s1", "b:s1", true},
		"a session only b has":      {true, true, "b:only-b", "b:only-b", true},
		"caller not listed":         {false, true, "b:s1", "", false},
		"target not listed":         {true, false, "b:s1", "", false},
		"missing session":           {true, true, "b:nope", "", false},
		"missing workspace":         {true, true, "c:s1", "", false},
		"own workspace, qualified":  {true, true, "a:s1", "a:s1", false},
		"own workspace, bare":       {true, true, "s1", "a:s1", false},
		"bare id of b only":         {true, true, "only-b", "", false},
		"bare id held by both is a": {true, true, "s1", "a:s1", false},
		"own workspace unlisted":    {false, true, "s1", "a:s1", false},
	} {
		t.Run(name, func(t *testing.T) {
			_, config := twoMessageWorkspaces(t, test.listA, test.listB, `{}`, `{}`)
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

// Both directions work once both are listed; a caller that does not resolve reaches nothing.
func TestResolveMessageTargetIsSymmetricBetweenListedWorkspaces(t *testing.T) {
	_, config := twoMessageWorkspaces(t, true, true, `{}`, `{}`)
	for _, pair := range [][2]string{{"a:s1", "b:s1"}, {"b:s1", "a:s1"}} {
		if _, foreign, err := ResolveMessageTarget(config.Roots, pair[0], pair[1]); err != nil || !foreign {
			t.Fatalf("%s to %s: foreign %v, %v", pair[0], pair[1], foreign, err)
		}
	}
	if _, _, err := ResolveMessageTarget(config.Roots, "zzz:s1", "b:s1"); err == nil {
		t.Fatal("an unknown caller reached b")
	}
}

// accept_from was the target's consent in the first cross-workspace design; it is refused by
// name, with what to do instead, so a file that still has it never silently stops protecting.
func TestWorkspaceJSONProfileMessageAcceptFromIsRemoved(t *testing.T) {
	for name, text := range map[string]string{
		"list":  `{"profile_message":{"accept_from":["a"]}}`,
		"empty": `{"profile_message":{"accept_from":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newWSFixture(t)
			a := f.workspace(t, "a", text)
			_, err := f.load(a)
			if err == nil || !strings.Contains(err.Error(), "profile_message.accept_from was removed") || !strings.Contains(err.Error(), "delete the key") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	for name, text := range map[string]string{
		"not an object": `{"profile_message":[]}`,
		"unknown key":   `{"profile_message":{"accept":["a"]}}`,
		"null section":  `{"profile_message":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newWSFixture(t)
			a := f.workspace(t, "a", text)
			wsMustRefuse(t, f, a, "profile_message")
		})
	}
}

// Only a listed workspace is reachable and reaches others: the layout says so and names the
// listed workspaces the tool offers.
func TestListedWorkspacesTakePartInProfileMessage(t *testing.T) {
	_, config := twoMessageWorkspaces(t, true, true, `{}`, `{}`)
	for _, root := range config.Roots {
		if !root.Layout.Listed || len(root.Layout.MessageWorkspaces) != 2 {
			t.Fatalf("%s: listed %v workspaces %q", root.ID, root.Layout.Listed, root.Layout.MessageWorkspaces)
		}
	}
	// Unlisted start workspace: not Listed, so it takes part in no cross-workspace message.
	g := newWSFixture(t)
	b := g.workspace(t, "b", `{}`)
	config = wsMustLoad(t, g, b)
	if layout := config.Roots[0].Layout; layout.Listed || len(layout.MessageWorkspaces) != 0 {
		t.Fatalf("an unlisted start workspace counts as listed: %v %q", layout.Listed, layout.MessageWorkspaces)
	}
}
