package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
)

// minOverflowToolOutputCap is the smallest cap later overflow attempts halve the tool output
// cap to: below it an output keeps too little to be worth another attempt. It is the smallest
// value compaction.overflow.tool_output_max_bytes takes besides 0.
const minOverflowToolOutputCap = sessionfs.MinCompactionOverflowToolOutputMaxBytes

// lowerToolOutputCap sets the cap on tool outputs in the requests of the rest of the run,
// for an overflow the compaction could not shrink: the first time to maxBytes, then half of
// the cap before. It reports false when cutting is off (maxBytes 0) or the cap is at its
// floor already.
func (run *runRounds) lowerToolOutputCap(maxBytes int) bool {
	if maxBytes <= 0 {
		return false
	}
	if run.toolOutputCap == 0 {
		run.toolOutputCap = maxBytes
		return true
	}
	if run.toolOutputCap/2 < minOverflowToolOutputCap {
		return false
	}
	run.toolOutputCap /= 2
	return true
}

// cutToolOutputs returns history with the text of every tool output longer than maxBytes cut
// to about maxBytes (its start and end kept, a note between them), and how many outputs were
// cut. Only the copy that is sent changes: the items of history, which the input history
// memo shares, are never written, and the transcript keeps the outputs whole.
func cutToolOutputs(history [][]byte, maxBytes int) ([][]byte, int) {
	if maxBytes <= 0 {
		return history, 0
	}
	var sent [][]byte
	cut := 0
	for index, item := range history {
		if len(item) <= maxBytes || !bytes.Contains(item, []byte(`_output"`)) {
			continue
		}
		replaced, changed := cutToolOutputItem(item, maxBytes)
		if !changed {
			continue
		}
		if sent == nil {
			sent = append([][]byte(nil), history...)
		}
		sent[index] = replaced
		cut++
	}
	if sent == nil {
		return history, 0
	}
	return sent, cut
}

// cutToolOutputItem cuts the output of one function_call_output or custom_tool_call_output
// item: a string output, or the text of each text part of an output list.
func cutToolOutputItem(item []byte, maxBytes int) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return item, false
	}
	var kind string
	if json.Unmarshal(fields["type"], &kind) != nil || (kind != "function_call_output" && kind != "custom_tool_call_output") {
		return item, false
	}
	raw, exists := fields["output"]
	if !exists {
		return item, false
	}
	changed := false
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if len(text) <= maxBytes {
			return item, false
		}
		fields["output"], _ = json.Marshal(cutOverflowText(text, maxBytes))
		changed = true
	} else {
		var parts []map[string]json.RawMessage
		if json.Unmarshal(raw, &parts) != nil {
			return item, false
		}
		for _, part := range parts {
			var partText string
			if json.Unmarshal(part["text"], &partText) != nil || len(partText) <= maxBytes {
				continue
			}
			part["text"], _ = json.Marshal(cutOverflowText(partText, maxBytes))
			changed = true
		}
		if changed {
			fields["output"], _ = json.Marshal(parts)
		}
	}
	if !changed {
		return item, false
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return item, false
	}
	return encoded, true
}

// cutOverflowText keeps the start and the end of text, at most maxBytes of them together,
// on UTF-8 boundaries, with a note between them of how much was left out and why. The note
// is for the model, which cannot read the transcript, so it says how to get the text again
// (run the tool narrower) and does not name the setting (the server log does).
func cutOverflowText(text string, maxBytes int) string {
	head := maxBytes / 2
	tail := maxBytes - head
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	start := len(text) - tail
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[:head] + fmt.Sprintf("\n[%d bytes of this tool output were left out of the request: the conversation was over the model's context window. They cannot be read back; if you need them, run the tool again with a narrower request (a filter, a smaller range, fewer lines)]\n", start-head) + text[start:]
}
