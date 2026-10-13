package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestWriteCreatesUpdatesNoopsAndBacksUp(t *testing.T) {
	root := t.TempDir()
	settings := testWriteSettings(root, 1024)
	tool, err := NewWriteTool(settings, NewPathLocks())
	if err != nil {
		t.Fatalf("NewWriteTool() error = %v", err)
	}
	path := "nested/deeper/file.txt"
	created := runNativeMutation(t, tool, root, "write-create", map[string]any{"path": path, "content": "created\n"})
	assertMutationStatus(t, created, "ok")
	if got := mutationInfo(created, "action"); got != "created" {
		t.Fatalf("create action = %q", got)
	}
	file := filepath.Join(root, filepath.FromSlash(path))
	if got, err := os.ReadFile(file); err != nil || string(got) != "created\n" {
		t.Fatalf("created content = %q, error = %v", got, err)
	}

	updated := runNativeMutation(t, tool, root, "write-update", map[string]any{"path": path, "content": "replacement\n"})
	assertMutationStatus(t, updated, "ok")
	if got := mutationInfo(updated, "action"); got != "updated" {
		t.Fatalf("update action = %q", got)
	}
	backupPath := mutationInfo(updated, "backup_path")
	if !strings.HasPrefix(backupPath, "backups/nested/deeper/file.txt.bak-") {
		t.Fatalf("backup path = %q", backupPath)
	}
	backup, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(backupPath)))
	if err != nil || string(backup) != "created\n" {
		t.Fatalf("backup content = %q, error = %v", backup, err)
	}

	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := runNativeMutation(t, tool, root, "write-unchanged", map[string]any{"path": path, "content": "replacement\n"})
	assertMutationStatus(t, unchanged, "ok")
	if got := mutationInfo(unchanged, "action"); got != "unchanged" {
		t.Fatalf("unchanged action = %q", got)
	}
	after, err := os.Stat(file)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("unchanged write replaced the target: before=%v after=%v error=%v", before, after, err)
	}
}

func TestWriteValidatesBase64AndSizeBeforeCreatingParents(t *testing.T) {
	root := t.TempDir()
	settings := testWriteSettings(root, 2)
	tool, err := NewWriteTool(settings, NewPathLocks())
	if err != nil {
		t.Fatalf("NewWriteTool() error = %v", err)
	}
	invalid := runNativeMutation(t, tool, root, "write-invalid-base64", map[string]any{
		"path": "invalid/nested/file.bin", "content": "!!!", "contentEncoding": "base64",
	})
	assertMutationStatus(t, invalid, "error")
	if !strings.Contains(invalid, "error_code: invalid_base64") {
		t.Fatalf("invalid base64 output = %s", invalid)
	}
	if _, err := os.Stat(filepath.Join(root, "invalid")); !os.IsNotExist(err) {
		t.Fatalf("invalid base64 created a parent: %v", err)
	}

	oversized := runNativeMutation(t, tool, root, "write-oversized", map[string]any{
		"path": "too-large/nested/file.txt", "content": "abc",
	})
	assertMutationStatus(t, oversized, "blocked")
	if !strings.Contains(oversized, "error_code: size_limit") {
		t.Fatalf("oversized output = %s", oversized)
	}
	if _, err := os.Stat(filepath.Join(root, "too-large")); !os.IsNotExist(err) {
		t.Fatalf("oversized write created a parent: %v", err)
	}

	binary := runNativeMutation(t, tool, root, "write-binary", map[string]any{
		"path": "binary.dat", "content": " \nAP8= \n", "contentEncoding": "base64",
	})
	assertMutationStatus(t, binary, "ok")
	got, err := os.ReadFile(filepath.Join(root, "binary.dat"))
	if err != nil || string(got) != string([]byte{0, 255}) {
		t.Fatalf("binary content = %v, error = %v", got, err)
	}
}

