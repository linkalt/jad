package tools

import (
	"encoding/json"
	"testing"
)

func TestDraftJSONBytesMeasuresParseThenStringifySemantics(t *testing.T) {
	raw := json.RawMessage(`{"escaped":"\u0061","html":"\u003c","line":"\u2028","zero":-0,"one":1.0,"large":1e10000}`)
	got, err := DraftJSONBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("{\"escaped\":\"a\",\"html\":\"<\",\"line\":\"\u2028\",\"zero\":0,\"one\":1,\"large\":null}")
	if got != int64(len(want)) {
		t.Fatalf("DraftJSONBytes() = %d, want %d for %s", got, len(want), want)
	}
}

func TestDraftJSONDepthCountsContainersNotScalars(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{raw: `1`, want: 0},
		{raw: `{"a":1}`, want: 1},
		{raw: `{"a":[{"b":true}]}`, want: 3},
		{raw: `[]`, want: 1},
	}
	for _, test := range cases {
		got, err := DraftJSONDepth(json.RawMessage(test.raw))
		if err != nil || got != test.want {
			t.Errorf("DraftJSONDepth(%s) = %d, err=%v, want %d", test.raw, got, err, test.want)
		}
	}
}
