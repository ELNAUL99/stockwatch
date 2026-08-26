package storage_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ELNAUL99/stockwatch/internal/inventory"
	"github.com/ELNAUL99/stockwatch/internal/storage"
)

// These tests run against a real Postgres in a throwaway container.
//
// Gating is via testing.Short() rather than a //go:build tag, deliberately.
// A build tag excludes the file from compilation unless you remember to pass
// -tags, which means the default `go test ./...` silently never runs them and CI
// has to be specially configured to notice. With testing.Short():
//
//	go test ./...          runs everything, including these  (what CI does)
//	go test -short ./...   skips these                       (the fast local loop)
//
// The failure mode is the safe one: forgetting a flag runs more tests, not fewer.
//
// The package clause is storage_test, not storage. That forces these tests
// through the exported API exactly as a real caller would use it, so they cannot
// quietly reach past the interface into unexported internals.

// pool is shared by every test in this package. One container for the whole
// package rather than one per test: starting Postgres costs a few seconds, and
// paying that per-test would make the suite unusable. Isolation comes from
// resetDB truncating between tests instead.
var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	// testing.Short() reads a flag, so the flags must be parsed first. The
	// testing package normally does this inside m.Run(), which is too late.
	flag.Parse()

	if testing.Short() {
		os.Exit(m.Run())
	}

	ctx := context.Background()

	// An existing Postgres wins over starting a container.
	//
	// testcontainers is the right default — it guarantees a clean, correctly
	// versioned database and needs no setup — but requiring a Docker daemon
	// makes these tests unrunnable on any machine without one, which is exactly
	// the situation where you most want to check your SQL before pushing.
	// TEST_DATABASE_URL points them at a database you already have.
	//
	// The tests are identical either way: they migrate on entry and truncate
	// between cases, so they neither assume an empty database nor leave one
	// behind.
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		log.Printf("using TEST_DATABASE_URL instead of starting a container")

		var err error
		pool, err = pgxpool.New(ctx, dsn)
		if err != nil {
			log.Fatalf("open pool: %v", err)
		}
		if err := pool.Ping(ctx); err != nil {
			log.Fatalf("ping TEST_DATABASE_URL: %v", err)
		}
		if err := storage.Migrate(ctx, pool); err != nil {
			log.Fatalf("migrate: %v", err)
		}

		code := m.Run()
		pool.Close()
		os.Exit(code)
	}

	// Run(ctx, image, opts...) is the current form; the older RunContainer,
	// which took the image as a WithImage option, is deprecated in v0.44.
	//
	// The image tag is pinned rather than left at :latest so a Postgres release
	// cannot turn a green CI run red overnight.
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("stockwatch"),
		tcpostgres.WithUsername("stockwatch"),
		tcpostgres.WithPassword("stockwatch"),
		testcontainers.WithWaitStrategy(
			// Occurrence(2) is not a typo. The Postgres entrypoint starts the
			// server once to run initdb scripts, shuts it down, then starts it
			// for real — so the readiness line appears twice. Waiting for the
			// first one connects to a server that is about to stop, which is a
			// classic flaky-integration-test cause.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		log.Fatalf("start postgres container: %v\n\n"+
			"These integration tests need either a Docker daemon, or an existing\n"+
			"Postgres named by TEST_DATABASE_URL:\n\n"+
			"  TEST_DATABASE_URL=postgres://localhost:5432/stockwatch_test?sslmode=disable \\\n"+
			"    go test ./internal/storage/\n\n"+
			"Or skip them entirely with `go test -short ./...`.", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("connection string: %v", err)
	}

	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("open pool: %v", err)
	}

	if err := storage.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	code := m.Run()

	// os.Exit does not run deferred functions, so teardown is explicit here.
	// This is the one place in Go where defer will quietly fail you.
	pool.Close()
	if err := container.Terminate(ctx); err != nil {
		log.Printf("terminate container: %v", err)
	}

	os.Exit(code)
}

