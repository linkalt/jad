package session

import (
	"encoding/json"

	"jad/backend-go/internal/responses"
)

func finalAnswerRound(text string, inputTokens int) []responses.Event {
	item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
	return []responses.Event{
		{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)},
		{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-` + text + `","status":"completed","usage":{"input_tokens":` + itoa(inputTokens) + `},"output":[` + item + `]}}`)},
	}
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
