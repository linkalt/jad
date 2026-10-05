package mcp

import "strings"

var inheritedEnvironmentVariables = map[string][]string{
	"windows": {
		"APPDATA",
		"HOMEDRIVE",
		"HOMEPATH",
		"LOCALAPPDATA",
		"PATH",
		"PROCESSOR_ARCHITECTURE",
		"SYSTEMDRIVE",
		"SYSTEMROOT",
		"TEMP",
		"USERNAME",
		"USERPROFILE",
		"PROGRAMFILES",
	},
	"default": {"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "USER"},
}

func defaultEnvironment(source map[string]string, goos string) map[string]string {
	platform := "default"
	if strings.EqualFold(goos, "windows") {
		platform = "windows"
	}
	keys := inheritedEnvironmentVariables[platform]
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		value, exists := source[key]
		if !exists && platform == "windows" {
			for sourceKey, sourceValue := range source {
				if strings.EqualFold(sourceKey, key) {
					value, exists = sourceValue, true
					break
				}
			}
		}
		if exists && !strings.HasPrefix(value, "()") {
			result[key] = value
		}
	}
	return result
}
