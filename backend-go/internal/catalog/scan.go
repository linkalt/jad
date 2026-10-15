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
// are ignored. A .json file directly in dir or a bad name is a load error
// naming the path. The same name may appear in several categories; that is
// how one program gets different settings (see Catalog.Unique).
func Scan(kind, dir string) (*Catalog, error) {
	return ScanObserved(kind, dir, nil)
}

// ScanObserved is Scan that calls observe with each folder just before it lists it, so a
// caller can record the folder's state from before the listing.
func ScanObserved(kind, dir string, observe func(folder string)) (*Catalog, error) {
	if observe != nil {
		observe(dir)
	}
	categories, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return newCatalog(kind, nil), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s catalog %s: %w", kind, dir, err)
	}
	var entries []Entry
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
		if observe != nil {
			observe(path)
		}
		found, err := scanCategory(kind, item.Name(), path)
		if err != nil {
			return nil, err
		}
		entries = append(entries, found...)
	}
	return newCatalog(kind, entries), nil
}

func scanCategory(kind, category, dir string) ([]Entry, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // the folder was removed after the category list was read
	}
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
