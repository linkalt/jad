package imageresize

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

func testOptions() Options {
	return Options{MaxEdgePx: 100, MaxBytes: 1 << 20, MaxPixels: 4_000_000, JPEGQuality: 85, CacheEntries: 8}
}

// noisy returns an image that does not compress, so its encoded size grows with its area.
func noisy(width, height int, alpha uint8) *image.NRGBA {
	random := rand.New(rand.NewSource(1))
	picture := image.NewNRGBA(image.Rect(0, 0, width, height))
	for index := 0; index < len(picture.Pix); index += 4 {
		picture.Pix[index] = uint8(random.Intn(256))
		picture.Pix[index+1] = uint8(random.Intn(256))
		picture.Pix[index+2] = uint8(random.Intn(256))
		picture.Pix[index+3] = alpha
	}
	return picture
}

func pngURL(t *testing.T, picture image.Image) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func jpegURL(t *testing.T, picture image.Image, exif []byte) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, picture, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	if exif != nil {
		// SOI, then the APP1 segment, then the rest.
		segment := append([]byte{0xff, 0xe1, 0, 0}, exif...)
		binary.BigEndian.PutUint16(segment[2:], uint16(len(exif)+2))
		data = append(append(append([]byte{}, data[:2]...), segment...), data[2:]...)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)
}

func exifWithOrientation(order binary.ByteOrder, value uint16) []byte {
	tiff := make([]byte, 8+2+12+4)
	if order == binary.LittleEndian {
		copy(tiff, "II")
	} else {
		copy(tiff, "MM")
	}
	order.PutUint16(tiff[2:], 42)
	order.PutUint32(tiff[4:], 8)
	order.PutUint16(tiff[8:], 1)
	order.PutUint16(tiff[10:], 0x0112)
	order.PutUint16(tiff[12:], 3)
	order.PutUint32(tiff[14:], 1)
	order.PutUint16(tiff[18:], value)
	return append([]byte("Exif\x00\x00"), tiff...)
}

func decodeURL(t *testing.T, url string) (image.Image, string) {
	t.Helper()
	mime, data, ok := parseDataURL(url)
	if !ok {
		t.Fatalf("not a data URL: %.40s", url)
	}
	picture, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return picture, mime
}

func TestResizeScalesDownToTheLongestEdge(t *testing.T) {
	url := pngURL(t, noisy(400, 200, 255))
	got, err := resizeDataURL(url, testOptions())
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	picture, _ := decodeURL(t, got)
	if bounds := picture.Bounds(); bounds.Dx() != 100 || bounds.Dy() != 50 {
		t.Fatalf("size = %dx%d, want 100x50 (aspect kept)", bounds.Dx(), bounds.Dy())
	}
	if len(got) >= len(url) {
		t.Fatalf("result %d bytes is not smaller than the original %d", len(got), len(url))
	}
}

func TestResizeLeavesAnImageThatAlreadyFits(t *testing.T) {
	if _, err := resizeDataURL(pngURL(t, noisy(60, 40, 255)), testOptions()); err != errUnchanged {
		t.Fatalf("err = %v, want errUnchanged", err)
	}
}

