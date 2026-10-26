package expiry

import (
	"testing"
	"time"
)

func TestParseLocalDateTimeUsesNamedZoneNotBrowserLocal(t *testing.T) {
	got, err := ParseLocalDateTime("2026-10-01T09:30:45", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 1, 9, 30, 45, 0, loc).UTC()
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	if FormatLocalDateTime(got, "America/New_York") != "2026-10-01T09:30:45" {
		t.Fatalf("round-trip = %q", FormatLocalDateTime(got, "America/New_York"))
	}
}

func TestParseLocalDateTimeRejectsImpossibleCalendarDate(t *testing.T) {
	_, err := ParseLocalDateTime("2026-02-30T09:30:00", "Asia/Shanghai")
	if err == nil {
		t.Fatal("February 30 should be rejected")
	}
}

func TestParseLocalDateTimeRejectsMissingDSTClock(t *testing.T) {
	_, err := ParseLocalDateTime("2026-03-08T02:30:00", "America/New_York")
	if err == nil {
		t.Fatal("missing DST spring-forward time should be rejected")
	}
}

func TestParseLocalDateTimeKeepsFirstAmbiguousDSTClock(t *testing.T) {
	got, err := ParseLocalDateTime("2026-11-01T01:30:00", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 11, 1, 1, 30, 0, 0, loc)
	if !got.Equal(first.UTC()) {
		t.Fatalf("ambiguous time = %s want first occurrence %s", got, first.UTC())
	}
}

func TestBeijingMidnightUTCUsesChinaCalendarDate(t *testing.T) {
	old := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	got := BeijingMidnightUTC(old)
	want := time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	if !BeijingMidnightUTC(want).Equal(want) {
		t.Fatal("already-midnight value should stay put")
	}
}

func TestAdvanceKeepsLocalClock(t *testing.T) {
	start, err := ParseLocalDateTime("2026-10-01T09:30:45", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	next, err := Advance(start, "America/New_York", 30)
	if err != nil {
		t.Fatal(err)
	}
	if FormatLocalDateTime(next, "America/New_York") != "2026-11-01T09:30:45" {
		t.Fatalf("monthly advance = %q", FormatLocalDateTime(next, "America/New_York"))
	}
}

func TestRemainingDaysCeilDoesNotExpireEarly(t *testing.T) {
	expire := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if RemainingDaysCeil(expire, expire.Add(-23*time.Hour)) != 1 {
		t.Fatal("less than one day should still count as 1 remaining day")
	}
	if RemainingDaysCeil(expire, expire) != 0 {
		t.Fatal("exact expiry should be 0")
	}
	if RemainingDaysCeil(expire, expire.Add(time.Second)) != 0 {
		t.Fatal("past expiry should be 0")
	}
	if RemainingDaysCeil(expire, expire.Add(-24*time.Hour)) != 1 {
		t.Fatal("exactly 24 hours should be 1 day")
	}
	if RemainingDaysCeil(expire, expire.Add(-24*time.Hour-time.Second)) != 2 {
		t.Fatal("just over 24 hours should ceil to 2 days")
	}
}

func TestDueUsesExactInstant(t *testing.T) {
	expire := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if Due(expire.Add(-time.Second), expire) {
		t.Fatal("one second early should not renew")
	}
	if !Due(expire, expire) {
		t.Fatal("exact instant should renew")
	}
}

func TestAdvanceSpringForwardSkipsMissingClockByGap(t *testing.T) {
	start, err := ParseLocalDateTime("2026-02-08T02:30:45", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	next, err := Advance(start, "America/New_York", 30)
	if err != nil {
		t.Fatal(err)
	}
	if FormatLocalDateTime(next, "America/New_York") != "2026-03-08T03:30:45" {
		t.Fatalf("spring-forward monthly advance = %q", FormatLocalDateTime(next, "America/New_York"))
	}
}

func TestAdvanceFallBackKeepsEarlierOccurrence(t *testing.T) {
	start, err := ParseLocalDateTime("2026-10-01T01:30:00", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	next, err := Advance(start, "America/New_York", 30)
	if err != nil {
		t.Fatal(err)
	}
	if FormatLocalDateTime(next, "America/New_York") != "2026-11-01T01:30:00" {
		t.Fatalf("fall-back monthly advance = %q", FormatLocalDateTime(next, "America/New_York"))
	}
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	if !next.Equal(first) {
		t.Fatalf("ambiguous advance = %s want first occurrence %s", next, first)
	}
}

func TestAdvanceKeepsClockAcrossOrdinaryDSTOffsetChange(t *testing.T) {
	start, err := ParseLocalDateTime("2026-03-01T09:30:45", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	next, err := Advance(start, "America/New_York", 30)
	if err != nil {
		t.Fatal(err)
	}
	if FormatLocalDateTime(next, "America/New_York") != "2026-04-01T09:30:45" {
		t.Fatalf("ordinary DST-offset monthly advance = %q", FormatLocalDateTime(next, "America/New_York"))
	}
}

func TestAdvanceSantiagoMidnightSpringForwardSkipsGap(t *testing.T) {
	if _, err := ParseLocalDateTime("2024-09-08T00:30:00", "America/Santiago"); err == nil {
		t.Fatal("Santiago 2024-09-08 00:30 should be missing")
	}
	start, err := ParseLocalDateTime("2024-08-08T00:30:00", "America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	next, err := Advance(start, "America/Santiago", 30)
	if err != nil {
		t.Fatal(err)
	}
	if FormatLocalDateTime(next, "America/Santiago") != "2024-09-08T01:30:00" {
		t.Fatalf("Santiago midnight spring-forward = %q", FormatLocalDateTime(next, "America/Santiago"))
	}
}

func TestAdvanceOrdinaryMidnightIsNotDSTGap(t *testing.T) {
	start, err := ParseLocalDateTime("2026-01-15T23:30:00", "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	next, err := Advance(start, "Asia/Shanghai", 1)
	if err != nil {
		t.Fatal(err)
	}
	if FormatLocalDateTime(next, "Asia/Shanghai") != "2026-01-16T23:30:00" {
		t.Fatalf("ordinary midnight advance = %q", FormatLocalDateTime(next, "Asia/Shanghai"))
	}
}

func TestNextDueEarlyRenewMatchesAutoRenewDST(t *testing.T) {
	start, err := ParseLocalDateTime("2026-02-08T02:30:45", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	next, err := NextDue(start, "America/New_York", 30, start.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if FormatLocalDateTime(next, "America/New_York") != "2026-03-08T03:30:45" {
		t.Fatalf("early renew DST = %q", FormatLocalDateTime(next, "America/New_York"))
	}
}

func TestNextDueRejectsLongTerm(t *testing.T) {
	long := time.Date(2226, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := NextDue(long, "Asia/Shanghai", 30, long); err == nil {
		t.Fatal("long-term expiry should be rejected")
	}
}

func TestNextDueCatchesUpOverdueShortCycle(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	start := now.Add(-20 * 24 * time.Hour)
	next, err := NextDue(start, "Asia/Shanghai", 7, now)
	if err != nil {
		t.Fatal(err)
	}
	want := start
	for !want.After(now) {
		want, err = Advance(want, "Asia/Shanghai", 7)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !next.Equal(want) {
		t.Fatalf("catch-up = %s want %s", next, want)
	}
}
