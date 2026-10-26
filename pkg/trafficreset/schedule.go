package trafficreset

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	DefaultTimezone = "Asia/Shanghai"
	DefaultTime     = "00:00:00"
)

// Schedule is a monthly traffic-reset instant: day-of-month plus clock in an IANA zone.
type Schedule struct {
	Day      int
	Hour     int
	Minute   int
	Second   int
	Timezone string
}

func Enabled(day *int) bool {
	return day != nil && *day >= 1 && *day <= 31
}

func ParseClock(value string) (hour, minute, second int, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, 0, nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("traffic reset time must be HH:MM or HH:MM:SS")
	}
	for i, part := range parts {
		if len(part) < 1 || len(part) > 2 {
			return 0, 0, 0, fmt.Errorf("traffic reset time must be HH:MM or HH:MM:SS")
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, 0, 0, fmt.Errorf("invalid traffic reset clock")
			}
		}
		value, parseErr := strconv.Atoi(part)
		if parseErr != nil {
			return 0, 0, 0, parseErr
		}
		switch i {
		case 0:
			hour = value
		case 1:
			minute = value
		case 2:
			second = value
		}
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return 0, 0, 0, fmt.Errorf("traffic reset time is out of range")
	}
	return hour, minute, second, nil
}

func FormatClock(hour, minute, second int) string {
	return fmt.Sprintf("%02d:%02d:%02d", hour, minute, second)
}

func NormalizeClock(value string) (string, error) {
	hour, minute, second, err := ParseClock(value)
	if err != nil {
		return "", err
	}
	return FormatClock(hour, minute, second), nil
}

func NormalizeTimezone(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultTimezone, nil
	}
	if strings.HasPrefix(value, "UTC+") || strings.HasPrefix(value, "UTC-") {
		_, offset, err := fixedOffset(value)
		if err != nil {
			return "", err
		}
		sign := "+"
		if offset < 0 {
			sign = "-"
			offset = -offset
		}
		return fmt.Sprintf("UTC%s%02d:%02d", sign, offset/3600, (offset/60)%60), nil
	}
	if _, err := time.LoadLocation(value); err != nil {
		return "", fmt.Errorf("unknown traffic reset timezone %q", value)
	}
	return value, nil
}

func Location(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" || name == DefaultTimezone || name == "Asia/Chongqing" || name == "PRC" {
		return time.FixedZone(DefaultTimezone, 8*60*60)
	}
	if strings.HasPrefix(name, "UTC+") || strings.HasPrefix(name, "UTC-") {
		loc, _, err := fixedOffset(name)
		if err == nil {
			return loc
		}
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone(DefaultTimezone, 8*60*60)
	}
	return loc
}

func FromFields(day *int, clock, timezone string) Schedule {
	hour, minute, second, err := ParseClock(clock)
	if err != nil {
		hour, minute, second = 0, 0, 0
	}
	tz, err := NormalizeTimezone(timezone)
	if err != nil {
		tz = DefaultTimezone
	}
	resolved := 0
	if day != nil {
		resolved = *day
	}
	return Schedule{
		Day:      resolved,
		Hour:     hour,
		Minute:   minute,
		Second:   second,
		Timezone: tz,
	}
}

func nextYearMonth(year int, month time.Month) (int, time.Month) {
	if month == 12 {
		return year + 1, 1
	}
	return year, month + 1
}

func previousYearMonth(year int, month time.Month) (int, time.Month) {
	if month == 1 {
		return year - 1, 12
	}
	return year, month - 1
}

func (s Schedule) loc() *time.Location {
	return Location(s.Timezone)
}

func (s Schedule) Active() bool {
	return s.Day >= 1 && s.Day <= 31
}

func daysInMonth(year int, month time.Month, loc *time.Location) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
}

func (s Schedule) boundary(year int, month time.Month) time.Time {
	loc := s.loc()
	day := s.Day
	if last := daysInMonth(year, month, loc); day > last {
		day = last
	}
	return time.Date(year, month, day, s.Hour, s.Minute, s.Second, 0, loc)
}

// Last is the most recent reset instant at or before now.
func (s Schedule) Last(now time.Time) time.Time {
	if !s.Active() {
		return time.Time{}
	}
	local := now.In(s.loc())
	this := s.boundary(local.Year(), local.Month())
	if !local.Before(this) {
		return this
	}
	year, month := previousYearMonth(local.Year(), local.Month())
	return s.boundary(year, month)
}

