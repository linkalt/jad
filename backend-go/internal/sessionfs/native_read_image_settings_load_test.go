package sessionfs

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigReadImageHasItsOwnPathsAndNotReads(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"read":       map[string]any{"paths": map[string]any{"allow": []any{"allowed"}}},
		"read_image": map[string]any{"max_image_bytes": 777, "paths": map[string]any{"allow": []any{"pictures"}, "deny": []any{"private"}}},
	}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	if config.NativeReadImage == nil || config.NativeReadImage.MaxImageBytes != 777 {
		t.Fatalf("native.read_image = %+v", config.NativeReadImage)
	}
	want := []NativePathRule{{Action: "allow", Path: filepath.Join(fixture.Root, "pictures")}, {Action: "deny", Path: filepath.Join(fixture.Root, "private")}}
	if !reflect.DeepEqual(config.NativeReadImage.Policy.Paths, want) {
		t.Fatalf("read_image policy %+v, want its own %+v", config.NativeReadImage.Policy.Paths, want)
	}
	if reflect.DeepEqual(config.NativeRead.Policy.Paths, want) {
		t.Fatalf("read policy %+v follows read_image", config.NativeRead.Policy.Paths)
	}
}

func TestLoadSessionConfigReadImageWithoutPathsDoesNotTakeReadsRules(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"read":       map[string]any{"paths": map[string]any{"allow": []any{"allowed"}}},
		"read_image": map[string]any{},
	}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(config.NativeReadImage.Policy.Paths) != 0 || len(config.NativeRead.Policy.Paths) != 1 {
		t.Fatalf("read_image %+v, read %+v", config.NativeReadImage.Policy.Paths, config.NativeRead.Policy.Paths)
	}
}

func TestLoadSessionConfigReadImageWorksWithoutTheReadTool(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{"read_image": map[string]any{}}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	if config.NativeRead != nil || config.NativeReadImage == nil || config.NativeReadImage.MaxImageBytes != 5*1024*1024 {
		t.Fatalf("read %+v, read_image %+v", config.NativeRead, config.NativeReadImage)
	}
}

func TestLoadSessionConfigReadImageIsOffUnlessConfigured(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{"read": map[string]any{}}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil || config.NativeReadImage != nil {
		t.Fatalf("read_image = %+v, err=%v; want off", config.NativeReadImage, err)
	}
}

func TestLoadSessionConfigRejectsBadReadImageKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		value any
		want  string
	}{
		"unknown key": {map[string]any{"path": "x"}, "tools.native.read_image.path is not supported"},
		"bad paths":   {map[string]any{"paths": []any{"/x"}}, "tools.native.read_image.paths"},
	} {
		t.Run(name, func(t *testing.T) {
			location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{"read_image": tc.value}})
			_, err := LoadSessionConfig(location, "", "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
