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

const memoryToolID = "native.memory"

var memoryInputSchema = json.RawMessage(`{
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

const memoryToolDescription = "Store and retrieve bounded JSON-object records. `create` assigns and returns the record's ref; never invent one or create a duplicate just to obtain a ref. Reuse returned refs with `get`/`update`/`delete`, and list or search first when checking for an existing record. `search` finds records whose values contain all the words of `query`; `delete` removes a record that is no longer true or needed."

type memoryOperation string

const (
	memoryCreate memoryOperation = "create"
	memoryGet    memoryOperation = "get"
	memoryList   memoryOperation = "list"
	memoryUpdate memoryOperation = "update"
	memorySearch memoryOperation = "search"
	memoryDelete memoryOperation = "delete"
)

type memoryToolContext struct {
	settings sessionfs.NativeMemorySettings
	store    *MemoryStore
}

// NewMemoryTool binds configured policy to a persistent memory store.
func NewMemoryTool(settings sessionfs.NativeMemorySettings, store *MemoryStore) (tools.Tool, error) {
	validator, err := tools.CompileSchema(memoryInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native memory schema: %w", err)
	}
	if store == nil {
		store = NewMemoryStore(settings.StorageDir)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          memoryToolID,
			Source:      "native",
			Name:        "memory",
			Description: memoryToolDescription,
			InputSchema: append(json.RawMessage(nil), memoryInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"operation":"list","type":"workspace"}`),
		NormalizeArguments: normalizeMemoryArguments,
		Execute: func(_ context.Context, args map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeMemory(args, execution, memoryToolContext{settings: settings, store: store})
		},
	}, nil
}

func normalizeMemoryArguments(args map[string]any) map[string]any {
	normalized := make(map[string]any, len(args))
	for key, value := range args {
		normalized[key] = value
	}
	if integer, ok := memorySafeInteger(normalized["limit"]); ok {
		if integer < 1 {
			integer = 1
		}
		normalized["limit"] = json.Number(fmt.Sprint(integer))
	}
	return normalized
}

