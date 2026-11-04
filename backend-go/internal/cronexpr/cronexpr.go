// Package cronexpr parses five-field cron expressions (minute hour day-of-month month
// day-of-week) and finds the next time one matches. It knows the forms of Vixie cron: *,
// numbers, ranges a-b, steps */n and a-b/n, comma lists, 7 as Sunday, and the macros
// @hourly, @daily (@midnight), @weekly, @monthly and @yearly (@annually). When both the
// day of the month and the day of the week are restricted, a day matching either one
// matches, as in cron. Names of months and days are not accepted.
package cronexpr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression. Each field is a bit set of the values it matches.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	// domAny and dowAny are true when the field was *, which matters for the OR rule.
	domAny, dowAny bool
}

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

type field struct {
	name     string
	min, max int
}

var fields = [5]field{{"minute", 0, 59}, {"hour", 0, 23}, {"day of month", 1, 31}, {"month", 1, 12}, {"day of week", 0, 7}}

// Parse reads an expression; the error names the field that is wrong.
func Parse(expression string) (*Schedule, error) {
	text := strings.TrimSpace(expression)
	if expanded, ok := macros[strings.ToLower(text)]; ok {
		text = expanded
	} else if strings.HasPrefix(text, "@") {
		return nil, fmt.Errorf("unknown macro %q (known: @hourly, @daily, @midnight, @weekly, @monthly, @yearly, @annually)", text)
	}
	parts := strings.Fields(text)
	if len(parts) != 5 {
		return nil, fmt.Errorf("want 5 fields (minute hour day-of-month month day-of-week), got %d", len(parts))
	}
	var sets [5]uint64
	for index, part := range parts {
		set, err := parseField(part, fields[index])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fields[index].name, err)
		}
		sets[index] = set
	}
	// 7 is Sunday as well as 0.
	if sets[4]&(1<<7) != 0 {
		sets[4] = (sets[4] | 1) &^ (1 << 7)
	}
	return &Schedule{minute: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4], domAny: parts[2] == "*", dowAny: parts[4] == "*"}, nil
}

func parseField(text string, f field) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(text, ",") {
		if item == "" {
			return 0, fmt.Errorf("empty list item in %q", text)
		}
		rangePart, stepPart, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			parsed, err := strconv.Atoi(stepPart)
			if err != nil || parsed < 1 {
				return 0, fmt.Errorf("step %q must be a positive number", stepPart)
			}
			step = parsed
		}
		low, high := f.min, f.max
		switch {
		case rangePart == "*":
		case strings.Contains(rangePart, "-"):
			a, b, _ := strings.Cut(rangePart, "-")
			var err error
			if low, err = number(a, f); err != nil {
				return 0, err
			}
			if high, err = number(b, f); err != nil {
				return 0, err
			}
			if low > high {
				return 0, fmt.Errorf("range %q runs backwards", rangePart)
			}
		default:
			value, err := number(rangePart, f)
			if err != nil {
				return 0, err
			}
			low = value
			if hasStep {
				// "a/n" is a..max step n, as in cron.
				high = f.max
			} else {
				high = value
			}
		}
		for value := low; value <= high; value += step {
			set |= 1 << uint(value)
		}
	}
	return set, nil
}

func number(text string, f field) (int, error) {
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	if value < f.min || value > f.max {
		return 0, fmt.Errorf("%d is outside %d-%d", value, f.min, f.max)
	}
	return value, nil
}

// maxSearchYears bounds Next: an expression such as "0 0 30 2 *" never matches.
const maxSearchYears = 5

// Next is the first time after after, in after's location, that the schedule matches, with
// seconds zero; the zero time when none lies within five years.
func (s *Schedule) Next(after time.Time) time.Time {
	location := after.Location()
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(maxSearchYears, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, location)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, location)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, location)
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domAny || s.dowAny {
		return dom && dow
	}
	return dom || dow
}
