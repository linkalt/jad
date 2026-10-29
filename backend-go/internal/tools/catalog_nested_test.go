package tools

import (
	"reflect"
	"strconv"
	"testing"
)

// filledExecutionContext returns an ExecutionContext whose every field is set to a
// distinct non-zero value, so a copy that drops one is visible.
func filledExecutionContext(t *testing.T) ExecutionContext {
	t.Helper()
	var execution ExecutionContext
	value := reflect.ValueOf(&execution).Elem()
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		name := value.Type().Field(index).Name
		switch field.Kind() {
		case reflect.String:
			field.SetString(name + "-value")
		case reflect.Bool:
			field.SetBool(true)
		case reflect.Int, reflect.Int64:
			field.SetInt(int64(index) + 1)
		case reflect.Slice:
			field.SetBytes([]byte(`{"filled":true}`))
		case reflect.Pointer:
			field.Set(reflect.New(field.Type().Elem()))
		case reflect.Func:
			function := field.Type()
			field.Set(reflect.MakeFunc(function, func([]reflect.Value) []reflect.Value {
				results := make([]reflect.Value, function.NumOut())
				for result := range results {
					results[result] = reflect.Zero(function.Out(result))
				}
				return results
			}))
		default:
			t.Fatalf("%s: a new ExecutionContext field of kind %s needs a filler in this test", name, field.Kind())
		}
	}
	return execution
}

// sameFieldValue compares one field of two ExecutionContext values. Functions are compared
// by presence: they are not comparable values.
func sameFieldValue(got, want reflect.Value) bool {
	if got.Kind() == reflect.Func {
		return got.IsNil() == want.IsNil()
	}
	return reflect.DeepEqual(got.Interface(), want.Interface())
}

// TestForNestedKeepsEveryRunScopedField fails when a field of ExecutionContext is neither
// carried over by ForNested nor named in executionContextPerCall: a nested call must not
// silently lose the run it belongs to, which is how the tool_smith bridge lost AgentDepth,
// RootRun and RemoteHops.
func TestForNestedKeepsEveryRunScopedField(t *testing.T) {
	parent := filledExecutionContext(t)
	nested := parent.ForNested()
	value := reflect.ValueOf(nested)
	parentValue := reflect.ValueOf(parent)
	perCall := make(map[string]bool, len(executionContextPerCall))
	for _, name := range executionContextPerCall {
		perCall[name] = true
	}
	if len(perCall) != len(executionContextPerCall) {
		t.Fatalf("executionContextPerCall names a field twice: %v", executionContextPerCall)
	}
	cleared := 0
	for index := 0; index < value.NumField(); index++ {
		name := value.Type().Field(index).Name
		got := value.Field(index)
		if perCall[name] {
			cleared++
			if !got.IsZero() {
				t.Errorf("%s is per call and must be cleared by ForNested, got %v", name, got)
			}
			continue
		}
		if want := parentValue.Field(index); !sameFieldValue(got, want) {
			t.Errorf("%s must carry over to a nested call: got %v, want %v", name, got, want)
		}
	}
	if cleared != len(perCall) {
		t.Errorf("executionContextPerCall names %d fields, %d exist on ExecutionContext: %v",
			len(perCall), cleared, executionContextPerCall)
	}
}

// TestForNestedKeepsTheThreeRunFields pins the fields the bridge dropped, so the check
// above cannot pass by listing them as per call.
func TestForNestedKeepsTheThreeRunFields(t *testing.T) {
	parent := ExecutionContext{AgentDepth: 2, RootRun: "sub_00000001", RemoteHops: 3, ProcessOwner: "sub_00000001"}
	nested := parent.ForNested()
	if nested.AgentDepth != parent.AgentDepth || nested.RootRun != parent.RootRun || nested.RemoteHops != parent.RemoteHops {
		t.Fatalf("nested context lost the run: depth=%s root=%s hops=%d",
			strconv.Itoa(nested.AgentDepth), nested.RootRun, nested.RemoteHops)
	}
	if nested.ProcessOwner != parent.ProcessOwner {
		t.Fatalf("nested context lost the owner: %q", nested.ProcessOwner)
	}
}