func TestWriteBase64CanonicalLength(t *testing.T) {
	if _, err := writeContentBytes("AAAA=", "base64"); err == nil {
		t.Fatal("base64 with invalid total length was accepted")
	}
	data, err := writeContentBytes("AP8", "base64")
	if err != nil || string(data) != string([]byte{0, 255}) {
		t.Fatalf("unpadded standard base64 = %v, error = %v", data, err)
	}
}

func TestEditPreservesLineEndingsAndBacksUp(t *testing.T) {
	root := t.TempDir()
	settings := testEditSettings(root, 1024)
	tool, err := NewEditTool(settings, NewPathLocks())
	if err != nil {
		t.Fatalf("NewEditTool() error = %v", err)
	}
	file := filepath.Join(root, "source.txt")
	original := "one\r\ntwo\r\n"
	if err := os.WriteFile(file, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runNativeMutation(t, tool, root, "edit-crlf", map[string]any{
		"path": "source.txt", "oldText": "two\n", "newText": "two\nthree\n",
	})
	assertMutationStatus(t, output, "ok")
	if got := mutationInfo(output, "line_ending"); got != "crlf" {
		t.Fatalf("line ending = %q", got)
	}
	if got := mutationInfo(output, "start_line"); got != "2" {
		t.Fatalf("start line = %q", got)
	}
	if got := mutationInfo(output, "end_line"); got != "3" {
		t.Fatalf("end line = %q", got)
	}
	updated, err := os.ReadFile(file)
	if err != nil || string(updated) != "one\r\ntwo\r\nthree\r\n" {
		t.Fatalf("edited content = %q, error = %v", updated, err)
	}
	backupPath := mutationInfo(output, "backup_path")
	backup, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(backupPath)))
	if err != nil || string(backup) != original {
		t.Fatalf("backup content = %q, error = %v", backup, err)
	}
}

