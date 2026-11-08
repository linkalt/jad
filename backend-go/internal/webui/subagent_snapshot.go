package webui

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// SubagentStepCompleted is the only status a subagent step is ever given: a step exists
// only because its response.completed record was written, and nothing running or queued
// is ever shown. The browser ignores any other status.
const SubagentStepCompleted = "completed"

// SubagentSnapshot is a one-shot, bounded copy of the steps a subagent run had completed
// when a tool result was served. It is built from the child's transcript, never streamed,
// and is never changed after it is built: parts of different messages and caches share
// one snapshot, so a holder must copy before it edits.
type SubagentSnapshot struct {
	Agent string         `json:"agent,omitempty"`
	Steps []SubagentStep `json:"steps"`
}

// SubagentStep is one completed response of the child, as the parts of a message step
// without tool results: text, reasoning and tool_call parts only.
type SubagentStep struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Parts  []Part `json:"parts"`
}

// SubagentLimits bounds a snapshot. A step, part or byte over a limit is left out (text
// is cut and marked with Truncated), so a snapshot never grows with its child.
type SubagentLimits struct {
	// MaxSteps is the most steps a snapshot keeps; the first ones win.
	MaxSteps int
	// MaxPartsPerStep is the most parts one step keeps.
	MaxPartsPerStep int
	// MaxPartBytes cuts the text, reasoning or arguments of one part.
	MaxPartBytes int
	// MaxTotalBytes bounds the text of the whole snapshot.
	MaxTotalBytes int
}

// DefaultSubagentLimits is the bound a served snapshot has unless a caller sets another.
func DefaultSubagentLimits() SubagentLimits {
	return SubagentLimits{MaxSteps: 32, MaxPartsPerStep: 12, MaxPartBytes: 2048, MaxTotalBytes: 48 * 1024}
}

// SubagentCall identifies the child of one subagent tool result.
type SubagentCall struct {
	CallID string
	// Agent is the agent name the result or the call reports, empty when neither does.
	Agent string
	// Transcript is the child's transcript path as the result printed it, empty when it
	// printed none. It is only a hint: a reader must confine it before it opens it.
	Transcript string
}

// IsSubagentTool reports whether a tool call name is the native subagent tool. It is the
// only discriminator: profile_message prints the same transcript line and carries no
// subagent steps.
func IsSubagentTool(name string) bool {
	return name == "subagent" || name == "native.subagent"
}

// SubagentCallOf describes the child of a tool result when its tool call is the subagent
// tool. The agent is read from the result's information first (it names the agent that
// ran), then from the call's arguments.
func SubagentCallOf(call, result Part) (SubagentCall, bool) {
	if call.Type != "tool_call" || result.Type != "tool_result" || !IsSubagentTool(call.Name) || call.CallID == "" || call.CallID != result.CallID {
		return SubagentCall{}, false
	}
	agent, transcript := subagentInformation(result.Output)
	if agent == "" && call.Arguments != nil {
		var arguments struct {
			Agent string `json:"agent"`
		}
		if json.Unmarshal([]byte(*call.Arguments), &arguments) == nil {
			agent = strings.TrimSpace(arguments.Agent)
		}
	}
	return SubagentCall{CallID: call.CallID, Agent: agent, Transcript: transcript}, true
}

// subagentInformation reads the agent and transcript lines of a tool result's
// INFORMATION section: "key: value" lines after the marker, up to the first blank line.
func subagentInformation(output string) (agent, transcript string) {
	_, section, found := strings.Cut(output, "\n--- INFORMATION ---\n")
	if !found {
		return "", ""
	}
	for len(section) > 0 {
		var line string
		line, section, _ = strings.Cut(section, "\n")
		if line == "" {
			break
		}
		switch key, value, _ := strings.Cut(line, ": "); key {
		case "agent":
			if agent == "" {
				agent = strings.TrimSpace(value)
			}
		case "transcript":
			if transcript == "" {
				transcript = strings.TrimSpace(value)
			}
		}
	}
	return agent, transcript
}

