package sessionfs

import ()

// serverUsers lists the toolsets whose mcp object names the server file: they stop resolving
// when the file is removed. A toolset file that cannot be read is not counted.
func serverUsers(layout Layout, id string) ([]string, error) {
	toolsets, err := layout.ScanToolsets(nil)
	if err != nil {
		return nil, configError("%v", err)
	}
	var users []string
	for _, entry := range toolsets.Entries() {
		var toolset layoutToolsetFile
		if err := readLayoutFile(entry.Path, &toolset); err != nil || len(toolset.MCP) == 0 {
			continue
		}
		keys, err := orderedObjectKeys(toolset.MCP)
		if err != nil {
			continue
		}
		for _, key := range keys {
			if key == id {
				users = append(users, entry.ID())
				break
			}
		}
	}
	return users, nil
}
