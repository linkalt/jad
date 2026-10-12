package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestNormalizeNativeWriteEditSettingsDefaults(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	write, edit, err := normalizeNativeWriteEditSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"write":{},"edit":{}}}`),
	}, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, "")
	if err != nil {
		t.Fatalf("normalizeNativeWriteEditSettings() error = %v", err)
	}
	if write == nil || edit == nil {
		t.Fatalf("settings = write:%+v edit:%+v", write, edit)
	}
	if write.MaxBytes != maxNativeToolBytes || edit.MaxBytes != defaultNativeEditMaxBytes {
		t.Fatalf("max bytes = write:%d edit:%d", write.MaxBytes, edit.MaxBytes)
	}
	wantBackupDir := filepath.Join(sessionDir, nativeMutationBackupDirectoryName)
	if write.BackupDir == nil || *write.BackupDir != wantBackupDir || edit.BackupDir == nil || *edit.BackupDir != wantBackupDir {
		t.Fatalf("backup dirs = write:%v edit:%v; want %q", write.BackupDir, edit.BackupDir, wantBackupDir)
	}
	if write.BackupMaxCount != 5 || edit.BackupMaxCount != 5 ||
		write.BackupMaxTotalBytes != 10*1024*1024 || edit.BackupMaxTotalBytes != 10*1024*1024 ||
		!write.Fsync || !edit.Fsync {
		t.Fatalf("mutation defaults = write:%+v edit:%+v", write.NativeMutationSettings, edit.NativeMutationSettings)
	}
	if write.Policy.UnrestrictedPaths || edit.Policy.UnrestrictedPaths || len(write.Policy.Paths) != 0 || len(edit.Policy.Paths) != 0 {
		t.Fatalf("default path policies = write:%+v edit:%+v", write.Policy, edit.Policy)
	}
}

func TestNormalizeNativeEditSettingsDefaultsWithoutWrite(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	write, edit, err := normalizeNativeWriteEditSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"edit":{}}}`),
	}, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, "")
	if err != nil {
		t.Fatalf("normalizeNativeWriteEditSettings() error = %v", err)
	}
	if write != nil || edit == nil {
		t.Fatalf("settings = write:%+v edit:%+v", write, edit)
	}
	if edit.MaxBytes != defaultNativeEditMaxBytes || edit.BackupDir == nil || *edit.BackupDir != filepath.Join(sessionDir, nativeMutationBackupDirectoryName) {
		t.Fatalf("edit defaults = %+v", edit)
	}
}

func TestNormalizeNativeWriteEditSettingsOverridesAndNeverInherit(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	toolStorage := t.TempDir()
	write, edit, err := normalizeNativeWriteEditSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"write":{"max_bytes":128,"backup_max_count":7,"backup_max_total_bytes":0,"backup_dir":"write-backups","fsync":false,"paths":{"allow":["outside"],"deny":["private"]}},"edit":{"max_bytes":64,"backup_max_count":2,"fsync":null}}}`),
	}, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, toolStorage)
	if err != nil {
		t.Fatalf("normalizeNativeWriteEditSettings() error = %v", err)
	}
	if write == nil || edit == nil {
		t.Fatalf("settings = write:%+v edit:%+v", write, edit)
	}
	if write.MaxBytes != 128 || edit.MaxBytes != 64 || write.BackupMaxCount != 7 || edit.BackupMaxCount != 2 || write.BackupMaxTotalBytes != 0 || write.Fsync {
		t.Fatalf("normalized settings = write:%+v edit:%+v", write, edit)
	}
	// edit names only max_bytes, backup_max_count and a null fsync: everything else is edit's
	// own default, not write's 0 total bytes, false fsync or write-backups folder.
	if edit.BackupMaxTotalBytes != defaultNativeMutationBackupMaxBytes || !edit.Fsync {
		t.Fatalf("edit took write's values: %+v", edit.NativeMutationSettings)
	}
	wantBackupDir := filepath.Join(toolStorage, "write-backups")
	wantEditBackupDir := filepath.Join(toolStorage, nativeMutationBackupDirectoryName)
	if write.BackupDir == nil || *write.BackupDir != wantBackupDir || edit.BackupDir == nil || *edit.BackupDir != wantEditBackupDir {
		t.Fatalf("backup dirs = write:%v edit:%v; want %q and %q", write.BackupDir, edit.BackupDir, wantBackupDir, wantEditBackupDir)
	}
	if len(write.Policy.Paths) != 2 || len(edit.Policy.Paths) != 0 || edit.Policy.UnrestrictedPaths {
		t.Fatalf("path policies = write:%+v edit:%+v, edit must not take write's", write.Policy, edit.Policy)
	}
	if write.Policy.Paths[0].Action != "allow" || write.Policy.Paths[0].Path != filepath.Join(globalRoot, "outside") ||
		write.Policy.Paths[1].Action != "deny" || write.Policy.Paths[1].Path != filepath.Join(globalRoot, "private") {
		t.Fatalf("write path rules = %+v", write.Policy.Paths)
	}
}

func TestNormalizeNativeWriteEditSettingsRejectsInvalidValues(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	cases := []string{
		`{"native":{"write":{"max_bytes":0}}}`,
		`{"native":{"write":{"backup_max_count":0}}}`,
		`{"native":{"write":{"backup_max_count":1001}}}`,
		`{"native":{"write":{"backup_max_total_bytes":-1}}}`,
		`{"native":{"write":{"backup_max_total_bytes":1073741825}}}`,
		`{"native":{"write":{"fsync":"false"}}}`,
		`{"native":{"write":{"backup_dir":" "}}}`,
		`{"native":{"edit":{"mystery":true}}}`,
		`{"native":{"edit":{"paths":null}}}`,
	}
	for _, native := range cases {
		_, _, err := normalizeNativeWriteEditSettings(map[string]json.RawMessage{
			"tools": json.RawMessage(native),
		}, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, "")
		if err == nil {
			t.Errorf("normalizeNativeWriteEditSettings(%s) succeeded", native)
		}
	}
}

func TestNormalizeToolStorageDirProfiles(t *testing.T) {
	cwd := t.TempDir()
	profile, err := normalizeToolStorageDir(nil, "work", false, cwd, "/session/setting.json")
	if err != nil || profile != "" {
		t.Fatalf("unprofiled tool storage = %q, error = %v", profile, err)
	}
	profile, err = normalizeToolStorageDir(map[string]json.RawMessage{}, "work", true, cwd, "/session/setting.json")
	if err != nil || profile != filepath.Join(cwd, ".jad", "profiles", "work", "tools") {
		t.Fatalf("default profile tool storage = %q, error = %v", profile, err)
	}
	profile, err = normalizeToolStorageDir(map[string]json.RawMessage{
		"tool_storage": json.RawMessage(`"custom/tools"`),
	}, "work", true, cwd, "/session/setting.json")
	if err != nil || profile != filepath.Join(cwd, "custom", "tools") {
		t.Fatalf("configured profile tool storage = %q, error = %v", profile, err)
	}
	if _, err := normalizeToolStorageDir(map[string]json.RawMessage{
		"tool_storage": json.RawMessage(`"  "`),
	}, "work", true, cwd, "/session/setting.json"); err == nil {
		t.Fatal("empty profile tool storage was accepted")
	}
}
