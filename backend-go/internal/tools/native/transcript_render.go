package native

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var transcriptDataURL = regexp.MustCompile(`data:[A-Za-z0-9.+/-]+;base64,[A-Za-z0-9+/=]{64,}`)

// cutMiddle keeps the head (70%) and the tail (30%) of text longer than limit bytes, on
// character boundaries, and says how much it left out.
func cutMiddle(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	head := limit * 7 / 10
	tail := limit - head
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	start := len(text) - tail
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return fmt.Sprintf("%s [... truncated %d bytes ...] %s", text[:head], start-head, text[start:])
}

type transcriptRecord struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Response  *struct {
		Output []json.RawMessage `json:"output"`
	} `json:"response"`
}

// transcriptText is the text of a message content or a tool output: a string, or the text of
// each part, with images shown as a marker.
func transcriptText(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		pieces := make([]string, 0, len(parts))
		for _, part := range parts {
			if strings.Contains(part.Type, "image") {
				pieces = append(pieces, "[image]")
			} else if part.Text != "" {
				pieces = append(pieces, part.Text)
			}
		}
		return strings.Join(pieces, "\n")
	}
	return string(raw)
}

// transcriptEntry is one line of output: a message, a tool call or a tool result. role is
// what the role filter matches (user, assistant, tool, or the record's own role); tool is the
// tool name of a call or of the result's call.
type transcriptEntry struct {
	turn    int64
	off     int64
	role    string
	tool    string
	text    string
	matched bool
}

// renderTranscriptRecord turns one transcript line into entries. Reasoning is not shown.
// limit cuts each payload (zero keeps it whole). calls maps call ids to tool names and is
// filled by every call seen, so a result later knows its tool. needle, lower case, marks the
// entries whose whole text contains it.
func renderTranscriptRecord(data []byte, turn, off int64, limit int, calls map[string]string, needle string) []transcriptEntry {
	var record transcriptRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return []transcriptEntry{{turn: turn, off: off, role: "unknown", text: fmt.Sprintf("#%d [unreadable record]", turn)}}
	}
	entry := func(role, tool, label, body string) transcriptEntry {
		body = transcriptDataURL.ReplaceAllStringFunc(body, func(match string) string {
			return fmt.Sprintf("[data URL %d bytes]", len(match))
		})
		matched := needle != "" && strings.Contains(strings.ToLower(label+body), needle)
		return transcriptEntry{turn: turn, off: off, role: role, tool: tool, matched: matched, text: fmt.Sprintf("#%d %s%s", turn, label, cutMiddle(body, limit))}
	}
	switch {
	case record.Type == "function_call_output":
		tool := calls[record.CallID]
		return []transcriptEntry{entry("tool", tool, "result ["+record.CallID+"] ", transcriptText(record.Output))}
	case record.Type == "response.completed" && record.Response != nil:
		var entries []transcriptEntry
		for _, raw := range record.Response.Output {
			var item transcriptRecord
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			switch item.Type {
			case "message":
				entries = append(entries, entry("assistant", "", "assistant: ", transcriptText(item.Content)))
			case "function_call":
				calls[item.CallID] = item.Name
				entries = append(entries, entry("tool", item.Name, "call "+item.Name+" ["+item.CallID+"] ", transcriptText(item.Arguments)))
			case "reasoning":
			default:
				entries = append(entries, transcriptEntry{turn: turn, off: off, role: item.Type, text: fmt.Sprintf("#%d [%s]", turn, item.Type)})
			}
		}
		return entries
	case record.Type == "message" || record.Role != "":
		role := record.Role
		if role == "" {
			role = "message"
		}
		return []transcriptEntry{entry(role, "", role+": ", transcriptText(record.Content))}
	}
	return []transcriptEntry{{turn: turn, off: off, role: record.Type, text: fmt.Sprintf("#%d [%s]", turn, record.Type)}}
}

// selectTranscriptEntries keeps the entries that pass the role and tool filters and, with a
// search, those that match it plus context entries on each side.
func selectTranscriptEntries(entries []transcriptEntry, role string, tool *compiledGlob, search bool, context int) []transcriptEntry {
	keep := make([]bool, len(entries))
	for index, entry := range entries {
		keep[index] = (role == "" || entry.role == role) && (tool == nil || entry.role == "tool" && entry.tool != "" && tool.matches(entry.tool, entry.tool))
	}
	if search {
		hit := make([]bool, len(entries))
		for index, entry := range entries {
			hit[index] = keep[index] && entry.matched
		}
		for index := range entries {
			keep[index] = false
		}
		for index, matched := range hit {
			if !matched {
				continue
			}
			for near := max(index-context, 0); near <= min(index+context, len(entries)-1); near++ {
				keep[near] = true
			}
		}
	}
	selected := entries[:0:0]
	for index, entry := range entries {
		if keep[index] {
			selected = append(selected, entry)
		}
	}
	return selected
}