// skipIfShort guards every test in this file.
func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test; skipped by -short")
	}
}

// resetDB truncates everything between tests.
//
// TRUNCATE ... CASCADE rather than DROP and re-migrate: it is far faster and it
// keeps the schema fixed, so a test cannot accidentally depend on migration
// side effects. Registered with t.Cleanup so it runs even when a test fails
// partway, which a plain defer at the end of the test body would not guarantee
// if the test calls t.Fatal.
func resetDB(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`TRUNCATE products, stock_positions, sales_days CASCADE`); err != nil {
		t.Fatalf("reset database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`TRUNCATE products, stock_positions, sales_days CASCADE`)
	})
}

func newStore(t *testing.T) *storage.Store {
	t.Helper()
	resetDB(t)
	return storage.New(pool)
}

// testProduct returns a valid product, mutated by the caller as needed.
func testProduct(sku string) *inventory.Product {
	return &inventory.Product{
		SKU:                  sku,
		Name:                 "Test " + sku,
		LeadTimeDays:         3,
		MinimumOrderQuantity: 12,
		CaseSize:             12,
		ReviewPeriodDays:     1,
		ShelfLifeDays:        14,
		TargetServiceLevel:   1.65,
		OnHandUnits:          40,
		OnOrderUnits:         0,
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	skipIfShort(t)
	ctx := context.Background()

	// TestMain already migrated once. Running again must be a no-op, because
	// every replica calls this on boot and a redeploy must not fail.
	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("third Migrate: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations has %d rows, want 1; migrations are being "+
			"recorded more than once", count)
	}
}

func TestUpsertAndGetProduct(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	want := testProduct("MILK-1L")
	if err := store.UpsertProduct(ctx, want); err != nil {
		t.Fatalf("UpsertProduct: %v", err)
	}

	got, err := store.GetProduct(ctx, "MILK-1L")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}

	// Field-by-field rather than reflect.DeepEqual on the pointer, so a failure
	// names the column that is wrong instead of dumping two structs.
	tests := []struct {
		field     string
		got, want any
	}{
		{"SKU", got.SKU, want.SKU},
		{"Name", got.Name, want.Name},
		{"LeadTimeDays", got.LeadTimeDays, want.LeadTimeDays},
		{"MinimumOrderQuantity", got.MinimumOrderQuantity, want.MinimumOrderQuantity},
		{"CaseSize", got.CaseSize, want.CaseSize},
		{"ReviewPeriodDays", got.ReviewPeriodDays, want.ReviewPeriodDays},
		{"ShelfLifeDays", got.ShelfLifeDays, want.ShelfLifeDays},
		{"TargetServiceLevel", got.TargetServiceLevel, want.TargetServiceLevel},
		{"OnHandUnits", got.OnHandUnits, want.OnHandUnits},
		{"OnOrderUnits", got.OnOrderUnits, want.OnOrderUnits},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.field, tt.got, tt.want)
			}
		})
	}
}

func TestUpsertProductOverwrites(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	p := testProduct("MILK-1L")
	if err := store.UpsertProduct(ctx, p); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	p.CaseSize = 24
	p.OnHandUnits = 100
	if err := store.UpsertProduct(ctx, p); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := store.GetProduct(ctx, "MILK-1L")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if got.CaseSize != 24 {
		t.Errorf("CaseSize = %d, want 24", got.CaseSize)
	}
	if got.OnHandUnits != 100 {
		t.Errorf("OnHandUnits = %d, want 100; the stock_positions row was not updated",
			got.OnHandUnits)
	}
}

func TestGetProductNotFound(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)

	_, err := store.GetProduct(context.Background(), "NOPE")

	// The point of this test is the translation. pgx returns its own ErrNoRows;
	// the storage layer must convert that to the domain sentinel so nothing
	// above this layer ever needs to know which driver is in use.
	if !errors.Is(err, inventory.ErrProductNotFound) {
		t.Fatalf("got error %v, want inventory.ErrProductNotFound", err)
	}
}

