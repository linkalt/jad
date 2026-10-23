package sessionfs

import (
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// loadSetting loads a catalog-layout session built from the given object, so a test goes
// through the whole loader and its key allow-lists rather than one normalizer. "tools"
// becomes the toolset's native block, "mcp_servers" its mcp block (one server file per
// entry id), and every other key the session's settings.
func loadSetting(t *testing.T, extra map[string]any) (SessionConfig, error) {
	t.Helper()
	spec := layouttest.Spec{Settings: map[string]any{}}
	for key, value := range extra {
		switch key {
		case "tools":
			spec.Native = value.(map[string]any)["native"].(map[string]any)
			if extension, ok := value.(map[string]any)["extension"].(map[string]any); ok {
				spec.Extension = extension
			}
		case "mcp_servers":
			spec.MCP = map[string]map[string]any{}
			for _, entry := range value.([]any) {
				server := entry.(map[string]any)
				spec.MCP["m/"+server["id"].(string)] = map[string]any{}
			}
		default:
			spec.Settings[key] = value
		}
	}
	return loadLayoutSpec(t, spec)
}

func nativeTools(tools map[string]any) map[string]any {
	return map[string]any{"tools": map[string]any{"native": tools}}
}