func memorySafeInteger(value any) (int64, bool) {
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

func executeNativeMemory(args map[string]any, execution tools.ExecutionContext, toolContext memoryToolContext) (string, error) {
	callID := execution.CallID
	operation, okOperation := args["operation"].(string)
	recordType, okType := args["type"].(string)
	if !okOperation || !okType || !validMemoryOperation(memoryOperation(operation)) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_operation", "Memory requires a valid operation and type"), nil
	}
	settings := toolContext.settings
	store := toolContext.store
	if store == nil {
		return tools.ExecutionError(callID, tools.StatusError, "memory_storage_error", "memory storage is not available"), nil
	}

	switch memoryOperation(operation) {
	case memoryCreate:
		data, ok := args["data"].(map[string]any)
		if !ok || !memoryOnlyKeys(args, "operation", "type", "data") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_create", "Create requires only type and an object data payload"), nil
		}
		value := memoryArgumentData(execution.RawArguments, data)
		item, err := store.CreateValue(recordType, value, data, settings)
		if err != nil {
			return memoryExecutionError(callID, memoryCreate, recordType, "", err, store, settings), nil
		}
		return renderMemoryItem(callID, memoryCreate, item, "Memory record created")
	case memoryGet:
		ref, ok := args["ref"].(string)
		if !ok || !memoryOnlyKeys(args, "operation", "type", "ref") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_get", "Get requires only type and ref"), nil
		}
		item, err := store.Get(recordType, ref, settings)
		if err != nil {
			return memoryExecutionError(callID, memoryGet, recordType, ref, err, store, settings), nil
		}
		return renderMemoryItem(callID, memoryGet, item, "Memory record retrieved")
	case memoryUpdate:
		ref, okRef := args["ref"].(string)
		data, okData := args["data"].(map[string]any)
		if_match, ifMatchExists := args["if_match"]
		if !okRef || !okData || (ifMatchExists && func() bool { _, ok := if_match.(string); return !ok }()) || !memoryOnlyKeys(args, "operation", "type", "ref", "if_match", "data") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_update", "Update requires type, ref, and an object data payload; if_match is optional"), nil
		}
		ifMatch, _ := if_match.(string)
		value := memoryArgumentData(execution.RawArguments, data)
		item, err := store.UpdateValue(recordType, ref, ifMatch, value, data, settings)
		if err != nil {
			return memoryExecutionError(callID, memoryUpdate, recordType, ref, err, store, settings), nil
		}
		return renderMemoryItem(callID, memoryUpdate, item, "Memory record updated")
	case memoryDelete:
		ref, okRef := args["ref"].(string)
		ifMatchValue, ifMatchExists := args["if_match"]
		ifMatch, okIfMatch := ifMatchValue.(string)
		if !okRef || (ifMatchExists && !okIfMatch) || !memoryOnlyKeys(args, "operation", "type", "ref", "if_match") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_delete", "Delete requires type and ref; if_match is optional"), nil
		}
		item, err := store.Delete(recordType, ref, ifMatch, settings)
		if err != nil {
			return memoryExecutionError(callID, memoryDelete, recordType, ref, err, store, settings), nil
		}
		return renderMemoryItem(callID, memoryDelete, item, "Memory record deleted")
	case memoryList, memorySearch:
		filter, okFilter := args["filter"].(map[string]any)
		if filter == nil && args["filter"] == nil {
			okFilter = true
		}
		query, okQuery := args["query"].(string)
		if memoryOperation(operation) == memoryList {
			if !okFilter || !memoryOnlyKeys(args, "operation", "type", "filter", "limit", "order", "cursor") {
				return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_list", "List accepts type, filter, limit, order, and cursor"), nil
			}
		} else if !okFilter || !okQuery || !memoryOnlyKeys(args, "operation", "type", "query", "filter", "limit", "order", "cursor") {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_search", "Search requires type and query; it accepts filter, limit, order, and cursor"), nil
		}
		var requestedLimit int64
		if args["limit"] != nil {
			if parsed, ok := memorySafeInteger(args["limit"]); ok {
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
		var result MemoryListResult
		var err error
		summary := "listed"
		if memoryOperation(operation) == memoryList {
			result, err = store.List(recordType, requestedFilter, requestedLimit, args["order"], args["cursor"], settings)
		} else {
			summary = "found"
			result, err = store.Search(recordType, query, requestedFilter, requestedLimit, args["order"], args["cursor"], settings)
		}
		if err != nil {
			return memoryExecutionError(callID, memoryOperation(operation), recordType, "", err, store, settings), nil
		}
		text, err := memoryListContentText(result)
		if err != nil {
			return tools.ExecutionError(callID, tools.StatusError, "memory_operation_failed", err.Error()), nil
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
			Summary:     fmt.Sprintf("%d memory record%s %s", len(result.Items), pluralMemorySuffix(len(result.Items)), summary),
			Information: information,
			Content:     &tools.Content{Encoding: "utf8", Text: text},
		})
		return output, err
	default:
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_operation", "Memory requires a valid operation and type"), nil
	}
}

func validMemoryOperation(operation memoryOperation) bool {
	switch operation {
	case memoryCreate, memoryGet, memoryList, memorySearch, memoryUpdate, memoryDelete:
		return true
	}
	return false
}

func memoryArgumentData(raw json.RawMessage, fallback map[string]any) *memoryJSONValue {
	if len(raw) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			if data, exists := fields["data"]; exists {
				if value, err := parseMemoryJSON(data); err == nil && value.kind == memoryObject {
					return value
				}
			}
		}
	}
	return memoryValueFromGo(fallback)
}

