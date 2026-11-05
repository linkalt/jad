package sessionfs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

var (
	toolNodeNames   = []string{"native", "extension", "mcp"}
	nativeToolNames = []string{
		"read_image", "lsp", "exec", "async", "task", "memory", "request_draft", "skill", "subagent", "profile_message", "tool_smith", "transcript", "transcripts", "profile", "load_tool",
	}
	// removedNativeTools are the tools exec replaced, each with what to do instead. Their
	// keys are refused by name, never ignored, so a setting that still names one says why.
	removedNativeTools = map[string]string{
		"read":        "read files with exec (cat, sed -n, head) or a saved exec helper; delete the key",
		"write":       "write files with exec (a heredoc, or a helper); delete the key",
		"edit":        "edit files with exec (a helper or a short script); delete the key",
		"apply_patch": "apply patches with exec (git apply, patch, or a helper); delete the key",
		"notebook":    "edit notebooks with exec (a python helper using nbformat); delete the key",
		"grep":        "search with exec (rg, grep); delete the key",
		"glob":        "list files with exec (rg --files, find); delete the key",
		"bash":        "exec runs shell commands (lang bash, its default); move the settings to tools.native.exec and delete the key",
		"process":     "exec runs long commands: a call that outlasts block_ms becomes an async handle; delete the key",
		"fetch":       "fetch pages with exec (curl, or a helper); delete the key",
	}
	// extensionToolNames are the tools under tools.extension: tools that call a provider API.
	extensionToolNames = []string{"decide_systemone", "web_search"}
)

// NativeToolNames lists the names tools.native accepts, in registration order.
func NativeToolNames() []string {
	return append([]string(nil), nativeToolNames...)
}

// ExtensionToolNames lists the names tools.extension accepts.
func ExtensionToolNames() []string {
	return append([]string(nil), extensionToolNames...)
}

// validateToolSettingsShape mirrors the TypeScript loader's namespace checks.
// Individual tool normalizers validate the keys inside each known native tool.
func validateToolSettingsShape(setting map[string]json.RawMessage, settingPath string) error {
	rawTools, exists := setting["tools"]
	if !exists {
		return nil
	}
	tools, err := decodeConfigObject(rawTools, settingPath+": tools")
	if err != nil {
		return configError("%s: tools must be an object", settingPath)
	}
	toolKeys, err := orderedObjectKeys(rawTools)
	if err != nil {
		return configError("%s: tools must be an object", settingPath)
	}
	for _, key := range toolKeys {
		if !containsString(toolNodeNames, key) {
			return unsupportedToolKey(settingPath+": tools", key, toolNodeNames, "tool definitions are configured with tool_definition_paths")
		}
	}

	changed := false
	nativeRaw, nativeExists := tools["native"]
	if nativeExists && strings.TrimSpace(string(nativeRaw)) == "null" {
		// TypeScript uses `native ?? {}`. Canonicalize null here so the native
		// per-tool normalizers receive the same empty object.
		tools["native"] = json.RawMessage(`{}`)
		nativeRaw = tools["native"]
		changed = true
	}
	if nativeExists {
		if _, err := decodeConfigObject(nativeRaw, settingPath+": tools.native"); err != nil {
			return configError("%s: tools.native must be an object", settingPath)
		}
		nativeKeys, err := orderedObjectKeys(nativeRaw)
		if err != nil {
			return configError("%s: tools.native must be an object", settingPath)
		}
		for _, key := range nativeKeys {
			if hint, removed := removedNativeTools[key]; removed {
				return configError("%s: tools.native.%s was removed; %s", settingPath, key, hint)
			}
			if !containsString(nativeToolNames, key) {
				return unsupportedToolKey(settingPath+": tools.native", key, nativeToolNames, "MCP servers are configured under tools.mcp")
			}
		}
	}

	if extensionRaw, exists := tools["extension"]; exists && strings.TrimSpace(string(extensionRaw)) != "null" {
		if _, err := decodeConfigObject(extensionRaw, settingPath+": tools.extension"); err != nil {
			return configError("%s: tools.extension must be an object", settingPath)
		}
		extensionKeys, err := orderedObjectKeys(extensionRaw)
		if err != nil {
			return configError("%s: tools.extension must be an object", settingPath)
		}
		for _, key := range extensionKeys {
			if !containsString(extensionToolNames, key) {
				return unsupportedToolKey(settingPath+": tools.extension", key, extensionToolNames, "")
			}
		}
	}

	if mcpRaw, exists := tools["mcp"]; exists && strings.TrimSpace(string(mcpRaw)) != "null" {
		if _, err := decodeConfigObject(mcpRaw, settingPath+": tools.mcp"); err != nil {
			return configError("%s: tools.mcp must be an object", settingPath)
		}
	}

	if changed {
		canonical, err := json.Marshal(tools)
		if err != nil {
			return fmt.Errorf("normalize tools settings: %w", err)
		}
		setting["tools"] = canonical
	}
	return nil
}

func unsupportedToolKey(fieldPath, key string, candidates []string, hint string) error {
	if suggestion, distance := closestToolKey(key, candidates); suggestion != "" && distance <= 2 {
		return configError("%s.%s is not supported; did you mean %s?", fieldPath, key, suggestion)
	}
	sorted := append([]string(nil), candidates...)
	sort.Strings(sorted)
	detail := "supported keys are " + strings.Join(sorted, ", ")
	if hint != "" {
		detail += "; " + hint
	}
	return configError("%s.%s is not supported; %s", fieldPath, key, detail)
}

func closestToolKey(key string, candidates []string) (string, int) {
	best, bestDistance := "", len(key)+1
	for _, candidate := range candidates {
		distance := toolKeyEditDistance(key, candidate)
		if distance <= 2 && distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best, bestDistance
}

func toolKeyEditDistance(left, right string) int {
	previous := make([]int, len(right)+1)
	for column := range previous {
		previous[column] = column
	}
	for row := 1; row <= len(left); row++ {
		diagonal := previous[0]
		previous[0] = row
		for column := 1; column <= len(right); column++ {
			current := previous[column]
			cost := 1
			if left[row-1] == right[column-1] {
				cost = 0
			}
			deletion := previous[column] + 1
			insertion := previous[column-1] + 1
			substitution := diagonal + cost
			previous[column] = min(deletion, insertion, substitution)
			diagonal = current
		}
	}
	return previous[len(right)]
}
