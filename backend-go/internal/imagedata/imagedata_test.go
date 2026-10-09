package imagedata

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func encode(t *testing.T, format string) []byte {
	t.Helper()
	picture := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	var buffer bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buffer, picture)
	case "jpeg":
		err = jpeg.Encode(&buffer, picture, nil)
	case "gif":
		err = gif.Encode(&buffer, picture, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestDecodeAcceptsBase64AndDataURLsInEveryForm(t *testing.T) {
	data := encode(t, "png")
	text := base64.StdEncoding.EncodeToString(data)
	for name, input := range map[string]string{
		"plain":          text,
		"data url":       "data:image/png;base64," + text,
		"padding gone":   strings.TrimRight(text, "="),
		"line breaks":    text[:10] + "\n" + text[10:20] + "\r\n" + text[20:],
		"spaces":         "  " + text[:8] + " 	" + text[8:] + "  ",
		"wrong declared": "data:image/gif;base64," + text,
	} {
		got, err := Decode(input, int64(len(data)))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s: %v, %d bytes (want %d)", name, err, len(got), len(data))
		}
	}
}

func TestDecodeRefusesWhatIsNotBase64(t *testing.T) {
	text := base64.StdEncoding.EncodeToString(encode(t, "png"))
	for name, input := range map[string]string{
		"empty":            "",
		"only spaces":      "   ",
		"not base64":       "this is not base64!",
		"url not base64":   "data:image/png,rawbytes",
		"url without data": "data:image/png;base64",
		"url ;base64 gone": "data:image/png;charset=utf8," + text,
		"empty body":       "data:image/png;base64,",
	} {
		if _, err := Decode(input, 1<<20); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestDecodeLimitIsAboutDecodedBytes(t *testing.T) {
	data := encode(t, "png")
	text := base64.StdEncoding.EncodeToString(data)
	if _, err := Decode(text, int64(len(data))); err != nil {
		t.Fatalf("exactly at the limit: %v", err)
	}
	if _, err := Decode(text, int64(len(data))-1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("one byte over: err = %v, want ErrTooLarge", err)
	}
	// Far over the limit and not even base64: the size answers first, so the text is
	// never decoded.
	if _, err := Decode(strings.Repeat("!", 100000), 1000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("long text: err = %v, want ErrTooLarge", err)
	}
}

func TestSniffJudgesTheBytesAndNotTheDeclaredType(t *testing.T) {
	for format, want := range map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"} {
		if mime, err := Sniff(encode(t, format)); err != nil || mime != want {
			t.Errorf("%s: %q, %v", format, mime, err)
		}
	}
	webp := append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)
	if mime, err := Sniff(webp); err != nil || mime != "image/webp" {
		t.Errorf("webp: %q, %v", mime, err)
	}
	for name, data := range map[string][]byte{"text": []byte("hello"), "svg": []byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"), "pdf": []byte("%PDF-1.4"), "empty": nil} {
		if _, err := Sniff(data); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
}

func TestDataURLRoundTrips(t *testing.T) {
	data := encode(t, "jpeg")
	url := DataURL("image/jpeg", data)
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatal(url)
	}
	if got, err := Decode(url, int64(len(data))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("%v", err)
	}
}
