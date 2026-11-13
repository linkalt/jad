package sessionfs

import (
	"testing"
)

func loadServerSetting(t *testing.T, server string) ServerSettings {
	t.Helper()
	cwd := t.TempDir()
	writeServerSetting(t, cwd, server)
	config, err := loadServerSection(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return config.Server
}

func TestRequestImageSettingsDefaultsAndValues(t *testing.T) {
	if defaults := Defaults(); defaults.MaxRequestImages != 8 || defaults.MaxRequestImageBytes != 5*1024*1024 {
		t.Fatalf("defaults = %d images, %d bytes; want 8 and 5242880", defaults.MaxRequestImages, defaults.MaxRequestImageBytes)
	}
	if got := loadServerSetting(t, `{}`); got.MaxRequestImages != 8 || got.MaxRequestImageBytes != 5242880 {
		t.Fatalf("unset keys = %d, %d", got.MaxRequestImages, got.MaxRequestImageBytes)
	}
	if got := loadServerSetting(t, `{"max_request_images":0}`); got.MaxRequestImages != 0 {
		t.Fatalf("0 turns image input off, got %d", got.MaxRequestImages)
	}
	if got := loadServerSetting(t, `{"max_request_images":16,"max_request_image_bytes":26214400}`); got.MaxRequestImages != 16 || got.MaxRequestImageBytes != 26214400 {
		t.Fatalf("upper bounds = %d, %d", got.MaxRequestImages, got.MaxRequestImageBytes)
	}
	if got := loadServerSetting(t, `{"max_request_images":3,"max_request_image_bytes":1}`); got.MaxRequestImages != 3 || got.MaxRequestImageBytes != 1 {
		t.Fatalf("explicit = %d, %d", got.MaxRequestImages, got.MaxRequestImageBytes)
	}
	if got := loadServerSetting(t, `{"max_request_images":null,"max_request_image_bytes":null}`); got.MaxRequestImages != 8 || got.MaxRequestImageBytes != 5242880 {
		t.Fatalf("null keeps the default, got %d, %d", got.MaxRequestImages, got.MaxRequestImageBytes)
	}
}
