package anthropicadapter

import (
	"encoding/json"
)

// translateTools converts Responses function tools. Hosted tools such as
// file_search or code_interpreter have no equivalent and are rejected: ignoring
// them would change what the model can do without telling the caller.
func translateTools(tools []rawObject) ([]anthropicTool, *apiError) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for index, tool := range tools {
		kind := stringOf(tool["type"])
		if kind != "function" {
			return nil, unsupported("tools", "tools[%d]: type %q is not supported, only function tools can be translated", index, kind)
		}
		name := stringOf(tool["name"])
		if name == "" {
			return nil, badRequest("tools", "tools[%d]: function tools need a name", index)
		}
		schema := json.RawMessage(`{"type":"object"}`)
		if raw, ok := tool["parameters"]; ok && !isJSONNull(raw) {
			schema = compactJSON(raw)
		}
		out = append(out, anthropicTool{
			Name:        name,
			Description: stringOf(tool["description"]),
			InputSchema: schema,
		})
	}
	return out, nil
}

// translateToolChoice maps tool_choice and parallel_tool_calls. With no tools
// the choice is dropped, because Anthropic rejects tool_choice without tools.
func translateToolChoice(raw json.RawMessage, parallel *bool, hasTools bool) (*anthropicToolChoice, *apiError) {
	if !hasTools {
		return nil, nil
	}
	choice := &anthropicToolChoice{Type: "auto"}
	if len(raw) > 0 && !isJSONNull(raw) {
		var mode string
		if err := json.Unmarshal(raw, &mode); err == nil {
			switch mode {
			case "auto":
			case "required":
				choice.Type = "any"
			case "none":
				choice.Type = "none"
			default:
				return nil, badRequest("tool_choice", "tool_choice %q is not supported", mode)
			}
		} else {
			var object struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(raw, &object); err != nil {
				return nil, badRequest("tool_choice", "tool_choice must be a string or an object")
			}
			if object.Type != "function" {
				return nil, unsupported("tool_choice", "tool_choice type %q is not supported", object.Type)
			}
			name := object.Name
			if name == "" {
				name = object.Function.Name
			}
			if name == "" {
				return nil, badRequest("tool_choice", "tool_choice of type function needs a name")
			}
			choice.Type, choice.Name = "tool", name
		}
	}
	// disable_parallel_tool_use is not allowed together with type none.
	if parallel != nil && !*parallel && choice.Type != "none" {
		choice.DisableParallelToolUse = true
	}
	return choice, nil
}

func stringOf(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}
