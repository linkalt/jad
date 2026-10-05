package transcript

import (
	"testing"
)

func TestCanonicalJSONMatchesJavaScriptStringifyOrderingAndEscaping(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "integer-like keys, duplicate keys, escapes, and HTML characters",
			raw:  `{"z":1.0,"a":"\u0061 <>&","10":"ten","2":"two","0":"zero","z":-0}`,
			want: `{"0":"zero","2":"two","10":"ten","z":0,"a":"a <>&"}`,
		},
		{
			name: "number notation thresholds and IEEE rounding",
			raw:  `{"values":[1.0,-0,1e-7,1e-6,1e21,1e20,9007199254740993]}`,
			want: `{"values":[1,0,1e-7,0.000001,1e+21,100000000000000000000,9007199254740992]}`,
		},
		{
			name: "Unicode line separators stay unescaped",
			raw:  `"\u2028<>&"`,
			want: "\"\u2028<>&\"",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := canonicalJSON([]byte(test.raw))
			if err != nil {
				t.Fatalf("canonicalJSON() error = %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("canonicalJSON() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestCanonicalJSONRejectsInvalidInput(t *testing.T) {
	if _, err := canonicalJSON([]byte(`{"bad":}`)); err == nil {
		t.Fatal("canonicalJSON() accepted invalid JSON")
	}
}
