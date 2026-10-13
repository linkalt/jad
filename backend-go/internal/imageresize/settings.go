package imageresize

import "jad/backend-go/internal/sessionfs"

// FromSettings builds the resizer of the session's image_resize setting. It returns nil
// when the setting is off, so a caller sends the images as they are.
func FromSettings(settings sessionfs.ImageResizeSettings) *Resizer {
	if !settings.Active() {
		return nil
	}
	return New(Options{
		MaxEdgePx:    int(settings.MaxEdgePx),
		MaxBytes:     int(settings.MaxBytes),
		MaxPixels:    settings.MaxPixels,
		JPEGQuality:  int(settings.JPEGQuality),
		CacheEntries: int(settings.CacheEntries),
	}, settings.AppliesTo)
}
