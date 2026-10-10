package sessionfs

import "testing"

func TestRequestFileSettingsDefaultsAndValues(t *testing.T) {
	if defaults := Defaults(); defaults.MaxRequestFiles != 4 || defaults.MaxRequestFileBytes != 10*1024*1024 {
		t.Fatalf("defaults = %d files, %d bytes; want 4 and 10485760", defaults.MaxRequestFiles, defaults.MaxRequestFileBytes)
	}
	if got := loadServerSetting(t, `{}`); got.MaxRequestFiles != 4 || got.MaxRequestFileBytes != 10485760 {
		t.Fatalf("unset keys = %d, %d", got.MaxRequestFiles, got.MaxRequestFileBytes)
	}
	if got := loadServerSetting(t, `{"max_request_files":0}`); got.MaxRequestFiles != 0 {
		t.Fatalf("0 turns file input off, got %d", got.MaxRequestFiles)
	}
	if got := loadServerSetting(t, `{"max_request_files":8,"max_request_file_bytes":26214400}`); got.MaxRequestFiles != 8 || got.MaxRequestFileBytes != 26214400 {
		t.Fatalf("upper bounds = %d, %d", got.MaxRequestFiles, got.MaxRequestFileBytes)
	}
	if got := loadServerSetting(t, `{"max_request_files":3,"max_request_file_bytes":1}`); got.MaxRequestFiles != 3 || got.MaxRequestFileBytes != 1 {
		t.Fatalf("explicit = %d, %d", got.MaxRequestFiles, got.MaxRequestFileBytes)
	}
	if got := loadServerSetting(t, `{"max_request_files":null,"max_request_file_bytes":null}`); got.MaxRequestFiles != 4 || got.MaxRequestFileBytes != 10485760 {
		t.Fatalf("null keeps the default, got %d, %d", got.MaxRequestFiles, got.MaxRequestFileBytes)
	}
	// The file keys leave the image keys alone.
	if got := loadServerSetting(t, `{"max_request_files":1}`); got.MaxRequestImages != 8 || got.MaxRequestImageBytes != 5242880 {
		t.Fatalf("image keys changed: %d, %d", got.MaxRequestImages, got.MaxRequestImageBytes)
	}
}
