package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// decideAnswer is one answer in a form both providers map to. Score is the
// probability-weighted 0-based level index, so it can fall between levels.
type decideAnswer struct {
	Type          string
	Probability   float64
	Choice        string
	Score         float64
	Confidence    float64
	Probabilities []decideProbability
	Refused       bool
	// HasNumber is set when the provider gave the number its type needs (probability
	// or score). A missing or null number is not a 0.
	HasNumber bool
	// Broken marks an answer that could not be decoded at all.
	Broken bool
}

type decideProbability struct {
	Label       string
	Probability float64
}

// decideHTTPError is a provider answer that was not 2xx.
type decideHTTPError struct {
	Status  int
	Message string
}

// decideTransportError keeps the original error, so a timeout or a cancel is still
// recognised, and hides the key from its text.
type decideTransportError struct {
	Err error
	Key string
}

func (e *decideTransportError) Error() string { return decideRedact(e.Err.Error(), e.Key) }
func (e *decideTransportError) Unwrap() error { return e.Err }

// decideRedact removes the API key from provider text, because some providers echo
// the key they rejected.
func decideRedact(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[redacted]")
}

func (e *decideHTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

// decideResponseError is a 2xx answer the tool cannot use: too large, not JSON, or
// without answers.
type decideResponseError struct{ Message string }

func (e *decideResponseError) Error() string { return e.Message }

// requestDecisions sends one request and returns the answers by question name.
func requestDecisions(ctx context.Context, client *http.Client, settings sessionfs.ExtensionDecideSettings, key string, input []decideInputPart, questions []decideQuestion) (map[string]decideAnswer, error) {
	var body any
	if settings.Backend == "openai" {
		body = openAIDecisionsRequest(settings.Model, input, questions)
	} else {
		body = openRouterDecisionsRequest(settings.Model, input, questions)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.BaseURL+"/decisions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, &decideTransportError{Err: err, Key: key}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, settings.MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > settings.MaxResponseBytes {
		return nil, &decideResponseError{fmt.Sprintf("response is larger than %d bytes", settings.MaxResponseBytes)}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, &decideHTTPError{Status: response.StatusCode, Message: decideRedact(decideErrorMessage(data), key)}
	}
	var answers map[string]decideAnswer
	if settings.Backend == "openai" {
		answers, err = parseOpenAIDecisions(data, questions)
	} else {
		answers, err = parseOpenRouterDecisions(data, questions)
	}
	if err != nil {
		return nil, &decideResponseError{err.Error()}
	}
	return answers, nil
}

// decideErrorMessage takes the provider's error text from either error shape and cuts
// it, so a long validation report stays small.
func decideErrorMessage(data []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	text := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Error) > 0 {
		var object struct {
			Message string `json:"message"`
		}
		var plain string
		switch {
		case json.Unmarshal(envelope.Error, &object) == nil && object.Message != "":
			text = object.Message
		case json.Unmarshal(envelope.Error, &plain) == nil && plain != "":
			text = plain
		}
	}
	text = oneLine(text)
	if cut, ok := cutUTF8(text, 400); ok {
		text = cut + "..."
	}
	return text
}

func decideRequestError(callID string, err error) string {
	var httpError *decideHTTPError
	var responseError *decideResponseError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return decideError(callID, tools.StatusError, "timeout", "The decisions request timed out")
	case errors.Is(err, context.Canceled):
		return decideError(callID, tools.StatusError, "cancelled", "The decisions request was cancelled")
	case errors.As(err, &responseError):
		return decideError(callID, tools.StatusError, "bad_response", "The provider's answer could not be used", tools.Information{Key: "reason", Value: oneLine(responseError.Message)})
	case errors.As(err, &httpError):
		code, status := "provider_error", tools.StatusError
		switch {
		case httpError.Status == http.StatusUnauthorized || httpError.Status == http.StatusForbidden:
			code = "unauthorized"
		case httpError.Status == http.StatusTooManyRequests:
			code = "rate_limited"
		case httpError.Status >= 400 && httpError.Status < 500:
			code, status = "request_rejected", tools.StatusInvalidInput
		}
		return decideError(callID, status, code, "The provider answered HTTP "+strconv.Itoa(httpError.Status), tools.Information{Key: "reason", Value: httpError.Message})
	default:
		return decideError(callID, tools.StatusError, "request_failed", "The decisions request failed", tools.Information{Key: "reason", Value: oneLine(err.Error())})
	}
}

