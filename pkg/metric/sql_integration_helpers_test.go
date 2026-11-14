package metric

import (
	"strings"
	"testing"
)

func dropIntegrationTables(t *testing.T, store *Store, prefix string) {
	t.Helper()
	if strings.TrimSpace(prefix) == "" {
		t.Fatal("refusing to drop tables with empty prefix")
	}
	for _, name := range []string{
		prefix + "point_blocks",
		prefix + "series",
		prefix + "rollups",
		prefix + "points",
		prefix + "compaction_watermarks",
		prefix + "definitions",
	} {
		if _, err := store.db.Exec("DROP TABLE IF EXISTS " + name); err != nil {
			t.Fatalf("drop integration table %s: %v", name, err)
		}
	}
}
