package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const DecideSystemoneToolID = "extension.decide_systemone"

const decideSystemoneDescription = "Ask a fast classifier typed questions about some text or JSON. predicate: probability 0-1 that a condition holds. choice: the best of your choices, with confidence. score: a rating against ordered levels, lowest first. box: where a target is in an image sent in assets. Use it for yes/no checks, routing and severity, not for open-ended answers."

var DecideSystemoneInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "input":{
      "type":"array",
      "minItems":1,
      "items":{"anyOf":[
        {"type":"string","minLength":1},
        {"type":"object","properties":{"type":{"type":"string","enum":["text"]},"text":{"type":"string","minLength":1}},"required":["type","text"],"additionalProperties":false},
        {"type":"object","properties":{"type":{"type":"string","enum":["json"]},"value":{}},"required":["type","value"],"additionalProperties":false}
      ]},
      "description":"What to evaluate, in order: a string or {type:text,text}, {type:json,value} for structured data. Every question is asked about all of it."
    },
    "questions":{
      "type":"array",
      "minItems":1,
      "items":{
        "type":"object",
        "properties":{
          "name":{"type":"string","minLength":1,"description":"Label of the answer. Defaults to q1, q2, ..."},
          "type":{"type":"string","enum":["predicate","choice","score","box"],"description":"Inferred when omitted: choices gives choice, levels gives score, otherwise predicate. box is never inferred: set it, with instructions naming the target in an image."},
          "instructions":{"anyOf":[{"type":"string","minLength":1},{"type":"object"},{"type":"array"},{"type":"null"}],"description":"The question, with observable criteria: text, or a JSON object or array. Optional."},
          "criteria":{"anyOf":[{"type":"object"},{"type":"array","minItems":1},{"type":"null"}],"description":"Native criteria. predicate: {true,false} meanings; choice (set type): {label: meaning}, 1-255; score (set type): ordered meanings, lowest first, 1-10. Not with choices or levels."},
          "choices":{"type":"array","minItems":1,"items":{"anyOf":[{"type":"string","minLength":1},{"type":"object","properties":{"value":{"type":"string","minLength":1},"description":{"type":"string"}},"required":["value"],"additionalProperties":false}]},"description":"Options for a choice question: a value, or a value with when it applies."},
          "levels":{"type":"array","minItems":1,"items":{"anyOf":[{"type":"string","minLength":1},{"type":"object","properties":{"label":{"type":"string","minLength":1},"description":{"type":"string"}},"required":["label"],"additionalProperties":false}]},"description":"Ordered levels for a score question, lowest first: a label, or a label with its criteria."}
        },
        "additionalProperties":false
      }
    },
    "assets":{
      "type":"object",
      "additionalProperties":{"type":"object","properties":{"format":{"type":"string","enum":["png","jpg","jpeg"]},"data_base64":{"type":"string"},"type":{"type":"string","enum":["image"]}},"required":["format","data_base64"],"additionalProperties":false},
      "description":"Images by reference. Key: a reference you choose (a UUID is safest), written verbatim in the input, instructions or criteria; every reference must appear. Value: a PNG or JPEG as standard padded base64. A box question needs one."
    }
  },
  "required":["input","questions"],
  "additionalProperties":false
}`)

// decideSystemoneQuestion is one question after normalization.
type decideSystemoneQuestion struct {
	Name         string
	Type         string // predicate, choice, score or box
	Instructions string // text instructions; empty when none or structured
	// StructuredInstructions is an object or array, kept as the JSON it arrived in.
	StructuredInstructions json.RawMessage
	// Criteria is a predicate's native {true,false} object.
	Criteria json.RawMessage
	Options  []decideSystemoneOption // choices, or levels in order
	// Native is set when Options come from native criteria: each Raw is sent as given.
	Native bool
}

// decideSystemoneOption is a choice (Label is its value) or a score level. A native
// option carries its criteria in Raw; a native score level has no label.
type decideSystemoneOption struct {
	Label, Description string
	Raw                json.RawMessage
}

// decideSystemoneInputPart is one part of the input: text, or a JSON value when JSON is set.
type decideSystemoneInputPart struct {
	Text string
	JSON json.RawMessage
}

// size is the part's bytes as sent: the text, or the compact JSON.
func (p decideSystemoneInputPart) size() int {
	if p.JSON != nil {
		return len(p.JSON)
	}
	return len(p.Text)
}

// decideSystemoneEnvironment reads the API key at call time, so a key set after the server
// started is used and none is kept in the tool.
type decideSystemoneEnvironment func(name string) string

// NewDecideSystemoneTool creates extension.decide_systemone from its normalized settings.
func NewDecideSystemoneTool(settings sessionfs.ExtensionDecideSystemoneSettings) (tools.Tool, error) {
	return newDecideSystemoneTool(settings, os.Getenv, newDecideSystemoneHTTPClient())
}

// newDecideSystemoneHTTPClient never follows a redirect: the request carries the API key, and
// a provider has no reason to send this endpoint elsewhere.
func newDecideSystemoneHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func newDecideSystemoneTool(settings sessionfs.ExtensionDecideSystemoneSettings, environment decideSystemoneEnvironment, client *http.Client) (tools.Tool, error) {
	validator, err := tools.CompileSchema(DecideSystemoneInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile decide_systemone schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          DecideSystemoneToolID,
			Source:      "extension",
			Name:        "decide_systemone",
			Description: decideSystemoneDescription,
			InputSchema: append(json.RawMessage(nil), DecideSystemoneInputSchema...),
		},
		Validator:          validator,
		ReadOnly:           true,
		ExampleOperation:   json.RawMessage(`{"input":["I was charged twice."],"questions":[{"name":"billing","instructions":"Is this a billing problem?"}]}`),
		NormalizeArguments: normalizeDecideSystemoneArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeDecideSystemone(ctx, arguments, settings, environment, client, execution), nil
		},
	}, nil
}

// normalizeDecideSystemoneArguments accepts the spellings a model reaches for: OpenRouter's
// "noul" for a predicate, any letter case in the type, and padding around text.
func normalizeDecideSystemoneArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	normalized["input"] = normalizeDecideSystemoneInput(normalized["input"])
	if normalized["input"] == nil {
		delete(normalized, "input")
	}
	if assets, present := normalized["assets"]; present {
		if assets = normalizeDecideSystemoneAssets(assets); assets == nil {
			delete(normalized, "assets")
		} else {
			normalized["assets"] = assets
		}
	}
	questions, ok := normalized["questions"].([]any)
	if !ok {
		return normalized
	}
	fixed := make([]any, 0, len(questions))
	for _, item := range questions {
		question, ok := item.(map[string]any)
		if !ok {
			fixed = append(fixed, item)
			continue
		}
		copied := make(map[string]any, len(question))
		for key, value := range question {
			copied[key] = value
		}
		if kind, ok := copied["type"].(string); ok {
			kind = strings.ToLower(strings.TrimSpace(kind))
			switch kind {
			case "noul", "boolean", "bool":
				kind = "predicate"
			}
			if kind == "" {
				delete(copied, "type")
			} else {
				copied["type"] = kind
			}
		}
		for _, key := range []string{"name", "instructions"} {
			if text, ok := copied[key].(string); ok {
				copied[key] = strings.TrimSpace(text)
			}
		}
		fixed = append(fixed, copied)
	}
	normalized["questions"] = fixed
	return normalized
}

// normalizeDecideSystemoneInput lets a bare string or a single part stand for a one-part
// input, and trims the type of each part. Anything else is left for the schema.
func normalizeDecideSystemoneInput(input any) any {
	switch value := input.(type) {
	case string:
		return []any{value}
	case map[string]any:
		if _, hasType := value["type"]; hasType {
			input = []any{value}
		}
	}
	parts, ok := input.([]any)
	if !ok {
		return input
	}
	fixed := make([]any, 0, len(parts))
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			fixed = append(fixed, item)
			continue
		}
		copied := make(map[string]any, len(part))
		for key, value := range part {
			copied[key] = value
		}
		if kind, ok := copied["type"].(string); ok {
			copied["type"] = strings.ToLower(strings.TrimSpace(kind))
		}
		fixed = append(fixed, copied)
	}
	return fixed
}

func executeDecideSystemone(ctx context.Context, arguments map[string]any, settings sessionfs.ExtensionDecideSystemoneSettings, environment decideSystemoneEnvironment, client *http.Client, execution tools.ExecutionContext) string {
	callID := execution.CallID
	input, err := parseDecideSystemoneInput(arguments["input"])
	if err != nil {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "invalid_input", err.Error())
	}
	size := 0
	for _, part := range input {
		size += part.size()
	}
	assets, err := parseDecideSystemoneAssets(arguments["assets"])
	if err != nil {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "invalid_input", err.Error())
	}
	questions, err := parseDecideSystemoneQuestions(arguments["questions"], execution.RawArguments)
	if err != nil {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "invalid_question", err.Error())
	}
	if len(assets) == 0 && decideSystemoneHasBox(questions) {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "invalid_question", "A box question needs an image: send it in assets, with its reference in the input or the instructions")
	}
	if int64(len(questions)) > settings.MaxQuestions {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "too_many_questions", fmt.Sprintf("%d questions; the limit is %d", len(questions), settings.MaxQuestions))
	}
	// One request goes out whole, so one counter covers it: every instruction, option
	// label and description is in the questions, and max_input_bytes bounds the input
	// parts, the questions and the assets together.
	questionBytes, err := tools.MarshalJSON(systemoneQuestionItems(questions))
	if err != nil {
		return decideSystemoneError(callID, tools.StatusError, "bad_request", fmt.Sprintf("The questions cannot be encoded: %v", err))
	}
	assetBytes := decideSystemoneAssetsSize(assets)
	if requestBytes := int64(size) + int64(len(questionBytes)) + int64(assetBytes); requestBytes > settings.MaxInputBytes {
		breakdown := fmt.Sprintf("%d input, %d questions", size, len(questionBytes))
		if len(assets) > 0 {
			breakdown += fmt.Sprintf(", %d assets", assetBytes)
		}
		return decideSystemoneError(callID, tools.StatusInvalidInput, "input_too_large", fmt.Sprintf("The request is %d bytes (%s); max_input_bytes is %d", requestBytes, breakdown, settings.MaxInputBytes))
	}
	// The scan comes after the size check, so an oversized request is refused without
	// reading its images.
	if err := checkDecideSystemoneAssetData(assets); err != nil {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "invalid_input", err.Error())
	}
	key := strings.TrimSpace(environment(settings.APIKeyEnv))
	if key == "" {
		return decideSystemoneError(callID, tools.StatusError, "api_key_missing", "Environment variable "+settings.APIKeyEnv+" is not set")
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutMS)*time.Millisecond)
	defer cancel()
	response, err := requestDecisions(ctx, client, settings, key, input, questions, assets)
	if err != nil {
		return decideSystemoneRequestError(callID, err)
	}
	text, err := renderDecideSystemoneAnswers(questions, response.Answers)
	if err != nil {
		return decideSystemoneError(callID, tools.StatusError, "bad_response", err.Error())
	}
	noun := "questions"
	if len(questions) == 1 {
		noun = "question"
	}
	model := response.Model
	if strings.TrimSpace(model) == "" {
		model = settings.Model
	}
	information := []tools.Information{{Key: "model", Value: model}}
	if response.InputTokens != nil {
		information = append(information, tools.Information{Key: "input_tokens", Value: *response.InputTokens})
	}
	if response.OutputTokens != nil {
		information = append(information, tools.Information{Key: "output_tokens", Value: *response.OutputTokens})
	}
	result, err := tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("Answered %d %s", len(questions), noun),
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
	if err != nil {
		return decideSystemoneError(callID, tools.StatusError, "render_failed", err.Error())
	}
	return result
}

// parseDecideSystemoneInput turns the validated input into parts. A blank text or a null JSON
// value is refused, since the classifier would answer about nothing.
func parseDecideSystemoneInput(raw any) ([]decideSystemoneInputPart, error) {
	items, _ := raw.([]any)
	parts := make([]decideSystemoneInputPart, 0, len(items))
	for index, item := range items {
		var part decideSystemoneInputPart
		switch value := item.(type) {
		case string:
			part.Text = value
		case map[string]any:
			kind, _ := value["type"].(string)
			if kind == "json" {
				encoded, err := tools.MarshalJSON(value["value"])
				if err != nil {
					return nil, fmt.Errorf("input part %d: the JSON value cannot be encoded: %v", index+1, err)
				}
				if string(encoded) == "null" {
					return nil, fmt.Errorf("input part %d: the JSON value is null", index+1)
				}
				part.JSON = encoded
			} else {
				part.Text, _ = value["text"].(string)
			}
		}
		if part.JSON == nil && strings.TrimSpace(part.Text) == "" {
			return nil, fmt.Errorf("input part %d: the text is blank", index+1)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// TypeSafe SystemOne protocol cardinalities, shared by both configured routes.
// These are criteria-shape bounds, not configurable resource budgets.
const (
	decideSystemoneMaxScoreLevels = 10
	decideSystemoneMaxChoices     = 255
)

// parseDecideSystemoneQuestions turns the validated arguments into questions: it infers a
// missing type from choices or levels, names the unnamed ones q1, q2, ..., and
// refuses what the provider would refuse (a duplicate name, options that do not fit
// the type), so a mistake costs no request.
func parseDecideSystemoneQuestions(raw any, rawArguments json.RawMessage) ([]decideSystemoneQuestion, error) {
	items, _ := raw.([]any)
	original := decideSystemoneOriginal{arguments: rawArguments}
	questions := make([]decideSystemoneQuestion, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for index, item := range items {
		fields, _ := item.(map[string]any)
		question := decideSystemoneQuestion{Name: fmt.Sprintf("q%d", index+1)}
		if name, _ := fields["name"].(string); name != "" {
			question.Name = name
		}
		if _, duplicate := seen[question.Name]; duplicate {
			return nil, fmt.Errorf("question name %q is used twice", question.Name)
		}
		seen[question.Name] = struct{}{}
		// Instructions are optional: text, or an object or array sent as given.
		switch value := fields["instructions"].(type) {
		case string:
			question.Instructions = value
		case map[string]any, []any:
			encoded, err := original.field(index, "instructions", value)
			if err != nil {
				return nil, fmt.Errorf("question %q: the instructions cannot be encoded: %v", question.Name, err)
			}
			question.StructuredInstructions = encoded
		}
		choices, hasChoices := fields["choices"].([]any)
		levels, hasLevels := fields["levels"].([]any)
		if hasChoices && hasLevels {
			return nil, fmt.Errorf("question %q has both choices and levels", question.Name)
		}
		criteria := fields["criteria"]
		if criteria != nil && (hasChoices || hasLevels) {
			return nil, fmt.Errorf("question %q has criteria and also choices or levels; use one", question.Name)
		}
		question.Type, _ = fields["type"].(string)
		if question.Type == "" {
			switch {
			case hasChoices:
				question.Type = "choice"
			case hasLevels:
				question.Type = "score"
			default:
				question.Type = "predicate"
			}
		}
		switch {
		case question.Type == "box":
			if err := checkDecideSystemoneBox(question.Name, fields["instructions"], criteria, hasChoices || hasLevels); err != nil {
				return nil, err
			}
			questions = append(questions, question)
			continue
		case question.Type == "predicate" && (hasChoices || hasLevels):
			return nil, fmt.Errorf("question %q is a predicate and takes no choices or levels", question.Name)
		case question.Type == "choice" && hasLevels:
			return nil, fmt.Errorf("question %q is a choice and cannot have levels; use choices, or type score", question.Name)
		case question.Type == "score" && hasChoices:
			return nil, fmt.Errorf("question %q is a score and cannot have choices; use levels, or type choice", question.Name)
		case question.Type == "choice" && !hasChoices && criteria == nil:
			return nil, fmt.Errorf("question %q is a choice and needs choices", question.Name)
		case question.Type == "score" && !hasLevels && criteria == nil:
			return nil, fmt.Errorf("question %q is a score and needs levels", question.Name)
		}
		if criteria != nil {
			if err := parseDecideSystemoneCriteria(&question, criteria, index, &original); err != nil {
				return nil, err
			}
			questions = append(questions, question)
			continue
		}
		labelKey, source := "value", choices
		if question.Type == "score" {
			labelKey, source = "label", levels
		}
		labels := make(map[string]struct{}, len(source))
		for _, option := range source {
			parsed := decideSystemoneOption{}
			switch value := option.(type) {
			case string:
				parsed.Label = strings.TrimSpace(value)
			case map[string]any:
				parsed.Label, _ = value[labelKey].(string)
				parsed.Label = strings.TrimSpace(parsed.Label)
				parsed.Description, _ = value["description"].(string)
				parsed.Description = strings.TrimSpace(parsed.Description)
			}
			if parsed.Label == "" {
				return nil, fmt.Errorf("question %q has an option without a %s", question.Name, labelKey)
			}
			if _, duplicate := labels[parsed.Label]; duplicate {
				return nil, fmt.Errorf("question %q lists %q twice", question.Name, parsed.Label)
			}
			labels[parsed.Label] = struct{}{}
			question.Options = append(question.Options, parsed)
		}
		if question.Type == "score" && len(question.Options) > decideSystemoneMaxScoreLevels {
			return nil, fmt.Errorf("question %q has %d levels; the maximum is %d", question.Name, len(question.Options), decideSystemoneMaxScoreLevels)
		}
		if question.Type == "choice" && len(question.Options) > decideSystemoneMaxChoices {
			return nil, fmt.Errorf("question %q has %d choices; the maximum is %d", question.Name, len(question.Options), decideSystemoneMaxChoices)
		}
		questions = append(questions, question)
	}
	return questions, nil
}

// checkDecideSystemoneBox refuses a box question the provider could not answer: a box is
// located by its instructions alone, so they must name the target, and it takes no options.
func checkDecideSystemoneBox(name string, instructions, criteria any, options bool) error {
	meaningful := false
	switch value := instructions.(type) {
	case string:
		meaningful = strings.TrimSpace(value) != ""
	case map[string]any:
		meaningful = len(value) > 0
	case []any:
		meaningful = len(value) > 0
	}
	switch {
	case !meaningful:
		return fmt.Errorf("question %q is a box and needs instructions that name the target to locate", name)
	case criteria != nil || options:
		return fmt.Errorf("question %q is a box and takes no criteria, choices or levels", name)
	}
	return nil
}

func decideSystemoneError(callID string, status tools.Status, code, summary string, information ...tools.Information) string {
	return tools.ExecutionError(callID, status, code, summary, information...)
}
