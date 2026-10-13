package native

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"jad/backend-go/internal/imagedata"
	"jad/backend-go/internal/imageresize"
	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// DecideImages is what decide needs to turn an image part into a data URL. Every field
// may be empty: no store means a stored-image reference is refused, and no resizer
// means the image is sent as it is.
type DecideImages struct {
	Store   *imagestore.Store
	Resizer *imageresize.Resizer
}

// loadDecideImages replaces the path or data of every image part by a data URL. It
// returns a ready tool result when one image cannot be used, and nothing otherwise.
func loadDecideImages(parts []decideInputPart, images DecideImages, settings sessionfs.NativeDecideSettings, execution tools.ExecutionContext) string {
	count := int64(0)
	for _, part := range parts {
		if part.isImage() {
			count++
		}
	}
	if count > settings.MaxImages {
		return decideError(execution.CallID, tools.StatusInvalidInput, "too_many_images", fmt.Sprintf("%d images; the limit is %d", count, settings.MaxImages))
	}
	for index := range parts {
		part := &parts[index]
		if !part.isImage() {
			continue
		}
		label := fmt.Sprintf("input part %d", index+1)
		var data []byte
		if part.ImagePath != "" {
			loaded, failure := readDecideImageFile(part.ImagePath, images, settings, execution, label)
			if failure != "" {
				return failure
			}
			data = loaded
		} else {
			decoded, failure := decodeDecideImageData(part.ImageData, settings, execution.CallID, label)
			if failure != "" {
				return failure
			}
			data = decoded
		}
		mime, err := imagedata.Sniff(data)
		if err != nil {
			return decideError(execution.CallID, tools.StatusInvalidInput, "unsupported_image", label+": the image is "+mime+"; png, jpeg, webp and gif are supported")
		}
		url := imagedata.DataURL(mime, data)
		if replaced, ok := images.Resizer.Resize(imageresize.SourceNative, url); ok {
			url = replaced
		}
		part.Image = url
	}
	return ""
}

// decodeDecideImageData decodes a base64 string, or a data URL, after checking that its
// decoded size is within the limit, so a huge string is never decoded.
func decodeDecideImageData(raw string, settings sessionfs.NativeDecideSettings, callID, label string) ([]byte, string) {
	data, err := imagedata.Decode(raw, settings.MaxImageBytes)
	switch {
	case errors.Is(err, imagedata.ErrTooLarge):
		return nil, decideError(callID, tools.StatusInvalidInput, "image_too_large", fmt.Sprintf("%s: the image is larger than %d bytes", label, settings.MaxImageBytes))
	case err != nil:
		return nil, decideError(callID, tools.StatusInvalidInput, "invalid_image", label+": "+err.Error())
	}
	return data, ""
}

// readDecideImageFile reads one image of a decide call by path, under decide's own
// path rules, or from the image store when the path is a stored-image reference.
func readDecideImageFile(path string, images DecideImages, settings sessionfs.NativeDecideSettings, execution tools.ExecutionContext, label string) ([]byte, string) {
	return readImageFile(path, settings.Policy, images.Store, settings.MaxImageBytes, execution, label)
}

// readWithin reads at most limit bytes. It reads one byte more to learn that the source
// is over the limit, and stops there, so a source of any size costs limit+1 bytes.
func readWithin(source io.Reader, limit int64) (data []byte, within bool, err error) {
	data, err = io.ReadAll(io.LimitReader(source, limit+1))
	if err != nil {
		return nil, false, err
	}
	return data, int64(len(data)) <= limit, nil
}

func imageReadFailure(callID string, err error, path string) string {
	if errors.Is(err, fs.ErrNotExist) {
		return decideError(callID, tools.StatusNotFound, "not_found", "Could not find "+path, tools.Information{Key: "path", Value: path})
	}
	return decideError(callID, tools.StatusError, "image_read_failed", "Could not read "+path, tools.Information{Key: "path", Value: path})
}