func TestResizeKeepsTransparencyAsPNG(t *testing.T) {
	got, err := resizeDataURL(pngURL(t, noisy(300, 300, 128)), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	picture, mime := decodeURL(t, got)
	if mime != "image/png" {
		t.Fatalf("mime = %s, want image/png for an image with alpha", mime)
	}
	if _, _, _, alpha := picture.At(10, 10).RGBA(); alpha == 0xffff {
		t.Fatalf("alpha was flattened away")
	}
}

func TestResizeUsesJPEGWhenAnOpaquePNGDoesNotFit(t *testing.T) {
	options := testOptions()
	options.MaxEdgePx = 400
	options.MaxBytes = 60 * 1024
	url := pngURL(t, noisy(400, 400, 255)) // about 640 KB of noise as png
	got, err := resizeDataURL(url, options)
	if err != nil {
		t.Fatal(err)
	}
	_, mime := decodeURL(t, got)
	if mime != "image/jpeg" {
		t.Fatalf("mime = %s, want image/jpeg", mime)
	}
	if _, data, _ := parseDataURL(got); len(data) > options.MaxBytes {
		t.Fatalf("result is %d bytes, over max_bytes %d", len(data), options.MaxBytes)
	}
}

func TestResizeShrinksFurtherWhenQualityAloneIsNotEnough(t *testing.T) {
	options := testOptions()
	options.MaxEdgePx = 400
	options.MaxBytes = 8 * 1024
	got, err := resizeDataURL(pngURL(t, noisy(400, 400, 255)), options)
	if err != nil {
		t.Fatal(err)
	}
	picture, _ := decodeURL(t, got)
	if picture.Bounds().Dx() >= 400 {
		t.Fatalf("width = %d, want it reduced below 400", picture.Bounds().Dx())
	}
	if _, data, _ := parseDataURL(got); len(data) > options.MaxBytes {
		t.Fatalf("result is %d bytes, over max_bytes %d", len(data), options.MaxBytes)
	}
}

func TestResizeGivesUpWhenNothingFits(t *testing.T) {
	options := testOptions()
	options.MaxEdgePx = 400
	options.MaxBytes = 100 // not reachable for a noisy image even after 4 shrinks
	if _, err := resizeDataURL(pngURL(t, noisy(400, 400, 255)), options); err != errUnchanged {
		t.Fatalf("err = %v, want errUnchanged: the original is kept, never a broken image", err)
	}
}

func TestResizeIgnoresWhatItCannotRead(t *testing.T) {
	animated := &bytes.Buffer{}
	if err := gif.EncodeAll(animated, &gif.GIF{Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 400, 400), color.Palette{color.Black, color.White})}, Delay: []int{1}}); err != nil {
		t.Fatal(err)
	}
	tooManyPixels := testOptions()
	tooManyPixels.MaxPixels = 10_000
	cases := map[string]struct {
		url     string
		options Options
	}{
		"http url":        {"https://example.com/a.png", testOptions()},
		"file id":         {"file-abc", testOptions()},
		"not base64":      {"data:image/png,abc", testOptions()},
		"bad base64":      {"data:image/png;base64,@@@@", testOptions()},
		"corrupt png":     {"data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image at all")), testOptions()},
		"gif":             {"data:image/gif;base64," + base64.StdEncoding.EncodeToString(animated.Bytes()), testOptions()},
		"svg":             {"data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>")), testOptions()},
		"over max_pixels": {pngURL(t, noisy(200, 200, 255)), tooManyPixels},
		"empty":           {"", testOptions()},
	}
	for name, test := range cases {
		if _, err := resizeDataURL(test.url, test.options); err != errUnchanged {
			t.Errorf("%s: err = %v, want errUnchanged", name, err)
		}
	}
}

func TestResizeTrustsTheBytesNotTheDeclaredMIME(t *testing.T) {
	// A png declared as jpeg still decodes by its content.
	url := strings.Replace(pngURL(t, noisy(300, 300, 255)), "image/png", "image/jpeg", 1)
	got, err := resizeDataURL(url, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if picture, _ := decodeURL(t, got); picture.Bounds().Dx() != 100 {
		t.Fatalf("width = %d, want 100", picture.Bounds().Dx())
	}
}

func TestJPEGOrientationIsReadFromEXIF(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for value := uint16(1); value <= 8; value++ {
			_, data, _ := parseDataURL(jpegURL(t, noisy(8, 8, 255), exifWithOrientation(order, value)))
			if got := jpegOrientation(data); got != int(value) {
				t.Errorf("%v value %d: orientation = %d", order, value, got)
			}
		}
	}
	for name, exif := range map[string][]byte{
		"none":         nil,
		"out of range": exifWithOrientation(binary.BigEndian, 9),
		"zero":         exifWithOrientation(binary.BigEndian, 0),
		"truncated":    exifWithOrientation(binary.BigEndian, 6)[:12],
	} {
		_, data, _ := parseDataURL(jpegURL(t, noisy(8, 8, 255), exif))
		if got := jpegOrientation(data); got != 1 {
			t.Errorf("%s: orientation = %d, want 1", name, got)
		}
	}
	if got := jpegOrientation([]byte("not a jpeg")); got != 1 {
		t.Errorf("not a jpeg: orientation = %d, want 1", got)
	}
}

func TestResizeRotatesATurnedPhotoUpright(t *testing.T) {
	// 40 wide, 20 high, red left half and blue right half. Orientation 6 means "rotate 90
	// degrees clockwise to display": the result is 20 wide, 40 high, red on top.
	picture := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			if x < 20 {
				picture.Set(x, y, color.NRGBA{R: 255, A: 255})
			} else {
				picture.Set(x, y, color.NRGBA{B: 255, A: 255})
			}
		}
	}
	url := jpegURL(t, picture, exifWithOrientation(binary.BigEndian, 6))
	got, err := resizeDataURL(url, testOptions())
	if err != nil {
		t.Fatalf("a turned jpeg that fits must still be rewritten: %v", err)
	}
	rotated, _ := decodeURL(t, got)
	if bounds := rotated.Bounds(); bounds.Dx() != 20 || bounds.Dy() != 40 {
		t.Fatalf("size = %dx%d, want 20x40", bounds.Dx(), bounds.Dy())
	}
	top, _, _, _ := rotated.At(10, 5).RGBA()
	_, _, bottomBlue, _ := rotated.At(10, 35).RGBA()
	if top>>8 < 200 || bottomBlue>>8 < 200 {
		t.Fatalf("red should be on top and blue below (top red %d, bottom blue %d)", top>>8, bottomBlue>>8)
	}
}

