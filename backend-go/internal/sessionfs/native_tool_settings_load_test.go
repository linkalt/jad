package sessionfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSessionConfigNormalizesWriteEditAndProfileToolStorage(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := `{"schema_version":1,"cwd":"` + rootDir + `","paths":{"active_profile":"work","profiles":{"work":{"tool_storage":"custom-tools"}}},"tools":{"native":{"write":{"max_bytes":128,"backup_max_count":3,"backup_max_total_bytes":512,"backup_dir":"write-backups","fsync":false,"paths":{"allow":["allowed"]}},"edit":{"max_bytes":64}}}}`
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(`{"model":"gpt-test","api_key_env":"TEST_KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	location := Location{SessionID: "session-1", Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	wantToolStorage := filepath.Join(rootDir, "custom-tools")
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
		t.Fatalf("edit did not inherit write fsync: write=%t edit=%t", config.NativeWrite.Fsync, config.NativeEdit.Fsync)
	}
	if len(config.NativeWrite.Policy.Paths) != 1 || len(config.NativeEdit.Policy.Paths) != 1 || config.NativeWrite.Policy.Paths[0].Path != filepath.Join(rootDir, "allowed") || config.NativeEdit.Policy.Paths[0].Path != filepath.Join(rootDir, "allowed") {
		t.Fatalf("path policy inheritance = write:%+v edit:%+v", config.NativeWrite.Policy, config.NativeEdit.Policy)
	}
}

func TestLoadSessionConfigRejectsTopLevelToolStorage(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := `{"schema_version":1,"paths":{"tool_storage":"tools"}}`
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(`{"model":"gpt-test","api_key_env":"TEST_KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	location := Location{SessionID: "session-1", Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}
	if _, err := LoadSessionConfig(location, "", ""); err == nil {
		t.Fatal("LoadSessionConfig() accepted top-level paths.tool_storage")
	}
}
