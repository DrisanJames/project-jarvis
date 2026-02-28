package engine

import "time"

type holidayInfo struct {
	Name  string
	Month time.Month
	Day   int
	Float bool // true = floating holiday (computed)
}

var fixedHolidays = []holidayInfo{
	{"New Year's Day", time.January, 1, false},
	{"Valentine's Day", time.February, 14, false},
	{"Independence Day", time.July, 4, false},
	{"Halloween", time.October, 31, false},
	{"Veterans Day", time.November, 11, false},
	{"Christmas Eve", time.December, 24, false},
	{"Christmas", time.December, 25, false},
	{"New Year's Eve", time.December, 31, false},
}

// DetectHoliday returns whether the given time falls on a US holiday
// and the holiday name. Covers both fixed and floating holidays.
func DetectHoliday(t time.Time) (bool, string) {
	for _, h := range fixedHolidays {
		if t.Month() == h.Month && t.Day() == h.Day {
			return true, h.Name
		}
	}

	year := t.Year()
	month := t.Month()
	day := t.Day()

	// MLK Day — 3rd Monday in January
	if month == time.January && t.Weekday() == time.Monday && nthWeekday(day) == 3 {
		return true, "MLK Day"
	}
	// Presidents' Day — 3rd Monday in February
	if month == time.February && t.Weekday() == time.Monday && nthWeekday(day) == 3 {
		return true, "Presidents' Day"
	}
	// Memorial Day — last Monday in May
	if month == time.May && t.Weekday() == time.Monday && day > 24 {
		return true, "Memorial Day"
	}
	// Labor Day — 1st Monday in September
	if month == time.September && t.Weekday() == time.Monday && nthWeekday(day) == 1 {
		return true, "Labor Day"
	}
	// Columbus Day — 2nd Monday in October
	if month == time.October && t.Weekday() == time.Monday && nthWeekday(day) == 2 {
		return true, "Columbus Day"
	}
	// Thanksgiving — 4th Thursday in November
	if month == time.November && t.Weekday() == time.Thursday && nthWeekday(day) == 4 {
		return true, "Thanksgiving"
	}
	// Black Friday — day after Thanksgiving
	if month == time.November && t.Weekday() == time.Friday && nthWeekday(day-1) == 4 {
		return true, "Black Friday"
	}
	// Cyber Monday — Monday after Thanksgiving
	if month == time.November || month == time.December {
		thanksgiving := findNthWeekdayInMonth(year, time.November, time.Thursday, 4)
		cyberMonday := thanksgiving.AddDate(0, 0, 4)
		if t.Month() == cyberMonday.Month() && t.Day() == cyberMonday.Day() {
			return true, "Cyber Monday"
		}
	}
	// Easter (Western) — approximate via anonymous Gregorian algorithm
	em, ed := easterDate(year)
	if month == em && day == ed {
		return true, "Easter"
	}
	// Mother's Day — 2nd Sunday in May
	if month == time.May && t.Weekday() == time.Sunday && nthWeekday(day) == 2 {
		return true, "Mother's Day"
	}
	// Father's Day — 3rd Sunday in June
	if month == time.June && t.Weekday() == time.Sunday && nthWeekday(day) == 3 {
		return true, "Father's Day"
	}

	return false, ""
}

// TemporalContext builds a MicroContext with just the temporal fields populated.
func TemporalContext(t time.Time) MicroContext {
	isHoliday, holidayName := DetectHoliday(t)
	return MicroContext{
		Date:        t.Format("2006-01-02"),
		DayOfWeek:   t.Weekday().String(),
		HourUTC:     t.UTC().Hour(),
		IsHoliday:   isHoliday,
		HolidayName: holidayName,
	}
}

func nthWeekday(dayOfMonth int) int {
	return (dayOfMonth-1)/7 + 1
}

func findNthWeekdayInMonth(year int, month time.Month, weekday time.Weekday, n int) time.Time {
	t := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	count := 0
	for d := 1; d <= 31; d++ {
		candidate := time.Date(year, month, d, 0, 0, 0, 0, time.UTC)
		if candidate.Month() != month {
			break
		}
		if candidate.Weekday() == weekday {
			count++
			if count == n {
				return candidate
			}
		}
	}
	return t
}

// Anonymous Gregorian Easter algorithm.
func easterDate(year int) (time.Month, int) {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	return time.Month(month), day
}
