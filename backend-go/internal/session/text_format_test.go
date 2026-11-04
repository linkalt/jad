package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildRequestSetsTheTextFormatBesideOtherTextFields(t *testing.T) {
	history := [][]byte{[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`)}
	format := json.RawMessage(`{"type":"json_schema","name":"output","schema":{"type":"object"},"strict":true}`)
	for name, test := range map[string]struct {
		request string
		format  json.RawMessage
		want    string
	}{
		"no format":         {`{"text":{"verbosity":"low"}}`, nil, `{"verbosity":"low"}`},
		"no text":           {`{}`, format, `{"format":` + string(format) + `}`},
		"null text":         {`{"text":null}`, format, `{"format":` + string(format) + `}`},
		"keeps verbosity":   {`{"text":{"verbosity":"low"}}`, format, `{"format":` + string(format) + `,"verbosity":"low"}`},
		"replaces a format": {`{"text":{"format":{"type":"text"}}}`, format, `{"format":` + string(format) + `}`},
	} {
		request, err := BuildRequest(ModelSettings{Model: "m", Request: json.RawMessage(test.request), TextFormat: test.format}, history)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(request, &fields); err != nil {
			t.Fatal(err)
		}
		if got := string(fields["text"]); got != test.want {
			t.Errorf("%s: text = %s, want %s", name, got, test.want)
		}
	}
	_, err := BuildRequest(ModelSettings{Model: "m", Request: json.RawMessage(`{"text":"plain"}`), TextFormat: format}, history)
	if err == nil || !strings.Contains(err.Error(), "text must be a JSON object") {
		t.Fatalf("a text that is not an object: %v", err)
	}
}
