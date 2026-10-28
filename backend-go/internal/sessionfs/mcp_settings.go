package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// MCPServerReference selects an MCP server configuration for one session.
type MCPServerReference struct {
	ID         string
	ConfigPath string
	Enabled    bool
	DenyTools  []string
}

var mcpServerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func normalizeMCPServers(raw json.RawMessage, cwd, settingPath string) ([]MCPServerReference, error) {
	if len(raw) == 0 {
		return []MCPServerReference{}, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, configError("%s: mcp_servers must be an array", settingPath)
	}
	servers := make([]MCPServerReference, 0, len(entries))
	seenIDs := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		entryPath := fmt.Sprintf("%s: mcp_servers[%d]", settingPath, index)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &fields); err != nil || fields == nil {
			return nil, configError("%s must be an object", entryPath)
		}
		if err := rejectRemovedApproval(fields, fmt.Sprintf("mcp_servers[%d]", index), settingPath); err != nil {
			return nil, err
		}
		if err := rejectUnknownKeys(fields, entryPath, []string{"id", "config_path", "enabled", "deny_tools"}); err != nil {
			return nil, err
		}
		id, err := mcpRequiredString(fields["id"], fmt.Sprintf("mcp_servers[%d].id", index), settingPath)
		if err != nil {
			return nil, err
		}
		if !mcpServerIDPattern.MatchString(id) {
			return nil, configError("%s.id may contain only letters, digits, underscores, and hyphens", entryPath)
		}
		if _, exists := seenIDs[id]; exists {
			return nil, configError("%s.id duplicates %s", entryPath, id)
		}
		seenIDs[id] = struct{}{}

		configuredPath, err := mcpRequiredString(fields["config_path"], fmt.Sprintf("mcp_servers[%d].config_path", index), settingPath)
		if err != nil {
			return nil, err
		}
		configPath := configuredPath
		if !filepath.IsAbs(configPath) {
			configPath = filepath.Join(cwd, configPath)
		}
		configPath = filepath.Clean(configPath)

		enabled := false
		if value, exists := fields["enabled"]; exists {
			if err := json.Unmarshal(value, &enabled); err != nil || strings.TrimSpace(string(value)) == "null" {
				return nil, configError("%s.enabled must be a boolean", entryPath)
			}
		}

		denyTools := []string{}
		if value, exists := fields["deny_tools"]; exists && strings.TrimSpace(string(value)) != "null" {
			var rawNames []json.RawMessage
			if err := json.Unmarshal(value, &rawNames); err != nil || rawNames == nil {
				return nil, configError("%s.deny_tools must be an array", entryPath)
			}
			denyTools = make([]string, 0, len(rawNames))
			seenNames := make(map[string]struct{}, len(rawNames))
			for denyIndex, rawName := range rawNames {
				name, err := mcpRequiredString(rawName, fmt.Sprintf("mcp_servers[%d].deny_tools[%d]", index, denyIndex), settingPath)
				if err != nil {
					return nil, err
				}
				if _, exists := seenNames[name]; exists {
					return nil, configError("%s.deny_tools must not contain duplicates", entryPath)
				}
				seenNames[name] = struct{}{}
				denyTools = append(denyTools, name)
			}
		}
		servers = append(servers, MCPServerReference{ID: id, ConfigPath: configPath, Enabled: enabled, DenyTools: denyTools})
	}
	return servers, nil
}

func mcpRequiredString(raw json.RawMessage, field, settingPath string) (string, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
		return "", configError("%s: %s must be a non-empty string", settingPath, field)
	}
	return strings.TrimSpace(value), nil
}
