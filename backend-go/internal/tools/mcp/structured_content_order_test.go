package mcp

import (
	"encoding/json"
	"runtime"
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

// A number the server sent must survive the conversion: a literal above float64's range
// cannot be represented, but it must not fail the call either, and every in-range
// literal keeps its digits instead of being rounded through a float64.
func TestPreserveStructuredContentOrderKeepsNumberLiterals(t *testing.T) {
	for _, testCase := range []struct{ name, raw, want string }{
		{"out of range becomes null like JSON.stringify", `{"m":1e400}`, `{"m":null}`},
		{"negative out of range becomes null", `{"m":-1e400}`, `{"m":null}`},
		{"integer beyond 2^53 keeps its digits", `{"id":9007199254740993}`, `{"id":9007199254740993}`},
		{"long integer keeps its digits", `{"big":123456789012345678901234567890}`, `{"big":123456789012345678901234567890}`},
		{"in-range literals are not normalized", `{"a":1.0,"b":1e2,"c":0.5}`, `{"a":1.0,"b":1e2,"c":0.5}`},
		{"underflow keeps the literal", `{"small":1e-400}`, `{"small":1e-400}`},
		{"numbers inside arrays and nested objects", `{"a":[1e400,{"b":9007199254740993}]}`, `{"a":[null,{"b":9007199254740993}]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ordered, err := preserveStructuredContentOrder(json.RawMessage(testCase.raw))
			if err != nil {
				t.Fatalf("preserveStructuredContentOrder(%s) error = %v", testCase.raw, err)
			}
			got, err := compactJSON(ordered)
			if err != nil {
				t.Fatalf("compactJSON(%s) error = %v", testCase.raw, err)
			}
			if got != testCase.want {
				t.Fatalf("structured content = %s, want %s", got, testCase.want)
			}
		})
	}
}

// Converting a deeply nested value must cost time and memory proportional to its size,
// not to size x depth: the encoder writes the whole tree into one buffer. The assertion
// is on allocated bytes (deterministic), not on wall-clock time. Before the shared
// buffer this 1 MiB value at 1,000 levels allocated 7.39 GB, ~7,000 bytes per payload
// byte; one pass over the value allocates tens.
func TestPreserveStructuredContentOrderStaysLinearInNestingDepth(t *testing.T) {
	const depth = 1000
	raw := benchmarkNestedStructuredContent(depth, 1<<20)
	if _, err := preserveStructuredContentOrder(raw); err != nil {
		t.Fatalf("preserveStructuredContentOrder() error = %v", err)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	ordered, err := preserveStructuredContentOrder(raw)
	if err != nil {
		t.Fatalf("preserveStructuredContentOrder() error = %v", err)
	}
	encoded, err := compactJSON(ordered)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("compactJSON() error = %v", err)
	}

	if encoded != string(raw) {
		t.Fatalf("converted value is %d bytes, want the %d bytes of the input", len(encoded), len(raw))
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if limit := uint64(64 * len(raw)); allocated > limit {
		t.Fatalf("converting a %d-byte value at depth %d allocated %d bytes, want at most %d: the encoder is re-encoding each nesting level instead of writing the value once", len(raw), depth, allocated, limit)
	}
}
