package clock

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Recurrence kinds. none fires once, on its day; yearly and monthly re-fire on
// the same month/day or day-of-month; every_n_days re-fires on a fixed cycle.
const (
	RecurNone       = "none"
	RecurYearly     = "yearly"
	RecurMonthly    = "monthly"
	RecurEveryNDays = "every_n_days"
)

// Entry is the pure half of a scheduled_events row: everything Due needs and
// nothing it does not. The store maps its rows onto this; Due never sees a
// database.
type Entry struct {
	ID         string
	Day        int64
	Recurrence string // none | yearly | monthly | every_n_days
	EveryN     int64  // the N of every_n_days; 0 otherwise
}

// ParseRecurrence decodes the wire/storage form of a schedule entry's
// recurrence: "none", "yearly", "monthly", or "every_n_days:<N>" with N ≥ 1.
func ParseRecurrence(s string) (kind string, n int64, err error) {
	s = strings.TrimSpace(s)
	kind, arg, hasArg := strings.Cut(s, ":")
	switch kind {
	case RecurNone, RecurYearly, RecurMonthly:
		if hasArg {
			return "", 0, fmt.Errorf("%w: recurrence %q takes no argument", ErrInvalid, s)
		}
		return kind, 0, nil
	case RecurEveryNDays:
		if !hasArg {
			return "", 0, fmt.Errorf("%w: recurrence every_n_days needs :N", ErrInvalid)
		}
		n, err := strconv.ParseInt(arg, 10, 64)
		if err != nil || n < 1 {
			return "", 0, fmt.Errorf("%w: recurrence every_n_days:%q", ErrInvalid, arg)
		}
		return kind, n, nil
	default:
		return "", 0, fmt.Errorf("%w: recurrence %q", ErrInvalid, s)
	}
}

// FormatRecurrence is ParseRecurrence's inverse: the storage form of a
// recurrence kind and its N.
func FormatRecurrence(kind string, n int64) string {
	if kind == RecurEveryNDays {
		return fmt.Sprintf("%s:%d", kind, n)
	}
	return kind
}

// Occurrence is one entry landing on one day inside a asked-about window.
type Occurrence struct {
	EntryID string
	Day     int64
}

// Due answers "what happens between day from (inclusive) and day to
// (exclusive)". It is the single most important input the simulation tick
// consumes, and recurrence expansion happens here, once — nowhere else in the
// codebase expands a schedule.
//
// Semantics:
//
//   - none: the entry's own day, if it falls in the window.
//   - yearly: the same month and day of every year the window touches. Years
//     where that month/day does not exist (a leap-day festival in a common
//     year) are skipped, not moved.
//   - monthly: the same day-of-month of every month the window touches,
//     skipping months too short for it.
//   - every_n_days:N: Day, Day+N, Day+2N, … — the cycle starts at the entry's
//     own day, never before it.
//
// The result is sorted by day, then entry id, so output is stable.
func Due(cal *Calendar, entries []Entry, from, to int64) []Occurrence {
	if to <= from || len(entries) == 0 {
		return nil
	}
	var out []Occurrence
	for _, e := range entries {
		switch e.Recurrence {
		case "", RecurNone:
			if e.Day >= from && e.Day < to {
				out = append(out, Occurrence{EntryID: e.ID, Day: e.Day})
			}
		case RecurYearly:
			if cal == nil {
				continue
			}
			d := cal.DateOf(e.Day)
			for y := cal.DateOf(from).Year - 1; y <= cal.DateOf(to-1).Year+1; y++ {
				day, err := cal.DayOf(Date{Year: y, Month: d.Month, Day: d.Day})
				if err != nil {
					continue // that year has no such date
				}
				if day >= from && day < to {
					out = append(out, Occurrence{EntryID: e.ID, Day: day})
				}
			}
		case RecurMonthly:
			if cal == nil {
				continue
			}
			d := cal.DateOf(e.Day)
			y, m := cal.DateOf(from).Year, cal.DateOf(from).Month
			for {
				if m > len(cal.Months) {
					m = 1
					y++
				}
				first, err := cal.DayOf(Date{Year: y, Month: m, Day: 1})
				if err != nil || first >= to {
					break
				}
				if d.Day <= cal.MonthDays(y, m) {
					day, err := cal.DayOf(Date{Year: y, Month: m, Day: d.Day})
					if err == nil && day >= from && day < to {
						out = append(out, Occurrence{EntryID: e.ID, Day: day})
					}
				}
				m++
			}
		case RecurEveryNDays:
			n := e.EveryN
			if n < 1 {
				continue
			}
			for day := e.Day; day < to; day += n {
				if day >= from {
					out = append(out, Occurrence{EntryID: e.ID, Day: day})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].EntryID < out[j].EntryID
	})
	return out
}
