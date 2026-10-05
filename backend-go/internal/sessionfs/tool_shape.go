package sessionfs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

var (
	toolNodeNames   = []string{"native", "mcp"}
	nativeToolNames = []string{
		"read", "write", "edit", "grep", "glob", "bash", "process", "task", "memory", "fetch", "request_draft", "skill",
	}
)

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
			if !containsString(nativeToolNames, key) {
				return unsupportedToolKey(settingPath+": tools.native", key, nativeToolNames, "MCP servers are configured under tools.mcp")
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
	if suggestion, distance := closestToolKey(key, candidates); distance <= 2 {
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
