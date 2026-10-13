package app

import (
	"jad/backend-go/internal/imageresize"
	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
)

// newImageResizer builds the resizer of one run from the session's image_resize setting.
// It returns nil when the setting is off, so the runner sends the history as stored.
func newImageResizer(settings sessionfs.ImageResizeSettings) *imageresize.Resizer {
	return imageresize.FromSettings(settings)
}

// newImageStore builds the image store of one run from the session's image_store setting.
// It returns nil when no directory is set, so every image stays inline in the transcript.
func newImageStore(settings sessionfs.ImageStoreSettings) *imagestore.Store {
	return imagestore.FromSettings(settings)
}
