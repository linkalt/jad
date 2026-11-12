package app

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// fileStat is what the session version, the transcript generation and the usage
// cache read from one stat call.
type fileStat struct {
	Size        int64
	ModTimeNS   int64
	Dev, Ino    uint64
	HasIdentity bool
	ChangeSec   int64
	ChangeNsec  int64
	HasChange   bool
	// BirthMS is 0 when the filesystem records no creation time.
	BirthMS int64
}

// modMS is the modification time as whole milliseconds, truncated toward zero.
// The TypeScript backend stores Math.trunc(mtimeMs) and rejects a usage cache
// whose timestamps are not safe integers, so both backends must write and
// compare whole milliseconds.
func (s fileStat) modMS() int64 { return s.ModTimeNS / int64(time.Millisecond) }

// changeMS is the inode change time as whole milliseconds, truncated toward zero
// like Math.trunc(ctimeMs) in the TypeScript backend.
func (s fileStat) changeMS() (int64, bool) {
	if !s.HasChange {
		return 0, false
	}
	return s.ChangeSec*1000 + s.ChangeNsec/int64(time.Millisecond), true
}

// generation identifies one incarnation of a file as "device:inode:birth-ms",
// like the TypeScript store: a replaced file changes it even when the
// filesystem reuses its inode.
func (s fileStat) generation() string {
	if !s.HasIdentity {
		return "0:0:0"
	}
	return string(s.appendGeneration(nil))
}

func (s fileStat) appendGeneration(buffer []byte) []byte {
	if !s.HasIdentity {
		return append(buffer, "0:0:0"...)
	}
	buffer = strconv.AppendUint(buffer, s.Dev, 10)
	buffer = append(buffer, ':')
	buffer = strconv.AppendUint(buffer, s.Ino, 10)
	buffer = append(buffer, ':')
	return strconv.AppendInt(buffer, s.BirthMS, 10)
}

// fileStamps is fileStamp of each path, joined; for one path it is fileStamp.
func fileStamps(paths []string) string {
	stamps := make([]string, len(paths))
	for index, path := range paths {
		stamps[index] = fileStamp(path)
	}
	return strings.Join(stamps, "+")
}

// fileStamp changes whenever the file is edited, replaced or removed.
func fileStamp(path string) string {
	stat, err := statFile(path)
	if err != nil {
		return "-"
	}
	buffer := stat.appendGeneration(make([]byte, 0, 96))
	buffer = append(buffer, ':')
	buffer = strconv.AppendInt(buffer, stat.Size, 10)
	buffer = append(buffer, ':')
	buffer = strconv.AppendInt(buffer, stat.ModTimeNS, 10)
	buffer = append(buffer, ':')
	if stat.HasChange {
		buffer = strconv.AppendInt(buffer, stat.ChangeSec, 10)
		buffer = append(buffer, '.')
		buffer = strconv.AppendInt(buffer, stat.ChangeNsec, 10)
	} else {
		buffer = append(buffer, '-')
	}
	return string(buffer)
}

// statFileGeneric reads the fields through os.Stat and the platform's stat
// struct. It serves platforms without a direct stat call and a Linux kernel that
// refuses statx; neither reports a creation time.
func statFileGeneric(path string) (fileStat, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStat{}, err
	}
	stat := fileStat{Size: info.Size(), ModTimeNS: info.ModTime().UnixNano()}
	raw := reflect.ValueOf(info.Sys())
	if raw.Kind() == reflect.Pointer {
		if raw.IsNil() {
			return stat, nil
		}
		raw = raw.Elem()
	}
	if raw.Kind() != reflect.Struct {
		return stat, nil
	}
	dev, devOK := unsignedStatField(raw, "Dev")
	ino, inoOK := unsignedStatField(raw, "Ino")
	stat.Dev, stat.Ino, stat.HasIdentity = dev, ino, devOK && inoOK
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := raw.FieldByName(name)
		if !field.IsValid() || field.Kind() != reflect.Struct {
			continue
		}
		seconds, secondsOK := signedOrUnsignedInt(field.FieldByName("Sec"))
		nanoseconds, nanosOK := signedOrUnsignedInt(field.FieldByName("Nsec"))
		if secondsOK && nanosOK {
			stat.ChangeSec, stat.ChangeNsec, stat.HasChange = seconds, nanoseconds, true
			break
		}
	}
	return stat, nil
}

func unsignedStatField(stat reflect.Value, name string) (uint64, bool) {
	field := stat.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := field.Int()
		if value >= 0 {
			return uint64(value), true
		}
	}
	return 0, false
}

func signedOrUnsignedInt(value reflect.Value) (int64, bool) {
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		unsigned := value.Uint()
		if unsigned <= uint64(maxSafeUsageInteger) {
			return int64(unsigned), true
		}
	}
	return 0, false
}
