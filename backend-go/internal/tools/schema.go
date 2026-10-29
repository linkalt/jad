// Package tools contains reusable native-tool contracts and validation helpers.
package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const schemaLocation = "memory://native-tool/schema.json"

// SchemaValidator compiles a JSON Schema once and validates JSON instances
// without modifying them. Schemas without an explicit $schema use Draft-07,
// matching the TypeScript AJV configuration used for native tools.
type SchemaValidator struct {
	compiled *jsonschema.Schema
	document any
}

// ValidationIssue is the stable AJV-shaped subset surfaced in native tool
// validation errors. Params carries the keyword-specific details where the
// schema library exposes them.
type ValidationIssue struct {
	InstancePath string         `json:"instancePath"`
	SchemaPath   string         `json:"schemaPath"`
	Keyword      string         `json:"keyword"`
	Params       map[string]any `json:"params"`
	Message      string         `json:"message"`
}

// A failed validation reports at most this many issues and cuts every text in
// one issue to this many bytes. Validate still returns the full list, because
// the AJV-shaped output and its tests depend on it; only the model-facing
// report is bounded, so malformed arguments cannot be amplified into a large
// tool result that is persisted and re-sent on every round.
const (
	maxReportedValidationIssues   = 20
	maxReportedValidationTextSize = 200
)

// boundValidationIssues keeps the first maxReportedValidationIssues issues with
// their text cut, then says how many it left out, so the model still sees the
// shape of its mistake without the report growing with the argument list.
func boundValidationIssues(issues []ValidationIssue) []ValidationIssue {
	kept, omitted := issues, 0
	if len(kept) > maxReportedValidationIssues {
		kept = kept[:maxReportedValidationIssues]
		omitted = len(issues) - maxReportedValidationIssues
	}
	bounded := make([]ValidationIssue, 0, len(kept)+1)
	for _, issue := range kept {
		bounded = append(bounded, boundValidationIssue(issue))
	}
	if omitted > 0 {
		bounded = append(bounded, ValidationIssue{
			Params:  map[string]any{},
			Message: fmt.Sprintf("%d more issues omitted", omitted),
		})
	}
	return bounded
}

// boundValidationIssue copies an issue with its strings cut. The paths carry
// property names from the arguments, and an enum/const keyword carries values
// from the schema, so any of them can be long.
func boundValidationIssue(issue ValidationIssue) ValidationIssue {
	issue.InstancePath = boundValidationText(issue.InstancePath)
	issue.SchemaPath = boundValidationText(issue.SchemaPath)
	issue.Message = boundValidationText(issue.Message)
	if len(issue.Params) == 0 {
		return issue
	}
	params := make(map[string]any, len(issue.Params))
	for key, value := range issue.Params {
		params[key] = boundValidationParam(value)
	}
	issue.Params = params
	return issue
}

func boundValidationParam(value any) any {
	switch typed := value.(type) {
	case string:
		return boundValidationText(typed)
	case []string:
		bounded := make([]string, len(typed))
		for index, item := range typed {
			bounded[index] = boundValidationText(item)
		}
		return bounded
	case []any:
		bounded := make([]any, len(typed))
		for index, item := range typed {
			bounded[index] = boundValidationParam(item)
		}
		return bounded
	case map[string]any:
		bounded := make(map[string]any, len(typed))
		for key, item := range typed {
			bounded[key] = boundValidationParam(item)
		}
		return bounded
	default:
		return value
	}
}

func boundValidationText(text string) string {
	if cut, ok := CutText(text, maxReportedValidationTextSize); ok {
		return cut + "..."
	}
	return text
}

