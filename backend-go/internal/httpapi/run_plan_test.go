package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadRunBodyReadsPlan(t *testing.T) {
	limits := RunInputLimits{MaxImages: 1, MaxImageBytes: 1024, MaxFiles: 1, MaxFileBytes: 1024}
	read := func(raw string) (runRequest, error) {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		return readRunBody(httptest.NewRecorder(), request, 1<<20, 1<<20, limits)
	}
	for raw, want := range map[string]bool{
		`{"input":"x"}`:                          false,
		`{"input":"x","plan":false}`:             false,
		`{"input":"x","plan":true}`:              true,
		`{"plan":true,"input":"x"}`:              true,
		`{"input":"x","plan":true,"plan":false}`: false,
	} {
		body, err := read(raw)
		if err != nil || body.Plan != want {
			t.Errorf("%s: plan %v err %v, want %v", raw, body.Plan, err, want)
		}
	}
	for _, raw := range []string{`{"input":"x","plan":"yes"}`, `{"input":"x","plan":null}`, `{"input":"x","plan":1}`} {
		_, err := read(raw)
		var status *StatusError
		if !errors.As(err, &status) || status.Code != http.StatusBadRequest || status.Message != "plan must be a boolean" {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestReadRunBodyReadsToolsAndExecutePlan(t *testing.T) {
	limits := RunInputLimits{MaxImages: 1, MaxImageBytes: 1024, MaxFiles: 1, MaxFileBytes: 1024}
	read := func(raw string) (runRequest, error) {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		return readRunBody(httptest.NewRecorder(), request, 1<<20, 1<<20, limits)
	}
	body, err := read(`{"input":"x","tools":["read","mcp:docs:*"]}`)
	if err != nil || strings.Join(body.Tools, ",") != "read,mcp:docs:*" {
		t.Fatalf("tools: %+v %v", body, err)
	}
	if body, err = read(`{"input":"x","tools":[]}`); err != nil || body.Tools == nil || len(body.Tools) != 0 {
		t.Fatalf("no tools: %+v %v", body, err)
	}
	if body, err = read(`{"input":"x"}`); err != nil || body.Tools != nil || body.ExecutePlan {
		t.Fatalf("no tools member: %+v %v", body, err)
	}
	for raw, want := range map[string]string{
		`{"execute_plan":true}`:                   `""`,
		`{"execute_plan":true,"input":""}`:        `""`,
		`{"execute_plan":true,"input":"  "}`:      `""`,
		`{"execute_plan":true,"input":"skip it"}`: `"skip it"`,
	} {
		body, err := read(raw)
		if err != nil || !body.ExecutePlan || string(body.Input) != want {
			t.Errorf("%s: %+v %v, want input %s", raw, body, err, want)
		}
	}
	for raw, message := range map[string]string{
		`{"input":"x","tools":"read"}`:                               "tools must be an array of tool names",
		`{"input":"x","tools":null}`:                                 "tools must be an array of tool names",
		`{"input":"x","tools":["rm"]}`:                               `tools[0] "rm" is not a native tool`,
		`{"input":"x","tools":["read","read"]}`:                      `tools[1] repeats "read"`,
		`{"input":"x","tools":["mcp:docs"]}`:                         `tools[0] "mcp:docs" must be mcp:<server>:<tool>`,
		`{"input":"x","execute_plan":"yes"}`:                         "execute_plan must be a boolean",
		`{"execute_plan":false}`:                                     "input must be a non-empty string",
		`{"execute_plan":true,"input":[{"type":"text","text":"x"}]}`: "with execute_plan, input must be a string or left out",
		`{"execute_plan":true,"plan":true}`:                          "execute_plan carries out a plan, so it cannot run in plan mode",
	} {
		_, err := read(raw)
		var status *StatusError
		if !errors.As(err, &status) || status.Code != http.StatusBadRequest || !strings.Contains(status.Message, message) {
			t.Errorf("%s: %v, want %q", raw, err, message)
		}
	}
}
