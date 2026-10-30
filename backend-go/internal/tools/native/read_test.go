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

func newReadCatalog(t *testing.T, settings sessionfs.NativeReadSettings) *tools.Catalog {
	t.Helper()
	tool, err := NewReadTool(settings)
	if err != nil {
		t.Fatalf("NewReadTool() error = %v", err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func invokeRead(t *testing.T, catalog *tools.Catalog, workspace, callID string, arguments string) string {
	t.Helper()
	call, err := json.Marshal(struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Type: "function_call", CallID: callID, Name: "read", Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	output, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall() error = %v", err)
	}
	var result struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode function output: %v", err)
	}
	return result.Output
}

func TestNativeReadValidatesAndReadsTextWithRanges(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newReadCatalog(t, sessionfs.NativeReadSettings{
		Policy:       sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		MaxBytes:     8,
		MaxFileBytes: 1024,
		DefaultLimit: 250,
	})

	read := invokeRead(t, catalog, workspace, "read-1", `{"path":"sample.txt"}`)
	if !strings.Contains(read, `"status":"ok"`) || !strings.Contains(read, "one\ntwo") || !strings.Contains(read, "content_truncated: true") || !strings.Contains(read, "max_content_bytes: 8") {
		t.Fatalf("read output = %s", read)
	}

	byteRange := invokeRead(t, catalog, workspace, "read-2", `{"path":"sample.txt","rangeType":"BYTE","offset":0,"limit":3,"contentEncoding":"UTF-8"}`)
	if !strings.Contains(byteRange, "range_type: bytes") || !strings.Contains(byteRange, "Read bytes 0-2 from sample.txt") || !strings.Contains(byteRange, "one") {
		t.Fatalf("byte range output = %s", byteRange)
	}

	lineRange := invokeRead(t, catalog, workspace, "read-line-range", `{"path":"sample.txt","offset":2,"limit":1}`)
	if !strings.Contains(lineRange, "Read lines 2-2 from sample.txt") || !strings.Contains(lineRange, "two") || !strings.Contains(lineRange, "next_offset: 3") {
		t.Fatalf("line range output = %s", lineRange)
	}

	metadata := invokeRead(t, catalog, workspace, "read-3", `{"path":"sample.txt","includeContent":false}`)
	if !strings.Contains(metadata, "metadata_only: true") || strings.Contains(metadata, "total_line_count:") || strings.Contains(metadata, "--- CONTENT token=") {
		t.Fatalf("metadata output = %s", metadata)
	}

	invalid := invokeRead(t, catalog, workspace, "read-4", `{}`)
	if !strings.Contains(invalid, `"status":"invalid_input"`) || !strings.Contains(invalid, "missingProperty") {
		t.Fatalf("missing path output = %s", invalid)
	}
}

func TestNativeReadListsDirectoriesWithPathPolicyAndConfiguredLimit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "allowed.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "denied.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := newReadCatalog(t, sessionfs.NativeReadSettings{
		Policy:       sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(workspace, "denied.txt")}}},
		MaxBytes:     1024,
		MaxFileBytes: 4096,
		DefaultLimit: 1,
	})
	output := invokeRead(t, catalog, workspace, "directory", `{"path":"."}`)
	if !strings.Contains(output, "kind: directory") || !strings.Contains(output, "entry_count: 1") || !strings.Contains(output, "directory_entries_truncated: true") {
		t.Fatalf("directory output = %s", output)
	}
	if strings.Contains(output, "denied.txt") {
		t.Fatalf("directory output leaked a denied entry: %s", output)
	}
}

func TestNativeReadDirectoryHidesDeniedEntriesEvenBelowTheLimit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "allowed.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "denied.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newReadCatalog(t, sessionfs.NativeReadSettings{
		Policy:       sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(workspace, "denied.txt")}}},
		MaxBytes:     1024,
		MaxFileBytes: 4096,
		DefaultLimit: 10,
	})
	output := invokeRead(t, catalog, workspace, "directory", `{"path":"."}`)
	if !strings.Contains(output, "entry_count: 1") || strings.Contains(output, "denied.txt") {
		t.Fatalf("directory output leaked a denied entry: %s", output)
	}
}

func TestNativeReadChoosesBase64ForBinaryAndHonorsExplicitEncoding(t *testing.T) {
	workspace := t.TempDir()
	binary := []byte{0xff, 0xfe, 0x00, 0x01}
	if err := os.WriteFile(filepath.Join(workspace, "binary.dat"), binary, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newReadCatalog(t, sessionfs.NativeReadSettings{
		Policy:       sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		MaxBytes:     1024,
		MaxFileBytes: 1024,
		DefaultLimit: 250,
	})

	automatic := invokeRead(t, catalog, workspace, "binary-1", `{"path":"binary.dat"}`)
	if !strings.Contains(automatic, "encoding: base64") || !strings.Contains(automatic, "encoding_reason: invalid_utf8") || !strings.Contains(automatic, "//4AAQ==") {
		t.Fatalf("automatic binary output = %s", automatic)
	}
	explicit := invokeRead(t, catalog, workspace, "binary-2", `{"path":"binary.dat","contentEncoding":"utf8"}`)
	if !strings.Contains(explicit, `"status":"unsupported_encoding"`) || !strings.Contains(explicit, "error_code: binary_file") {
		t.Fatalf("explicit UTF-8 output = %s", explicit)
	}
}

func TestNativeReadEnforcesWorkspacePathAndFileSizePolicies(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(file, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(workspace, "large.txt")
	if err := os.WriteFile(local, []byte("larger than limit"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newReadCatalog(t, sessionfs.NativeReadSettings{
		Policy:       sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		MaxBytes:     4,
		MaxFileBytes: 4,
		DefaultLimit: 250,
	})

	denied := invokeRead(t, catalog, workspace, "denied", `{"path":`+mustJSONString(t, file)+`}`)
	if !strings.Contains(denied, `"status":"forbidden"`) || !strings.Contains(denied, "path_outside_workspace") || strings.Contains(denied, file) {
		t.Fatalf("outside-workspace output = %s", denied)
	}
	blocked := invokeRead(t, catalog, workspace, "blocked", `{"path":"large.txt"}`)
	if !strings.Contains(blocked, `"status":"blocked"`) || !strings.Contains(blocked, "size_limit") {
		t.Fatalf("large file output = %s", blocked)
	}
	metadata := invokeRead(t, catalog, workspace, "metadata", `{"path":"large.txt","includeContent":false}`)
	if !strings.Contains(metadata, "metadata_only: true") {
		t.Fatalf("metadata bypassed file-size limit incorrectly: %s", metadata)
	}
}

func TestNativeReadRejectsSymlinkEscapingWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "linked.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	catalog := newReadCatalog(t, sessionfs.NativeReadSettings{
		Policy:       sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		MaxBytes:     1024,
		MaxFileBytes: 1024,
		DefaultLimit: 250,
	})
	output := invokeRead(t, catalog, workspace, "symlink", `{"path":"linked.txt"}`)
	if !strings.Contains(output, "symlink_outside_workspace") || strings.Contains(output, "secret") {
		t.Fatalf("symlink output = %s", output)
	}
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
