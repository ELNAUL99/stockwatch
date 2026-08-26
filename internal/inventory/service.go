package inventory

import (
	"context"
	"fmt"
	"time"
)

// The interfaces below are the domain's *ports*: the data it needs, described in
// the domain's own vocabulary, with no hint of SQL. internal/storage implements
// them. This is why the dependency arrow points inward — inventory names what it
// needs, storage adapts to that shape, and inventory never learns Postgres
// exists.
//
// Note they are declared here, in the consuming package, not next to their
// implementation. That is the Go convention and the opposite of the Java habit:
// interfaces belong to the caller. It also means storage can be swapped or faked
// without touching a line of domain code — the tests in this package do exactly
// that.
//
// They are split rather than merged into one Repository because the two have
// different lifecycles and different consumers: a future read-through cache
// would wrap ProductStore alone, and a Kafka consumer would write through
// SalesStore alone. The same concrete pgx type satisfies both.

// ProductStore reads product master data and current stock position.
type ProductStore interface {
	// ListProducts returns all products.
	ListProducts(ctx context.Context) ([]*Product, error)

	// GetProduct returns ErrProductNotFound if the SKU is unknown.
	GetProduct(ctx context.Context, sku string) (*Product, error)

	// GetProducts fetches many SKUs in one round trip. Unknown SKUs are simply
	// absent from the map rather than an error — a batch request naming one dead
	// SKU should still return recommendations for the rest.
	GetProducts(ctx context.Context, skus []string) (map[string]*Product, error)
}

// SalesStore reads and writes daily sales observations.
type SalesStore interface {
	// GetSalesHistory returns days on or after since, ascending by date.
	GetSalesHistory(ctx context.Context, sku string, since time.Time) ([]SalesDay, error)

	// GetSalesHistories is the batch form, keyed by SKU. Same round-trip
	// motivation as GetProducts: the batch endpoint must not issue N queries.
	GetSalesHistories(ctx context.Context, skus []string, since time.Time) (map[string][]SalesDay, error)

	// RecordSalesDay upserts one day. Recording the same day twice overwrites
	// rather than erroring, because a late-arriving corrected figure from the
	// POS is normal and should win.
	RecordSalesDay(ctx context.Context, sku string, day SalesDay) error
}

// Config holds the tuning parameters for demand estimation. Passed in from the
// environment at startup rather than read here, so the domain stays free of
// os.Getenv and stays trivially testable.
type Config struct {
	HistoryWindowDays int
	Alpha             float64
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{HistoryWindowDays: DefaultHistoryWindowDays, Alpha: DefaultAlpha}
}

// Service orchestrates the pure calculation against stored data.
//
// A note on layering, because this is a fair thing to challenge: Service takes a
// context.Context and calls out to stores, so it is not "pure" in the way
// demand.go and reorder.go are. It lives here anyway because it imports nothing
// beyond the standard library — the package still cannot reference storage or
// httpapi, which is the property that actually matters and which
// architecture_test.go enforces. The alternative is a separate internal/app
// package holding Service and the ports. That is defensible and I would take it
// the moment a second domain lands, but for one domain it is a package boundary
// that buys nothing.
type Service struct {
	products ProductStore
	sales    SalesStore
	cfg      Config
	// now is injected so tests control the clock. Production passes time.Now.
	now func() time.Time
}

// NewService wires a Service. Returning a pointer rather than a value because
// Service is a long-lived collaborator held by the HTTP layer, not a value that
// gets copied around.
func NewService(products ProductStore, sales SalesStore, cfg Config) *Service {
	if cfg.HistoryWindowDays <= 0 {
		cfg.HistoryWindowDays = DefaultHistoryWindowDays
	}
	if cfg.Alpha <= 0 || cfg.Alpha > 1 {
		cfg.Alpha = DefaultAlpha
	}
	return &Service{products: products, sales: sales, cfg: cfg, now: time.Now}
}

