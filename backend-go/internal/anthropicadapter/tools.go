package anthropicadapter

import (
	"encoding/json"
)

// translateTools converts Responses function tools. Hosted tools such as
// file_search or code_interpreter have no equivalent and are rejected: ignoring
// them would change what the model can do without telling the caller.
//
// Anthropic refuses oneOf, allOf and anyOf at the top level of input_schema, and
// the harness's own tools use them to say "one of these fields is required". The
// keywords listed in config.StripSchemaKeywords are removed from the top level
// only. The caller still checks every call against its full schema, so the
// constraint is enforced there; the model just no longer sees it. Each removal is
// added to ignored so the response header shows it.
func translateTools(config *Config, tools []rawObject, ignored *[]string) ([]anthropicTool, *apiError) {
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
			stripped, removed := stripTopLevelKeywords(schema, config.StripSchemaKeywords)
			schema = stripped
			for _, keyword := range removed {
				*ignored = append(*ignored, "tools."+name+"."+keyword)
			}
		}
		out = append(out, anthropicTool{
			Name:        name,
			Description: stringOf(tool["description"]),
			InputSchema: schema,
		})
	}
	return out, nil
}

// stripTopLevelKeywords removes the listed keywords from the top level of a JSON
// schema object and returns the keywords it removed, in the order of keywords. A schema that holds
// none of them is returned unchanged, byte for byte. Nested schemas are not
// touched: Anthropic's rule is about the top level.
func stripTopLevelKeywords(schema json.RawMessage, keywords []string) (json.RawMessage, []string) {
	if len(keywords) == 0 {
		return schema, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(schema, &object); err != nil {
		return schema, nil
	}
	var removed []string
	for _, keyword := range keywords {
		if _, ok := object[keyword]; ok {
			delete(object, keyword)
			removed = append(removed, keyword)
		}
	}
	if len(removed) == 0 {
		return schema, nil
	}
	// Marshalling a map sorts its keys, so the result is stable across requests and
	// does not disturb a prompt-cache prefix.
	rebuilt, err := json.Marshal(object)
	if err != nil {
		return schema, nil
	}
	return rebuilt, removed
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
