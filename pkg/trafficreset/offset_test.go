package trafficreset

import (
	"testing"
	"time"
)

func TestFixedUTCOffsetsAndLeapMonthBoundary(t *testing.T) {
	day := 31
	s := FromFields(&day, "12:30", "UTC+05:30")
	at := time.Date(2028, 2, 29, 7, 0, 0, 0, time.UTC)
	if got := s.Last(at); !got.Equal(at) {
		t.Fatalf("leap boundary=%v", got)
	}
	if got := s.Next(at); !got.Equal(time.Date(2028, 3, 31, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("next boundary=%v", got)
	}
	if got := s.Last(at.Add(-time.Nanosecond)); !got.Equal(time.Date(2028, 1, 31, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("before boundary=%v", got)
	}
	for _, invalid := range []string{"UTC+15", "UTC-13", "UTC+14:01", "UTC+05:60", "UTC+5x"} {
		if _, err := NormalizeTimezone(invalid); err == nil {
			t.Fatalf("accepted invalid offset %q", invalid)
		}
	}
}