func TestGetProductWithoutStockPosition(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	// Insert a product with no stock_positions row — a real state for an SKU a
	// buyer has set up but that has never been counted or received. The LEFT
	// JOIN plus COALESCE must surface it as a zero position, not make it vanish.
	if _, err := pool.Exec(ctx, `
		INSERT INTO products (sku, name, lead_time_days, review_period_days)
		VALUES ('NEW-SKU', 'Brand New', 3, 1)`); err != nil {
		t.Fatalf("insert bare product: %v", err)
	}

	got, err := store.GetProduct(ctx, "NEW-SKU")
	if err != nil {
		t.Fatalf("GetProduct: %v; an INNER JOIN would produce not-found here", err)
	}
	if got.OnHandUnits != 0 || got.OnOrderUnits != 0 {
		t.Errorf("position = (%d, %d), want (0, 0)", got.OnHandUnits, got.OnOrderUnits)
	}
}

func TestGetProductsBatch(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	for _, sku := range []string{"A", "B", "C"} {
		if err := store.UpsertProduct(ctx, testProduct(sku)); err != nil {
			t.Fatalf("upsert %s: %v", sku, err)
		}
	}

	t.Run("returns every known sku", func(t *testing.T) {
		got, err := store.GetProducts(ctx, []string{"A", "B", "C"})
		if err != nil {
			t.Fatalf("GetProducts: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d products, want 3", len(got))
		}
	})

	t.Run("unknown skus are absent, not an error", func(t *testing.T) {
		got, err := store.GetProducts(ctx, []string{"A", "GHOST", "C"})
		if err != nil {
			t.Fatalf("GetProducts: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d products, want 2", len(got))
		}
		if _, ok := got["GHOST"]; ok {
			t.Error("GHOST should not be present")
		}
	})

	t.Run("empty request short-circuits", func(t *testing.T) {
		got, err := store.GetProducts(ctx, nil)
		if err != nil {
			t.Fatalf("GetProducts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d products, want 0", len(got))
		}
	})
}

func TestRecordSalesDayIsIdempotent(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	if err := store.UpsertProduct(ctx, testProduct("MILK-1L")); err != nil {
		t.Fatalf("upsert product: %v", err)
	}

	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	if err := store.RecordSalesDay(ctx, "MILK-1L",
		inventory.SalesDay{Date: day, UnitsSold: 20}); err != nil {
		t.Fatalf("first record: %v", err)
	}

	// A corrected figure arriving late from the POS must overwrite, not
	// duplicate and not error. This also makes a retried request safe.
	if err := store.RecordSalesDay(ctx, "MILK-1L",
		inventory.SalesDay{Date: day, UnitsSold: 25, StockOutOccurred: true}); err != nil {
		t.Fatalf("second record: %v", err)
	}

	history, err := store.GetSalesHistory(ctx, "MILK-1L", day.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("GetSalesHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("got %d rows, want 1; the upsert duplicated instead of updating",
			len(history))
	}
	if history[0].UnitsSold != 25 {
		t.Errorf("UnitsSold = %d, want 25", history[0].UnitsSold)
	}
	if !history[0].StockOutOccurred {
		t.Error("StockOutOccurred = false, want true")
	}
}

