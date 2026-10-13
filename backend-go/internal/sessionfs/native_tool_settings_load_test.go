package sessionfs

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigNormalizesWriteEditAndProfileToolStorage(t *testing.T) {
	rootDir := t.TempDir()
	location, fixture := layoutLocation(t, layouttest.Spec{CWD: rootDir, Settings: map[string]any{"tool_storage_dir": "custom-tools"}, Native: map[string]any{
		"write": map[string]any{"max_bytes": 128, "backup_max_count": 3, "backup_max_total_bytes": 512, "backup_dir": "write-backups", "fsync": false, "paths": map[string]any{"allow": []any{"allowed"}}},
		"edit":  map[string]any{"max_bytes": 64, "backup_max_count": 3, "backup_max_total_bytes": 512, "backup_dir": "write-backups", "fsync": false, "paths": map[string]any{"allow": []any{"allowed"}}},
	}})
	// relative paths.allow resolve against the global root, not the session cwd
	allowedRoot := fixture.Root
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	wantToolStorage := filepath.Join(rootDir, "custom-tools", "session-1", "dev", "coder", "tools")
	wantBackupDir := filepath.Join(wantToolStorage, "write-backups")
	if config.StoragePaths.ToolStorage != wantToolStorage {
		t.Fatalf("tool storage = %q, want %q", config.StoragePaths.ToolStorage, wantToolStorage)
	}
	if config.NativeWrite == nil || config.NativeEdit == nil {
		t.Fatalf("native settings = write:%+v edit:%+v", config.NativeWrite, config.NativeEdit)
	}
	if config.NativeWrite.MaxBytes != 128 || config.NativeEdit.MaxBytes != 64 || config.NativeWrite.BackupMaxCount != 3 || config.NativeEdit.BackupMaxCount != 3 || config.NativeWrite.BackupMaxTotalBytes != 512 || config.NativeEdit.BackupMaxTotalBytes != 512 {
		t.Fatalf("normalized native settings = write:%+v edit:%+v", config.NativeWrite, config.NativeEdit)
	}
	if config.NativeWrite.BackupDir == nil || *config.NativeWrite.BackupDir != wantBackupDir || config.NativeEdit.BackupDir == nil || *config.NativeEdit.BackupDir != wantBackupDir {
		t.Fatalf("backup dirs = write:%v edit:%v, want %q", config.NativeWrite.BackupDir, config.NativeEdit.BackupDir, wantBackupDir)
	}
	if config.NativeWrite.Fsync || config.NativeEdit.Fsync {
		t.Fatalf("fsync not applied: write=%t edit=%t", config.NativeWrite.Fsync, config.NativeEdit.Fsync)
	}
	if len(config.NativeWrite.Policy.Paths) != 1 || len(config.NativeEdit.Policy.Paths) != 1 || config.NativeWrite.Policy.Paths[0].Path != filepath.Join(allowedRoot, "allowed") || config.NativeEdit.Policy.Paths[0].Path != filepath.Join(allowedRoot, "allowed") {
		t.Fatalf("path policies = write:%+v edit:%+v", config.NativeWrite.Policy, config.NativeEdit.Policy)
	}
}

func TestLoadSessionConfigRejectsTopLevelToolStorage(t *testing.T) {
	// "paths" is not a settings key of the layout; its tool_storage is refused with it.
	_, err := loadLayoutConfig(t, layouttest.Spec{Settings: map[string]any{"paths": map[string]any{"tool_storage": "tools"}}})
	if err == nil || !strings.Contains(err.Error(), "settings may not carry paths") {
		t.Fatalf("LoadSessionConfig() error = %v, want it to refuse paths in settings", err)
	}
}
