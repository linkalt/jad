// Command jad-migrate converts a legacy session tree (one setting.json per session) into the
// catalog layout. It reads the source and writes a new tree; it never changes the source.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"example.com/agent-harness/backend-go/internal/migrate"
)

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 64
)

const usage = `Usage: jad-migrate --setting <root setting.json> --out <new directory>

Converts every legacy session of the root setting into the catalog layout and checks that
each migrated profile loads to the same settings as before.

  --setting  the legacy root setting.json
  --out      a missing or empty directory that receives the new tree

Exit status: 0 migrated and identical, 1 failed or the migrated tree differs, 64 usage.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jad-migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	setting := flags.String("setting", "", "legacy root setting.json")
	out := flags.String("out", "", "directory for the new tree")
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if *setting == "" || *out == "" || flags.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	report, err := migrate.Run(migrate.Options{SourceSetting: *setting, Out: *out})
	print(stdout, report)
	if err != nil {
		fmt.Fprintf(stderr, "jad-migrate: %v\n", err)
		return exitFailed
	}
	if len(report.Differences) > 0 {
		return exitFailed
	}
	return exitOK
}

func print(w io.Writer, report migrate.Report) {
	for _, session := range report.Sessions {
		fmt.Fprintf(w, "session %s:%s\n", session.Root, session.Name)
		for _, profile := range session.Profiles {
			fmt.Fprintf(w, "  %-20s -> %-30s %d state files\n", profile.Legacy, profile.ID, profile.Files)
		}
	}
	for _, item := range report.Skipped {
		fmt.Fprintf(w, "skipped: %s\n", item)
	}
	for _, item := range report.Warnings {
		fmt.Fprintf(w, "warning: %s\n", item)
	}
	for _, item := range report.Differences {
		fmt.Fprintf(w, "DIFFERENT: %s\n", item)
	}
	if report.Out != "" && len(report.Differences) == 0 && len(report.Sessions) > 0 {
		fmt.Fprintf(w, "migrated %d sessions to %s; every profile loads to the same settings\n", len(report.Sessions), report.Out)
	}
}
