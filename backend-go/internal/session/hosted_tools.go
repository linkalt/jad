package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// appendHostedTools returns the function tools followed by the hosted tools. With no
// hosted tools it returns responseTools itself, so the request is the one it was
// before the setting existed.
func appendHostedTools(responseTools, hostedTools []json.RawMessage) []json.RawMessage {
	if len(hostedTools) == 0 {
		return responseTools
	}
	all := make([]json.RawMessage, 0, len(responseTools)+len(hostedTools))
	all = append(all, responseTools...)
	return append(all, hostedTools...)
}

// cloneRawList copies the list and each entry so a later change by the caller cannot
// reach the runner. A nil or empty list stays nil.
func cloneRawList(list []json.RawMessage) []json.RawMessage {
	if len(list) == 0 {
		return nil
	}
	copies := make([]json.RawMessage, len(list))
	for index, item := range list {
		copies[index] = cloneRaw(item)
	}
	return copies
}

// validateHostedTools checks that every hosted tool is a JSON object with a non-empty
// string type other than "function". The loader checks the same; this guards callers
// that build ModelSettings without it.
func validateHostedTools(tools []json.RawMessage) error {
	for index, tool := range tools {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(tool, &object); err != nil || object == nil {
			return fmt.Errorf("hosted tool %d must be a JSON object", index)
		}
		var toolType string
		if err := json.Unmarshal(object["type"], &toolType); err != nil || strings.TrimSpace(toolType) == "" {
			return fmt.Errorf("hosted tool %d needs a non-empty string type", index)
		}
		if strings.EqualFold(strings.TrimSpace(toolType), "function") {
			return fmt.Errorf("hosted tool %d must not be a function tool", index)
		}
	}
	return nil
}
