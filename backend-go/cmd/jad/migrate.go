package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"jad/backend-go/internal/contentlibrary"
	"jad/backend-go/internal/sessionfs"
)

const migrateUsage = "usage: jad migrate workspace [--root NAME] [--json]; it plans the move of the v1 setting of this folder into the workspace layout and writes nothing"

// runMigrateCommand runs "jad migrate workspace": the dry run of the migration into the
// workspace layout (docs/design_workspace_layout.md §5). It plans for the exact current
// folder, prints the plan and writes nothing; a blocked plan exits 1. --apply is not built
// yet (phase 4b).
func runMigrateCommand(args []string, streams commandStreams) error {
	if len(args) == 0 || args[0] != "workspace" {
		return &exitError{code: exitFailed, err: errors.New(migrateUsage)}
	}
	flags := flag.NewFlagSet("jad migrate workspace", flag.ContinueOnError)
	flags.SetOutput(streams.stderr)
	root := flags.String("root", "", "the v1 session root to migrate, when the setting has several")
	asJSON := flags.Bool("json", false, "print the plan as JSON")
	apply := flags.Bool("apply", false, "apply the plan (not built yet)")
	if err := flags.Parse(args[1:]); err != nil {
		return &exitError{code: exitFailed}
	}
	if flags.NArg() != 0 {
		return &exitError{code: exitFailed, err: errors.New(migrateUsage)}
	}
	if *apply {
		return &exitError{code: exitFailed, err: errors.New("jad migrate workspace --apply is not built yet; run it without --apply for the plan (nothing was written)")}
	}
	defaults, err := contentlibrary.DefaultDirectories()
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	plan, err := sessionfs.PlanWorkspaceMigration(sessionfs.MigrateOptions{Root: *root, LibraryDefaults: defaults})
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	if *asJSON {
		encoder := json.NewEncoder(streams.stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(plan); err != nil {
			return err
		}
	} else {
		printMigrationPlan(streams.stdout, plan)
	}
	if plan.Blocked() {
		return &exitError{code: exitFailed, err: fmt.Errorf("the migration is blocked by %d problems; nothing was written", len(plan.Blockers))}
	}
	return nil
}

// printMigrationPlan prints the plan for a person: blockers first, then what would be
// created, copied or left in place, the additions to G in their own section, path rewrites,
// fields that look like secrets (names only), what is left out and the warnings.
func printMigrationPlan(out io.Writer, plan sessionfs.MigrationPlan) {
	line := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	line("jad migrate workspace: dry run, nothing was written")
	line("workspace: %s", plan.Workspace)
	line("global folder: %s", plan.Global)
	if plan.Setting != "" {
		line("v1 setting: %s", plan.Setting)
	}
	for _, root := range plan.Roots {
		mark := ""
		if root.Selected {
			mark = " (migrated)"
		}
		line("session root %s%s: %s, %d sessions", root.ID, mark, root.Directory, len(root.Sessions))
	}
	if plan.Blocked() {
		line("")
		line("blocked (%d):", len(plan.Blockers))
		for _, blocker := range plan.Blockers {
			if blocker.Path != "" {
				line("  [%s] %s: %s", blocker.Kind, blocker.Path, blocker.Message)
			} else {
				line("  [%s] %s", blocker.Kind, blocker.Message)
			}
		}
	}
	if len(plan.Settings) > 0 {
		line("")
		line("settings files:")
		for _, setting := range plan.Settings {
			line("  %s %s", setting.Action, setting.Path)
		}
	}
	printMigrationFiles(out, "workspace files", plan, false)
	printMigrationFiles(out, "additions to the global folder (every workspace sees them)", plan, true)
	if len(plan.Rewrites) > 0 {
		line("")
		line("path rewrites (each keeps the place it named in v1):")
		for _, item := range plan.Rewrites {
			line("  %s: %s: %s -> %s", item.File, item.Key, item.From, item.To)
		}
	}
	if len(plan.Secrets) > 0 {
		line("")
		line("fields that look like secrets (names only; values are never shown or changed):")
		for _, secret := range plan.Secrets {
			line("  %s: %s", secret.File, secret.Key)
		}
	}
	if len(plan.NotMigrated) > 0 {
		line("")
		line("not migrated:")
		for _, note := range plan.NotMigrated {
			line("  %s: %s", note.Path, note.Reason)
		}
	}
	if len(plan.Warnings) > 0 {
		line("")
		line("warnings:")
		for _, warning := range plan.Warnings {
			line("  %s", warning)
		}
	}
	line("")
	line("the v1 setting and files are left as they are; deleting %s goes back to v1", filepath.Join(plan.Workspace, ".jad", "workspace.json"))
	line("consider git-ignoring %s and %s", filepath.Join(".jad", "state"), filepath.Join(".jad", "profile-own"))
}

// printMigrationFiles prints the files of one side (W or G), grouped by action and group.
func printMigrationFiles(out io.Writer, title string, plan sessionfs.MigrationPlan, global bool) {
	type key struct{ action, group string }
	counts := map[key][2]int64{}
	var order []key
	var copies []sessionfs.MigrationFile
	for _, file := range plan.Files {
		if file.Global != global {
			continue
		}
		k := key{file.Action, file.Group}
		if _, seen := counts[k]; !seen {
			order = append(order, k)
		}
		total := counts[k]
		counts[k] = [2]int64{total[0] + 1, total[1] + file.Size}
		if file.Action == sessionfs.MigrationCopy && file.Group != "state" {
			copies = append(copies, file)
		}
	}
	if len(order) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\n%s:\n", title)
	for _, k := range order {
		_, _ = fmt.Fprintf(out, "  %s %s: %d files, %d bytes\n", k.action, k.group, counts[k][0], counts[k][1])
	}
	for _, file := range copies {
		mark := ""
		if file.Rewritten {
			mark = " (paths rewritten)"
		}
		_, _ = fmt.Fprintf(out, "    %s -> %s%s\n", file.Source, file.Destination, mark)
	}
}
