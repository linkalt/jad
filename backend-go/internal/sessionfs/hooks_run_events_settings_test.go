package sessionfs

import (
	"reflect"
	"strings"
	"testing"
)

func TestHooksRunEventsLoadWithDefaults(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"hooks": map[string]any{
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
	config, err := loadApprovalSetting(t, map[string]any{"hooks": map[string]any{
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
			_, err := loadApprovalSetting(t, map[string]any{"hooks": test.hooks})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
