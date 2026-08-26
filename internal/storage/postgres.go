package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ELNAUL99/stockwatch/internal/inventory"
)

// Store is the Postgres implementation of the domain's ports.
//
// One concrete type satisfies both inventory.ProductStore and
// inventory.SalesStore. They are declared as separate interfaces over there
// because their consumers differ, but there is no reason to split the
// implementation while both are backed by the same pool and the same schema.
//
// The compile-time assertions below are the idiomatic way to state "this type
// must satisfy that interface". Without them a mismatch only surfaces at the
// call site in main.go, with a worse error message. The blank identifier means
// we want the type check and nothing else.
type Store struct {
	pool *pgxpool.Pool
}

var (
	_ inventory.ProductStore = (*Store)(nil)
	_ inventory.SalesStore   = (*Store)(nil)
)

// New wraps an existing pool. The pool is created and configured in main so that
// connection sizing is a deployment concern, visible next to the rest of the
// config, rather than buried in the storage layer.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ping checks the database is reachable, for the readiness probe.
//
// This is on the concrete Store rather than in a domain interface on purpose:
// liveness and readiness are operational concerns belonging to the HTTP layer
// and the orchestrator, not facts the replenishment domain has any use for.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// productColumns is the shared SELECT list, so the single and batch queries
// cannot drift apart and scan different things into the same helper.
//
// The LEFT JOIN matters: a product with no stock_positions row is a real state —
// an SKU set up by a buyer that has not yet been counted or received. An INNER
// JOIN would make it vanish from the catalogue entirely. COALESCE turns the
// missing row into a legitimate zero position.
const productColumns = `
	p.sku,
	p.name,
	p.lead_time_days,
	p.minimum_order_quantity,
	p.case_size,
	p.review_period_days,
	p.shelf_life_days,
	p.target_service_level,
	COALESCE(sp.on_hand_units, 0),
	COALESCE(sp.on_order_units, 0)
`

const productFrom = `
	FROM products p
	LEFT JOIN stock_positions sp ON sp.sku = p.sku
`

// ListProducts loads all products with their current stock positions.
func (s *Store) ListProducts(ctx context.Context) ([]*inventory.Product, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+productColumns+productFrom+` ORDER BY p.sku`)
	if err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	defer rows.Close()

	var products []*inventory.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		products = append(products, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list products iteration: %w", err)
	}

	if len(products) == 0 {
		return []*inventory.Product{}, nil
	}
	return products, nil
}

// GetProduct loads one product with its current stock position.
func (s *Store) GetProduct(ctx context.Context, sku string) (*inventory.Product, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+productColumns+productFrom+` WHERE p.sku = $1`, sku)

	p, err := scanProduct(row)
	if err != nil {
		// Translate the driver's sentinel into the domain's. This is the whole
		// job of an adapter: above this line nothing knows pgx exists, so
		// leaking pgx.ErrNoRows upward would put a driver detail in the HTTP
		// layer's error mapping.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: sku %q", inventory.ErrProductNotFound, sku)
		}
		return nil, fmt.Errorf("get product %s: %w", sku, err)
	}
	return p, nil
}

// GetProducts loads many products in a single round trip.
//
// = ANY($1) with a slice parameter rather than a built-up IN (...) list: pgx
// sends the slice as one array parameter, so the statement text is identical for
// 1 or 1,000 SKUs. That keeps Postgres's prepared-statement cache effective and
// makes SQL injection structurally impossible — there is no string concatenation
// to get wrong.
//
// Unknown SKUs are simply absent from the returned map. The caller decides what
// a miss means; here it is not an error.
func (s *Store) GetProducts(ctx context.Context, skus []string) (map[string]*inventory.Product, error) {
	if len(skus) == 0 {
		return map[string]*inventory.Product{}, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+productColumns+productFrom+` WHERE p.sku = ANY($1)`, skus)
	if err != nil {
		return nil, fmt.Errorf("query products: %w", err)
	}
	defer rows.Close()

	out := make(map[string]*inventory.Product, len(skus))
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		out[p.SKU] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate products: %w", err)
	}
	return out, nil
}

