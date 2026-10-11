package sessionfs

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigReadImageSharesTheReadPolicyAndOwnsOnlyItsLimit(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"read":       map[string]any{"paths": map[string]any{"allow": []any{"allowed"}, "deny": []any{"private"}}},
		"read_image": map[string]any{"max_image_bytes": 777},
	}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	if config.NativeReadImage == nil || config.NativeReadImage.MaxImageBytes != 777 {
		t.Fatalf("native.read_image = %+v", config.NativeReadImage)
	}
	want := []NativePathRule{{Action: "allow", Path: filepath.Join(fixture.Root, "allowed")}, {Action: "deny", Path: filepath.Join(fixture.Root, "private")}}
	if !reflect.DeepEqual(config.NativeReadImage.Policy.Paths, want) || !reflect.DeepEqual(config.NativeRead.Policy.Paths, want) {
		t.Fatalf("read_image policy %+v and read policy %+v must both be %+v", config.NativeReadImage.Policy.Paths, config.NativeRead.Policy.Paths, want)
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

func TestLoadSessionConfigRejectsAPathKeyOnReadImage(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{"read_image": map[string]any{"paths": []any{"/x"}}}})
	_, err := LoadSessionConfig(location, "", "")
	if err == nil || !strings.Contains(err.Error(), "tools.native.read_image.paths is not supported") || !strings.Contains(err.Error(), "tools.native.read.paths") {
		t.Fatalf("error = %v", err)
	}
}
