package tools

import (
	"reflect"
	"testing"
)

// callerContextRenamed maps a CallerContext field to the ExecutionContext field it is built
// from when the names differ (the design's names, section 3.3). Every other mirrored field has
// the same name in both.
var callerContextRenamed = map[string]string{
	"Workspace":   "WorkspaceRoot",
	"Depth":       "AgentDepth",
	"HandleOwner": "ProcessOwner",
}

// callerContextCallSite names the CallerContext fields that ExecutionContext has no
// counterpart for: the tool that starts the run fills them.
var callerContextCallSite = map[string]bool{"RemoteCaller": true, "ModelChoice": true, "SandboxChoice": true}

// sameCallerValue compares a mirrored field: a pointer by identity (the caller's own scope and
// environment, not a copy), a function by presence (functions are not comparable), and any
// other value by equality.
func sameCallerValue(got, want reflect.Value) bool {
	switch got.Kind() {
	case reflect.Func:
		return got.IsNil() == want.IsNil()
	case reflect.Pointer:
		return got.Pointer() == want.Pointer()
	}
	return reflect.DeepEqual(got.Interface(), want.Interface())
}

// TestCallerContextCarriesEveryRunScopedField fails when a run-scoped ExecutionContext field
// (one ForNested keeps) has no CallerContext counterpart, when Caller drops or changes one,
// when a per-call field leaks into CallerContext, or when a CallerContext field is neither
// mirrored nor named as filled by the call site.
func TestCallerContextCarriesEveryRunScopedField(t *testing.T) {
	execution := filledExecutionContext(t)
	caller := execution.Caller()
	source := reflect.ValueOf(execution)
	callerValue := reflect.ValueOf(caller)
	perCall := make(map[string]bool, len(executionContextPerCall))
	for _, name := range executionContextPerCall {
		perCall[name] = true
	}
	mirrored := map[string]bool{}
	for index := 0; index < callerValue.NumField(); index++ {
		name := callerValue.Type().Field(index).Name
		got := callerValue.Field(index)
		if callerContextCallSite[name] {
			if !got.IsZero() {
				t.Errorf("%s is filled by the call site, but Caller set it to %v", name, got)
			}
			continue
		}
		from := name
		if renamed, ok := callerContextRenamed[name]; ok {
			from = renamed
		}
		want := source.FieldByName(from)
		if !want.IsValid() {
			t.Errorf("CallerContext.%s mirrors no ExecutionContext field; name it in callerContextCallSite or callerContextRenamed", name)
			continue
		}
		if perCall[from] {
			t.Errorf("CallerContext.%s mirrors %s, which is per call", name, from)
		}
		if mirrored[from] {
			t.Errorf("ExecutionContext.%s is mirrored twice", from)
		}
		mirrored[from] = true
		if got.Type() != want.Type() {
			t.Errorf("CallerContext.%s is %s, ExecutionContext.%s is %s", name, got.Type(), from, want.Type())
			continue
		}
		if !sameCallerValue(got, want) {
			t.Errorf("CallerContext.%s = %v, want ExecutionContext.%s = %v", name, got, from, want)
		}
	}
	for index := 0; index < source.NumField(); index++ {
		name := source.Type().Field(index).Name
		if !perCall[name] && !mirrored[name] {
			t.Errorf("ExecutionContext.%s is run-scoped (ForNested keeps it) but CallerContext has no field for it", name)
		}
	}
}

// The caller context of a nested call is the context of the call made before ForNested: the
// run scope is the same, only the per-call identity differs, and CallerContext has none.
func TestCallerContextIsTheSameBeforeAndAfterForNested(t *testing.T) {
	execution := filledExecutionContext(t)
	before, after := reflect.ValueOf(execution.Caller()), reflect.ValueOf(execution.ForNested().Caller())
	for index := 0; index < before.NumField(); index++ {
		if !sameCallerValue(after.Field(index), before.Field(index)) {
			t.Errorf("%s differs after ForNested", before.Type().Field(index).Name)
		}
	}
}
