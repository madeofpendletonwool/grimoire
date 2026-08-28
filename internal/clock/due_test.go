package clock

import "testing"

// TestDueYearlyFestivalOnceAndTwice is the acceptance case: a yearly
// festival, advanced over 40 days, comes due exactly once; over 400 days
// (the default calendar's year is 360 days), exactly twice.
func TestDueYearlyFestivalOnceAndTwice(t *testing.T) {
	cal := Default()
	const festivalDay = 100 // Firstmonth 101, year 1
	entries := []Entry{{ID: "festival", Day: festivalDay, Recurrence: RecurYearly}}

	// A 40-day window that contains the festival's first occurrence.
	due := Due(cal, entries, festivalDay, festivalDay+40)
	if len(due) != 1 || due[0].Day != festivalDay {
		t.Fatalf("40-day window: got %v, want one occurrence on day %d", due, festivalDay)
	}
	// A 400-day window from the same start contains it twice: days 100 and 460.
	due = Due(cal, entries, festivalDay, festivalDay+400)
	if len(due) != 2 || due[0].Day != festivalDay || due[1].Day != festivalDay+360 {
		t.Fatalf("400-day window: got %v, want occurrences on days %d and %d", due, festivalDay, festivalDay+360)
	}
}

func TestDueNoneOnlyInWindow(t *testing.T) {
	cal := Default()
	e := Entry{ID: "one", Day: 50, Recurrence: RecurNone}
	if got := Due(cal, []Entry{e}, 0, 50); len(got) != 0 {
		t.Fatalf("day 50 must not be due in [0,50): %v", got)
	}
	if got := Due(cal, []Entry{e}, 50, 60); len(got) != 1 || got[0].Day != 50 {
		t.Fatalf("day 50 must be due in [50,60): %v", got)
	}
	if got := Due(cal, []Entry{e}, 51, 60); len(got) != 0 {
		t.Fatalf("day 50 must not be due in [51,60): %v", got)
	}
}

// TestDueMonthlySkipsShortMonths walks the lopsided calendar's 7-day month:
// a "9th of every month" routine simply skips months without a 9th day.
func TestDueMonthlySkipsShortMonths(t *testing.T) {
	cal := lopsided()
	// Month M of year 1 is 7 days long; the 9th of every month skips it.
	day, err := cal.DayOf(Date{Year: 1, Month: 2, Day: 9})
	if err != nil {
		t.Fatal(err)
	}
	due := Due(cal, []Entry{{ID: "m", Day: day, Recurrence: RecurMonthly}}, 0, 400)
	for _, occ := range due {
		d := cal.DateOf(occ.Day)
		if d.Day != 9 {
			t.Fatalf("monthly occurrence on %v, want day-of-month 9", d)
		}
		if d.Month == 1 && d.Year == 1 {
			t.Fatal("month 1 of year 1 has 7 days; the 9th must be skipped")
		}
	}
	if len(due) < 10 {
		t.Fatalf("a 400-day window over 13 months must produce at least 10 monthly hits: %v", due)
	}
}

// TestDueYearlySkipsLeapDay: a festival pinned to a leap-only date simply
// does not fire in common years — it is skipped, not moved.
func TestDueYearlySkipsLeapDay(t *testing.T) {
	cal := lopsided()                                            // month 13 gains 2 days every 3rd year
	leapDay, err := cal.DayOf(Date{Year: 3, Month: 13, Day: 31}) // 31st exists only in leap years
	if err != nil {
		t.Fatal(err)
	}
	due := Due(cal, []Entry{{ID: "leapfest", Day: leapDay, Recurrence: RecurYearly}}, 0, 900)
	for _, occ := range due {
		d := cal.DateOf(occ.Day)
		if d.Month != 13 || d.Day != 31 {
			t.Fatalf("yearly occurrence on %v, want month 13 day 31", d)
		}
		if !cal.leapYear(d.Year) {
			t.Fatalf("occurrence %v is not a leap year", d)
		}
	}
	if len(due) == 0 {
		t.Fatal("the window covers several leap years; at least one occurrence is due")
	}
}

func TestDueEveryNDays(t *testing.T) {
	cal := Default()
	e := Entry{ID: "market", Day: 10, Recurrence: RecurEveryNDays, EveryN: 7}
	due := Due(cal, []Entry{e}, 0, 40)
	wantDays := []int64{10, 17, 24, 31, 38}
	if len(due) != len(wantDays) {
		t.Fatalf("got %v, want %v", due, wantDays)
	}
	for i, occ := range due {
		if occ.Day != wantDays[i] {
			t.Fatalf("got %v, want %v", due, wantDays)
		}
	}
	// The cycle starts at the entry's day, never before it.
	if got := Due(cal, []Entry{e}, 0, 10); len(got) != 0 {
		t.Fatalf("[0,10) must be empty: %v", got)
	}
}

// TestDueSortedAndStable pins the output ordering: day, then entry id.
func TestDueSortedAndStable(t *testing.T) {
	cal := Default()
	entries := []Entry{
		{ID: "b", Day: 20, Recurrence: RecurNone},
		{ID: "a", Day: 20, Recurrence: RecurNone},
		{ID: "c", Day: 10, Recurrence: RecurNone},
	}
	due := Due(cal, entries, 0, 30)
	if len(due) != 3 || due[0].EntryID != "c" || due[1].EntryID != "a" || due[2].EntryID != "b" {
		t.Fatalf("ordering broken: %v", due)
	}
}

func TestParseRecurrenceRoundTrip(t *testing.T) {
	for _, s := range []string{"none", "yearly", "monthly", "every_n_days:7", "every_n_days:120"} {
		kind, n, err := ParseRecurrence(s)
		if err != nil {
			t.Fatalf("ParseRecurrence(%q): %v", s, err)
		}
		if back := FormatRecurrence(kind, n); back != s {
			t.Fatalf("round trip %q -> %q", s, back)
		}
	}
	for _, s := range []string{"", "weekly", "every_n_days", "every_n_days:0", "every_n_days:-3", "none:4"} {
		if _, _, err := ParseRecurrence(s); err == nil {
			t.Fatalf("ParseRecurrence(%q) must fail", s)
		}
	}
}
