package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"slices"
)

const (
	defaultNativeApplyPatchMaxBytes      int64 = 1024 * 1024
	defaultNativeApplyPatchMaxFiles      int64 = 50
	maxNativeApplyPatchMaxFiles          int64 = 1000
	defaultNativeApplyPatchMaxPatchBytes int64 = 1024 * 1024
	nativeApplyPatchSection                    = "tools.native.apply_patch"
)

var nativeApplyPatchKeys = []string{"max_bytes", "max_files", "max_patch_bytes", "paths", "backup_max_count", "backup_max_total_bytes", "backup_dir", "fsync"}

// NativeApplyPatchSettings are the normalized tools.native.apply_patch settings: one call
// adds, changes, moves and deletes several files with a patch, all checked before any
// file is written.
type NativeApplyPatchSettings struct {
	Policy NativePathPolicy
	// MaxBytes bounds each file read or written by the patch.
	MaxBytes int64
	// MaxFiles bounds the file operations in one patch.
	MaxFiles int64
	// MaxPatchBytes bounds the patch text itself.
	MaxPatchBytes int64
	NativeMutationSettings
}

func normalizeNativeApplyPatchSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir string,
) (*NativeApplyPatchSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil || !exists {
		return nil, err
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["apply_patch"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": "+nativeApplyPatchSection)
	if err != nil {
		return nil, err
	}
	if err := rejectRemovedApproval(fields, nativeApplyPatchSection, settingPath); err != nil {
		return nil, err
	}
	for key := range fields {
		if !slices.Contains(nativeApplyPatchKeys, key) {
			return nil, configError("%s: %s.%s is not supported", settingPath, nativeApplyPatchSection, key)
		}
	}
	maxBytes, err := configuredInteger(fields, "max_bytes", defaultNativeApplyPatchMaxBytes, 1, maxNativeToolBytes, settingPath, nativeApplyPatchSection)
	if err != nil {
		return nil, err
	}
	maxFiles, err := configuredInteger(fields, "max_files", defaultNativeApplyPatchMaxFiles, 1, maxNativeApplyPatchMaxFiles, settingPath, nativeApplyPatchSection)
	if err != nil {
		return nil, err
	}
	maxPatchBytes, err := configuredInteger(fields, "max_patch_bytes", defaultNativeApplyPatchMaxPatchBytes, 1, maxNativeToolBytes, settingPath, nativeApplyPatchSection)
	if err != nil {
		return nil, err
	}
	policy, err := normalizeMutationPathPolicy(fields, "apply_patch", settingPath, globalJadRoot, NativePathPolicy{Paths: []NativePathRule{}})
	if err != nil {
		return nil, err
	}
	backupBase, defaultMutation := defaultNativeMutationSettings(cwd, sessionDir, toolStorageDir)
	mutation, err := normalizeNativeMutationSettings(fields, "apply_patch", settingPath, backupBase, defaultMutation)
	if err != nil {
		return nil, err
	}
	return &NativeApplyPatchSettings{Policy: policy, MaxBytes: maxBytes, MaxFiles: maxFiles, MaxPatchBytes: maxPatchBytes, NativeMutationSettings: mutation}, nil
}

// defaultNativeMutationSettings returns the base that relative backup_dir values resolve
// against and the backup defaults shared by write, edit and apply_patch.
func defaultNativeMutationSettings(cwd, sessionDir, toolStorageDir string) (string, NativeMutationSettings) {
	backupBase := cwd
	defaultBackupDir := filepath.Join(sessionDir, nativeMutationBackupDirectoryName)
	if toolStorageDir != "" {
		backupBase = toolStorageDir
		defaultBackupDir = filepath.Join(toolStorageDir, nativeMutationBackupDirectoryName)
	}
	return backupBase, NativeMutationSettings{
		BackupDir:           stringPointer(defaultBackupDir),
		BackupMaxCount:      defaultNativeMutationBackupMaxCount,
		BackupMaxTotalBytes: defaultNativeMutationBackupMaxBytes,
		Fsync:               true,
	}
}
