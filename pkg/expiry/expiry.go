package expiry

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	_ "time/tzdata"
)

var beijingLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

const (
	DefaultTimezone     = "Asia/Shanghai"
	LocalDateTimeLayout = "2006-01-02T15:04:05"
)

func NormalizeTimezone(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultTimezone, nil
	}
	if _, err := time.LoadLocation(value); err != nil {
		return "", fmt.Errorf("unknown expiry timezone %q", value)
	}
	return value, nil
}

func Location(name string) *time.Location {
	normalized, err := NormalizeTimezone(name)
	if err != nil {
		normalized = DefaultTimezone
	}
	if normalized == DefaultTimezone || normalized == "Asia/Chongqing" || normalized == "PRC" {
		return beijingLocation
	}
	loc, err := time.LoadLocation(normalized)
	if err != nil {
		return beijingLocation
	}
	return loc
}

func IsLongTerm(expiredAt *time.Time) bool {
	if expiredAt == nil {
		return true
	}
	stamp := expiredAt.UTC()
	return stamp.IsZero() || stamp.Year() < 2 || stamp.Year() > 2200
}

func IsFinite(expiredAt *time.Time) bool {
	return !IsLongTerm(expiredAt)
}

func ParseLocalDateTime(local, timezone string) (time.Time, error) {
	local = strings.TrimSpace(local)
	var year, month, day, hour, minute, second int
	if n, err := fmt.Sscanf(local, "%d-%d-%dT%d:%d:%d", &year, &month, &day, &hour, &minute, &second); err != nil || n != 6 || local != fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", year, month, day, hour, minute, second) {
		return time.Time{}, fmt.Errorf("expiry local datetime must be YYYY-MM-DDTHH:mm:ss")
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, fmt.Errorf("expiry local datetime must be YYYY-MM-DDTHH:mm:ss")
	}
	zone, err := NormalizeTimezone(timezone)
	if err != nil {
		return time.Time{}, err
	}
	if probe := time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC); probe.Year() != year || probe.Month() != time.Month(month) || probe.Day() != day {
		return time.Time{}, fmt.Errorf("expiry local datetime does not exist in timezone %s", zone)
	}
	matches := matchingWallTimes(Location(zone), year, time.Month(month), day, hour, minute, second, 0)
	if len(matches) == 0 {
		return time.Time{}, fmt.Errorf("expiry local datetime does not exist in timezone %s", zone)
	}
	return matches[0].UTC(), nil
}

func FormatLocalDateTime(instant time.Time, timezone string) string {
	zone, err := NormalizeTimezone(timezone)
	if err != nil {
		zone = DefaultTimezone
	}
	return instant.In(Location(zone)).Format(LocalDateTimeLayout)
}

func FormatLocalDisplay(instant time.Time, timezone string) string {
	zone, err := NormalizeTimezone(timezone)
	if err != nil {
		zone = DefaultTimezone
	}
	local := instant.In(Location(zone))
	return local.Format("2006-01-02 15:04:05") + " " + zone
}

func BeijingMidnightUTC(instant time.Time) time.Time {
	return time.Date(instant.In(beijingLocation).Year(), instant.In(beijingLocation).Month(), instant.In(beijingLocation).Day(), 0, 0, 0, 0, beijingLocation).UTC()
}

// Advance moves expiry one billing cycle in the named timezone.
// Calendar months and years keep the local hour, minute and second when that
// clock exists. A spring-forward gap is skipped by the actual DST gap
// (02:30:45 becomes 03:30:45). A fall-back overlap keeps the earlier
// occurrence. Direct admin input still uses ParseLocalDateTime, which
// rejects missing local times instead of shifting them.
func Advance(instant time.Time, timezone string, billingCycle int) (time.Time, error) {
	if billingCycle <= 0 {
		return time.Time{}, fmt.Errorf("billing cycle must be positive")
	}
	zone, err := NormalizeTimezone(timezone)
	if err != nil {
		return time.Time{}, err
	}
	loc := Location(zone)
	local := instant.In(loc)
	year, month, day := local.Date()
	hour, minute, second := local.Clock()
	nano := local.Nanosecond()
	years, months, days := 0, 0, 0
	switch {
	case billingCycle >= 27 && billingCycle <= 32:
		months = 1
	case billingCycle >= 87 && billingCycle <= 95:
		months = 3
	case billingCycle >= 175 && billingCycle <= 185:
		months = 6
	case billingCycle >= 360 && billingCycle <= 370:
		years = 1
	case billingCycle >= 720 && billingCycle <= 750:
		years = 2
	case billingCycle >= 1080 && billingCycle <= 1150:
		years = 3
	case billingCycle >= 1800 && billingCycle <= 1850:
		years = 5
	default:
		days = billingCycle
	}
	// Anchor the calendar date at noon so a midnight DST change cannot
	// shift the intended local day before the clock is applied.
	anchor := time.Date(year, month, day, 12, 0, 0, 0, loc).AddDate(years, months, days)
	nextYear, nextMonth, nextDay := anchor.Date()
	resolved, err := resolveWallTime(loc, nextYear, nextMonth, nextDay, hour, minute, second, nano)
	if err != nil {
		return time.Time{}, err
	}
	return resolved.UTC(), nil
}

// KeepClockOnDate rebuilds instant on the given local date, keeping hour,
// minute and second and applying the same DST rules as Advance.
func KeepClockOnDate(instant time.Time, timezone string, year int, month time.Month, day int) (time.Time, error) {
	zone, err := NormalizeTimezone(timezone)
	if err != nil {
		zone = DefaultTimezone
	}
	loc := Location(zone)
	local := instant.In(loc)
	hour, minute, second := local.Clock()
	resolved, err := resolveWallTime(loc, year, month, day, hour, minute, second, local.Nanosecond())
	if err != nil {
		return time.Time{}, err
	}
	return resolved.UTC(), nil
}

