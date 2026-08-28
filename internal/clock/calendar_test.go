package clock

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// lopsided is the deliberately awkward custom calendar: thirteen months, a
// 7-day month beside an 11-day one, and a leap rule — the acceptance case for
// the round-trip property.
func lopsided() *Calendar {
	cal := &Calendar{
		Name:       "Lopsided Test Reckoning",
		EpochLabel: "LT",
		Weekdays:   []string{"A", "B", "C", "D", "E"},
	}
	lengths := []int{7, 11, 30, 7, 11, 28, 7, 11, 30, 30, 7, 11, 29}
	for i, d := range lengths {
		cal.Months = append(cal.Months, Month{Name: "M" + string(rune('A'+i)), Days: d})
	}
	cal.LeapRule = &LeapRule{Every: 3, Month: 13, Days: 2}
	cal.Seasons = []Season{{Name: "wet", StartDay: 1, EndDay: 100}, {Name: "dry", StartDay: 101, EndDay: 219}}
	return cal
}

// TestDayOfDateOfRoundTrip is the acceptance property: across a multi-year
// range, DayOf(DateOf(d)) == d for the default calendar and the lopsided
// custom one. Days before the epoch (negative) are included.
func TestDayOfDateOfRoundTrip(t *testing.T) {
	const years = 6
	for _, tc := range []struct {
		name string
		cal  *Calendar
		span int64
	}{
		{"default", Default(), 360 * years},
		{"lopsided", lopsided(), 221 * years},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cal.Validate(); err != nil {
				t.Fatalf("calendar does not validate: %v", err)
			}
			total := tc.span + int64(tc.cal.commonDays())
			rng := rand.New(rand.NewSource(1))
			for i := 0; i < 2000; i++ {
				d := rng.Int63n(2*total) - total // [−span, +span]
				back, err := tc.cal.DayOf(tc.cal.DateOf(d))
				if err != nil {
					t.Fatalf("DayOf(DateOf(%d)) errored: %v", d, err)
				}
				if back != d {
					t.Fatalf("DayOf(DateOf(%d)) = %d", d, back)
				}
			}
			// And every day in a full year-plus range, not just samples.
			for d := int64(-total); d < total; d++ {
				back, err := tc.cal.DayOf(tc.cal.DateOf(d))
				if err != nil || back != d {
					t.Fatalf("DayOf(DateOf(%d)) = %d, %v", d, back, err)
				}
			}
		})
	}
}

// TestDateOfDayOfRoundTrip is the other direction: a valid date survives the
// trip through the day axis, including leap days.
func TestDateOfDayOfRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		cal  *Calendar
	}{
		{"default", Default()},
		{"lopsided", lopsided()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for y := -2; y <= 7; y++ {
				for m := 1; m <= len(tc.cal.Months); m++ {
					for d := 1; d <= tc.cal.MonthDays(y, m); d++ {
						want := Date{Year: y, Month: m, Day: d}
						day, err := tc.cal.DayOf(want)
						if err != nil {
							t.Fatalf("DayOf(%v): %v", want, err)
						}
						if got := tc.cal.DateOf(day); got != want {
							t.Fatalf("DateOf(DayOf(%v)) = %v (day %d)", want, got, day)
						}
					}
				}
			}
		})
	}
}

// TestDayZeroIsEpoch pins the axis convention: absolute day 0 is the first day
// of year 1, and the day before it belongs to year 0.
func TestDayZeroIsEpoch(t *testing.T) {
	cal := Default()
	if got := cal.DateOf(0); got != (Date{Year: 1, Month: 1, Day: 1}) {
		t.Fatalf("day 0 is %v, want 1/1/1", got)
	}
	day, err := cal.DayOf(Date{Year: 1, Month: 1, Day: 1})
	if err != nil || day != 0 {
		t.Fatalf("1/1/1 is day %d, %v; want 0", day, err)
	}
	if got := cal.DateOf(-1); got != (Date{Year: 0, Month: 12, Day: 30}) {
		t.Fatalf("day -1 is %v, want the last day of year 0", got)
	}
}

// TestLeapRuleGainsDays walks the lopsided calendar's leap month: every third
// year month 13 has 31 days, and only those years.
func TestLeapRuleGainsDays(t *testing.T) {
	cal := lopsided()
	if got := cal.MonthDays(3, 13); got != 31 {
		t.Fatalf("leap year month 13 has %d days, want 31", got)
	}
	if got := cal.MonthDays(2, 13); got != 29 {
		t.Fatalf("common year month 13 has %d days, want 29", got)
	}
	if got := cal.YearDays(6); got != 221 {
		t.Fatalf("leap year has %d days, want 221", got)
	}
	if got := cal.YearDays(5); got != 219 {
		t.Fatalf("common year has %d days, want 219", got)
	}
}

