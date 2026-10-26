package extension

import (
	"bytes"
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

const decideSystemoneDescription = "Ask a fast classifier typed questions about some text or JSON. predicate: probability 0-1 that a condition holds. choice: the best of your choices, with confidence. score: a rating against ordered levels, lowest first. Use it for yes/no checks, routing and severity, not for open-ended answers."

var DecideSystemoneInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "input":{
      "type":"array",
      "minItems":1,
      "items":{"anyOf":[
        {"type":"string","minLength":1},
        {"type":"object","properties":{"type":{"type":"string","enum":["text"]},"text":{"type":"string","minLength":1}},"required":["type","text"],"additionalProperties":false},
        {"type":"object","properties":{"type":{"type":"string","enum":["json"]},"value":{}},"required":["type","value"],"additionalProperties":false},
        {"type":"object","properties":{"type":{"type":"string","enum":["image"]},"path":{"type":"string","minLength":1,"description":"A file in the workspace, or a stored-image://<name> reference."},"data":{"type":"string","minLength":1,"description":"The image as base64 or a data URL."}},"required":["type"],"additionalProperties":false}
      ]},
      "description":"What to evaluate, in order: a string or {type:text,text}, {type:json,value} for structured data, or {type:image,path} for a png, jpeg, webp or gif (give exactly one of path and data; prefer path). Every question is asked about all of it."
    },
    "questions":{
      "type":"array",
      "minItems":1,
      "items":{
        "type":"object",
        "properties":{
          "name":{"type":"string","minLength":1,"description":"Label of the answer. Defaults to q1, q2, ..."},
          "type":{"type":"string","enum":["predicate","choice","score"],"description":"Inferred when omitted: choices gives choice, levels gives score, otherwise predicate."},
          "instructions":{"type":"string","minLength":1,"description":"The question, with observable criteria."},
          "choices":{"type":"array","minItems":2,"items":{"anyOf":[{"type":"string","minLength":1},{"type":"object","properties":{"value":{"type":"string","minLength":1},"description":{"type":"string"}},"required":["value"],"additionalProperties":false}]},"description":"Options for a choice question: a value, or a value with when it applies."},
          "levels":{"type":"array","minItems":2,"items":{"anyOf":[{"type":"string","minLength":1},{"type":"object","properties":{"label":{"type":"string","minLength":1},"description":{"type":"string"}},"required":["label"],"additionalProperties":false}]},"description":"Ordered levels for a score question, lowest first: a label, or a label with its criteria."}
        },
        "required":["instructions"],
        "additionalProperties":false
      }
    }
  },
  "required":["input","questions"],
  "additionalProperties":false
}`)

// decideSystemoneQuestion is one question after normalization.
type decideSystemoneQuestion struct {
	Name         string
	Type         string // predicate, choice or score
	Instructions string
	Options      []decideSystemoneOption // choices, or levels in order
}

// decideSystemoneOption is a choice (Label is its value) or a score level.
type decideSystemoneOption struct{ Label, Description string }

// decideSystemoneInputPart is one part of the input: text, a JSON value when JSON is set, or an
// image when ImagePath or ImageData is set. Image is the data URL sent, filled in by
// loadDecideSystemoneImages.
type decideSystemoneInputPart struct {
	Text      string
	JSON      json.RawMessage
	ImagePath string
	ImageData string
	Image     string
}

func (p decideSystemoneInputPart) isImage() bool { return p.ImagePath != "" || p.ImageData != "" }

// size is the part's text bytes as sent: the text, or the compact JSON. An image is
// limited by max_image_bytes and max_images instead.
func (p decideSystemoneInputPart) size() int {
	switch {
	case p.isImage():
		return 0
	case p.JSON != nil:
		return len(p.JSON)
	}
	return len(p.Text)
}

// decideSystemoneEnvironment reads the API key at call time, so a key set after the server
// started is used and none is kept in the tool.
type decideSystemoneEnvironment func(name string) string

// NewDecideSystemoneTool creates extension.decide_systemone from its normalized settings.
func NewDecideSystemoneTool(settings sessionfs.ExtensionDecideSystemoneSettings, images DecideSystemoneImages) (tools.Tool, error) {
	return newDecideSystemoneTool(settings, images, os.Getenv, newDecideSystemoneHTTPClient())
}

// newDecideSystemoneHTTPClient never follows a redirect: the request carries the API key, and
// a provider has no reason to send this endpoint elsewhere.
func newDecideSystemoneHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func newDecideSystemoneTool(settings sessionfs.ExtensionDecideSystemoneSettings, images DecideSystemoneImages, environment decideSystemoneEnvironment, client *http.Client) (tools.Tool, error) {
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
			return executeDecideSystemone(ctx, arguments, settings, images, environment, client, execution), nil
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

func executeDecideSystemone(ctx context.Context, arguments map[string]any, settings sessionfs.ExtensionDecideSystemoneSettings, images DecideSystemoneImages, environment decideSystemoneEnvironment, client *http.Client, execution tools.ExecutionContext) string {
	callID := execution.CallID
	input, err := parseDecideSystemoneInput(arguments["input"])
	if err != nil {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "invalid_input", err.Error())
	}
	size := 0
	for _, part := range input {
		size += part.size()
	}
	if int64(size) > settings.MaxInputBytes {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "input_too_large", fmt.Sprintf("Input is %d bytes; the limit is %d", size, settings.MaxInputBytes))
	}
	questions, err := parseDecideSystemoneQuestions(arguments["questions"])
	if err != nil {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "invalid_question", err.Error())
	}
	if int64(len(questions)) > settings.MaxQuestions {
		return decideSystemoneError(callID, tools.StatusInvalidInput, "too_many_questions", fmt.Sprintf("%d questions; the limit is %d", len(questions), settings.MaxQuestions))
	}
	if failure := loadDecideSystemoneImages(input, images, settings, execution); failure != "" {
		return failure
	}
	key := strings.TrimSpace(environment(settings.APIKeyEnv))
	if key == "" {
		return decideSystemoneError(callID, tools.StatusError, "api_key_missing", "Environment variable "+settings.APIKeyEnv+" is not set")
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutMS)*time.Millisecond)
	defer cancel()
	answers, err := requestDecisions(ctx, client, settings, key, input, questions)
	if err != nil {
		return decideSystemoneRequestError(callID, err)
	}
	text, err := renderDecideSystemoneAnswers(questions, answers)
	if err != nil {
		return decideSystemoneError(callID, tools.StatusError, "bad_response", err.Error())
	}
	noun := "questions"
	if len(questions) == 1 {
		noun = "question"
	}
	result, err := tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("Answered %d %s", len(questions), noun),
		Information: []tools.Information{{Key: "model", Value: settings.Model}},
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
			if kind == "image" {
				part.ImagePath, _ = value["path"].(string)
				part.ImageData, _ = value["data"].(string)
				part.ImagePath, part.ImageData = strings.TrimSpace(part.ImagePath), strings.TrimSpace(part.ImageData)
				if (part.ImagePath == "") == (part.ImageData == "") {
					return nil, fmt.Errorf("input part %d: an image needs exactly one of path and data", index+1)
				}
				parts = append(parts, part)
				continue
			}
			if kind == "json" {
				encoded, err := marshalDecideSystemoneJSON(value["value"])
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

// marshalDecideSystemoneJSON encodes compactly and leaves <, > and & as they are, which a
// reader of the text finds easier than \u003c.
func marshalDecideSystemoneJSON(value any) (json.RawMessage, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(out.Bytes(), "\n")), nil
}

// parseDecideSystemoneQuestions turns the validated arguments into questions: it infers a
// missing type from choices or levels, names the unnamed ones q1, q2, ..., and
// refuses what the provider would refuse (a duplicate name, options that do not fit
// the type), so a mistake costs no request.
func parseDecideSystemoneQuestions(raw any) ([]decideSystemoneQuestion, error) {
	items, _ := raw.([]any)
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
		question.Instructions, _ = fields["instructions"].(string)
		choices, hasChoices := fields["choices"].([]any)
		levels, hasLevels := fields["levels"].([]any)
		if hasChoices && hasLevels {
			return nil, fmt.Errorf("question %q has both choices and levels", question.Name)
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
		case question.Type == "predicate" && (hasChoices || hasLevels):
			return nil, fmt.Errorf("question %q is a predicate and takes no choices or levels", question.Name)
		case question.Type == "choice" && hasLevels:
			return nil, fmt.Errorf("question %q is a choice and cannot have levels; use choices, or type score", question.Name)
		case question.Type == "score" && hasChoices:
			return nil, fmt.Errorf("question %q is a score and cannot have choices; use levels, or type choice", question.Name)
		case question.Type == "choice" && !hasChoices:
			return nil, fmt.Errorf("question %q is a choice and needs choices", question.Name)
		case question.Type == "score" && !hasLevels:
			return nil, fmt.Errorf("question %q is a score and needs levels", question.Name)
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
		questions = append(questions, question)
	}
	return questions, nil
}

func decideSystemoneError(callID string, status tools.Status, code, summary string, information ...tools.Information) string {
	return tools.ExecutionError(callID, status, code, summary, information...)
}
