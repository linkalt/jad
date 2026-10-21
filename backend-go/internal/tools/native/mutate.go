package native

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"jad/backend-go/internal/sessionfs"
)

const (
	backupAttemptLimit  = 5
	identifierByteCount = 8
	procSelfFDDirectory = "/proc/self/fd"
	externalBackupDir   = "@external"
)

type pinnedDirectory struct {
	canonicalPath string
	operationPath string
	handle        *os.File
}

type pinnedParent struct {
	targetPath string
	directory  *pinnedDirectory
}

type backupFile struct {
	path          string
	operationPath string
	modifiedAt    time.Time
	size          int64
}

type commitResult struct {
	backupPath  string
	error       string
	systemError string
}

func backupDirectoryFor(target mutateTarget, backupDir *string) string {
	if backupDir == nil {
		return filepath.Dir(target.realPath)
	}
	relative, err := filepath.Rel(target.workspaceRoot, target.realPath)
	insideWorkspace := err == nil && relative != "" && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
	mirrored := filepath.Dir(relative)
	if !insideWorkspace {
		hash := sha256.Sum256([]byte(target.realPath))
		mirrored = filepath.Join(externalBackupDir, hex.EncodeToString(hash[:])[:16])
	}
	return filepath.Join(*backupDir, mirrored)
}

func pinDirectory(directory string) (*pinnedDirectory, bool) {
	canonical := filepath.Clean(directory)
	before, err := os.Lstat(canonical)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, false
	}
	realPath, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		return nil, false
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil || filepath.Clean(realPath) != canonical {
		return nil, false
	}
	handle, err := os.Open(canonical)
	if err != nil {
		return nil, false
	}
	opened, err := handle.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		_ = handle.Close()
		return nil, false
	}
	operationPath := canonical
	if runtime.GOOS == "linux" {
		operationPath = filepath.Join(procSelfFDDirectory, strconv.FormatUint(uint64(handle.Fd()), 10))
		pinnedPath, err := filepath.EvalSymlinks(operationPath)
		if err != nil || filepath.Clean(pinnedPath) != canonical {
			_ = handle.Close()
			return nil, false
		}
	}
	return &pinnedDirectory{canonicalPath: canonical, operationPath: operationPath, handle: handle}, true
}

func pinTargetParent(targetPath string) (*pinnedParent, bool) {
	directory, ok := pinDirectory(filepath.Dir(targetPath))
	if !ok {
		return nil, false
	}
	return &pinnedParent{
		targetPath: filepath.Join(directory.operationPath, filepath.Base(targetPath)),
		directory:  directory,
	}, true
}

func pinBackupDirectory(target mutateTarget, settings sessionfs.NativeMutationSettings, parent *pinnedParent) (*pinnedDirectory, bool, bool) {
	if settings.BackupDir == nil {
		return &pinnedDirectory{
			canonicalPath: filepath.Dir(target.realPath),
			operationPath: filepath.Dir(parent.targetPath),
			handle:        parent.directory.handle,
		}, false, true
	}
	directory := backupDirectoryFor(target, settings.BackupDir)
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return nil, false, false
	}
	pinned, ok := pinDirectory(directory)
	return pinned, true, ok
}

func pinBackupSource(target mutateTarget, targetPath string) (*os.File, bool) {
	if target.identity == nil {
		return nil, false
	}
	handle, err := os.Open(targetPath)
	if err != nil {
		return nil, false
	}
	opened, err := handle.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, target.identity) {
		_ = handle.Close()
		return nil, false
	}
	return handle, true
}

func createBackup(target mutateTarget, source *os.File, directory *pinnedDirectory) (string, error) {
	prefix := filepath.Base(target.realPath) + ".bak-"
	for attempt := 0; attempt < backupAttemptLimit; attempt++ {
		var random [identifierByteCount]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		name := fmt.Sprintf("%s%013d-%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(random[:]))
		backupPath := filepath.Join(directory.canonicalPath, name)
		operationPath := filepath.Join(directory.operationPath, name)
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		backup, err := os.OpenFile(operationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, target.mode)
		if err != nil {
			if errors.Is(err, fsErrExist) && attempt+1 < backupAttemptLimit {
				continue
			}
			return "", err
		}
		_, copyErr := io.Copy(backup, source)
		if copyErr == nil && target.mode != 0 {
			copyErr = backup.Chmod(target.mode)
		}
		closeErr := backup.Close()
		if copyErr != nil || closeErr != nil {
			_ = os.Remove(operationPath)
			if copyErr != nil {
				return "", copyErr
			}
			return "", closeErr
		}
		return backupPath, nil
	}
	return "", errors.New("could not create a unique backup path")
}

func listBackups(target mutateTarget, directory *pinnedDirectory) []backupFile {
	prefix := filepath.Base(target.realPath) + ".bak-"
	entries, err := os.ReadDir(directory.operationPath)
	if err != nil {
		return nil
	}
	backups := make([]backupFile, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || !validBackupSuffix(strings.TrimPrefix(entry.Name(), prefix)) {
			continue
		}
		operationPath := filepath.Join(directory.operationPath, entry.Name())
		info, err := os.Lstat(operationPath)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		backups = append(backups, backupFile{
			path:          filepath.Join(directory.canonicalPath, entry.Name()),
			operationPath: operationPath,
			modifiedAt:    info.ModTime(),
			size:          info.Size(),
		})
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].modifiedAt.Equal(backups[j].modifiedAt) {
			return backups[i].path > backups[j].path
		}
		return backups[i].modifiedAt.After(backups[j].modifiedAt)
	})
	return backups
}