func TestRecordSalesDayNormalisesTime(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	if err := store.UpsertProduct(ctx, testProduct("MILK-1L")); err != nil {
		t.Fatalf("upsert product: %v", err)
	}

	// Two writes on the same calendar day at different clock times must collapse
	// to one row, or a POS that reports twice a day would double-count.
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, ts := range []time.Time{
		base.Add(9 * time.Hour),
		base.Add(17 * time.Hour),
	} {
		if err := store.RecordSalesDay(ctx, "MILK-1L",
			inventory.SalesDay{Date: ts, UnitsSold: 20}); err != nil {
			t.Fatalf("record at %v: %v", ts, err)
		}
	}

	history, err := store.GetSalesHistory(ctx, "MILK-1L", base.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("GetSalesHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("got %d rows, want 1", len(history))
	}
	if !history[0].Date.Equal(base) {
		t.Errorf("Date = %v, want %v (midnight UTC)", history[0].Date, base)
	}
}

func TestGetSalesHistoryOrderingAndWindow(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	if err := store.UpsertProduct(ctx, testProduct("MILK-1L")); err != nil {
		t.Fatalf("upsert product: %v", err)
	}

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Insert out of order, to prove the ORDER BY does the work rather than the
	// insertion sequence happening to be right.
	for _, offset := range []int{5, 1, 9, 3, 7} {
		if err := store.RecordSalesDay(ctx, "MILK-1L", inventory.SalesDay{
			Date:      base.AddDate(0, 0, offset),
			UnitsSold: 10 + offset,
		}); err != nil {
			t.Fatalf("record day +%d: %v", offset, err)
		}
	}

	t.Run("returns ascending by date", func(t *testing.T) {
		history, err := store.GetSalesHistory(ctx, "MILK-1L", base)
		if err != nil {
			t.Fatalf("GetSalesHistory: %v", err)
		}
		if len(history) != 5 {
			t.Fatalf("got %d rows, want 5", len(history))
		}
		for i := 1; i < len(history); i++ {
			if !history[i-1].Date.Before(history[i].Date) {
				t.Fatalf("rows %d and %d are out of order: %v then %v",
					i-1, i, history[i-1].Date, history[i].Date)
			}
		}
	})

	t.Run("since is inclusive and excludes older days", func(t *testing.T) {
		// Cut at +5: days 5, 7 and 9 survive; 1 and 3 do not.
		history, err := store.GetSalesHistory(ctx, "MILK-1L", base.AddDate(0, 0, 5))
		if err != nil {
			t.Fatalf("GetSalesHistory: %v", err)
		}
		if len(history) != 3 {
			t.Fatalf("got %d rows, want 3", len(history))
		}
		if !history[0].Date.Equal(base.AddDate(0, 0, 5)) {
			t.Errorf("first row is %v, want the boundary day itself (since is inclusive)",
				history[0].Date)
		}
	})
}

func TestGetSalesHistoriesBatch(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, sku := range []string{"A", "B"} {
		if err := store.UpsertProduct(ctx, testProduct(sku)); err != nil {
			t.Fatalf("upsert %s: %v", sku, err)
		}
		for i := 0; i < 3; i++ {
			if err := store.RecordSalesDay(ctx, sku, inventory.SalesDay{
				Date: base.AddDate(0, 0, i), UnitsSold: 10,
			}); err != nil {
				t.Fatalf("record %s day %d: %v", sku, i, err)
			}
		}
	}

	got, err := store.GetSalesHistories(ctx, []string{"A", "B", "GHOST"}, base)
	if err != nil {
		t.Fatalf("GetSalesHistories: %v", err)
	}

	if len(got["A"]) != 3 {
		t.Errorf("A has %d days, want 3", len(got["A"]))
	}
	if len(got["B"]) != 3 {
		t.Errorf("B has %d days, want 3", len(got["B"]))
	}
	if _, ok := got["GHOST"]; ok {
		t.Error("GHOST should be absent from the map")
	}

	// Each SKU's slice must arrive already sorted, since the estimator relies on
	// it and re-sorting per SKU would be wasted work.
	for sku, days := range got {
		for i := 1; i < len(days); i++ {
			if !days[i-1].Date.Before(days[i].Date) {
				t.Errorf("%s rows %d and %d out of order", sku, i-1, i)
			}
		}
	}
}

func TestCheckConstraints(t *testing.T) {
	skipIfShort(t)
	newStore(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO products (sku, name, lead_time_days, review_period_days)
		VALUES ('X', 'X', 1, 1)`); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	// The database is the last line of defence. Application validation catches
	// these first with better messages, but a migration, a manual fix or a
	// future service could bypass that — these constraints could not.
	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "negative review period rejected",
			sql:  `INSERT INTO products (sku, name, lead_time_days, review_period_days) VALUES ('Y','Y',1,0)`,
		},
		{
			name: "negative lead time rejected",
			sql:  `INSERT INTO products (sku, name, lead_time_days, review_period_days) VALUES ('Z','Z',-1,1)`,
		},
		{
			name: "negative on-hand rejected",
			sql:  `INSERT INTO stock_positions (sku, on_hand_units) VALUES ('X', -5)`,
		},
		{
			name: "negative units sold rejected",
			sql:  `INSERT INTO sales_days (sku, sales_date, units_sold) VALUES ('X', '2026-06-01', -1)`,
		},
		{
			name: "sales for an unknown sku rejected by the foreign key",
			sql:  `INSERT INTO sales_days (sku, sales_date, units_sold) VALUES ('GHOST', '2026-06-01', 5)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tt.sql, tt.args...); err == nil {
				t.Error("insert succeeded, want a constraint violation")
			}
		})
	}
}