// AttachSubagents sets Subagent on the tool results of subagent calls, as lookup answers
// them (nil leaves the part as it is). It never edits messages or parts it was given: a
// message that gains a snapshot is copied. Call it only where a snapshot is served; it
// reads nothing but what lookup reads.
func AttachSubagents(messages []Message, lookup func(SubagentCall) *SubagentSnapshot) []Message {
	if lookup == nil {
		return messages
	}
	result := messages
	copied := false
	for index := range messages {
		message := messages[index]
		var calls map[string]Part
		for _, part := range message.Parts {
			if part.Type == "tool_call" && IsSubagentTool(part.Name) && part.CallID != "" {
				if calls == nil {
					calls = map[string]Part{}
				}
				calls[part.CallID] = part
			}
		}
		if calls == nil {
			continue
		}
		var parts []Part
		for partIndex, part := range message.Parts {
			if part.Type != "tool_result" || part.Subagent != nil {
				continue
			}
			call, ok := SubagentCallOf(calls[part.CallID], part)
			if !ok {
				continue
			}
			snapshot := lookup(call)
			if snapshot == nil {
				continue
			}
			if parts == nil {
				parts = append([]Part(nil), message.Parts...)
			}
			parts[partIndex].Subagent = snapshot
		}
		if parts == nil {
			continue
		}
		if !copied {
			result = append([]Message(nil), messages...)
			copied = true
		}
		message.Parts = parts
		result[index] = message
	}
	return result
}

var completedMarker = []byte("response.completed")

// SubagentBuilder builds a snapshot from the lines of a child's transcript, in file order.
// The child writes its transcript with the writer a session uses, so each step is a
// response.completed line normalized by NormalizeTranscript: grouping is the child's own
// (one line is one step), and a line that is not a completed response adds nothing.
type SubagentBuilder struct {
	limits   SubagentLimits
	snapshot SubagentSnapshot
	total    int
	done     bool
}

// NewSubagentBuilder starts a snapshot for agent. A limit of zero or less takes its default.
func NewSubagentBuilder(agent string, limits SubagentLimits) *SubagentBuilder {
	defaults := DefaultSubagentLimits()
	if limits.MaxSteps <= 0 {
		limits.MaxSteps = defaults.MaxSteps
	}
	if limits.MaxPartsPerStep <= 0 {
		limits.MaxPartsPerStep = defaults.MaxPartsPerStep
	}
	if limits.MaxPartBytes <= 0 {
		limits.MaxPartBytes = defaults.MaxPartBytes
	}
	if limits.MaxTotalBytes <= 0 {
		limits.MaxTotalBytes = defaults.MaxTotalBytes
	}
	return &SubagentBuilder{limits: limits, snapshot: SubagentSnapshot{Agent: agent}}
}

