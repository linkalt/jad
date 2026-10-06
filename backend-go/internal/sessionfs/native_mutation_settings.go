package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	defaultNativeWriteMaxBytes          int64 = maxNativeToolBytes
	defaultNativeEditMaxBytes           int64 = 1024 * 1024
	defaultNativeMutationBackupMaxCount int64 = 5
	maxNativeMutationBackupMaxCount     int64 = 1000
	defaultNativeMutationBackupMaxBytes int64 = 10 * 1024 * 1024
	maxNativeMutationBackupMaxBytes     int64 = 1024 * 1024 * 1024
	nativeMutationBackupDirectoryName         = "backups"
)

// NativeMutationSettings contains backup and durability policy shared by the
// native write and edit tools. A nil BackupDir keeps backups beside the target.
type NativeMutationSettings struct {
	BackupDir           *string
	BackupMaxCount      int64
	BackupMaxTotalBytes int64
	Fsync               bool
}

// NativeWriteSettings contains the normalized policy for native.write.
type NativeWriteSettings struct {
	Policy   NativePathPolicy
	MaxBytes int64
	// Approval is "off" (default) or "ask"; it is never inherited from another tool.
	Approval string
	NativeMutationSettings
}

// NativeEditSettings contains the normalized policy for native.edit.
type NativeEditSettings struct {
	Policy   NativePathPolicy
	MaxBytes int64
	// Approval is "off" (default) or "ask"; edit does not inherit write's.
	Approval string
	NativeMutationSettings
}

func normalizeNativeWriteEditSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath, cwd, sessionDir, toolStorageDir string,
) (*NativeWriteSettings, *NativeEditSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists {
		return nil, nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, nil, err
	}

	backupBase := cwd
	defaultBackupDir := filepath.Join(sessionDir, nativeMutationBackupDirectoryName)
	if toolStorageDir != "" {
		backupBase = toolStorageDir
		defaultBackupDir = filepath.Join(toolStorageDir, nativeMutationBackupDirectoryName)
	}
	defaultMutation := NativeMutationSettings{
		BackupDir:           stringPointer(defaultBackupDir),
		BackupMaxCount:      defaultNativeMutationBackupMaxCount,
		BackupMaxTotalBytes: defaultNativeMutationBackupMaxBytes,
		Fsync:               true,
	}
	defaultPolicy := NativePathPolicy{Paths: []NativePathRule{}}

	var write *NativeWriteSettings
	if raw, exists := nativeFields["write"]; exists {
		write, err = normalizeNativeWriteSettings(
			raw,
			"write",
			settingPath,
			globalJadRoot,
			backupBase,
			defaultNativeWriteMaxBytes,
			defaultMutation,
			defaultPolicy,
		)
		if err != nil {
			return nil, nil, err
		}
	}

	var edit *NativeEditSettings
	if raw, exists := nativeFields["edit"]; exists {
		mutationFallback := defaultMutation
		policyFallback := defaultPolicy
		if write != nil {
			mutationFallback = write.NativeMutationSettings
			policyFallback = cloneNativePathPolicy(write.Policy)
		}
		edit, err = normalizeNativeEditSettings(
			raw,
			settingPath,
			globalJadRoot,
			backupBase,
			defaultNativeEditMaxBytes,
			mutationFallback,
			policyFallback,
		)
		if err != nil {
			return nil, nil, err
		}
	}
	return write, edit, nil
}

func normalizeNativeWriteSettings(
	raw json.RawMessage,
	toolName, settingPath, globalJadRoot, backupBase string,
	maxBytesFallback int64,
	mutationFallback NativeMutationSettings,
	policyFallback NativePathPolicy,
) (*NativeWriteSettings, error) {
	fields, err := decodeConfigObject(raw, settingPath+": tools.native."+toolName)
	if err != nil {
		return nil, err
	}
	if err := validateNativeMutationKeys(fields, toolName, settingPath); err != nil {
		return nil, err
	}
	maxBytes, err := configuredInteger(fields, "max_bytes", maxBytesFallback, 1, maxNativeToolBytes, settingPath, "tools.native."+toolName)
	if err != nil {
		return nil, err
	}
	policy, err := normalizeMutationPathPolicy(fields, toolName, settingPath, globalJadRoot, policyFallback)
	if err != nil {
		return nil, err
	}
	mutation, err := normalizeNativeMutationSettings(fields, toolName, settingPath, backupBase, mutationFallback)
	if err != nil {
		return nil, err
	}
	approval, err := normalizeApprovalMode(fields, "tools.native."+toolName, settingPath)
	if err != nil {
		return nil, err
	}
	return &NativeWriteSettings{Policy: policy, MaxBytes: maxBytes, Approval: approval, NativeMutationSettings: mutation}, nil
}

