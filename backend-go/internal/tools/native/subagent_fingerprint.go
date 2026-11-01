package native

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// subagentFingerprint identifies the agent definition a run starts with: its settings, the
// tool definitions of its catalog (which carry the gated MCP tools) and its model path and prompt.
// It is a hash, so the raw configuration never reaches the manifest. It does not cover what the
// agent inherits from the calling session (its model settings, retry or compaction); a change
// there produces no config note, and the design says so.
func subagentFingerprint(config sessionfs.NativeSubagentAgent, catalog *tools.Catalog) (string, error) {
	encoded, err := json.Marshal(struct {
		Agent       sessionfs.NativeSubagentAgent `json:"agent"`
		Definitions []tools.Definition            `json:"definitions"`
	}{Agent: config, Definitions: catalog.Definitions()})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// configChangedNote is the note a resumed run carries when its agent definition differs from the
// one that produced the source transcript. Without both fingerprints there is no comparison, so
// there is no note and nothing is said about the config.
func configChangedNote(source, current string) string {
	if source == "" || current == "" || source == current {
		return ""
	}
	return "The agent definition changed since the source run: the compared scope is the agent definition (tools, lazy list, instructions, model path, prompt), not the parent session's inherited model, retry or compaction."
}
