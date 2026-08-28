// Package clock is the campaign calendar: a pure, deterministic mapping
// between the campaign's day counter and years, months, weekdays and seasons,
// plus the schedule expansion and weather derivation that hang off it.
//
// Nothing here touches a database, the wall clock, or the network — the same
// shape as the integrity rules. Every function is arithmetic over its
// arguments, which is what makes "what day is it", "what is due" and "what is
// the weather" answerable offline, with zero rows written and zero tokens
// spent. The one consequence worth stating: identical arguments always give
// identical answers, forever. Re-rolling the weather is a seed change, a
// recorded decision rather than a refresh button.
//
// No setting-specific calendar ships. Harptos, Golarion and the rest are
// product IP; the default here is a generic twelve-month "Common Reckoning"
// and a DM enters their own through the calendar editor.
package clock

import (
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalid marks input that violates a calendar or date constraint. The
// message carries the specifics.
var ErrInvalid = fmt.Errorf("invalid calendar input")

// Month is one named month of a fixed number of days. A leap rule may add
// days to one month in leap years; months themselves never vary otherwise.
type Month struct {
	Name string `json:"name"`
	Days int    `json:"days"`
}

// Season is a named band of the year, by day-of-year (1-based, inclusive).
// Bands may leave gaps; days outside every band simply have no season.
type Season struct {
	Name     string `json:"name"`
	StartDay int    `json:"start_day"`
	EndDay   int    `json:"end_day"`
}

// LeapRule makes one month of every Nth year longer: in a year divisible by
// Every, Month (1-based) gains Days extra days. Nil means the calendar has no
// leap years.
type LeapRule struct {
	Every int `json:"every"`
	Month int `json:"month"`
	Days  int `json:"days"`
}

// Calendar is one campaign's reckoning of time. It is authored by the DM,
// stored as JSON on campaign_calendars, and consumed everywhere a raw day
// number would otherwise be meaningless ("day 4,102" is not a date).
type Calendar struct {
	Name       string    `json:"name"`
	Months     []Month   `json:"months"`
	Weekdays   []string  `json:"weekdays"`
	Seasons    []Season  `json:"seasons"`
	LeapRule   *LeapRule `json:"leap_rule,omitempty"`
	EpochLabel string    `json:"epoch_label"`
}

// Date is a calendar date: 1-based month within Months, 1-based day within
// that month, and a year that may be 0 or negative (days before the epoch).
type Date struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

// Default is the generic "Common Reckoning": twelve 30-day months (a 360-day
// year), a seven-day week, four 90-day seasons, no leap years, epoch label
// "CR". It is what a campaign gets before its DM enters anything — a working
// calendar, deliberately free of any setting's IP.
func Default() *Calendar {
	cal := &Calendar{
		Name:       "Common Reckoning",
		Weekdays:   []string{"Firstday", "Secondday", "Thirdday", "Fourthday", "Fifthday", "Sixthday", "Seventhday"},
		EpochLabel: "CR",
	}
	ordinals := []string{"First", "Second", "Third", "Fourth", "Fifth", "Sixth", "Seventh", "Eighth", "Ninth", "Tenth", "Eleventh", "Twelfth"}
	for _, o := range ordinals {
		cal.Months = append(cal.Months, Month{Name: o + "month", Days: 30})
	}
	cal.Seasons = []Season{
		{Name: "spring", StartDay: 1, EndDay: 90},
		{Name: "summer", StartDay: 91, EndDay: 180},
		{Name: "autumn", StartDay: 181, EndDay: 270},
		{Name: "winter", StartDay: 271, EndDay: 360},
	}
	return cal
}

// Validate checks the structural rules: at least one month and one weekday,
// every month at least one day long, seasons inside the year, and a sane
// leap rule. Season bands may overlap or leave gaps — both are a DM's
// prerogative, not this package's.
func (c *Calendar) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: calendar is nil", ErrInvalid)
	}
	if len(c.Months) == 0 {
		return fmt.Errorf("%w: a calendar needs at least one month", ErrInvalid)
	}
	if len(c.Weekdays) == 0 {
		return fmt.Errorf("%w: a calendar needs at least one weekday", ErrInvalid)
	}
	for i, m := range c.Months {
		name := m.Name
		if name == "" {
			name = strconv.Itoa(i + 1)
		}
		if m.Days < 1 {
			return fmt.Errorf("%w: month %s has %d days", ErrInvalid, name, m.Days)
		}
	}
	year := c.YearDays(1)
	for _, s := range c.Seasons {
		if s.StartDay < 1 || s.EndDay > year || s.StartDay > s.EndDay {
			return fmt.Errorf("%w: season %s spans days %d..%d of a %d-day year",
				ErrInvalid, s.Name, s.StartDay, s.EndDay, year)
		}
	}
	if c.LeapRule != nil {
		if c.LeapRule.Every < 2 {
			return fmt.Errorf("%w: leap rule fires every %d years", ErrInvalid, c.LeapRule.Every)
		}
		if c.LeapRule.Month < 1 || c.LeapRule.Month > len(c.Months) {
			return fmt.Errorf("%w: leap month %d does not exist", ErrInvalid, c.LeapRule.Month)
		}
		if c.LeapRule.Days < 1 {
			return fmt.Errorf("%w: leap rule adds %d days", ErrInvalid, c.LeapRule.Days)
		}
	}
	return nil
}