// ---- OpenAI: POST /v1/decisions ----

// decideAllText joins the parts into one string when every part is text.
func decideAllText(input []decideInputPart) (string, bool) {
	texts := make([]string, 0, len(input))
	for _, part := range input {
		if part.JSON != nil || part.isImage() {
			return "", false
		}
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n\n"), true
}

// openAIDecisionsInput is a string when the input is all text, as in OpenAI's own
// examples, and otherwise one user message whose parts are input_text, a JSON value
// being its compact text, and input_image with the image as a data URL.
func openAIDecisionsInput(input []decideInputPart) any {
	if text, ok := decideAllText(input); ok {
		return text
	}
	parts := make([]map[string]any, 0, len(input))
	for _, part := range input {
		if part.isImage() {
			parts = append(parts, map[string]any{"type": "input_image", "image_url": part.Image})
			continue
		}
		text := part.Text
		if part.JSON != nil {
			text = string(part.JSON)
		}
		parts = append(parts, map[string]any{"type": "input_text", "text": text})
	}
	return []map[string]any{{"role": "user", "content": parts}}
}

// openRouterDecisionsState is a string when the input is all text, and otherwise the
// list of its parts, a text as a string and a JSON value as itself: the endpoint reads
// "a plain string, or a JSON object or array of related context".
func openRouterDecisionsState(input []decideInputPart) any {
	if text, ok := decideAllText(input); ok {
		return text
	}
	state := make([]any, 0, len(input))
	for _, part := range input {
		if part.isImage() {
			state = append(state, map[string]any{"type": "image_url", "image_url": map[string]any{"url": part.Image}})
		} else if part.JSON != nil {
			state = append(state, part.JSON)
		} else {
			state = append(state, part.Text)
		}
	}
	return state
}

func openAIDecisionsRequest(model string, input []decideInputPart, questions []decideQuestion) map[string]any {
	items := make([]map[string]any, 0, len(questions))
	for _, question := range questions {
		item := map[string]any{"type": question.Type, "name": question.Name, "instructions": question.Instructions}
		switch question.Type {
		case "choice":
			choices := make([]map[string]any, 0, len(question.Options))
			for _, option := range question.Options {
				choice := map[string]any{"value": option.Label}
				if option.Description != "" {
					choice["description"] = option.Description
				}
				choices = append(choices, choice)
			}
			item["choices"] = choices
		case "score":
			levels := make([]map[string]any, 0, len(question.Options))
			for _, option := range question.Options {
				level := map[string]any{"label": option.Label}
				if option.Description != "" {
					level["description"] = option.Description
				}
				levels = append(levels, level)
			}
			item["levels"] = levels
		}
		items = append(items, item)
	}
	return map[string]any{"model": model, "input": openAIDecisionsInput(input), "questions": items}
}

func parseOpenAIDecisions(data []byte, questions []decideQuestion) (map[string]decideAnswer, error) {
	var response struct {
		Answers []json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("the response is not valid JSON: %w", err)
	}
	if response.Answers == nil {
		return nil, errors.New("the response has no answers")
	}
	answers := make(map[string]decideAnswer, len(response.Answers))
	for index, raw := range response.Answers {
		var item struct {
			Type          string          `json:"type"`
			Name          string          `json:"name"`
			Probability   *float64        `json:"probability"`
			Choice        json.RawMessage `json:"choice"`
			Score         *float64        `json:"score"`
			Confidence    float64         `json:"confidence"`
			Probabilities []struct {
				Value       json.RawMessage `json:"value"`
				Label       string          `json:"label"`
				Probability float64         `json:"probability"`
			} `json:"probabilities"`
		}
		// A refusal may come without its name; the answers follow the question order.
		name := ""
		if index < len(questions) {
			name = questions[index].Name
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			// One odd field costs that answer only, not the others.
			if name != "" {
				answers[name] = decideAnswer{Broken: true}
			}
			continue
		}
		if item.Name != "" {
			name = item.Name
		}
		answer := decideAnswer{Type: item.Type, Confidence: item.Confidence}
		switch item.Type {
		case "refusal":
			answer.Refused = true
		case "predicate":
			if item.Probability != nil {
				answer.Probability, answer.HasNumber = *item.Probability, true
			}
		case "choice":
			answer.Choice = decideScalarText(item.Choice)
		case "score":
			if item.Score != nil {
				answer.Score, answer.HasNumber = *item.Score, true
			}
		}
		for _, probability := range item.Probabilities {
			label := probability.Label
			if label == "" {
				label = decideScalarText(probability.Value)
			}
			if label == "" {
				continue
			}
			answer.Probabilities = append(answer.Probabilities, decideProbability{Label: label, Probability: probability.Probability})
		}
		answers[name] = answer
	}
	return answers, nil
}

// decideScalarText renders a JSON string, number or bool without quotes.
func decideScalarText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return strings.TrimSpace(string(raw))
}

