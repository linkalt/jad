package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"jad/backend-go/internal/sessionlock"
	"jad/backend-go/internal/transcript"
)

const transcriptUsage = `usage: jad transcript strip-local [--dry-run] [--meta-only] <messages.jsonl or folder>...
       jad transcript strip-steer [--dry-run] <messages.jsonl or folder>...   (same as strip-local)

strip-local prepares transcripts for an older jad build. It blanks, in place, the fields jad
writes last in a record for itself: "__agent_session_input_steer" (the steering marker) and
"__agent_meta" (optional data, such as the time a user message was stored). The file keeps
its size and every record its offset. A folder is searched for files named messages.jsonl. A
transcript in use by a run is left alone and reported. Stop the server before you run it; a
run started afterwards writes the fields again.

A build from before "__agent_meta" (commit b9dc0c2a and earlier, the 6ac57497 migration build
included) sends it to the provider, which most providers refuse. Before going back to a build
that knows the steering marker, run it with --meta-only, which blanks "__agent_meta" only;
before going back to a build from before the steering marker, run it without.

It cannot be undone. The steering marker tells the current build that a steering or stop
hook message continues the run's turn; once stripped, each such message starts a turn of its
own for this build too, so compaction (keep_turns, the overflow compaction) and the counted
turns may cut those runs differently. Run it only right before the rollback.

Backups (messages.backup.jsonl) and compaction archives (messages-<time>.jsonl) keep the
fields: no build sends them to the provider.`

// runTranscriptCommand runs "jad transcript".
func runTranscriptCommand(args []string, streams commandStreams) error {
	if len(args) == 0 || (args[0] != "strip-local" && args[0] != "strip-steer") {
		return &exitError{code: exitFailed, err: errors.New(transcriptUsage)}
	}
	flags := flag.NewFlagSet("jad transcript "+args[0], flag.ContinueOnError)
	flags.SetOutput(streams.stderr)
	dryRun := flags.Bool("dry-run", false, "count the records without changing a file")
	metaOnly := false
	if args[0] == "strip-local" {
		flags.BoolVar(&metaOnly, "meta-only", false, "blank __agent_meta only and keep the steering marker")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return &exitError{code: exitFailed}
	}
	if flags.NArg() == 0 {
		return &exitError{code: exitFailed, err: errors.New(transcriptUsage)}
	}
	files, err := transcriptFiles(flags.Args())
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	verb := "stripped"
	if *dryRun {
		verb = "would strip"
	}
	var total transcript.LocalStripCounts
	changed, failed := 0, 0
	for _, path := range files {
		counts, err := stripTranscript(path, !*dryRun, metaOnly)
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(streams.stderr, "jad: %v\n", err)
			continue
		}
		if counts.Records > 0 {
			total.Records += counts.Records
			total.Steer += counts.Steer
			total.Meta += counts.Meta
			changed++
			_, _ = fmt.Fprintf(streams.stdout, "%s: %s %d records (%d steering markers, %d __agent_meta)\n", path, verb, counts.Records, counts.Steer, counts.Meta)
		}
	}
	_, _ = fmt.Fprintf(streams.stdout, "%d transcripts read, %s %d records in %d (%d steering markers, %d __agent_meta)\n", len(files)-failed, verb, total.Records, changed, total.Steer, total.Meta)
	if failed > 0 {
		return &exitError{code: exitFailed, err: fmt.Errorf("%d transcripts were not changed", failed)}
	}
	return nil
}

// transcriptFiles expands the arguments: a file is taken as it is, a folder is searched for
// files named messages.jsonl.
func transcriptFiles(args []string) ([]string, error) {
	var files []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, arg)
			continue
		}
		err = filepath.WalkDir(arg, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() && entry.Name() == "messages.jsonl" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// stripTranscript strips one transcript under its lock, which a run holds while it writes.
func stripTranscript(path string, write, metaOnly bool) (transcript.LocalStripCounts, error) {
	lockPath, err := sessionlock.PathFor(path)
	if err != nil {
		return transcript.LocalStripCounts{}, err
	}
	lock, err := sessionlock.TryAcquire(context.Background(), lockPath)
	if errors.Is(err, sessionlock.ErrBusy) {
		return transcript.LocalStripCounts{}, fmt.Errorf("%s is in use by a run; stop the server and try again", path)
	}
	if err != nil {
		return transcript.LocalStripCounts{}, fmt.Errorf("lock %s: %w", path, err)
	}
	defer lock.Close()
	return transcript.StripLocalFields(path, write, metaOnly)
}