func TestEditReportsNoMatchAndAmbiguousMatchesWithoutMutation(t *testing.T) {
	root := t.TempDir()
	settings := testEditSettings(root, 1024)
	tool, err := NewEditTool(settings, NewPathLocks())
	if err != nil {
		t.Fatalf("NewEditTool() error = %v", err)
	}
	file := filepath.Join(root, "code.txt")
	original := "function main() {\n    return 1;\n}\n"
	if err := os.WriteFile(file, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	miss := runNativeMutation(t, tool, root, "edit-no-match", map[string]any{
		"path": "code.txt", "oldText": "function main() {\n  return 1;\n}", "newText": "x",
	})
	assertMutationStatus(t, miss, "not_found")
	if mutationInfo(miss, "error_code") != "no_match" || mutationInfo(miss, "nearest_line") != "1" || mutationInfo(miss, "difference") != "whitespace" {
		t.Fatalf("no-match diagnostics = %s", miss)
	}
	if got, _ := os.ReadFile(file); string(got) != original {
		t.Fatalf("no-match modified file: %q", got)
	}

	ambiguousFile := filepath.Join(root, "repeat.txt")
	ambiguousOriginal := "value = 1;\nvalue = 2;\nvalue = 1;\n"
	if err := os.WriteFile(ambiguousFile, []byte(ambiguousOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	ambiguous := runNativeMutation(t, tool, root, "edit-ambiguous", map[string]any{
		"path": "repeat.txt", "oldText": "value = 1;", "newText": "value = 9;",
	})
	assertMutationStatus(t, ambiguous, "error")
	if mutationInfo(ambiguous, "error_code") != "ambiguous_match" || mutationInfo(ambiguous, "occurrences") != "2" {
		t.Fatalf("ambiguous output = %s", ambiguous)
	}
	if got, _ := os.ReadFile(ambiguousFile); string(got) != ambiguousOriginal {
		t.Fatalf("ambiguous edit modified file: %q", got)
	}
	all := runNativeMutation(t, tool, root, "edit-replace-all", map[string]any{
		"path": "repeat.txt", "oldText": "value = 1;", "newText": "value = 9;", "replaceAll": true,
	})
	assertMutationStatus(t, all, "ok")
	if mutationInfo(all, "replacements") != "2" {
		t.Fatalf("replace-all output = %s", all)
	}
}

func TestEditRejectsBinaryMissingTargetsAndSizeLimit(t *testing.T) {
	root := t.TempDir()
	settings := testEditSettings(root, 8)
	tool, err := NewEditTool(settings, NewPathLocks())
	if err != nil {
		t.Fatalf("NewEditTool() error = %v", err)
	}
	missing := runNativeMutation(t, tool, root, "edit-missing", map[string]any{"path": "missing.txt", "oldText": "a", "newText": "b"})
	assertMutationStatus(t, missing, "not_found")
	if mutationInfo(missing, "error_code") != "not_found" {
		t.Fatalf("missing output = %s", missing)
	}

	binaryFile := filepath.Join(root, "binary.bin")
	binaryData := []byte{0xff, 0xfe, 'A'}
	if err := os.WriteFile(binaryFile, binaryData, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := runNativeMutation(t, tool, root, "edit-binary", map[string]any{"path": "binary.bin", "oldText": "A", "newText": "B"})
	assertMutationStatus(t, binary, "unsupported_encoding")
	if mutationInfo(binary, "error_code") != "binary_file" {
		t.Fatalf("binary output = %s", binary)
	}
	if got, _ := os.ReadFile(binaryFile); string(got) != string(binaryData) {
		t.Fatalf("binary file changed: %v", got)
	}

	largeFile := filepath.Join(root, "large.txt")
	if err := os.WriteFile(largeFile, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := runNativeMutation(t, tool, root, "edit-too-large", map[string]any{"path": "large.txt", "oldText": "0", "newText": "1"})
	assertMutationStatus(t, large, "blocked")
	if mutationInfo(large, "bytes") != "10" || mutationInfo(large, "max_bytes") != "8" {
		t.Fatalf("size-limit output = %s", large)
	}
}

func TestMutationsRejectEscapesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outsideRoot := t.TempDir()
	outside := filepath.Join(outsideRoot, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTool, err := NewWriteTool(testWriteSettings(root, 1024), NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	editTool, err := NewEditTool(testEditSettings(root, 1024), NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	escape := runNativeMutation(t, writeTool, root, "write-escape", map[string]any{"path": "../outside.txt", "content": "changed"})
	assertMutationStatus(t, escape, "forbidden")
	if mutationInfo(escape, "error_code") != "path_outside_workspace" {
		t.Fatalf("escape output = %s", escape)
	}
	if got, _ := os.ReadFile(outside); string(got) != "outside\n" {
		t.Fatalf("outside file changed: %q", got)
	}

	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	blocked := runNativeMutation(t, editTool, root, "edit-symlink", map[string]any{"path": "link.txt", "oldText": "outside", "newText": "changed"})
	assertMutationStatus(t, blocked, "forbidden")
	if mutationInfo(blocked, "error_code") != "symlink_forbidden" {
		t.Fatalf("symlink output = %s", blocked)
	}
	if got, _ := os.ReadFile(outside); string(got) != "outside\n" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestWriteBackupQuotaAndNullBackupDirectory(t *testing.T) {
	root := t.TempDir()
	settings := testWriteSettings(root, 1024)
	settings.BackupMaxCount = 1
	tool, err := NewWriteTool(settings, NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	path := "history.txt"
	for index, content := range []string{"v0\n", "v1\n", "v2\n"} {
		output := runNativeMutation(t, tool, root, "write-history", map[string]any{"path": path, "content": content})
		assertMutationStatus(t, output, "ok")
		if index > 0 {
			if backups, err := filepath.Glob(filepath.Join(root, "backups", "history.txt.bak-*")); err != nil || len(backups) != 1 {
				t.Fatalf("backup quota: paths=%v error=%v", backups, err)
			}
		}
	}
	backups, err := filepath.Glob(filepath.Join(root, "backups", "history.txt.bak-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("retained backups = %v, error = %v", backups, err)
	}
	if data, err := os.ReadFile(backups[0]); err != nil || string(data) != "v1\n" {
		t.Fatalf("newest retained backup = %q, error = %v", data, err)
	}

	sidecarRoot := t.TempDir()
	sidecarSettings := testWriteSettings(sidecarRoot, 1024)
	sidecarSettings.BackupDir = nil
	sidecarTool, err := NewWriteTool(sidecarSettings, NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	_ = runNativeMutation(t, sidecarTool, sidecarRoot, "sidecar-create", map[string]any{"path": "file.txt", "content": "old"})
	updated := runNativeMutation(t, sidecarTool, sidecarRoot, "sidecar-update", map[string]any{"path": "file.txt", "content": "new"})
	assertMutationStatus(t, updated, "ok")
	if !strings.HasPrefix(mutationInfo(updated, "backup_path"), "file.txt.bak-") {
		t.Fatalf("sidecar backup path = %q", mutationInfo(updated, "backup_path"))
	}
}

func TestPathLocksSerializeSameKey(t *testing.T) {
	locks := NewPathLocks()
	releaseFirst := locks.Lock("/same/path")
	attempting := make(chan struct{})
	acquired := make(chan struct{})
	releaseSecond := make(chan func(), 1)
	go func() {
		close(attempting)
		release := locks.Lock("/same/path")
		releaseSecond <- release
		close(acquired)
	}()
	<-attempting
	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first lock was held")
	default:
	}
	releaseFirst()
	release := <-releaseSecond
	<-acquired
	release()
}

func testWriteSettings(root string, maxBytes int64) sessionfs.NativeWriteSettings {
	backupDir := filepath.Join(root, "backups")
	return sessionfs.NativeWriteSettings{
		Policy:   sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		MaxBytes: maxBytes,
		NativeMutationSettings: sessionfs.NativeMutationSettings{
			BackupDir:           &backupDir,
			BackupMaxCount:      5,
			BackupMaxTotalBytes: 10 * 1024 * 1024,
			Fsync:               true,
		},
	}
}

func testEditSettings(root string, maxBytes int64) sessionfs.NativeEditSettings {
	write := testWriteSettings(root, maxBytes)
	return sessionfs.NativeEditSettings{
		Policy:                 write.Policy,
		MaxBytes:               maxBytes,
		NativeMutationSettings: write.NativeMutationSettings,
	}
}

func runNativeMutation(t *testing.T, tool tools.Tool, root, callID string, arguments map[string]any) string {
	t.Helper()
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: callID, WorkspaceRoot: root})
	if err != nil {
		t.Fatalf("execute %s: %v", callID, err)
	}
	return output
}

func assertMutationStatus(t *testing.T, output, want string) {
	t.Helper()
	line, _, _ := strings.Cut(output, "\n")
	var header struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &header); err != nil {
		t.Fatalf("invalid result header %q: %v", line, err)
	}
	if header.Status != want {
		t.Fatalf("status = %q, want %q; output: %s", header.Status, want, output)
	}
}

func mutationInfo(output, key string) string {
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, key+": "); ok {
			return value
		}
	}
	return ""
}

func BenchmarkNativeEditWithBackupOn128KiBFile(b *testing.B) {
	root := b.TempDir()
	content := strings.Repeat("x", 64*1024) + "\nneedle\n" + strings.Repeat("y", 64*1024)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(content), 0o600); err != nil {
		b.Fatal(err)
	}
	settings := testEditSettings(root, int64(len(content)*2))
	settings.BackupMaxCount = 1
	tool, err := NewEditTool(settings, NewPathLocks())
	if err != nil {
		b.Fatal(err)
	}
	execution := tools.ExecutionContext{CallID: "benchmark-edit", WorkspaceRoot: root}
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		oldText, newText := "needle", "changed"
		if index%2 == 1 {
			oldText, newText = newText, oldText
		}
		_, err := tool.Execute(context.Background(), map[string]any{"path": "large.txt", "oldText": oldText, "newText": newText}, execution)
		if err != nil {
			b.Fatal(err)
		}
	}
}