func TestOrientMapsEveryCase(t *testing.T) {
	// A 3x2 image with six distinct colours, read row by row as the letters A..F:
	//   A B C
	//   D E F
	// The expected result of each EXIF orientation, written out by hand from the EXIF
	// specification (rows separated by "/"): 5 is a transpose, 6 a quarter turn clockwise,
	// 7 a transverse flip, 8 a quarter turn counter-clockwise.
	colours := map[byte]color.NRGBA{}
	for index, letter := range []byte("ABCDEF") {
		colours[letter] = color.NRGBA{R: uint8(40 * (index + 1)), G: uint8(255 - 30*index), B: uint8(7 * index), A: 255}
	}
	source := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for index, letter := range []byte("ABCDEF") {
		source.Set(index%3, index/3, colours[letter])
	}
	want := map[int]string{
		1: "ABC/DEF", 2: "CBA/FED", 3: "FED/CBA", 4: "DEF/ABC",
		5: "AD/BE/CF", 6: "DA/EB/FC", 7: "FC/EB/DA", 8: "CF/BE/AD",
	}
	for orientation := 1; orientation <= 8; orientation++ {
		turned := orient(source, orientation)
		bounds := turned.Bounds()
		var rows []string
		for y := 0; y < bounds.Dy(); y++ {
			var row []byte
			for x := 0; x < bounds.Dx(); x++ {
				found := byte('?')
				for letter, colour := range colours {
					if turned.At(x, y) == colour || colorEqual(turned.At(x, y), colour) {
						found = letter
					}
				}
				row = append(row, found)
			}
			rows = append(rows, string(row))
		}
		if got := strings.Join(rows, "/"); got != want[orientation] {
			t.Errorf("orientation %d = %s, want %s", orientation, got, want[orientation])
		}
	}
	if orient(source, 0) != image.Image(source) || orient(source, 9) != image.Image(source) {
		t.Errorf("an unknown orientation must return the image itself")
	}
}

func colorEqual(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

func TestEXIFWithAWrongTIFFMagicIsIgnored(t *testing.T) {
	exif := exifWithOrientation(binary.BigEndian, 6)
	binary.BigEndian.PutUint16(exif[6+2:], 43) // the "42" after the byte-order mark
	_, data, _ := parseDataURL(jpegURL(t, noisy(8, 8, 255), exif))
	if got := jpegOrientation(data); got != 1 {
		t.Fatalf("orientation = %d, want 1 for a TIFF header whose magic is not 42", got)
	}
}

func TestPixelsOutsideTheOriginAreRead(t *testing.T) {
	// A sub-image has bounds that do not start at 0,0.
	full := noisy(300, 300, 255)
	sub := full.SubImage(image.Rect(100, 100, 250, 250))
	got, err := resizeDataURL(pngURL(t, sub), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if picture, _ := decodeURL(t, got); picture.Bounds().Dx() != 100 {
		t.Fatalf("width = %d", picture.Bounds().Dx())
	}
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

// flatScreenshot is a flat-colour image with a few lines: it compresses very well as a png
// and very badly once scaling has blended its edges.
func flatScreenshot(width, height int) *image.NRGBA {
	picture := image.NewNRGBA(image.Rect(0, 0, width, height))
	random := rand.New(rand.NewSource(7))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := color.NRGBA{R: uint8(x * 255 / width), G: uint8(y * 255 / height), B: 200, A: 255}
			if y%40 < 3 && x%7 < 5 {
				c = color.NRGBA{A: 255}
			}
			if random.Intn(400) == 0 {
				c = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
			}
			picture.SetNRGBA(x, y, c)
		}
	}
	return picture
}

func TestResizePrefersAJPEGOverAScaledPNGThatGrewPastTheOriginal(t *testing.T) {
	options := testOptions()
	options.MaxEdgePx = 300
	options.MaxBytes = 4 << 20
	url := pngURL(t, flatScreenshot(900, 600))
	got, err := resizeDataURL(url, options)
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if len(got) > len(url) {
		t.Fatalf("the resized image is %d bytes, larger than the %d byte original although a smaller jpeg was possible", len(got), len(url))
	}
	_, mime := decodeURL(t, got)
	if mime != "image/jpeg" {
		t.Fatalf("mime = %s, want image/jpeg once the scaled png is larger than the original", mime)
	}
}

func TestResizeKeepsAPNGWhenTheScaledPNGIsNotLarger(t *testing.T) {
	// Pure noise has no flat areas to lose, so the scaled png is smaller than the original.
	got, err := resizeDataURL(pngURL(t, noisy(400, 400, 255)), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, mime := decodeURL(t, got); mime != "image/png" {
		t.Fatalf("mime = %s, want image/png when it is smaller than the original", mime)
	}
}
