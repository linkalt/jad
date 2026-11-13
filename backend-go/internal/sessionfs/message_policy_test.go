package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func rules(t *testing.T, text string) []MessageRule {
	t.Helper()
	parsed, err := parseMessageRules(json.RawMessage(text), "test: deny", true, true, "")
	if err != nil {
		t.Fatal(text, err)
	}
	return parsed
}

func TestParseMessageRulesAcceptsTheGrammar(t *testing.T) {
	parsed := rules(t, `[
		{"from":"local:scratch/*/*/*"},
		{"to":"local:*/*/ops/*","actions":["reset","compact"]},
		{"to":"remote:box-*/*/review/*"},
		{"from":"local:app/?/dev/*","to":"local:reports/daily/review/a.b_c-d","actions":["message","model_choice"]},
		{}
	]`)
	if len(parsed) != 5 || parsed[0].From != "local:scratch/*/*/*" || parsed[1].Actions[1] != "compact" || parsed[2].To != "remote:box-*/*/review/*" || parsed[4].From != "" || parsed[4].To != "" || parsed[4].Actions != nil {
		t.Fatalf("rules = %+v", parsed)
	}
}

func TestParseMessageRulesRefusesWhatIsNotARule(t *testing.T) {
	for name, text := range map[string]string{
		"not an array":         `{"to":"local:a/*/*/*"}`,
		"entry not an object":  `["local:a/*/*/*"]`,
		"null entry":           `[null]`,
		"unknown key":          `[{"target":"local:a/*/*/*"}]`,
		"no prefix":            `[{"to":"a/*/*/*"}]`,
		"three segments":       `[{"to":"local:a/*/*"}]`,
		"five segments":        `[{"to":"local:a/*/*/*/*"}]`,
		"empty segment":        `[{"to":"local:a//x/*"}]`,
		"class":                `[{"to":"local:a/[ab]/x/*"}]`,
		"short form":           `[{"to":"review/*"}]`,
		"colon in a segment":   `[{"to":"local:a:b/*/*/*"}]`,
		"remote in from":       `[{"from":"remote:box/*/*/*"}]`,
		"from not a string":    `[{"from":1}]`,
		"empty actions":        `[{"actions":[]}]`,
		"unknown action":       `[{"actions":["delete"]}]`,
		"duplicate action":     `[{"actions":["reset","reset"]}]`,
		"actions not strings":  `[{"actions":[1]}]`,
		"duplicate rule":       `[{"to":"local:a/*/*/*"},{"to":"local:a/*/*/*"}]`,
		"duplicate empty rule": `[{},{}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if parsed, err := parseMessageRules(json.RawMessage(text), "f.json: deny", true, true, ""); err == nil {
				t.Fatalf("accepted %s: %+v", text, parsed)
			} else if !strings.Contains(err.Error(), "f.json: deny") {
				t.Fatalf("error does not name the file and key: %v", err)
			}
		})
	}
	// A toolset's rules have no from; an allow_reset rule has no actions and names its own workspace.
	for name, call := range map[string]func() error{
		"from in a toolset": func() error {
			_, err := parseMessageRules(json.RawMessage(`[{"from":"local:a/*/*/*"}]`), "t: deny", false, true, "")
			return err
		},
		"actions in allow_reset": func() error {
			_, err := parseMessageRules(json.RawMessage(`[{"actions":["reset"]}]`), "t: allow_reset", true, false, "w")
			return err
		},
		"allow_reset to another workspace": func() error {
			_, err := parseMessageRules(json.RawMessage(`[{"to":"local:other/*/*/*"}]`), "t: allow_reset", true, false, "w")
			return err
		},
		"allow_reset to a glob of workspaces": func() error {
			_, err := parseMessageRules(json.RawMessage(`[{"to":"local:*/*/*/*"}]`), "t: allow_reset", true, false, "w")
			return err
		},
		"allow_reset to remote": func() error {
			_, err := parseMessageRules(json.RawMessage(`[{"to":"remote:box/*/*/*"}]`), "t: allow_reset", true, false, "w")
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseMessageRules(json.RawMessage(`[{"from":"local:a/*/dev/*","to":"local:w/*/review/*"},{}]`), "t: allow_reset", true, false, "w"); err != nil {
		t.Errorf("a valid allow_reset: %v", err)
	}
}

const (
	callerA = "local:app/release/dev/coder"
	targetA = "local:app/release/review/security"
	targetB = "local:reports/daily/review/auditor"
)

func TestMessagePolicyCheck(t *testing.T) {
	message := []string{MessageActionMessage}
	reset := []string{MessageActionMessage, MessageActionReset}
	for name, test := range map[string]struct {
		policy  MessagePolicy
		to      string
		actions []string
		want    string // by/action, empty = allowed
	}{
		"nothing": {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message, ""},
		"global from": {MessagePolicy{Global: rules(t, `[{"from":"local:app/*/*/*"}]`), CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message,
			"global/message"},
		"caller workspace to": {MessagePolicy{Caller: rules(t, `[{"to":"local:reports/*/*/*"}]`), CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message,
			"workspace:app/message"},
		"target workspace from": {MessagePolicy{Target: rules(t, `[{"from":"local:app/*/*/*"}]`), CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message,
			"workspace:reports/message"},
		"an empty rule closes the workspace both ways": {MessagePolicy{Target: rules(t, `[{}]`), CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message,
			"workspace:reports/message"},
		"a rule of another pattern":                         {MessagePolicy{Global: rules(t, `[{"from":"local:scratch/*/*/*"},{"to":"local:*/*/ops/*"}]`), CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message, ""},
		"star does not cross a slash":                       {MessagePolicy{Global: rules(t, `[{"to":"local:*/review/*/*"}]`), CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message, ""},
		"a reset rule leaves the message":                   {MessagePolicy{Global: rules(t, `[{"actions":["reset"]}]`), CallerWorkspace: "app", TargetWorkspace: "app"}, targetA, message, ""},
		"a reset rule refuses the reset":                    {MessagePolicy{Global: rules(t, `[{"actions":["reset"]}]`), CallerWorkspace: "app", TargetWorkspace: "app"}, targetA, reset, "global/reset"},
		"message implies reset":                             {MessagePolicy{Global: rules(t, `[{"to":"local:app/*/*/*"}]`), CallerWorkspace: "app", TargetWorkspace: "app"}, targetA, reset, "global/message"},
		"compact rule":                                      {MessagePolicy{Caller: rules(t, `[{"actions":["compact"]}]`), CallerWorkspace: "app", TargetWorkspace: "app"}, targetA, []string{"message", "compact"}, "workspace:app/compact"},
		"model_choice rule":                                 {MessagePolicy{Caller: rules(t, `[{"actions":["model_choice"]}]`), CallerWorkspace: "app", TargetWorkspace: "app"}, targetA, []string{"message", "model_choice"}, "workspace:app/model_choice"},
		"a model_choice rule leaves a call without a model": {MessagePolicy{Caller: rules(t, `[{"actions":["model_choice"]}]`), CallerWorkspace: "app", TargetWorkspace: "app"}, targetA, message, ""},
		"reset inside one workspace needs no allow_reset":   {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "app"}, targetA, reset, ""},
		"reset across workspaces is refused by default":     {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, reset, "workspace:reports/reset"},
		"allow_reset matches":                               {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "reports", AllowReset: rules(t, `[{"from":"local:app/*/dev/*","to":"local:reports/*/review/*"}]`)}, targetB, reset, ""},
		"allow_reset from is another":                       {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "reports", AllowReset: rules(t, `[{"from":"local:app/*/ops/*"}]`)}, targetB, reset, "workspace:reports/reset"},
		"allow_reset to is another":                         {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "reports", AllowReset: rules(t, `[{"to":"local:reports/*/admin/*"}]`)}, targetB, reset, "workspace:reports/reset"},
		"allow_reset with no keys allows any":               {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "reports", AllowReset: rules(t, `[{}]`)}, targetB, reset, ""},
		"allow_reset is not a message allowance": {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "reports", AllowReset: rules(t, `[{}]`), Target: rules(t, `[{"from":"local:app/*/*/*"}]`)}, targetB, reset,
			"workspace:reports/message"},
		"a deny beats allow_reset": {MessagePolicy{CallerWorkspace: "app", TargetWorkspace: "reports", AllowReset: rules(t, `[{}]`), Target: rules(t, `[{"actions":["reset"]}]`)}, targetB, reset,
			"workspace:reports/reset"},
		"attribution: global before workspaces": {MessagePolicy{Global: rules(t, `[{}]`), Caller: rules(t, `[{}]`), Target: rules(t, `[{}]`), CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message, "global/message"},
		"attribution: caller before target": {MessagePolicy{Caller: rules(t, `[{}]`), Target: rules(t, `[{}]`), CallerWorkspace: "app", TargetWorkspace: "reports"}, targetB, message,
			"workspace:app/message"},
		"a call without a local caller is matched only by rules without from": {MessagePolicy{Target: rules(t, `[{"from":"local:*/*/*/*"}]`), CallerWorkspace: "", TargetWorkspace: "reports"}, targetB, message, ""},
	} {
		t.Run(name, func(t *testing.T) {
			from := callerA
			if strings.Contains(name, "without a local caller") {
				from = ""
			}
			refusal := test.policy.Check(from, test.to, test.actions)
			got := ""
			if refusal != nil {
				got = refusal.By + "/" + refusal.Action
			}
			if got != test.want {
				t.Fatalf("refusal = %q, want %q", got, test.want)
			}
		})
	}
}

// The rules of workspace.json and global.json are read at start into the layouts; a bad rule
// refuses the start of the workspace it is in, naming the file and key.
func TestWorkspaceAndGlobalMessageRulesLoad(t *testing.T) {
	f, _ := twoMessageWorkspaces(t, true, true,
		`{"profile_message":{"deny":[{"from":"local:b/*/*/*"}],"allow_reset":[{"from":"local:b/*/dev/*","to":"local:a/*/dev/*"}]}}`, `{}`)
	a := filepath.Join(f.base, "a")
	b := filepath.Join(f.base, "b")
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}],"profile_message":{"deny":[{"to":"local:*/*/ops/*","actions":["reset"]}]}}`)
	config := wsMustLoad(t, f, a)
	for _, root := range config.Roots {
		if len(root.Layout.GlobalMessageDeny) != 1 || root.Layout.GlobalMessageDeny[0].Actions[0] != "reset" {
			t.Errorf("%s: global rules = %+v", root.ID, root.Layout.GlobalMessageDeny)
		}
	}
	layoutA, layoutB := config.Roots[0].Layout, config.Roots[1].Layout
	if len(layoutA.MessageDeny) != 1 || len(layoutA.MessageAllowReset) != 1 || len(layoutB.MessageDeny) != 0 || len(layoutB.MessageAllowReset) != 0 {
		t.Errorf("workspace rules: a %+v %+v, b %+v %+v", layoutA.MessageDeny, layoutA.MessageAllowReset, layoutB.MessageDeny, layoutB.MessageAllowReset)
	}
	policy := PolicyOf(layoutA, layoutB, "a", "b")
	if refusal := policy.Check("local:a/s1/dev/coder", "local:b/s1/dev/coder", []string{"message"}); refusal != nil {
		t.Errorf("a's own rule (from b) refused a call from a: %+v", refusal)
	}
	if refusal := PolicyOf(layoutB, layoutA, "b", "a").Check("local:b/s1/dev/coder", "local:a/s1/dev/coder", []string{"message"}); refusal == nil || refusal.By != "workspace:a" {
		t.Errorf("a's rule did not refuse a call from b into a: %+v", refusal)
	}
	if refusal := PolicyOf(layoutB, layoutA, "b", "a").Check("local:b/s1/dev/coder", "local:a/s1/dev/coder", []string{"message", "reset"}); refusal == nil {
		t.Error("reset allowed although the deny refuses the whole call")
	}
}