// NextDue returns the first expiry strictly after now. Overdue short cycles
// are caught up in this call so auto-renewal and early renewal save once.
func NextDue(expiredAt time.Time, timezone string, billingCycle int, now time.Time) (time.Time, error) {
	if billingCycle <= 0 {
		return time.Time{}, fmt.Errorf("billing cycle must be positive")
	}
	if IsLongTerm(&expiredAt) {
		return time.Time{}, fmt.Errorf("long-term expiry cannot be renewed")
	}
	zone, err := NormalizeTimezone(timezone)
	if err != nil {
		zone = DefaultTimezone
	}
	base := expiredAt.UTC()
	loc := Location(zone)
	localNow := now.In(loc)
	localExp := base.In(loc)
	if localExp.Before(localNow.AddDate(0, 0, -30)) {
		year, month, day := localNow.Date()
		base, err = KeepClockOnDate(base, zone, year, month, day)
		if err != nil {
			return time.Time{}, err
		}
	}
	next, err := Advance(base, zone, billingCycle)
	if err != nil {
		return time.Time{}, err
	}
	const maxCatchUp = 4000
	for i := 0; i < maxCatchUp && !next.After(now); i++ {
		next, err = Advance(next, zone, billingCycle)
		if err != nil {
			return time.Time{}, err
		}
	}
	if !next.After(now) {
		return time.Time{}, fmt.Errorf("unable to advance expiry past %s", now.UTC().Format(time.RFC3339))
	}
	return next, nil
}

// resolveWallTime maps a civil datetime in loc to a UTC instant.
// Existing unique clocks keep hour:minute:second. A spring-forward gap is
// skipped by the actual DST gap, so 02:30:45 becomes 03:30:45, including
// midnight jumps that cross a local date. A fall-back overlap keeps the
// earlier occurrence. Direct admin input still uses ParseLocalDateTime,
// which rejects missing local times instead of shifting them.
func resolveWallTime(loc *time.Location, year int, month time.Month, day, hour, minute, second, nano int) (time.Time, error) {
	if matches := matchingWallTimes(loc, year, month, day, hour, minute, second, nano); len(matches) > 0 {
		return matches[0], nil
	}
	gap, ok := dstForwardGapContaining(loc, year, month, day, hour, minute, second, nano)
	if !ok || gap <= 0 {
		return time.Time{}, fmt.Errorf("expiry local datetime does not exist in timezone %s", loc.String())
	}
	shifted := civilUTC(year, month, day, hour, minute, second, nano).Add(gap)
	nextYear, nextMonth, nextDay := shifted.Date()
	nextHour, nextMinute, nextSecond := shifted.Clock()
	matches := matchingWallTimes(loc, nextYear, nextMonth, nextDay, nextHour, nextMinute, nextSecond, shifted.Nanosecond())
	if len(matches) == 0 {
		return time.Time{}, fmt.Errorf("expiry local datetime does not exist in timezone %s", loc.String())
	}
	return matches[0], nil
}

func matchingWallTimes(loc *time.Location, year int, month time.Month, day, hour, minute, second, nano int) []time.Time {
	start, end := scanWindow(year, month, day)
	seen := map[int64]time.Time{}
	for probe := start; !probe.After(end); probe = probe.Add(30 * time.Minute) {
		_, off := probe.In(loc).Zone()
		civil := civilUTC(year, month, day, hour, minute, second, nano)
		candidate := civil.Add(-time.Duration(off) * time.Second)
		local := candidate.In(loc)
		gotYear, gotMonth, gotDay := local.Date()
		gotHour, gotMinute, gotSecond := local.Clock()
		if gotYear == year && gotMonth == month && gotDay == day && gotHour == hour && gotMinute == minute && gotSecond == second && local.Nanosecond() == nano {
			seen[candidate.UnixNano()] = candidate
		}
	}
	out := make([]time.Time, 0, len(seen))
	for _, instant := range seen {
		out = append(out, instant)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

func dstForwardGapContaining(loc *time.Location, year int, month time.Month, day, hour, minute, second, nano int) (time.Duration, bool) {
	want := civilUTC(year, month, day, hour, minute, second, nano)
	start, end := scanWindow(year, month, day)
	prev := start.In(loc)
	step := time.Minute
	for utc := start.Add(step); utc.Before(end); utc = utc.Add(step) {
		cur := utc.In(loc)
		prevCivil := civilFromLocal(prev)
		curCivil := civilFromLocal(cur)
		jump := curCivil.Sub(prevCivil)
		if jump > step+time.Second && want.After(prevCivil) && want.Before(curCivil) {
			return jump - step, true
		}
		prev = cur
	}
	return 0, false
}

func scanWindow(year int, month time.Month, day int) (time.Time, time.Time) {
	midnight := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return midnight.Add(-24 * time.Hour), midnight.Add(48 * time.Hour)
}

func civilUTC(year int, month time.Month, day, hour, minute, second, nano int) time.Time {
	return time.Date(year, month, day, hour, minute, second, nano, time.UTC)
}

func civilFromLocal(value time.Time) time.Time {
	year, month, day := value.Date()
	hour, minute, second := value.Clock()
	return civilUTC(year, month, day, hour, minute, second, value.Nanosecond())
}

func RemainingDaysCeil(expiredAt, now time.Time) int {
	remaining := expiredAt.UTC().Sub(now.UTC())
	if remaining <= 0 {
		return 0
	}
	days := remaining.Hours() / 24
	return int(math.Ceil(days - 1e-12))
}

func Due(now, expiredAt time.Time) bool {
	return !now.UTC().Before(expiredAt.UTC())
}