func validBackupSuffix(value string) bool {
	if len(value) != 13+1+16 || value[13] != '-' {
		return false
	}
	for _, character := range value[:13] {
		if character < '0' || character > '9' {
			return false
		}
	}
	for _, character := range value[14:] {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func pruneBackups(target mutateTarget, settings sessionfs.NativeMutationSettings, directory *pinnedDirectory, keepPath string) {
	backups := listBackups(target, directory)
	retainedCount := int64(0)
	retainedBytes := int64(0)
	for _, backup := range backups {
		if backup.path == keepPath {
			retainedCount = 1
			retainedBytes = backup.size
			break
		}
	}
	for _, backup := range backups {
		if backup.path == keepPath {
			continue
		}
		withinCount := retainedCount < settings.BackupMaxCount
		withinBytes := settings.BackupMaxTotalBytes == 0 || retainedBytes+backup.size <= settings.BackupMaxTotalBytes
		if withinCount && withinBytes {
			retainedCount++
			retainedBytes += backup.size
			continue
		}
		_ = os.Remove(backup.operationPath)
	}
}

func targetStillMatches(target mutateTarget, targetPath string) bool {
	current, err := os.Lstat(targetPath)
	if !target.existed {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && current.Mode().IsRegular() && target.identity != nil && os.SameFile(current, target.identity)
}

func hasSameContent(target mutateTarget, data []byte) bool {
	if target.size != int64(len(data)) || target.identity == nil {
		return false
	}
	handle, err := os.Open(target.realPath)
	if err != nil {
		return false
	}
	defer handle.Close()
	before, err := handle.Stat()
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(before, target.identity) {
		return false
	}
	current, err := io.ReadAll(handle)
	if err != nil {
		return false
	}
	after, err := handle.Stat()
	return err == nil && os.SameFile(after, target.identity) && bytes.Equal(current, data)
}

func commitFile(target mutateTarget, settings sessionfs.NativeMutationSettings, data []byte, makeBackup bool) commitResult {
	parent, ok := pinTargetParent(target.realPath)
	if !ok {
		return commitResult{error: "target_changed"}
	}
	defer parent.directory.handle.Close()
	if !targetStillMatches(target, parent.targetPath) {
		return commitResult{error: "target_changed"}
	}

	var backupDirectory *pinnedDirectory
	var ownsBackupDirectory bool
	var backupSource *os.File
	var backupPath string
	if makeBackup && target.existed {
		backupDirectory, ownsBackupDirectory, ok = pinBackupDirectory(target, settings, parent)
		if !ok {
			return commitResult{error: "backup_failed"}
		}
		if ownsBackupDirectory {
			defer backupDirectory.handle.Close()
		}
		backupSource, ok = pinBackupSource(target, parent.targetPath)
		if !ok {
			return commitResult{error: "target_changed"}
		}
		defer backupSource.Close()
		backupPath, _ = createBackup(target, backupSource, backupDirectory)
		if backupPath == "" {
			return commitResult{error: "backup_failed"}
		}
	}

	if !targetStillMatches(target, parent.targetPath) {
		if backupPath != "" && backupDirectory != nil {
			_ = os.Remove(filepath.Join(backupDirectory.operationPath, filepath.Base(backupPath)))
		}
		return commitResult{error: "target_changed"}
	}
	if err := writeAtomically(parent.targetPath, data, target.mode, settings.Fsync, parent.directory); err != nil {
		return commitResult{backupPath: backupPath, error: "write_failed", systemError: systemErrorCode(err)}
	}
	if backupPath != "" && backupDirectory != nil {
		pruneBackups(target, settings, backupDirectory, backupPath)
	}
	return commitResult{backupPath: backupPath}
}

func writeAtomically(targetPath string, data []byte, mode os.FileMode, shouldSync bool, parent *pinnedDirectory) error {
	var random [identifierByteCount]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temporaryPath := targetPath + ".tmp-" + hex.EncodeToString(random[:])
	fileMode := mode
	if fileMode == 0 {
		fileMode = 0o666
	}
	handle, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	written, writeErr := handle.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil && mode != 0 {
		writeErr = handle.Chmod(mode)
	}
	if writeErr == nil && shouldSync {
		writeErr = handle.Sync()
	}
	closeErr := handle.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(temporaryPath)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if shouldSync && parent != nil && parent.handle != nil {
		// Best effort: the rename already happened; some filesystems cannot sync a directory.
		_ = parent.handle.Sync()
	}
	return nil
}

func systemErrorCode(err error) string {
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		err = pathError.Err
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return ""
	}
	switch errno {
	case syscall.EACCES:
		return "EACCES"
	case syscall.EEXIST:
		return "EEXIST"
	case syscall.EIO:
		return "EIO"
	case syscall.ENOSPC:
		return "ENOSPC"
	case syscall.EPERM:
		return "EPERM"
	case syscall.EROFS:
		return "EROFS"
	default:
		return ""
	}
}

var fsErrExist = os.ErrExist
