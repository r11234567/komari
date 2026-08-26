package jsonrpc

import (
	"testing"

	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestDatabaseQueryResponseAsMapUsesJSONFieldNames(t *testing.T) {
	response := databaseQueryResponse{
		Database: "main",
		Driver:   "sqlite",
		Columns:  []string{"uuid"},
		Rows:     [][]any{{"user-1"}},
		RowCount: 1,
	}
	result := response.asMap()
	if result["row_count"] != 1 {
		t.Fatalf("row_count = %v, want 1", result["row_count"])
	}
	if _, exists := result["RowCount"]; exists {
		t.Fatal("response must not expose Go field names to plugins")
	}
	rows, ok := result["rows"].([][]any)
	if !ok || len(rows) != 1 || rows[0][0] != "user-1" {
		t.Fatalf("rows = %#v, want one database row", result["rows"])
	}
}

func TestParseDatabaseTarget(t *testing.T) {
	tests := []struct {
		name  string
		value *string
		want  string
		valid bool
	}{
		{name: "default main", want: databaseTargetMain, valid: true},
		{name: "main", value: stringPointer(databaseTargetMain), want: databaseTargetMain, valid: true},
		{name: "metrics", value: stringPointer(databaseTargetMetrics), want: databaseTargetMetrics, valid: true},
		{name: "invalid", value: stringPointer("other")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseDatabaseTarget(test.value)
			if test.valid {
				if err != nil || got != test.want {
					t.Fatalf("parseDatabaseTarget() = %q, %v; want %q, nil", got, err, test.want)
				}
				return
			}
			if err == nil {
				t.Fatal("parseDatabaseTarget() error = nil; want error")
			}
		})
	}
}

func TestParseDatabaseQueryParamsLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit any
		valid bool
	}{
		{name: "default", valid: true},
		{name: "minimum", limit: 1, valid: true},
		{name: "maximum", limit: maxDBQueryLimit, valid: true},
		{name: "zero", limit: 0},
		{name: "too large", limit: maxDBQueryLimit + 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := map[string]any{"sql": "SELECT 1"}
			if test.limit != nil {
				params["limit"] = test.limit
			}
			_, _, _, got, err := parseDatabaseQueryParams(rpc.NewRequest(1, "admin:dbQuery", params))
			if test.valid {
				if err != nil {
					t.Fatalf("parseDatabaseQueryParams() error = %v", err)
				}
				if test.limit == nil && got != defaultDBQueryLimit {
					t.Fatalf("default limit = %d, want %d", got, defaultDBQueryLimit)
				}
				return
			}
			if err == nil {
				t.Fatal("parseDatabaseQueryParams() error = nil; want error")
			}
		})
	}
}

func TestTableListSQL(t *testing.T) {
	for _, driver := range []metric.Driver{metric.DriverSQLite, metric.DriverMySQL, metric.DriverPostgreSQL} {
		t.Run(string(driver), func(t *testing.T) {
			statement, err := tableListSQL(driver)
			if err != nil || statement == "" {
				t.Fatalf("tableListSQL(%q) = %q, %v", driver, statement, err)
			}
		})
	}
	if _, err := tableListSQL(metric.Driver("unknown")); err == nil {
		t.Fatal("tableListSQL(unknown) error = nil; want error")
	}
}

func stringPointer(value string) *string { return &value }
