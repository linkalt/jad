package native

import (
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestNativeToolConfiguredKnowsProfileMessage(t *testing.T) {
	if !nativeToolConfigured(sessionfs.SessionConfig{NativeProfileMessage: &sessionfs.NativeProfileMessageSettings{}}, "profile_message") {
		t.Fatal("profile_message is not seen as configured")
	}
	if nativeToolConfigured(sessionfs.SessionConfig{}, "profile_message") {
		t.Fatal("profile_message is seen as configured on an empty config")
	}
}

// Every name the loader accepts under tools.native has exactly one Native* settings
// field in SessionConfig, and nativeToolConfigured sees that field.
func TestNativeToolConfiguredCoversEveryRegisteredName(t *testing.T) {
	names := sessionfs.NativeToolNames()
	configType := reflect.TypeOf(sessionfs.SessionConfig{})
	var fields []int
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		if strings.HasPrefix(field.Name, "Native") && field.Type.Kind() == reflect.Pointer && strings.HasSuffix(field.Type.Elem().Name(), "Settings") {
			fields = append(fields, index)
		}
	}
	if len(fields) != len(names) {
		t.Fatalf("SessionConfig has %d Native*Settings fields, NativeToolNames has %d names", len(fields), len(names))
	}
	for _, index := range fields {
		config := reflect.New(configType).Elem()
		config.Field(index).Set(reflect.New(configType.Field(index).Type.Elem()))
		var configured []string
		for _, name := range names {
			if nativeToolConfigured(config.Interface().(sessionfs.SessionConfig), name) {
				configured = append(configured, name)
			}
		}
		if len(configured) != 1 {
			t.Errorf("field %s is seen as configured by %v, want exactly one name", configType.Field(index).Name, configured)
		}
	}
}
