package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const recordsToolID = "native.records"

var recordsInputSchema = json.RawMessage(`{
  "type":"object",
  "required":["operation","type"],
  "additionalProperties":false,
  "properties":{
    "operation":{"type":"string","enum":["create","get","list","search","update","delete"],"description":"Create assigns and returns a ref for a new record. Use that exact returned ref for get/update/delete; never invent a ref or create a duplicate just to obtain one. List or search first if checking for an existing record. Delete removes a record for good."},
    "type":{"type":"string","minLength":1},
    "ref":{"type":"string","minLength":1,"description":"For get/update/delete, use the exact ref returned by create, list or search; never invent one. Create assigns and returns its ref."},
    "data":{"type":"object"},
    "query":{"type":"string","minLength":1,"description":"For search: words that must all appear, ignoring case, in the record's string or number values; keys are not searched."},
    "filter":{"type":"object","description":"Top-level or dotted-path exact-match filter for list and search; omitted values inherit from a persistent cursor."},
    "limit":{"type":"integer","minimum":1},
    "order":{"type":"string","enum":["newest","oldest"],"description":"List and search order; when omitted while continuing, the cursor order is used."},
    "cursor":{"type":"string","minLength":1,"description":"Cursor returned by list or search, given back to the same operation with the same type, query and filter; it continues even if the last record it returned was deleted since."},
    "if_match":{"type":"string","minLength":1,"description":"Optional current ETag for update or delete. On mismatch, the call is rejected and the current_etag is returned; omitted means an unconditional call."}
  },
  "allOf":[
    {"if":{"properties":{"operation":{"const":"create"}}},"then":{"properties":{"data":{}},"required":["data"]}},
    {"if":{"properties":{"operation":{"const":"get"}}},"then":{"properties":{"ref":{}},"required":["ref"]}},
    {"if":{"properties":{"operation":{"const":"search"}}},"then":{"properties":{"query":{}},"required":["query"]}},
    {"if":{"properties":{"operation":{"const":"update"}}},"then":{"properties":{"ref":{},"data":{}},"required":["ref","data"]}},
    {"if":{"properties":{"operation":{"const":"delete"}}},"then":{"properties":{"ref":{}},"required":["ref"]}}
  ]
}`)

const recordsToolDescription = "Store and retrieve bounded JSON-object records. `create` assigns and returns the record's ref; never invent one or create a duplicate just to obtain a ref. Reuse returned refs with `get`/`update`/`delete`, and list or search first when checking for an existing record. `search` finds records whose values contain all the words of `query`; `delete` removes a record that is no longer true or needed."

type recordsOperation string

const (
	recordsCreate recordsOperation = "create"
	recordsGet    recordsOperation = "get"
	recordsList   recordsOperation = "list"
	recordsUpdate recordsOperation = "update"
	recordsSearch recordsOperation = "search"
	recordsDelete recordsOperation = "delete"
)

type recordsToolContext struct {
	settings sessionfs.NativeRecordsSettings
	store    *RecordsStore
}

