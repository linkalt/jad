package transcript

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// ResponseMetadataMode controls the provider response fields retained in the
// committed transcript record.
type ResponseMetadataMode string

const (
	ResponseMetadataMinimal ResponseMetadataMode = "minimal"
	ResponseMetadataFull    ResponseMetadataMode = "full"
)

type completionPlan struct {
	prefix    []byte
	suffix    []byte
	itemCount int
	// hasOutput reports that the completed response carried an output array; outputs
	// are its items in order, and order the staged output indexes in that order. They
	// are set by makeCompletionPlan and by ResponseTurn.CommitWithObserver.
	hasOutput   bool
	outputs     []completedItem
	order       []int64
	responseID  string
	sessionTurn int64
	sessionStep int64
}

func makeCompletionPlan(event []byte, stagedItemCount int, mode ResponseMetadataMode) (completionPlan, error) {
	if stagedItemCount < 0 {
		return completionPlan{}, errors.New("staged item count cannot be negative")
	}
	if mode != ResponseMetadataMinimal && mode != ResponseMetadataFull {
		return completionPlan{}, fmt.Errorf("unsupported response metadata mode %q", mode)
	}
	eventFields, err := objectFields(event)
	if err != nil {
		return completionPlan{}, errors.New("completed event must be a JSON object")
	}
	typeRaw, ok := fieldValue(eventFields, "type")
	if !ok {
		return completionPlan{}, errors.New("expected a response.completed event")
	}
	eventType, ok := stringValue(typeRaw)
	if !ok || eventType != "response.completed" {
		return completionPlan{}, errors.New("expected a response.completed event")
	}
	responseRaw, ok := fieldValue(eventFields, "response")
	if !ok {
		return completionPlan{}, errors.New("completed response must be an object")
	}
	responseFields, err := objectFields(responseRaw)
	if err != nil {
		return completionPlan{}, errors.New("completed response must be an object")
	}

	plan := completionPlan{suffix: []byte("]}}\n"), itemCount: stagedItemCount}
	if raw, exists := fieldValue(eventFields, "session_turn"); exists {
		value, valid := safeInteger(raw, true)
		if !valid {
			return completionPlan{}, errors.New("session_turn must be a positive safe integer")
		}
		plan.sessionTurn = value
	}
	if raw, exists := fieldValue(eventFields, "session_step"); exists {
		value, valid := safeInteger(raw, true)
		if !valid {
			return completionPlan{}, errors.New("session_step must be a positive safe integer")
		}
		plan.sessionStep = value
	}
	if raw, exists := fieldValue(responseFields, "id"); exists {
		if id, isString := stringValue(raw); isString && id != "" {
			plan.responseID = id
		}
	}

	if outputRaw, exists := fieldValue(responseFields, "output"); exists {
		var outputs []completedItem
		err := arrayValues(outputRaw, func(item []byte) error {
			identity, err := identityOfItem(item)
			if err != nil {
				return err
			}
			outputs = append(outputs, completedItem{raw: item, identity: identity})
			return nil
		})
		if err != nil {
			return completionPlan{}, fmt.Errorf("completed response.output must be an array of output items: %w", err)
		}
		if len(outputs) != stagedItemCount {
			return completionPlan{}, fmt.Errorf("staged item count %d does not match completed output count %d", stagedItemCount, len(outputs))
		}
		plan.itemCount = len(outputs)
		plan.hasOutput = true
		plan.outputs = outputs
	}

	metadataFields := make([]jsonField, 0, len(responseFields))
	for _, field := range responseFields {
		if field.key == "output" {
			continue
		}
		if field.key == "usage" {
			if compact, ok, err := compactUsage(field.value); err != nil {
				return completionPlan{}, fmt.Errorf("compact response usage: %w", err)
			} else if ok {
				metadataFields = append(metadataFields, jsonField{key: field.key, value: compact})
			}
			continue
		}
		if mode == ResponseMetadataMinimal && !minimalResponseField(field.key) {
			continue
		}
		canonical, err := canonicalJSON(field.value)
		if err != nil {
			return completionPlan{}, fmt.Errorf("canonicalize response field %q: %w", field.key, err)
		}
		metadataFields = append(metadataFields, jsonField{key: field.key, value: canonical})
	}

	var responsePrefix bytes.Buffer
	responsePrefix.WriteByte('{')
	for index, field := range metadataFields {
		if index > 0 {
			responsePrefix.WriteByte(',')
		}
		responsePrefix.Write(appendJSONString(nil, field.key))
		responsePrefix.WriteByte(':')
		responsePrefix.Write(field.value)
	}
	if len(metadataFields) > 0 {
		responsePrefix.WriteByte(',')
	}
	responsePrefix.WriteString(`"output":[`)

	var prefix bytes.Buffer
	prefix.WriteString(`{"type":"response.completed"`)
	if plan.sessionTurn > 0 {
		prefix.WriteString(`,"session_turn":`)
		prefix.WriteString(strconv.FormatInt(plan.sessionTurn, 10))
	}
	if plan.sessionStep > 0 {
		prefix.WriteString(`,"session_step":`)
		prefix.WriteString(strconv.FormatInt(plan.sessionStep, 10))
	}
	prefix.WriteString(`,"response":`)
	prefix.Write(responsePrefix.Bytes())
	plan.prefix = prefix.Bytes()
	return plan, nil
}

func fieldValue(fields []jsonField, key string) ([]byte, bool) {
	for _, field := range fields {
		if field.key == key {
			return field.value, true
		}
	}
	return nil, false
}

func validateOutputItemJSON(raw []byte) error {
	fields, err := objectFields(raw)
	if err != nil {
		return errors.New("output item must be an object")
	}
	return validateOutputItemType(fields)
}

// validateOutputItemType checks the parsed fields of an output item for a
// non-empty string type.
func validateOutputItemType(fields []jsonField) error {
	typeRaw, ok := fieldValue(fields, "type")
	if !ok {
		return errors.New("output item must contain a non-empty string type")
	}
	itemType, ok := stringValue(typeRaw)
	if !ok || itemType == "" {
		return errors.New("output item must contain a non-empty string type")
	}
	return nil
}

func minimalResponseField(key string) bool {
	switch key {
	case "id", "object", "status", "model", "created_at", "completed_at", "incomplete_details", "usage":
		return true
	default:
		return false
	}
}

func compactUsage(raw []byte) ([]byte, bool, error) {
	fields, err := objectFields(raw)
	if err != nil {
		return nil, false, nil
	}
	usageFields := make([]jsonField, 0, 4)
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if value, exists := fieldValue(fields, key); exists {
			if count, ok := safeInteger(value, false); ok {
				usageFields = append(usageFields, jsonField{key: key, value: []byte(strconv.FormatInt(count, 10))})
			}
		}
	}
	if detailsRaw, exists := fieldValue(fields, "input_tokens_details"); exists {
		if details, detailsErr := objectFields(detailsRaw); detailsErr == nil {
			if cachedRaw, cachedExists := fieldValue(details, "cached_tokens"); cachedExists {
				if cached, ok := safeInteger(cachedRaw, false); ok {
					cachedObject := encodeJSONObject([]jsonField{{key: "cached_tokens", value: []byte(strconv.FormatInt(cached, 10))}})
					usageFields = append(usageFields, jsonField{key: "input_tokens_details", value: cachedObject})
				}
			}
		}
	}
	if len(usageFields) == 0 {
		return nil, false, nil
	}
	return encodeJSONObject(usageFields), true, nil
}
