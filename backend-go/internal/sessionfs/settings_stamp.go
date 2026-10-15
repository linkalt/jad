package sessionfs

import (
	"os"
	"time"
)

// SettingsStamp says which catalog files and folders a settings load read and what they
// looked like, so a caller can tell later whether loading again would read the same.
//
// It covers the session file, the profile files, the toolset file and the catalog
// folders. It does not cover prompt documents, env files and the files of sub-agent
// folders, so a change to only one of those is not seen.
type SettingsStamp struct {
	files []fileStamp
}

type fileStamp struct {
	path    string
	exists  bool
	size    int64
	modTime time.Time
	taken   time.Time
}

// racyStampWindow is how close to the stamp a modification time may be before the
// stamp cannot tell an edit from no edit: file systems keep times at a coarse
// granularity (a jiffy, or two seconds on some), so a same-size edit or a folder change
// within one tick of the stamp leaves the same size and time.
const racyStampWindow = 2 * time.Second

func statStamp(path string) fileStamp {
	taken := time.Now()
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{path: path, taken: taken}
	}
	return fileStamp{path: path, exists: true, size: info.Size(), modTime: info.ModTime(), taken: taken}
}

func (f fileStamp) racy() bool {
	return f.exists && f.taken.Sub(f.modTime) < racyStampWindow
}

// Current reports whether every stamped file and folder still has the size and
// modification time it had when it was read. A file changed within racyStampWindow of
// its stamp is never current, because the stamp could not show a further change.
func (s SettingsStamp) Current() bool {
	if len(s.files) == 0 {
		return false
	}
	for _, before := range s.files {
		if before.racy() {
			return false
		}
		now := statStamp(before.path)
		if now.exists != before.exists || now.size != before.size || !now.modTime.Equal(before.modTime) {
			return false
		}
	}
	return true
}

// LoadSessionSettingsStamped is LoadSessionSettings that also returns the stamp of what
// it read. Each file is stamped just before it is read, so a change made after the stamp
// is always seen by Current.
func LoadSessionSettingsStamped(location Location, requestedProfile, requestedModel string) (SessionConfig, SettingsStamp, error) {
	scans := NewCatalogScans()
	scans.recording = true
	config, err := LoadSessionSettingsScoped(location, requestedProfile, requestedModel, scans)
	if err != nil {
		return SessionConfig{}, SettingsStamp{}, err
	}
	return config, scans.stamp(), nil
}
