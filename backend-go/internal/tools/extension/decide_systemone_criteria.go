package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"jad/backend-go/internal/tools"
)

// decideSystemoneOriginal gives structured question fields as the JSON the model wrote, so
// key order and number text survive. It parses the raw call arguments at most once, and
// only when a question has a structured field. When the raw text is missing or does not
// describe the same value, the decoded value is encoded instead.
type decideSystemoneOriginal struct {
	arguments json.RawMessage
	loaded    bool
	questions []map[string]json.RawMessage
}

func (o *decideSystemoneOriginal) field(index int, name string, decoded any) (json.RawMessage, error) {
	if !o.loaded {
		o.loaded = true
		var envelope struct {
			Questions []map[string]json.RawMessage `json:"questions"`
		}
		if len(o.arguments) > 0 && json.Unmarshal(o.arguments, &envelope) == nil {
			o.questions = envelope.Questions
		}
	}
	if index < len(o.questions) {
		if raw, found := o.questions[index][name]; found {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			var again any
			if decoder.Decode(&again) == nil && reflect.DeepEqual(again, decoded) {
				return raw, nil
			}
		}
	}
	return tools.MarshalJSON(decoded)
}

// parseDecideSystemoneCriteria fills a question from its native criteria. Predicate
// criteria are kept as one object; choice criteria become options in declared key order and
// score criteria options in order, each keeping its value as given. Choice and score
// need an explicit type, since a bare criteria is read as a predicate's.
func parseDecideSystemoneCriteria(question *decideSystemoneQuestion, criteria any, index int, original *decideSystemoneOriginal) error {
	switch question.Type {
	case "predicate":
		object, ok := criteria.(map[string]any)
		if !ok {
			return fmt.Errorf("question %q is a predicate and its criteria must be an object with true and false; for a list set type score", question.Name)
		}
		for key, value := range object {
			if key != "true" && key != "false" {
				return fmt.Errorf("question %q is a predicate and its criteria take only true and false, not %q; for labelled criteria set type choice", question.Name, key)
			}
			if !decideSystemoneCriterionFits(value, true) {
				return fmt.Errorf("question %q: the criteria for %s must be text, an object, an array or null", question.Name, key)
			}
		}
		encoded, err := original.field(index, "criteria", criteria)
		if err != nil {
			return fmt.Errorf("question %q: the criteria cannot be encoded: %v", question.Name, err)
		}
		question.Criteria = encoded
	case "choice":
		object, ok := criteria.(map[string]any)
		if !ok {
			return fmt.Errorf("question %q is a choice and its criteria must be an object of label to meaning", question.Name)
		}
		if len(object) == 0 {
			return fmt.Errorf("question %q is a choice and its criteria are empty", question.Name)
		}
		if len(object) > decideSystemoneMaxChoices {
			return fmt.Errorf("question %q has %d choices; the maximum is %d", question.Name, len(object), decideSystemoneMaxChoices)
		}
		for key, value := range object {
			if key == "" {
				return fmt.Errorf("question %q has an option without a value", question.Name)
			}
			if !decideSystemoneCriterionFits(value, true) {
				return fmt.Errorf("question %q: the criteria for %q must be text, an object, an array or null", question.Name, key)
			}
		}
		encoded, err := original.field(index, "criteria", criteria)
		if err != nil {
			return fmt.Errorf("question %q: the criteria cannot be encoded: %v", question.Name, err)
		}
		options, err := decideSystemoneOrderedObject(encoded)
		if err != nil {
			return fmt.Errorf("question %q: the criteria cannot be read: %v", question.Name, err)
		}
		seen := make(map[string]struct{}, len(options))
		for _, option := range options {
			if _, duplicate := seen[option.Label]; duplicate {
				return fmt.Errorf("question %q lists %q twice", question.Name, option.Label)
			}
			seen[option.Label] = struct{}{}
		}
		question.Options, question.Native = options, true
	case "score":
		list, ok := criteria.([]any)
		if !ok {
			return fmt.Errorf("question %q is a score and its criteria must be a list, lowest first", question.Name)
		}
		if len(list) == 0 {
			return fmt.Errorf("question %q is a score and its criteria are empty", question.Name)
		}
		if len(list) > decideSystemoneMaxScoreLevels {
			return fmt.Errorf("question %q has %d levels; the maximum is %d", question.Name, len(list), decideSystemoneMaxScoreLevels)
		}
		for position, value := range list {
			if !decideSystemoneCriterionFits(value, false) {
				return fmt.Errorf("question %q: criterion %d must be text, an object or an array", question.Name, position+1)
			}
		}
		encoded, err := original.field(index, "criteria", criteria)
		if err != nil {
			return fmt.Errorf("question %q: the criteria cannot be encoded: %v", question.Name, err)
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(encoded, &entries); err != nil {
			return fmt.Errorf("question %q: the criteria cannot be read: %v", question.Name, err)
		}
		options := make([]decideSystemoneOption, len(entries))
		for position, entry := range entries {
			options[position] = decideSystemoneOption{Raw: entry}
		}
		question.Options, question.Native = options, true
	}
	return nil
}

// decideSystemoneCriterionFits reports whether a criterion value is text, an object or an
// array, or null when the protocol allows it.
func decideSystemoneCriterionFits(value any, nullAllowed bool) bool {
	switch value.(type) {
	case string, map[string]any, []any:
		return true
	case nil:
		return nullAllowed
	}
	return false
}

// decideSystemoneOrderedObject reads a JSON object in one pass, keeping key order and
// each value as written.
func decideSystemoneOrderedObject(raw json.RawMessage) ([]decideSystemoneOption, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("not an object")
	}
	var options []decideSystemoneOption
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		label, _ := key.(string)
		options = append(options, decideSystemoneOption{Label: label, Raw: value})
	}
	return options, nil
}

// decideSystemoneRawObject writes native choice criteria in declared order.
type decideSystemoneRawObject []decideSystemoneOption

func (object decideSystemoneRawObject) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for index, option := range object {
		if index > 0 {
			out.WriteByte(',')
		}
		label, err := tools.MarshalJSON(option.Label)
		if err != nil {
			return nil, err
		}
		out.Write(label)
		out.WriteByte(':')
		out.Write(option.Raw)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
