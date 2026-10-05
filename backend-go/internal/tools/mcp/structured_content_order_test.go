package mcp

import (
	"encoding/json"
	"testing"
)

func TestPreserveStructuredContentOrderMatchesJavaScriptObjectKeys(t *testing.T) {
	raw := json.RawMessage(`{"z":"last insertion","10":"ten","2":"two","nested":{"second":2,"first":1},"items":[{"right":true,"left":false}]}`)
	ordered, err := preserveStructuredContentOrder(raw)
	if err != nil {
		t.Fatalf("preserveStructuredContentOrder() error = %v", err)
	}
	got, err := compactJSON(ordered)
	if err != nil {
		t.Fatalf("compactJSON() error = %v", err)
	}
	want := `{"2":"two","10":"ten","z":"last insertion","nested":{"second":2,"first":1},"items":[{"right":true,"left":false}]}`
	if got != want {
		t.Fatalf("structured content = %s, want %s", got, want)
	}
}

func TestPreserveStructuredContentOrderCollapsesDuplicateKeysLikeJSONParse(t *testing.T) {
	ordered, err := preserveStructuredContentOrder(json.RawMessage(`{"a":1,"b":2,"a":3}`))
	if err != nil {
		t.Fatalf("preserveStructuredContentOrder() error = %v", err)
	}
	got, err := compactJSON(ordered)
	if err != nil {
		t.Fatalf("compactJSON() error = %v", err)
	}
	if want := `{"a":3,"b":2}`; got != want {
		t.Fatalf("structured content = %s, want %s", got, want)
	}
}

func TestPreserveStructuredContentOrderAcceptsNonObjectJSON(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`false`), json.RawMessage(`[1,{"second":2,"first":1}]`)} {
		ordered, err := preserveStructuredContentOrder(raw)
		if err != nil {
			t.Fatalf("preserveStructuredContentOrder(%s) error = %v", raw, err)
		}
		got, err := compactJSON(ordered)
		if err != nil {
			t.Fatalf("compactJSON(%s) error = %v", raw, err)
		}
		if want := string(raw); got != want {
			t.Fatalf("structured content = %s, want %s", got, want)
		}
	}
}
