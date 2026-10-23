package analytics

import (
	"context"
	"testing"
)

// The index audit of 22 October 2026 removed analytics_cycles_end: no report
// filters cycles by ended_at, and the index only added work to every snapshot
// publish. Migrate must retire it from databases created before that change.
func TestMigrateDropsUnusedCycleIndex(t *testing.T) {
	s := integrationStore(t)
	var exists bool
	if err := s.Pool.QueryRow(context.Background(), "SELECT to_regclass('analytics.analytics_cycles_end') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("analytics_cycles_end still exists after Migrate")
	}
}
