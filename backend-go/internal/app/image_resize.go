package app

import (
	"example.com/agent-harness/backend-go/internal/imageresize"
	"example.com/agent-harness/backend-go/internal/imagestore"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// newImageResizer builds the resizer of one run from the session's image_resize setting.
// It returns nil when the setting is off, so the runner sends the history as stored.
func newImageResizer(settings sessionfs.ImageResizeSettings) *imageresize.Resizer {
	if !settings.Active() {
		return nil
	}
	return imageresize.New(imageresize.Options{
		MaxEdgePx:    int(settings.MaxEdgePx),
		MaxBytes:     int(settings.MaxBytes),
		MaxPixels:    settings.MaxPixels,
		JPEGQuality:  int(settings.JPEGQuality),
		CacheEntries: int(settings.CacheEntries),
	}, settings.AppliesTo)
}

// newImageStore builds the image store of one run from the session's image_store setting.
// It returns nil when no directory is set, so every image stays inline in the transcript.
func newImageStore(settings sessionfs.ImageStoreSettings) *imagestore.Store {
	return imagestore.New(imagestore.Options{
		Dir:           settings.Dir,
		MinBytes:      settings.MinBytes,
		MaxFileBytes:  settings.MaxFileBytes,
		MaxTotalBytes: settings.MaxTotalBytes,
	})
}