func normalizeNativeEditSettings(
	raw json.RawMessage,
	settingPath, globalJadRoot, backupBase string,
	maxBytesFallback int64,
	mutationFallback NativeMutationSettings,
	policyFallback NativePathPolicy,
) (*NativeEditSettings, error) {
	const toolName = "edit"
	fields, err := decodeConfigObject(raw, settingPath+": tools.native."+toolName)
	if err != nil {
		return nil, err
	}
	if err := validateNativeMutationKeys(fields, toolName, settingPath); err != nil {
		return nil, err
	}
	maxBytes, err := configuredInteger(fields, "max_bytes", maxBytesFallback, 1, maxNativeToolBytes, settingPath, "tools.native."+toolName)
	if err != nil {
		return nil, err
	}
	policy, err := normalizeMutationPathPolicy(fields, toolName, settingPath, globalJadRoot, policyFallback)
	if err != nil {
		return nil, err
	}
	mutation, err := normalizeNativeMutationSettings(fields, toolName, settingPath, backupBase, mutationFallback)
	if err != nil {
		return nil, err
	}
	approval, err := normalizeApprovalMode(fields, "tools.native."+toolName, settingPath)
	if err != nil {
		return nil, err
	}
	return &NativeEditSettings{Policy: policy, MaxBytes: maxBytes, Approval: approval, NativeMutationSettings: mutation}, nil
}

func validateNativeMutationKeys(fields map[string]json.RawMessage, toolName, settingPath string) error {
	for key := range fields {
		switch key {
		case "max_bytes", "paths", "backup_max_count", "backup_max_total_bytes", "backup_dir", "fsync", "approval":
		default:
			return configError("%s: tools.native.%s.%s is not supported", settingPath, toolName, key)
		}
	}
	return nil
}

func normalizeMutationPathPolicy(
	fields map[string]json.RawMessage,
	toolName, settingPath, globalJadRoot string,
	fallback NativePathPolicy,
) (NativePathPolicy, error) {
	raw, exists := fields["paths"]
	if !exists {
		return cloneNativePathPolicy(fallback), nil
	}
	policy, err := normalizeNativePathPolicy(raw, globalJadRoot, settingPath+": tools.native."+toolName+".paths")
	if err != nil {
		return NativePathPolicy{}, err
	}
	return policy, nil
}

func normalizeNativeMutationSettings(
	fields map[string]json.RawMessage,
	toolName, settingPath, backupBase string,
	fallback NativeMutationSettings,
) (NativeMutationSettings, error) {
	count, err := configuredInteger(
		fields,
		"backup_max_count",
		fallback.BackupMaxCount,
		1,
		maxNativeMutationBackupMaxCount,
		settingPath,
		"tools.native."+toolName,
	)
	if err != nil {
		return NativeMutationSettings{}, err
	}
	maxBytes, err := configuredInteger(
		fields,
		"backup_max_total_bytes",
		fallback.BackupMaxTotalBytes,
		0,
		maxNativeMutationBackupMaxBytes,
		settingPath,
		"tools.native."+toolName,
	)
	if err != nil {
		return NativeMutationSettings{}, err
	}
	fsync := fallback.Fsync
	if raw, exists := fields["fsync"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &fsync); err != nil {
			return NativeMutationSettings{}, configError("%s: tools.native.%s.fsync must be a boolean", settingPath, toolName)
		}
	}

	backupDir := cloneStringPointer(fallback.BackupDir)
	if raw, exists := fields["backup_dir"]; exists {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "null" {
			backupDir = nil
		} else {
			var configured string
			if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
				return NativeMutationSettings{}, configError("%s: tools.native.%s.backup_dir must be a path string, or null to keep each backup beside its file", settingPath, toolName)
			}
			configured = expandHome(strings.TrimSpace(configured))
			if !filepath.IsAbs(configured) {
				configured = filepath.Join(backupBase, configured)
			}
			absolute, err := filepath.Abs(configured)
			if err != nil {
				return NativeMutationSettings{}, fmt.Errorf("resolve tools.native.%s.backup_dir: %w", toolName, err)
			}
			backupDir = stringPointer(filepath.Clean(absolute))
		}
	}
	return NativeMutationSettings{
		BackupDir:           backupDir,
		BackupMaxCount:      count,
		BackupMaxTotalBytes: maxBytes,
		Fsync:               fsync,
	}, nil
}

func cloneNativePathPolicy(policy NativePathPolicy) NativePathPolicy {
	return NativePathPolicy{
		Paths:             append([]NativePathRule(nil), policy.Paths...),
		UnrestrictedPaths: policy.UnrestrictedPaths,
	}
}

func stringPointer(value string) *string { return &value }

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
