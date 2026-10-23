package postgres

import (
	"context"
	"testing"
)

// The index audit of 22 October 2026 removed the unique index on
// bvk_client_numbers.number: the value comes from a sequence and no query looks
// a client up by number. Recreating it would silently restore dead weight.
func TestIndexCleanupDropsRedundantNumberIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT to_regclass('bvk_client_numbers_number_key') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("bvk_client_numbers_number_key still exists after migration 010")
	}
	var markers int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_migrations WHERE version='010_index_cleanup'").Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 1 {
		t.Fatalf("010_index_cleanup marker rows = %d, want 1", markers)
	}
	// Registration still deduplicates on the remaining primary key.
	for i := 0; i < 2; i++ {
		if _, err := s.pool.Exec(ctx, "INSERT INTO bvk_client_numbers(client_id) VALUES(99) ON CONFLICT DO NOTHING"); err != nil {
			t.Fatalf("client number insert %d: %v", i, err)
		}
	}
	var rows int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_client_numbers WHERE client_id=99").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("client number rows = %d, want 1", rows)
	}
}
