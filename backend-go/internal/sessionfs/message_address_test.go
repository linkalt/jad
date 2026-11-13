package sessionfs

import (
	"errors"
	"strings"
	"testing"
)

// The caller of every case: workspace app, session release, profile dev/coder.
var addressCaller = MessageCaller{Workspace: "app", Session: "release", Profile: "dev/coder"}

func TestParseMessageRefFiveForms(t *testing.T) {
	for reference, want := range map[string]string{
		"reviewer":                            "local:app/release/dev/reviewer",
		"review/security":                     "local:app/release/review/security",
		"nightly/review/security":             "local:app/nightly/review/security",
		"local:reports/daily/review/auditor":  "local:reports/daily/review/auditor",
		"local:app/release/dev/coder":         "local:app/release/dev/coder",
		"local:remote/main/dev/x":             "local:remote/main/dev/x",
		"local:local/local/local/local":       "local:local/local/local/local",
		"local/dev/x":                         "local:app/local/dev/x",
		"remote/dev/x":                        "local:app/remote/dev/x",
		"a.b_c-d/e.f/g-h":                     "local:app/a.b_c-d/e.f/g-h",
		"local:w.s-1/s_2.x/cat.1/n-1":         "local:w.s-1/s_2.x/cat.1/n-1",
		"release/dev/coder":                   "local:app/release/dev/coder",
		"dev/coder":                           "local:app/release/dev/coder",
		"coder":                               "local:app/release/dev/coder",
		"local:reports/daily/dev/Reviewer.v2": "local:reports/daily/dev/Reviewer.v2",
	} {
		ref, err := ParseMessageRef(reference, addressCaller)
		if err != nil {
			t.Errorf("%q: %v", reference, err)
			continue
		}
		if ref.Canonical() != want {
			t.Errorf("%q = %s, want %s", reference, ref.Canonical(), want)
		}
	}
}

// Wrong segment counts, stray colons, traversal and empty segments are all the same error: no
// form is ever guessed, searched or defaulted.
func TestParseMessageRefRefusesEverythingElse(t *testing.T) {
	for _, reference := range []string{
		"",
		"local:reports/daily/auditor",
		"local:reports/daily/review/auditor/extra",
		"local:reports",
		"local:",
		"local:/s/c/n",
		"remote:box/main/review/reviewer",
		"remote:box-proj/review/reviewer",
		"reports:daily/review/auditor",
		"local:reports:daily/review/auditor",
		"local:local:reports/daily/review/auditor",
		"a:b/c/d",
		"a/b/c/d",
		"a/b/c/d/e",
		"../review/reviewer",
		"./review/reviewer",
		"review/..",
		"release/../coder",
		"local:../s/c/n",
		"local:w/./c/n",
		"local:w/s/c/..",
		"a//b",
		"/a/b",
		"a/b/",
		".hidden",
		"dev/.hidden",
		"-lead/x",
		"a b/c",
		"a*/b",
		"a?/b",
		"a[b]/c",
		"a\\b/c",
		"dev/coder\n",
		" dev/coder",
		"LOCAL:w/s/c/n",
		"local:w/s/c/n ",
		"local:" + strings.Repeat("w", 65) + "/s/c/n",
		"local:w/" + strings.Repeat("s", 129) + "/c/n",
	} {
		if ref, err := ParseMessageRef(reference, addressCaller); !errors.Is(err, ErrInvalidMessageRef) {
			t.Errorf("%q = %v, %v; want ErrInvalidMessageRef", reference, ref, err)
		}
	}
	// A caller without a category cannot write the one-segment form: it never guesses one.
	if _, err := ParseMessageRef("coder", MessageCaller{Workspace: "app", Session: "release"}); !errors.Is(err, ErrInvalidMessageRef) {
		t.Errorf("no caller category: %v", err)
	}
}

func TestMessageRefOwnAndReference(t *testing.T) {
	for reference, test := range map[string]struct {
		own  bool
		want string
	}{
		"coder":                          {true, "release"},
		"dev/reviewer":                   {true, "release"},
		"release/dev/reviewer":           {true, "release"},
		"local:app/release/dev/reviewer": {true, "release"},
		"nightly/dev/reviewer":           {false, "nightly"},
		"local:app/nightly/dev/reviewer": {false, "nightly"},
		"local:reports/release/dev/x":    {false, "reports:release"},
		"local:reports/daily/dev/x":      {false, "reports:daily"},
	} {
		ref, err := ParseMessageRef(reference, addressCaller)
		if err != nil {
			t.Fatal(err)
		}
		if ref.Own(addressCaller) != test.own || ref.Reference(addressCaller) != test.want {
			t.Errorf("%q: own %v reference %q, want %v %q", reference, ref.Own(addressCaller), ref.Reference(addressCaller), test.own, test.want)
		}
	}
}
