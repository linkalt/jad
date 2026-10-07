package sessionfs

import "testing"

// The parity golden records only whether a setting loads, so the value a name resolves
// to is checked here against what the TypeScript loader stores.
func TestExecDefaultLangResolvesToTheTableNameAndDefaultsToBash(t *testing.T) {
	cases := []struct {
		name string
		exec map[string]any
		want string
	}{
		{"absent", map[string]any{}, "bash"},
		{"null", map[string]any{"default_lang": nil}, "bash"},
		{"a name", map[string]any{"default_lang": "python"}, "python"},
		{"an alias, any case, padded", map[string]any{"default_lang": " PY "}, "python"},
		{"a configured language", map[string]any{"default_lang": "mine", "langs": map[string]any{"mine": map[string]any{"argv": []any{"bash", "-s", "--"}}}}, "mine"},
		{"a replaced built-in keeps its name", map[string]any{"langs": map[string]any{"bash": map[string]any{"argv": []any{"bash", "-s"}}}}, "bash"},
	}
	for _, tc := range cases {
		config, err := loadApprovalSetting(t, map[string]any{
			"tools": map[string]any{"native": map[string]any{"bash": map[string]any{}, "exec": tc.exec}},
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := config.NativeExec.DefaultLang; got != tc.want {
			t.Errorf("%s: DefaultLang = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestExecDefaultLangRefusesWhatIsNotInTheTable(t *testing.T) {
	for name, value := range map[string]any{"unknown": "cobol", "empty": "", "a number": float64(3)} {
		_, err := loadApprovalSetting(t, map[string]any{
			"tools": map[string]any{"native": map[string]any{"bash": map[string]any{}, "exec": map[string]any{"default_lang": value}}},
		})
		if err == nil {
			t.Errorf("%s: default_lang %v was accepted", name, value)
		}
	}
}
