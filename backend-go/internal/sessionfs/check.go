package sessionfs

import (
	"errors"
	"fmt"
	"os"

	"jad/backend-go/internal/catalog"
)

// CheckProblem is one file the loader would refuse, with the loader's own message.
type CheckProblem struct {
	Kind    string
	ID      string
	Message string
}

// CheckReport is the result of CheckLayout: how many files were read and what is wrong.
type CheckReport struct {
	Checked  int
	Problems []CheckProblem
	seen     map[string]bool
}

// CheckLayout reads every file of the catalog layout without changing anything and reports
// each one the loader would refuse: the catalog files with the rules a write applies, and
// every session once for each profile in its scope, which resolves the references from the
// session to its profile, models, toolset, MCP servers and sub-agents. It fails only when
// no root uses the layout.
func CheckLayout(roots []Root, validateServer ServerFileValidator) (CheckReport, error) {
	var layout *Layout
	for index := range roots {
		if roots[index].Layout != nil {
			layout = roots[index].Layout
			break
		}
	}
	if layout == nil {
		return CheckReport{}, errors.New("no session root uses the catalog layout")
	}
	var report CheckReport
	for _, kind := range []CatalogKind{CatalogModels, CatalogMCP, CatalogToolsets, CatalogProfiles} {
		report.checkCatalog(*layout, roots, kind, validateServer)
	}
	report.checkSessions(roots)
	return report, nil
}

// add records a problem. A broken catalog file makes every session that uses it fail with
// the loader's identical message, so a session problem whose message a catalog file already
// reported is left out; two sessions are never merged, since a message without a path can
// come from two different files.
func (r *CheckReport) add(kind, id string, err error) {
	message := err.Error()
	if kind == "session" {
		if r.seen[message] {
			return
		}
	} else {
		if r.seen == nil {
			r.seen = map[string]bool{}
		}
		r.seen[message] = true
	}
	r.Problems = append(r.Problems, CheckProblem{Kind: kind, ID: id, Message: message})
}

func (r *CheckReport) checkCatalog(layout Layout, roots []Root, kind CatalogKind, validateServer ServerFileValidator) {
	directory, err := layout.catalogDir(kind)
	if err != nil {
		r.add(string(kind), "", err)
		return
	}
	entries, err := catalog.Scan(string(kind), directory)
	if err != nil {
		r.add(string(kind), "", err)
		return
	}
	for _, entry := range entries.Entries() {
		r.Checked++
		data, err := os.ReadFile(entry.Path)
		if err == nil {
			err = validateCatalogFile(layout, roots, kind, entry.Path, data, validateServer)
		}
		if err != nil {
			r.add(string(kind), entry.ID(), err)
		}
	}
}

func (r *CheckReport) checkSessions(roots []Root) {
	files, err := ListSessionFiles(roots)
	if err != nil {
		r.add("session", "", err)
		return
	}
	for _, file := range files {
		r.Checked++
		location, err := Resolve(roots, file.ID)
		if err != nil {
			r.add("session", file.ID, err)
			continue
		}
		config, err := LoadLayoutSessionSettings(location, *location.Root.Layout, "", "")
		if err != nil {
			r.add("session", file.ID, err)
			continue
		}
		for _, profile := range config.Profiles {
			if profile == config.Profile {
				continue
			}
			if _, err := LoadLayoutSessionSettings(location, *location.Root.Layout, profile, ""); err != nil {
				r.add("session", fmt.Sprintf("%s (profile %s)", file.ID, profile), err)
			}
		}
	}
}
