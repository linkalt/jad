package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// ProcessRecoveryReport counts what RecoverProcesses looked at.
type ProcessRecoveryReport struct {
	// Sessions is the number of session directories with a setting.json.
	Sessions int
	// Storages is the number of distinct process storage directories recovered.
	Storages int
	// Unreadable counts sessions and profiles whose settings could not be loaded.
	Unreadable int
	// Failed counts storages whose recovery returned an error.
	Failed int
}

// RecoverProcesses recovers the managed-process storage of every session and profile
// that configures the process tool, so a process a crashed server left running is
// stopped at start instead of at the first use of the process tool in its session.
//
// It only visits a storage directory that already exists: a session that never started
// a process gets no new directory. It builds the same shared manager a run uses, so
// the recovery done here is the recovery that run would have done. Recovery of one
// storage is serialized by the manager, so a run that starts meanwhile waits for it.
// A session or profile that cannot be read is skipped and logged; it never stops the
// scan. The context is checked before each session and before each storage; a recovery
// already going on is not interrupted.
func (r *Resolver) RecoverProcesses(ctx context.Context) (ProcessRecoveryReport, error) {
	var report ProcessRecoveryReport
	if ctx == nil {
		return report, errors.New("process recovery context is required")
	}
	seen := make(map[string]struct{})
	for _, root := range r.roots {
		entries, err := os.ReadDir(root.Directory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return report, fmt.Errorf("read session root %q: %w", root.ID, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			// Loading a session's settings is the cost of a session without the tool.
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if err := r.recoverSessionProcesses(ctx, root, entry.Name(), seen, &report); err != nil {
				return report, err
			}
		}
	}
	return report, nil
}

func (r *Resolver) recoverSessionProcesses(ctx context.Context, root sessionfs.Root, name string, seen map[string]struct{}, report *ProcessRecoveryReport) error {
	location, err := sessionfs.Resolve(r.roots, root.ID+":"+name)
	if err != nil {
		var resolveErr *sessionfs.ResolveError
		if errors.As(err, &resolveErr) {
			return nil
		}
		return err
	}
	hasSetting, err := hasSessionSetting(location.Directory)
	if err != nil {
		r.logger.Warn("skipping session for process recovery", "session", location.Reference, "error", err)
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
	// The active profile is the base config; every other profile may keep its processes
	// in its own tool storage, so each one is looked at.
	configs := []sessionfs.SessionConfig{base}
	for _, profile := range base.PathProfiles {
		if profile == base.Profile {
			continue
		}
		config, err := sessionfs.LoadSessionSettings(location, profile, "")
		if err != nil {
			r.logger.Warn("skipping profile for process recovery", "session", location.Reference, "profile", profile, "error", err)
			report.Unreadable++
			continue
		}
		configs = append(configs, config)
	}
	for _, config := range configs {
		if config.NativeProcess == nil {
			continue
		}
		// A recovery can last the whole kill grace, so stop before starting the next.
		if err := ctx.Err(); err != nil {
			return err
		}
		storage := filepath.Clean(config.NativeProcess.StorageDir)
		if _, done := seen[storage]; done {
			continue
		}
		seen[storage] = struct{}{}
		if info, err := os.Stat(storage); err != nil || !info.IsDir() {
			continue
		}
		manager := r.sharedProcessManager(config)
		if manager == nil {
			return errors.New("resolver is closed")
		}
		report.Storages++
		if err := manager.Recover(); err != nil {
			r.logger.Warn("process recovery failed", "session", location.Reference, "storage", storage, "error", err)
			report.Failed++
		}
	}
	return nil
}
