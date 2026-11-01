package transcript

import "testing"

func TestHasUnansweredToolCall(t *testing.T) {
	call := `{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}`
	output := `{"type":"function_call_output","call_id":"c1","output":"ok"}`
	message := `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	tests := []struct {
		name    string
		journal string
		want    bool
	}{
		{"no calls", message + "\n", false},
		{"answered", message + "\n" + call + "\n" + output + "\n", false},
		{"unanswered at the tail", message + "\n" + call + "\n", true},
		{"unanswered before a later answered call", call + "\n" + message + "\n" + `{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"}` + "\n" + output + "\n", true},
		{"answer before the call does not count", output + "\n" + call + "\n", true},
		{"empty journal", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := HasUnansweredToolCall([]byte(test.journal)); got != test.want {
				t.Fatalf("HasUnansweredToolCall = %v, want %v", got, test.want)
			}
		})
	}
}