// CompileSchema validates and compiles a schema. External references are
// rejected; callers should provide self-contained tool schemas rather than
// allowing a schema to load arbitrary filesystem or network resources.
func CompileSchema(raw []byte) (*SchemaValidator, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("JSON Schema is required")
	}
	schema, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode JSON Schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft7)
	compiler.UseLoader(rejectExternalSchemaLoader{})
	if err := compiler.AddResource(schemaLocation, schema); err != nil {
		return nil, fmt.Errorf("add JSON Schema resource: %w", err)
	}
	compiled, err := compiler.Compile(schemaLocation)
	if err != nil {
		return nil, fmt.Errorf("compile JSON Schema: %w", err)
	}
	return &SchemaValidator{compiled: compiled, document: schema}, nil
}

// Validate reports an empty issue slice for valid instances. Invalid JSON is an
// input-decoding error, distinct from a schema validation failure.
func (v *SchemaValidator) Validate(raw []byte) ([]ValidationIssue, error) {
	if v == nil || v.compiled == nil {
		return nil, errors.New("JSON Schema validator is not initialized")
	}
	if !json.Valid(raw) {
		return nil, errors.New("instance must be valid JSON")
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode JSON instance: %w", err)
	}
	if err := v.compiled.Validate(instance); err != nil {
		var validationErr *jsonschema.ValidationError
		if !errors.As(err, &validationErr) {
			return nil, fmt.Errorf("validate JSON instance: %w", err)
		}
		issues := collectValidationIssues(validationErr, message.NewPrinter(language.English))
		issues = normalizeFalseSchemaArrayIssues(issues, v.document, instance)
		issues = normalizeTypeUnionOrder(issues, v.document)
		return addConditionalValidationIssues(issues), nil
	}
	return []ValidationIssue{}, nil
}

type rejectExternalSchemaLoader struct{}

func (rejectExternalSchemaLoader) Load(location string) (any, error) {
	return nil, fmt.Errorf("external JSON Schema reference %q is not allowed", location)
}

func collectValidationIssues(validationErr *jsonschema.ValidationError, printer *message.Printer) []ValidationIssue {
	if validationErr == nil {
		return nil
	}
	var issues []ValidationIssue
	appendCauses := func() {
		for _, cause := range validationErr.Causes {
			issues = append(issues, collectValidationIssues(cause, printer)...)
		}
	}
	switch validationErr.ErrorKind.(type) {
	case *kind.FalseSchema:
		issues = append(issues, ValidationIssue{
			InstancePath: jsonPointer(validationErr.InstanceLocation),
			SchemaPath:   schemaKeywordPath(validationErr.SchemaURL, nil),
			Keyword:      "false schema",
			Params:       map[string]any{},
			Message:      "boolean schema is false",
		})
	case *kind.Group, *kind.Schema, *kind.Reference, *kind.AllOf:
		// These are structural wrappers. Their causes contain the actionable
		// keyword errors, and AJV does not expose allOf wrapper errors.
		appendCauses()
	case *kind.AnyOf, *kind.OneOf:
		// AJV emits each failing branch's errors before the combinator error.
		appendCauses()
		keywordPath := validationErr.ErrorKind.KeywordPath()
		if len(keywordPath) > 0 {
			issues = append(issues, issuesForKind(validationErr, keywordPath, printer)...)
		}
	default:
		keywordPath := validationErr.ErrorKind.KeywordPath()
		if _, isNot := validationErr.ErrorKind.(*kind.Not); isNot {
			// The library reports `not` without a keyword path; AJV names it.
			keywordPath = []string{"not"}
		}
		if len(keywordPath) > 0 {
			issues = append(issues, issuesForKind(validationErr, keywordPath, printer)...)
		}
		appendCauses()
	}
	return issues
}