func memoryOnlyKeys(args map[string]any, allowed ...string) bool {
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

func memoryExecutionError(callID string, operation memoryOperation, recordType, ref string, err error, store *MemoryStore, settings sessionfs.NativeMemorySettings) string {
	var conflict *MemoryConflictError
	if errors.As(err, &conflict) {
		return tools.ExecutionError(callID, tools.StatusError, "memory_conflict", err.Error(),
			tools.Information{Key: "operation", Value: string(operation)},
			tools.Information{Key: "type", Value: recordType},
			tools.Information{Key: "ref", Value: ref},
			tools.Information{Key: "current_etag", Value: conflict.CurrentETag},
		)
	}
	var storeError *MemoryStoreError
	if errors.As(err, &storeError) {
		status := tools.StatusError
		if strings.Contains(storeError.Error(), "not found") {
			status = tools.StatusNotFound
		}
		return tools.ExecutionError(callID, status, "memory_storage_error", storeError.Error(), memoryValidValues(storeError, recordType, store, settings)...)
	}
	return tools.ExecutionError(callID, tools.StatusError, "memory_operation_failed", err.Error())
}

// memoryValidValues lists what the agent can use instead: the allowed types after a
// type error, the newest refs of the type after a ref error.
func memoryValidValues(storeError *MemoryStoreError, recordType string, store *MemoryStore, settings sessionfs.NativeMemorySettings) []tools.Information {
	switch storeError.Kind {
	case memoryErrorType:
		if len(settings.AllowedTypes) > 0 {
			return []tools.Information{{Key: "available_types", Value: strings.Join(settings.AllowedTypes, ", ")}}
		}
	case memoryErrorRef:
		if refs := store.RecentRefs(recordType, settings.ListLimit); len(refs) > 0 {
			return []tools.Information{{Key: "available_refs", Value: strings.Join(refs, ", ")}}
		}
	}
	return nil
}

func renderMemoryItem(callID string, operation memoryOperation, item MemoryItem, summary string) (string, error) {
	information := []tools.Information{
		{Key: "operation", Value: string(operation)},
		{Key: "type", Value: strings.SplitN(item.Ref, "/", 2)[0]},
		{Key: "ref", Value: item.Ref},
		{Key: "etag", Value: item.ETag},
	}
	var content *tools.Content
	if operation == memoryGet {
		text, err := memoryJSONPretty(item.value)
		if err != nil {
			return "", err
		}
		text = strings.TrimSuffix(text, "\n")
		content = &tools.Content{Encoding: "utf8", Text: text}
	}
	return tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: summary, Information: information, Content: content})
}

func pluralMemorySuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

// MemoryStateContext is the configured record snapshot included after compaction.
type MemoryStateContext struct {
	Items             []MemoryStateContextItem    `json:"items"`
	Queries           []MemoryStateContextQuery   `json:"queries"`
	Summaries         []MemoryStateContextSummary `json:"summaries,omitempty"`
	HasMore           bool                        `json:"has_more"`
	OmittedBySnapshot int                         `json:"omitted_by_snapshot,omitempty"`
	OmittedQueries    int                         `json:"omitted_queries,omitempty"`
}

// MemoryStateContextSummary replaces the records of a group_by selector with a
// count per field value. Each group's Filter holds the selector filter plus the
// group value, so it is also the filter of a memory list call that fetches it.
type MemoryStateContextSummary struct {
	Type          string                    `json:"type"`
	GroupBy       string                    `json:"group_by"`
	Groups        []MemoryStateContextGroup `json:"groups"`
	Ungrouped     int                       `json:"ungrouped,omitempty"`
	ExcludeAny    []map[string]any          `json:"exclude_any,omitempty"`
	AtLeast       bool                      `json:"at_least,omitempty"`
	OmittedGroups int                       `json:"omitted_groups,omitempty"`
}

type MemoryStateContextGroup struct {
	Filter map[string]any `json:"filter"`
	Count  int            `json:"count"`
}

func (summary MemoryStateContextSummary) entryCount() int {
	if summary.Ungrouped > 0 {
		return len(summary.Groups) + 1
	}
	return len(summary.Groups)
}

type MemoryStateContextItem struct {
	Type       string           `json:"type"`
	Ref        string           `json:"ref"`
	Filter     map[string]any   `json:"filter"`
	ExcludeAny []map[string]any `json:"exclude_any,omitempty"`
	Data       json.RawMessage  `json:"data"`
}
type MemoryStateContextQuery struct {
	Type       string           `json:"type"`
	Filter     map[string]any   `json:"filter"`
	ExcludeAny []map[string]any `json:"exclude_any,omitempty"`
	Returned   int              `json:"returned"`
	HasMore    bool             `json:"has_more"`
}

