package native

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// SubagentAnswerText joins the output text of the assistant messages in a completed
// response event. Reasoning and tool-call items carry no answer and are skipped.
func SubagentAnswerText(event json.RawMessage) string {
	var envelope struct {
		Response struct {
			Output []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(event, &envelope) != nil {
		return ""
	}
	var parts []string
	for _, item := range envelope.Response.Output {
		if item.Type != "message" || (item.Role != "" && item.Role != "assistant") {
			continue
		}
		for _, content := range item.Content {
			if content.Type == "output_text" && strings.TrimSpace(content.Text) != "" {
				parts = append(parts, content.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// transcriptAnswers is what a child's transcript says it answered.
type transcriptAnswers struct {
	// last is the text of the final response, empty when that response wrote none.
	last string
	// lastNonEmpty is the newest text any response wrote: the partial work of a child
	// that was cut off before its final answer.
	lastNonEmpty string
}

// readTranscriptAnswers scans a child's messages.jsonl for its completed responses. It
// reads one line at a time and parses only the lines that can be a completed response,
// so a transcript with large tool results costs time but not memory beyond its longest
// line.
func readTranscriptAnswers(path string) (transcriptAnswers, error) {
	file, err := os.Open(path)
	if err != nil {
		return transcriptAnswers{}, err
	}
	defer file.Close()
	var answers transcriptAnswers
	marker := []byte("response.completed")
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, marker) {
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &head) == nil && head.Type == "response.completed" {
				text := SubagentAnswerText(line)
				answers.last = text
				if strings.TrimSpace(text) != "" {
					answers.lastNonEmpty = text
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return answers, nil
		}
		if readErr != nil {
			return answers, readErr
		}
	}
}
