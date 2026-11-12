package sessionfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

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
	Sandboxes []CheckedSandbox
	// Shares are the isolation settings that name share_paths, which only the overlay
	// materialiser can give: the caller tries it on this host for each.
	Shares      []CheckedShares
	seen        map[string]bool
	seenSandbox map[string]bool
	seenFolders map[string]bool
}

// CheckedSandbox is an enabled sandbox setting and a session that uses it.
type CheckedSandbox struct {
	SessionID  string
	WorkingDir string
	Settings   *SandboxSettings
}

// CheckedShares is a session, profile, agent or sandbox_choice template whose isolation has
// share_paths, with the sandbox its runs start in (nil: the default bwrap holds the overlay).
type CheckedShares struct {
	Where   string
	Paths   []string
	Sandbox *SandboxSettings
}

// addShares records the share_paths of config: those of the isolation of the profile's own
// delegated runs, of each sub-agent with isolation worktree, and of each sandbox_choice
// template. materialize checkout with share_paths never loads, so every entry here asks for
// overlay (explicitly or through auto).
func (r *CheckReport) addShares(id string, config SessionConfig) {
	add := func(what string, paths []string, sandbox *SandboxSettings) {
		if len(paths) > 0 {
			r.Shares = append(r.Shares, CheckedShares{Where: id + " " + what, Paths: paths, Sandbox: sandbox})
		}
	}
	add("sandbox.isolation", config.Isolation.SharePaths, config.Sandbox)
	if settings := config.NativeSubagent; settings != nil {
		for _, agent := range settings.Agents {
			if !agent.Isolated() {
				continue
			}
			sandbox := agent.Sandbox
			if sandbox == nil {
				sandbox = config.Sandbox
			}
			add("agent "+agent.Name+" sandbox.isolation", agent.IsolationShare, sandbox)
		}
		for _, choice := range settings.SandboxChoices {
			add("subagent sandbox_choice "+choice.Name+" isolation", choice.Isolation.SharePaths, choiceSandbox(choice, config.Sandbox))
		}
	}
	if settings := config.NativeProfileMessage; settings != nil {
		for _, choice := range settings.SandboxChoices {
			add("profile_message sandbox_choice "+choice.Name+" isolation", choice.Isolation.SharePaths, choiceSandbox(choice, config.Sandbox))
		}
	}
}

func choiceSandbox(choice SandboxChoice, session *SandboxSettings) *SandboxSettings {
	if choice.Sandbox != nil {
		return choice.Sandbox
	}
	return session
}

func (r *CheckReport) addSandbox(id string, config SessionConfig) {
	if config.Sandbox == nil {
		return
	}
	// The session's own read-only paths differ per session and change no trial start, so
	// sandboxes that differ only there are started once.
	probed := *config.Sandbox
	probed.ReadOnlyPaths, probed.SealedPaths, probed.SealedInner = nil, nil, nil
	probed.ProfileHidden, probed.ProfileShown, probed.ProfileData = nil, nil, ""
	key, _ := json.Marshal(probed)
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
	report.checkOwnModels(*layout)
	report.checkSandboxFolder(layout.Sandbox)
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
	// A name the loader leaves out is reported on its own, as the loader's warning words it, and
	// the files around it are still checked.
	for _, skipped := range entries.Skipped() {
		r.Checked++
		r.add(string(kind), skipped.Path, fmt.Errorf("%s catalog: file %s left out: %s", kind, skipped.Path, skipped.Reason))
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

// checkSandboxFolder reports each file of a sandbox template folder that is left out for its name,
// once per folder. A folder that is not given (catalogs.sandbox is {cwd}/<folder>) is checked per
// session, with the session's cwd.
func (r *CheckReport) checkSandboxFolder(dir string) {
	if dir == "" || r.seenFolders[dir] {
		return
	}
	if r.seenFolders == nil {
		r.seenFolders = map[string]bool{}
	}
	r.seenFolders[dir] = true
	_, skipped := scanSandboxTemplates(dir)
	for _, item := range skipped {
		r.Checked++
		r.add("sandbox", item.Path, fmt.Errorf("sandbox catalog: file %s left out: %s", item.Path, item.Reason))
	}
}

// checkMisnamedSessions reports each file of a session root that holds a session under a name no
// session route accepts. Files that are not sessions are not reported.
func (r *CheckReport) checkMisnamedSessions(roots []Root) {
	var finder MisnamedSessions
	for _, root := range roots {
		if root.Layout == nil {
			continue
		}
		entries, err := os.ReadDir(root.Directory)
		if err != nil {
			continue // listSessionFiles reports an unreadable root
		}
		for _, warning := range finder.Find(root, entries) {
			r.Checked++
			r.add("session", root.ID, errors.New(warning))
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

// addLeftOutByComposition reports each profile of the session's scope that a load leaves out
// because its toolsets do not apply to this session's settings. The load only warns, and the
// profile never shows in the session's profile list, so nothing else would name it.
func (r *CheckReport) addLeftOutByComposition(id string, location Location) {
	session, err := readLayoutSession(location)
	if err != nil {
		return
	}
	judged, err := judgeScope(*location.Root.Layout, location, session, location.SettingFile(), nil)
	if err != nil {
		return
	}
	for _, profile := range judged.leftOut() {
		if verdict := judged.verdicts[profile]; verdict.composition {
			r.add("session", fmt.Sprintf("%s (profile %s)", id, profile), verdict.err)
		}
	}
}

// checkOwnModels reports, in the workspace layout, each own model file of each profile that the
// profile's load leaves out (decider 3c.1 item 5, 3c.2): a name or folder the scan skips, a
// file that fails the own model rules, a base that is missing or fails, and a composed model
// that fails the model rules. A load only warns about these in the server log, and a session
// whose selected model is one of them fails only when it runs.
func (r *CheckReport) checkOwnModels(layout Layout) {
	if layout.Workspace == "" || layout.Profiles == "" {
		return
	}
	profiles, err := catalog.Scan(string(CatalogProfiles), layout.Profiles)
	if err != nil {
		return // checkCatalog reports the profiles folder
	}
	scans := NewCatalogScans()
	for _, profile := range profiles.Entries() {
		own, err := layout.profileModels(scans, profile.ID())
		if err != nil {
			r.add("own model", profile.ID(), err)
			continue
		}
		for _, note := range own.notes {
			r.add("own model", profile.ID(), errors.New(note))
		}
		for _, path := range slices.Sorted(maps.Keys(own.ids)) {
			r.Checked++
			if err := own.problem(path); err != nil {
				r.add("own model", profile.ID()+" profile:"+own.ids[path], err)
			}
		}
	}
}

func (r *CheckReport) checkSessions(roots []Root) {
	r.checkMisnamedSessions(roots)
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
		r.checkSandboxFolder(location.Root.Layout.SandboxTemplateDir(config.WorkingDir))
		r.checkProfileMessage(file.ID, config)
		r.addSandbox(file.ID, config)
		r.addShares(file.ID, config)
		r.addLeftOutByComposition(file.ID, location)
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
			r.addShares(fmt.Sprintf("%s (profile %s)", file.ID, profile), profileConfig)
		}
	}
}
