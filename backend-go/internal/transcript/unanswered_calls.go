package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// HasUnansweredToolCall reports whether a transcript holds a function_call that has no
// function_call_output with its call_id after it. A resumed run is refused on such a journal:
// its child would start from a call that never got its answer. The journal is read whole, and a
// line that is not a JSON object is skipped, as the compaction code skips it.
func HasUnansweredToolCall(journal []byte) bool {
	open := make(map[string]int)
	reader := bufio.NewReader(bytes.NewReader(journal))
	for {
		line, err := reader.ReadBytes('\n')
		if fields, parseErr := objectFields(line); parseErr == nil {
			typeRaw, _ := fieldValue(fields, "type")
			itemType, _ := stringValue(typeRaw)
			switch itemType {
			case "function_call":
				open[rawCallID(fields)]++
			case "function_call_output":
				delete(open, rawCallID(fields))
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return true
		}
	}
	return len(open) > 0
}
