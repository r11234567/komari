package renewal

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/expiry"
)

func TestNextExpiryCatchesUpShortCyclesInOneCall(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-20 * 24 * time.Hour)
	next, err := nextExpiry(models.Client{
		BillingCycle:   7,
		ExpiryTimezone: "Asia/Shanghai",
		ExpiredAt:      &expired,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := expired.Add(21 * 24 * time.Hour)
	if !next.Equal(want) {
		t.Fatalf("7-day catch-up = %s want %s", next, want)
	}
	if !next.After(now) {
		t.Fatal("caught-up expiry must be strictly in the future")
	}
}

func TestNextExpiryDailyCatchUpLandsAfterNow(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-29 * 24 * time.Hour)
	next, err := nextExpiry(models.Client{
		BillingCycle:   1,
		ExpiryTimezone: "Asia/Shanghai",
		ExpiredAt:      &expired,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(24 * time.Hour)
	if !next.Equal(want) {
		t.Fatalf("1-day catch-up = %s want %s", next, want)
	}
}

func TestNextExpiryMonthlyAdvancesOnceWhenJustDue(t *testing.T) {
	start, err := expiry.ParseLocalDateTime("2026-10-01T09:30:45", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	next, err := nextExpiry(models.Client{
		BillingCycle:   30,
		ExpiryTimezone: "America/New_York",
		ExpiredAt:      &start,
	}, start)
	if err != nil {
		t.Fatal(err)
	}
	if expiry.FormatLocalDateTime(next, "America/New_York") != "2026-11-01T09:30:45" {
		t.Fatalf("monthly just-due = %q", expiry.FormatLocalDateTime(next, "America/New_York"))
	}
}

func TestNextExpiryRebasesWhenOverdueMoreThan30Days(t *testing.T) {
	loc := expiry.Location("Asia/Shanghai")
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, loc)
	expired := now.AddDate(0, 0, -40)
	utcExpired := expired.UTC()
	next, err := nextExpiry(models.Client{
		BillingCycle:   30,
		ExpiryTimezone: "Asia/Shanghai",
		ExpiredAt:      &utcExpired,
	}, now.UTC())
	if err != nil {
		t.Fatal(err)
	}
	if expiry.FormatLocalDateTime(next, "Asia/Shanghai") != "2026-11-20T15:00:00" {
		t.Fatalf("30-day rebase = %q", expiry.FormatLocalDateTime(next, "Asia/Shanghai"))
	}
}
