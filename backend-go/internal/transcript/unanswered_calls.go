package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// HasUnansweredToolCall reports whether a transcript holds a function_call that has no
// function_call_output with its call_id after it. A resumed run is refused on such a journal:
// its child would start from a call that never got its answer. Calls are read the way the
// runner reads them: a committed response.completed line is expanded into its output items,
// and any other line is an item of its own. A line that is not a JSON object is skipped, as
// the compaction code skips it.
func HasUnansweredToolCall(journal []byte) bool {
	open := make(map[string]int)
	reader := bufio.NewReader(bytes.NewReader(journal))
	for {
		line, err := reader.ReadBytes('\n')
		if items, expandErr := expandTranscriptRecord(line); expandErr == nil {
			for _, item := range items {
				fields, parseErr := objectFields(item)
				if parseErr != nil {
					continue
				}
				typeRaw, _ := fieldValue(fields, "type")
				itemType, _ := stringValue(typeRaw)
				switch itemType {
				case "function_call":
					open[rawCallID(fields)]++
				case "function_call_output":
					delete(open, rawCallID(fields))
				}
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
