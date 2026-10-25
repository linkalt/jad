package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const transcriptToolID = "native.transcript"

var transcriptInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "list":{"type":"boolean","description":"List the transcript files that can be read, with their size and time of last change."}
  },
  "additionalProperties":false
}`)

const transcriptDescription = "List the messages.jsonl transcript files this session can read, with their size and time of last change."

// TranscriptFiles are the transcript files of the profile the tool is built for.
type TranscriptFiles struct {
	Messages string
	Backup   string
}

type transcriptTool struct {
	settings sessionfs.NativeTranscriptSettings
	own      TranscriptFiles
}

// NewTranscriptTool constructs native.transcript. own is the transcript of the session and
// profile the catalog is built for.
func NewTranscriptTool(settings sessionfs.NativeTranscriptSettings, own TranscriptFiles) (tools.Tool, error) {
	validator, err := tools.CompileSchema(transcriptInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native transcript schema: %w", err)
	}
	tool := &transcriptTool{settings: settings, own: own}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          transcriptToolID,
			Source:      "native",
			Name:        "transcript",
			Description: transcriptDescription,
			InputSchema: append(json.RawMessage(nil), transcriptInputSchema...),
		},
		Validator:        validator,
		ReadOnly:         true,
		ExampleOperation: json.RawMessage(`{"list":true}`),
		Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
			return tool.execute(execution), nil
		},
	}, nil
}

func (t *transcriptTool) execute(execution tools.ExecutionContext) string {
	if execution.AgentDepth > 0 && !t.settings.AllowChildren {
		return tools.ExecutionError(execution.CallID, tools.StatusForbidden, "transcript_children_denied", "A sub-agent may not read transcripts: tools.native.transcript.allow_children is false")
	}
	var lines []string
	for _, file := range []struct{ name, path string }{{"messages", t.own.Messages}, {"backup", t.own.Backup}} {
		if file.path == "" {
			continue
		}
		info, err := os.Stat(file.path)
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s  %d bytes  %s", file.name, info.Size(), info.ModTime().UTC().Format(time.RFC3339)))
	}
	var content *tools.Content
	if len(lines) > 0 {
		content = &tools.Content{Encoding: "utf8", Text: strings.Join(lines, "\n")}
	}
	rendered, err := tools.RenderToolResult(tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: fmt.Sprintf("%d transcript files", len(lines)), Content: content})
	if err != nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "tool_output_failed", "The listing could not be rendered")
	}
	return rendered
}
