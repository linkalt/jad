package imagestore

import "example.com/agent-harness/backend-go/internal/sessionfs"

// FromSettings builds the store of the session's image_store setting. It returns nil when
// no directory is set, so every image stays inline.
func FromSettings(settings sessionfs.ImageStoreSettings) *Store {
	return New(Options{
		Dir:           settings.Dir,
		MinBytes:      settings.MinBytes,
		MaxFileBytes:  settings.MaxFileBytes,
		MaxTotalBytes: settings.MaxTotalBytes,
	})
}
