package native

import (
	"bytes"
	"encoding/json"
	"io"
)

// A process manifest's key order is visible to the model (process list output
// embeds it) and TypeScript preserves the order keys were first assigned. Go's
// struct layout reproduces the usual order; the one exception is a running
// process recovered as lost without a stop reason, where ended_at is assigned
// before stop_reason. The helpers below express that single exception.

type processManifestPair struct {
	key   string
	value json.RawMessage
}

// marshalProcessManifest encodes a manifest, honouring endedBeforeStop.
func marshalProcessManifest(manifest ProcessManifest) ([]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil || !manifest.endedBeforeStop {
		return encoded, err
	}
	return moveKeyBefore(encoded, "ended_at", "stop_reason"), nil
}

// MarshalJSON encodes a snapshot like its manifest, honouring endedBeforeStop.
func (snapshot ProcessSnapshot) MarshalJSON() ([]byte, error) {
	type plain ProcessSnapshot
	encoded, err := json.Marshal(plain(snapshot))
	if err != nil || !snapshot.endedBeforeStop {
		return encoded, err
	}
	return moveKeyBefore(encoded, "ended_at", "stop_reason"), nil
}

// endedAtPrecedesStopReason reports whether a persisted manifest lists ended_at
// before stop_reason.
func endedAtPrecedesStopReason(data []byte) bool {
	pairs, ok := topLevelPairs(data)
	if !ok {
		return false
	}
	ended, stop := -1, -1
	for index, pair := range pairs {
		switch pair.key {
		case "ended_at":
			ended = index
		case "stop_reason":
			stop = index
		}
	}
	return ended >= 0 && stop >= 0 && ended < stop
}

// moveKeyBefore reorders a JSON object so key precedes before. The input is
// returned unchanged when it cannot be parsed or either key is absent.
func moveKeyBefore(encoded []byte, key, before string) []byte {
	pairs, ok := topLevelPairs(encoded)
	if !ok {
		return encoded
	}
	from, to := -1, -1
	for index, pair := range pairs {
		switch pair.key {
		case key:
			from = index
		case before:
			to = index
		}
	}
	if from < 0 || to < 0 || from < to {
		return encoded
	}
	moved := pairs[from]
	reordered := make([]processManifestPair, 0, len(pairs))
	reordered = append(reordered, pairs[:to]...)
	reordered = append(reordered, moved)
	reordered = append(reordered, pairs[to:from]...)
	reordered = append(reordered, pairs[from+1:]...)
	var out bytes.Buffer
	out.WriteByte('{')
	for index, pair := range reordered {
		if index > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(pair.key)
		out.Write(name)
		out.WriteByte(':')
		out.Write(pair.value)
	}
	out.WriteByte('}')
	return out.Bytes()
}

// topLevelPairs lists an object's keys and raw values in document order.
func topLevelPairs(data []byte) ([]processManifestPair, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	var pairs []processManifestPair
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, isString := token.(string)
		if !isString {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		pairs = append(pairs, processManifestPair{key: key, value: value})
	}
	if _, err := decoder.Token(); err != nil && err != io.EOF {
		return nil, false
	}
	return pairs, true
}