func normalizeFalseSchemaArrayIssues(issues []ValidationIssue, schemaDocument, instance any) []ValidationIssue {
	for index := range issues {
		issue := &issues[index]
		if issue.Keyword != "false schema" {
			continue
		}
		parentSchemaPath, keyword, ok := pointerParent(issue.SchemaPath)
		if !ok || (keyword != "items" && keyword != "additionalItems") {
			continue
		}
		parentSchemaValue, ok := lookupJSONPointer(schemaDocument, parentSchemaPath)
		if !ok {
			continue
		}
		parentSchema, ok := parentSchemaValue.(map[string]any)
		if !ok {
			continue
		}
		falseSchema, ok := parentSchema[keyword].(bool)
		if !ok || falseSchema {
			continue
		}

		limit := 0
		if keyword == "items" {
			prefixItems, _ := parentSchema["prefixItems"].([]any)
			limit = len(prefixItems)
		} else {
			tupleItems, ok := parentSchema["items"].([]any)
			if !ok {
				continue
			}
			limit = len(tupleItems)
		}
		arrayPath, ok := pointerParentInstance(issue.InstancePath)
		if !ok {
			continue
		}
		arrayValue, ok := lookupJSONPointer(instance, arrayPath)
		if !ok {
			continue
		}
		array, ok := arrayValue.([]any)
		if !ok || len(array) <= limit {
			continue
		}
		issue.InstancePath = arrayPath
		issue.Keyword = keyword
		issue.Params = map[string]any{"limit": limit}
		issue.Message = fmt.Sprintf("must NOT have more than %d items", limit)
	}
	return issues
}

// normalizeTypeUnionOrder restores the schema's own order for a type union. The
// compiled schema sorts its allowed types, but AJV reports them as written, and
// that order appears in both the message and the params the model reads.
func normalizeTypeUnionOrder(issues []ValidationIssue, schemaDocument any) []ValidationIssue {
	for index := range issues {
		issue := &issues[index]
		if issue.Keyword != "type" {
			continue
		}
		parentPath, _, ok := pointerParent(issue.SchemaPath)
		if !ok {
			continue
		}
		parent, ok := lookupJSONPointer(schemaDocument, strings.TrimPrefix(parentPath, "#"))
		if !ok {
			continue
		}
		schema, ok := parent.(map[string]any)
		if !ok {
			continue
		}
		declared, ok := schema["type"].([]any)
		if !ok {
			continue
		}
		ordered := make([]string, 0, len(declared))
		for _, entry := range declared {
			name, isString := entry.(string)
			if !isString {
				ordered = nil
				break
			}
			ordered = append(ordered, name)
		}
		if len(ordered) < 2 {
			continue
		}
		issue.Params = map[string]any{"type": ordered}
		issue.Message = "must be " + strings.Join(ordered, ",")
	}
	return issues
}

func pointerParent(pointer string) (string, string, bool) {
	if !strings.HasPrefix(pointer, "#/") {
		return "", "", false
	}
	pointer = strings.TrimPrefix(pointer, "#")
	separator := strings.LastIndexByte(pointer, '/')
	if separator < 0 || separator == len(pointer)-1 {
		return "", "", false
	}
	return pointer[:separator], decodeJSONPointerToken(pointer[separator+1:]), true
}

func pointerParentInstance(pointer string) (string, bool) {
	if !strings.HasPrefix(pointer, "/") {
		return "", false
	}
	separator := strings.LastIndexByte(pointer, '/')
	if separator < 0 {
		return "", false
	}
	return pointer[:separator], true
}