// leapYear reports whether year y carries the leap days.
func (c *Calendar) leapYear(y int) bool {
	return c.LeapRule != nil && y%c.LeapRule.Every == 0
}

// YearDays is the length of year y, leap days included.
func (c *Calendar) YearDays(y int) int {
	if c.leapYear(y) {
		return c.commonDays() + c.LeapRule.Days
	}
	return c.commonDays()
}

// commonDays is the length of every non-leap year.
func (c *Calendar) commonDays() int {
	total := 0
	for _, m := range c.Months {
		total += m.Days
	}
	return total
}

// MonthDays is the length of month m (1-based) in year y, leap days included.
func (c *Calendar) MonthDays(y, m int) int {
	if m < 1 || m > len(c.Months) {
		return 0
	}
	days := c.Months[m-1].Days
	if c.leapYear(y) && c.LeapRule.Month == m {
		days += c.LeapRule.Days
	}
	return days
}

// daysBeforeYear is the absolute day number of the first day of year y:
// daysBeforeYear(1) == 0, the epoch. Years at or below zero walk backwards.
func (c *Calendar) daysBeforeYear(y int) int64 {
	if y >= 1 {
		var total int64
		for i := 1; i < y; i++ {
			total += int64(c.YearDays(i))
		}
		return total
	}
	var total int64
	for i := 0; i >= y; i-- {
		total -= int64(c.YearDays(i))
	}
	return total
}

// daysBeforeMonth is the day-of-year (0-based) of the first day of month m in
// year y.
func (c *Calendar) daysBeforeMonth(y, m int) int {
	total := 0
	for i := 1; i < m; i++ {
		total += c.MonthDays(y, i)
	}
	return total
}

// DayOf converts a calendar date to the absolute day number the campaign
// clock runs on: 0 is the first day of year 1, negative days precede the
// epoch. An impossible date (month 13, day 31 of a 30-day month) is
// ErrInvalid, not a silent round-up.
func (c *Calendar) DayOf(d Date) (int64, error) {
	if d.Month < 1 || d.Month > len(c.Months) {
		return 0, fmt.Errorf("%w: month %d", ErrInvalid, d.Month)
	}
	if d.Day < 1 || d.Day > c.MonthDays(d.Year, d.Month) {
		return 0, fmt.Errorf("%w: day %d of month %d in year %d", ErrInvalid, d.Day, d.Month, d.Year)
	}
	return c.daysBeforeYear(d.Year) + int64(c.daysBeforeMonth(d.Year, d.Month)) + int64(d.Day) - 1, nil
}

// DateOf converts an absolute day number to its calendar date. Any int64 is a
// valid day; days before the epoch land in year 0 and below.
func (c *Calendar) DateOf(day int64) Date {
	y := 1
	rem := day
	if rem >= 0 {
		for rem >= int64(c.YearDays(y)) {
			rem -= int64(c.YearDays(y))
			y++
		}
	} else {
		y = 0
		start := c.daysBeforeYear(0) // first day of year 0, a negative number
		for rem < start {
			y--
			start -= int64(c.YearDays(y))
		}
		rem -= start
	}
	m := 1
	for rem >= int64(c.MonthDays(y, m)) {
		rem -= int64(c.MonthDays(y, m))
		m++
	}
	return Date{Year: y, Month: m, Day: int(rem) + 1}
}