// NewRecordsTool binds configured policy to a persistent records store.
func NewRecordsTool(settings sessionfs.NativeRecordsSettings, store *RecordsStore) (tools.Tool, error) {
	validator, err := tools.CompileSchema(recordsInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native records schema: %w", err)
	}
	if store == nil {
		store = NewRecordsStore(settings.StorageDir)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          recordsToolID,
			Source:      "native",
			Name:        "records",
			Description: recordsToolDescription,
			InputSchema: append(json.RawMessage(nil), recordsInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"operation":"list","type":"workspace"}`),
		NormalizeArguments: normalizeRecordsArguments,
		Execute: func(_ context.Context, args map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeRecords(args, execution, recordsToolContext{settings: settings, store: store})
		},
	}, nil
}

func normalizeRecordsArguments(args map[string]any) map[string]any {
	normalized := make(map[string]any, len(args))
	for key, value := range args {
		normalized[key] = value
	}
	if integer, ok := recordsSafeInteger(normalized["limit"]); ok {
		if integer < 1 {
			integer = 1
		}
		normalized["limit"] = json.Number(fmt.Sprint(integer))
	}
	return normalized
}

func recordsSafeInteger(value any) (int64, bool) {
	const maxSafe = int64(9_007_199_254_740_991)
	switch number := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(number.String(), 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < -float64(maxSafe) || parsed > float64(maxSafe) || math.Trunc(parsed) != parsed {
			return 0, false
		}
		return int64(parsed), true
	case int:
		parsed := int64(number)
		return parsed, parsed >= -maxSafe && parsed <= maxSafe
	case int64:
		return number, number >= -maxSafe && number <= maxSafe
	case float64:
		if number < -float64(maxSafe) || number > float64(maxSafe) || number != float64(int64(number)) {
			return 0, false
		}
		return int64(number), true
	default:
		return 0, false
	}
}

func executeNativeRecords(args map[string]any, execution tools.ExecutionContext, toolContext recordsToolContext) (string, error) {
	callID := execution.CallID
	operation, okOperation := args["operation"].(string)
	recordType, okType := args["type"].(string)
	if !okOperation || !okType || !validRecordsOperation(recordsOperation(operation)) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_operation", "Records require a valid operation and type"), nil
	}
	settings := toolContext.settings
	store := toolContext.store
	if store == nil {
		return tools.ExecutionError(callID, tools.StatusError, "records_storage_error", "records storage is not available"), nil
	}

	switch recordsOperation(operation) {
	case recordsCreate:
		data, ok := args["data"].(map[string]any)
		if !ok || !recordsOnlyKeys(args, "operation", "type", "data") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_create", "Create requires only type and an object data payload"), nil
		}
		value := recordsArgumentData(execution.RawArguments, data)
		item, err := store.CreateValue(recordType, value, data, settings)
		if err != nil {
			return recordsExecutionError(callID, recordsCreate, recordType, "", err, store, settings), nil
		}
		return renderRecordsItem(callID, recordsCreate, item, "Record created")
	case recordsGet:
		ref, ok := args["ref"].(string)
		if !ok || !recordsOnlyKeys(args, "operation", "type", "ref") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_get", "Get requires only type and ref"), nil
		}
		item, err := store.Get(recordType, ref, settings)
		if err != nil {
			return recordsExecutionError(callID, recordsGet, recordType, ref, err, store, settings), nil
		}
		return renderRecordsItem(callID, recordsGet, item, "Record retrieved")
	case recordsUpdate:
		ref, okRef := args["ref"].(string)
		data, okData := args["data"].(map[string]any)
		if_match, ifMatchExists := args["if_match"]
		if !okRef || !okData || (ifMatchExists && func() bool { _, ok := if_match.(string); return !ok }()) || !recordsOnlyKeys(args, "operation", "type", "ref", "if_match", "data") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_update", "Update requires type, ref, and an object data payload; if_match is optional"), nil
		}
		ifMatch, _ := if_match.(string)
		value := recordsArgumentData(execution.RawArguments, data)
		item, err := store.UpdateValue(recordType, ref, ifMatch, value, data, settings)
		if err != nil {
			return recordsExecutionError(callID, recordsUpdate, recordType, ref, err, store, settings), nil
		}
		return renderRecordsItem(callID, recordsUpdate, item, "Record updated")
	case recordsDelete:
		ref, okRef := args["ref"].(string)
		ifMatchValue, ifMatchExists := args["if_match"]
		ifMatch, okIfMatch := ifMatchValue.(string)
		if !okRef || (ifMatchExists && !okIfMatch) || !recordsOnlyKeys(args, "operation", "type", "ref", "if_match") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_delete", "Delete requires type and ref; if_match is optional"), nil
		}
		item, err := store.Delete(recordType, ref, ifMatch, settings)
		if err != nil {
			return recordsExecutionError(callID, recordsDelete, recordType, ref, err, store, settings), nil
		}
		return renderRecordsItem(callID, recordsDelete, item, "Record deleted")
	case recordsList, recordsSearch:
		filter, okFilter := args["filter"].(map[string]any)
		if filter == nil && args["filter"] == nil {
			okFilter = true
		}
		query, okQuery := args["query"].(string)
		if recordsOperation(operation) == recordsList {
			if !okFilter || !recordsOnlyKeys(args, "operation", "type", "filter", "limit", "order", "cursor") {
				return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_list", "List accepts type, filter, limit, order, and cursor"), nil
			}
		} else if !okFilter || !okQuery || !recordsOnlyKeys(args, "operation", "type", "query", "filter", "limit", "order", "cursor") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_search", "Search requires type and query; it accepts filter, limit, order, and cursor"), nil
		}
		var requestedLimit int64
		if args["limit"] != nil {
			if parsed, ok := recordsSafeInteger(args["limit"]); ok {
				requestedLimit = parsed
			}
		}
		if requestedLimit == 0 {
			requestedLimit = settings.ListLimit
		}
		if requestedLimit < 1 {
			requestedLimit = 1
		}
		if requestedLimit > settings.MaxListLimit {
			requestedLimit = settings.MaxListLimit
		}
		var requestedFilter map[string]any
		if _, exists := args["filter"]; exists {
			requestedFilter = filter
		}
		var result RecordsListResult
		var err error
		summary := "listed"
		if recordsOperation(operation) == recordsList {
			result, err = store.List(recordType, requestedFilter, requestedLimit, args["order"], args["cursor"], settings)
		} else {
			summary = "found"
			result, err = store.Search(recordType, query, requestedFilter, requestedLimit, args["order"], args["cursor"], settings)
		}
		if err != nil {
			return recordsExecutionError(callID, recordsOperation(operation), recordType, "", err, store, settings), nil
		}
		text, err := recordsListContentText(result)
		if err != nil {
			return tools.ExecutionError(callID, tools.StatusError, "records_operation_failed", err.Error()), nil
		}
		information := []tools.Information{
			{Key: "operation", Value: operation},
			{Key: "type", Value: recordType},
			{Key: "count", Value: len(result.Items)},
		}
		if result.NextCursor != "" {
			information = append(information, tools.Information{Key: "has_more", Value: true})
		}
		output, err := tools.RenderToolResult(tools.ResultOptions{
			ID: callID, Status: tools.StatusOK,
			Summary:     fmt.Sprintf("%d record%s %s", len(result.Items), pluralRecordsSuffix(len(result.Items)), summary),
			Information: information,
			Content:     &tools.Content{Encoding: "utf8", Text: text},
		})
		return output, err
	default:
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_operation", "Records require a valid operation and type"), nil
	}
}

func validRecordsOperation(operation recordsOperation) bool {
	switch operation {
	case recordsCreate, recordsGet, recordsList, recordsSearch, recordsUpdate, recordsDelete:
		return true
	}
	return false
}

func recordsArgumentData(raw json.RawMessage, fallback map[string]any) *recordsJSONValue {
	if len(raw) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			if data, exists := fields["data"]; exists {
				if value, err := parseRecordsJSON(data); err == nil && value.kind == recordsObject {
					return value
				}
			}
		}
	}
	return recordsValueFromGo(fallback)
}

func recordsOnlyKeys(args map[string]any, allowed ...string) bool {
	keys := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		keys[key] = struct{}{}
	}
	for key := range args {
		if _, exists := keys[key]; !exists {
			return false
		}
	}
	return true
}

func recordsExecutionError(callID string, operation recordsOperation, recordType, ref string, err error, store *RecordsStore, settings sessionfs.NativeRecordsSettings) string {
	var conflict *RecordsConflictError
	if errors.As(err, &conflict) {
		return tools.ExecutionError(callID, tools.StatusError, "records_conflict", err.Error(),
			tools.Information{Key: "operation", Value: string(operation)},
			tools.Information{Key: "type", Value: recordType},
			tools.Information{Key: "ref", Value: ref},
			tools.Information{Key: "current_etag", Value: conflict.CurrentETag},
		)
	}
	var storeError *RecordsStoreError
	if errors.As(err, &storeError) {
		status := tools.StatusError
		if strings.Contains(storeError.Error(), "not found") {
			status = tools.StatusNotFound
		}
		return tools.ExecutionError(callID, status, "records_storage_error", storeError.Error(), recordsValidValues(storeError, recordType, store, settings)...)
	}
	return tools.ExecutionError(callID, tools.StatusError, "records_operation_failed", err.Error())
}

// recordsValidValues lists what the agent can use instead: the allowed types after a
// type error, the newest refs of the type after a ref error.
func recordsValidValues(storeError *RecordsStoreError, recordType string, store *RecordsStore, settings sessionfs.NativeRecordsSettings) []tools.Information {
	switch storeError.Kind {
	case recordsErrorType:
		if len(settings.AllowedTypes) > 0 {
			return []tools.Information{{Key: "available_types", Value: strings.Join(settings.AllowedTypes, ", ")}}
		}
	case recordsErrorRef:
		if refs := store.RecentRefs(recordType, settings.ListLimit); len(refs) > 0 {
			return []tools.Information{{Key: "available_refs", Value: strings.Join(refs, ", ")}}
		}
	}
	return nil
}

func renderRecordsItem(callID string, operation recordsOperation, item RecordsItem, summary string) (string, error) {
	information := []tools.Information{
		{Key: "operation", Value: string(operation)},
		{Key: "type", Value: strings.SplitN(item.Ref, "/", 2)[0]},
		{Key: "ref", Value: item.Ref},
		{Key: "etag", Value: item.ETag},
	}
	var content *tools.Content
	if operation == recordsGet {
		text, err := recordsJSONPretty(item.value)
		if err != nil {
			return "", err
		}
		text = strings.TrimSuffix(text, "\n")
		content = &tools.Content{Encoding: "utf8", Text: text}
	}
	return tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: summary, Information: information, Content: content})
}

func pluralRecordsSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

// RecordsStateContext is the configured record snapshot included after compaction.
type RecordsStateContext struct {
	Items             []RecordsStateContextItem    `json:"items"`
	Queries           []RecordsStateContextQuery   `json:"queries"`
	Summaries         []RecordsStateContextSummary `json:"summaries,omitempty"`
	HasMore           bool                         `json:"has_more"`
	OmittedBySnapshot int                          `json:"omitted_by_snapshot,omitempty"`
	OmittedQueries    int                          `json:"omitted_queries,omitempty"`
}

// RecordsStateContextSummary replaces the records of a group_by selector with a
// count per field value. Each group's Filter holds the selector filter plus the
// group value, so it is also the filter of a records list call that fetches it.
type RecordsStateContextSummary struct {
	Type          string                     `json:"type"`
	GroupBy       string                     `json:"group_by"`
	Groups        []RecordsStateContextGroup `json:"groups"`
	Ungrouped     int                        `json:"ungrouped,omitempty"`
	ExcludeAny    []map[string]any           `json:"exclude_any,omitempty"`
	AtLeast       bool                       `json:"at_least,omitempty"`
	OmittedGroups int                        `json:"omitted_groups,omitempty"`
}

type RecordsStateContextGroup struct {
	Filter map[string]any `json:"filter"`
	Count  int            `json:"count"`
}

func (summary RecordsStateContextSummary) entryCount() int {
	if summary.Ungrouped > 0 {
		return len(summary.Groups) + 1
	}
	return len(summary.Groups)
}

type RecordsStateContextItem struct {
	Type       string           `json:"type"`
	Ref        string           `json:"ref"`
	Filter     map[string]any   `json:"filter"`
	ExcludeAny []map[string]any `json:"exclude_any,omitempty"`
	Data       json.RawMessage  `json:"data"`
}
type RecordsStateContextQuery struct {
	Type       string           `json:"type"`
	Filter     map[string]any   `json:"filter"`
	ExcludeAny []map[string]any `json:"exclude_any,omitempty"`
	Returned   int              `json:"returned"`
	HasMore    bool             `json:"has_more"`
}

// CollectRecordsStateContext applies the configured selectors without creating
// cursors, then returns the bounded records and per-selector query metadata.
func CollectRecordsStateContext(store *RecordsStore, settings sessionfs.NativeRecordsSettings, selectors []sessionfs.NativeRecordsStateContextSelector) (*RecordsStateContext, error) {
	if store == nil {
		return nil, fmt.Errorf("records statecontext store is not available")
	}
	state := &RecordsStateContext{Items: make([]RecordsStateContextItem, 0), Queries: make([]RecordsStateContextQuery, 0, len(selectors))}
	for _, selector := range selectors {
		if selector.GroupBy != "" {
			summary, err := collectRecordsSummary(store, settings, selector)
			if err != nil {
				return nil, err
			}
			state.Summaries = append(state.Summaries, summary)
			state.HasMore = state.HasMore || summary.AtLeast
			continue
		}
		listed, err := store.list(selector.Type, selector.Filter, selector.Limit, "newest", nil, settings, recordsListOptions{CreateCursor: false, ExcludeAny: selector.ExcludeAny})
		if err != nil {
			return nil, err
		}
		for _, item := range listed.Items {
			data, err := recordsJSONCompact(item.value)
			if err != nil {
				return nil, err
			}
			entry := RecordsStateContextItem{Type: selector.Type, Ref: item.Ref, Filter: selector.Filter, Data: append(json.RawMessage(nil), data...)}
			if len(selector.ExcludeAny) > 0 {
				entry.ExcludeAny = selector.ExcludeAny
			}
			state.Items = append(state.Items, entry)
		}
		query := RecordsStateContextQuery{Type: selector.Type, Filter: selector.Filter, Returned: len(listed.Items), HasMore: listed.HasMore}
		if len(selector.ExcludeAny) > 0 {
			query.ExcludeAny = selector.ExcludeAny
		}
		state.Queries = append(state.Queries, query)
		state.HasMore = state.HasMore || listed.HasMore
	}
	return state, nil
}

func collectRecordsSummary(store *RecordsStore, settings sessionfs.NativeRecordsSettings, selector sessionfs.NativeRecordsStateContextSelector) (RecordsStateContextSummary, error) {
	grouped, err := store.GroupBy(selector.Type, selector.Filter, selector.ExcludeAny, selector.GroupBy, settings)
	if err != nil {
		return RecordsStateContextSummary{}, err
	}
	summary := RecordsStateContextSummary{Type: selector.Type, GroupBy: selector.GroupBy, Groups: make([]RecordsStateContextGroup, 0, len(grouped.Groups)), Ungrouped: grouped.Ungrouped, AtLeast: grouped.AtLeast}
	if len(selector.ExcludeAny) > 0 {
		summary.ExcludeAny = selector.ExcludeAny
	}
	for _, group := range grouped.Groups {
		filter := make(map[string]any, len(selector.Filter)+1)
		for key, value := range selector.Filter {
			filter[key] = value
		}
		filter[selector.GroupBy] = group.Value
		summary.Groups = append(summary.Groups, RecordsStateContextGroup{Filter: filter, Count: group.Count})
	}
	return summary, nil
}

func (state *RecordsStateContext) StateContextItemCount() int {
	if state == nil {
		return 0
	}
	count := len(state.Items)
	for _, summary := range state.Summaries {
		count += summary.entryCount()
	}
	return count
}
func (state *RecordsStateContext) TrimStateContextItem() bool {
	if state == nil {
		return false
	}
	if len(state.Items) > 0 {
		state.Items = state.Items[:len(state.Items)-1]
		state.OmittedBySnapshot++
		return true
	}
	for index := len(state.Summaries) - 1; index >= 0; index-- {
		summary := &state.Summaries[index]
		if len(summary.Groups) > 0 {
			summary.Groups = summary.Groups[:len(summary.Groups)-1]
		} else if summary.Ungrouped > 0 {
			summary.Ungrouped = 0
		} else {
			continue
		}
		summary.OmittedGroups++
		return true
	}
	return false
}
func (state *RecordsStateContext) StateContextQueryCount() int {
	if state == nil {
		return 0
	}
	return len(state.Queries)
}
func (state *RecordsStateContext) TrimStateContextQuery() bool {
	if state == nil || len(state.Queries) == 0 {
		return false
	}
	state.Queries = state.Queries[:len(state.Queries)-1]
	state.OmittedQueries++
	return true
}
