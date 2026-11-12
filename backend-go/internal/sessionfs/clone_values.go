package sessionfs

func cloneNativePathPolicy(policy NativePathPolicy) NativePathPolicy {
	return NativePathPolicy{
		Paths:             append([]NativePathRule(nil), policy.Paths...),
		UnrestrictedPaths: policy.UnrestrictedPaths,
		Within:            append([]string(nil), policy.Within...),
		Hidden:            append([]string(nil), policy.Hidden...),
		HiddenExcept:      append([]string(nil), policy.HiddenExcept...),
	}
}

func stringPointer(value string) *string { return &value }

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