// Add returns the date n days away from d (n may be negative).
func (c *Calendar) Add(d Date, n int64) Date {
	day, err := c.DayOf(d)
	if err != nil {
		return d // an impossible date has no offset; callers validate first
	}
	return c.DateOf(day + n)
}

// WeekdayOf is the weekday name of an absolute day. Day 0 is the first
// weekday; the cycle never shifts, because a week is not tied to the year.
func (c *Calendar) WeekdayOf(day int64) string {
	n := len(c.Weekdays)
	idx := day % int64(n)
	if idx < 0 {
		idx += int64(n)
	}
	return c.Weekdays[idx]
}

// SeasonOf is the season an absolute day falls in. The second return is false
// when the day lies in no declared band — a gap a DM left on purpose.
func (c *Calendar) SeasonOf(day int64) (Season, bool) {
	d := c.DateOf(day)
	doy := int(day-c.daysBeforeYear(d.Year)) + 1
	for _, s := range c.Seasons {
		if doy >= s.StartDay && doy <= s.EndDay {
			return s, true
		}
	}
	return Season{}, false
}

// Format renders an absolute day in full: "Thirdday, 15 Thirdmonth 12 CR".
// An empty epoch label is simply omitted. The result is one of the two forms
// Parse accepts.
func (c *Calendar) Format(day int64) string {
	d := c.DateOf(day)
	var b strings.Builder
	b.WriteString(c.WeekdayOf(day))
	b.WriteString(", ")
	b.WriteString(strconv.Itoa(d.Day))
	b.WriteByte(' ')
	b.WriteString(c.Months[d.Month-1].Name)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(d.Year))
	if c.EpochLabel != "" {
		b.WriteByte(' ')
		b.WriteString(c.EpochLabel)
	}
	return b.String()
}

// FormatShort renders an absolute day without the weekday: "15 Thirdmonth 12
// CR" — the form the calendar strip labels use.
func (c *Calendar) FormatShort(day int64) string {
	d := c.DateOf(day)
	parts := []string{strconv.Itoa(d.Day), c.Months[d.Month-1].Name, strconv.Itoa(d.Year)}
	if c.EpochLabel != "" {
		parts = append(parts, c.EpochLabel)
	}
	return strings.Join(parts, " ")
}

// Parse converts text to an absolute day number. Two forms are accepted: the
// full Format output ("Thirdday, 15 Thirdmonth 12 CR" — the weekday is
// checked, not trusted) and the compact numeric "Y-M-D" ("12-3-15"). Anything
// else is ErrInvalid.
func (c *Calendar) Parse(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty date", ErrInvalid)
	}
	// Compact numeric form first: three integers joined by '-', with an
	// optional leading '-' on a negative year ("-2-9-3" is year -2, month 9).
	negYear := false
	numeric := s
	if strings.HasPrefix(s, "-") {
		negYear = true
		numeric = s[1:]
	}
	if parts := strings.Split(numeric, "-"); len(parts) == 3 {
		y, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		d, err3 := strconv.Atoi(parts[2])
		if err1 == nil && err2 == nil && err3 == nil {
			if negYear {
				y = -y
			}
			return c.DayOf(Date{Year: y, Month: m, Day: d})
		}
	}
	// Full form: "Weekday, D Month Y [Epoch]".
	weekday, rest, ok := strings.Cut(s, ", ")
	if !ok {
		return 0, fmt.Errorf("%w: %q is neither %q nor \"Y-M-D\"", ErrInvalid, s, c.Format(0))
	}
	fields := strings.Fields(rest)
	if len(fields) < 3 {
		return 0, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	day, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("%w: day %q", ErrInvalid, fields[0])
	}
	year, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, fmt.Errorf("%w: year %q", ErrInvalid, fields[2])
	}
	month := 0
	for i, m := range c.Months {
		if strings.EqualFold(m.Name, fields[1]) {
			month = i + 1
			break
		}
	}
	if month == 0 {
		return 0, fmt.Errorf("%w: no month named %q", ErrInvalid, fields[1])
	}
	got, err := c.DayOf(Date{Year: year, Month: month, Day: day})
	if err != nil {
		return 0, err
	}
	if wd := c.WeekdayOf(got); !strings.EqualFold(wd, weekday) {
		return 0, fmt.Errorf("%w: %q is a %s, not %q", ErrInvalid, s, wd, weekday)
	}
	return got, nil
}
