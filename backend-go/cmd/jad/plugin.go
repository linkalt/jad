package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"jad/backend-go/internal/contentlibrary"
	"jad/backend-go/internal/plugins"
	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
)

const pluginUsage = `usage: jad plugin install [--force] <folder>
       jad plugin install [--force] [--ref <branch|tag|commit>] [--subdir <folder>] [--timeout <duration>] <git URL>
       jad plugin remove  [--force] <name>
       jad plugin list

A plugin is a folder with plugin.json ({"schema_version":1,"name":"<name>","version":"..."})
and any of models/, mcp/, tools/ (toolsets, with their hooks), profiles/ (agents),
skills/<skill>/SKILL.md, commands/ (custom slash commands, .md files) and files/. install
copies each catalog file to <catalog>/<name>/<file>.json, so the plugin's files name each
other as <name>/<file>; each skill folder to the first of libraries.skill_dirs; commands/ to
<first of libraries.command_dirs>/<name>, so its commands are /<name>:<command>; and files/ to
<state>/.plugins/<name>/files, whose path replaces ${JAD_PLUGIN_FILES} in the catalog files.
Every catalog file is checked as the loader reads it, and a failed install is undone.
Installing a plugin that is already installed upgrades it: files the new version drops are
removed.

A file that exists and is not the plugin's is never overwritten. A file the plugin
installed and that was changed since stops install and remove unless --force is given.
remove refuses, and changes nothing, while a session or another file still needs a profile,
toolset, MCP server or model of the plugin.

A git URL (https://, http://, ssh://, git://, file:// or user@host:path) is cloned into a
temporary folder with git, which never asks for a password here: use an SSH key or a
credential helper for a private repository. --ref picks a branch, tag or commit (the default
branch otherwise), --subdir the plugin's folder in the repository, and --timeout bounds the
clone (default: cli.plugin_install_timeout_ms of the root setting, 5m unless set; 0 for no
limit). The plugin list shows the URL without credentials, the ref, the folder
and the commit installed.
`

func runPluginCommand(args []string, streams commandStreams) error {
	if len(args) == 0 {
		return usageError("%s", pluginUsage)
	}
	action := args[0]
	switch action {
	case "install", "remove", "list":
	case "-h", "--help", "help":
		_, _ = io.WriteString(streams.stdout, pluginUsage)
		return nil
	default:
		return usageError("%s", pluginUsage)
	}
	flags := flag.NewFlagSet("jad plugin "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	force := false
	var git plugins.GitSource
	if action != "list" {
		flags.BoolVar(&force, "force", false, "overwrite or remove files changed since they were installed")
	}
	if action == "install" {
		flags.StringVar(&git.Ref, "ref", "", "branch, tag or commit of a git URL")
		flags.StringVar(&git.Subdir, "subdir", "", "the plugin's folder in a git repository")
		flags.DurationVar(&git.Timeout, "timeout", 0, "how long the git clone may take (default: cli.plugin_install_timeout_ms)")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(streams.stdout, pluginUsage)
			return nil
		}
		return usageError("%v\n\n%s", err, pluginUsage)
	}
	want := 1
	if action == "list" {
		want = 0
	}
	if flags.NArg() != want {
		return usageError("%s", pluginUsage)
	}
	fromGit := false
	if action == "install" {
		// An existing folder is a folder, whatever its name looks like.
		if _, err := os.Stat(flags.Arg(0)); err != nil && plugins.IsGitURL(flags.Arg(0)) {
			fromGit, git.URL = true, flags.Arg(0)
		}
		if !fromGit && (git.Ref != "" || git.Subdir != "") {
			return usageError("--ref and --subdir go with a git URL\n\n%s", pluginUsage)
		}
		if git.Timeout < 0 {
			return usageError("--timeout cannot be negative\n\n%s", pluginUsage)
		}
	}
	target, cli, err := pluginTarget()
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	if !flagGiven(flags, "timeout") {
		git.Timeout = cli.PluginInstallTimeout
	}
	ctx, stop := interruptContext()
	defer stop()
	switch action {
	case "install":
		var result plugins.InstallResult
		if fromGit {
			result, err = target.InstallGit(ctx, git, force)
		} else {
			result, err = target.Install(ctx, flags.Arg(0), force)
		}
		if err != nil {
			return &exitError{code: exitFailed, err: err}
		}
		verb := "Installed"
		if result.Upgraded != nil {
			verb = "Upgraded"
		}
		_, _ = fmt.Fprintf(streams.stdout, "%s %s %s: %d parts\n", verb, result.Record.Name, versionText(result.Record.Version), len(result.Record.Entries))
		for _, entry := range result.Record.Entries {
			_, _ = fmt.Fprintf(streams.stdout, "  %-8s %s\n", entry.Kind, entry.ID)
		}
		for _, entry := range result.Removed {
			_, _ = fmt.Fprintf(streams.stdout, "  removed  %s %s\n", entry.Kind, entry.ID)
		}
	case "remove":
		record, err := target.Remove(ctx, flags.Arg(0), force)
		if err != nil {
			return &exitError{code: exitFailed, err: err}
		}
		_, _ = fmt.Fprintf(streams.stdout, "Removed %s %s: %d parts\n", record.Name, versionText(record.Version), len(record.Entries))
	case "list":
		statuses, problems, err := target.ListReport()
		if err != nil {
			return &exitError{code: exitFailed, err: err}
		}
		// A record that cannot be read is reported and left out; the other plugins are listed.
		for _, problem := range problems {
			_, _ = fmt.Fprintf(streams.stderr, "warning: plugin left out of the list: %v\n", problem)
		}
		if len(statuses) == 0 && len(problems) == 0 {
			_, _ = fmt.Fprintln(streams.stdout, "No plugins are installed.")
		}
		for _, status := range statuses {
			changed := ""
			if len(status.Modified) > 0 {
				changed = fmt.Sprintf(", %d changed since install", len(status.Modified))
			}
			_, _ = fmt.Fprintf(streams.stdout, "%s %s: %d parts%s (from %s)\n", status.Record.Name, versionText(status.Record.Version), len(status.Record.Entries), changed, status.Record.Source)
		}
	}
	return nil
}

