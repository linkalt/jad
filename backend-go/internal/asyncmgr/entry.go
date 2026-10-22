package asyncmgr

import "encoding/json"

// envelopeBytes is reserved in every response for the fields around the entries.
const envelopeBytes = 256

func entryOf(rec Record, summaryLimit int) Entry {
	e := Entry{ID: rec.ID, Kind: rec.Kind, State: rec.State, OK: rec.OK, Code: rec.Code, Reason: rec.Reason,
		Label: rec.Label, Cut: rec.Cut, Bytes: rec.Bytes, Ref: rec.Ref}
	e.Summary, e.Cut = cutUTF8(rec.Summary, summaryLimit)
	e.Cut = e.Cut || rec.Cut
	if rec.State.Terminal() {
		e.T = formatTime(rec.EndedAt)
	} else {
		e.T = formatTime(rec.StartedAt)
	}
	return e
}

func encodedSize(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

// fitEntry shortens only the summary of an entry that alone exceeds budget, never the
// id and state fields. A summary that cannot be made to fit is dropped.
func fitEntry(e Entry, budget int) Entry {
	for encodedSize(e) > budget && e.Summary != "" {
		excess := encodedSize(e) - budget
		keep := len(e.Summary) - excess - 1
		if keep < 0 {
			keep = 0
		}
		if keep >= len(e.Summary) {
			keep = len(e.Summary) - 1
		}
		e.Summary, _ = cutUTF8(e.Summary, keep)
		e.Cut = true
	}
	return e
}
