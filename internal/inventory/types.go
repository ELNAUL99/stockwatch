// Package inventory holds the replenishment domain: how much of a product to
// order, when, and why. It is deliberately pure — no database, no HTTP, no
// clock of its own. Everything it needs arrives as an argument and everything it
// decides comes back as a return value, which is what makes it cheap to test and
// what keeps the dependency arrow pointing inward from storage and httpapi.
package inventory

import (
	"errors"
	"fmt"
	"time"
)

// SalesDay is one product's demand on one date.
//
// StockOutOccurred is the field that makes this a replenishment system rather
// than an averaging script. When it is true, UnitsSold is a lower bound on
// demand, not demand — we sold everything we had and turned away an unknown
// number of customers. See ComputeDemandStats for how that is handled.
type SalesDay struct {
	Date             time.Time
	UnitsSold        int
	StockOutOccurred bool
}

// Product is an SKU together with the vendor terms and physical limits that
// constrain what we may order.
//
// This is a value type passed by pointer. The pointer is not about mutation —
// Recommend never writes to it — but about avoiding a struct copy per SKU in the
// batch path, and about letting a nil pointer mean "not found" at the boundary.
type Product struct {
	SKU  string
	Name string

	// Vendor terms.
	LeadTimeDays         int // days from placing an order to receiving it
	MinimumOrderQuantity int // vendor will not ship below this
	CaseSize             int // units per case; orders must be whole cases

	// Operating parameters.
	ReviewPeriodDays   int     // days between replenishment reviews; 1 for daily
	ShelfLifeDays      int     // 0 means non-perishable
	TargetServiceLevel float64 // the z in the safety stock formula, not a percentage

	// Current position.
	OnHandUnits  int
	OnOrderUnits int
}

// Validate rejects parameter sets whose recommendation would be meaningless.
//
// The rule for what belongs here: reject only what makes the arithmetic
// nonsensical, not what looks unusual. A zero lead time is a valid same-day
// vendor; a zero shelf life means non-perishable. Neither is an error.
func (p *Product) Validate() error {
	switch {
	case p.SKU == "":
		return fmt.Errorf("%w: sku is empty", ErrInvalidProduct)
	case p.LeadTimeDays < 0:
		return fmt.Errorf("%w: lead time %d is negative", ErrInvalidProduct, p.LeadTimeDays)
	case p.ReviewPeriodDays < 1:
		// A zero review period means we never look again, so the target level
		// collapses to the reorder point and the model has nothing to order up to.
		return fmt.Errorf("%w: review period %d must be at least 1 day", ErrInvalidProduct, p.ReviewPeriodDays)
	case p.CaseSize < 0:
		return fmt.Errorf("%w: case size %d is negative", ErrInvalidProduct, p.CaseSize)
	case p.MinimumOrderQuantity < 0:
		return fmt.Errorf("%w: minimum order quantity %d is negative", ErrInvalidProduct, p.MinimumOrderQuantity)
	case p.ShelfLifeDays < 0:
		return fmt.Errorf("%w: shelf life %d is negative", ErrInvalidProduct, p.ShelfLifeDays)
	case p.TargetServiceLevel < 0:
		return fmt.Errorf("%w: service level z-score %.2f is negative", ErrInvalidProduct, p.TargetServiceLevel)
	case p.OnHandUnits < 0:
		return fmt.Errorf("%w: on-hand %d is negative", ErrInvalidProduct, p.OnHandUnits)
	case p.OnOrderUnits < 0:
		return fmt.Errorf("%w: on-order %d is negative", ErrInvalidProduct, p.OnOrderUnits)
	}
	return nil
}

// DemandStats is the estimated demand signal derived from sales history.
type DemandStats struct {
	// AverageDailyDemand is exponentially smoothed and deseasonalized: the
	// expected units on a typical day, not on any particular weekday.
	AverageDailyDemand float64
	DemandStdDev       float64

	// DayOfWeekFactors multiplies AverageDailyDemand to get a specific weekday's
	// expectation. Saturday at 1.4 means Saturdays run 40% above typical.
	DayOfWeekFactors map[time.Weekday]float64

	// DataPoints counts uncensored observations; CensoredDays counts the
	// stockout days excluded. A high censored ratio means low confidence, and
	// surfacing both lets an operator see that rather than guess.
	DataPoints   int
	CensoredDays int
}

// ExpectedDemandOn returns the seasonally-adjusted expectation for a weekday.
func (s *DemandStats) ExpectedDemandOn(wd time.Weekday) float64 {
	f, ok := s.DayOfWeekFactors[wd]
	if !ok {
		return s.AverageDailyDemand
	}
	return s.AverageDailyDemand * f
}

// RecommendationConstraint records that a limit changed the answer, and by how
// much. This exists so an operator who expected 40 units and sees 24 can read
// why instead of filing a bug against the algorithm.
//
// The json tags are a deliberate, and debatable, compromise. Strictly, a pure
// domain type should not know about wire formats, and the orthodox layout puts a
// separate DTO in httpapi with the mapping written out by hand. I have put the
// tags here because encoding/json is stdlib — this creates no dependency on the
// HTTP layer and the arrow still points inward. Phase 3 may still introduce a
// DTO if the API shape needs to diverge from the domain shape for versioning.
type RecommendationConstraint struct {
	Name             string `json:"name"`
	OriginalQuantity int    `json:"original_quantity"`
	FinalQuantity    int    `json:"final_quantity"`
	Reason           string `json:"reason"`
}

// Recommendation is the decision, with the intermediate values that produced it.
// The intermediates are part of the output on purpose: a replenishment number no
// one can audit is a number no one will act on.
type Recommendation struct {
	SKU                 string `json:"sku"`
	RecommendedQuantity int    `json:"recommended_quantity"`

	SafetyStock        float64 `json:"safety_stock"`
	ReorderPoint       float64 `json:"reorder_point"`
	TargetLevel        float64 `json:"target_level"`
	AverageDailyDemand float64 `json:"average_daily_demand"`

	CurrentOnHand  int `json:"current_on_hand"`
	CurrentOnOrder int `json:"current_on_order"`

	// OrderingIsInhibited means position was above the reorder point: we hold,
	// and RecommendedQuantity is 0. Distinct from a computed zero.
	OrderingIsInhibited bool   `json:"ordering_is_inhibited"`
	InhibitReason       string `json:"inhibit_reason,omitempty"`

	Constraints []RecommendationConstraint `json:"constraints"`
}

// Sentinel errors. These are compared with errors.Is by callers, and wrapped
// with %w at the point of return so the wrapping message adds context without
// destroying the identity the caller matches on.
var (
	ErrEmptySalesHistory   = errors.New("no sales history")
	ErrInsufficientHistory = errors.New("fewer than two uncensored observations")
	ErrInvalidProduct      = errors.New("invalid product")
	ErrProductNotFound     = errors.New("product not found")
	ErrInvalidDemandStats  = errors.New("invalid demand stats")
	ErrNoProducts          = errors.New("no skus requested")
)
