package native

import (
	"sort"

	"jad/backend-go/internal/sessionfs"
)

// RecordsGroup counts the records of one type that share a value at a field path.
type RecordsGroup struct {
	Value     any
	Canonical string
	Count     int
}

// RecordsGroupResult is a bounded count of matching records by field value.
// Ungrouped counts matching records without the field. AtLeast is true when a
// scan limit stopped the scan, so every count is a lower bound.
type RecordsGroupResult struct {
	Groups    []RecordsGroup
	Ungrouped int
	AtLeast   bool
}

// GroupBy scans the records of recordType that match filter and excludeAny and
// counts them by the value at path. The scan is bounded by the policy's
// MaxScanRecords and MaxScanBytes. Groups are ordered by count, then by value.
func (s *RecordsStore) GroupBy(recordType string, filter map[string]any, excludeAny []map[string]any, path string, policy sessionfs.NativeRecordsSettings) (RecordsGroupResult, error) {
	var result RecordsGroupResult
	err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		names, err := s.files(recordType)
		if err != nil {
			return err
		}
		sort.Strings(names)
		counted := map[string]*RecordsGroup{}
		var scanned, scannedBytes int64
		for _, name := range names {
			if scanned >= policy.MaxScanRecords {
				result.AtLeast = true
				break
			}
			scanned++
			text, readErr := s.readRecordText(recordType, name)
			if readErr != nil {
				continue
			}
			if scannedBytes+int64(len(text)) > policy.MaxScanBytes {
				result.AtLeast = true
				break
			}
			scannedBytes += int64(len(text))
			value, parseErr := parseRecordsJSON(text)
			if parseErr != nil || value.kind != recordsObject {
				continue
			}
			data := recordsGoMap(value)
			if !recordsMatches(data, filter, excludeAny) {
				continue
			}
			fieldValue, found := recordsValueAt(data, path)
			if !found {
				result.Ungrouped++
				continue
			}
			canonical, canonicalErr := recordsJSONCanonical(recordsValueFromGo(fieldValue))
			if canonicalErr != nil {
				result.Ungrouped++
				continue
			}
			group := counted[canonical]
			if group == nil {
				group = &RecordsGroup{Value: fieldValue, Canonical: canonical}
				counted[canonical] = group
			}
			group.Count++
		}
		result.Groups = make([]RecordsGroup, 0, len(counted))
		for _, group := range counted {
			result.Groups = append(result.Groups, *group)
		}
		sort.Slice(result.Groups, func(i, j int) bool {
			if result.Groups[i].Count != result.Groups[j].Count {
				return result.Groups[i].Count > result.Groups[j].Count
			}
			return result.Groups[i].Canonical < result.Groups[j].Canonical
		})
		return nil
	})
	return result, err
}
