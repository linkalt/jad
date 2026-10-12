package sessionfs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeStateContextSettingsDefaultsAndProcessProvider(t *testing.T) {
	const settingPath = "/session/setting.json"
	defaults, err := normalizeStateContextSettings(map[string]json.RawMessage{}, settingPath, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("normalizeStateContextSettings(defaults) error = %v", err)
	}
	if defaults.Enabled || defaults.Role != "developer" || defaults.MaxBytes != 64*1024 || !reflect.DeepEqual(defaults.Trigger, []string{"compaction"}) || len(defaults.Tools) != 0 || defaults.Header != DefaultStateContextHeader {
		t.Fatalf("statecontext defaults = %+v", defaults)
	}

	setting := map[string]json.RawMessage{
		"statecontext": json.RawMessage(`{"enabled":true,"trigger":["compaction","compaction"],"tools":["process","process"],"role":"system","max_bytes":2048}`),
	}
	configured, err := normalizeStateContextSettings(setting, settingPath, &NativeProcessSettings{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("normalizeStateContextSettings(process) error = %v", err)
	}
	if !configured.Enabled || configured.Role != "system" || configured.MaxBytes != 2048 || !reflect.DeepEqual(configured.Trigger, []string{"compaction"}) || !reflect.DeepEqual(configured.Tools, []string{"process"}) {
		t.Fatalf("normalized process statecontext = %+v", configured)
	}
}

func TestNormalizeStateContextSettingsRejectsInvalidAndUnavailableProviders(t *testing.T) {
	const settingPath = "/session/setting.json"
	for _, raw := range []string{
		`null`,
		`{"enabled":"yes"}`,
		`{"trigger":["timer"]}`,
		`{"enabled":true,"trigger":[]}`,
		`{"enabled":true,"tools":[]}`,
		`{"role":"assistant"}`,
		`{"max_bytes":1023}`,
		`{"max_bytes":10485761}`,
		`{"header":""}`,
		`{"header":"   "}`,
		`{"header":7}`,
	} {
		_, err := normalizeStateContextSettings(map[string]json.RawMessage{"statecontext": json.RawMessage(raw)}, settingPath, nil, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "statecontext") {
			t.Errorf("normalizeStateContextSettings(%s) error = %v, want statecontext config error", raw, err)
		}
	}

	_, err := normalizeStateContextSettings(
		map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"tools":["process"]}`)},
		settingPath,
		nil,
		nil,
		nil,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "requires tools.native.process") {
		t.Fatalf("missing process provider error = %v", err)
	}
	_, err = normalizeStateContextSettings(
		map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"tools":["task"]}`)},
		settingPath,
		nil,
		nil,
		nil,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "requires tools.native.task") {
		t.Fatalf("missing task provider error = %v", err)
	}
	configuredTask, err := normalizeStateContextSettings(
		map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"tools":["task"]}`)},
		settingPath,
		nil,
		&NativeTaskSettings{},
		nil,
		nil,
	)
	if err != nil || !configuredTask.Enabled || !reflect.DeepEqual(configuredTask.Tools, []string{"task"}) {
		t.Fatalf("configured task provider = %+v, error = %v", configuredTask, err)
	}
	_, err = normalizeStateContextSettings(
		map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"tools":["memory"]}`)},
		settingPath,
		nil,
		nil,
		nil,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "requires tools.native.memory") {
		t.Fatalf("missing memory provider error = %v", err)
	}
	memorySettings := &NativeMemorySettings{StateContext: []NativeMemoryStateContextSelector{{Type: "project", Filter: map[string]any{}, Limit: 1}}}
	configuredMemory, err := normalizeStateContextSettings(
		map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"tools":["memory"]}`)},
		settingPath,
		nil,
		nil,
		memorySettings,
		nil,
	)
	if err != nil || !configuredMemory.Enabled || !reflect.DeepEqual(configuredMemory.Tools, []string{"memory"}) {
		t.Fatalf("configured memory provider = %+v, error = %v", configuredMemory, err)
	}
	if _, err := normalizeStateContextSettings(
		map[string]json.RawMessage{"statecontext": json.RawMessage(`{"tools":["task"]}`)},
		settingPath,
		nil,
		nil,
		nil,
		nil,
	); err != nil {
		t.Fatalf("disabled task provider should remain inert, error = %v", err)
	}
}

func TestNormalizeStateContextSettingsHeader(t *testing.T) {
	const settingPath = "/session/setting.json"
	configured, err := normalizeStateContextSettings(map[string]json.RawMessage{
		"statecontext": json.RawMessage(`{"header":"Untrusted data follows."}`),
	}, settingPath, nil, nil, nil, nil)
	if err != nil || configured.Header != "Untrusted data follows." {
		t.Fatalf("custom header = %q, err = %v", configured.Header, err)
	}
	long := strings.Repeat("h", 1025)
	_, err = normalizeStateContextSettings(map[string]json.RawMessage{
		"statecontext": json.RawMessage(`{"header":"` + long + `","max_bytes":1024}`),
	}, settingPath, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "statecontext.header is larger than statecontext.max_bytes") {
		t.Fatalf("oversized header error = %v", err)
	}
}
