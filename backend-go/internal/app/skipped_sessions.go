package app

import (
	"log/slog"
	"sync"
)

// skippedSessions remembers why each invalid session was left out of the list.
// The frontend polls the list every few seconds, so logging every skip would
// repeat one line forever; a skip is logged only when its reason is new or has
// changed, and a session that becomes valid again is forgotten.
type skippedSessions struct {
	mu      sync.Mutex
	reasons map[string]string
}

// report logs that reference was skipped for reason, unless that exact reason
// was already reported for it.
func (s *skippedSessions) report(logger *slog.Logger, reference, reason string) {
	s.mu.Lock()
	if s.reasons == nil {
		s.reasons = make(map[string]string)
	}
	previous, seen := s.reasons[reference]
	s.reasons[reference] = reason
	s.mu.Unlock()
	if seen && previous == reason {
		return
	}
	logger.Warn("skipping invalid session", slog.String("session", reference), slog.String("reason", reason))
}

// resolved forgets reference after it loaded again, so a later failure is
// reported afresh.
func (s *skippedSessions) resolved(reference string) {
	s.mu.Lock()
	delete(s.reasons, reference)
	s.mu.Unlock()
}

// warnedConfigs remembers the configuration warnings the session list already logged for each
// session: a profile or model file left out of a session is warned about once, not on every
// poll of the list. A warning that goes away and comes back is logged again.
type warnedConfigs struct {
	mu   sync.Mutex
	seen map[string]map[string]struct{}
}

// report logs the warnings of reference that its previous report did not carry.
func (w *warnedConfigs) report(logger *slog.Logger, reference, profile string, warnings []string) {
	if logger == nil {
		return
	}
	current := make(map[string]struct{}, len(warnings))
	for _, warning := range warnings {
		current[warning] = struct{}{}
	}
	w.mu.Lock()
	if w.seen == nil {
		w.seen = make(map[string]map[string]struct{})
	}
	previous := w.seen[reference]
	if len(current) == 0 {
		delete(w.seen, reference)
	} else {
		w.seen[reference] = current
	}
	w.mu.Unlock()
	for _, warning := range warnings {
		if _, logged := previous[warning]; !logged {
			logger.Warn(warning, "session", reference, "profile", profile)
		}
	}
}
