package sessionfs

import (
	"reflect"
	"strings"
	"testing"
)

func TestHooksRunEventsLoadWithDefaults(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"hooks": map[string]any{
		"enabled": true,
		"run_start": []any{
			map[string]any{"name": "prepare", "argv": []any{"./prepare.sh", "--fast"}},
		},
		"run_end": []any{
			map[string]any{"name": "notify", "argv": []any{"notify"}, "on_failure_only": true},
			map[string]any{"name": "tidy", "argv": []any{"tidy"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := defaultHooksWant()
	want.Enabled = true
	want.RunStart = []RunEventHook{{Name: "prepare", Argv: []string{"./prepare.sh", "--fast"}}}
	want.RunEnd = []RunEventHook{
		{Name: "notify", Argv: []string{"notify"}, OnFailureOnly: true},
		{Name: "tidy", Argv: []string{"tidy"}},
	}
	hooks := config.Hooks
	if !reflect.DeepEqual(hooks, want) || !hooks.RunEventsActive() || hooks.ToolEventsActive() || hooks.PostEditActive() || !hooks.Active() {
		t.Fatalf("hooks = %+v, want %+v", hooks, want)
	}
}

func TestHooksRunEventsAreInactiveWhenDisabled(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"hooks": map[string]any{
		"run_start": []any{map[string]any{"name": "a", "argv": []any{"a"}}},
	}})
	if err != nil || config.Hooks.RunEventsActive() || config.Hooks.Active() {
		t.Fatalf("err=%v runEvents=%v active=%v; a list without enabled must start nothing", err, config.Hooks.RunEventsActive(), config.Hooks.Active())
	}
}

func TestHooksRunEventsRejectBadValuesWithTheKeyInTheMessage(t *testing.T) {
	entry := func(extra map[string]any) map[string]any {
		value := map[string]any{"name": "a", "argv": []any{"a"}}
		for key, item := range extra {
			value[key] = item
		}
		return value
	}
	cases := []struct {
		name  string
		hooks map[string]any
		want  string
	}{
		{"run_start object", map[string]any{"run_start": map[string]any{}}, "hooks.run_start must be a list"},
		{"run_end null", map[string]any{"run_end": nil}, "hooks.run_end must be a list"},
		{"run_start too long", map[string]any{"run_start": make([]any, 33)}, "hooks.run_start must be a list of at most 32"},
		{"run_start entry string", map[string]any{"run_start": []any{"echo"}}, "hooks.run_start[0] must be an object"},
		{"run_start match", map[string]any{"run_start": []any{entry(map[string]any{"match": "*"})}}, "hooks.run_start[0].match is not supported"},
		{"run_end match", map[string]any{"run_end": []any{entry(map[string]any{"match": "grep"})}}, "hooks.run_end[0].match is not supported"},
		{"run_end ext", map[string]any{"run_end": []any{entry(map[string]any{"ext": []any{".go"}})}}, "hooks.run_end[0].ext is not supported"},
		{"run_start name", map[string]any{"run_start": []any{entry(map[string]any{"name": "A b"})}}, "hooks.run_start[0].name must match"},
		{"run_end argv missing", map[string]any{"run_end": []any{map[string]any{"name": "a"}}}, "hooks.run_end[0].argv must be 1 to 32"},
		{"run_end argv blank", map[string]any{"run_end": []any{entry(map[string]any{"argv": []any{"a", " "}})}}, "hooks.run_end[0].argv must be 1 to 32"},
		{"run_start placeholder", map[string]any{"run_start": []any{entry(map[string]any{"argv": []any{"a", "{file}"}})}}, "hooks.run_start[0].argv[1] has a placeholder"},
		{"run_end failure only", map[string]any{"run_end": []any{entry(map[string]any{"on_failure_only": "yes"})}}, "hooks.run_end[0].on_failure_only must be a boolean"},
		{"run_start duplicate", map[string]any{"run_start": []any{entry(nil), entry(nil)}}, `hooks.run_start[1].name "a" is used twice`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadSetting(t, map[string]any{"hooks": test.hooks})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestHooksPreCompactLoadsWithDefaults(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"hooks": map[string]any{
		"enabled":     true,
		"pre_compact": []any{map[string]any{"name": "snapshot", "argv": []any{"./snap.sh"}, "on_failure_only": true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := defaultHooksWant()
	want.Enabled = true
	want.PreCompact = []RunEventHook{{Name: "snapshot", Argv: []string{"./snap.sh"}, OnFailureOnly: true}}
	if !reflect.DeepEqual(config.Hooks, want) || !config.Hooks.RunEventsActive() || config.Hooks.ToolEventsActive() {
		t.Fatalf("hooks = %+v, want %+v", config.Hooks, want)
	}
}

func TestHooksPreCompactRejectsBadValues(t *testing.T) {
	entry := func(extra map[string]any) map[string]any {
		value := map[string]any{"name": "a", "argv": []any{"a"}}
		for key, item := range extra {
			value[key] = item
		}
		return value
	}
	cases := []struct {
		name  string
		hooks map[string]any
		want  string
	}{
		{"object", map[string]any{"pre_compact": map[string]any{}}, "hooks.pre_compact must be a list"},
		{"too long", map[string]any{"pre_compact": make([]any, 33)}, "hooks.pre_compact must be a list of at most 32"},
		{"match", map[string]any{"pre_compact": []any{entry(map[string]any{"match": "*"})}}, "hooks.pre_compact[0].match is not supported"},
		{"blocking not a boolean", map[string]any{"pre_compact": []any{entry(map[string]any{"blocking": "yes"})}}, "hooks.pre_compact[0].blocking must be a boolean"},
		{"context", map[string]any{"pre_compact": []any{entry(map[string]any{"context": true})}}, "hooks.pre_compact[0].context is not supported"},
		{"duplicate", map[string]any{"pre_compact": []any{entry(nil), entry(nil)}}, `hooks.pre_compact[1].name "a" is used twice`},
		{"run_start blocking", map[string]any{"run_start": []any{entry(map[string]any{"blocking": true})}}, "hooks.run_start[0].blocking is not supported"},
		{"post_tool blocking", map[string]any{"post_tool": []any{entry(map[string]any{"blocking": true})}}, "hooks.post_tool[0].blocking is not supported"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadSetting(t, map[string]any{"hooks": test.hooks})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestHooksPreToolBlockingLoads(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"hooks": map[string]any{
		"enabled": true,
		"pre_tool": []any{
			map[string]any{"name": "gate", "match": "write", "argv": []any{"gate"}, "blocking": true},
			map[string]any{"name": "note", "argv": []any{"note"}, "blocking": false},
			map[string]any{"name": "plain", "argv": []any{"plain"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := config.Hooks.PreTool
	if len(got) != 3 || !got[0].Blocking || got[1].Blocking || got[2].Blocking {
		t.Fatalf("pre_tool = %+v; only the first entry blocks", got)
	}
	_, err = loadSetting(t, map[string]any{"hooks": map[string]any{"pre_tool": []any{map[string]any{"name": "a", "argv": []any{"a"}, "blocking": "yes"}}}})
	if err == nil || !strings.Contains(err.Error(), "hooks.pre_tool[0].blocking must be a boolean") {
		t.Fatalf("error = %v", err)
	}
	_, err = loadSetting(t, map[string]any{"hooks": map[string]any{"pre_tool": []any{map[string]any{"name": "a", "argv": []any{"a"}, "blocking": nil}}}})
	if err == nil || !strings.Contains(err.Error(), "hooks.pre_tool[0].blocking must be a boolean") {
		t.Fatalf("null blocking: error = %v", err)
	}
}

func TestHooksPreCompactBlockingLoads(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"hooks": map[string]any{
		"enabled":     true,
		"pre_compact": []any{map[string]any{"name": "keep", "argv": []any{"keep"}, "blocking": true}, map[string]any{"name": "log", "argv": []any{"log"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []RunEventHook{{Name: "keep", Argv: []string{"keep"}, Blocking: true}, {Name: "log", Argv: []string{"log"}}}
	if !reflect.DeepEqual(config.Hooks.PreCompact, want) {
		t.Fatalf("pre_compact = %+v, want %+v", config.Hooks.PreCompact, want)
	}
}

func TestHooksPromptSubmitAndSubagentStopLoad(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"hooks": map[string]any{
		"enabled":             true,
		"prompt_context_role": "system",
		"prompt_submit": []any{
			map[string]any{"name": "gate", "argv": []any{"gate"}, "blocking": true},
			map[string]any{"name": "notes", "argv": []any{"notes"}, "context": true, "on_failure_only": true},
		},
		"subagent_stop": []any{map[string]any{"name": "done", "argv": []any{"done"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := defaultHooksWant()
	want.Enabled = true
	want.PromptContextRole = "system"
	want.PromptSubmit = []RunEventHook{
		{Name: "gate", Argv: []string{"gate"}, Blocking: true},
		{Name: "notes", Argv: []string{"notes"}, Context: true, OnFailureOnly: true},
	}
	want.SubagentStop = []RunEventHook{{Name: "done", Argv: []string{"done"}}}
	if !reflect.DeepEqual(config.Hooks, want) || !config.Hooks.RunEventsActive() || config.Hooks.ToolEventsActive() {
		t.Fatalf("hooks = %+v, want %+v", config.Hooks, want)
	}
	for _, only := range []map[string]any{
		{"enabled": true, "prompt_submit": []any{map[string]any{"name": "a", "argv": []any{"a"}}}},
		{"enabled": true, "subagent_stop": []any{map[string]any{"name": "a", "argv": []any{"a"}}}},
	} {
		config, err := loadSetting(t, map[string]any{"hooks": only})
		if err != nil || !config.Hooks.RunEventsActive() {
			t.Fatalf("hooks %v: err=%v active=%v", only, err, config.Hooks.RunEventsActive())
		}
	}
}

func TestHooksPromptSubmitAndSubagentStopRejectBadValues(t *testing.T) {
	entry := func(extra map[string]any) map[string]any {
		value := map[string]any{"name": "a", "argv": []any{"a"}}
		for key, item := range extra {
			value[key] = item
		}
		return value
	}
	cases := []struct {
		name  string
		hooks map[string]any
		want  string
	}{
		{"prompt_submit object", map[string]any{"prompt_submit": map[string]any{}}, "hooks.prompt_submit must be a list"},
		{"prompt_submit match", map[string]any{"prompt_submit": []any{entry(map[string]any{"match": "*"})}}, "hooks.prompt_submit[0].match is not supported"},
		{"prompt_submit context", map[string]any{"prompt_submit": []any{entry(map[string]any{"context": "yes"})}}, "hooks.prompt_submit[0].context must be a boolean"},
		{"prompt_submit context null", map[string]any{"prompt_submit": []any{entry(map[string]any{"context": nil})}}, "hooks.prompt_submit[0].context must be a boolean"},
		{"prompt_submit duplicate", map[string]any{"prompt_submit": []any{entry(nil), entry(nil)}}, `hooks.prompt_submit[1].name "a" is used twice`},
		{"subagent_stop too long", map[string]any{"subagent_stop": make([]any, 33)}, "hooks.subagent_stop must be a list of at most 32"},
		{"subagent_stop blocking", map[string]any{"subagent_stop": []any{entry(map[string]any{"blocking": true})}}, "hooks.subagent_stop[0].blocking is not supported"},
		{"subagent_stop context", map[string]any{"subagent_stop": []any{entry(map[string]any{"context": true})}}, "hooks.subagent_stop[0].context is not supported"},
		{"pre_tool context", map[string]any{"pre_tool": []any{entry(map[string]any{"context": true})}}, "hooks.pre_tool[0].context is not supported"},
		{"run_end context", map[string]any{"run_end": []any{entry(map[string]any{"context": true})}}, "hooks.run_end[0].context is not supported"},
		{"context role", map[string]any{"prompt_context_role": "user"}, `hooks.prompt_context_role must be "developer" or "system"`},
		{"context role null", map[string]any{"prompt_context_role": nil}, `hooks.prompt_context_role must be "developer" or "system"`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadSetting(t, map[string]any{"hooks": test.hooks})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