// ---- OpenRouter: POST /api/alpha/decisions ----

func openRouterDecisionsRequest(model string, input []decideInputPart, questions []decideQuestion) map[string]any {
	items := make(map[string]any, len(questions))
	for _, question := range questions {
		item := map[string]any{"instructions": question.Instructions}
		switch question.Type {
		case "predicate":
			item["type"] = "noul"
		case "choice":
			criteria := make(map[string]string, len(question.Options))
			for _, option := range question.Options {
				criteria[option.Label] = firstNonEmpty(option.Description, option.Label)
			}
			item["type"], item["criteria"] = "choice", criteria
		case "score":
			// OpenRouter takes the criteria in level order, and has no separate label.
			criteria := make([]string, 0, len(question.Options))
			for _, option := range question.Options {
				if option.Description == "" {
					criteria = append(criteria, option.Label)
				} else {
					criteria = append(criteria, option.Label+": "+option.Description)
				}
			}
			item["type"], item["criteria"] = "score", criteria
		}
		items[question.Name] = item
	}
	return map[string]any{"model": model, "state": openRouterDecisionsState(input), "questions": items}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseOpenRouterDecisions(data []byte, questions []decideQuestion) (map[string]decideAnswer, error) {
	var response struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("the response is not valid JSON: %w", err)
	}
	if response.Answers == nil {
		return nil, errors.New("the response has no answers")
	}
	answers := make(map[string]decideAnswer, len(response.Answers))
	for _, question := range questions {
		raw, found := response.Answers[question.Name]
		if !found {
			continue
		}
		var item struct {
			Type          string             `json:"type"`
			Noul          *float64           `json:"noul"`
			Choice        json.RawMessage    `json:"choice"`
			Score         *float64           `json:"score"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			answers[question.Name] = decideAnswer{Broken: true}
			continue
		}
		answer := decideAnswer{Confidence: item.Confidence}
		switch item.Type {
		case "noul":
			answer.Type = "predicate"
			if item.Noul != nil {
				answer.Probability, answer.HasNumber = *item.Noul, true
			}
		case "choice":
			answer.Type, answer.Choice = "choice", decideScalarText(item.Choice)
			for _, option := range question.Options {
				if probability, found := item.Probabilities[option.Label]; found {
					answer.Probabilities = append(answer.Probabilities, decideProbability{Label: option.Label, Probability: probability})
				}
			}
		case "score":
			answer.Type = "score"
			if item.Score != nil {
				answer.Score, answer.HasNumber = *item.Score, true
			}
			// The keys are the 0-based level indices; show the level labels instead.
			for index, option := range question.Options {
				if probability, found := item.Probabilities[strconv.Itoa(index)]; found {
					answer.Probabilities = append(answer.Probabilities, decideProbability{Label: option.Label, Probability: probability})
				}
			}
		case "refusal":
			answer.Type, answer.Refused = item.Type, true
		default:
			// Kept as it came, so renderDecideAnswers sees it does not fit the question.
			answer.Type = item.Type
		}
		answers[question.Name] = answer
	}
	return answers, nil
}

// ---- Result ----

// renderDecideAnswers writes the answers in question order as compact JSON. A question
// the provider did not answer is listed as missing, so the model never reads a gap
// as a negative.
func renderDecideAnswers(questions []decideQuestion, answers map[string]decideAnswer) (string, error) {
	var out bytes.Buffer
	out.WriteString(`{"answers":[`)
	for index, question := range questions {
		if index > 0 {
			out.WriteByte(',')
		}
		item := map[string]any{"name": question.Name, "type": question.Type}
		answer, found := answers[question.Name]
		switch {
		case !found:
			item["status"] = "missing"
		case answer.Refused:
			item["status"] = "refused"
		case !decideAnswerFits(question, answer):
			// Never show a number the provider did not give: a made-up 0 reads as "no".
			item["status"] = "unusable"
		default:
			switch question.Type {
			case "predicate":
				item["probability"] = roundDecide(answer.Probability)
			case "choice":
				item["choice"], item["confidence"] = answer.Choice, roundDecide(answer.Confidence)
			case "score":
				item["score"], item["confidence"] = roundDecide(answer.Score), roundDecide(answer.Confidence)
				if level := nearestDecideLevel(question, answer.Score); level != "" {
					item["level"] = level
				}
			}
			if len(answer.Probabilities) > 0 && question.Type != "predicate" {
				item["probabilities"] = orderedDecideProbabilities(answer.Probabilities)
			}
		}
		encoded, err := marshalDecideItem(item)
		if err != nil {
			return "", err
		}
		out.Write(encoded)
	}
	out.WriteString(`]}`)
	return out.String(), nil
}

// decideAnswerFits reports whether an answer is of its question's type and has the
// value that type needs: the number for a predicate or a score, and for a choice one
// of the options that were offered.
func decideAnswerFits(question decideQuestion, answer decideAnswer) bool {
	if answer.Broken || answer.Type != question.Type {
		return false
	}
	if question.Type != "choice" {
		return answer.HasNumber
	}
	for _, option := range question.Options {
		if option.Label == answer.Choice {
			return true
		}
	}
	return false
}

func nearestDecideLevel(question decideQuestion, score float64) string {
	if len(question.Options) == 0 {
		return ""
	}
	index := int(math.Round(score))
	if index < 0 {
		index = 0
	}
	if index >= len(question.Options) {
		index = len(question.Options) - 1
	}
	return question.Options[index].Label
}

func roundDecide(value float64) float64 { return math.Round(value*10000) / 10000 }

// orderedDecideProbabilities keeps the provider's order in a JSON object.
type orderedDecideProbabilities []decideProbability

func (p orderedDecideProbabilities) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for index, entry := range p {
		if index > 0 {
			out.WriteByte(',')
		}
		label, err := json.Marshal(entry.Label)
		if err != nil {
			return nil, err
		}
		out.Write(label)
		out.WriteByte(':')
		out.WriteString(strconv.FormatFloat(roundDecide(entry.Probability), 'f', -1, 64))
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// marshalDecideItem writes the answer fields in a fixed, readable order.
func marshalDecideItem(item map[string]any) ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	for _, key := range []string{"name", "type", "probability", "choice", "score", "level", "confidence", "probabilities", "status"} {
		value, found := item[key]
		if !found {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&out, "%q:", key)
		out.Write(encoded)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
