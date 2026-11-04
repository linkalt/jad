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