func TestWorkspaceAndGlobalMessageRuleErrors(t *testing.T) {
	for name, test := range map[string]struct {
		workspace, global, want string
	}{
		"accept_from":              {`{"profile_message":{"accept_from":["a"]}}`, ``, "accept_from was removed"},
		"unknown key":              {`{"profile_message":{"allow":[]}}`, ``, "profile_message.allow is not a key"},
		"bad rule":                 {`{"profile_message":{"deny":[{"to":"x"}]}}`, ``, "workspace.json: profile_message.deny[0]: to must be a canonical address pattern"},
		"allow_reset to another":   {`{"profile_message":{"allow_reset":[{"to":"local:b/*/*/*"}]}}`, ``, "allow_reset of workspace a"},
		"allow_reset with actions": {`{"profile_message":{"allow_reset":[{"actions":["reset"]}]}}`, ``, "allow_reset"},
		"global allow_reset":       {`{}`, `"profile_message":{"allow_reset":[{}]}`, "belongs in the workspace.json"},
		"global accept_from":       {`{}`, `"profile_message":{"accept_from":["a"]}`, "accept_from was removed"},
		"global bad rule":          {`{}`, `"profile_message":{"deny":[{"actions":["x"]}]}`, "global.json: profile_message.deny[0]: action"},
		"global not an object":     {`{}`, `"profile_message":[]`, "profile_message must be an object"},
		"global duplicate":         {`{}`, `"profile_message":{"deny":[{},{}]}`, "repeats an earlier rule"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWSFixture(t)
			a := f.workspace(t, "a", test.workspace)
			if test.global != "" {
				f.globalJSON(t, `{`+test.global+`}`)
			}
			_, err := f.load(a)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

// The toolset's own deny and the removed deny_profiles: the layouts read different keys.
func TestProfileMessageDenyKeysByLayout(t *testing.T) {
	v2 := MessageCaller{Workspace: "app", Session: "release", Profile: "dev/coder"}
	settings, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":{"deny":[{"to":"local:*/*/ops/*"},{"actions":["reset"]}]}}`), "t.json", v2)
	if err != nil || len(settings.Deny) != 2 {
		t.Fatalf("v2 deny: %+v, %v", settings, err)
	}
	for name, test := range map[string]struct {
		native string
		caller MessageCaller
		want   string
	}{
		"v2 deny_profiles":     {`{"profile_message":{"deny_profiles":["ops/root"]}}`, v2, `deny_profiles was removed in the workspace layout; replace each entry <category>/<profile> with a deny rule {"to": "local:*/*/<category>/<profile>"}`},
		"v2 from in a toolset": {`{"profile_message":{"deny":[{"from":"local:a/*/*/*"}]}}`, v2, "from is not a key of this rule; a toolset's rules are about the profiles that use it"},
		"v2 bad rule":          {`{"profile_message":{"deny":[{"to":"ops/root"}]}}`, v2, "t.json: tools.native.profile_message.deny[0]: to must be"},
		"v1 deny":              {`{"profile_message":{"deny":[{}]}}`, MessageCaller{}, "deny is a key of the workspace layout"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(test.native), "t.json", test.caller); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
	// v1 reads deny_profiles as it always did.
	v1, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":{"deny_profiles":["ops/root"]}}`), "t.json", MessageCaller{})
	if err != nil || len(v1.DenyProfiles) != 1 {
		t.Fatalf("v1 deny_profiles: %+v, %v", v1, err)
	}
}

func TestShownTargetsLeavesOutWhatIsRefusedAsAWhole(t *testing.T) {
	settings := NativeProfileMessageSettings{
		Caller:        MessageCaller{Workspace: "app", Session: "release", Profile: "dev/coder"},
		Deny:          rules(t, `[{"to":"local:*/*/ops/*"}]`),
		GlobalDeny:    rules(t, `[{"from":"local:app/*/dev/*","to":"local:*/*/admin/*"}]`),
		WorkspaceDeny: rules(t, `[{"to":"local:app/*/review/security","actions":["reset"]},{"to":"local:app/release/review/secret"}]`),
	}
	got := settings.ShownTargets([]string{"ops/root", "admin/x", "review/security", "review/secret", "review/ok"})
	if strings.Join(got, ",") != "review/security,review/ok" {
		t.Fatalf("shown = %q (a reset-only rule must not hide a profile)", got)
	}
	if v1 := (NativeProfileMessageSettings{}).ShownTargets([]string{"a/b", "c/d"}); len(v1) != 2 {
		t.Fatalf("v1 shown = %q", v1)
	}
}
