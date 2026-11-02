package asyncmgr

import "encoding/json"

// envelopeBytes is reserved in every response for the fields around the entries.
const envelopeBytes = 256

// shortSummaryBytes is the summary kept beside a detail in a record.
const shortSummaryBytes = 96

func entryOf(rec Record, summaryLimit, detailLimit int) Entry {
	e := Entry{ID: rec.ID, Kind: rec.Kind, State: rec.State, OK: rec.OK, Code: rec.Code, Reason: rec.Reason,
		Label: rec.Label, Cut: rec.Cut, Bytes: rec.Bytes, Ref: rec.Ref, ResumedFrom: rec.ResumedFrom, ConfigNote: rec.ConfigNote}
	e.Summary, e.Cut = cutUTF8(rec.Summary, summaryLimit)
	e.Cut = e.Cut || rec.Cut
	if rec.Detail != "" {
		// The record's result text is what the model gets; the summary is only a short form of it.
		e.Summary, e.Cut = cutUTF8(rec.Detail, detailLimit)
		e.Cut = e.Cut || rec.DetailCut || rec.SrcCut
	}
	if rec.State.Terminal() {
		e.T = formatTime(rec.EndedAt)
	} else {
		e.T = formatTime(rec.StartedAt)
	}
	return e
}

// detailLimit is the most result text a record keeps and an entry delivers: what is left of
// a response after the envelope and the fixed fields of one entry, the same for wait and status.
func (m *Manager) detailLimit() int {
	return int(m.cfg.ResponseMaxBytes) - envelopeBytes - EntryOverheadBytes
}

func encodedSize(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

// fitEntry makes an entry that alone exceeds budget fit it. The resume fields go first, the
// config note before the resumed_from reference, since they are informational; then only the
// summary shrinks, never the id and state fields. A summary that cannot be made to fit is dropped.
func fitEntry(e Entry, budget int) Entry {
	if encodedSize(e) > budget {
		e.ConfigNote = ""
	}
	if encodedSize(e) > budget {
		e.ResumedFrom = ""
	}
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
