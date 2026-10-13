package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const requestDraftToolID = "native.request_draft"

var requestDraftInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "title":{"type":"string","description":"One line naming what the draft decides, shown above it."},
    "draft":{"type":"object","description":"Your structured understanding: the fields the user should confirm or correct (goal, scope, constraints, assumptions, open questions, …). Any shape is allowed; keep it small enough to read at a glance.","additionalProperties":true}
  },
  "required":["draft"],
  "additionalProperties":false
}`)

const requestDraftDescription = "Present how you read the user's request as a JSON draft and wait for an interactive user to accept, edit or decline it. Call it alone before starting work when the request could reasonably be read in more than one way, or when a wrong reading would waste the turn. The backend will defer other calls from the same response until after the decision and a fresh plan. Without an interactive client, the backend accepts the draft after its configured headless timeout. The result carries `decision` (accept = proceed as drafted, update = proceed with `draft`, the user's edited version, decline = do not proceed), `status` (answered, timed_out or cancelled; `answered` alone does not mean approval), the user's `reason` when they decline, and, when the user changed something, `changed` — the paths they added, removed or corrected. Consider the decline reason as context, but treat decline, timeout or cancellation as no approval; do not proceed."

// NewRequestDraftTool creates the user-decision tool with its per-session policy.
func NewRequestDraftTool(settings sessionfs.NativeRequestDraftSettings) (tools.Tool, error) {
	validator, err := tools.CompileSchema(requestDraftInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native request_draft schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          requestDraftToolID,
			Source:      "native",
			Name:        "request_draft",
			Description: requestDraftDescription,
			InputSchema: append(json.RawMessage(nil), requestDraftInputSchema...),
		},
		Validator:            validator,
		ExampleOperation:     json.RawMessage(`{"title":"How I read your request","draft":{"goal":"..."}}`),
		RequiresUserDecision: true,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeRequestDraft(ctx, arguments, execution, settings)
		},
	}, nil
}

func executeNativeRequestDraft(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext, policy sessionfs.NativeRequestDraftSettings) (string, error) {
	draft, ok := arguments["draft"].(map[string]any)
	if !ok || draft == nil {
		return renderRequestDraftRefusal("invalid_draft", "draft must be a JSON object.", nil), nil
	}
	draftRaw := rawRequestDraft(execution.RawArguments, draft)
	byteCount, err := tools.DraftJSONBytes(draftRaw)
	if err != nil {
		return "", fmt.Errorf("measure native request_draft payload: %w", err)
	}
	if byteCount > policy.MaxDraftBytes {
		maximum := policy.MaxDraftBytes
		return renderRequestDraftRefusal("draft_too_large", fmt.Sprintf("draft is %d bytes; this session accepts at most %d. Send a smaller draft — the user reads it at a glance.", byteCount, maximum), &requestDraftRefusalDetails{MaxDraftBytes: &maximum}), nil
	}
	depth, err := tools.DraftJSONDepth(draftRaw)
	if err != nil {
		return "", fmt.Errorf("measure native request_draft depth: %w", err)
	}
	if depth > policy.MaxDraftDepth {
		maximum := policy.MaxDraftDepth
		return renderRequestDraftRefusal("draft_too_deep", fmt.Sprintf("draft nests %d levels; this session accepts at most %d. Flatten it, or move the deep part into a JSON string the user can edit as text.", depth, maximum), &requestDraftRefusalDetails{MaxDraftDepth: &maximum}), nil
	}
	title, _ := arguments["title"].(string)
	title = strings.TrimSpace(title)
	if titleLength := len(utf16.Encode([]rune(title))); int64(titleLength) > policy.MaxTitleChars {
		maximum := policy.MaxTitleChars
		return renderRequestDraftRefusal("title_too_long", fmt.Sprintf("title is %d characters; this session accepts at most %d.", titleLength, maximum), &requestDraftRefusalDetails{MaxTitleChars: &maximum}), nil
	}

	question := tools.InteractionQuestion{
		Draft:        append(json.RawMessage(nil), draftRaw...),
		Title:        title,
		TimeoutMS:    policy.TimeoutMS,
		ExtendMS:     policy.ExtendMS,
		MaxTimeoutMS: policy.MaxTimeoutMS,
		Limits: tools.InteractionLimits{
			MaxDraftBytes: policy.MaxDraftBytes,
			MaxDraftDepth: policy.MaxDraftDepth,
		},
	}
	var answer tools.InteractionAnswer
	if execution.RequestInteraction == nil {
		answer = autoAcceptRequestDraft(ctx, policy.HeadlessAutoAcceptTimeoutMS)
	} else {
		answer, err = execution.RequestInteraction(ctx, tools.InteractionIdentity{Tool: execution.ToolName, ToolCallID: execution.CallID}, question)
		if err != nil {
			return "", err
		}
	}
	if answer.Decision == "" || answer.Outcome == "" {
		return "", errors.New("request_draft interaction returned an incomplete answer")
	}
	if execution.OnInteractionResolved != nil {
		if err := execution.OnInteractionResolved(tools.InteractionIdentity{Tool: execution.ToolName, ToolCallID: execution.CallID}, answer, append(json.RawMessage(nil), draftRaw...)); err != nil {
			return "", err
		}
	}
	return renderRequestDraftAnswer(answer, draftRaw)
}

func autoAcceptRequestDraft(ctx context.Context, timeoutMS int64) tools.InteractionAnswer {
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return tools.InteractionAnswer{Decision: tools.InteractionAccept, Outcome: tools.InteractionAnswered, Source: tools.InteractionSourceTimeout}
	case <-ctx.Done():
		reason := "The run was stopped before the draft was answered."
		return tools.InteractionAnswer{Decision: tools.InteractionDecline, Outcome: tools.InteractionCancelled, Reason: &reason}
	}
}

func rawRequestDraft(raw json.RawMessage, fallback map[string]any) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		if draft := fields["draft"]; len(draft) > 0 && json.Valid(draft) {
			return append(json.RawMessage(nil), draft...)
		}
	}
	encoded, err := json.Marshal(fallback)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

type requestDraftRefusalDetails struct {
	MaxDraftBytes *int64 `json:"max_draft_bytes,omitempty"`
	MaxDraftDepth *int64 `json:"max_draft_depth,omitempty"`
	MaxTitleChars *int64 `json:"max_title_chars,omitempty"`
}

func renderRequestDraftRefusal(code, message string, details *requestDraftRefusalDetails) string {
	response := struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		requestDraftRefusalDetails
	}{Error: code, Message: message}
	if details != nil {
		response.requestDraftRefusalDetails = *details
	}
	return marshalRequestDraftJSON(response)
}

func renderRequestDraftAnswer(answer tools.InteractionAnswer, proposed json.RawMessage) (string, error) {
	var draftNode *requestDraftJSONNode
	var changed []string
	if len(answer.Draft) > 0 && json.Valid(answer.Draft) {
		var err error
		draftNode, err = decodeOrderedRequestDraft(answer.Draft)
		if err != nil {
			return "", fmt.Errorf("decode request_draft answer: %w", err)
		}
		if len(proposed) > 0 && json.Valid(proposed) {
			before, err := decodeOrderedRequestDraft(proposed)
			if err != nil {
				return "", fmt.Errorf("decode proposed request_draft: %w", err)
			}
			changed = changedRequestDraftPaths(before, draftNode, "")
		}
	}

	output := []byte("{\n")
	fieldCount := 0
	field := func(name string) {
		if fieldCount > 0 {
			output = append(output, ',', '\n')
		}
		output = appendRequestDraftIndent(output, 1)
		output = appendRequestDraftJSONString(output, name)
		output = append(output, ':', ' ')
		fieldCount++
	}
	field("decision")
	output = appendRequestDraftJSONString(output, string(answer.Decision))
	field("status")
	output = appendRequestDraftJSONString(output, string(answer.Outcome))
	if draftNode != nil {
		field("draft")
		var err error
		output, err = appendRequestDraftJSON(output, draftNode, 1)
		if err != nil {
			return "", err
		}
	}
	if len(changed) > 0 {
		field("changed")
		output = appendRequestDraftStringArray(output, changed, 1)
	}
	if answer.Reason != nil {
		field("reason")
		output = appendRequestDraftJSONString(output, *answer.Reason)
	}
	output = append(output, '\n', '}')
	return string(output), nil
}

func marshalRequestDraftJSON(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

func requestDraftChangedPaths(beforeRaw, afterRaw json.RawMessage) ([]string, error) {
	before, err := decodeOrderedRequestDraft(beforeRaw)
	if err != nil {
		return nil, err
	}
	after, err := decodeOrderedRequestDraft(afterRaw)
	if err != nil {
		return nil, err
	}
	return changedRequestDraftPaths(before, after, ""), nil
}

type requestDraftJSONMember struct {
	key   string
	value *requestDraftJSONNode
}

type requestDraftJSONNode struct {
	kind    byte
	members []requestDraftJSONMember
	items   []*requestDraftJSONNode
	scalar  any
}

func appendRequestDraftIndent(output []byte, level int) []byte {
	for range level * 2 {
		output = append(output, ' ')
	}
	return output
}

func appendRequestDraftJSONString(output []byte, value string) []byte {
	output = append(output, '"')
	const hexadecimal = "0123456789abcdef"
	for _, current := range value {
		switch current {
		case '"':
			output = append(output, '\\', '"')
		case '\\':
			output = append(output, '\\', '\\')
		case '\b':
			output = append(output, '\\', 'b')
		case '\f':
			output = append(output, '\\', 'f')
		case '\n':
			output = append(output, '\\', 'n')
		case '\r':
			output = append(output, '\\', 'r')
		case '\t':
			output = append(output, '\\', 't')
		default:
			if current < 0x20 {
				output = append(output, '\\', 'u', '0', '0', hexadecimal[byte(current)>>4], hexadecimal[byte(current)&0x0f])
			} else {
				output = append(output, string(current)...)
			}
		}
	}
	return append(output, '"')
}

func appendRequestDraftJSON(output []byte, node *requestDraftJSONNode, level int) ([]byte, error) {
	switch node.kind {
	case 'o':
		if len(node.members) == 0 {
			return append(output, '{', '}'), nil
		}
		output = append(output, '{', '\n')
		for index, member := range node.members {
			output = appendRequestDraftIndent(output, level+1)
			output = appendRequestDraftJSONString(output, member.key)
			output = append(output, ':', ' ')
			var err error
			output, err = appendRequestDraftJSON(output, member.value, level+1)
			if err != nil {
				return nil, err
			}
			if index+1 < len(node.members) {
				output = append(output, ',')
			}
			output = append(output, '\n')
		}
		output = appendRequestDraftIndent(output, level)
		return append(output, '}'), nil
	case 'a':
		if len(node.items) == 0 {
			return append(output, '[', ']'), nil
		}
		output = append(output, '[', '\n')
		for index, item := range node.items {
			output = appendRequestDraftIndent(output, level+1)
			var err error
			output, err = appendRequestDraftJSON(output, item, level+1)
			if err != nil {
				return nil, err
			}
			if index+1 < len(node.items) {
				output = append(output, ',')
			}
			output = append(output, '\n')
		}
		output = appendRequestDraftIndent(output, level)
		return append(output, ']'), nil
	case 'v':
		switch value := node.scalar.(type) {
		case nil:
			return append(output, "null"...), nil
		case string:
			return appendRequestDraftJSONString(output, value), nil
		case bool:
			if value {
				return append(output, "true"...), nil
			}
			return append(output, "false"...), nil
		case json.Number:
			return appendRequestDraftNumber(output, value)
		default:
			return nil, fmt.Errorf("unsupported request_draft JSON value %T", value)
		}
	default:
		return nil, errors.New("unsupported request_draft JSON node")
	}
}

func appendRequestDraftNumber(output []byte, value json.Number) ([]byte, error) {
	number, err := strconv.ParseFloat(string(value), 64)
	if err != nil {
		if math.IsInf(number, 0) {
			return append(output, "null"...), nil
		}
		if number == 0 {
			return append(output, '0'), nil
		}
		return nil, err
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return append(output, "null"...), nil
	}
	if number == 0 {
		return append(output, '0'), nil
	}
	absolute := math.Abs(number)
	if absolute >= 1e-6 && absolute < 1e21 {
		return append(output, strconv.FormatFloat(number, 'f', -1, 64)...), nil
	}
	scientific := strconv.FormatFloat(number, 'e', -1, 64)
	mantissa, exponent, ok := strings.Cut(scientific, "e")
	if !ok || len(exponent) == 0 {
		return nil, fmt.Errorf("format request_draft number %q", value)
	}
	sign := exponent[0]
	digits := strings.TrimLeft(exponent[1:], "0")
	if digits == "" {
		digits = "0"
	}
	output = append(output, mantissa...)
	output = append(output, 'e', sign)
	return append(output, digits...), nil
}

func appendRequestDraftStringArray(output []byte, values []string, level int) []byte {
	if len(values) == 0 {
		return append(output, '[', ']')
	}
	output = append(output, '[', '\n')
	for index, value := range values {
		output = appendRequestDraftIndent(output, level+1)
		output = appendRequestDraftJSONString(output, value)
		if index+1 < len(values) {
			output = append(output, ',')
		}
		output = append(output, '\n')
	}
	output = appendRequestDraftIndent(output, level)
	return append(output, ']')
}

func decodeOrderedRequestDraft(raw json.RawMessage) (*requestDraftJSONNode, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	node, err := decodeRequestDraftNode(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err == nil {
		return nil, errors.New("unexpected trailing JSON value")
	}
	return node, nil
}

func decodeRequestDraftNode(decoder *json.Decoder) (*requestDraftJSONNode, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			node := &requestDraftJSONNode{kind: 'o'}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("JSON object key is not a string")
				}
				child, err := decodeRequestDraftNode(decoder)
				if err != nil {
					return nil, err
				}
				updated := false
				for index := range node.members {
					if node.members[index].key == key {
						node.members[index].value = child
						updated = true
						break
					}
				}
				if !updated {
					node.members = append(node.members, requestDraftJSONMember{key: key, value: child})
				}
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			sortRequestDraftObjectMembers(node)
			return node, nil
		case '[':
			node := &requestDraftJSONNode{kind: 'a'}
			for decoder.More() {
				child, err := decodeRequestDraftNode(decoder)
				if err != nil {
					return nil, err
				}
				node.items = append(node.items, child)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return node, nil
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	default:
		return &requestDraftJSONNode{kind: 'v', scalar: value}, nil
	}
}

func sortRequestDraftObjectMembers(node *requestDraftJSONNode) {
	sort.SliceStable(node.members, func(left, right int) bool {
		leftIndex, leftIsIndex := requestDraftArrayIndex(node.members[left].key)
		rightIndex, rightIsIndex := requestDraftArrayIndex(node.members[right].key)
		if leftIsIndex && rightIsIndex {
			return leftIndex < rightIndex
		}
		return leftIsIndex && !rightIsIndex
	})
}

func requestDraftArrayIndex(key string) (uint64, bool) {
	if key == "" || (len(key) > 1 && key[0] == '0') {
		return 0, false
	}
	for _, character := range key {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	index, err := strconv.ParseUint(key, 10, 32)
	if err != nil || index >= 4_294_967_295 {
		return 0, false
	}
	return index, true
}

func changedRequestDraftPaths(before, after *requestDraftJSONNode, prefix string) []string {
	if equalRequestDraftJSON(before, after) {
		return nil
	}
	if before == nil || after == nil {
		return []string{prefix}
	}
	if before.kind == 'o' && after.kind == 'o' {
		paths := make([]string, 0)
		seen := make(map[string]struct{}, len(before.members)+len(after.members))
		for _, member := range before.members {
			seen[member.key] = struct{}{}
			path := member.key
			if prefix != "" {
				path = prefix + "." + member.key
			}
			paths = append(paths, changedRequestDraftPaths(member.value, orderedObjectValue(after, member.key), path)...)
		}
		for _, member := range after.members {
			if _, exists := seen[member.key]; exists {
				continue
			}
			path := member.key
			if prefix != "" {
				path = prefix + "." + member.key
			}
			paths = append(paths, changedRequestDraftPaths(nil, member.value, path)...)
		}
		return paths
	}
	if before.kind == 'a' && after.kind == 'a' {
		paths := make([]string, 0)
		max := len(before.items)
		if len(after.items) > max {
			max = len(after.items)
		}
		for index := 0; index < max; index++ {
			var beforeItem, afterItem *requestDraftJSONNode
			if index < len(before.items) {
				beforeItem = before.items[index]
			}
			if index < len(after.items) {
				afterItem = after.items[index]
			}
			paths = append(paths, changedRequestDraftPaths(beforeItem, afterItem, fmt.Sprintf("%s[%d]", prefix, index))...)
		}
		return paths
	}
	return []string{prefix}
}

func orderedObjectValue(node *requestDraftJSONNode, key string) *requestDraftJSONNode {
	for _, member := range node.members {
		if member.key == key {
			return member.value
		}
	}
	return nil
}

func equalRequestDraftJSON(left, right *requestDraftJSONNode) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.kind != right.kind {
		return false
	}
	switch left.kind {
	case 'o':
		if len(left.members) != len(right.members) {
			return false
		}
		for _, member := range left.members {
			if !equalRequestDraftJSON(member.value, orderedObjectValue(right, member.key)) {
				return false
			}
		}
		return true
	case 'a':
		if len(left.items) != len(right.items) {
			return false
		}
		for index := range left.items {
			if !equalRequestDraftJSON(left.items[index], right.items[index]) {
				return false
			}
		}
		return true
	case 'v':
		leftNumber, leftOK := left.scalar.(json.Number)
		rightNumber, rightOK := right.scalar.(json.Number)
		if leftOK && rightOK {
			leftFloat, leftErr := strconv.ParseFloat(string(leftNumber), 64)
			rightFloat, rightErr := strconv.ParseFloat(string(rightNumber), 64)
			return leftErr == nil && rightErr == nil && leftFloat == rightFloat && (leftFloat != 0 || math.Signbit(leftFloat) == math.Signbit(rightFloat))
		}
		return left.scalar == right.scalar
	default:
		return false
	}
}
