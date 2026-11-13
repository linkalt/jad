package sessionfs

import (
	"encoding/json"
	"slices"
)

const (
	defaultMetaUserTimestamp = true
	defaultMetaMaxBytes      = 4096
	minMetaMaxBytes          = 64
	maxMetaMaxBytes          = 65536
)

// PersistenceMeta configures the optional "__agent_meta" data JAD writes into messages.jsonl
// for itself (persistence.meta). It is never sent to a provider; see transcript.MetaField.
type PersistenceMeta struct {
	// UserTimestamp writes "stored_at" (the time JAD stored the record) on every user message.
	UserTimestamp bool
	// MaxBytes caps the encoded "__agent_meta" value of one record, for writing and reading.
	MaxBytes int
}

var persistenceMetaKeys = []string{"user_timestamp", "max_bytes"}

func defaultPersistenceMeta() PersistenceMeta {
	return PersistenceMeta{UserTimestamp: defaultMetaUserTimestamp, MaxBytes: defaultMetaMaxBytes}
}

// normalizePersistenceMeta reads persistence.meta: null or a missing key means the default,
// and an unknown key is refused (it is configuration, so a typo must not pass silently).
func normalizePersistenceMeta(raw json.RawMessage, path string) (PersistenceMeta, error) {
	settings := defaultPersistenceMeta()
	if len(raw) == 0 || string(raw) == "null" {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return PersistenceMeta{}, configError("%s: persistence.meta must be an object", path)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if !slices.Contains(persistenceMetaKeys, key) {
			return PersistenceMeta{}, configError("%s: persistence.meta.%s is not supported; the keys are user_timestamp and max_bytes", path, key)
		}
	}
	if value, ok := fields["user_timestamp"]; ok && string(value) != "null" {
		if err := json.Unmarshal(value, &settings.UserTimestamp); err != nil {
			return PersistenceMeta{}, configError("%s: persistence.meta.user_timestamp must be a boolean", path)
		}
	}
	if value, ok := fields["max_bytes"]; ok && string(value) != "null" {
		configured, valid := configSafeInteger(value)
		if !valid || configured < minMetaMaxBytes || configured > maxMetaMaxBytes {
			return PersistenceMeta{}, configError("%s: persistence.meta.max_bytes must be a safe integer between %d and %d", path, minMetaMaxBytes, maxMetaMaxBytes)
		}
		settings.MaxBytes = int(configured)
	}
	return settings, nil
}
