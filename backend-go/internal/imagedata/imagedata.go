// Package imagedata decodes an image a caller sent as base64 or as a data URL and checks
// by its bytes that it is a type the providers take. The decide tool and the run request
// share it, so both refuse the same input for the same reason.
package imagedata

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

var (
	// ErrInvalid: the text is not base64, or a data URL that is not base64, or is empty.
	ErrInvalid = errors.New("the data is not valid base64")
	// ErrTooLarge: the decoded image is over the limit.
	ErrTooLarge = errors.New("the image is too large")
	// ErrUnsupported: the bytes are not a png, jpeg, webp or gif.
	ErrUnsupported = errors.New("the image is not a png, jpeg, webp or gif")
)

// mimeTypes are what http.DetectContentType finds for the types the providers take.
var mimeTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// Decode decodes base64 text or a base64 data URL. The decoded size is judged from the
// text length first, so a huge string is never decoded. maxBytes is the limit on the
// decoded image.
func Decode(raw string, maxBytes int64) ([]byte, error) {
	encoded := strings.TrimSpace(raw)
	if strings.HasPrefix(encoded, "data:") {
		header, body, found := strings.Cut(encoded, ",")
		if !found || !strings.HasSuffix(header, ";base64") {
			return nil, ErrInvalid
		}
		encoded = body
	}
	encoded = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, encoded)
	if int64(len(encoded))/4*3 > maxBytes+3 {
		return nil, ErrTooLarge
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(data) == 0 {
		return nil, ErrInvalid
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Sniff returns the MIME type of data, judged by its first bytes and never by a name or
// a declared type. It returns ErrUnsupported for anything but png, jpeg, webp and gif.
func Sniff(data []byte) (string, error) {
	mime := http.DetectContentType(data)
	if !mimeTypes[mime] {
		return mime, ErrUnsupported
	}
	return mime, nil
}

// DataURL returns the base64 data URL of data.
func DataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