func versionText(version string) string {
	if version == "" {
		return "(no version)"
	}
	return version
}

// pluginTarget is the catalog layout of the roots and the first skill and command folders
// of its root setting, with the setting's cli section. The catalogs are global to the root setting, so every root shares them.
func pluginTarget() (plugins.Target, sessionfs.CLISettings, error) {
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{})
	if err != nil {
		return plugins.Target{}, sessionfs.CLISettings{}, fmt.Errorf("load server configuration: %w", err)
	}
	var root *sessionfs.Root
	for index := range loaded.Roots {
		if loaded.Roots[index].Layout != nil {
			root = &loaded.Roots[index]
			break
		}
	}
	if root == nil {
		return plugins.Target{}, sessionfs.CLISettings{}, errors.New("no session root uses the catalog layout")
	}
	target := plugins.Target{
		Layout:         *root.Layout,
		Roots:          loaded.Roots,
		ValidateServer: mcpclient.ValidateServerFile,
		ReadSkill: func(ctx context.Context, directory, name string, maxBytes int64) error {
			_, ok, err := contentlibrary.ReadSkill(ctx, []string{directory}, name, maxBytes)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("SKILL.md needs front matter with a name equal to its folder name (lowercase letters, digits and hyphens) and a description")
			}
			return nil
		},
	}
	directories, err := contentlibrary.SkillDirectories(root.GlobalJadRoot)
	if err != nil {
		return plugins.Target{}, sessionfs.CLISettings{}, err
	}
	if len(directories) > 0 {
		target.SkillDir = directories[0]
	}
	commandDirectories, err := contentlibrary.CommandDirectories(root.GlobalJadRoot)
	if err != nil {
		return plugins.Target{}, sessionfs.CLISettings{}, err
	}
	if len(commandDirectories) > 0 {
		target.CommandDir = commandDirectories[0]
	}
	target.ReadCommands = contentlibrary.CheckCommands
	if target.SkillMaxBytes, err = contentlibrary.AssetByteLimit(root.GlobalJadRoot); err != nil {
		return plugins.Target{}, sessionfs.CLISettings{}, err
	}
	return target, loaded.CLI, nil
}

// flagGiven reports whether the command line set the flag, so an unset one can take its
// default from the root setting instead of from the flag declaration.
func flagGiven(flags *flag.FlagSet, name string) bool {
	given := false
	flags.Visit(func(set *flag.Flag) { given = given || set.Name == name })
	return given
}
