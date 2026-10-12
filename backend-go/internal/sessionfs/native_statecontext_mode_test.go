package sessionfs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func nativeStateContextSection(t *testing.T, tool, raw string) (any, error) {
	t.Helper()
	agents := ""
	if tool == "subagent" {
		agents = `"agents":{"a":{}},`
	}
	setting := map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"` + tool + `":{` + agents + `"statecontext":` + raw + `}}}`)}
	switch tool {
	case "task":
		settings, err := normalizeNativeTaskSettings(setting, "/s/setting.json", t.TempDir(), "")
		if err != nil {
			return nil, err
		}
		return settings.StateContext, nil
	case "process":
		settings, err := normalizeNativeProcessSettings(setting, t.TempDir(), "/s/setting.json", t.TempDir(), t.TempDir(), "", t.TempDir())
		if err != nil {
			return nil, err
		}
		return settings.StateContext, nil
	default:
		settings, err := normalizeNativeSubagentSettings(setting, "/s/setting.json", t.TempDir(), "")
		if err != nil {
			return nil, err
		}
		return settings.StateContext, nil
	}
}

func TestNativeStateContextSummaryModeAcceptedForTaskProcessAndSubagent(t *testing.T) {
	cases := []struct {
		tool, raw string
		want      any
	}{
		{"task", `{"mode":"summary","statuses":["todo","in-progress","done"]}`,
			NativeTaskStateContextSettings{Statuses: []string{"todo", "in-progress", "done"}, Limit: 25, Mode: "summary", DetailStatuses: []string{"in-progress"}}},
		{"task", `{"mode":"summary","statuses":["todo","done"],"detail_statuses":["todo","done"],"limit":3}`,
			NativeTaskStateContextSettings{Statuses: []string{"todo", "done"}, Limit: 3, Mode: "summary", DetailStatuses: []string{"todo", "done"}}},
		{"process", `{"mode":"summary","states":["running","exited"]}`,
			NativeProcessStateContextSettings{States: []string{"running", "exited"}, Limit: 20, Mode: "summary", DetailStates: []string{"running"}}},
		{"subagent", `{"mode":"summary","states":["queued","running","done"],"detail_states":["running","queued","running"]}`,
			NativeSubagentStateContextSettings{States: []string{"queued", "running", "done"}, Limit: 20, Mode: "summary", DetailStates: []string{"running", "queued"}}},
		{"subagent", `{"mode":"items"}`,
			NativeSubagentStateContextSettings{States: []string{"queued", "running"}, Limit: 20, Mode: "items"}},
	}
	for _, tc := range cases {
		got, err := nativeStateContextSection(t, tc.tool, tc.raw)
		if err != nil {
			t.Errorf("%s %s: %v", tc.tool, tc.raw, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %s:\n got  %+v\n want %+v", tc.tool, tc.raw, got, tc.want)
		}
	}
}

func TestNativeStateContextSummaryModeRejectsInvalidValues(t *testing.T) {
	cases := []struct{ tool, raw, want string }{
		{"task", `{"mode":"counts"}`, "statecontext.mode"},
		{"task", `{"mode":7}`, "statecontext.mode"},
		{"process", `{"mode":""}`, "statecontext.mode"},
		{"subagent", `{"mode":"Summary"}`, "statecontext.mode"},
		{"task", `{"detail_statuses":["done"]}`, "only valid with mode"},
		{"process", `{"detail_states":["running"]}`, "only valid with mode"},
		{"subagent", `{"mode":"items","detail_states":["running"]}`, "only valid with mode"},
		{"task", `{"mode":"summary","statuses":["done"],"detail_statuses":["todo"]}`, "not one of the states"},
		{"process", `{"mode":"summary","states":["exited"],"detail_states":["running"]}`, "not one of the states"},
		{"subagent", `{"mode":"summary","detail_states":["done"]}`, "not one of the states"},
		{"task", `{"mode":"summary","detail_statuses":"done"}`, "array of states"},
	}
	for _, tc := range cases {
		_, err := nativeStateContextSection(t, tc.tool, tc.raw)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: error = %v, want it to contain %q", tc.tool, tc.raw, err, tc.want)
		}
	}
}
