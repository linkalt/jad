package sessionfs

import (
	"strings"
	"testing"
)

// The approval system was removed. Its keys stay refused for good, each error naming the
// key, so a setting that used to protect the user is never silently ignored.
func TestRemovedApprovalKeysAreRefusedByName(t *testing.T) {
	for name, tc := range map[string]struct {
		setting map[string]any
		want    string
	}{
		"top level section": {map[string]any{"approval": map[string]any{"default": "ask"}}, "settings.approval was removed"},
		"top level string":  {map[string]any{"approval": "ask"}, "settings.approval was removed"},
		"exec":              {nativeTools(map[string]any{"exec": map[string]any{"approval": "ask"}}), "tools.native.exec.approval was removed"},
		"tool_smith":        {nativeTools(map[string]any{"tool_smith": map[string]any{"approval": "ask"}}), "tools.native.tool_smith.approval was removed"},
		"profile_message":   {nativeTools(map[string]any{"profile_message": map[string]any{"approval": "ask"}}), "tools.native.profile_message.approval was removed"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadSetting(t, tc.setting)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "run JAD inside a container") {
				t.Fatalf("err = %v, want %q and the container hint", err, tc.want)
			}
		})
	}
}

func TestRemovedApprovalKeyIsRefusedEvenWithAValidLookingValue(t *testing.T) {
	for _, value := range []any{"off", "ask", nil, true, 5} {
		_, err := loadSetting(t, nativeTools(map[string]any{"exec": map[string]any{"approval": value}}))
		if err == nil || !strings.Contains(err.Error(), "approval was removed") {
			t.Errorf("approval %v: err = %v", value, err)
		}
	}
}

func TestBackgroundWriterSettingsAreStillAccepted(t *testing.T) {
	agent := map[string]any{"a": map[string]any{}}
	config, err := loadSetting(t, nativeTools(map[string]any{
		"read_image": map[string]any{},
		"subagent":   map[string]any{"agents": agent, "background_writers": true, "max_background_writers": 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeSubagent == nil || !config.NativeSubagent.BackgroundWriters || config.NativeSubagent.MaxBackgroundWriters != 2 {
		t.Fatalf("subagent = %+v", config.NativeSubagent)
	}
}