// CollectMemoryStateContext applies the configured selectors without creating
// cursors, then returns the bounded records and per-selector query metadata.
func CollectMemoryStateContext(store *MemoryStore, settings sessionfs.NativeMemorySettings, selectors []sessionfs.NativeMemoryStateContextSelector) (*MemoryStateContext, error) {
	if store == nil {
		return nil, fmt.Errorf("memory statecontext store is not available")
	}
	state := &MemoryStateContext{Items: make([]MemoryStateContextItem, 0), Queries: make([]MemoryStateContextQuery, 0, len(selectors))}
	for _, selector := range selectors {
		if selector.GroupBy != "" {
			summary, err := collectMemorySummary(store, settings, selector)
			if err != nil {
				return nil, err
			}
			state.Summaries = append(state.Summaries, summary)
			state.HasMore = state.HasMore || summary.AtLeast
			continue
		}
		listed, err := store.list(selector.Type, selector.Filter, selector.Limit, "newest", nil, settings, memoryListOptions{CreateCursor: false, ExcludeAny: selector.ExcludeAny})
		if err != nil {
			return nil, err
		}
		for _, item := range listed.Items {
			data, err := memoryJSONCompact(item.value)
			if err != nil {
				return nil, err
			}
			entry := MemoryStateContextItem{Type: selector.Type, Ref: item.Ref, Filter: selector.Filter, Data: append(json.RawMessage(nil), data...)}
			if len(selector.ExcludeAny) > 0 {
				entry.ExcludeAny = selector.ExcludeAny
			}
			state.Items = append(state.Items, entry)
		}
		query := MemoryStateContextQuery{Type: selector.Type, Filter: selector.Filter, Returned: len(listed.Items), HasMore: listed.HasMore}
		if len(selector.ExcludeAny) > 0 {
			query.ExcludeAny = selector.ExcludeAny
		}
		state.Queries = append(state.Queries, query)
		state.HasMore = state.HasMore || listed.HasMore
	}
	return state, nil
}

func collectMemorySummary(store *MemoryStore, settings sessionfs.NativeMemorySettings, selector sessionfs.NativeMemoryStateContextSelector) (MemoryStateContextSummary, error) {
	grouped, err := store.GroupBy(selector.Type, selector.Filter, selector.ExcludeAny, selector.GroupBy, settings)
	if err != nil {
		return MemoryStateContextSummary{}, err
	}
	summary := MemoryStateContextSummary{Type: selector.Type, GroupBy: selector.GroupBy, Groups: make([]MemoryStateContextGroup, 0, len(grouped.Groups)), Ungrouped: grouped.Ungrouped, AtLeast: grouped.AtLeast}
	if len(selector.ExcludeAny) > 0 {
		summary.ExcludeAny = selector.ExcludeAny
	}
	for _, group := range grouped.Groups {
		filter := make(map[string]any, len(selector.Filter)+1)
		for key, value := range selector.Filter {
			filter[key] = value
		}
		filter[selector.GroupBy] = group.Value
		summary.Groups = append(summary.Groups, MemoryStateContextGroup{Filter: filter, Count: group.Count})
	}
	return summary, nil
}

func (state *MemoryStateContext) StateContextItemCount() int {
	if state == nil {
		return 0
	}
	count := len(state.Items)
	for _, summary := range state.Summaries {
		count += summary.entryCount()
	}
	return count
}
func (state *MemoryStateContext) TrimStateContextItem() bool {
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
func (state *MemoryStateContext) StateContextQueryCount() int {
	if state == nil {
		return 0
	}
	return len(state.Queries)
}
func (state *MemoryStateContext) TrimStateContextQuery() bool {
	if state == nil || len(state.Queries) == 0 {
		return false
	}
	state.Queries = state.Queries[:len(state.Queries)-1]
	state.OmittedQueries++
	return true
}