// rowScanner is the common ground between pgx.Row and pgx.Rows.
//
// Defining a one-method interface locally like this — rather than importing
// something — is very Go: the interface belongs to the code that needs it, and
// both pgx types satisfy it without knowing it exists.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanProduct maps one row onto the domain type.
//
// Note "any" rather than "interface{}" in rowScanner above: since Go 1.18 they
// are exact aliases for one another, and "any" is the preferred spelling. The
// older form still compiles and appears throughout older codebases.
func scanProduct(row rowScanner) (*inventory.Product, error) {
	var p inventory.Product
	err := row.Scan(
		&p.SKU,
		&p.Name,
		&p.LeadTimeDays,
		&p.MinimumOrderQuantity,
		&p.CaseSize,
		&p.ReviewPeriodDays,
		&p.ShelfLifeDays,
		&p.TargetServiceLevel,
		&p.OnHandUnits,
		&p.OnOrderUnits,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

const salesColumns = `sku, sales_date, units_sold, stockout_occurred`

// GetSalesHistory returns one SKU's sales on or after since, oldest first.
func (s *Store) GetSalesHistory(ctx context.Context, sku string, since time.Time) ([]inventory.SalesDay, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+salesColumns+`
		FROM sales_days
		WHERE sku = $1 AND sales_date >= $2
		ORDER BY sales_date ASC`, sku, since)
	if err != nil {
		return nil, fmt.Errorf("query sales history for %s: %w", sku, err)
	}
	defer rows.Close()

	var out []inventory.SalesDay
	for rows.Next() {
		day, _, err := scanSalesDay(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sales day: %w", err)
		}
		out = append(out, day)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sales history: %w", err)
	}
	return out, nil
}

// GetSalesHistories is the batch form: one query for every requested SKU.
//
// This is the query that keeps the batch endpoint from becoming N+1. Sorting by
// sku then date means each SKU's slice comes back already in the ascending order
// the estimator wants, so no post-sort is needed.
func (s *Store) GetSalesHistories(ctx context.Context, skus []string, since time.Time) (map[string][]inventory.SalesDay, error) {
	if len(skus) == 0 {
		return map[string][]inventory.SalesDay{}, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+salesColumns+`
		FROM sales_days
		WHERE sku = ANY($1) AND sales_date >= $2
		ORDER BY sku ASC, sales_date ASC`, skus, since)
	if err != nil {
		return nil, fmt.Errorf("query sales histories: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]inventory.SalesDay, len(skus))
	for rows.Next() {
		day, sku, err := scanSalesDay(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sales day: %w", err)
		}
		out[sku] = append(out[sku], day)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sales histories: %w", err)
	}
	return out, nil
}

// scanSalesDay returns the day and its SKU. The SKU comes back separately
// because SalesDay does not carry one — it is always held in the context of a
// product, and duplicating it per row would just invite the two to disagree.
func scanSalesDay(row rowScanner) (inventory.SalesDay, string, error) {
	var (
		day inventory.SalesDay
		sku string
	)
	if err := row.Scan(&sku, &day.Date, &day.UnitsSold, &day.StockOutOccurred); err != nil {
		return inventory.SalesDay{}, "", err
	}
	return day, sku, nil
}

// RecordSalesDay upserts one day of sales.
//
// ON CONFLICT DO UPDATE rather than plain INSERT because a corrected figure
// arriving late from the POS is normal operation, not an error. Making the write
// idempotent also means a retried request — a client timeout, an at-least-once
// message queue in a future phase — cannot double-count a day's sales.
//
// The date is truncated to midnight UTC before writing. The column is DATE so
// Postgres would discard the time anyway, but doing it here means the value we
// send matches the value we read back, which keeps round-trip tests honest.
func (s *Store) RecordSalesDay(ctx context.Context, sku string, day inventory.SalesDay) error {
	date := day.Date.UTC().Truncate(24 * time.Hour)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO sales_days (sku, sales_date, units_sold, stockout_occurred)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (sku, sales_date) DO UPDATE
		SET units_sold        = EXCLUDED.units_sold,
		    stockout_occurred = EXCLUDED.stockout_occurred,
		    recorded_at       = now()`,
		sku, date, day.UnitsSold, day.StockOutOccurred)
	if err != nil {
		return fmt.Errorf("upsert sales day %s/%s: %w", sku, date.Format(time.DateOnly), err)
	}
	return nil
}

// RecordSalesDays upserts many days for one SKU in a single round trip.
//
// The obvious implementation — calling RecordSalesDay in a loop — issues one
// round trip per row. For the seed generator that is 1,260 sequential
// round trips, and on anything but a local socket the latency dominates
// completely. pgx.Batch pipelines the statements: they are sent together and the
// results read together, so the cost is one network round trip rather than N.
//
// CopyFrom would be faster still, but it cannot express ON CONFLICT, and losing
// idempotency to save milliseconds on a seed job is a poor trade.
func (s *Store) RecordSalesDays(ctx context.Context, sku string, days []inventory.SalesDay) error {
	if len(days) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, day := range days {
		batch.Queue(`
			INSERT INTO sales_days (sku, sales_date, units_sold, stockout_occurred)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (sku, sales_date) DO UPDATE
			SET units_sold        = EXCLUDED.units_sold,
			    stockout_occurred = EXCLUDED.stockout_occurred,
			    recorded_at       = now()`,
			sku, day.Date.UTC().Truncate(24*time.Hour), day.UnitsSold, day.StockOutOccurred)
	}

	results := s.pool.SendBatch(ctx, batch)
	// Close reports the first error from any queued statement, so the loop below
	// is not strictly required — but reading each result names the failing row,
	// which turns "batch failed" into "row 37 failed" during debugging.
	defer func() { _ = results.Close() }()

	for i := range days {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert sales day %s/%s: %w",
				sku, days[i].Date.Format(time.DateOnly), err)
		}
	}
	return nil
}

// DeleteSalesHistory removes every recorded day for one SKU.
//
// Used only by the seed generator's -reset. Scoped to a single SKU rather than
// exposing a TRUNCATE, so the most destructive thing this package offers still
// requires naming what to destroy.
func (s *Store) DeleteSalesHistory(ctx context.Context, sku string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sales_days WHERE sku = $1`, sku); err != nil {
		return fmt.Errorf("delete sales history for %s: %w", sku, err)
	}
	return nil
}

// UpsertProduct writes product master data. Used by the seed generator and the
// integration tests; there is no HTTP route for it, because product setup is a
// merchandising workflow that belongs in a different system.
func (s *Store) UpsertProduct(ctx context.Context, p *inventory.Product) error {
	if err := p.Validate(); err != nil {
		return err
	}

	// Both statements in one transaction: a product without its position row, or
	// a position without its product, is a state no reader should ever observe.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO products (
			sku, name, lead_time_days, minimum_order_quantity, case_size,
			review_period_days, shelf_life_days, target_service_level
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (sku) DO UPDATE SET
			name                   = EXCLUDED.name,
			lead_time_days         = EXCLUDED.lead_time_days,
			minimum_order_quantity = EXCLUDED.minimum_order_quantity,
			case_size              = EXCLUDED.case_size,
			review_period_days     = EXCLUDED.review_period_days,
			shelf_life_days        = EXCLUDED.shelf_life_days,
			target_service_level   = EXCLUDED.target_service_level,
			updated_at             = now()`,
		p.SKU, p.Name, p.LeadTimeDays, p.MinimumOrderQuantity, p.CaseSize,
		p.ReviewPeriodDays, p.ShelfLifeDays, p.TargetServiceLevel,
	); err != nil {
		return fmt.Errorf("upsert product %s: %w", p.SKU, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO stock_positions (sku, on_hand_units, on_order_units)
		VALUES ($1, $2, $3)
		ON CONFLICT (sku) DO UPDATE SET
			on_hand_units  = EXCLUDED.on_hand_units,
			on_order_units = EXCLUDED.on_order_units,
			updated_at     = now()`,
		p.SKU, p.OnHandUnits, p.OnOrderUnits,
	); err != nil {
		return fmt.Errorf("upsert stock position %s: %w", p.SKU, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit product upsert: %w", err)
	}
	return nil
}
