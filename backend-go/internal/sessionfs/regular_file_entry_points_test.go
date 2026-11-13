package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func withinTimeout(t *testing.T, name string, call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: it is blocked on a non-regular file", name)
		return nil
	}
}

func replaceWith(t *testing.T, path, kind string, size int64) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "fifo":
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	case "directory":
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	default:
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(size); err != nil {
			t.Fatal(err)
		}
		file.Close()
	}
}

func TestEveryConfigReaderRefusesNonRegularAndOversizeFiles(t *testing.T) {
	type entry struct {
		name string
		cap  int64
		// path returns the file to replace; call exercises the reader that opens it.
		path func(layout Layout, location Location, root string) string
		call func(layout Layout, location Location, root string) error
	}
	settings := func(layout Layout, location Location, root string) error {
		_, err := LoadSessionSettings(location, "", "")
		return err
	}
	// A profile, toolset or model file that fails is left out of a load that only lists it, so
	// these name the profile they exercise: a profile the caller names must pass itself.
	named := func(layout Layout, location Location, root string) error {
		_, err := LoadSessionSettings(location, "dev/coder", "")
		return err
	}
	entries := []entry{
		{"session file", maxInternalJSONFileBytes, func(_ Layout, l Location, _ string) string { return l.File }, settings},
		{"profile file", maxInternalJSONFileBytes, func(l Layout, _ Location, _ string) string { return filepath.Join(l.Profiles, "dev", "coder.json") }, named},
		{"toolset file", maxInternalJSONFileBytes, func(l Layout, _ Location, _ string) string { return filepath.Join(l.Tools, "coding", "full.json") }, named},
		{"model file", maxInternalJSONFileBytes, func(l Layout, _ Location, _ string) string { return filepath.Join(l.Models, "openai", "gpt-deep.json") },
			func(_ Layout, l Location, _ string) error {
				// The session loads without a model it cannot read; a run reports the file.
				config, err := LoadSessionSettings(l, "dev/coder", "")
				if err != nil {
					return err
				}
				_, err = config.WithModel()
				return err
			}},
		{"prompt document", maxPromptDocumentBytes, func(_ Layout, _ Location, _ string) string { return "" }, settings},
		{"global setting", maxInternalJSONFileBytes, func(_ Layout, _ Location, root string) string {
			// A workspace beside the sample root, with the sample root as its global folder.
			workspace := filepath.Join(root+"-w", ".jad")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				panic(err)
			}
			for path, text := range map[string]string{filepath.Join(workspace, "workspace.json"): `{}`, filepath.Join(root, "global.json"): `{}`} {
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					panic(err)
				}
			}
			return filepath.Join(root, "global.json")
		},
			func(_ Layout, _ Location, root string) error {
				_, err := Load(LoadOptions{WorkingDir: root + "-w", GlobalDir: root, Environment: map[string]string{}})
				return err
			}},
		{"ReadCatalogFile", maxInternalJSONFileBytes, func(l Layout, _ Location, _ string) string { return filepath.Join(l.Models, "openai", "gpt-fast.json") },
			func(l Layout, _ Location, _ string) error {
				_, err := ReadCatalogFile(l, CatalogModels, "openai/gpt-fast")
				return err
			}},
		{"ListCatalogFiles", maxInternalJSONFileBytes, func(l Layout, _ Location, _ string) string { return filepath.Join(l.Models, "openai", "gpt-fast.json") },
			func(l Layout, _ Location, _ string) error {
				files, err := ListCatalogFiles(l, CatalogModels)
				for _, file := range files {
					if file.Error != "" && err == nil {
						err = errors.New(file.Error) // the row of the unreadable file; the others are listed
					}
				}
				return err
			}},
		{"CheckLayout", maxInternalJSONFileBytes, func(l Layout, _ Location, _ string) string { return filepath.Join(l.Models, "openai", "gpt-fast.json") },
			func(l Layout, loc Location, _ string) error {
				report, err := CheckLayout(layoutRoots(l, loc), func(string, []byte) error { return nil })
				if err == nil && len(report.Problems) > 0 {
					err = errors.New(report.Problems[0].Message)
				}
				return err
			}},
		{"ReadSessionFile", maxInternalJSONFileBytes, func(_ Layout, l Location, _ string) string { return l.File },
			func(l Layout, loc Location, _ string) error {
				_, err := ReadSessionFile(layoutRoots(l, loc), "default")
				return err
			}},
		{"ListSessionFiles", maxInternalJSONFileBytes, func(_ Layout, l Location, _ string) string { return l.File },
			func(l Layout, loc Location, _ string) error {
				listing, err := ListSessionFilesReport(layoutRoots(l, loc))
				if err == nil && len(listing.Skipped) > 0 {
					err = errors.New(listing.Skipped[0].Error)
				}
				return err
			}},
	}
	// The catalog scan and the session lookup drop these before any reader opens them, so the
	// reader is not reachable with that file type (the pass-through is covered by the oversize case).
	prefiltered := map[string]bool{
		"session file/fifo": true, "session file/directory": true, "ReadSessionFile/fifo": true,
		"ReadSessionFile/directory": true, "ListSessionFiles/fifo": true, "ListSessionFiles/directory": true,
		"profile file/directory": true, "toolset file/directory": true, "model file/directory": true,
		"ListCatalogFiles/directory": true, "CheckLayout/directory": true,
	}
	for _, e := range entries {
		for _, kind := range []string{"fifo", "directory", "oversize"} {
			if prefiltered[e.name+"/"+kind] {
				continue
			}
			t.Run(e.name+"/"+kind, func(t *testing.T) {
				layout, location, root := sampleLayout(t)
				location.Root.Layout = &layout
				path := e.path(layout, location, root)
				if e.name == "prompt document" {
					path = filepath.Join(sessionCWDForTest(t, location), "CLAUDE.md")
				}
				replaceWith(t, path, kind, e.cap+1)
				err := withinTimeout(t, e.name, func() error { return e.call(layout, location, root) })
				if err == nil {
					t.Fatalf("%s as %s: want an error, got nil", e.name, kind)
				}
				if kind == "oversize" && !strings.Contains(err.Error(), "larger than") {
					t.Errorf("%s oversize: error does not name the limit: %v", e.name, err)
				}
				if kind != "oversize" && !strings.Contains(err.Error(), "not a regular file") && !strings.Contains(err.Error(), "directory") && !strings.Contains(err.Error(), "readable file") {
					t.Errorf("%s as %s: unexpected error %v", e.name, kind, err)
				}
			})
		}
	}
}

func layoutRoots(layout Layout, location Location) []Root {
	root := location.Root
	root.Layout = &layout
	return []Root{root}
}

func sessionCWDForTest(t *testing.T, location Location) string {
	t.Helper()
	session, err := readLayoutSession(location)
	if err != nil {
		t.Fatal(err)
	}
	return session.CWD
}

func TestPromptDocumentAtTheCapLoads(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	location.Root.Layout = &layout
	replaceWith(t, filepath.Join(sessionCWDForTest(t, location), "CLAUDE.md"), "file", maxPromptDocumentBytes)
	if err := withinTimeout(t, "load", func() error { _, err := LoadSessionSettings(location, "", ""); return err }); err != nil {
		t.Fatalf("a prompt document exactly at the cap: %v", err)
	}
}

func TestReadRegularFileCapIsInclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	replaceWith(t, path, "file", 100)
	if data, err := readRegularFile(path, 100); err != nil || len(data) != 100 {
		t.Fatalf("at the cap: %d bytes, %v", len(data), err)
	}
	if _, err := readRegularFile(path, 99); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("over the cap: %v", err)
	}
}
