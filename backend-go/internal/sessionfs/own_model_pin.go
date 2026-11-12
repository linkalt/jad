package sessionfs

import "encoding/json"

// ownModelPin is one of a profile's own models as a run read it when it started: the own
// file composed with its base (ownModelFields), or why that failed.
type ownModelPin struct {
	fields map[string]json.RawMessage
	err    error
}

// PinOwnModels is c with each of the selected profile's own models read once, for a run that
// starts now (decider 3c.2b). Its model and its model_fallback models are composed with their
// bases, while the profile file's lock is held, so the copy is of one revision of the own files:
// an edit publishes under that lock and is never seen halfway. Every later model load of the
// run and of what it starts (WithModel: the fallback plan, the hooks' key variables, a sub-agent
// that keeps the run's model) uses this copy and reads neither the own file nor its base again;
// an edit applies from the next run. An own model the run could not reach when it started fails
// closed, and is never read from the disk during the run. Shared models are still read from their
// files as before, and a config with no own model (v1 and every shared-only profile) is returned
// as it is. A pinned config pins nothing again.
func (c SessionConfig) PinOwnModels() SessionConfig {
	if len(c.ModelBases) == 0 {
		return c
	}
	reached := map[string]bool{c.ModelPath: true}
	if c.ModelFallback != nil {
		for _, entry := range c.ModelFallback.Models {
			reached[entry.ModelPath] = true
		}
	}
	if c.ProfilePath != "" {
		defer waitProfileFile(c.ProfilePath)()
	}
	pinned := make(map[string]ModelBase, len(c.ModelBases))
	for path, base := range c.ModelBases {
		// A pinned model's fields come from its pin (ownModelFields), so pinning again reads nothing.
		pin := &ownModelPin{}
		if reached[path] {
			pin.fields, pin.err = ownModelFields(path, base)
		} else {
			pin.err = configError("%s: this run did not read this own model when it started, so it does not use it; a run that starts later can", path)
		}
		base.pin = pin
		pinned[path] = base
	}
	c.ModelBases = pinned
	return c
}
