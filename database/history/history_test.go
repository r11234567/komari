package history

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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
	}
	if total > 8 {
		t.Fatalf("returned points = %d, want at most 8", total)
	}
	if total == 0 {
		t.Fatalf("returned zero points, decimation too aggressive")
	}
}

func TestLimitTotalPointsAlignsTimestamps(t *testing.T) {
	// Two series with different point counts should align to the same timestamps after decimation.
	pointsA := make([]Point, 100)
	pointsB := make([]Point, 80)
	base := time.Unix(1000, 0)
	for i := range pointsA {
		pointsA[i].Time = base.Add(time.Duration(i) * 10 * time.Second)
		pointsA[i].TotalCount = 1
	}
	for i := range pointsB {
		pointsB[i].Time = base.Add(time.Duration(i) * 10 * time.Second)
		pointsB[i].TotalCount = 1
	}

	series := []Series{
		{Kind: "ping", Client: "a", Points: pointsA},
		{Kind: "ping", Client: "b", Points: pointsB},
	}
	limited := limitTotalPoints(series, 20)

	if len(limited) != 2 {
		t.Fatalf("series count changed")
	}
	if len(limited[0].Points) == 0 || len(limited[1].Points) == 0 {
		t.Fatalf("series lost all points")
	}

	// Check that the two series have the same set of timestamps.
	timestampsA := make(map[int64]bool)
	for _, pt := range limited[0].Points {
		timestampsA[pt.Time.Unix()] = true
	}
	for _, pt := range limited[1].Points {
		if !timestampsA[pt.Time.Unix()] {
			t.Fatalf("series b has timestamp %s not present in series a — timestamps not aligned", pt.Time)
		}
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

func TestQueryRawPingBucketsSQLiteAggregatesBeforeScan(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:history-ping-buckets?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE ping_records (client TEXT, task_id INTEGER, time TEXT, value INTEGER)`).Error; err != nil {
		t.Fatal(err)
	}
	for _, value := range []int{10, 20, -1} {
		if err := db.Exec(`INSERT INTO ping_records (client, task_id, time, value) VALUES (?, ?, ?, ?)`,
			"node", 7, "2026-08-06 12:00:30.0000000", value).Error; err != nil {
			t.Fatal(err)
		}
	}

	buckets := make(map[pingKey]*Point)
	count, err := queryRawPingBucketsSQLite(
		context.Background(),
		db.Model(&models.PingRecord{}).Where(
			"time >= ? AND time <= ?",
			"2026-08-06 12:00:00.0000000",
			"2026-08-06 12:01:00.0000000",
		),
		time.Minute,
		buckets,
	)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || len(buckets) != 1 {
		t.Fatalf("count = %d, buckets = %d; want 3 and 1", count, len(buckets))
	}
	for _, point := range buckets {
		if point.TotalCount != 3 || point.LossCount != 1 || point.Avg != 30 || point.Min != 10 || point.Max != 20 {
			t.Fatalf("unexpected aggregate: %#v", point)
		}
	}
}
