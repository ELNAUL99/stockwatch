package storage_test

import (
	"context"
	"testing"

	"github.com/ELNAUL99/stockwatch/internal/storage"
)

// Rollback is the most dangerous code in the repository — it drops tables — and
// it is the least exercised, since nothing calls it on the happy path. These
// tests are the only thing standing between a typo in a down file and an
// operator discovering it against production.
//
// They run last-ish by name and each restores the schema before returning, so
// they do not strand the rest of the package without tables.

func TestRollbackAndReapply(t *testing.T) {
	skipIfShort(t)
	ctx := context.Background()

	// Restore the schema no matter how this test exits, including on t.Fatal.
	// Without this a failure here cascades into every test that runs afterwards,
	// turning one real failure into a wall of noise.
	t.Cleanup(func() {
		if err := storage.Migrate(ctx, pool); err != nil {
			t.Fatalf("failed to restore schema after rollback test: %v", err)
		}
	})

	t.Run("rolls back the most recent migration", func(t *testing.T) {
		reverted, err := storage.Rollback(ctx, pool, false)
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if len(reverted) != 1 {
			t.Fatalf("reverted %d migrations, want 1", len(reverted))
		}

		// The tables should be gone.
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = 'products'
			)`).Scan(&exists); err != nil {
			t.Fatalf("check for products table: %v", err)
		}
		if exists {
			t.Error("products table still exists after rollback")
		}

		// And schema_migrations should no longer record it.
		applied, err := storage.AppliedMigrations(ctx, pool)
		if err != nil {
			t.Fatalf("AppliedMigrations: %v", err)
		}
		if len(applied) != 0 {
			t.Errorf("schema_migrations still has %d rows", len(applied))
		}
	})

	t.Run("reapplying restores the schema", func(t *testing.T) {
		// The real point of testing rollback: down followed by up must land back
		// where it started, or the down file is wrong in a way that only shows
		// up during an incident.
		applied, err := storage.MigrateVerbose(ctx, pool)
		if err != nil {
			t.Fatalf("MigrateVerbose: %v", err)
		}
		if len(applied) != 1 {
			t.Fatalf("applied %d migrations, want 1", len(applied))
		}

		// Every table must be back and usable, not merely present.
		if _, err := pool.Exec(ctx, `
			INSERT INTO products (sku, name, lead_time_days, review_period_days)
			VALUES ('ROUNDTRIP', 'Round Trip', 2, 1)`); err != nil {
			t.Fatalf("insert into the restored schema: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO sales_days (sku, sales_date, units_sold)
			VALUES ('ROUNDTRIP', '2026-06-01', 7)`); err != nil {
			t.Fatalf("insert sales into the restored schema: %v", err)
		}
		// A fully censored day must still be storable — no constraint forbids
		// zero sales alongside the stockout flag, and none should. See the
		// comment on sales_days in migrations/0001_init.up.sql.
		if _, err := pool.Exec(ctx, `
			INSERT INTO sales_days (sku, sales_date, units_sold, stockout_occurred)
			VALUES ('ROUNDTRIP', '2026-06-02', 0, true)`); err != nil {
			t.Errorf("a fully censored day was rejected after the round trip: %v", err)
		}

		// The CHECK constraints must have come back too, not just the columns.
		// Negative sales are still rejected.
		if _, err := pool.Exec(ctx, `
			INSERT INTO sales_days (sku, sales_date, units_sold)
			VALUES ('ROUNDTRIP', '2026-06-03', -5)`); err == nil {
			t.Error("sales_units_non_negative did not survive the round trip")
		}

		if _, err := pool.Exec(ctx, `DELETE FROM products WHERE sku = 'ROUNDTRIP'`); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})
}

func TestRollbackAllThenMigrate(t *testing.T) {
	skipIfShort(t)
	ctx := context.Background()

	t.Cleanup(func() {
		if err := storage.Migrate(ctx, pool); err != nil {
			t.Fatalf("failed to restore schema: %v", err)
		}
	})

	reverted, err := storage.Rollback(ctx, pool, true)
	if err != nil {
		t.Fatalf("Rollback(all): %v", err)
	}
	if len(reverted) == 0 {
		t.Fatal("nothing was rolled back")
	}

	// Rolling back everything twice must be a no-op, not an error — an operator
	// re-running a command should not be punished for it.
	again, err := storage.Rollback(ctx, pool, true)
	if err != nil {
		t.Fatalf("second Rollback(all): %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second rollback reverted %d migrations, want 0", len(again))
	}
}

func TestAppliedMigrationsReportsMetadata(t *testing.T) {
	skipIfShort(t)
	ctx := context.Background()

	applied, err := storage.AppliedMigrations(ctx, pool)
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("no migrations reported as applied")
	}

	first := applied[0]
	if first.Version != 1 {
		t.Errorf("Version = %d, want 1", first.Version)
	}
	if first.Name != "init" {
		t.Errorf("Name = %q, want \"init\"", first.Name)
	}
	if first.AppliedAt.IsZero() {
		t.Error("AppliedAt is zero")
	}

	// Ordering is relied on by Rollback, which reverts the last entry.
	for i := 1; i < len(applied); i++ {
		if applied[i-1].Version >= applied[i].Version {
			t.Errorf("versions are not ascending at index %d", i)
		}
	}
}