// WithClock overrides the time source. Test-only in practice, but exported
// rather than hidden behind an export_test.go shim because an integration test
// in another package needs it too.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// RecordSale stores one day of sales for an SKU.
//
// It verifies the product exists first. The foreign key would catch this anyway,
// but a constraint violation surfaces as an opaque driver error; checking here
// produces ErrProductNotFound, which the HTTP layer maps to a 404 rather than a
// 500. Cheap, and it turns a server error into a client error where it belongs.
func (s *Service) RecordSale(ctx context.Context, sku string, day SalesDay) error {
	if _, err := s.products.GetProduct(ctx, sku); err != nil {
		return err
	}
	if day.UnitsSold < 0 {
		return fmt.Errorf("%w: units sold %d is negative", ErrInvalidProduct, day.UnitsSold)
	}
	if err := s.sales.RecordSalesDay(ctx, sku, day); err != nil {
		return fmt.Errorf("record sales day for %s: %w", sku, err)
	}
	return nil
}

// ListProducts returns all products.
func (s *Service) ListProducts(ctx context.Context) ([]*Product, error) {
	return s.products.ListProducts(ctx)
}

// GetProduct returns a product with its current stock position.
func (s *Service) GetProduct(ctx context.Context, sku string) (*Product, error) {
	return s.products.GetProduct(ctx, sku)
}

// Recommend produces a replenishment decision for one SKU.
func (s *Service) Recommend(ctx context.Context, sku string) (*Recommendation, error) {
	product, err := s.products.GetProduct(ctx, sku)
	if err != nil {
		return nil, err
	}

	history, err := s.sales.GetSalesHistory(ctx, sku, s.historyCutoff())
	if err != nil {
		return nil, fmt.Errorf("load sales history for %s: %w", sku, err)
	}

	return s.recommendFrom(product, history)
}

// BatchResult pairs an SKU with its outcome. A batch is not all-or-nothing: one
// SKU with two days of history must not sink the other ninety-nine, so failures
// travel alongside successes instead of aborting the request.
type BatchResult struct {
	SKU            string          `json:"sku"`
	Recommendation *Recommendation `json:"recommendation,omitempty"`
	Error          string          `json:"error,omitempty"`
}

// RecommendBatch produces recommendations for many SKUs.
//
// Two queries total, regardless of SKU count — this is the whole reason
// GetProducts and GetSalesHistories exist alongside their singular forms. The
// naive version calls Recommend in a loop and issues 2N round trips, which for a
// 2,000-SKU DashMart replenishment run is the difference between one second and
// several minutes.
func (s *Service) RecommendBatch(ctx context.Context, skus []string) ([]BatchResult, error) {
	if len(skus) == 0 {
		return nil, ErrNoProducts
	}

	products, err := s.products.GetProducts(ctx, skus)
	if err != nil {
		return nil, fmt.Errorf("load products: %w", err)
	}

	histories, err := s.sales.GetSalesHistories(ctx, skus, s.historyCutoff())
	if err != nil {
		return nil, fmt.Errorf("load sales histories: %w", err)
	}

	results := make([]BatchResult, 0, len(skus))
	for _, sku := range skus {
		product, ok := products[sku]
		if !ok {
			results = append(results, BatchResult{SKU: sku, Error: ErrProductNotFound.Error()})
			continue
		}

		rec, err := s.recommendFrom(product, histories[sku])
		if err != nil {
			results = append(results, BatchResult{SKU: sku, Error: err.Error()})
			continue
		}
		results = append(results, BatchResult{SKU: sku, Recommendation: rec})
	}
	return results, nil
}

// recommendFrom is the shared tail of Recommend and RecommendBatch: estimate
// demand, then decide. Kept in one place so the single and batch paths cannot
// drift apart and start producing different numbers for the same SKU.
func (s *Service) recommendFrom(product *Product, history []SalesDay) (*Recommendation, error) {
	stats, err := ComputeDemandStats(history, s.cfg.HistoryWindowDays, s.cfg.Alpha)
	if err != nil {
		return nil, fmt.Errorf("estimate demand for %s: %w", product.SKU, err)
	}
	return Recommend(product, stats)
}

// historyCutoff is the earliest date the estimator will consider.
//
// It asks for a wider window than the estimator uses — see the constant below —
// and lets ComputeDemandStats do the precise trimming. Fetching a little extra
// costs one more day-row per SKU and means a run of stockouts at the start of
// the window does not leave us short of uncensored observations.
func (s *Service) historyCutoff() time.Time {
	return s.now().AddDate(0, 0, -(s.cfg.HistoryWindowDays + historyFetchSlackDays))
}

// historyFetchSlackDays is the extra history fetched beyond the estimation
// window. Seven days is one full weekly cycle, so the slack cannot skew
// day-of-week factors by over-representing some weekdays.
const historyFetchSlackDays = 7