func TestFullyCensoredDayIsStorable(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	if err := store.UpsertProduct(ctx, testProduct("EMPTY-SHELF")); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// units_sold = 0 with stockout_occurred = true: the shelf was empty when the
	// doors opened, so the entire day's demand went unrecorded. An earlier
	// version of this schema rejected exactly this row, which forced the most
	// censored observation there is to be stored as an ordinary zero-demand day.
	//
	// This test exists because that bug was real and shipped through three
	// phases before the seed generator surfaced it.
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := store.RecordSalesDay(ctx, "EMPTY-SHELF", inventory.SalesDay{
		Date: day, UnitsSold: 0, StockOutOccurred: true,
	}); err != nil {
		t.Fatalf("a fully censored day was rejected: %v", err)
	}

	// And a genuine zero-demand day, which looks identical in units_sold but
	// means the opposite.
	if err := store.RecordSalesDay(ctx, "EMPTY-SHELF", inventory.SalesDay{
		Date: day.AddDate(0, 0, 1), UnitsSold: 0, StockOutOccurred: false,
	}); err != nil {
		t.Fatalf("a genuine zero-demand day was rejected: %v", err)
	}

	history, err := store.GetSalesHistory(ctx, "EMPTY-SHELF", day)
	if err != nil {
		t.Fatalf("GetSalesHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("got %d rows, want 2", len(history))
	}
	if !history[0].StockOutOccurred {
		t.Error("the censored day lost its flag in the round trip")
	}
	if history[1].StockOutOccurred {
		t.Error("the genuine zero-demand day gained a stockout flag")
	}
}

func TestCascadeDelete(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	if err := store.UpsertProduct(ctx, testProduct("DOOMED")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.RecordSalesDay(ctx, "DOOMED", inventory.SalesDay{
		Date: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), UnitsSold: 5,
	}); err != nil {
		t.Fatalf("record sale: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM products WHERE sku = 'DOOMED'`); err != nil {
		t.Fatalf("delete product: %v", err)
	}

	// ON DELETE CASCADE means deleting a product takes its position and history
	// with it, rather than leaving orphan rows that later break a join.
	for _, table := range []string{"stock_positions", "sales_days"} {
		var count int
		if err := pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE sku = 'DOOMED'`, table),
		).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s still has %d orphan rows", table, count)
		}
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)

	// An already-cancelled context must abort before touching the database.
	// This is what makes the HTTP timeout middleware in phase 3 actually stop
	// work rather than just stop waiting for it — a query that keeps running
	// after the client has gone is a connection held for nothing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.GetProduct(ctx, "ANY"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetProduct with a cancelled context returned %v, want context.Canceled", err)
	}
}

// TestServiceEndToEnd exercises the whole stack: real Postgres, real storage
// adapter, real domain logic. Everything below the HTTP layer.
func TestServiceEndToEnd(t *testing.T) {
	skipIfShort(t)
	store := newStore(t)
	ctx := context.Background()

	product := testProduct("BAGEL-6PK")
	product.ShelfLifeDays = 3
	product.CaseSize = 12
	product.OnHandUnits = 14
	product.OnOrderUnits = 0
	if err := store.UpsertProduct(ctx, product); err != nil {
		t.Fatalf("upsert product: %v", err)
	}

	// 28 days of weekend-heavy sales with a three-day stockout run in the middle.
	start := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC) // a Monday
	for i := 0; i < 28; i++ {
		date := start.AddDate(0, 0, i)
		units := 18
		if wd := date.Weekday(); wd == time.Saturday || wd == time.Sunday {
			units = 34
		}
		day := inventory.SalesDay{Date: date, UnitsSold: units}
		if i >= 10 && i <= 12 {
			day.UnitsSold = units / 2
			day.StockOutOccurred = true
		}
		if err := store.RecordSalesDay(ctx, "BAGEL-6PK", day); err != nil {
			t.Fatalf("record day %d: %v", i, err)
		}
	}

	// Clock pinned just past the history so the window covers all 28 days.
	svc := inventory.NewService(store, store, inventory.DefaultConfig()).
		WithClock(func() time.Time { return start.AddDate(0, 0, 28) })

	t.Run("single recommendation", func(t *testing.T) {
		rec, err := svc.Recommend(ctx, "BAGEL-6PK")
		if err != nil {
			t.Fatalf("Recommend: %v", err)
		}
		if rec.SKU != "BAGEL-6PK" {
			t.Errorf("SKU = %q", rec.SKU)
		}
		if rec.RecommendedQuantity <= 0 {
			t.Errorf("RecommendedQuantity = %d, want > 0 for a fast-moving SKU "+
				"with only 14 units on hand", rec.RecommendedQuantity)
		}
		if rec.RecommendedQuantity%product.CaseSize != 0 {
			t.Errorf("RecommendedQuantity %d is not a whole number of %d-unit cases",
				rec.RecommendedQuantity, product.CaseSize)
		}
		// The censored days must not have dragged demand down to the halved
		// figures they recorded.
		if rec.AverageDailyDemand < 15 {
			t.Errorf("AverageDailyDemand = %.2f, want >= 15; the stockout days "+
				"appear to have been averaged in", rec.AverageDailyDemand)
		}
	})

	t.Run("batch matches the single path", func(t *testing.T) {
		single, err := svc.Recommend(ctx, "BAGEL-6PK")
		if err != nil {
			t.Fatalf("Recommend: %v", err)
		}

		results, err := svc.RecommendBatch(ctx, []string{"BAGEL-6PK", "GHOST"})
		if err != nil {
			t.Fatalf("RecommendBatch: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("got %d results, want 2", len(results))
		}
		if results[0].Recommendation == nil {
			t.Fatalf("BAGEL-6PK missing a recommendation: %s", results[0].Error)
		}
		// The two paths share recommendFrom, so they must never diverge.
		if results[0].Recommendation.RecommendedQuantity != single.RecommendedQuantity {
			t.Errorf("batch says %d, single says %d",
				results[0].Recommendation.RecommendedQuantity, single.RecommendedQuantity)
		}
		if results[1].Error == "" {
			t.Error("GHOST should report an error")
		}
	})

	t.Run("recording a sale through the service", func(t *testing.T) {
		err := svc.RecordSale(ctx, "BAGEL-6PK", inventory.SalesDay{
			Date: start.AddDate(0, 0, 28), UnitsSold: 21,
		})
		if err != nil {
			t.Fatalf("RecordSale: %v", err)
		}

		err = svc.RecordSale(ctx, "GHOST", inventory.SalesDay{
			Date: start, UnitsSold: 5,
		})
		// The service checks the product exists first, so this is a clean
		// not-found rather than an opaque foreign-key violation from the driver.
		if !errors.Is(err, inventory.ErrProductNotFound) {
			t.Errorf("got %v, want ErrProductNotFound", err)
		}
	})
}
