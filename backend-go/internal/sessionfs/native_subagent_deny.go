package sessionfs

import (
	"encoding/json"
	"strings"
)

func subagentDenyAgents(fields map[string]json.RawMessage, settingPath, section string) ([]string, error) {
	raw, exists := fields["deny_agents"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, configError("%s: %s.deny_agents must be an array of agent names", settingPath, section)
	}
	cleaned := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		if entry == "" {
			return nil, configError("%s: %s.deny_agents[%d] must be a non-empty string", settingPath, section, index)
		}
		if !subagentNamePattern.MatchString(entry) {
			return nil, configError("%s: %s.deny_agents[%d]: the name must be 1 to 64 letters, digits, underscores or hyphens", settingPath, section, index)
		}
		if _, duplicate := seen[entry]; duplicate {
			return nil, configError("%s: %s.deny_agents lists %s twice", settingPath, section, entry)
		}
		seen[entry] = struct{}{}
		cleaned = append(cleaned, entry)
	}
	return cleaned, nil
}

func withoutDeniedSubagents(agents []NativeSubagentAgent, denied []string) []NativeSubagentAgent {
	if len(denied) == 0 {
		return agents
	}
	kept := agents[:0:0]
	for _, agent := range agents {
		if !containsString(denied, agent.Name) {
			kept = append(kept, agent)
		}
	}
	return kept
}
