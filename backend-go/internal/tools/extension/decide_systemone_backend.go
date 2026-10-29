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

// decideSystemoneAnswer is one answer in a form both providers map to. Score is the
// probability-weighted 0-based level index, so it can fall between levels. Confidence
// is nil when the provider gave none, so a gap stays a gap.
type decideSystemoneAnswer struct {
	Type          string
	Probability   float64
	Choice        string
	Score         float64
	Confidence    *float64
	Probabilities []decideSystemoneProbability
	Refused       bool
	// HasNumber is set when the provider gave the number its type needs (probability
	// or score). A missing or null number is not a 0.
	HasNumber bool
	// Broken marks an answer that could not be decoded at all.
	Broken bool
}

type decideSystemoneProbability struct {
	Label       string
	Probability float64
}

// decideSystemoneHTTPError is a provider answer that was not 2xx.
type decideSystemoneHTTPError struct {
	Status  int
	Message string
}

// decideSystemoneTransportError keeps the original error, so a timeout or a cancel is still
// recognised, and hides the key from its text.
type decideSystemoneTransportError struct {
	Err error
	Key string
}

func (e *decideSystemoneTransportError) Error() string {
	return decideSystemoneRedact(e.Err.Error(), e.Key)
}
func (e *decideSystemoneTransportError) Unwrap() error { return e.Err }

// decideSystemoneRedact removes the API key from provider text, because some providers echo
// the key they rejected.
func decideSystemoneRedact(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[redacted]")
}

func (e *decideSystemoneHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// decideSystemoneResponseError is a 2xx answer the tool cannot use: too large, not JSON, or
// without answers.
type decideSystemoneResponseError struct{ Message string }

func (e *decideSystemoneResponseError) Error() string { return e.Message }

// requestDecisions sends one request and returns the answers by question name.
func requestDecisions(ctx context.Context, client *http.Client, settings sessionfs.ExtensionDecideSystemoneSettings, key string, input []decideSystemoneInputPart, questions []decideSystemoneQuestion) (map[string]decideSystemoneAnswer, error) {
	// The shared marshaler keeps <, > and & as they are, so the bytes max_input_bytes
	// counted for the JSON input parts are the bytes on the wire.
	payload, err := tools.MarshalJSON(systemoneRequest(settings.Model, input, questions))
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.BaseURL+settings.Path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, &decideSystemoneTransportError{Err: err, Key: key}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, settings.MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > settings.MaxResponseBytes {
		return nil, &decideSystemoneResponseError{fmt.Sprintf("response is larger than %d bytes", settings.MaxResponseBytes)}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, &decideSystemoneHTTPError{Status: response.StatusCode, Message: decideSystemoneErrorMessage(data, key)}
	}
	answers, err := parseSystemoneAnswers(data, questions)
	if err != nil {
		return nil, &decideSystemoneResponseError{err.Error()}
	}
	return answers, nil
}

// decideSystemoneErrorMessage takes the provider's error text from the error shapes it
// knows ({"error":{"message"}}, {"error":"text"}, and the 422 {"detail":[{"loc","msg"}]}
// of the System One API), redacts the key and cuts it, so a long validation
// report stays small. Redacting before the cut keeps a key that straddles the
// cut from leaving a prefix of itself in the reported text.
func decideSystemoneErrorMessage(data []byte, key string) string {
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	text := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &envelope) == nil {
		if len(envelope.Error) > 0 {
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
		} else if detail := decideSystemoneDetailText(envelope.Detail); detail != "" {
			text = detail
		}
	}
	text = decideSystemoneRedact(oneLine(text), key)
	if cut, ok := tools.CutText(text, 400); ok {
		text = cut + "..."
	}
	return text
}

