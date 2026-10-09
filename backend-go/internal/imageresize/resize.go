// Package imageresize shrinks the images of an outgoing Responses request. It works on
// a copy of the history that is about to be sent and never on the transcript, so
// messages.jsonl keeps the original image and the golden output does not change.
package imageresize

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"

	// Registered for image.DecodeConfig and image.Decode only; webp has no encoder here.
	_ "golang.org/x/image/webp"

	"golang.org/x/image/draw"
)

// Options are the limits one Resizer works with. The caller owns the defaults.
type Options struct {
	// MaxEdgePx is the longest side an image may keep.
	MaxEdgePx int
	// MaxBytes is the largest encoded image that is sent as it is.
	MaxBytes int
	// MaxPixels refuses to decode an image with more pixels (a decompression bomb guard).
	MaxPixels int64
	// JPEGQuality is the first quality tried; lower ones follow when the result is too big.
	JPEGQuality int
	// CacheEntries bounds the memo of finished results.
	CacheEntries int
}

const (
	minJPEGQuality = 40
	qualityStep    = 15
	// shrinkFactor and maxShrinks bound the retries when even the lowest quality is too big.
	shrinkFactor = 0.75
	maxShrinks   = 4
)

var errUnchanged = errors.New("image is left as it is")

// parseDataURL splits data:<mime>;base64,<data>. Other URLs (http, file ids) are not
// resized because the bytes are not here.
func parseDataURL(url string) (mime string, data []byte, ok bool) {
	rest, found := strings.CutPrefix(url, "data:")
	if !found {
		return "", nil, false
	}
	header, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", nil, false
	}
	mime, found = strings.CutSuffix(header, ";base64")
	if !found {
		return "", nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	return strings.ToLower(mime), decoded, true
}

// resizeDataURL returns the data URL of the shrunk image, or errUnchanged when the image
// already fits, is not a supported kind, or could not be improved.
func resizeDataURL(url string, options Options) (string, error) {
	mime, raw, ok := parseDataURL(url)
	if !ok {
		return "", errUnchanged
	}
	switch mime {
	case "image/png", "image/jpeg", "image/jpg", "image/webp":
	default:
		// gif may be animated and every other type has no decoder here.
		return "", errUnchanged
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return "", errUnchanged
	}
	if int64(config.Width)*int64(config.Height) > options.MaxPixels {
		return "", errUnchanged
	}
	orientation := 1
	if mime == "image/jpeg" || mime == "image/jpg" {
		orientation = jpegOrientation(raw)
	}
	tooBig := max(config.Width, config.Height) > options.MaxEdgePx
	// A turned jpeg is rewritten even when it fits: re-encoding drops the EXIF tag, so
	// the rotation has to be applied to the pixels or the provider would see it sideways.
	if !tooBig && len(raw) <= options.MaxBytes && orientation == 1 {
		return "", errUnchanged
	}
	source, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", errUnchanged
	}
	source = orient(source, orientation)
	encoded, outMIME, resized, err := encodeFitting(source, options, len(raw))
	if err != nil {
		return "", errUnchanged
	}
	// Keep the original unless the new image is smaller or has fewer pixels.
	if len(encoded) >= len(raw) && !resized && orientation == 1 {
		return "", errUnchanged
	}
	return "data:" + outMIME + ";base64," + base64.StdEncoding.EncodeToString(encoded), nil
}

// encodeFitting scales the image to MaxEdgePx and encodes it under MaxBytes. An image with
// transparency stays a png. An opaque image stays a png while that is no larger than
// ceiling (the size of the original): scaling smooths flat areas, so a scaled screenshot
// can be several times larger as a png than the original was, and then a jpeg is sent
// instead. When nothing fits, it shrinks the image by shrinkFactor, at most maxShrinks times.
func encodeFitting(source image.Image, options Options, ceiling int) (data []byte, mime string, resized bool, err error) {
	current := source
	if edge := max(current.Bounds().Dx(), current.Bounds().Dy()); edge > options.MaxEdgePx {
		current = scale(current, float64(options.MaxEdgePx)/float64(edge))
		resized = true
	}
	opaque := isOpaque(current)
	for attempt := 0; ; attempt++ {
		encodedPNG, pngErr := encodePNG(current)
		pngFits := pngErr == nil && len(encodedPNG) <= options.MaxBytes
		if pngFits && (!opaque || len(encodedPNG) <= ceiling) {
			return encodedPNG, "image/png", resized, nil
		}
		if opaque {
			for quality := options.JPEGQuality; ; quality -= qualityStep {
				quality = max(quality, minJPEGQuality)
				encoded, jpegErr := encodeJPEG(current, quality)
				if jpegErr != nil {
					return nil, "", false, jpegErr
				}
				if len(encoded) <= options.MaxBytes && (!pngFits || len(encoded) < len(encodedPNG)) {
					return encoded, "image/jpeg", resized, nil
				}
				if quality == minJPEGQuality {
					break
				}
			}
			if pngFits {
				// The jpeg was not smaller: the png is the better of two large results.
				return encodedPNG, "image/png", resized, nil
			}
		}
		if attempt == maxShrinks {
			return nil, "", false, errUnchanged
		}
		current = scale(current, shrinkFactor)
		resized = true
	}
}

func scale(source image.Image, factor float64) image.Image {
	bounds := source.Bounds()
	width := max(1, int(float64(bounds.Dx())*factor+0.5))
	height := max(1, int(float64(bounds.Dy())*factor+0.5))
	target := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(target, target.Bounds(), source, bounds, draw.Src, nil)
	return target
}

func isOpaque(source image.Image) bool {
	if opaque, ok := source.(interface{ Opaque() bool }); ok {
		return opaque.Opaque()
	}
	bounds := source.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, alpha := source.At(x, y).RGBA(); alpha != 0xffff {
				return false
			}
		}
	}
	return true
}

var pngEncoder = png.Encoder{CompressionLevel: png.BestCompression}

func encodePNG(source image.Image) ([]byte, error) {
	var buffer bytes.Buffer
	if err := pngEncoder.Encode(&buffer, source); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func encodeJPEG(source image.Image, quality int) ([]byte, error) {
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, flatten(source), &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// flatten returns an opaque copy for the jpeg encoder, which has no alpha channel.
func flatten(source image.Image) image.Image {
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Over)
	return target
}
