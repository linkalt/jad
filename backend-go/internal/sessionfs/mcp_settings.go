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
	// Resources and Prompts add the tools that list and read the server's resources and
	// prompts. Both are off unless the toolset entry turns them on.
	Resources bool
	Prompts   bool
	// TokenPath is where "jad mcp login" stores the server's OAuth tokens; empty outside the
	// catalog layout, where a server cannot use OAuth sign-in.
	TokenPath string
}

// mcpOAuthDir is the folder under the state root that holds MCP OAuth tokens. The leading
// dot keeps it apart from session folders, which are named by session ID.
const mcpOAuthDir = ".mcp-oauth"

// MCPTokenPath is where the OAuth tokens of the MCP server file <category>/<name> are kept:
// <state>/.mcp-oauth/<category>/<name>.json. Every session of the layout shares them.
func MCPTokenPath(layout Layout, id string) string {
	return filepath.Join(layout.State, mcpOAuthDir, filepath.FromSlash(id)+".json")
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
		if err := rejectUnknownKeys(fields, entryPath, []string{"id", "config_path", "enabled", "deny_tools", "resources", "prompts", "token_path"}); err != nil {
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

		enabled, err := mcpOptionalBool(fields, "enabled", entryPath)
		if err != nil {
			return nil, err
		}
		resources, err := mcpOptionalBool(fields, "resources", entryPath)
		if err != nil {
			return nil, err
		}
		prompts, err := mcpOptionalBool(fields, "prompts", entryPath)
		if err != nil {
			return nil, err
		}
		tokenPath := ""
		if _, exists := fields["token_path"]; exists {
			tokenPath, err = mcpRequiredString(fields["token_path"], fmt.Sprintf("mcp_servers[%d].token_path", index), settingPath)
			if err != nil {
				return nil, err
			}
			if !filepath.IsAbs(tokenPath) {
				tokenPath = filepath.Join(cwd, tokenPath)
			}
			tokenPath = filepath.Clean(tokenPath)
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
		servers = append(servers, MCPServerReference{ID: id, ConfigPath: configPath, Enabled: enabled, DenyTools: denyTools, Resources: resources, Prompts: prompts, TokenPath: tokenPath})
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

// mcpOptionalBool reads an optional boolean key of an mcp_servers entry; a missing key is false.
func mcpOptionalBool(fields map[string]json.RawMessage, key, entryPath string) (bool, error) {
	value, exists := fields[key]
	if !exists {
		return false, nil
	}
	var result bool
	if err := json.Unmarshal(value, &result); err != nil || strings.TrimSpace(string(value)) == "null" {
		return false, configError("%s.%s must be a boolean", entryPath, key)
	}
	return result, nil
}
