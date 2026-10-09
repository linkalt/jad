package webui

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// handledItemTypes are the response output items the reducers already turn into
// text, reasoning or client tool calls. Every other item that has an id is a
// provider-hosted tool: the provider ran it itself and no client reply follows.
var handledItemTypes = map[string]struct{}{
	"message":              {},
	"reasoning":            {},
	"function_call":        {},
	"function_call_output": {},
}

// isHostedToolItem reports whether a response output item is a provider-hosted
// tool call. It names no tool: web_search_call, openrouter:web_fetch and any
// future provider tool all qualify, so none needs its own code.
func isHostedToolItem(item map[string]json.RawMessage) bool {
	typeName := stringField(item["type"])
	if typeName == "" || stringField(item["id"]) == "" {
		return false
	}
	_, handled := handledItemTypes[typeName]
	return !handled
}

// hostedToolName is what the UI calls the tool: the item type without a trailing
// "_call" (web_search_call -> web_search); any other type is shown as it is.
func hostedToolName(typeName string) string {
	if name := strings.TrimSuffix(typeName, "_call"); name != "" {
		return name
	}
	return typeName
}

// hostedToolPart projects one provider-hosted tool item. limit is the inline cap
// for the raw JSON in bytes; 0 keeps it whole (snapshots are capped later).
func hostedToolPart(item map[string]json.RawMessage, limit int) (Part, bool) {
	if !isHostedToolItem(item) {
		return Part{}, false
	}
	part := Part{
		Type:   "hosted_tool",
		CallID: stringField(item["id"]),
		Name:   hostedToolName(stringField(item["type"])),
		Status: stringField(item["status"]),
		Target: hostedToolTarget(item),
	}
	raw, ok := encodeCompact(item)
	if !ok {
		return part, true
	}
	part.Raw = raw
	if limit > 0 {
		capHostedRaw(&part, limit)
	}
	return part, true
}

// hostedToolTarget is the short text shown next to the tool name: what was
// searched, opened or fetched. It reads common field names, not a tool's schema.
func hostedToolTarget(item map[string]json.RawMessage) string {
	var action struct {
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
		URL     string   `json:"url"`
		Pattern string   `json:"pattern"`
	}
	_ = json.Unmarshal(item["action"], &action)
	if action.Query != "" {
		return action.Query
	}
	if len(action.Queries) > 0 {
		return strings.Join(action.Queries, "; ")
	}
	for _, candidate := range []string{action.URL, action.Pattern, stringField(item["url"]), stringField(item["query"]), stringField(item["title"])} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// hostedRawSize is the size of the part's raw item before any cap.
func hostedRawSize(part Part) int {
	if part.Truncated != nil {
		return part.Truncated.Bytes
	}
	return len(part.Raw)
}

// capHostedRaw shortens the raw JSON of a part to limit bytes. It sets Truncated,
// with the size before the cap, only when the raw text actually changed.
func capHostedRaw(part *Part, limit int) {
	full := len(part.Raw)
	if limit <= 0 || full <= limit {
		return
	}
	shrunk := shrinkJSON(part.Raw, limit)
	if shrunk == part.Raw {
		return
	}
	part.Raw = shrunk
	part.Truncated = &PartTruncation{Bytes: full}
}

// shrinkJSON returns raw as compact JSON of at most limit bytes. It shortens string
// values only, so the result stays valid JSON: the longest strings are cut to a
// common length, chosen so that the total cut covers the excess in one pass (a
// fetch's page text is one such string, a long result list is many). If cutting
// every string to nothing is still too big, keys and numbers dominate and the
// result is "{}" (or "" for a limit below 2). The same input always gives the
// same bytes.
func shrinkJSON(raw string, limit int) string {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return cutUTF8(raw, limit)
	}
	for attempt := 0; attempt < 4; attempt++ {
		encoded, ok := encodeCompact(value)
		if !ok {
			break
		}
		excess := len(encoded) - limit
		if excess <= 0 {
			return encoded
		}
		if !cutLongestStrings(value, excess) {
			break
		}
	}
	if encoded, ok := encodeCompact(value); ok && len(encoded) <= limit {
		return encoded
	}
	switch {
	case limit >= 2:
		return "{}"
	default:
		return ""
	}
}

// cutLongestStrings shortens the longest strings of a decoded document so that at
// least excess bytes are removed, and reports whether that was possible. With k
// strings cut to length L the removed total is sum(len)-k*L, so L is the largest
// value for which that reaches excess; strings not longer than L stay whole.
func cutLongestStrings(value any, excess int) bool {
	refs := stringRefs(value)
	sort.SliceStable(refs, func(i, j int) bool { return len(refs[i].get()) > len(refs[j].get()) })
	total := 0
	for _, ref := range refs {
		total += len(ref.get())
	}
	if total < excess {
		for _, ref := range refs {
			ref.set("")
		}
		return false
	}
	sum := 0
	for k, ref := range refs {
		sum += len(ref.get())
		length := (sum - excess) / (k + 1)
		if sum-excess < 0 {
			length = 0
		}
		next := 0
		if k+1 < len(refs) {
			next = len(refs[k+1].get())
		}
		if length >= next || k+1 == len(refs) {
			for _, cut := range refs[:k+1] {
				cut.set(cutUTF8(cut.get(), length))
			}
			return true
		}
	}
	return true
}

func encodeCompact(value any) (string, bool) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return "", false
	}
	return strings.TrimRight(buffer.String(), "\n"), true
}

func cutUTF8(text string, limit int) string {
	if limit >= len(text) {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// stringRef points at one string value inside a decoded JSON document.
type stringRef struct {
	object map[string]any
	key    string
	list   []any
	index  int
}

func (ref *stringRef) get() string {
	if ref.object != nil {
		value, _ := ref.object[ref.key].(string)
		return value
	}
	value, _ := ref.list[ref.index].(string)
	return value
}

func (ref *stringRef) set(value string) {
	if ref.object != nil {
		ref.object[ref.key] = value
		return
	}
	ref.list[ref.index] = value
}

// stringRefs lists every string value of a decoded document in a fixed order
// (object keys sorted), so the cap does not depend on map iteration order.
func stringRefs(value any) []*stringRef {
	var refs []*stringRef
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if _, isString := typed[key].(string); isString {
					refs = append(refs, &stringRef{object: typed, key: key})
					continue
				}
				visit(typed[key])
			}
		case []any:
			for index, child := range typed {
				if _, isString := child.(string); isString {
					refs = append(refs, &stringRef{list: typed, index: index})
					continue
				}
				visit(child)
			}
		}
	}
	visit(value)
	return refs
}
