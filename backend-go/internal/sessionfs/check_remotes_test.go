package sessionfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkKeys(t *testing.T, k keysFixture) CheckReport {
	t.Helper()
	config, err := k.f.load(k.w)
	if err != nil {
		t.Fatal(err)
	}
	report, err := CheckLayout(config.Roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func problemText(report CheckReport) string {
	var all []string
	for _, problem := range report.Problems {
		all = append(all, problem.Kind+" "+problem.ID+": "+problem.Message)
	}
	return strings.Join(all, "\n")
}

// jad check reads the bindings and the key files of the workspace layout: a binding that does not
// parse, a key file that is open or names a binding that is not valid, and a session whose
// remote_keys cannot be used are problems; none of them repeats the secret.
func TestCheckReportsBrokenBindingsAndKeys(t *testing.T) {
	k := newKeysFixture(t, `{"remote_keys":["ka"]}`, "", "")
	if report := checkKeys(t, k); len(report.Problems) != 0 {
		t.Fatalf("a valid layout: %s", problemText(report))
	}
	wsPut(t, filepath.Join(k.f.global, "remotes", "boxb.json"), `{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj","token_env":"JAD_REMOTE_X"}}`)
	_ = os.Chmod(filepath.Join(k.f.global, "auth", "keys", "ka.json"), 0o644)
	wsPut(t, filepath.Join(k.f.global, "auth", "keys", "orphan.json"), `{"kind":"remote_key","binding":"gone","token":"`+keySecret+`"}`)
	report := checkKeys(t, k)
	text := problemText(report)
	for _, want := range []string{
		"remote.token_env was removed in the workspace layout",
		"key ka: the key file must not be readable by group or others",
		"key orphan is for binding gone, which has no valid file",
		"remote_keys",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, keySecret) {
		t.Fatalf("the secret is in the report:\n%s", text)
	}
}

// A binding a tool lists but no key reaches is a note, not a problem: the tool leaves it out.
func TestCheckNotesAListedBindingWithoutAKey(t *testing.T) {
	k := newKeysFixture(t, `{"remote_keys":["ka"]}`, "", "")
	wsPut(t, filepath.Join(k.f.global, "tools", "coding", "x.json"), `{"native":{"profile_message":{"remotes":["boxa","boxb"]}},"mcp":{}}`)
	wsPut(t, filepath.Join(k.w, ".jad", "profiles", "dev", "coder.json"), `{`+profileBase+`,"toolset":"coding/x"}`)
	report := checkKeys(t, k)
	if len(report.Problems) != 0 {
		t.Fatalf("problems: %s", problemText(report))
	}
	if len(report.Notes) != 1 || !strings.Contains(report.Notes[0], "lists binding boxb, but no key in remote_keys reaches it") || strings.Contains(strings.Join(report.Notes, " "), "boxa,") {
		t.Fatalf("notes: %q", report.Notes)
	}
}
