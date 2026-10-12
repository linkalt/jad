package sessionfs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeStateContextSettingsDefaultsAndProcessProvider(t *testing.T) {
	const settingPath = "/session/setting.json"
	defaults, err := normalizeStateContextSettings(map[string]json.RawMessage{}, settingPath, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("normalizeStateContextSettings(defaults) error = %v", err)
	}
	if defaults.Enabled || defaults.Role != "developer" || defaults.MaxBytes != 64*1024 || !reflect.DeepEqual(defaults.Trigger, []string{"compaction"}) || len(defaults.Tools) != 0 {
		t.Fatalf("statecontext defaults = %+v", defaults)
	}

	setting := map[string]json.RawMessage{
		"statecontext": json.RawMessage(`{"enabled":true,"trigger":["compaction","compaction"],"tools":["process","process"],"role":"system","max_bytes":2048}`),
	}
	configured, err := normalizeStateContextSettings(setting, settingPath, &NativeProcessSettings{}, nil, nil, nil, nil)
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
	} {
		_, err := normalizeStateContextSettings(map[string]json.RawMessage{"statecontext": json.RawMessage(raw)}, settingPath, nil, nil, nil, nil, nil)
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
		nil,
	); err != nil {
		t.Fatalf("disabled task provider should remain inert, error = %v", err)
	}
}

func TestStateContextRequestDraftProviderNeedsItsToolAndIsOtherwiseInert(t *testing.T) {
	const settingPath = "/tmp/setting.json"
	enabled := map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"tools":["request_draft"]}`)}
	if _, err := normalizeStateContextSettings(enabled, settingPath, nil, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "statecontext provider request_draft requires tools.native.request_draft") {
		t.Fatalf("missing request_draft tool error = %v", err)
	}
	configured, err := normalizeStateContextSettings(enabled, settingPath, nil, nil, nil, nil, &NativeRequestDraftSettings{})
	if err != nil || !configured.Enabled || !reflect.DeepEqual(configured.Tools, []string{"request_draft"}) {
		t.Fatalf("configured request_draft provider = %+v, error = %v", configured, err)
	}
	inert := map[string]json.RawMessage{"statecontext": json.RawMessage(`{"tools":["request_draft"]}`)}
	if _, err := normalizeStateContextSettings(inert, settingPath, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("disabled request_draft provider should remain inert, error = %v", err)
	}
	both := map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"tools":["task","request_draft","task"]}`)}
	mixed, err := normalizeStateContextSettings(both, settingPath, nil, &NativeTaskSettings{}, nil, nil, &NativeRequestDraftSettings{})
	if err != nil || !reflect.DeepEqual(mixed.Tools, []string{"task", "request_draft"}) {
		t.Fatalf("task plus request_draft = %+v, error = %v", mixed, err)
	}
}
