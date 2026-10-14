package sessionfs

import (
	"encoding/json"
	"path/filepath"
)

type storagePathEntry struct {
	Field string
	Path  string
}

func (p SessionStoragePaths) pathEntries() []storagePathEntry {
	return []storagePathEntry{
		{Field: "messages", Path: p.Messages},
		{Field: "messages_backup", Path: p.MessagesBackup},
		{Field: "compaction_cache", Path: p.CompactionCache},
		{Field: "usage_cache", Path: p.UsageCache},
		{Field: "cursor_cache", Path: p.CursorCache},
		{Field: "run_error_cache", Path: p.RunErrorCache},
		{Field: "statecontext", Path: p.StateContext},
		{Field: "staging", Path: p.Staging},
		{Field: "http_debug", Path: p.HTTPDebug},
	}
}

func selectProfileStoragePaths(directory string, profileSettings map[string]json.RawMessage, profile, activeProfile string, profileConfigured bool) (map[string]json.RawMessage, string) {
	if !profileConfigured {
		return nil, directory
	}
	_, hasMessages := profileSettings["messages"]
	_, hasStaging := profileSettings["staging"]
	profileStorageDir := directory
	if profile != activeProfile || hasMessages || hasStaging {
		profileStorageDir = filepath.Join(directory, "profiles", profile)
	}
	return profileSettings, profileStorageDir
}

func resolveStoragePaths(fields map[string]json.RawMessage, profileStorageDir, settingDir string, profileConfigured bool) (SessionStoragePaths, error) {
	messagesFallback := "messages.jsonl"
	stagingFallback := "staging"
	if profileConfigured {
		messagesFallback = filepath.Join(profileStorageDir, "messages.jsonl")
		stagingFallback = filepath.Join(profileStorageDir, "staging")
	}
	messages, err := resolveConfiguredPath(fields, "messages", messagesFallback, settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	staging, err := resolveConfiguredPath(fields, "staging", stagingFallback, settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	// Only profile-scoped storage keeps the backup beside the transcript; the
	// legacy layout always defaults it into the session directory.
	messagesBackupFallback := "messages.backup.jsonl"
	if profileConfigured {
		messagesBackupFallback = filepath.Join(filepath.Dir(messages), "messages.backup.jsonl")
	}
	messagesBackup, err := resolveConfiguredPath(fields, "messages_backup", messagesBackupFallback, settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	compactionCache, err := resolveConfiguredPath(fields, "compaction_cache", filepath.Join(staging, "compaction-cache.json"), settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	usageCache, err := resolveConfiguredPath(fields, "usage_cache", filepath.Join(staging, "usage-cache.json"), settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	cursorCache, err := resolveConfiguredPath(fields, "cursor_cache", filepath.Join(staging, "cursor-cache.json"), settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	runErrorCache, err := resolveConfiguredPath(fields, "run_error_cache", filepath.Join(staging, "run-error-cache.json"), settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	stateContext, err := resolveConfiguredPath(fields, "statecontext", filepath.Join(staging, "statecontext.jsonl"), settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	httpDebugFallback := filepath.Join("debug", "http")
	if profileConfigured {
		httpDebugFallback = filepath.Join(profileStorageDir, "debug", "http")
	}
	httpDebug, err := resolveConfiguredPath(fields, "http_debug", httpDebugFallback, settingDir)
	if err != nil {
		return SessionStoragePaths{}, err
	}
	return SessionStoragePaths{
		Messages:        messages,
		MessagesBackup:  messagesBackup,
		CompactionCache: compactionCache,
		UsageCache:      usageCache,
		CursorCache:     cursorCache,
		RunErrorCache:   runErrorCache,
		StateContext:    stateContext,
		Staging:         staging,
		HTTPDebug:       httpDebug,
	}, nil
}

func validateProfileStoragePaths(settingPath string, storage SessionStoragePaths) error {
	const duplicateFilesMessage = "paths.messages, paths.messages_backup, paths.compaction_cache, paths.usage_cache, paths.cursor_cache, paths.run_error_cache, paths.statecontext, and the compaction-cache backup must be different files"
	fileFields := map[string]struct{}{
		"messages": {}, "messages_backup": {}, "compaction_cache": {},
		"usage_cache": {}, "cursor_cache": {}, "run_error_cache": {}, "statecontext": {},
	}
	seen := make(map[string]struct{}, len(fileFields)+1)
	for _, entry := range storage.pathEntries() {
		if _, isFile := fileFields[entry.Field]; !isFile {
			continue
		}
		clean := filepath.Clean(entry.Path)
		if _, exists := seen[clean]; exists {
			return configError("%s: %s", settingPath, duplicateFilesMessage)
		}
		seen[clean] = struct{}{}
		if entry.Field == "compaction_cache" {
			backup := filepath.Clean(entry.Path + ".backup")
			if _, exists := seen[backup]; exists {
				return configError("%s: %s", settingPath, duplicateFilesMessage)
			}
			seen[backup] = struct{}{}
		}
	}
	return nil
}

func validateAllStoragePaths(location Location, settingPath string, paths map[string]json.RawMessage, pathProfiles []string, activeProfile string, profileConfigured bool) error {
	settingDir := filepath.Dir(settingPath)
	if !profileConfigured {
		storage, err := resolveStoragePaths(nil, location.StateDir(), settingDir, false)
		if err != nil {
			return err
		}
		return validateProfileStoragePaths(settingPath+": paths", storage)
	}

	profiles, err := rawObject(paths["profiles"], settingPath+": paths.profiles")
	if err != nil {
		return err
	}
	type owner struct {
		profile string
		field   string
	}
	owners := make(map[string]owner, len(pathProfiles)*9)
	for _, profile := range pathProfiles {
		profileSettings, err := rawObject(profiles[profile], settingPath+": paths.profiles."+profile)
		if err != nil {
			return err
		}
		selected, storageDir := selectProfileStoragePaths(location.StateDir(), profileSettings, profile, activeProfile, true)
		storage, err := resolveStoragePaths(selected, storageDir, settingDir, true)
		if err != nil {
			return err
		}
		if err := validateProfileStoragePaths(settingPath+": paths.profiles."+profile, storage); err != nil {
			return err
		}
		for _, entry := range storage.pathEntries() {
			key := filepath.Clean(entry.Path)
			if previous, exists := owners[key]; exists && previous.profile != profile {
				return configError("%s: path profiles '%s' and '%s' resolve to the same storage path (%s and %s); each profile must use distinct transcript and cache paths", settingPath, previous.profile, profile, previous.field, entry.Field)
			}
			owners[key] = owner{profile: profile, field: entry.Field}
		}
	}
	return nil
}
