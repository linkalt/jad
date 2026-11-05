package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"jad/backend-go/internal/sessionfs"
)

// StorageRecoveryOptions chooses which storages RecoverStorages visits.
type StorageRecoveryOptions struct {
	// SubagentRuns recovers the sub-agent run manifests: a run the last server left
	// queued or running is marked lost, and the oldest ended runs beyond keep_finished
	// are dropped.
	SubagentRuns bool
}

// StorageRecoveryReport counts what RecoverStorages looked at.
type StorageRecoveryReport struct {
	// Sessions is the number of session directories with a setting.json.
	Sessions int
	// RunStorages is the number of distinct sub-agent storage directories recovered.
	RunStorages int
	// Unreadable counts sessions and profiles whose settings could not be loaded.
	Unreadable int
}

// RecoverStorages recovers, for every session and path profile, the storages the tools
// of that session keep between server runs, so the recovery happens at start instead of
// at the first use of the tool in its session. A restart otherwise leaves a stale
// "running" manifest until someone touches the session.
//
// It only visits a storage directory that already exists: a session that never started a
// child gets no new directory. It builds the same shared manager a run uses, so the
// recovery done here is the recovery that run would have done. A session or
// profile that cannot be read is skipped and logged; it never stops the scan. The context
// is checked before each session and before each storage; a recovery already going on is
// not interrupted.
func (r *Resolver) RecoverStorages(ctx context.Context, options StorageRecoveryOptions) (StorageRecoveryReport, error) {
	var report StorageRecoveryReport
	if ctx == nil {
		return report, errors.New("storage recovery context is required")
	}
	seenRuns := make(map[string]struct{})
	for _, root := range r.roots {
		entries, err := os.ReadDir(root.Directory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return report, fmt.Errorf("read session root %q: %w", root.ID, err)
		}
		for _, entry := range entries {
			id, ok := sessionfs.SessionFileID(entry)
			if !ok {
				continue
			}
			// Loading a session's settings is the cost of a session without the tools.
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if err := r.recoverSessionStorages(ctx, root, id, options, seenRuns, &report); err != nil {
				return report, err
			}
		}
	}
	return report, nil
}

func (r *Resolver) recoverSessionStorages(
	ctx context.Context, root sessionfs.Root, name string, options StorageRecoveryOptions,
	seenRuns map[string]struct{}, report *StorageRecoveryReport,
) error {
	location, err := sessionfs.Resolve(r.roots, root.ID+":"+name)
	if err != nil {
		var resolveErr *sessionfs.ResolveError
		if errors.As(err, &resolveErr) {
			return nil
		}
		return err
	}
	hasSetting, err := hasSessionSetting(location)
	if err != nil {
		r.logger.Warn("skipping session for storage recovery", "session", location.Reference, "error", err)
		report.Unreadable++
		return nil
	}
	if !hasSetting {
		return nil
	}
	report.Sessions++
	base, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		r.skipped.report(r.logger, location.Reference, err.Error())
		report.Unreadable++
		return nil
	}
	// The active profile is the base config; every other profile may keep its tool
	// storage, and so its child runs, in its own folder, so each is read.
	configs := []sessionfs.SessionConfig{base}
	for _, profile := range base.Profiles {
		if profile == base.Profile {
			continue
		}
		config, err := sessionfs.LoadSessionSettings(location, profile, "")
		if err != nil {
			r.logger.Warn("skipping profile for storage recovery", "session", location.Reference, "profile", profile, "error", err)
			report.Unreadable++
			continue
		}
		configs = append(configs, config)
	}
	for _, config := range configs {
		if options.SubagentRuns {
			if err := r.recoverSubagentStorage(ctx, config, seenRuns, report); err != nil {
				return err
			}
		}
	}
	return nil
}

// recoverSubagentStorage builds the shared sub-agent manager of a storage that keeps run
// manifests; building it is what loads the manifests and marks the interrupted runs lost.
// A session that does not persist its runs has nothing on disk to recover.
func (r *Resolver) recoverSubagentStorage(
	ctx context.Context, config sessionfs.SessionConfig, seen map[string]struct{}, report *StorageRecoveryReport,
) error {
	if config.NativeSubagent == nil || !config.NativeSubagent.PersistRuns {
		return nil
	}
	storage := filepath.Clean(config.NativeSubagent.StorageDir)
	if !firstVisit(seen, storage) || !directoryExists(storage) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.sharedSubagentManager(config) == nil {
		return errors.New("resolver is closed")
	}
	report.RunStorages++
	return nil
}

// firstVisit records path in seen and reports whether it was new.
func firstVisit(seen map[string]struct{}, path string) bool {
	if _, done := seen[path]; done {
		return false
	}
	seen[path] = struct{}{}
	return true
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
