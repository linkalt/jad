package transcript

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const (
	partialContentFilename = "responses.partial"
	partialOwnerFilename   = "responses.partial.owner"
	partialCompactFilename = "responses.partial.compact"
	partialCleanupLockName = "responses.partial.cleanup-lock"
)

type partialOwnerRecord struct {
	Hostname string `json:"hostname"`
	PID      int    `json:"pid"`
}

// cleanupOrphanedPartialFiles removes only cache files whose same-host owner
// process is confirmed gone. It deliberately leaves staged output, manifests,
// malformed owners, remote owners, and unknown process states untouched.
func cleanupOrphanedPartialFiles(stagingDir string, logger *slog.Logger) {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("scan response staging for partial caches", slog.Any("error", err))
		}
		return
	}
	hostname, err := os.Hostname()
	if err != nil {
		logger.Warn("read hostname while cleaning partial caches", slog.Any("error", err))
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		turnDir := filepath.Join(stagingDir, entry.Name())
		ownerPath := filepath.Join(turnDir, partialOwnerFilename)
		lockPath := filepath.Join(turnDir, partialCleanupLockName)
		release, ok := acquirePartialCleanupLock(lockPath, hostname, logger)
		if !ok {
			continue
		}
		cleanupPartialCache(turnDir, ownerPath, hostname, logger)
		release()
	}
}

func cleanupPartialCache(turnDir, ownerPath, hostname string, logger *slog.Logger) {
	ownerBytes, err := os.ReadFile(ownerPath)
	if err != nil {
		return
	}
	var owner partialOwnerRecord
	if json.Unmarshal(ownerBytes, &owner) != nil || owner.PID <= 0 || owner.Hostname != hostname {
		return
	}
	alive, confirmed := processAlive(owner.PID)
	if !confirmed || alive {
		return
	}
	for _, filename := range []string{partialContentFilename, partialCompactFilename, partialOwnerFilename} {
		path := filepath.Join(turnDir, filename)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("remove orphaned response partial cache", slog.String("path", path), slog.Any("error", err))
		}
	}
}

func acquirePartialCleanupLock(lockPath, hostname string, logger *slog.Logger) (func(), bool) {
	for attempt := 0; attempt < 2; attempt++ {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			payload, marshalErr := json.Marshal(partialOwnerRecord{Hostname: hostname, PID: os.Getpid()})
			if marshalErr != nil {
				_ = lock.Close()
				_ = os.Remove(lockPath)
				return nil, false
			}
			if _, writeErr := lock.Write(append(payload, '\n')); writeErr != nil {
				_ = lock.Close()
				_ = os.Remove(lockPath)
				return nil, false
			}
			if closeErr := lock.Close(); closeErr != nil {
				_ = os.Remove(lockPath)
				return nil, false
			}
			return func() {
				if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					logger.Warn("remove response partial cleanup lock", slog.String("path", lockPath), slog.Any("error", err))
				}
			}, true
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, false
		}
		lockBytes, readErr := os.ReadFile(lockPath)
		var lockOwner partialOwnerRecord
		if readErr != nil || json.Unmarshal(lockBytes, &lockOwner) != nil || lockOwner.Hostname != hostname || lockOwner.PID <= 0 {
			return nil, false
		}
		alive, confirmed := processAlive(lockOwner.PID)
		if !confirmed || alive {
			return nil, false
		}
		stalePath := lockPath + ".stale-" + strconv.Itoa(os.Getpid())
		if err := os.Rename(lockPath, stalePath); err != nil {
			return nil, false
		}
		_ = os.Remove(stalePath)
	}
	return nil, false
}

// processAlive returns whether a PID is alive and whether that answer is
// definitive. Permission errors count as alive; unknown OS errors are kept.
func processAlive(pid int) (alive, confirmed bool) {
	if pid <= 0 {
		return false, false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, true
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true, true
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return false, true
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) {
		return true, true
	}
	return false, false
}