func lookupJSONPointer(root any, pointer string) (any, bool) {
	if pointer == "" {
		return root, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	current := root
	for _, encoded := range strings.Split(pointer[1:], "/") {
		token := decodeJSONPointerToken(encoded)
		switch value := current.(type) {
		case map[string]any:
			var exists bool
			current, exists = value[token]
			if !exists {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(value) {
				return nil, false
			}
			current = value[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func decodeJSONPointerToken(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
}

type conditionalErrorKey struct {
	schemaPath     string
	instancePath   string
	failingKeyword string
}

type conditionalSchemaLocation struct {
	schemaPath     string
	failingKeyword string
}

func addConditionalValidationIssues(issues []ValidationIssue) []ValidationIssue {
	lastIssueByKey := make(map[conditionalErrorKey]int)
	for index, issue := range issues {
		for _, conditional := range conditionalSchemaLocations(issue.SchemaPath) {
			key := conditionalErrorKey{
				schemaPath:     conditional.schemaPath,
				instancePath:   issue.InstancePath,
				failingKeyword: conditional.failingKeyword,
			}
			lastIssueByKey[key] = index
		}
	}
	if len(lastIssueByKey) == 0 {
		return issues
	}

	issuesAt := make(map[int][]ValidationIssue)
	for key, index := range lastIssueByKey {
		issuesAt[index] = append(issuesAt[index], ValidationIssue{
			InstancePath: key.instancePath,
			SchemaPath:   key.schemaPath,
			Keyword:      "if",
			Params:       map[string]any{"failingKeyword": key.failingKeyword},
			Message:      fmt.Sprintf("must match \"%s\" schema", key.failingKeyword),
		})
	}

	result := make([]ValidationIssue, 0, len(issues)+len(lastIssueByKey))
	for index, issue := range issues {
		result = append(result, issue)
		conditionalIssues := issuesAt[index]
		sort.Slice(conditionalIssues, func(i, j int) bool {
			if len(conditionalIssues[i].SchemaPath) != len(conditionalIssues[j].SchemaPath) {
				return len(conditionalIssues[i].SchemaPath) > len(conditionalIssues[j].SchemaPath)
			}
			return conditionalIssues[i].SchemaPath < conditionalIssues[j].SchemaPath
		})
		result = append(result, conditionalIssues...)
	}
	return result
}

func conditionalSchemaLocations(schemaPath string) []conditionalSchemaLocation {
	segments := strings.Split(schemaPath, "/")
	locations := make([]conditionalSchemaLocation, 0, 1)
	for index, segment := range segments {
		if segment != "then" && segment != "else" {
			continue
		}
		if index > 0 && conditionalSchemaMapKeyContainer(segments[index-1]) {
			continue
		}
		prefix := strings.Join(segments[:index], "/")
		locations = append(locations, conditionalSchemaLocation{
			schemaPath:     prefix + "/if",
			failingKeyword: segment,
		})
	}
	return locations
}

func conditionalSchemaMapKeyContainer(keyword string) bool {
	switch keyword {
	case "properties", "patternProperties", "definitions", "$defs", "dependentSchemas":
		return true
	default:
		return false
	}
}

func issuesForKind(validationErr *jsonschema.ValidationError, keywordPath []string, printer *message.Printer) []ValidationIssue {
	base := ValidationIssue{
		InstancePath: jsonPointer(validationErr.InstanceLocation),
		SchemaPath:   schemaKeywordPath(validationErr.SchemaURL, keywordPath),
		Keyword:      keywordPath[len(keywordPath)-1],
		Params:       map[string]any{},
		Message:      validationErr.ErrorKind.LocalizedString(printer),
	}

	switch typed := validationErr.ErrorKind.(type) {
	case *kind.Required:
		issues := make([]ValidationIssue, 0, len(typed.Missing))
		for _, missing := range typed.Missing {
			issue := base
			issue.Params = map[string]any{"missingProperty": missing}
			issue.Message = fmt.Sprintf("must have required property '%s'", missing)
			issues = append(issues, issue)
		}
		return issues
	case *kind.AdditionalProperties:
		issues := make([]ValidationIssue, 0, len(typed.Properties))
		for _, property := range typed.Properties {
			issue := base
			issue.Params = map[string]any{"additionalProperty": property}
			issue.Message = "must NOT have additional properties"
			issues = append(issues, issue)
		}
		return issues
	case *kind.Type:
		want := append([]string(nil), typed.Want...)
		if len(want) == 1 {
			base.Params = map[string]any{"type": want[0]}
		} else {
			base.Params = map[string]any{"type": want}
		}
		base.Message = "must be " + strings.Join(want, ",")
	case *kind.AnyOf:
		base.Message = "must match a schema in anyOf"
	case *kind.OneOf:
		if len(typed.Subschemas) == 0 {
			base.Params = map[string]any{"passingSchemas": nil}
		} else {
			base.Params = map[string]any{"passingSchemas": append([]int(nil), typed.Subschemas...)}
		}
		base.Message = "must match exactly one schema in oneOf"
	case *kind.Const:
		base.Params = map[string]any{"allowedValue": typed.Want}
		base.Message = "must be equal to constant"
	case *kind.Enum:
		base.Params = map[string]any{"allowedValues": typed.Want}
		base.Message = "must be equal to one of the allowed values"
	case *kind.MinLength:
		base.Params = map[string]any{"limit": typed.Want}
		base.Message = fmt.Sprintf("must NOT have fewer than %d characters", typed.Want)
	case *kind.MaxLength:
		base.Params = map[string]any{"limit": typed.Want}
		base.Message = fmt.Sprintf("must NOT have more than %d characters", typed.Want)
	case *kind.MinItems:
		base.Params = map[string]any{"limit": typed.Want}
		base.Message = fmt.Sprintf("must NOT have fewer than %d items", typed.Want)
	case *kind.MaxItems:
		base.Params = map[string]any{"limit": typed.Want}
		base.Message = fmt.Sprintf("must NOT have more than %d items", typed.Want)
	case *kind.MinProperties:
		base.Params = map[string]any{"limit": typed.Want}
		base.Message = fmt.Sprintf("must NOT have fewer than %d properties", typed.Want)
	case *kind.MaxProperties:
		base.Params = map[string]any{"limit": typed.Want}
		base.Message = fmt.Sprintf("must NOT have more than %d properties", typed.Want)
	case *kind.Minimum:
		setComparison(&base, ">=", typed.Want)
	case *kind.Maximum:
		setComparison(&base, "<=", typed.Want)
	case *kind.ExclusiveMinimum:
		setComparison(&base, ">", typed.Want)
	case *kind.ExclusiveMaximum:
		setComparison(&base, "<", typed.Want)
	case *kind.Not:
		base.Message = "must NOT be valid"
	case *kind.MultipleOf:
		multiple := ratNumber(typed.Want)
		base.Params = map[string]any{"multipleOf": multiple}
		base.Message = "must be multiple of " + formatNumber(multiple)
	case *kind.Pattern:
		base.Params = map[string]any{"pattern": typed.Want}
		base.Message = fmt.Sprintf("must match pattern %q", typed.Want)
	case *kind.UniqueItems:
		first, second := typed.Duplicates[0], typed.Duplicates[1]
		if first > second {
			first, second = second, first
		}
		base.Params = map[string]any{"i": second, "j": first}
		base.Message = fmt.Sprintf("must NOT have duplicate items (items ## %d and %d are identical)", first, second)
	case *kind.PropertyNames:
		base.Params = map[string]any{"propertyName": typed.Property}
		base.Message = "property name must be valid"
	case *kind.Contains:
		base.Params = map[string]any{"minContains": 1}
		base.Message = "must contain at least 1 valid item(s)"
	}
	return []ValidationIssue{base}
}

func schemaKeywordPath(schemaURL string, keywordPath []string) string {
	if fragmentStart := strings.IndexByte(schemaURL, '#'); fragmentStart >= 0 {
		return schemaURL[fragmentStart:] + jsonPointer(keywordPath)
	}
	return "#" + jsonPointer(keywordPath)
}

func jsonPointer(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, token := range tokens {
		builder.WriteByte('/')
		builder.WriteString(strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1"))
	}
	return builder.String()
}

// setComparison fills an AJV numeric-bound issue: its params carry the operator
// and the numeric limit, and the message spells both.
func setComparison(issue *ValidationIssue, operator string, limit *big.Rat) {
	number := ratNumber(limit)
	issue.Params = map[string]any{"comparison": operator, "limit": number}
	issue.Message = "must be " + operator + " " + formatNumber(number)
}

func ratNumber(value *big.Rat) float64 {
	number, _ := value.Float64()
	return number
}

// formatNumber renders a number the way JavaScript's String(number) does for
// the magnitudes schemas use: no exponent or trailing zeros for ordinary values.
func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