// Next is the upcoming reset instant after now.
func (s Schedule) Next(now time.Time) time.Time {
	if !s.Active() {
		return time.Time{}
	}
	local := now.In(s.loc())
	this := s.boundary(local.Year(), local.Month())
	if local.Before(this) {
		return this
	}
	year, month := nextYearMonth(local.Year(), local.Month())
	return s.boundary(year, month)
}

func beijingTimezone(name string) bool {
	name = strings.TrimSpace(name)
	return name == "" || name == DefaultTimezone || name == "Asia/Chongqing" || name == "PRC"
}

// UsesLegacyCycleKey is true for the default Asia/Shanghai 00:00:00 plan whose
// stored cycle records remain YYYY-MM-DD.
func (s Schedule) UsesLegacyCycleKey() bool {
	return s.Hour == 0 && s.Minute == 0 && s.Second == 0 && beijingTimezone(s.Timezone)
}

func (s Schedule) CivilDateKey(now time.Time) string {
	last := s.Last(now)
	if last.IsZero() {
		return ""
	}
	return last.In(s.loc()).Format(time.DateOnly)
}

func (s Schedule) CycleKey(now time.Time) string {
	last := s.Last(now)
	if last.IsZero() {
		return ""
	}
	if s.UsesLegacyCycleKey() {
		return last.In(s.loc()).Format(time.DateOnly)
	}
	return last.UTC().Format(time.RFC3339)
}

// LegacyCustomCycleKeys returns the current civil date key and RFC3339 key for
// a custom plan. Default Beijing midnight plans keep date-only keys.
func LegacyCustomCycleKeys(day *int, clock, timezone string, now time.Time) (civil, precise string, ok bool) {
	schedule := FromFields(day, clock, timezone)
	if !schedule.Active() || schedule.UsesLegacyCycleKey() {
		return "", "", false
	}
	civil = schedule.CivilDateKey(now)
	precise = schedule.CycleKey(now)
	if civil == "" || precise == "" {
		return "", "", false
	}
	return civil, precise, true
}

// RemapLegacyCustomCycleKey upgrades a pre-RFC3339 YYYY-MM-DD key for a
// custom plan that is still in that civil cycle. Default Beijing midnight
// plans keep date-only keys. A stored RFC3339 key is left unchanged so a
// later time or timezone edit cannot reuse the previous plan's records.
func RemapLegacyCustomCycleKey(stored string, day *int, clock, timezone string, now time.Time) (string, bool) {
	civil, precise, ok := LegacyCustomCycleKeys(day, clock, timezone, now)
	if !ok {
		return stored, false
	}
	stored = strings.TrimSpace(stored)
	if stored == "" || strings.Contains(stored, "T") || stored != civil {
		return stored, false
	}
	return precise, true
}

func (s Schedule) FormatNext(now time.Time) string {
	next := s.Next(now)
	if next.IsZero() {
		return ""
	}
	return next.In(s.loc()).Format(time.RFC3339)
}

// Fixed UTC offsets do not observe daylight saving time.
func fixedOffset(value string) (*time.Location, int, error) {
	raw := value[4:]
	parts := strings.Split(raw, ":")
	if len(parts) > 2 || len(parts[0]) < 1 || len(parts[0]) > 2 {
		return nil, 0, fmt.Errorf("invalid UTC offset")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 {
		return nil, 0, fmt.Errorf("invalid UTC offset")
	}
	minute := 0
	if len(parts) == 2 {
		if len(parts[1]) != 2 {
			return nil, 0, fmt.Errorf("invalid UTC offset")
		}
		minute, err = strconv.Atoi(parts[1])
		if err != nil || minute < 0 || minute > 59 {
			return nil, 0, fmt.Errorf("invalid UTC offset")
		}
	}
	offset := hour*3600 + minute*60
	if value[3] == '-' {
		offset = -offset
	}
	if offset < -12*3600 || offset > 14*3600 {
		return nil, 0, fmt.Errorf("UTC offset must be between UTC-12:00 and UTC+14:00")
	}
	return time.FixedZone(value, offset), offset, nil
}
