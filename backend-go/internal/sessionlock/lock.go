// Package sessionlock provides cancellable, cross-process advisory locks for
// one session directory. Lock files are persistent: removing a lock file while
// another process is waiting could split waiters across different inodes.
package sessionlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const acquirePollInterval = 25 * time.Millisecond

// PathFor derives a hidden, stable lock-file path beside target. Lock files are
// intentionally separate from the protected file so atomic replacement of the
// target cannot change the lock inode.
func PathFor(target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", errors.New("session lock target is required")
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve session lock target: %w", err)
	}
	base := filepath.Base(absolute)
	return filepath.Join(filepath.Dir(absolute), "."+base+".jad-lock"), nil
}

// IsLockFileName reports whether name is one of the hidden lock files created
// by this package, so workspace file listings can keep implementation details
// out of the public API even when hidden files are enabled.
func IsLockFileName(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".jad-lock")
}

// Lock owns one acquired operating-system lock. Close releases the lock and is
// safe to call more than once.
type Lock struct {
	file      *os.File
	waited    bool
	closeOnce sync.Once
	closeErr  error
}

// Waited reports whether another holder made Acquire wait for this lock.
func (l *Lock) Waited() bool {
	return l != nil && l.waited
}

// ErrBusy is returned by TryAcquire when another holder has the lock.
var ErrBusy = errors.New("session lock is held")

// TryAcquire locks path without waiting and returns ErrBusy when it is held.
func TryAcquire(ctx context.Context, path string) (*Lock, error) {
	file, err := openLockFile(ctx, path)
	if err != nil {
		return nil, err
	}
	acquired, tryErr := tryAcquire(file)
	if tryErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire session lock: %w", tryErr)
	}
	if !acquired {
		_ = file.Close()
		return nil, ErrBusy
	}
	return &Lock{file: file}, nil
}

func openLockFile(ctx context.Context, path string) (*os.File, error) {
	if ctx == nil {
		return nil, errors.New("session lock context is required")
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("session lock path is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve session lock path: %w", err)
	}
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create session lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	return file, nil
}

// Acquire waits until path can be locked or ctx is cancelled. Callers should
// use a stable path shared by all processes that mutate the same session.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	file, err := openLockFile(ctx, path)
	if err != nil {
		return nil, err
	}
	lock := &Lock{file: file}

	ticker := time.NewTicker(acquirePollInterval)
	defer ticker.Stop()
	for {
		acquired, tryErr := tryAcquire(file)
		if tryErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("acquire session lock: %w", tryErr)
		}
		if acquired {
			if err := ctx.Err(); err != nil {
				_ = lock.Close()
				return nil, err
			}
			return lock, nil
		}
		lock.waited = true
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Close releases the operating-system lock and closes its descriptor.
func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.file == nil {
			return
		}
		unlockErr := release(l.file)
		closeErr := l.file.Close()
		l.closeErr = errors.Join(unlockErr, closeErr)
	})
	return l.closeErr
}