// TestWeekdayCycles checks the weekday walk across a month boundary and the
// epoch alignment.
func TestWeekdayCycles(t *testing.T) {
	cal := Default()
	for d := int64(-70); d < 70; d++ {
		want := cal.Weekdays[((d%7)+7)%7]
		if got := cal.WeekdayOf(d); got != want {
			t.Fatalf("weekday of %d is %q, want %q", d, got, want)
		}
	}
}

// TestSeasonOf checks the day-of-year bands, including a no-band gap.
func TestSeasonOf(t *testing.T) {
	cal := Default()
	cases := []struct {
		day  int64
		want string
		ok   bool
	}{
		{0, "spring", true}, {89, "spring", true}, {90, "summer", true},
		{269, "autumn", true}, {270, "winter", true}, {359, "winter", true},
	}
	for _, c := range cases {
		s, ok := cal.SeasonOf(c.day)
		if ok != c.ok || s.Name != c.want {
			t.Fatalf("season of day %d = %q,%v; want %q,%v", c.day, s.Name, ok, c.want, c.ok)
		}
	}
	gappy := &Calendar{Months: []Month{{Name: "Only", Days: 40}}, Weekdays: []string{"W"},
		Seasons: []Season{{Name: "high", StartDay: 1, EndDay: 10}}}
	if _, ok := gappy.SeasonOf(20); ok {
		t.Fatal("day 20 lies in no season; the gap must report false")
	}
}

// TestFormatParseRoundTrip checks both Parse forms against Format, and that
// the weekday in the long form is checked rather than trusted.
func TestFormatParseRoundTrip(t *testing.T) {
	cal := lopsided()
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		d := rng.Int63n(3000) - 1500
		full := cal.Format(d)
		got, err := cal.Parse(full)
		if err != nil || got != d {
			t.Fatalf("Parse(%q) = %d, %v; want %d", full, got, err, d)
		}
		date := cal.DateOf(d)
		compact := strconv.Itoa(date.Year) + "-" + strconv.Itoa(date.Month) + "-" + strconv.Itoa(date.Day)
		got, err = cal.Parse(compact)
		if err != nil || got != d {
			t.Fatalf("Parse(%q) = %d, %v; want %d", compact, got, err, d)
		}
	}
	// The wrong weekday on an otherwise-valid long form is a rejection.
	day, err := cal.DayOf(Date{Year: 2, Month: 5, Day: 3})
	if err != nil {
		t.Fatal(err)
	}
	good := cal.Format(day)
	bad := "Someday," + good[strings.Index(good, ", ")+1:]
	if _, err := cal.Parse(bad); err == nil {
		t.Fatalf("%q names the wrong weekday and must be rejected", bad)
	}
	if _, err := cal.Parse("not a date at all"); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

// TestValidateRejectsBadCalendars covers the structural rules.
func TestValidateRejectsBadCalendars(t *testing.T) {
	cases := []func(*Calendar) *Calendar{
		func(c *Calendar) *Calendar { c.Months = nil; return c },
		func(c *Calendar) *Calendar { c.Weekdays = nil; return c },
		func(c *Calendar) *Calendar { c.Months[2].Days = 0; return c },
		func(c *Calendar) *Calendar { c.Seasons[0].EndDay = 9999; return c },
		func(c *Calendar) *Calendar { c.LeapRule = &LeapRule{Every: 1, Month: 1, Days: 1}; return c },
		func(c *Calendar) *Calendar { c.LeapRule = &LeapRule{Every: 4, Month: 99, Days: 1}; return c },
	}
	for i, mutate := range cases {
		cal := mutate(Default())
		if err := cal.Validate(); err == nil {
			t.Fatalf("case %d must be rejected", i)
		}
	}
	if err := Default().Validate(); err != nil {
		t.Fatalf("default calendar must validate: %v", err)
	}
	// Impossible dates are errors, not silent rounds.
	if _, err := Default().DayOf(Date{Year: 1, Month: 13, Day: 1}); err == nil {
		t.Fatal("month 13 must be rejected")
	}
	if _, err := Default().DayOf(Date{Year: 1, Month: 1, Day: 31}); err == nil {
		t.Fatal("day 31 of a 30-day month must be rejected")
	}
}

// TestAdd walks forward and backward over month and year boundaries.
func TestAdd(t *testing.T) {
	cal := Default()
	start := Date{Year: 1, Month: 12, Day: 30}
	if got := cal.Add(start, 1); got != (Date{Year: 2, Month: 1, Day: 1}) {
		t.Fatalf("add 1 over new year: %v", got)
	}
	if got := cal.Add(start, -29); got != (Date{Year: 1, Month: 12, Day: 1}) {
		t.Fatalf("subtract inside month: %v", got)
	}
}
