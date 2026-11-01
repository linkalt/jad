package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolTimeoutSettingsAreCappedAtSixHours(t *testing.T) {
	if maxToolTimeoutMs != 6*60*60*1000 {
		t.Fatalf("maxToolTimeoutMs = %d, want 21600000", maxToolTimeoutMs)
	}
	dir := t.TempDir()
	settingPath := filepath.Join(dir, "setting.json")
	bashStyle := func(section string) func(string) error {
		return func(body string) error {
			setting := map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"` + section + `":` + body + `}}`)}
			var err error
			switch section {
			case "bash":
				_, err = normalizeNativeBashSettings(setting, dir, settingPath, dir, dir, "", dir, NativePathPolicy{}, NativePathPolicy{})
			case "transcripts":
				_, err = normalizeNativeTranscriptsSettings(setting, settingPath, NativePathPolicy{})
			case "transcript":
				_, err = normalizeNativeTranscriptSettings(setting, settingPath, NativePathPolicy{})
			case "exec":
				_, err = normalizeNativeExecSettings(setting, dir, settingPath, dir, dir, "", dir, NativePathPolicy{}, NativePathPolicy{})
			default:
				_, err = normalizeNativeToolSmithSettings(setting, dir, settingPath, dir, dir, "", dir, NativePathPolicy{}, NativePathPolicy{})
			}
			return err
		}
	}
	sixHours, over := "21600000", "21600001"
	cases := []struct {
		name   string
		run    func(string) error
		ok     string
		tooBig string
		key    string
	}{
		{"bash timeout_ms", bashStyle("bash"), `{"timeout_ms":` + sixHours + `,"max_timeout_ms":` + sixHours + `}`, `{"timeout_ms":` + over + `}`, "timeout_ms"},
		{"bash max_timeout_ms", bashStyle("bash"), `{"max_timeout_ms":` + sixHours + `}`, `{"max_timeout_ms":` + over + `}`, "max_timeout_ms"},
		{"exec timeout_ms", bashStyle("exec"), `{"timeout_ms":` + sixHours + `,"max_timeout_ms":` + sixHours + `}`, `{"timeout_ms":` + over + `}`, "timeout_ms"},
		{"exec max_timeout_ms", bashStyle("exec"), `{"max_timeout_ms":` + sixHours + `}`, `{"max_timeout_ms":` + over + `}`, "max_timeout_ms"},
		{"tool_smith timeout_ms", bashStyle("tool_smith"), `{"timeout_ms":` + sixHours + `,"max_timeout_ms":` + sixHours + `}`, `{"timeout_ms":` + over + `}`, "timeout_ms"},
		{"tool_smith max_timeout_ms", bashStyle("tool_smith"), `{"max_timeout_ms":` + sixHours + `}`, `{"max_timeout_ms":` + over + `}`, "max_timeout_ms"},
		{"transcripts timeout_ms", bashStyle("transcripts"), `{"timeout_ms":` + sixHours + `}`, `{"timeout_ms":` + over + `}`, "timeout_ms"},
		{"transcript timeout_ms", bashStyle("transcript"), `{"timeout_ms":` + sixHours + `}`, `{"timeout_ms":` + over + `}`, "timeout_ms"},
		{"profile_message timeout_ms", func(body string) error {
			_, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":`+body+`}`), "setting.json", "")
			return err
		}, `{"timeout_ms":` + sixHours + `}`, `{"timeout_ms":` + over + `}`, "timeout_ms"},
		{"profile_message busy_wait_ms", func(body string) error {
			_, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":`+body+`}`), "setting.json", "")
			return err
		}, `{"timeout_ms":` + sixHours + `,"busy_wait_ms":` + sixHours + `}`, `{"busy_wait_ms":` + over + `}`, "busy_wait_ms"},
		{"subagent agent timeout_ms", func(body string) error {
			_, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":`+body+`}}`), "/s/setting.json", "/w", "")
			return err
		}, `{"timeout_ms":` + sixHours + `}`, `{"timeout_ms":` + over + `}`, "timeout_ms"},
	}
	for _, key := range []string{"default_block_ms", "max_block_ms", "default_wait_ms", "max_wait_ms", "stop_wait_ms"} {
		okBody := `{"` + key + `":` + sixHours + `}`
		if strings.HasPrefix(key, "default_") {
			okBody = `{"` + key + `":` + sixHours + `,"max_` + strings.TrimPrefix(key, "default_") + `":` + sixHours + `}`
		}
		cases = append(cases, struct {
			name   string
			run    func(string) error
			ok     string
			tooBig string
			key    string
		}{"async " + key, func(body string) error {
			_, err := normalizeAsync(t, `{"native":{"async":`+body+`}}`)
			return err
		}, okBody, `{"` + key + `":` + over + `}`, key})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(tc.ok); err != nil {
				t.Fatalf("6 hours rejected: %v", err)
			}
			err := tc.run(tc.tooBig)
			if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), "21600000") && !strings.Contains(err.Error(), "between") {
				t.Fatalf("above 6 hours: error = %v", err)
			}
		})
	}
}
