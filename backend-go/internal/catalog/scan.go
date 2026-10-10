package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const fileExtension = ".json"

// ValidateIdentifier checks a file name or category name. The set is small
// on purpose: these strings reach URLs, selectors and transcript paths, so
// no separator, colon, star or space may appear.
func ValidateIdentifier(text string) error {
	if text == "" {
		return errors.New("must not be empty")
	}
	for i, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
			if i == 0 {
				return fmt.Errorf("%q must start with a letter or digit", text)
			}
		default:
			return fmt.Errorf("%q may only hold letters, digits, '-', '_' and '.'", text)
		}
	}
	return nil
}

// Scan reads <dir>/<category>/<name>.json for one kind. A missing dir is an
// empty catalog. Entries that start with '.' and files that are not .json
// are ignored. A .json file directly in dir, a bad name, or the same name
// in two categories is a load error naming the paths.
func Scan(kind, dir string) (*Catalog, error) {
	categories, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return newCatalog(kind, nil), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s catalog %s: %w", kind, dir, err)
	}
	var entries []Entry
	seen := map[string]string{}
	for _, item := range categories {
		if strings.HasPrefix(item.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, item.Name())
		if !isDir(path) {
			if strings.HasSuffix(item.Name(), fileExtension) {
				return nil, fmt.Errorf("%s catalog: %s must live in a category folder, for example %s",
					kind, path, filepath.Join(dir, "<category>", item.Name()))
			}
			continue
		}
		if err := ValidateIdentifier(item.Name()); err != nil {
			return nil, fmt.Errorf("%s catalog: category folder %s: %w", kind, path, err)
		}
		found, err := scanCategory(kind, item.Name(), path)
		if err != nil {
			return nil, err
		}
		for _, entry := range found {
			if first, ok := seen[entry.Name]; ok {
				return nil, fmt.Errorf("%s catalog: name %q is used twice: %s and %s",
					kind, entry.Name, first, entry.Path)
			}
			seen[entry.Name] = entry.Path
			entries = append(entries, entry)
		}
	}
	return newCatalog(kind, entries), nil
}

func scanCategory(kind, category, dir string) ([]Entry, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%s catalog %s: %w", kind, dir, err)
	}
	var entries []Entry
	for _, file := range files {
		fileName := file.Name()
		if strings.HasPrefix(fileName, ".") || !strings.HasSuffix(fileName, fileExtension) {
			continue
		}
		path := filepath.Join(dir, fileName)
		if isDir(path) {
			continue
		}
		name := strings.TrimSuffix(fileName, fileExtension)
		if err := ValidateIdentifier(name); err != nil {
			return nil, fmt.Errorf("%s catalog: file %s: name %w", kind, path, err)
		}
		entries = append(entries, Entry{Name: name, Category: category, Path: path})
	}
	return entries, nil
}

// isDir follows symlinks, so a linked category folder works.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
