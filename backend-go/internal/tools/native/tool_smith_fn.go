package native

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/sessionfs"
)

const toolSmithFunctionsEnv = "TOOL_SMITH_FUNCTIONS"

type toolSmithFunctionsManifest struct {
	Available    []string          `json:"available"`
	Callable     []string          `json:"callable"`
	More         int               `json:"more"`
	Malformed    map[string]string `json:"malformed"`
	LibraryError string            `json:"library_error"`
	ScanCut      int               `json:"scan_cut"`
}

// stageToolSmithFunctions copies the valid saved functions to workDir/fn and writes workDir/fn-manifest.json.
// An unusable library is reported in the manifest, not as an error.
func stageToolSmithFunctions(workDir string, settings sessionfs.NativeToolSmithSettings, workspaceRoot string) error {
	manifest := toolSmithFunctionsManifest{Available: []string{}, Callable: []string{}, Malformed: map[string]string{}}
	dir, realRoot, err := resolveToolSmithFunctionsDir(settings.FunctionsDir, workspaceRoot)
	if err != nil {
		manifest.LibraryError = err.Error()
		return writeToolSmithManifest(workDir, manifest)
	}
	entries, err := os.ReadDir(viewPath(dir))
	if err != nil {
		manifest.LibraryError = err.Error()
		return writeToolSmithManifest(workDir, manifest)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".ts") {
			files = append(files, entry.Name())
		}
	}
	scanned := files
	if int64(len(scanned)) > settings.ListMaxFiles {
		scanned = scanned[:settings.ListMaxFiles]
	}
	manifest.ScanCut = len(files) - len(scanned)

	copied := []string{}
	for _, file := range scanned {
		name := strings.TrimSuffix(file, ".ts")
		if _, problem := readToolSmithListEntry(dir, realRoot, file, settings.ListHeaderBytes); problem != "" {
			manifest.Malformed[name] = problem
			continue
		}
		problem, err := copyToolSmithFunction(workDir, dir, realRoot, file, name, settings.FunctionMaxBytes)
		if err != nil {
			return err
		}
		if problem != "" {
			manifest.Malformed[name] = problem
			continue
		}
		copied = append(copied, name)
	}
	manifest.Callable = copied
	manifest.Available = copied
	if int64(len(copied)) > settings.ListMaxResults {
		manifest.Available = copied[:settings.ListMaxResults]
		manifest.More = len(copied) - len(manifest.Available)
	}
	return writeToolSmithManifest(workDir, manifest)
}

// copyToolSmithFunction copies one validated library file; problem is non-empty when the file cannot be used.
// err is set only when the fn directory cannot be created.
func copyToolSmithFunction(workDir, dir, realRoot, file, name string, maxBytes int64) (problem string, err error) {
	tooBig := func(size string) string {
		return fmt.Sprintf("is %s bytes, over function_max_bytes %d", size, maxBytes)
	}
	handle, info, openProblem := openLibraryFile(dir, realRoot, file)
	if openProblem != "" {
		return openProblem, nil
	}
	defer handle.Close()
	if info.Size() > maxBytes {
		return tooBig(fmt.Sprint(info.Size())), nil
	}
	fnDir := filepath.Join(workDir, "fn")
	if err := os.MkdirAll(fnDir, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(fnDir, name+".ts")
	out, openOutErr := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if openOutErr != nil {
		return "cannot be copied: " + openOutErr.Error(), nil
	}
	written, copyErr := io.Copy(out, io.LimitReader(handle, maxBytes+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || written > maxBytes {
		os.Remove(target)
		if copyErr != nil {
			return "cannot be read: " + copyErr.Error(), nil
		}
		if closeErr != nil {
			return "cannot be copied: " + closeErr.Error(), nil
		}
		return tooBig("more than " + fmt.Sprint(maxBytes)), nil
	}
	return "", nil
}

func writeToolSmithManifest(workDir string, manifest toolSmithFunctionsManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workDir, "fn-manifest.json"), data, 0o600)
}
