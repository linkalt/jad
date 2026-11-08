package native

import "testing"

func TestRecordsJSONStringifyMatchesJavaScriptObjectOrdering(t *testing.T) {
	value, err := parseRecordsJSON([]byte(`{"2":"two","1":"one","html":"<tag>","line":"a\u2028b"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := recordsJSONPretty(value)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"1\": \"one\",\n  \"2\": \"two\",\n  \"html\": \"<tag>\",\n  \"line\": \"a\u2028b\"\n}\n"
	if encoded != want {
		t.Fatalf("JavaScript-compatible JSON = %q, want %q", encoded, want)
	}
}