// decideSystemoneDetailText renders a 422 detail: a list of {loc, msg} as
// "body.questions.q1.criteria: Field required; ...", or a plain string.
func decideSystemoneDetailText(raw json.RawMessage) string {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	var items []struct {
		Loc []json.RawMessage `json:"loc"`
		Msg string            `json:"msg"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	entries := make([]string, 0, len(items))
	for _, item := range items {
		if item.Msg == "" {
			continue
		}
		path := make([]string, 0, len(item.Loc))
		for _, part := range item.Loc {
			path = append(path, decideSystemoneScalarText(part))
		}
		if len(path) > 0 {
			entries = append(entries, strings.Join(path, ".")+": "+item.Msg)
		} else {
			entries = append(entries, item.Msg)
		}
	}
	return strings.Join(entries, "; ")
}

func decideSystemoneRequestError(callID string, err error) string {
	var httpError *decideSystemoneHTTPError
	var responseError *decideSystemoneResponseError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return decideSystemoneError(callID, tools.StatusError, "timeout", "The decisions request timed out")
	case errors.Is(err, context.Canceled):
		return decideSystemoneError(callID, tools.StatusError, "cancelled", "The decisions request was cancelled")
	case errors.As(err, &responseError):
		return decideSystemoneError(callID, tools.StatusError, "bad_response", "The provider's answer could not be used", tools.Information{Key: "reason", Value: oneLine(responseError.Message)})
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
		return decideSystemoneError(callID, status, code, "The provider answered HTTP "+strconv.Itoa(httpError.Status), tools.Information{Key: "reason", Value: httpError.Message})
	default:
		return decideSystemoneError(callID, tools.StatusError, "request_failed", "The decisions request failed", tools.Information{Key: "reason", Value: oneLine(err.Error())})
	}
}

// decideSystemoneAllText joins the parts into one string when every part is text.
func decideSystemoneAllText(input []decideSystemoneInputPart) (string, bool) {
	texts := make([]string, 0, len(input))
	for _, part := range input {
		if part.JSON != nil {
			return "", false
		}
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n\n"), true
}

// systemoneState is a string when the input is all text, and otherwise the
// list of its parts, a text as a string and a JSON value as itself: the endpoint reads
// "a plain string, or a JSON object or array of related context".
func systemoneState(input []decideSystemoneInputPart) any {
	if text, ok := decideSystemoneAllText(input); ok {
		return text
	}
	state := make([]any, 0, len(input))
	for _, part := range input {
		if part.JSON != nil {
			state = append(state, part.JSON)
		} else {
			state = append(state, part.Text)
		}
	}
	return state
}

// decideSystemoneScalarText renders a JSON string, number or bool without quotes.
func decideSystemoneScalarText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return strings.TrimSpace(string(raw))
}

// ---- Request and answers ----

func systemoneRequest(model string, input []decideSystemoneInputPart, questions []decideSystemoneQuestion) map[string]any {
	return map[string]any{"model": model, "state": systemoneState(input), "questions": systemoneQuestionItems(questions)}
}

// systemoneQuestionItems renders the questions as the provider receives them, so the
// byte budget and the request body measure the same text.
func systemoneQuestionItems(questions []decideSystemoneQuestion) map[string]any {
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
	return items
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseSystemoneAnswers(data []byte, questions []decideSystemoneQuestion) (map[string]decideSystemoneAnswer, error) {
	var response struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("the response is not valid JSON: %w", err)
	}
	if response.Answers == nil {
		return nil, errors.New("the response has no answers")
	}
	answers := make(map[string]decideSystemoneAnswer, len(response.Answers))
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
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			answers[question.Name] = decideSystemoneAnswer{Broken: true}
			continue
		}
		answer := decideSystemoneAnswer{Confidence: item.Confidence}
		switch item.Type {
		case "noul":
			answer.Type = "predicate"
			if item.Noul != nil {
				answer.Probability, answer.HasNumber = *item.Noul, true
			}
		case "choice":
			answer.Type, answer.Choice = "choice", decideSystemoneScalarText(item.Choice)
			for _, option := range question.Options {
				if probability, found := item.Probabilities[option.Label]; found {
					answer.Probabilities = append(answer.Probabilities, decideSystemoneProbability{Label: option.Label, Probability: probability})
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
					answer.Probabilities = append(answer.Probabilities, decideSystemoneProbability{Label: option.Label, Probability: probability})
				}
			}
		case "refusal":
			answer.Type, answer.Refused = item.Type, true
		default:
			// Kept as it came, so renderDecideSystemoneAnswers sees it does not fit the question.
			answer.Type = item.Type
		}
		answers[question.Name] = answer
	}
	return answers, nil
}

// ---- Result ----

// renderDecideSystemoneAnswers writes the answers in question order as compact JSON. A question
// the provider did not answer is listed as missing, so the model never reads a gap
// as a negative.
func renderDecideSystemoneAnswers(questions []decideSystemoneQuestion, answers map[string]decideSystemoneAnswer) (string, error) {
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
		case !decideSystemoneAnswerFits(question, answer):
			// Never show a number the provider did not give: a made-up 0 reads as "no".
			item["status"] = "unusable"
		default:
			switch question.Type {
			case "predicate":
				item["probability"] = roundDecideSystemone(answer.Probability)
			case "choice":
				item["choice"] = answer.Choice
			case "score":
				item["score"] = roundDecideSystemone(answer.Score)
				if level := nearestDecideSystemoneLevel(question, answer.Score); level != "" {
					item["level"] = level
				}
			}
			// A confidence the provider left out stays out: a made-up 0 would read
			// as "no confidence" instead of "not given".
			if question.Type != "predicate" && answer.Confidence != nil {
				item["confidence"] = roundDecideSystemone(*answer.Confidence)
			}
			if len(answer.Probabilities) > 0 && question.Type != "predicate" {
				item["probabilities"] = orderedDecideSystemoneProbabilities(answer.Probabilities)
			}
		}
		encoded, err := marshalDecideSystemoneItem(item)
		if err != nil {
			return "", err
		}
		out.Write(encoded)
	}
	out.WriteString(`]}`)
	return out.String(), nil
}

// decideSystemoneAnswerFits reports whether an answer is of its question's type and has the
// value that type needs: the number for a predicate or a score, and for a choice one
// of the options that were offered. A probability is 0 to 1, so a provider number
// outside that range is a bug and must not be reported as an answer; a score is a
// level index and may fall between the levels (nearestDecideSystemoneLevel clamps it).
func decideSystemoneAnswerFits(question decideSystemoneQuestion, answer decideSystemoneAnswer) bool {
	if answer.Broken || answer.Type != question.Type {
		return false
	}
	if answer.Confidence != nil && !decideSystemoneProbabilityFits(*answer.Confidence) {
		return false
	}
	if question.Type == "predicate" {
		return answer.HasNumber && decideSystemoneProbabilityFits(answer.Probability)
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

func nearestDecideSystemoneLevel(question decideSystemoneQuestion, score float64) string {
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

func roundDecideSystemone(value float64) float64 { return math.Round(value*10000) / 10000 }

// decideSystemoneProbabilityFits reports whether a number can be shown as a probability.
func decideSystemoneProbabilityFits(value float64) bool { return value >= 0 && value <= 1 }

// orderedDecideSystemoneProbabilities keeps the provider's order in a JSON object.
type orderedDecideSystemoneProbabilities []decideSystemoneProbability

func (p orderedDecideSystemoneProbabilities) MarshalJSON() ([]byte, error) {
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
		out.WriteString(strconv.FormatFloat(roundDecideSystemone(entry.Probability), 'f', -1, 64))
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// marshalDecideSystemoneItem writes the answer fields in a fixed, readable order.
func marshalDecideSystemoneItem(item map[string]any) ([]byte, error) {
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
