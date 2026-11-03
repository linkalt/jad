package session

import (
	"bytes"
	"encoding/json"
	"testing"
)

// responseTokensAndCalls decodes the committed event once (audit R2 / p2b2 PH2). It must
// answer exactly what responseTokens and responseFunctionCalls answer, malformed events
// included.
func TestResponseTokensAndCallsMatchesTheSeparateDecoders(t *testing.T) {
	call := `{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}`
	events := []string{
		`{"type":"response.completed","response":{"output":[{"type":"message"},` + call + `],"usage":{"total_tokens":42}}}`,
		`{"response":{"output":[` + call + `],"usage":{"input_tokens":3,"output_tokens":4}}}`,
		`{"response":{"output":[` + call + `],"usage":"broken"}}`,
		`{"response":{"output":"broken","usage":{"total_tokens":5}}}`,
		`{"response":{"Output":[` + call + `],"USAGE":{"total_tokens":6}}}`,
		`{"response":{"output":[` + call + `,` + call + `]}}`,
		`{"response":null}`,
		`not json`,
		`{"response":{"output":[1,"x",{"type":"function_call"}],"usage":{"total_tokens":-1}}}`,
	}
	for _, text := range events {
		event := json.RawMessage(text)
		wantTokens := responseTokens(event)
		wantCalls, wantErr := responseFunctionCalls(event)
		tokens, calls, err := responseTokensAndCalls(event)
		if tokens != wantTokens {
			t.Errorf("%s: tokens = %d, want %d", text, tokens, wantTokens)
		}
		if (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
			t.Errorf("%s: err = %v, want %v", text, err, wantErr)
		}
		if len(calls) != len(wantCalls) {
			t.Errorf("%s: %d calls, want %d", text, len(calls), len(wantCalls))
			continue
		}
		for index := range calls {
			if !bytes.Equal(calls[index], wantCalls[index]) {
				t.Errorf("%s: call %d = %s, want %s", text, index, calls[index], wantCalls[index])
			}
		}
	}
}
