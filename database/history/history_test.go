package history

import (
	"errors"
	"testing"
	"time"
)

func TestParseRequestBoundsPoints(t *testing.T) {
	start := time.Now().Add(-15 * 24 * time.Hour).UTC().Truncate(time.Second)
	end := time.Now().UTC().Truncate(time.Second)
	_, _, resolution, maxPoints, err := parseRequest(QueryRequest{
		Type:      "ping",
		UUID:      "node",
		Start:     start.Format(time.RFC3339),
		End:       end.Format(time.RFC3339),
		MaxPoints: 1500,
	})
	if err != nil {
		t.Fatalf("parseRequest returned an error: %v", err)
	}
	if maxPoints != 1500 {
		t.Fatalf("maxPoints = %d, want 1500", maxPoints)
	}
	if resolution < 14*time.Minute {
		t.Fatalf("resolution = %s, expected a bounded multi-minute bucket", resolution)
	}
}

func TestParseRequestRejectsLargeRange(t *testing.T) {
	end := time.Now().UTC()
	start := end.Add(-91 * 24 * time.Hour)
	_, _, _, _, err := parseRequest(QueryRequest{
		Type:  "load",
		UUID:  "node",
		Start: start.Format(time.RFC3339),
		End:   end.Format(time.RFC3339),
	})
	if !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("error = %v, want ErrRangeTooLarge", err)
	}
}

func TestParseRequestCapsMaxPoints(t *testing.T) {
	_, _, _, maxPoints, err := parseRequest(QueryRequest{Type: "ping", UUID: "node", Hours: 1, MaxPoints: MaxMaxPoints + 1})
	if err != nil {
		t.Fatalf("parseRequest returned an error: %v", err)
	}
	if maxPoints != MaxMaxPoints {
		t.Fatalf("maxPoints = %d, want %d", maxPoints, MaxMaxPoints)
	}
}

func TestParseRequestDefaultsStartRelativeToExplicitEnd(t *testing.T) {
	end := time.Date(2025, 1, 2, 12, 0, 0, 0, time.UTC)
	start, parsedEnd, _, _, err := parseRequest(QueryRequest{
		Type: "ping", UUID: "node", End: end.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !parsedEnd.Equal(end) || !start.Equal(end.Add(-4*time.Hour)) {
		t.Fatalf("range = %s to %s, expected four hours before explicit end", start, parsedEnd)
	}
}

func TestLimitTotalPointsUsesResponseWideBudget(t *testing.T) {
	series := []Series{
		{Kind: "ping", Client: "a", Points: testPoints(10)},
		{Kind: "ping", Client: "b", Points: testPoints(10)},
		{Kind: "ping", Client: "c", Points: testPoints(10)},
	}
	limited := limitTotalPoints(series, 8)
	total := 0
	for _, item := range limited {
		total += len(item.Points)
		if len(item.Points) > 1 && !item.Points[0].Time.Equal(time.Unix(0, 0)) {
			t.Fatalf("series %s did not preserve the start of its range", item.Client)
		}
	}
	if total != 8 {
		t.Fatalf("returned points = %d, want 8", total)
	}
}

func testPoints(count int) []Point {
	points := make([]Point, count)
	for index := range points {
		points[index].Time = time.Unix(int64(index), 0)
	}
	return points
}

func TestSummarizePingPointsKeepsCountsBeforeSampling(t *testing.T) {
	points := []Point{
		{Time: time.Unix(1, 0), Avg: 10, Min: 8, Max: 12, TotalCount: 4, LossCount: 1},
		{Time: time.Unix(2, 0), Avg: 20, Min: 18, Max: 22, TotalCount: 3, LossCount: 1},
	}
	summary := summarizePingPoints(points)
	if summary.TotalCount != 7 || summary.LossCount != 2 || summary.ValidCount != 5 {
		t.Fatalf("unexpected counts: %#v", summary)
	}
	if summary.Min != 8 || summary.Max != 22 || summary.Latest != 20 {
		t.Fatalf("unexpected bounds: %#v", summary)
	}
	if summary.Avg != 14 {
		t.Fatalf("average = %v, want 14", summary.Avg)
	}
}
