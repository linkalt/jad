package imageresize

import (
	"encoding/binary"
	"image"
)

// jpegOrientation reads the EXIF orientation (1 to 8) of a jpeg, or 1 when there is none.
// The go jpeg decoder ignores EXIF, and re-encoding drops it, so a turned photo would
// reach the provider sideways unless the rotation is applied to the pixels.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	position := 2
	for position+4 <= len(data) {
		if data[position] != 0xff {
			return 1
		}
		marker := data[position+1]
		if marker == 0xda || marker == 0xd9 {
			return 1
		}
		length := int(binary.BigEndian.Uint16(data[position+2:]))
		if length < 2 || position+2+length > len(data) {
			return 1
		}
		segment := data[position+4 : position+2+length]
		if marker == 0xe1 && len(segment) > 6 && string(segment[:6]) == "Exif\x00\x00" {
			return exifOrientation(segment[6:])
		}
		position += 2 + length
	}
	return 1
}

func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:]) != 42 {
		return 1
	}
	offset := int(order.Uint32(tiff[4:]))
	if offset < 8 || offset+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[offset:]))
	for index := 0; index < count; index++ {
		entry := offset + 2 + index*12
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:]) == 0x0112 {
			value := int(order.Uint16(tiff[entry+8:]))
			if value >= 1 && value <= 8 {
				return value
			}
			return 1
		}
	}
	return 1
}

// orient applies an EXIF orientation so the returned image is upright.
func orient(source image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return source
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	outWidth, outHeight := width, height
	if orientation >= 5 {
		outWidth, outHeight = height, width
	}
	target := image.NewNRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var tx, ty int
			switch orientation {
			case 2:
				tx, ty = width-1-x, y
			case 3:
				tx, ty = width-1-x, height-1-y
			case 4:
				tx, ty = x, height-1-y
			case 5:
				tx, ty = y, x
			case 6:
				tx, ty = height-1-y, x
			case 7:
				tx, ty = height-1-y, width-1-x
			case 8:
				tx, ty = y, width-1-x
			}
			target.Set(tx, ty, source.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return target
}
