package metric

import (
	"context"
	"testing"
)

func TestInspectSQLiteMigrationSkipsExternalDatabases(t *testing.T) {
	for _, driver := range []Driver{DriverMySQL, DriverPostgreSQL} {
		t.Run(string(driver), func(t *testing.T) {
			summary, err := InspectSQLiteMigration(context.Background(), Config{Driver: driver})
			if err != nil {
				t.Fatalf("inspect external database: %v", err)
			}
			if summary != (SQLiteMigrationSummary{}) {
				t.Fatalf("external database requested SQLite migration: %#v", summary)
			}
		})
	}
}
