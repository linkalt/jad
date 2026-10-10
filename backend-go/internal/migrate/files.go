package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"example.com/agent-harness/backend-go/internal/catalog"
)

// pair is one key of an object whose key order matters (a toolset's mcp servers are
// ordered by their keys).
type pair struct {
	Key   string
	Value any
}

func marshalOrdered(pairs []pair) ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for index, item := range pairs {
		if index > 0 {
			out.WriteByte(',')
		}
		key, err := json.Marshal(item.Key)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(item.Value)
		if err != nil {
			return nil, err
		}
		out.Write(key)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// writeJSON writes value as indented JSON, creating the directory. It refuses to
// overwrite a file, so two sources can never land on one name unnoticed.
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeNew(path, append(data, '\n'))
}

func writeNew(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// copyPath copies a file or a directory tree. A missing source is not an error (the
// profile never wrote it). Lock and temporary files are skipped. The destination must
// not exist.
func copyPath(source, destination string) (int, error) {
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err := os.Lstat(destination); err == nil {
		return 0, fmt.Errorf("%s already exists", destination)
	}
	if !info.IsDir() {
		return copyFile(source, destination, info.Mode())
	}
	copied := 0
	err = filepath.Walk(source, func(path string, entry os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, _ := filepath.Rel(source, path)
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if skipFile(entry.Name()) || !entry.Mode().IsRegular() {
			return nil
		}
		count, err := copyFile(path, target, entry.Mode())
		copied += count
		return err
	})
	return copied, err
}

func skipFile(name string) bool {
	return strings.HasSuffix(name, ".jad-lock") || strings.Contains(name, ".tmp-")
}

func copyFile(source, destination string, mode os.FileMode) (int, error) {
	if skipFile(filepath.Base(source)) {
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return 0, err
	}
	in, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return 0, err
	}
	return 1, out.Close()
}

// identifier turns a legacy name into a catalog identifier: letters, digits, '-', '_'
// and '.', starting with a letter or digit.
func identifier(text string) string {
	var out strings.Builder
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out.WriteRune(r)
		default:
			out.WriteRune('-')
		}
	}
	result := strings.TrimLeft(out.String(), "-_.")
	if result == "" || catalog.ValidateIdentifier(result) != nil {
		return "x"
	}
	return result
}

func expandHome(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(value, "~"))
		}
	}
	return value
}

func resolveFrom(base, value string) string {
	value = expandHome(value)
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(base, value))
}