// Add takes one complete line of the child's transcript and reports whether more lines
// can still change the snapshot: false once a limit is reached.
func (b *SubagentBuilder) Add(offset int64, line []byte) bool {
	if b.done {
		return false
	}
	if !bytes.Contains(line, completedMarker) {
		return true
	}
	var head struct {
		Type        string `json:"type"`
		SessionTurn *int64 `json:"session_turn"`
		SessionStep *int64 `json:"session_step"`
		Response    struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(line, &head) != nil || head.Type != "response.completed" {
		return true
	}
	// A response the provider reports as anything but completed is not a completed step.
	if head.Response.Status != "" && head.Response.Status != "completed" {
		return true
	}
	messages, err := NormalizeTranscript([]TranscriptRecord{{Offset: offset, Prefix: "o", Raw: line}})
	if err != nil || len(messages) == 0 {
		return true
	}
	parts := b.keep(messages[0].Parts)
	if len(parts) == 0 {
		return !b.done
	}
	id := strings.TrimPrefix(messages[0].ID, "session-assistant-")
	if head.SessionTurn != nil && head.SessionStep != nil {
		id = strconv.FormatInt(*head.SessionTurn, 10) + "." + strconv.FormatInt(*head.SessionStep, 10)
	}
	b.snapshot.Steps = append(b.snapshot.Steps, SubagentStep{ID: id, Status: SubagentStepCompleted, Parts: parts})
	if len(b.snapshot.Steps) >= b.limits.MaxSteps {
		b.done = true
	}
	return !b.done
}

// Snapshot is what the lines added so far completed, nil when no step was.
func (b *SubagentBuilder) Snapshot() *SubagentSnapshot {
	if len(b.snapshot.Steps) == 0 {
		return nil
	}
	snapshot := b.snapshot
	return &snapshot
}

// keep copies the renderable parts of one step within the limits: text, reasoning and
// tool_call parts only, with nothing a tool result or a nested snapshot would add.
func (b *SubagentBuilder) keep(parts []Part) []Part {
	kept := make([]Part, 0, min(len(parts), b.limits.MaxPartsPerStep))
	for _, part := range parts {
		if len(kept) >= b.limits.MaxPartsPerStep {
			break
		}
		remaining := b.limits.MaxTotalBytes - b.total
		if remaining <= 0 {
			b.done = true
			break
		}
		limit := min(b.limits.MaxPartBytes, remaining)
		switch part.Type {
		case "text", "reasoning":
			if part.Text == "" {
				continue
			}
			text := cutUTF8(part.Text, limit)
			next := Part{Type: part.Type, Text: text}
			if len(text) < len(part.Text) {
				next.Truncated = &PartTruncation{Bytes: len(part.Text)}
			}
			b.total += len(text)
			kept = append(kept, next)
		case "tool_call":
			if part.Arguments == nil || part.CallID == "" || part.Name == "" {
				continue
			}
			arguments := cutUTF8(*part.Arguments, limit)
			next := Part{Type: "tool_call", CallID: part.CallID, Name: part.Name, Arguments: &arguments}
			if len(arguments) < len(*part.Arguments) {
				next.Truncated = &PartTruncation{Bytes: len(*part.Arguments)}
			}
			b.total += len(arguments) + len(part.Name)
			kept = append(kept, next)
		}
	}
	return kept
}

// capSubagent cuts the text of a snapshot's parts to max bytes, as CapMessages cuts the
// parts of a message. It returns the same snapshot when nothing is over max, and a copy
// otherwise: the snapshot is shared and never edited.
func capSubagent(snapshot *SubagentSnapshot, max int) (*SubagentSnapshot, bool) {
	var steps []SubagentStep
	for stepIndex, step := range snapshot.Steps {
		var parts []Part
		for partIndex := range step.Parts {
			part := step.Parts[partIndex]
			var value string
			var set func(string)
			switch part.Type {
			case "text", "reasoning":
				value, set = part.Text, func(text string) { part.Text = text }
			case "tool_call":
				if part.Arguments == nil {
					continue
				}
				value, set = *part.Arguments, func(text string) { part.Arguments = &text }
			default:
				continue
			}
			if len(value) <= max {
				continue
			}
			full := len(value)
			if part.Truncated != nil && part.Truncated.Bytes > full {
				full = part.Truncated.Bytes
			}
			set(cutUTF8(value, max))
			part.Truncated = &PartTruncation{Bytes: full}
			if parts == nil {
				parts = append([]Part(nil), step.Parts...)
			}
			parts[partIndex] = part
		}
		if parts == nil {
			continue
		}
		if steps == nil {
			steps = append([]SubagentStep(nil), snapshot.Steps...)
		}
		steps[stepIndex].Parts = parts
	}
	if steps == nil {
		return snapshot, false
	}
	return &SubagentSnapshot{Agent: snapshot.Agent, Steps: steps}, true
}

// subagentEqual matches reflect.DeepEqual on a snapshot pointer, like partsEqual does for a part.
func subagentEqual(a, b *SubagentSnapshot) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || a.Agent != b.Agent || (a.Steps == nil) != (b.Steps == nil) || len(a.Steps) != len(b.Steps) {
		return false
	}
	for index := range a.Steps {
		x, y := a.Steps[index], b.Steps[index]
		if x.ID != y.ID || x.Status != y.Status || !partListEqual(x.Parts, y.Parts) {
			return false
		}
	}
	return true
}

func partListEqual(a, b []Part) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for index := range a {
		if !partsEqual(a[index], b[index]) {
			return false
		}
	}
	return true
}
