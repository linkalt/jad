package sessionfs

import (
	"encoding/json"
	"errors"
	"fmt"

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
	// Sandboxes are the distinct enabled sandboxes of the sessions, each with the first session
	// that uses it. Reading files cannot show that a sandbox starts on this machine, so the
	// caller tries each one.
	Sandboxes   []CheckedSandbox
	seen        map[string]bool
	seenSandbox map[string]bool
}

// CheckedSandbox is an enabled sandbox setting and a session that uses it.
type CheckedSandbox struct {
	SessionID  string
	WorkingDir string
	Settings   *SandboxSettings
}

func (r *CheckReport) addSandbox(id string, config SessionConfig) {
	if config.Sandbox == nil {
		return
	}
	key, _ := json.Marshal(config.Sandbox)
	if r.seenSandbox[string(key)] {
		return
	}
	if r.seenSandbox == nil {
		r.seenSandbox = map[string]bool{}
	}
	r.seenSandbox[string(key)] = true
	r.Sandboxes = append(r.Sandboxes, CheckedSandbox{SessionID: id, WorkingDir: config.WorkingDir, Settings: config.Sandbox})
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
		data, err := readRegularFile(entry.Path, maxInternalJSONFileBytes)
		if err == nil {
			err = validateCatalogFile(layout, roots, kind, entry.Path, data, validateServer)
		}
		if err != nil {
			r.add(string(kind), entry.ID(), err)
		}
	}
}

func (r *CheckReport) checkProfileMessage(id string, config SessionConfig) {
	if config.NativeProfileMessage == nil {
		return
	}
	if len(config.NativeProfileMessage.Sessions) == 0 && len(ProfileMessageTargets(config.Profiles, config.Profile, config.NativeProfileMessage.DenyProfiles)) == 0 {
		r.add("session", id, ProfileMessageNoTargetsError(config.SettingPath, config.Profile))
	}
}

func (r *CheckReport) checkSessions(roots []Root) {
	files, _, err := listSessionFiles(roots, 0, func(location Location, err error) {
		r.Checked++
		r.add("session", location.Reference, err)
	})
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
		r.checkProfileMessage(file.ID, config)
		r.addSandbox(file.ID, config)
		for _, profile := range config.Profiles {
			if profile == config.Profile {
				continue
			}
			profileConfig, err := LoadLayoutSessionSettings(location, *location.Root.Layout, profile, "")
			if err != nil {
				r.add("session", fmt.Sprintf("%s (profile %s)", file.ID, profile), err)
				continue
			}
			r.checkProfileMessage(fmt.Sprintf("%s (profile %s)", file.ID, profile), profileConfig)
			r.addSandbox(fmt.Sprintf("%s (profile %s)", file.ID, profile), profileConfig)
		}
	}
}
