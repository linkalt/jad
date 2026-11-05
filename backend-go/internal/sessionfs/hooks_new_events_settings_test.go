package sessionfs

import (
	"reflect"
	"strings"
	"testing"
)

func TestHooksLoadTheNewEventsAndKeys(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"hooks": map[string]any{
		"enabled":                 true,
		"pre_tool":                []any{map[string]any{"name": "fix", "argv": []any{"fix"}, "rewrite": true}},
		"prompt_submit":           []any{map[string]any{"name": "polish", "argv": []any{"polish"}, "rewrite": true}},
		"session_start":           []any{map[string]any{"name": "intro", "argv": []any{"intro"}, "context": true}},
		"stop":                    []any{map[string]any{"name": "verify", "argv": []any{"verify"}}},
		"notification":            []any{map[string]any{"name": "ping", "argv": []any{"ping"}, "on_failure_only": true}},
		"stop_max_continues":      5,
		"stop_message_role":       "developer",
		"shell_changes":           "scan",
		"shell_scan_max_files":    500,
		"shell_max_changed_files": 9,
		"shell_scan_skip_dirs":    []any{"vendor"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	hooks := config.Hooks
	if !hooks.PreTool[0].Rewrite || !hooks.PromptSubmit[0].Rewrite || !hooks.SessionStart[0].Context || len(hooks.Stop) != 1 || !hooks.Notification[0].OnFailureOnly {
		t.Fatalf("events = %+v", hooks)
	}
	if hooks.StopMaxContinues != 5 || hooks.StopMessageRole != "developer" || !hooks.ShellScanActive() || hooks.ShellScanMaxFiles != 500 || hooks.ShellMaxChangedFiles != 9 || !reflect.DeepEqual(hooks.ShellScanSkipDirs, []string{"vendor"}) {
		t.Fatalf("keys = %+v", hooks)
	}
	if !hooks.RunEventsActive() {
		t.Fatal("session_start, stop and notification make the run events active")
	}
}

func TestHooksRefuseBadNewKeys(t *testing.T) {
	for name, hooks := range map[string]map[string]any{
		"stop takes no blocking":        {"stop": []any{map[string]any{"name": "a", "argv": []any{"a"}, "blocking": true}}},
		"notification takes no context": {"notification": []any{map[string]any{"name": "a", "argv": []any{"a"}, "context": true}}},
		"run_start takes no rewrite":    {"run_start": []any{map[string]any{"name": "a", "argv": []any{"a"}, "rewrite": true}}},
		"post_tool takes no rewrite":    {"post_tool": []any{map[string]any{"name": "a", "argv": []any{"a"}, "rewrite": true}}},
		"rewrite must be a boolean":     {"pre_tool": []any{map[string]any{"name": "a", "argv": []any{"a"}, "rewrite": "yes"}}},
		"stop_max_continues bound":      {"stop_max_continues": 101},
		"stop_message_role value":       {"stop_message_role": "assistant"},
		"shell_changes value":           {"shell_changes": "git"},
		"shell_scan_max_files bound":    {"shell_scan_max_files": 0},
		"shell_max_changed_files bound": {"shell_max_changed_files": 10001},
		"skip dir with a slash":         {"shell_scan_skip_dirs": []any{"a/b"}},
		"skip dirs not a list":          {"shell_scan_skip_dirs": "vendor"},
	} {
		if _, err := loadSetting(t, map[string]any{"hooks": hooks}); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "hooks") {
			t.Errorf("%s: error %v does not name hooks", name, err)
		}
	}
}
